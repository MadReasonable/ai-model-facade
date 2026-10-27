package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// llama-swap starts a llama-server per model on demand. Its models are in
// its own config file in backend-config/, which its section of config.yaml
// names.

func init() {
	register(backend{
		Name: "llamaSwap", Service: "llama-swap", Profile: "llama-swap", URL: "http://llama-swap:8080",
		NeedsFiles: true, Health: "/health", kind: llamaSwapKind{},
	})
}

type llamaSwapKind struct{}

type llamaSwap struct {
	basic
	Config string `yaml:"config"` // llama-swap's config file in backend-config/
}

func (llamaSwapKind) configure(section *yaml.Node, _ fileIndex) (configured, error) {
	var s llamaSwap
	if err := decodeSection(section, &s); err != nil {
		return nil, err
	}
	if s.Config == "" || strings.Contains(s.Config, "/") {
		return nil, fmt.Errorf("needs config, the name of llama-swap's config file in backend-config/")
	}
	return &s, nil
}

// service runs llama-swap with the image's own entrypoint, reading its
// config from the mounted folder in place of /app/config.yaml.
func (s *llamaSwap) service() *service {
	return &service{Entrypoint: []string{"/app/llama-swap", "-config", "/config/" + s.Config, "-watch-config"}}
}

// models lists llama-swap's models and presets with the state /running
// reports for each base model; a preset has its base's.
func (s *llamaSwap) models(ctx context.Context, b backend) ([]model, error) {
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				LlamaSwap struct {
					Type    string `json:"type"`
					ModelID string `json:"modelID"`
				} `json:"llamaswap"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := getJSON(ctx, b.URL+"/v1/models", &list); err != nil {
		return nil, err
	}
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := getJSON(ctx, b.URL+"/running", &running); err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, r := range running.Running {
		states[r.Model] = r.State
	}
	var models []model
	for _, m := range list.Data {
		base := m.ID
		if m.Meta.LlamaSwap.Type == "alias" {
			base = m.Meta.LlamaSwap.ModelID
		}
		state := states[base]
		if state == "" {
			state = "unloaded"
		}
		entry := model{ID: m.ID, State: state}
		if base != m.ID {
			entry.AliasOf = base
		}
		models = append(models, entry)
	}
	return models, nil
}

func (s *llamaSwap) loading(backend) (loader, error) { return s, nil }

// load asks for the model's own health check through /upstream/<model>/,
// which passes a request to that model's llama-server, starting it if it
// isn't running, and answers once it is ready (verified on v263).
func (s *llamaSwap) load(ctx context.Context, b backend, id string) error {
	return getJSON(ctx, b.URL+"/upstream/"+url.PathEscape(id)+"/health", nil)
}

// unload answers once the model has stopped (v263 source, server.go).
func (s *llamaSwap) unload(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/api/models/unload/"+url.PathEscape(id), nil)
}

// reload sends SIGHUP, on which llama-swap reloads its config in the
// background (v263 source, llama-swap.go).
func (s *llamaSwap) reload(ctx context.Context, b backend, e *env) (bool, error) {
	return false, e.compose.signal(ctx, b.Profile, b.Service, "SIGHUP")
}
