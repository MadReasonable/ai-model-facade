package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// LM Studio's headless daemon, llmster, from an image built from
// dev-env/lm-studio, since LM Studio publishes no image. It names a model
// after the folder its file is in, lowercased, so its start script makes a
// folder for each model its section lists, linked to the model's file. It
// holds one model loaded by a request at a time: loading another unloads it.

func init() {
	register(backend{
		Name: "lmStudio", Service: "lm-studio", Profile: "lm-studio", URL: "http://lm-studio:1234",
		NeedsFiles: true, API: "openai", Health: "/api/v1/models", Build: "lm-studio", Image: "ai-model-facade-dev-lm-studio",
		kind: lmStudioKind{},
	})
}

type lmStudioKind struct{}

type lmStudio struct {
	basic
	Models []modelConfig `yaml:"models"`
}

func (lmStudioKind) configure(section *yaml.Node, files fileIndex) (configured, error) {
	var l lmStudio
	if err := decodeSection(section, &l); err != nil {
		return nil, err
	}
	found := checkModels("lmStudio", l.Models, files)
	for _, m := range l.Models {
		if m.ID != strings.ToLower(m.ID) {
			found = append(found, fmt.Sprintf("model '%s' must be lowercase: LM Studio names a model after its folder, lowercased, so it would list '%s'", m.ID, strings.ToLower(m.ID)))
		}
	}
	return &l, problems(found)
}

// service hands the start script the models, as space-separated
// <id>=<file> pairs.
func (l *lmStudio) service() *service {
	var pairs []string
	for _, m := range l.Models {
		pairs = append(pairs, m.ID+"="+filepath.Join(backendModels, m.File))
	}
	return &service{Environment: map[string]string{"LM_STUDIO_MODELS": strings.Join(pairs, " ")}}
}

func (l *lmStudio) modelIDs() []string { return ids(l.Models) }

// lmStudioModel is one model in LM Studio's own list, GET /api/v1/models.
type lmStudioModel struct {
	Key             string `json:"key"`
	LoadedInstances []struct {
		ID string `json:"id"`
	} `json:"loaded_instances"`
}

// lmStudioList reads LM Studio's model list. The list answers 500, "Model
// does not exist.", for as long as a load is under way, and no endpoint
// reports a loading state (llmster 0.0.25-1; verified), so the error says
// so.
func lmStudioList(ctx context.Context, b backend) ([]lmStudioModel, error) {
	var list struct {
		Models []lmStudioModel `json:"models"`
	}
	err := getJSON(ctx, b.URL+"/api/v1/models", &list)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusInternalServerError {
		return nil, fmt.Errorf("LM Studio's model list fails while it loads a model, so ask again once the load ends: %w", err)
	}
	if err != nil {
		return nil, err
	}
	return list.Models, nil
}

// models lists LM Studio's models: one with a loaded instance is loaded;
// LM Studio reports no loading state.
func (l *lmStudio) models(ctx context.Context, b backend) ([]model, error) {
	listed, err := lmStudioList(ctx, b)
	if err != nil {
		return nil, err
	}
	var models []model
	for _, m := range listed {
		state := "unloaded"
		if len(m.LoadedInstances) > 0 {
			state = "loaded"
		}
		models = append(models, model{ID: m.Key, State: state})
	}
	return models, nil
}

// ready waits, once LM Studio's list answers, for it to unpack its engines,
// which it does on every start of a new container, for about 25 s: a load
// asked for meanwhile waits, and every other request answers 500 (llmster
// 0.0.25-1; its log says "Finished unbundling engines" when it is done). A
// container seen ready is remembered by its ID, so its log is read only
// until then.
func (l *lmStudio) ready(ctx context.Context, b backend, e *env) string {
	if waiting := l.basic.ready(ctx, b, e); waiting != "" {
		return waiting
	}
	id, err := e.compose.containerID(ctx, b.Service)
	if err != nil || id == "" {
		return "waiting for its container"
	}
	if _, ok := lmStudioUnpacked.Load(id); ok {
		return ""
	}
	logs, err := e.compose.logs(ctx, b.Profile, b.Service)
	if err != nil || !bytes.Contains(logs, []byte("Finished unbundling engines")) {
		return "waiting for it to unpack its engines"
	}
	lmStudioUnpacked.Store(id, true)
	return ""
}

// lmStudioUnpacked holds the IDs of LM Studio containers whose engines are
// unpacked.
var lmStudioUnpacked sync.Map

func (l *lmStudio) loading(backend) (loader, error) { return l, nil }

// load sends a one-token request, not /api/v1/models/load: LM Studio
// unloads to make room only models loaded by a request, and treats a load
// through that endpoint as one by hand, which stays (LM Studio docs, Idle
// TTL and Auto-Evict).
func (l *lmStudio) load(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/v1/chat/completions", map[string]any{
		"model":      id,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
}

// unload unloads every loaded instance of the model, since LM Studio
// unloads by instance.
func (l *lmStudio) unload(ctx context.Context, b backend, id string) error {
	listed, err := lmStudioList(ctx, b)
	if err != nil {
		return err
	}
	for _, m := range listed {
		if m.Key != id {
			continue
		}
		for _, instance := range m.LoadedInstances {
			if err := postJSON(ctx, b.URL+"/api/v1/models/unload", map[string]string{"instance_id": instance.ID}); err != nil {
				return err
			}
		}
	}
	return nil
}
