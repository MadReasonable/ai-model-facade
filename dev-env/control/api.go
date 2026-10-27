package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

// api serves the HTTP API. Every POST means "make it so": asked for what is
// already true it answers 200 with the current state and does nothing, and
// work that takes more than a moment answers 202 at once and goes on in the
// background, where the matching GET shows it. What a backend can do is its
// kind's; the API only looks the backend up and asks it.
type api struct {
	config func() (*config, error) // reads dev-env/config.yaml afresh
	files  *files
	jobs   *jobs
	env    *env
	log    *slog.Logger
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/setup", a.getSetup)
	mux.HandleFunc("POST /api/setup", a.postSetup)
	mux.HandleFunc("GET /api/files", a.getFiles)
	mux.HandleFunc("POST /api/files/download", a.postFilesDownload)
	mux.HandleFunc("POST /api/files/prune", a.postFilesPrune)
	mux.HandleFunc("POST /api/stop", a.postStopAll)
	mux.HandleFunc("GET /api/backends", a.getBackends)
	mux.HandleFunc("GET /api/backends/{backend}", a.withBackend(a.getBackend))
	mux.HandleFunc("POST /api/backends/{backend}/start", a.withBackend(a.postStart))
	mux.HandleFunc("POST /api/backends/{backend}/stop", a.withBackend(a.postStop))
	mux.HandleFunc("POST /api/backends/{backend}/reload", a.withBackend(a.postReload))
	mux.HandleFunc("GET /api/backends/{backend}/setup", a.withBackend(a.getBackendSetup))
	mux.HandleFunc("POST /api/backends/{backend}/setup", a.withBackend(a.postBackendSetup))
	mux.HandleFunc("GET /api/backends/{backend}/models", a.withBackend(a.getModels))
	mux.HandleFunc("POST /api/backends/{backend}/models/{model}/load", a.withBackend(a.postLoad))
	mux.HandleFunc("POST /api/backends/{backend}/models/{model}/unload", a.withBackend(a.postUnload))
	return mux
}

// target is a backend with what config.yaml makes of it; c is nil for a
// backend config.yaml has no section for, which GET requests still report
// and every other request refuses.
type target struct {
	backend
	c configured
}

// withBackend resolves the {backend} wildcard and reads the config,
// answering 404 for a name the environment doesn't have.
func (a *api) withBackend(handle func(http.ResponseWriter, *http.Request, target)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("backend")
		b, ok := findBackend(name)
		if !ok {
			a.problemText(w, r, http.StatusNotFound, "no backend '"+name+"'; the environment has "+joinQuoted(backendNames()))
			return
		}
		c, err := a.config()
		if err != nil {
			a.problem(w, r, http.StatusInternalServerError, err)
			return
		}
		t := target{b, c.configured[b.Name]}
		if t.c == nil && r.Method != http.MethodGet {
			a.notConfigured(w, r, t)
			return
		}
		handle(w, r, t)
	}
}

func (a *api) notConfigured(w http.ResponseWriter, r *http.Request, t target) {
	a.problemText(w, r, http.StatusConflict, "config.yaml has no section for backend '"+t.Name+"', so it isn't configured; add one under backends")
}

// step is one setup step's state.
type step struct {
	Name  string `json:"name"`
	State string `json:"state"` // done, pending, running or failed
	// Detail says what is happening, or why the step failed, for people:
	// a client decides by State, never by Detail.
	Detail   string    `json:"detail"`
	Progress *byteDone `json:"progress,omitempty"`
	// Key is a credential the step made, which the setup state reports
	// rather than the step: Open WebUI's API key.
	Key string `json:"-"`
}

type setupStatus struct {
	Ready bool   `json:"ready"`
	Steps []step `json:"steps"`
	// Key is the credential a backend's setup made, such as Open WebUI's
	// API key for the gateway's openWebUi backend. control is only on
	// 127.0.0.1, behind the same port that already controls Docker.
	Key string `json:"key,omitempty"`
}

// filesStep sums the files' states into one step: failed or running if any
// file is, done if all are.
func (a *api) filesStep() (step, error) {
	statuses, err := a.files.status()
	if err != nil {
		return step{}, err
	}
	s := step{Name: "files", State: stateDone}
	for _, f := range statuses {
		switch f.State {
		case stateFailed:
			s.State, s.Detail = stateFailed, f.Detail
		case stateRunning:
			s.State, s.Progress, s.Detail = stateRunning, f.Progress, "downloading '"+f.Path+"'"
		case statePending:
			if s.State == stateDone {
				s.State = statePending
			}
		}
	}
	return s, nil
}

// setupSteps lists the environment's own steps for a nil t, which so far
// are the files, or the steps a backend needs: the files it reads, and its
// own setup, which runs once it is up. A backend's own steps aren't part of
// the environment's, since running them means starting it.
func (a *api) setupSteps(ctx context.Context, t *target) (setupStatus, error) {
	status := setupStatus{Ready: true, Steps: []step{}}
	add := func(s step) {
		status.Steps = append(status.Steps, s)
		if s.State != stateDone {
			status.Ready = false
		}
	}
	if t == nil || t.NeedsFiles {
		s, err := a.filesStep()
		if err != nil {
			return setupStatus{}, err
		}
		add(s)
	}
	if t != nil && t.c != nil {
		if su := t.c.setup(); su != nil {
			s := a.ownStep(ctx, *t, su)
			add(s)
			status.Key = s.Key
		}
	}
	return status, nil
}

func setupJob(b backend) string { return "setup:" + b.Name }

// ownStep is the state of a backend's own setup: running while its start
// or its run is under way, failed if the last run failed, pending while the
// backend is down, and otherwise what the step itself finds.
func (a *api) ownStep(ctx context.Context, t target, su setup) step {
	s := step{Name: t.Name}
	run := a.jobs.last(setupJob(t.backend))
	switch {
	case run != nil && !run.finished():
		s.State, s.Detail = stateRunning, su.running()
		if total := run.progress.total.Load(); total > 0 {
			s.Progress = &byteDone{Done: run.progress.done.Load(), Total: total}
		}
		return s
	case a.starting(t.backend):
		s.State, s.Detail = stateRunning, "waiting for '"+t.Name+"' to start"
		return s
	case run != nil && run.err != nil:
		s.State, s.Detail = stateFailed, run.err.Error()
		return s
	}
	if container, err := a.env.compose.state(ctx, t.Service); err != nil || container != "running" {
		s.State, s.Detail = statePending, "runs when '"+t.Name+"' starts"
		return s
	}
	return su.state(ctx, t.backend, a.env)
}

func (a *api) getSetup(w http.ResponseWriter, r *http.Request) {
	status, err := a.setupSteps(r.Context(), nil)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *api) postSetup(w http.ResponseWriter, r *http.Request) {
	a.runSetup(w, r, nil)
}

// runSetup starts whatever setup isn't done, and answers with the state. A
// backend with a setup of its own, which needs it running, is started for
// it.
func (a *api) runSetup(w http.ResponseWriter, r *http.Request, t *target) {
	status, err := a.setupSteps(r.Context(), t)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	if status.Ready {
		writeJSON(w, http.StatusOK, status)
		return
	}
	if t != nil && t.c.setup() != nil {
		a.startBackend(*t)
	} else {
		a.files.download()
	}
	if status, err = a.setupSteps(r.Context(), t); err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (a *api) getFiles(w http.ResponseWriter, r *http.Request) {
	statuses, err := a.files.status()
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (a *api) postFilesDownload(w http.ResponseWriter, r *http.Request) {
	ready, err := a.files.ready()
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	code := http.StatusOK
	if !ready {
		a.files.download()
		code = http.StatusAccepted
	}
	statuses, err := a.files.status()
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, code, statuses)
}

// postFilesPrune removes the files the config doesn't list that control
// downloaded (files.prune), answering with their paths. It refuses while a
// download runs, since the file being written may be one of them, and while
// a backend that uses the files runs, since it may have started with a
// config that still lists one.
func (a *api) postFilesPrune(w http.ResponseWriter, r *http.Request) {
	if run := a.jobs.last(filesJob); run != nil && !run.finished() {
		a.problemText(w, r, http.StatusConflict, "a download is under way; prune once it has finished (GET /api/files shows it)")
		return
	}
	var using []string
	for _, b := range backends {
		if !b.NeedsFiles {
			continue
		}
		container, err := a.env.compose.state(r.Context(), b.Service)
		if err != nil {
			a.problem(w, r, http.StatusInternalServerError, err)
			return
		}
		if container == "running" {
			using = append(using, b.Name)
		}
	}
	if len(using) > 0 {
		a.problemText(w, r, http.StatusConflict, "backends that use the model files are running: "+joinQuoted(using)+"; stop them first (POST /api/stop stops them all)")
		return
	}
	removed, err := a.files.prune()
	for _, path := range removed {
		a.log.Info("pruned", "file", path)
	}
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"removed": removed})
}

// backendStatus is one backend as GET /api/backends reports it. State is
// what a client decides by, always one of stopped, starting, ready and
// failed; Detail says what a start is doing, or why it failed, for people.
type backendStatus struct {
	Name      string      `json:"name"`
	Service   string      `json:"service"`
	Profile   string      `json:"profile"`
	State     string      `json:"state"`
	Detail    string      `json:"detail"`
	Container string      `json:"container"` // Docker's state, such as running or exited, or absent
	Setup     setupStatus `json:"setup"`
}

// A backend's states, as backendStatus reports them; failed is shared with
// the steps.
const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateReady    = "ready"
)

// backendState decides a backend's state, in this order: a start under way
// is starting, with what it is doing; a backend whose ready check passes is
// ready, however it was started; one whose last start failed is failed,
// with why; one whose container runs but isn't ready yet is starting, with
// what it waits for; and anything else is stopped.
func backendState(starting bool, doing string, failed error, running bool, waiting string) (state, detail string) {
	switch {
	case starting:
		return stateStarting, doing
	case running && waiting == "":
		return stateReady, ""
	case failed != nil:
		return stateFailed, failed.Error()
	case running:
		return stateStarting, waiting
	default:
		return stateStopped, ""
	}
}

func (a *api) backendStatus(ctx context.Context, t target) (backendStatus, error) {
	container, err := a.env.compose.state(ctx, t.Service)
	if err != nil {
		return backendStatus{}, err
	}
	setup, err := a.setupSteps(ctx, &t)
	if err != nil {
		return backendStatus{}, err
	}
	status := backendStatus{Name: t.Name, Service: t.Service, Profile: t.Profile, Container: container, Setup: setup}
	var starting bool
	var doing, waiting string
	var failed error
	if run := a.jobs.last(startJob(t.backend)); run != nil {
		starting, doing = !run.finished(), run.progress.getDetail()
		if run.finished() {
			failed = run.err
		}
	}
	if container == "running" && !starting {
		waiting = t.check().ready(ctx, t.backend, a.env)
	}
	status.State, status.Detail = backendState(starting, doing, failed, container == "running", waiting)
	return status, nil
}

// check is what checks the backend's readiness: its kind's, or for a
// backend config.yaml has no section for, the default.
func (t target) check() configured {
	if t.c == nil {
		return basic{}
	}
	return t.c
}

func (a *api) getBackends(w http.ResponseWriter, r *http.Request) {
	c, err := a.config()
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	statuses := []backendStatus{}
	for _, b := range backends {
		status, err := a.backendStatus(r.Context(), target{b, c.configured[b.Name]})
		if err != nil {
			a.problem(w, r, http.StatusInternalServerError, err)
			return
		}
		statuses = append(statuses, status)
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (a *api) getBackend(w http.ResponseWriter, r *http.Request, t target) {
	a.writeBackend(w, r, http.StatusOK, t)
}

func (a *api) writeBackend(w http.ResponseWriter, r *http.Request, code int, t target) {
	status, err := a.backendStatus(r.Context(), t)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, code, status)
}

func startJob(b backend) string { return "start:" + b.Name }

// starting reports whether b's start job is running.
func (a *api) starting(b backend) bool {
	run := a.jobs.last(startJob(b))
	return run != nil && !run.finished()
}

// postStart starts the backend with its setup done. A backend that is
// ready with its setup done answers 200.
func (a *api) postStart(w http.ResponseWriter, r *http.Request, t target) {
	status, err := a.backendStatus(r.Context(), t)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	if status.State == stateReady && status.Setup.Ready {
		a.writeBackend(w, r, http.StatusOK, t)
		return
	}
	a.startBackend(t)
	a.writeBackend(w, r, http.StatusAccepted, t)
}

// startBackend starts, or joins, the job that brings a backend up with its
// setup done: the files it reads downloaded, its container started, then,
// once its ready check passes, its own setup run. The job's detail says
// which of these it is doing.
// Starting a running container changes nothing, so the job also serves a
// running backend whose own setup isn't done.
func (a *api) startBackend(t target) *job {
	return a.jobs.start(startJob(t.backend), func(ctx context.Context, p *progress) error {
		if t.NeedsFiles {
			p.setDetail("downloading the model files")
			if err := a.files.download().wait(ctx); err != nil {
				return err
			}
		}
		p.setDetail("starting its container")
		err := a.env.compose.start(ctx, t.Profile, t.Service)
		a.logResult("start", err, "backend", t.Name)
		if err != nil {
			return err
		}
		if err := waitReady(ctx, t.backend, t.c, a.env, p); err != nil {
			return err
		}
		su := t.c.setup()
		if su == nil {
			return nil
		}
		p.setDetail(su.running())
		return a.jobs.start(setupJob(t.backend), func(ctx context.Context, p *progress) error {
			err := su.run(ctx, t.backend, a.env, p)
			a.logResult("setup", err, "backend", t.Name)
			return err
		}).wait(ctx)
	})
}

func (a *api) postStop(w http.ResponseWriter, r *http.Request, t target) {
	if err := a.env.compose.stop(r.Context(), t.backend); err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	a.log.Info("stop", "backend", t.Name)
	a.writeBackend(w, r, http.StatusOK, t)
}

// postStopAll stops every backend and answers once they have stopped.
func (a *api) postStopAll(w http.ResponseWriter, r *http.Request) {
	if err := a.stopAll(r.Context()); err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	a.getBackends(w, r)
}

// stopAll stops every backend that has a container, configured or not, and
// returns once they have stopped. A container stopped some other way is
// removed too, since it keeps the network as a running one does.
func (a *api) stopAll(ctx context.Context) error {
	var present []backend
	for _, b := range backends {
		container, err := a.env.compose.state(ctx, b.Service)
		if err != nil {
			return err
		}
		if container != "absent" {
			present = append(present, b)
		}
	}
	if len(present) == 0 {
		return nil
	}
	if err := a.env.compose.stop(ctx, present...); err != nil {
		return err
	}
	for _, b := range present {
		a.log.Info("stop", "backend", b.Name)
	}
	return nil
}

// postReload makes a running backend read its config file again without
// restarting, answering 200 with its models if it has done so, and 202 if
// it does so in the background, where GET .../models shows the result.
func (a *api) postReload(w http.ResponseWriter, r *http.Request, t target) {
	if !a.running(w, r, t) {
		return
	}
	done, err := t.c.reload(r.Context(), t.backend, a.env)
	if a.refused(w, r, err) {
		return
	}
	if err != nil {
		a.problem(w, r, http.StatusBadGateway, err)
		return
	}
	a.log.Info("reload", "backend", t.Name)
	models, err := t.c.models(r.Context(), t.backend)
	if err != nil {
		a.problem(w, r, http.StatusBadGateway, err)
		return
	}
	code := http.StatusOK
	if !done {
		code = http.StatusAccepted
	}
	writeJSON(w, code, models)
}

func (a *api) getBackendSetup(w http.ResponseWriter, r *http.Request, t target) {
	status, err := a.setupSteps(r.Context(), &t)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *api) postBackendSetup(w http.ResponseWriter, r *http.Request, t target) {
	a.runSetup(w, r, &t)
}

// running answers 409 and returns false if the backend's container isn't
// running, since its models can only be asked for while it is.
func (a *api) running(w http.ResponseWriter, r *http.Request, t target) bool {
	container, err := a.env.compose.state(r.Context(), t.Service)
	if err != nil {
		a.problem(w, r, http.StatusInternalServerError, err)
		return false
	}
	if container != "running" {
		a.problemText(w, r, http.StatusConflict, "backend '"+t.Name+"' isn't running (its container is "+container+"); start it with POST /api/backends/"+t.Name+"/start")
		return false
	}
	return true
}

// refused answers 409 with the reason and returns true if err is an action
// the backend can't do.
func (a *api) refused(w http.ResponseWriter, r *http.Request, err error) bool {
	var no *unsupportedError
	if !errors.As(err, &no) {
		return false
	}
	a.problemText(w, r, http.StatusConflict, no.reason)
	return true
}

func (a *api) getModels(w http.ResponseWriter, r *http.Request, t target) {
	if t.c == nil {
		a.notConfigured(w, r, t)
		return
	}
	if !a.running(w, r, t) {
		return
	}
	models, err := t.c.models(r.Context(), t.backend)
	if a.refused(w, r, err) {
		return
	}
	if err != nil {
		a.problem(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, models)
}

// findModel returns the model with this ID from the backend's list, or
// answers 404 and returns false.
func (a *api) findModel(w http.ResponseWriter, r *http.Request, t target, id string) (model, bool) {
	models, err := t.c.models(r.Context(), t.backend)
	if err != nil {
		a.problem(w, r, http.StatusBadGateway, err)
		return model{}, false
	}
	var listed []string
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
		listed = append(listed, m.ID)
	}
	a.problemText(w, r, http.StatusNotFound, "backend '"+t.Name+"' has no model '"+id+"'; it has "+joinQuoted(listed))
	return model{}, false
}

// loaded reports whether a model's state, in its backend's own words, means
// it is ready to answer.
func loaded(state string) bool {
	return state == "loaded" || state == "ready"
}

func (a *api) postLoad(w http.ResponseWriter, r *http.Request, t target) {
	id := r.PathValue("model")
	l, err := t.c.loading(t.backend)
	if a.refused(w, r, err) || !a.running(w, r, t) {
		return
	}
	m, ok := a.findModel(w, r, t, id)
	if !ok {
		return
	}
	if loaded(m.State) {
		writeJSON(w, http.StatusOK, m)
		return
	}
	a.jobs.start("load:"+t.Name+":"+id, func(ctx context.Context, _ *progress) error {
		err := l.load(ctx, t.backend, id)
		a.logResult("load", err, "backend", t.Name, "model", id)
		return err
	})
	// The answer has the state from before the load began; GET .../models
	// shows it loading.
	writeJSON(w, http.StatusAccepted, m)
}

// postUnload unloads the model and answers once the backend no longer
// reports it loaded, since a backend may unload in the background after
// answering; the wait is bounded at 15 s.
func (a *api) postUnload(w http.ResponseWriter, r *http.Request, t target) {
	id := r.PathValue("model")
	l, err := t.c.loading(t.backend)
	if a.refused(w, r, err) || !a.running(w, r, t) {
		return
	}
	if _, ok := a.findModel(w, r, t, id); !ok {
		return
	}
	if err := l.unload(r.Context(), t.backend, id); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errNoModel) {
			code = http.StatusNotFound
		}
		a.problem(w, r, code, err)
		return
	}
	a.log.Info("unload", "backend", t.Name, "model", id)
	deadline := time.Now().Add(15 * time.Second)
	for {
		m, ok := a.findModel(w, r, t, id)
		if !ok {
			return
		}
		if !loaded(m.State) || time.Now().After(deadline) {
			writeJSON(w, http.StatusOK, m)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// problem answers with an RFC 9457 problem details body.
func (a *api) problem(w http.ResponseWriter, r *http.Request, code int, err error) {
	a.problemText(w, r, code, err.Error())
}

func (a *api) problemText(w http.ResponseWriter, r *http.Request, code int, detail string) {
	if code >= 500 {
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", code, "detail", detail)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(code), "status": code, "detail": detail})
}

func joinQuoted(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	quoted := slices.Clone(names)
	for i, n := range quoted {
		quoted[i] = "'" + n + "'"
	}
	out := quoted[0]
	for _, q := range quoted[1:] {
		out += ", " + q
	}
	return out
}

// logResult logs a background action's outcome: INFO if it worked, ERROR
// with the error if not.
func (a *api) logResult(action string, err error, attrs ...any) {
	if err != nil {
		a.log.Error(action+" failed", append(attrs, "error", err.Error())...)
		return
	}
	a.log.Info(action, attrs...)
}
