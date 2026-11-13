package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LocalAI, from an image built from dev-env/localai, which adds the llama.cpp
// engine of the same release to LocalAI's own image. Its start script writes
// a model config for each model and alias its section lists, each model
// reading its file in place from the models store.

func init() {
	register(backend{
		Name: "localAi", Service: "localai", Profile: "localai", URL: "http://localai:8080",
		NeedsFiles: true, API: "openai", Health: "/readyz", Build: "localai", Image: "ai-model-facade-dev-localai",
		kind: localAIKind{},
	})
}

type localAIKind struct{}

type localAI struct {
	basic
	MaxActiveBackends int            `yaml:"maxActiveBackends"`
	CtxSize           int            `yaml:"ctxSize"`
	Models            []modelConfig  `yaml:"models"`
	Aliases           []localAIAlias `yaml:"aliases"`
}

// localAIAlias is a LocalAI alias: another name for a model, with no
// settings of its own.
type localAIAlias struct {
	ID    string `yaml:"id"`
	Model string `yaml:"model"`
}

func (localAIKind) configure(section *yaml.Node, files fileIndex) (configured, error) {
	var l localAI
	if err := decodeSection(section, &l); err != nil {
		return nil, err
	}
	found := checkModels("localAi", l.Models, files)
	models := map[string]bool{}
	for _, m := range l.Models {
		models[m.ID] = true
	}
	for _, a := range l.Aliases {
		switch {
		case a.ID == "":
			found = append(found, "an alias of 'localAi' has no id")
		case models[a.ID]:
			found = append(found, fmt.Sprintf("'localAi' has '%s' as both a model and an alias", a.ID))
		case !models[a.Model]:
			found = append(found, fmt.Sprintf("alias '%s' of 'localAi' names model '%s', which its models don't list", a.ID, a.Model))
		}
	}
	return &l, problems(found)
}

// service hands the start script the models, as space-separated
// <id>=<file> pairs with each file under the models store, and the aliases,
// as <alias>=<model> pairs; LocalAI reads its limit on loaded models and
// its default context size from its own environment.
func (l *localAI) service() *service {
	var models, aliases []string
	for _, m := range l.Models {
		models = append(models, m.ID+"="+m.File)
	}
	for _, a := range l.Aliases {
		aliases = append(aliases, a.ID+"="+a.Model)
	}
	env := map[string]string{
		"LOCALAI_DEV_MODELS":          strings.Join(models, " "),
		"LOCALAI_DEV_ALIASES":         strings.Join(aliases, " "),
		"LOCALAI_MAX_ACTIVE_BACKENDS": strconv.Itoa(l.MaxActiveBackends),
	}
	if l.CtxSize > 0 {
		env["LOCALAI_CONTEXT_SIZE"] = strconv.Itoa(l.CtxSize)
	}
	return &service{Environment: env}
}

func (l *localAI) modelIDs() []string {
	ids := ids(l.Models)
	for _, a := range l.Aliases {
		ids = append(ids, a.ID)
	}
	return ids
}

// models lists LocalAI's models and aliases: a model /system lists is
// loaded, and an alias has its model's state. LocalAI reports no loading
// state.
func (l *localAI) models(ctx context.Context, b backend) ([]model, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, b.URL+"/v1/models", &list); err != nil {
		return nil, err
	}
	var aliases []struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := getJSON(ctx, b.URL+"/api/aliases", &aliases); err != nil {
		return nil, err
	}
	var system struct {
		LoadedModels []struct {
			ID string `json:"id"`
		} `json:"loaded_models"`
	}
	if err := getJSON(ctx, b.URL+"/system", &system); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	for _, a := range aliases {
		targets[a.Name] = a.Target
	}
	loaded := map[string]bool{}
	for _, m := range system.LoadedModels {
		loaded[m.ID] = true
	}
	var models []model
	for _, m := range list.Data {
		base := m.ID
		if target, isAlias := targets[m.ID]; isAlias {
			base = target
		}
		state := "unloaded"
		if loaded[base] {
			state = "loaded"
		}
		entry := model{ID: m.ID, State: state}
		if base != m.ID {
			entry.AliasOf = base
		}
		models = append(models, entry)
	}
	return models, nil
}

func (l *localAI) loading(backend) (loader, error) { return l, nil }

// load sends a one-token request, which LocalAI answers once the model has
// loaded; an alias loads its model.
func (l *localAI) load(ctx context.Context, b backend, id string) error {
	return postJSON(ctx, b.URL+"/v1/chat/completions", map[string]any{
		"model":      id,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
}

// unload stops the model's backend process, answering once it has stopped
// (verified on v4.11.0); an alias unloads its model. LocalAI answers 404 for
// a model that isn't loaded, and the API has checked that LocalAI lists the
// model, so a 404 means it is unloaded already.
func (l *localAI) unload(ctx context.Context, b backend, id string) error {
	for _, a := range l.Aliases {
		if a.ID == id {
			id = a.Model
		}
	}
	err := postJSON(ctx, b.URL+"/backend/shutdown", map[string]string{"model": id})
	if errors.Is(err, errNoModel) {
		return nil
	}
	return err
}
