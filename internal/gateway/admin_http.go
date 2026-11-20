package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
)

// configHeader names, on every response of GET, PUT and DELETE on
// /amf/config, the copy of the config the response is about, with what
// /amf/config-status says of it: "draft; source=files; edited=true;
// live=false; saved=false", or "live; source=files; edited=false;
// saved=true".
const configHeader = "X-Gateway-Config"

// routeAdmin adds the endpoints that read and change the config, each for
// the callers that server.adminApi lists.
func (s *server) routeAdmin(mux *http.ServeMux) {
	read := func(h clientHandler) http.HandlerFunc {
		return s.authenticated(amfFamily, s.admin(false, "read the config", h))
	}
	write := func(what string, h clientHandler) http.HandlerFunc {
		return s.authenticated(amfFamily, s.admin(true, what, h))
	}
	mux.HandleFunc("GET /amf/config", read(s.getConfig))
	mux.HandleFunc("GET /amf/config/{path...}", read(s.getConfig))
	mux.HandleFunc("PUT /amf/config", write("edit the config", s.putConfig))
	mux.HandleFunc("PUT /amf/config/{path...}", write("edit the config", s.putConfig))
	mux.HandleFunc("DELETE /amf/config/{path...}", write("edit the config", s.deleteConfig))
	mux.HandleFunc("POST /amf/config-rename/{path...}", write("edit the config", s.renameConfig))
	mux.HandleFunc("GET /amf/config-status", read(s.configStatus))
	mux.HandleFunc("GET /amf/config-backups", read(s.configBackups))
	if s.live.logs != nil {
		mux.HandleFunc("GET /amf/logs", read(s.getLogs))
	}
	mux.HandleFunc("POST /amf/reload", write("reload the config", s.reloadConfig))
	mux.HandleFunc("POST /amf/config-apply", write("apply the draft", s.applyConfig))
	mux.HandleFunc("POST /amf/config-revert", write("revert the draft", s.revertConfig))
	mux.HandleFunc("POST /amf/config-load", write("load the config", s.loadConfig))
	mux.HandleFunc("POST /amf/config-save", write("save the config", s.saveConfig))
}

// admin admits a caller that server.adminApi lists for the endpoint: write for
// one that changes something, read or write for one that only shows. With no
// list for it, the endpoint is off and answers 404, as for a path that
// doesn't exist; a caller not listed gets 403.
func (s *server) admin(write bool, what string, next clientHandler) clientHandler {
	return func(w http.ResponseWriter, r *http.Request, client backend.Client) {
		admins := s.gateway.Config.Server.AdminAPI
		on, allowed, list := admins.ReadOn(), admins.CanRead(client.Name, client.Groups), "server.adminApi.read"
		if write {
			on, allowed, list = admins.Write.On(), admins.Write.On() && admins.Write.Allows(client.Name, client.Groups), "server.adminApi.write"
		}
		endpoint := strings.Replace(r.Pattern, "{path...}", "**", 1)
		if !on {
			s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", fmt.Sprintf("%s is off; list its admins in %s to turn it on", endpoint, list))
			return
		}
		if !allowed {
			s.reject(amfFamily, w, r, client, http.StatusForbidden, "permission_error", fmt.Sprintf("this client may not %s; %s doesn't list it", what, list))
			return
		}
		next(w, r, client)
	}
}

// configPath returns the path of a /amf/config request: its segments after
// /amf/config/, each unescaped, so a key holding a slash is sent as %2F.
func configPath(r *http.Request) (config.Path, error) {
	return pathAfter(r, "/amf/config/")
}

// pathAfter is configPath for an endpoint whose path follows prefix.
func pathAfter(r *http.Request, prefix string) (config.Path, error) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok || rest == "" {
		return nil, nil
	}
	var path config.Path
	for _, seg := range strings.Split(rest, "/") {
		key, err := url.PathUnescape(seg)
		if err != nil || key == "" {
			return nil, fmt.Errorf("'%s' is not a path of keys; separate keys with one '/', and escape a '/' inside a key as %%2F", rest)
		}
		path = append(path, key)
	}
	return path, nil
}

// responseFormat returns the format a client asks for in Accept: YAML for a
// YAML media type, else JSON.
func responseFormat(r *http.Request) config.Format {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && isYAML(mediaType) {
			return config.YAML
		}
	}
	return config.JSON
}

func isYAML(mediaType string) bool {
	switch mediaType {
	case "application/yaml", "application/x-yaml", "text/yaml", "text/x-yaml":
		return true
	}
	return false
}

// liveCopy reports whether a request asks for the running config, with
// ?live, rather than the draft.
func liveCopy(r *http.Request) bool {
	return r.URL.Query().Has("live")
}

// setConfigHeader says which copy a response is about; see configHeader.
func (s *server) setConfigHeader(w http.ResponseWriter, live bool) {
	s.live.stateMu.Lock()
	d, l := s.live.copies()
	s.live.stateMu.Unlock()
	if live {
		w.Header().Set(configHeader, fmt.Sprintf("live; source=%s; edited=%t; saved=%t", l.Source, l.Edited, l.Saved))
		return
	}
	w.Header().Set(configHeader, fmt.Sprintf("draft; source=%s; edited=%t; live=%t; saved=%t", d.Source, d.Edited, d.Live, d.Saved))
}

// getConfig serves GET /amf/config and its paths: the draft, or with ?live
// the running config, as config.Document.View shows it, with the node's
// ETag.
func (s *server) getConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	path, err := configPath(r)
	if err != nil {
		s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", err.Error())
		return
	}
	live := liveCopy(r)
	doc := s.live.Draft()
	if live {
		doc = s.live.Gateway().Config.Source
	}
	node, err := doc.View(path)
	if err != nil {
		s.editFailed(w, r, client, err)
		return
	}
	var body []byte
	format := responseFormat(r)
	if format == config.YAML {
		body, err = config.MarshalYAML(node)
	} else {
		body, err = config.MarshalJSON(node)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	tag, _ := doc.ETag(path)
	w.Header().Set("ETag", tag)
	s.setConfigHeader(w, live)
	if format == config.YAML {
		w.Header().Set("Content-Type", "application/yaml")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Write(body)
}

// editResult is the body of a PUT or DELETE that succeeded.
type editResult struct {
	Notes []string `json:"notes"`
}

// putConfig serves PUT /amf/config and its paths, which edit the draft: the
// body replaces the node at the path, or is added under a new key. It
// answers 201 for a node added, 200 for one replaced.
func (s *server) putConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	path, err := configPath(r)
	if err != nil {
		s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", err.Error())
		return
	}
	format := config.JSON
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		switch {
		case err == nil && isYAML(mediaType):
			format = config.YAML
		case err == nil && mediaType == "application/json":
		default:
			s.reject(amfFamily, w, r, client, http.StatusUnsupportedMediaType, "invalid_request_error", fmt.Sprintf("Content-Type '%s' is neither JSON (application/json) nor YAML (application/yaml)", contentType))
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.reject(amfFamily, w, r, client, http.StatusBadRequest, "invalid_request_error", "reading the body: "+err.Error())
		return
	}
	var created bool
	var notes []string
	draft, err := s.live.EditDraft(len(path) == 0, func(d *config.Document) (*config.Document, error) {
		if err := preconditions(r, d, path); err != nil {
			return nil, err
		}
		edited, c, n, err := d.Put(path, body, format)
		created, notes = c, n
		return edited, err
	})
	if err != nil {
		s.editFailed(w, r, client, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.edited(w, draft, path, status, notes)
	s.log.Info("draft edited", "by", "key "+client.Name, "path", path.String(), "created", created)
}

// deleteConfig serves DELETE on a path of /amf/config, which removes that
// node from the draft.
func (s *server) deleteConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	path, err := configPath(r)
	if err != nil || len(path) == 0 {
		s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", "DELETE takes a path of a node in the config, such as /amf/config/models/<name>")
		return
	}
	draft, err := s.live.EditDraft(false, func(d *config.Document) (*config.Document, error) {
		if err := preconditions(r, d, path); err != nil {
			return nil, err
		}
		return d.Delete(path)
	})
	if err != nil {
		s.editFailed(w, r, client, err)
		return
	}
	s.edited(w, draft, nil, http.StatusOK, nil)
	s.log.Info("draft edited", "by", "key "+client.Name, "path", path.String(), "deleted", true)
}

// renameConfig serves POST /amf/config-rename/<path>?to=<name>, which gives
// the key at the path a new name in the draft, in its place, keeping its
// value as the files hold it. If-Match, if sent, is checked against the
// node at the path; the reply carries the node's ETag, which a rename
// doesn't change.
func (s *server) renameConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	path, err := pathAfter(r, "/amf/config-rename/")
	if err != nil || len(path) == 0 {
		s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", "config-rename takes the path of a key, such as /amf/config-rename/models/<name>?to=<new name>")
		return
	}
	q := r.URL.Query()
	if !q.Has("to") {
		s.reject(amfFamily, w, r, client, http.StatusBadRequest, "invalid_request_error", "config-rename needs the new name, in ?to=")
		return
	}
	to := q.Get("to")
	var notes []string
	draft, err := s.live.EditDraft(false, func(d *config.Document) (*config.Document, error) {
		if err := preconditions(r, d, path); err != nil {
			return nil, err
		}
		renamed, n, err := d.Rename(path, to)
		notes = n
		return renamed, err
	})
	if err != nil {
		s.editFailed(w, r, client, err)
		return
	}
	s.edited(w, draft, append(slices.Clone(path[:len(path)-1]), to), http.StatusOK, notes)
	s.log.Info("draft edited", "by", "key "+client.Name, "path", path.String(), "renamed", to)
}

func (s *server) edited(w http.ResponseWriter, draft *config.Document, path config.Path, status int, notes []string) {
	if path != nil {
		if tag, ok := draft.ETag(path); ok {
			w.Header().Set("ETag", tag)
		}
	}
	s.setConfigHeader(w, false)
	if notes == nil {
		notes = []string{}
	}
	writeJSON(w, status, editResult{Notes: notes})
}

// preconditions checks a write's If-Match and If-None-Match against the node
// at path in d. Neither is required: a client that sends neither gets the
// write it asked for.
func preconditions(r *http.Request, d *config.Document, path config.Path) error {
	tag, exists := d.ETag(path)
	if inm := r.Header.Get("If-None-Match"); inm != "" && exists && (strings.TrimSpace(inm) == "*" || etagMatches(inm, tag)) {
		return fmt.Errorf("%w: %s exists, and If-None-Match asks for a write only if it doesn't", ErrPrecondition, path)
	}
	if im := r.Header.Get("If-Match"); im != "" && (!exists || !etagMatches(im, tag)) {
		if !exists {
			return fmt.Errorf("%w: %s doesn't exist, and If-Match asks for a write only if it does", ErrPrecondition, path)
		}
		return fmt.Errorf("%w: %s changed since its ETag %s was read; it is now %s", ErrPrecondition, path, strings.TrimSpace(im), tag)
	}
	return nil
}

// etagMatches reports whether a header listing ETags, or "*", names tag.
func etagMatches(header, tag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == tag {
			return true
		}
	}
	return false
}

// editFailed answers an edit or a read of the config that was refused.
func (s *server) editFailed(w http.ResponseWriter, r *http.Request, client backend.Client, err error) {
	var edit *config.EditError
	switch {
	case errors.Is(err, ErrPrecondition):
		s.reject(amfFamily, w, r, client, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.As(err, &edit) && edit.Kind == config.EditNotFound:
		s.reject(amfFamily, w, r, client, http.StatusNotFound, "not_found", err.Error())
	case errors.As(err, &edit) && edit.Kind == config.EditConflict:
		s.reject(amfFamily, w, r, client, http.StatusConflict, "conflict", err.Error())
	default:
		s.reject(amfFamily, w, r, client, http.StatusUnprocessableEntity, "invalid_config", err.Error())
	}
}

// changeResult is the body of a reload or an apply that succeeded.
type changeResult struct {
	Reloaded bool     `json:"reloaded,omitempty"`
	Applied  bool     `json:"applied,omitempty"`
	Models   int      `json:"models"`
	Warnings []string `json:"warnings"`
	Notes    []string `json:"notes"`
	// DiscardedDraft and DiscardedUnsaved say what a reload replaced: draft
	// edits not applied, and running changes not saved.
	DiscardedDraft   bool `json:"discardedDraft,omitempty"`
	DiscardedUnsaved bool `json:"discardedUnsaved,omitempty"`
}

func newChangeResult(g *Gateway) changeResult {
	result := changeResult{Models: len(g.Models), Warnings: []string{}, Notes: []string{}}
	for _, warning := range g.Warnings {
		result.Warnings = append(result.Warnings, warning.Error())
	}
	result.Notes = append(result.Notes, g.Config.Notes...)
	return result
}

// reloadConfig serves POST /amf/reload: a reload from the config
// files, as SIGHUP does, into the running config and the draft. A config
// that fails to load is a 422, a file that can't be read a 500, and either
// way the running gateway and the draft are kept. The gateway this request
// arrived on answers it, whatever replaces it.
func (s *server) reloadConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	reloaded, err := s.live.ReloadFiles("key " + client.Name)
	if err != nil {
		status, errType := http.StatusUnprocessableEntity, "invalid_config"
		var unread *fs.PathError
		if errors.As(err, &unread) {
			status, errType = http.StatusInternalServerError, "server_error"
		}
		writeOpenAIError(w, status, errType, "config reload failed; keeping the running config: "+err.Error())
		return
	}
	result := newChangeResult(reloaded.Gateway)
	result.Reloaded, result.DiscardedDraft, result.DiscardedUnsaved = true, reloaded.DiscardedDraft, reloaded.DiscardedUnsaved
	writeJSON(w, http.StatusOK, result)
}

// applyConfig serves POST /amf/config-apply, which puts the draft into
// service, as a reload does the files. If-Match, if sent, must be the
// draft's ETag, so an admin applies the draft they read.
func (s *server) applyConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	g, err := s.live.ApplyDraft("key "+client.Name, r.Header.Get("If-Match"))
	if errors.Is(err, ErrPrecondition) {
		writeOpenAIError(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusUnprocessableEntity, "invalid_config", "draft apply failed; keeping the running config: "+err.Error())
		return
	}
	result := newChangeResult(g)
	result.Applied = true
	writeJSON(w, http.StatusOK, result)
}

// revertConfig serves POST /amf/config-revert, which replaces the draft with
// the running config.
func (s *server) revertConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	if r.URL.Query().Has("backup") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "config-revert goes back to the running config only; load a backup into the draft with POST /amf/config-load?backup=<id>")
		return
	}
	s.live.RevertDraft("key " + client.Name)
	writeJSON(w, http.StatusOK, struct {
		Reverted bool `json:"reverted"`
	}{true})
}

// loadConfig serves POST /amf/config-load, which replaces the draft with the
// files as they are on disk now, or with ?backup= with a backup, named by its
// ID or by its index from the newest, -1 for the newest. The reply names the
// backup's ID, and in notes each secret put where the gateway's files keep
// it rather than where the backup had it.
func (s *server) loadConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	backup, notes, err := s.live.LoadDraft("key "+client.Name, r.URL.Query().Get("backup"))
	var unread *fs.PathError
	switch {
	case errors.Is(err, configfile.ErrNoBackup):
		writeOpenAIError(w, http.StatusNotFound, "not_found", err.Error()+"; GET /amf/config-backups lists them")
		return
	case errors.As(err, &unread):
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "config load failed; keeping the draft: "+err.Error())
		return
	case err != nil:
		writeOpenAIError(w, http.StatusUnprocessableEntity, "invalid_config", "config load failed; keeping the draft: "+err.Error())
		return
	}
	if notes == nil {
		notes = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Loaded bool     `json:"loaded"`
		Backup string   `json:"backup,omitempty"`
		Notes  []string `json:"notes"`
	}{true, backup, notes})
}

// saveConfig serves POST /amf/config-save, which writes the running config
// to the files. A save refused before it changed anything is a 409, one that
// failed while writing, and put the files back, a 500.
func (s *server) saveConfig(w http.ResponseWriter, r *http.Request, client backend.Client) {
	saved, err := s.live.SaveRunning("key "+client.Name, r.URL.Query().Has("force"))
	var saveErr *configfile.SaveError
	switch {
	case errors.As(err, &saveErr) && saveErr.Conflict:
		writeOpenAIError(w, http.StatusConflict, "conflict", "config save failed; nothing was written: "+err.Error())
		return
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "config save failed: "+err.Error())
		return
	}
	notes := saved.Notes
	if notes == nil {
		notes = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Saved  []string `json:"saved"`
		Backup string   `json:"backup,omitempty"`
		Notes  []string `json:"notes"`
	}{saved.Written, saved.Backup, notes})
}

// configStatus serves GET /amf/config-status.
func (s *server) configStatus(w http.ResponseWriter, r *http.Request, client backend.Client) {
	writeJSON(w, http.StatusOK, s.live.Status())
}

// configBackups serves GET /amf/config-backups, newest first.
func (s *server) configBackups(w http.ResponseWriter, r *http.Request, client backend.Client) {
	backups, err := s.live.Backups()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "listing the backups: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Backups []configfile.Backup `json:"backups"`
	}{backups})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
