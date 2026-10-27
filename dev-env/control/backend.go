package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A backend of the environment is a Compose service plus a kind, which knows
// how to read the backend's section of config.yaml and how to manage the
// models of the backend that section describes. Each kind is a file of its
// own, which registers its backends from an init function, so adding a
// backend adds a file and changes nothing the other backends use.

// backend is one backend of the environment. Its Name is what the API,
// config.yaml and its model IDs call it.
type backend struct {
	Name    string
	Service string // in compose.yaml
	Profile string // the profile that enables Service
	URL     string // inside the project network, by service name
	// NeedsFiles is set for a backend that reads the model files, so they
	// must be downloaded before it starts.
	NeedsFiles bool
	// Health is the path that answers 200 once the backend can serve
	// requests, which configured.ready checks by default.
	Health string
	// API is what Open WebUI can connect to it as: "openai", "ollama", or
	// "" for nothing.
	API  string
	kind kind
}

// kind reads a backend's section of config.yaml into the backend it
// configures. It is called only for a backend config.yaml has a section for.
type kind interface {
	configure(section *yaml.Node, files fileIndex) (configured, error)
}

// configured is a backend as config.yaml sets it up: what its container
// takes at start, and how control manages its models. Embedding basic gives
// every method a default, so a kind writes only what its backend does.
type configured interface {
	// service is the backend's part of the Compose override, or nil.
	service() *service
	// modelIDs are the IDs the config gives the backend, for a backend that
	// can't list its models at every moment.
	modelIDs() []string
	models(ctx context.Context, b backend) ([]model, error)
	// loading returns what loads and unloads the backend's models, or an
	// unsupported error saying why it can't, which the API answers with
	// before it starts a load in the background.
	loading(b backend) (loader, error)
	// reload makes the running backend read its config file again; done is
	// false when it does so in the background, after answering.
	reload(ctx context.Context, b backend, e *env) (done bool, err error)
	// setup is the backend's own setup, which runs once it is up, or nil.
	setup() setup
	// ready checks once, without waiting, whether the running backend can
	// serve requests: "" if it can, else what it is still waiting for.
	ready(ctx context.Context, b backend, e *env) (waiting string)
}

// loader loads and unloads a backend's models. load returns once the
// backend has taken the load on; unload once it has unloaded, or is about
// to, which the API waits out.
type loader interface {
	load(ctx context.Context, b backend, id string) error
	unload(ctx context.Context, b backend, id string) error
}

// setup is a backend's own setup step, for what can exist only once the
// backend runs, such as Ollama's imported models.
type setup interface {
	// running is what the step's detail says while a run is under way.
	running() string
	run(ctx context.Context, b backend, e *env, p *progress) error
	// state reads whether the step's result exists, while no run is under
	// way.
	state(ctx context.Context, b backend, e *env) step
}

// env is what a backend's operations can use besides its own address.
type env struct {
	store   string // the models store's mount point
	compose *compose
	log     *slog.Logger
}

// service is a backend's part of the Compose override: settings its
// container takes at start.
type service struct {
	Command     []string          `yaml:"command,omitempty"`
	Entrypoint  []string          `yaml:"entrypoint,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
}

// basic gives every configured method its default: no override, models
// listed by nothing, and load, unload and reload refused.
type basic struct{}

func (basic) service() *service  { return nil }
func (basic) modelIDs() []string { return nil }
func (basic) models(_ context.Context, b backend) ([]model, error) {
	return nil, unsupported("backend '%s' has no model list control can read", b.Name)
}
func (basic) loading(b backend) (loader, error) {
	return nil, unsupported("backend '%s' can't load or unload a model", b.Name)
}
func (basic) reload(_ context.Context, b backend, _ *env) (bool, error) {
	return false, unsupported("backend '%s' takes its settings at start and has no config to reload; stop and start it", b.Name)
}
func (basic) setup() setup { return nil }

// ready waits for the backend's Health path to answer.
func (basic) ready(ctx context.Context, b backend, _ *env) string {
	if getJSON(ctx, b.URL+b.Health, nil) != nil {
		return "waiting for it to answer " + b.Health
	}
	return ""
}

// unsupportedError is an action a backend can't do, which the API answers
// with 409 and its reason.
type unsupportedError struct{ reason string }

func (e *unsupportedError) Error() string { return e.reason }

func unsupported(format string, args ...any) error {
	return &unsupportedError{reason: fmt.Sprintf(format, args...)}
}

// backendModels is where compose.yaml mounts the models store in the
// backends' containers, for the model paths an override hands them.
const backendModels = "/models"

// errNoModel is a model the backend doesn't have.
var errNoModel = errors.New("no such model")

// backends is every backend of the environment, sorted by name; each kind's
// file adds its own (register).
var backends []backend

func register(b backend) {
	backends = append(backends, b)
	slices.SortFunc(backends, func(x, y backend) int { return strings.Compare(x.Name, y.Name) })
}

func findBackend(name string) (backend, bool) {
	for _, b := range backends {
		if b.Name == name {
			return b, true
		}
	}
	return backend{}, false
}

func backendNames() []string {
	var names []string
	for _, b := range backends {
		names = append(names, b.Name)
	}
	return names
}

// model is one model ID as GET /api/backends/{backend}/models reports it. State
// is the backend's own word for it, not mapped onto a common set, since the
// environment exists to show what each backend reports.
type model struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	AliasOf string `json:"aliasOf,omitempty"`
}

// modelConfig is one model ID on a backend and the file it loads, as a
// kind's section of config.yaml lists it.
type modelConfig struct {
	ID   string `yaml:"id"`
	File string `yaml:"file"`
}

// checkModels returns what is wrong with a backend's model list: an ID
// missing or given twice, or a file that files doesn't list.
func checkModels(backend string, ms []modelConfig, files fileIndex) []string {
	var problems []string
	seen := map[string]bool{}
	for _, m := range ms {
		switch {
		case m.ID == "":
			problems = append(problems, fmt.Sprintf("a model of '%s' has no id", backend))
		case seen[m.ID]:
			problems = append(problems, fmt.Sprintf("'%s' has model '%s' twice", backend, m.ID))
		case !files.has(m.File):
			problems = append(problems, fmt.Sprintf("model '%s' of '%s' loads '%s', which files doesn't list", m.ID, backend, m.File))
		}
		seen[m.ID] = true
	}
	return problems
}

func ids(ms []modelConfig) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// waitReady waits until the backend's ready check passes, after its
// container starts, keeping p's detail on what it is waiting for. The limit
// is long, since a backend's first start can include setting up its own
// store or database, or unpacking its engines.
func waitReady(ctx context.Context, b backend, c configured, e *env, p *progress) error {
	const limit = 5 * time.Minute
	deadline := time.Now().Add(limit)
	for {
		waiting := c.ready(ctx, b, e)
		if waiting == "" {
			return nil
		}
		p.setDetail(waiting)
		if time.Now().After(deadline) {
			return fmt.Errorf("backend '%s' wasn't ready within %v of starting: still %s", b.Name, limit, waiting)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
