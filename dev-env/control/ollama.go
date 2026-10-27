package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Ollama keeps its own copy of every model and doesn't load a GGUF by path,
// so its setup, and a reload, make its models match the config through its
// API once it is running (Ollama 0.40.8 API docs):
//
//  1. POST /api/show reports, for each model Ollama has, the blob it was
//     created from (its modelfile's FROM line) and its num_ctx, which are
//     compared with the config, and what Ollama found the model can do,
//     such as thinking and tool calls, which the setup step's detail shows.
//  2. HEAD /api/blobs/sha256:<digest> asks whether Ollama has a blob; if
//     not, POST to the same path streams it, and Ollama checks it against
//     the digest. The Ollama container never mounts the models store.
//  3. POST /api/create makes each model that is missing or differs, from
//     its blob, with the context size config.yaml gives, in place of
//     Ollama's 2048; a create replaces a model of the same name.
//  4. DELETE /api/delete removes each model the config doesn't list. It
//     comes last because Ollama deletes a blob no model uses any more,
//     which would otherwise be uploaded again for a model created from it.
//
// Ollama runs one runner per blob, not per model name (0.40.8 source,
// schedulerModelKey in server/sched.go; verified): models created from the
// same bytes share it, a request to any of them is served by whichever
// loaded it, and /api/ps lists only that one's name. So that two models can
// be loaded at once and a third evict one, each model in the config names a
// copy: 0 is its file as it is, n > 0 a copy whose general.name ends in "(n)",
// a blob of its own. Models with the same file and copy share a blob, as
// the config's ollama-llm-1 and -4 do, to show the sharing.

func init() {
	register(backend{
		Name: "ollama", Service: "ollama", Profile: "ollama", URL: "http://ollama:11434",
		NeedsFiles: true, API: "ollama", Health: "/api/version", kind: ollamaKind{},
	})
}

type ollamaKind struct{}

type ollama struct {
	basic
	MaxLoadedModels int           `yaml:"maxLoadedModels"`
	KeepAlive       string        `yaml:"keepAlive"`
	CtxSize         int           `yaml:"ctxSize"`
	Models          []ollamaModel `yaml:"models"`
	files           fileIndex
}

// ollamaModel is one Ollama model and the blob it is created from.
type ollamaModel struct {
	ID   string `yaml:"id"`
	File string `yaml:"file"`
	Copy int    `yaml:"copy"`
}

func (ollamaKind) configure(section *yaml.Node, files fileIndex) (configured, error) {
	o := ollama{files: files}
	if err := decodeSection(section, &o); err != nil {
		return nil, err
	}
	var specs []modelConfig
	var found []string
	for _, m := range o.Models {
		specs = append(specs, modelConfig{ID: m.ID, File: m.File})
		if m.Copy < 0 {
			found = append(found, fmt.Sprintf("model '%s' has a negative copy", m.ID))
		}
	}
	found = append(checkModels("ollama", specs, files), found...)
	return &o, problems(found)
}

func (o *ollama) service() *service {
	return &service{Environment: map[string]string{
		"OLLAMA_MAX_LOADED_MODELS": strconv.Itoa(o.MaxLoadedModels),
		"OLLAMA_KEEP_ALIVE":        o.KeepAlive,
	}}
}

func (o *ollama) modelIDs() []string {
	var out []string
	for _, m := range o.Models {
		out = append(out, m.ID)
	}
	return out
}

// models lists Ollama's models: /api/tags has every one, /api/ps the loaded
// ones; Ollama reports no loading state (verified on 0.40.8).
func (o *ollama) models(ctx context.Context, b backend) ([]model, error) {
	tags, err := ollamaNames(ctx, b.URL+"/api/tags")
	if err != nil {
		return nil, err
	}
	loadedNames, err := ollamaNames(ctx, b.URL+"/api/ps")
	if err != nil {
		return nil, err
	}
	var models []model
	for _, name := range tags {
		state := "unloaded"
		if slices.Contains(loadedNames, name) {
			state = "loaded"
		}
		models = append(models, model{ID: name, State: state})
	}
	return models, nil
}

func (o *ollama) loading(backend) (loader, error) { return o, nil }

// load sends a generate request with no prompt, which loads the model and
// answers once it is loaded (Ollama API docs).
func (o *ollama) load(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/api/generate", map[string]any{"model": id, "stream": false})
}

// unload sends the same with keep_alive 0 (Ollama API docs).
func (o *ollama) unload(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/api/generate", map[string]any{"model": id, "keep_alive": 0, "stream": false})
}

// reload makes the running Ollama's models match the config, as its setup
// does, and answers once they do. Its server settings, maxLoadedModels and
// keepAlive, are environment variables, which take a stop and start.
func (o *ollama) reload(ctx context.Context, b backend, e *env) (bool, error) {
	return true, o.run(ctx, b, e, &progress{})
}

func (o *ollama) setup() setup { return o }

func (o *ollama) running() string { return "making its models match the config" }

// ollamaNames returns the model names in an Ollama list, /api/tags or
// /api/ps.
func ollamaNames(ctx context.Context, target string) ([]string, error) {
	var list struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := getJSON(ctx, target, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range list.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// ollamaPlan is what it takes to make Ollama's models match the config.
type ollamaPlan struct {
	create       []ollamaModel // missing, or differing from the config
	remove       []string      // names Ollama has and the config doesn't list
	changes      []string      // what differs, for the setup step's detail
	capabilities []string      // what each matching model can do
}

func (p ollamaPlan) matches() bool { return len(p.create) == 0 && len(p.remove) == 0 }

// plan compares Ollama's models with the config: by name, by the blob each
// was created from, and by num_ctx.
func (o *ollama) plan(ctx context.Context, b backend, e *env) (ollamaPlan, error) {
	have, err := ollamaNames(ctx, b.URL+"/api/tags")
	if err != nil {
		return ollamaPlan{}, err
	}
	var p ollamaPlan
	listed := map[string]bool{}
	for _, m := range o.Models {
		name := m.ID + ":latest"
		listed[name] = true
		if !slices.Contains(have, name) {
			p.create = append(p.create, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' is missing", m.ID))
			continue
		}
		var show struct {
			Modelfile    string   `json:"modelfile"`
			Capabilities []string `json:"capabilities"`
		}
		if err := postJSONOut(ctx, b.URL+"/api/show", map[string]string{"model": name}, &show); err != nil {
			return ollamaPlan{}, err
		}
		want, err := o.blob(e.store, m.File, m.Copy)
		if err != nil {
			return ollamaPlan{}, err
		}
		from, ctxSize := modelfileFacts(show.Modelfile)
		switch {
		case from != want.digest:
			p.create = append(p.create, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' is made from blob '%s', not '%s'", m.ID, from, want.digest))
		case ctxSize != o.CtxSize:
			p.create = append(p.create, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' has num_ctx %d, not %d", m.ID, ctxSize, o.CtxSize))
		default:
			p.capabilities = append(p.capabilities, m.ID+": "+strings.Join(show.Capabilities, ", "))
		}
	}
	for _, name := range have {
		if !listed[name] {
			p.remove = append(p.remove, name)
			p.changes = append(p.changes, fmt.Sprintf("'%s' isn't in the config", name))
		}
	}
	return p, nil
}

// modelfileFacts reads a modelfile, as /api/show returns it, for the digest
// of the blob in its FROM line and its num_ctx, 0 if it sets none.
func modelfileFacts(modelfile string) (digest string, ctxSize int) {
	for _, line := range strings.Split(modelfile, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "FROM":
			if hex, ok := strings.CutPrefix(filepath.Base(fields[1]), "sha256-"); ok {
				digest = "sha256:" + hex
			}
		case len(fields) == 3 && fields[0] == "PARAMETER" && fields[1] == "num_ctx":
			ctxSize, _ = strconv.Atoi(fields[2])
		}
	}
	return digest, ctxSize
}

// run makes Ollama's models match the config: it creates what is missing
// or differs, then deletes what the config doesn't list.
func (o *ollama) run(ctx context.Context, b backend, e *env, p *progress) error {
	plan, err := o.plan(ctx, b, e)
	if err != nil {
		return err
	}
	uploaded := map[string]bool{} // digests
	for _, m := range plan.create {
		bl, err := o.blob(e.store, m.File, m.Copy)
		if err != nil {
			return err
		}
		if !uploaded[bl.digest] {
			if err := upload(ctx, b, e, bl, p); err != nil {
				return err
			}
			uploaded[bl.digest] = true
		}
		body := map[string]any{
			"model":      m.ID,
			"files":      map[string]string{filepath.Base(m.File): bl.digest},
			"parameters": map[string]any{"num_ctx": o.CtxSize},
			"stream":     false,
		}
		if err := postJSON(ctx, b.URL+"/api/create", body); err != nil {
			return fmt.Errorf("creating '%s': %w", m.ID, err)
		}
		e.log.Info("created", "backend", b.Name, "model", m.ID, "blob", bl.digest)
	}
	for _, name := range plan.remove {
		if err := sendJSON(ctx, http.MethodDelete, b.URL+"/api/delete", "", map[string]string{"model": name}, nil); err != nil {
			return fmt.Errorf("deleting '%s': %w", name, err)
		}
		e.log.Info("deleted", "backend", b.Name, "model", name)
	}
	return nil
}

// state reports the step done once Ollama's models match the config, with
// what each can do, and otherwise what differs.
func (o *ollama) state(ctx context.Context, b backend, e *env) step {
	s := step{Name: b.Name}
	plan, err := o.plan(ctx, b, e)
	switch {
	case err != nil:
		s.State, s.Detail = stateFailed, err.Error()
	case !plan.matches():
		s.State, s.Detail = statePending, "differs from the config: "+strings.Join(plan.changes, "; ")
	default:
		s.State, s.Detail = stateDone, "capabilities: "+strings.Join(plan.capabilities, "; ")
	}
	return s
}

// blob is one blob to give Ollama: a model file, or a renamed copy of it,
// read through open.
type blob struct {
	digest string
	size   int64
	open   func() (io.ReadCloser, error)
}

// copyDigests holds each copy's digest by its original's sha256 and its
// copy number, for as long as control runs, since the setup state compares
// digests on every read and computing one reads the whole file.
var copyDigests sync.Map

// blob returns the blob for copy n of file. The original's digest is the
// sha256 in the config; a copy's is computed by reading it once, since its
// bytes exist only as the stream sent. The same file and n give the same
// bytes on every run, so Ollama already has a blob made before.
func (o *ollama) blob(store, file string, n int) (blob, error) {
	name := filepath.Join(store, file)
	info, err := os.Stat(name)
	if err != nil {
		return blob{}, err
	}
	original := func() (io.ReadCloser, error) { return os.Open(name) }
	if n == 0 {
		return blob{digest: "sha256:" + o.files[file].SHA256, size: info.Size(), open: original}, nil
	}
	title, offset, err := ggufString(name, "general.name")
	if err != nil {
		return blob{}, err
	}
	renamed, err := copyName(title, n)
	if err != nil {
		return blob{}, err
	}
	open := func() (io.ReadCloser, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		end := offset + int64(len(title))
		r := io.MultiReader(io.NewSectionReader(f, 0, offset), strings.NewReader(renamed), io.NewSectionReader(f, end, info.Size()-end))
		return struct {
			io.Reader
			io.Closer
		}{r, f}, nil
	}
	key := fmt.Sprintf("%s#%d", o.files[file].SHA256, n)
	if digest, ok := copyDigests.Load(key); ok {
		return blob{digest: digest.(string), size: info.Size(), open: open}, nil
	}
	r, err := open()
	if err != nil {
		return blob{}, err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return blob{}, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	copyDigests.Store(key, digest)
	return blob{digest: digest, size: info.Size(), open: open}, nil
}

// copyName is title with its end replaced by "(n)", the same length, so the
// copy's metadata keeps every offset of the original's.
func copyName(title string, n int) (string, error) {
	suffix := fmt.Sprintf("(%d)", n)
	if len(suffix) >= len(title) {
		return "", fmt.Errorf("general.name '%s' is too short to mark as copy %d", title, n)
	}
	return title[:len(title)-len(suffix)] + suffix, nil
}

// upload sends a blob to Ollama unless it has it.
func upload(ctx context.Context, b backend, e *env, bl blob, p *progress) error {
	target := b.URL + "/api/blobs/" + bl.digest
	head, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(head)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	r, err := bl.open()
	if err != nil {
		return err
	}
	defer r.Close()
	p.total.Store(bl.size)
	p.done.Store(0)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &counting{r: r, n: &p.done})
	if err != nil {
		return err
	}
	req.ContentLength = bl.size
	e.log.Info("uploading", "backend", b.Name, "blob", bl.digest, "bytes", bl.size)
	return do(req, nil)
}
