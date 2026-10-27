package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// llama.cpp's llama-server runs here twice, once in each of its modes, and
// each mode is a backend of its own: llamaRouter loads and unloads models on
// demand, from a presets file in backend-config/ that its section names,
// and llamaServer holds the one model its section gives it for as long as
// it runs.

func init() {
	register(backend{
		Name: "llamaRouter", Service: "llama-server-router", Profile: "llama-server", URL: "http://llama-server-router:8080",
		NeedsFiles: true, API: "openai", Health: "/health", kind: llamaRouterKind{},
	})
	register(backend{
		Name: "llamaServer", Service: "llama-server-single", Profile: "llama-server", URL: "http://llama-server-single:8080",
		NeedsFiles: true, API: "openai", Health: "/health", kind: llamaServerKind{},
	})
}

type llamaRouterKind struct{}

type llamaRouter struct {
	basic
	Config    string `yaml:"config"`    // the presets file in backend-config/
	ModelsMax int    `yaml:"modelsMax"` // models loaded at once
}

func (llamaRouterKind) configure(section *yaml.Node, _ fileIndex) (configured, error) {
	var r llamaRouter
	if err := decodeSection(section, &r); err != nil {
		return nil, err
	}
	if r.Config == "" || strings.Contains(r.Config, "/") {
		return nil, fmt.Errorf("needs config, the name of its presets file in backend-config/")
	}
	return &r, nil
}

func (r *llamaRouter) service() *service {
	return &service{Command: []string{"--models-preset", "/config/" + r.Config, "--models-max", strconv.Itoa(r.ModelsMax)}}
}

func (r *llamaRouter) models(ctx context.Context, b backend) ([]model, error) {
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := getJSON(ctx, b.URL+"/models", &list); err != nil {
		return nil, err
	}
	var models []model
	for _, m := range list.Data {
		models = append(models, model{ID: m.ID, State: m.Status.Value})
	}
	return models, nil
}

func (r *llamaRouter) loading(backend) (loader, error) { return r, nil }

// load answers once the router has begun loading; GET .../models shows the
// rest.
func (r *llamaRouter) load(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/models/load", map[string]string{"model": id})
}

// unload asks the router to stop the model; it stops it in the background
// and reports it loaded until it is gone (b11580), which the API waits for.
func (r *llamaRouter) unload(ctx context.Context, b backend, id string) error {
	err := postJSON(ctx, b.URL+"/models/unload", map[string]string{"model": id})
	var status *statusError
	if errors.As(err, &status) && strings.Contains(status.body, "model is not running") {
		return nil // already unloaded
	}
	return err
}

// reload makes the router re-read its presets before it answers,
// unloading running models whose entry changed or went (b11580 docs).
func (r *llamaRouter) reload(ctx context.Context, b backend, _ *env) (bool, error) {
	return true, getJSON(ctx, b.URL+"/models?reload=1", nil)
}

type llamaServerKind struct{}

type llamaServer struct {
	basic
	Model   modelConfig `yaml:"model"`
	CtxSize int         `yaml:"ctxSize"`
}

func (llamaServerKind) configure(section *yaml.Node, files fileIndex) (configured, error) {
	var s llamaServer
	if err := decodeSection(section, &s); err != nil {
		return nil, err
	}
	return &s, problems(checkModels("llamaServer", []modelConfig{s.Model}, files))
}

// service runs llama-server on the one model, with --jinja for tool calls
// and --reasoning-format deepseek, which puts thinking in
// reasoning_content.
func (s *llamaServer) service() *service {
	return &service{Command: []string{
		"--model", filepath.Join(backendModels, s.Model.File),
		"--alias", s.Model.ID,
		"--ctx-size", strconv.Itoa(s.CtxSize),
		"--jinja", "--reasoning-format", "deepseek",
	}}
}

func (s *llamaServer) modelIDs() []string { return []string{s.Model.ID} }

// models reports the one model: loading while the server answers every
// request with 503, as it does until the load ends, and sleeping when /props
// says so.
func (s *llamaServer) models(ctx context.Context, b backend) ([]model, error) {
	state := "loaded"
	var props struct {
		IsSleeping bool `json:"is_sleeping"`
	}
	var status *statusError
	switch err := getJSON(ctx, b.URL+"/props", &props); {
	case errors.As(err, &status) && status.code == http.StatusServiceUnavailable:
		state = "loading"
	case err != nil:
		return nil, err
	case props.IsSleeping:
		state = "sleeping"
	}
	return []model{{ID: s.Model.ID, State: state}}, nil
}

func (s *llamaServer) loading(b backend) (loader, error) {
	return nil, unsupported("backend '%s' holds its one model for as long as it runs; it has no load or unload", b.Name)
}

func (s *llamaServer) reload(_ context.Context, b backend, _ *env) (bool, error) {
	return false, unsupported("backend '%s' takes its settings as command-line flags and has no config to reload; stop and start it", b.Name)
}
