package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
	"github.com/MadReasonable/ai-model-facade/internal/virtualmodel"
)

// The admin endpoints keep three copies of the config: the files, the
// running config, and one draft that every admin shares. PUT and DELETE edit
// the draft; apply copies it over the running config, revert copies the
// running config over it, a load copies the files or a backup over it, a
// reload copies the files over both, and a save copies the running config
// to the files. Live holds the draft and the files' last-known content under
// stateMu, and each copy's provenance; its mu orders everything that
// changes the running config or the files.

// The sources a copy of the config can come from, as /amf/config-status
// reports them.
const (
	sourceFiles = "files" // read from the files: at startup, on a reload, by a load, or written by a save
	sourceAPI   = "api"   // sent entire, by a PUT of the config itself
)

func sourceBackup(id string) string { return "backup:" + id }

// provenance is where a copy of the config came from, and whether a path
// PUT or DELETE has changed it since, even if undone.
type provenance struct {
	source string
	edited bool
}

// draftCopy is the draft: its content, its provenance, and loaded, the files
// POST /amf/config-load read into it, kept through its edits, so applying it
// can adopt them (filesHold); nil for a draft from anywhere else.
type draftCopy struct {
	doc *config.Document
	provenance
	loaded *config.Document
}

// ErrPrecondition is an If-Match or If-None-Match that the copy it names
// doesn't meet.
var ErrPrecondition = errors.New("precondition failed")

// Draft returns the draft.
func (l *Live) Draft() *config.Document {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	return l.draft.doc
}

// EditDraft replaces the draft with what edit returns for it, if edit
// succeeds and the result passes CheckDraft; whole says edit replaces the
// entire draft, which then counts as the API's, rather than editing a part
// of it. edit runs under the lock of the draft, so it can check an ETag
// against the draft it edits.
func (l *Live) EditDraft(whole bool, edit func(*config.Document) (*config.Document, error)) (*config.Document, error) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	d, err := edit(l.draft.doc)
	if err != nil {
		return nil, err
	}
	if err := CheckDraft(d); err != nil {
		return nil, &config.EditError{Kind: config.EditInvalid, Err: err}
	}
	l.draft.doc = d
	if whole {
		l.draft.provenance = provenance{source: sourceAPI}
	} else {
		l.draft.edited = true
	}
	return d, nil
}

// CheckDraft checks what each part of a draft can get wrong by itself, each
// backend and facade entry included, without contacting a backend; what
// spans parts is checked when it is applied.
func CheckDraft(d *config.Document) error {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	return d.CheckSchema(os.LookupEnv, func(section string, e config.Entry) error {
		if section == "backends" {
			_, err := newBackend(e, discard)
			return err
		}
		return virtualmodel.Check(e)
	})
}

// ApplyDraft builds a gateway from the draft and swaps it in, as a reload
// does from the files, if ifMatch is empty or the draft's ETag. It refuses a
// draft that would leave no key in server.apiKeys able to use the write
// endpoints, since nothing over the API could then undo it.
func (l *Live) ApplyDraft(by, ifMatch string) (*Gateway, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stateMu.Lock()
	draft := l.draft
	l.stateMu.Unlock()
	if tag, _ := draft.doc.ETag(nil); ifMatch != "" && !etagMatches(ifMatch, tag) {
		return nil, fmt.Errorf("%w: the draft changed since its ETag '%s' was read", ErrPrecondition, ifMatch)
	}
	cfg, err := draft.doc.Parse(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if !cfg.Server.Writers() {
		return nil, fmt.Errorf("server.adminApi.write: the draft leaves no key in server.apiKeys that may change the config again, so it isn't applied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
	defer cancel()
	g, err := l.apply(ctx, cfg)
	if err != nil {
		l.log.Error("draft apply failed; keeping the running config", "by", by, "error", err)
		return nil, err
	}
	adopted := draft.loaded != nil && l.filesHold(draft.loaded)
	l.stateMu.Lock()
	l.running = draft.provenance
	if adopted {
		l.saved = draft.loaded
	}
	l.stateMu.Unlock()
	l.log.Info("draft applied", "by", by, "models", len(g.Models), "adoptedFiles", adopted)
	return g, nil
}

// filesHold reports whether the files still hold what loaded was read from.
// An apply of a draft that came from POST /amf/config-load, edited or not,
// adopts those files as the last-known ones if so: the admin has seen them,
// so a save over them overwrites nothing unseen, and they stop counting as
// changed on disk. Files edited again since the load stay unseen.
func (l *Live) filesHold(loaded *config.Document) bool {
	if l.files == nil || loaded.Origin == nil {
		return false
	}
	current, err := l.files.Read()
	return err == nil && *current.Origin == *loaded.Origin
}

// RevertDraft replaces the draft with the running config, discarding every
// edit not applied.
func (l *Live) RevertDraft(by string) {
	l.stateMu.Lock()
	l.draft = draftCopy{doc: l.Gateway().Config.Source.Copy(), provenance: l.running}
	l.stateMu.Unlock()
	l.log.Info("draft reverted", "by", by)
}

// LoadDraft replaces the draft with the files as they are on disk now, or,
// with backup set, with that backup, by ID or by index from the newest, -1
// for the newest; it returns the backup's ID. What it loads must pass
// CheckDraft. A backup made with the other layout, with or without a secrets
// file, has its secrets put where the gateway's own files keep them, which
// notes list (config.Document.Fit). Nothing runs until the draft is applied.
func (l *Live) LoadDraft(by, backup string) (id string, notes []string, err error) {
	if l.files == nil {
		return "", nil, fmt.Errorf("the gateway has no config files to load")
	}
	var draft draftCopy
	if backup == "" {
		doc, err := l.files.Read()
		if err != nil {
			return "", nil, err
		}
		draft = draftCopy{doc: doc, provenance: provenance{source: sourceFiles}, loaded: doc}
	} else {
		var doc *config.Document
		if id, doc, err = l.files.ReadBackup(backup); err != nil {
			return "", nil, err
		}
		if doc, notes, err = doc.Fit(l.Gateway().Config.Source.Secrets != nil); err != nil {
			return "", nil, fmt.Errorf("backup '%s': %w", id, err)
		}
		draft = draftCopy{doc: doc, provenance: provenance{source: sourceBackup(id)}}
	}
	if err := CheckDraft(draft.doc); err != nil {
		return "", nil, &config.EditError{Kind: config.EditInvalid, Err: err}
	}
	l.stateMu.Lock()
	l.draft = draft
	l.stateMu.Unlock()
	l.log.Info("draft loaded", "by", by, "from", draft.source)
	return id, notes, nil
}

// SaveRunning writes the running config to the files; see
// configfile.Files.Save.
func (l *Live) SaveRunning(by string, force bool) (*configfile.Saved, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.files == nil {
		return nil, &configfile.SaveError{Conflict: true, Err: fmt.Errorf("the gateway has no config files to save to")}
	}
	cfg := l.Gateway().Config
	l.stateMu.Lock()
	lastKnown := l.saved
	l.stateMu.Unlock()
	saved, err := l.files.Save(cfg.Source, lastKnown, force, cfg.Server.AdminAPI.KeepBackups())
	if err != nil {
		l.log.Error("config save failed", "by", by, "error", err)
		return nil, err
	}
	l.stateMu.Lock()
	l.saved = saved.Document
	l.running = provenance{source: sourceFiles} // what runs is what the files hold, as after a reload of them
	l.stateMu.Unlock()
	l.log.Info("config saved", "by", by, "files", saved.Written, "backup", saved.Backup)
	return saved, nil
}

// Status is where the draft and the running config came from, how the three
// copies of the config compare, and what a save would find of the files.
// "Saved" compares with the files as the gateway last read or wrote them;
// whether a file changed on disk since is each file's ChangedOnDisk.
type Status struct {
	Draft   DraftStatus               `json:"draft"`
	Live    LiveStatus                `json:"live"`
	Files   []configfile.FileStatus   `json:"files"`
	Backups *configfile.BackupsStatus `json:"backups,omitempty"`
}

// DraftStatus is where the draft came from, whether it was edited since,
// and whether it holds what is running and what is saved.
type DraftStatus struct {
	Source string `json:"source"`
	Edited bool   `json:"edited"`
	Live   bool   `json:"live"`
	Saved  bool   `json:"saved"`
}

// LiveStatus is where the running config came from, whether it was edited
// since, and whether it holds what is saved.
type LiveStatus struct {
	Source string `json:"source"`
	Edited bool   `json:"edited"`
	Saved  bool   `json:"saved"`
}

// copies compares the draft and the running config with each other and
// with the files as last read or written. The caller holds l.stateMu.
func (l *Live) copies() (DraftStatus, LiveStatus) {
	running := l.Gateway().Config.Source
	return DraftStatus{Source: l.draft.source, Edited: l.draft.edited, Live: l.draft.doc.Equal(running), Saved: l.draft.doc.Equal(l.saved)},
		LiveStatus{Source: l.running.source, Edited: l.running.edited, Saved: running.Equal(l.saved)}
}

// Status returns how the copies stand, and what a save would find of the
// files. The files are checked after the lock is released, since that is
// file I/O.
func (l *Live) Status() Status {
	l.stateMu.Lock()
	s := Status{Files: []configfile.FileStatus{}}
	s.Draft, s.Live = l.copies()
	saved := l.saved
	l.stateMu.Unlock()
	if l.files != nil {
		files, backups := l.files.Status(saved, l.Gateway().Config.Server.AdminAPI.KeepBackups())
		s.Files, s.Backups = files, &backups
	}
	return s
}

// Backups lists the backups of the files, newest first.
func (l *Live) Backups() ([]configfile.Backup, error) {
	if l.files == nil {
		return []configfile.Backup{}, nil
	}
	return l.files.Backups()
}
