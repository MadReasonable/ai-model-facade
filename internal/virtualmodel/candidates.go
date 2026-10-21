package virtualmodel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/mergepatch"
	"github.com/MadReasonable/ai-model-facade/internal/selector"
)

type candidatesConfig struct {
	Type        string      `yaml:"type"`
	Backend     string      `yaml:"backend"`
	Description string      `yaml:"description"`
	Params      yaml.Node   `yaml:"params"`
	Candidates  []yaml.Node `yaml:"candidates"`
}

type candidateConfig struct {
	Model      string    `yaml:"model"`
	LoadIfNone bool      `yaml:"loadIfNone"`
	Params     yaml.Node `yaml:"params"`
}

// candidates is the default type: route params, then the first eligible
// candidate's params, then the model rewritten to the candidate's concrete ID.
type candidates struct {
	backendName string
	backend     backend.Backend
	catalog     *backend.Catalog // the backend's model list at startup
	description string
	params      []mergepatch.Patch
	selector    *selector.FirstLoaded
	patches     [][]mergepatch.Patch // per candidate
}

func newCandidates(ctx context.Context, entry config.Entry, env Env) (*candidates, error) {
	var c candidatesConfig
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	m := &candidates{backendName: c.Backend, description: c.Description, selector: &selector.FirstLoaded{}}
	if c.Backend == "" {
		return nil, fmt.Errorf("line %d: backend is required", entry.Node.Line)
	}
	m.backend = env.Backends[c.Backend]
	if m.backend == nil {
		return nil, fmt.Errorf("line %d: backend %q is not defined", entry.Node.Line, c.Backend)
	}
	for _, capability := range m.selector.Requires() {
		if !backend.Has(m.backend, capability) {
			return nil, fmt.Errorf("line %d: backend %q lacks capability %s needed to pick a loaded candidate", entry.Node.Line, c.Backend, capability)
		}
	}
	var err error
	if m.params, err = mergepatch.Compile(&c.Params); err != nil {
		return nil, err
	}
	if len(c.Candidates) == 0 {
		return nil, fmt.Errorf("line %d: candidates is required", entry.Node.Line)
	}
	if m.catalog, err = env.Catalog(ctx, c.Backend); err != nil {
		return nil, fmt.Errorf("backend %q: listing models: %w", c.Backend, err)
	}
	for i := range c.Candidates {
		node := &c.Candidates[i]
		var cc candidateConfig
		if err := config.DecodeStrict(node, &cc); err != nil {
			return nil, err
		}
		candidate := selector.Candidate{Model: cc.Model, LoadIfNone: cc.LoadIfNone}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", node.Line, err)
		}
		if !selector.IsGlob(candidate.Model) && !m.catalog.Contains(candidate.Model) {
			return nil, fmt.Errorf("line %d: model %q is not listed by backend %q", node.Line, candidate.Model, c.Backend)
		}
		patches, err := mergepatch.Compile(&cc.Params)
		if err != nil {
			return nil, err
		}
		m.noteAliases(env.Log, entry.Name, candidate.Model, len(m.params)+len(patches) > 0)
		m.selector.Candidates = append(m.selector.Candidates, candidate)
		m.patches = append(m.patches, patches)
	}
	return m, nil
}

// noteAliases logs when a candidate can resolve to a backend alias while
// params are in play: the backend applies the alias's own params after the
// gateway's (llama-swap's setParamsByID replaces top-level keys), so they may
// override what the config asked for.
func (m *candidates) noteAliases(log *slog.Logger, model, candidate string, hasParams bool) {
	if !hasParams {
		return
	}
	const msg = "candidate can resolve to a backend alias; the backend applies the alias's own params after the gateway's and may override them"
	if base, ok := m.catalog.AliasOf[candidate]; ok {
		log.Info(msg, "model", model, "candidate", candidate, "aliasOf", base)
		return
	}
	if selector.IsGlob(candidate) {
		if _, aliases := selector.Matches(candidate, m.catalog.Models); len(aliases) > 0 {
			log.Info(msg, "model", model, "candidate", candidate, "aliases", aliases)
		}
	}
}

func (m *candidates) Description() string {
	return m.description
}

func (m *candidates) Current(ctx context.Context) (Current, error) {
	sel, err := m.selector.Select(ctx, nil, m.backend)
	if err != nil {
		return Current{}, err
	}
	return Current{Model: sel.Target, AliasOf: sel.AliasOf, Reason: sel.Reason}, nil
}

func (m *candidates) Chat(ctx context.Context, endpoint backend.Endpoint, body []byte, client backend.Client) (Result, error) {
	sel, err := m.selector.Select(ctx, body, m.backend)
	res := Result{Target: sel.Target, AliasOf: sel.AliasOf, Checked: sel.Checked, Reason: sel.Reason}
	if err != nil {
		return res, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend %q state unavailable: %v", m.backendName, err)}
	}
	if sel.Index < 0 {
		return res, &Error{Status: http.StatusServiceUnavailable, Type: "no_candidate_loaded", Message: "no candidate model is loaded"}
	}
	for _, p := range append(append([]mergepatch.Patch(nil), m.params...), m.patches[sel.Index]...) {
		if body, err = p.Apply(body); err != nil {
			return res, &Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
	}
	if body, err = sjson.SetBytes(body, "model", sel.Target); err != nil {
		return res, &Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
	}
	req := backend.ChatRequest{Endpoint: endpoint, Body: body, Stream: gjson.GetBytes(body, "stream").Bool(), Client: client}
	if res.Chat, err = m.backend.Chat(ctx, req); err != nil {
		return res, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend %q: %v", m.backendName, err)}
	}
	return res, nil
}

func (m *candidates) Describe(w io.Writer) {
	fmt.Fprintf(w, "  backend: %s\n", m.backendName)
	if m.description != "" {
		fmt.Fprintf(w, "  description: %s\n", m.description)
	}
	for i, c := range m.selector.Candidates {
		note := ""
		if c.LoadIfNone {
			note = " (loadIfNone)"
		}
		fmt.Fprintf(w, "  %d. %s%s\n", i+1, c.Model, note)
		patches := append(append([]mergepatch.Patch(nil), m.params...), m.patches[i]...)
		for _, line := range m.describeAliases(c.Model, len(patches) > 0) {
			fmt.Fprintf(w, "     %s\n", line)
		}
		if composed, ok := mergepatch.Compose(patches); ok {
			fmt.Fprintf(w, "     params: %s\n", composed)
			continue
		}
		fmt.Fprintln(w, "     params (not expressible as one patch; applied in order):")
		for _, p := range patches {
			fmt.Fprintf(w, "       %s\n", p)
		}
	}
}

const aliasOverrideNote = "note: the backend's own params for an alias may override the params below"

// describeAliases lists the aliases a candidate can route to, using the model
// list fetched at startup: what a pattern matches, or an alias's base model.
// An exact base model only ever routes to itself, so it gets no lines. The
// override note only applies when the candidate has params.
func (m *candidates) describeAliases(candidate string, hasParams bool) []string {
	if selector.IsGlob(candidate) {
		bases, aliases := selector.Matches(candidate, m.catalog.Models)
		lines := []string{"base models: " + listOrNone(bases), "aliases: " + listOrNone(aliases)}
		if len(aliases) > 0 && hasParams {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	if base, ok := m.catalog.AliasOf[candidate]; ok {
		lines := []string{"alias of " + base}
		if hasParams {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	return nil
}

func listOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
