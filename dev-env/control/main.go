// Command control runs the development environment in dev-env/: it downloads
// the model files, starts and stops the backends with Docker Compose, and
// loads and unloads their models, all through one HTTP API. It is the one
// service `docker compose up` starts; dev-env/README.md describes the API.
//
// It runs in a container with the host's Docker socket, dev-env/ mounted at
// /work, and the models store at /models.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	listen   = ":8080"
	workDir  = "/work"   // dev-env/, mounted read-only
	store    = "/models" // the models store, read-write
	stateDir = "/state"  // control's own state, such as Open WebUI's API key
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A stop of control's container, such as by `docker compose down`, stops
	// the backends too, unless KEEP_BACKENDS is set: a recreate of control's
	// container sends the same SIGTERM, and the old container's setting is
	// the one that counts.
	keepBackends := false
	if v := os.Getenv("KEEP_BACKENDS"); v != "" {
		var err error
		if keepBackends, err = strconv.ParseBool(v); err != nil {
			log.Error("KEEP_BACKENDS must be true or false, or empty for false", "value", v)
			os.Exit(1)
		}
	}
	dc, err := newCompose(ctx, workDir)
	if err != nil {
		log.Error("finding where dev-env/ is mounted from", "error", err.Error())
		os.Exit(1)
	}
	readConfig := func() (*config, error) { return loadConfig(filepath.Join(workDir, "config.yaml")) }
	if _, err := readConfig(); err != nil {
		// Reported, not fatal: each request reads the config again, so a fix
		// applies without restarting control.
		log.Error("reading the config", "error", err.Error())
	}
	dc.override = func() ([]byte, error) {
		c, err := readConfig()
		if err != nil {
			return nil, err
		}
		return c.override()
	}
	jobs := newJobs(ctx)
	a := &api{
		config: readConfig,
		files:  &files{config: readConfig, store: store, jobs: jobs, manifest: &manifest{store: store}},
		jobs:   jobs,
		env:    &env{store: store, compose: dc, log: log},
		log:    log,
	}
	srv := &http.Server{Addr: listen, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		if !keepBackends {
			// Within compose.yaml's stop_grace_period for control, after
			// which Docker kills it.
			stopping, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			a.logResult("stop on exit", a.stopAll(stopping))
			cancel()
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", listen, "project", dc.hostDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serving", "error", err.Error())
		os.Exit(1)
	}
}
