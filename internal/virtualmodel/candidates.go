package virtualmodel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
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
	Backend    string    `yaml:"backend"` // default: the virtual model's backend
	LoadIfNone bool      `yaml:"loadIfNone"`
	Params     yaml.Node `yaml:"params"`
}

// candidates is the default type: route params, then the first eligible
// candidate's params, then the model rewritten to the candidate's concrete ID.
// Candidates may be on different backends.
type candidates struct {
	backendName  string          // the virtual model's own backend; "" if every candidate names one
	backend      backend.Backend // nil likewise
	backendNames []string        // per candidate
	backends     []backend.Backend
	catalogs     []*backend.Catalog // per candidate: its backend's model list at startup
	description  string
	params       []mergepatch.Patch
	selector     *selector.FirstLoaded
	patches      [][]mergepatch.Patch // per candidate
	passthrough  []string             // backends with passthroughAuth, when more than one
}

func newCandidates(entry config.Entry, env Env) (*candidates, error) {
	var c candidatesConfig
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	m := &candidates{backendName: c.Backend, description: c.Description, selector: &selector.FirstLoaded{}}
	if c.Backend != "" {
		if m.backend = env.Backends[c.Backend]; m.backend == nil {
			return nil, fmt.Errorf("line %d: backend '%s' is not defined", entry.Node.Line, c.Backend)
		}
	}
	var err error
	if m.params, err = mergepatch.Compile(&c.Params); err != nil {
		return nil, err
	}
	if len(c.Candidates) == 0 {
		return nil, fmt.Errorf("line %d: candidates is required", entry.Node.Line)
	}
	checked := map[string]bool{} // backends whose capabilities were checked
	for i := range c.Candidates {
		node := &c.Candidates[i]
		var cc candidateConfig
		if err := config.DecodeStrict(node, &cc); err != nil {
			return nil, err
		}
		name := cc.Backend
		if name == "" {
			name = c.Backend
		}
		if name == "" {
			return nil, fmt.Errorf("line %d: backend is required, on the model or on the candidate", node.Line)
		}
		b := env.Backends[name]
		if b == nil {
			return nil, fmt.Errorf("line %d: backend '%s' is not defined", node.Line, name)
		}
		if !checked[name] {
			checked[name] = true
			for _, capability := range m.selector.Requires() {
				if !backend.Has(b, capability) {
					return nil, fmt.Errorf("line %d: backend '%s' lacks capability %s needed to pick a loaded candidate", node.Line, name, capability)
				}
			}
		}
		candidate := selector.Candidate{Model: cc.Model, LoadIfNone: cc.LoadIfNone}
		if name != c.Backend {
			candidate.Backend, candidate.BackendName = b, name
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", node.Line, err)
		}
		patches, err := mergepatch.Compile(&cc.Params)
		if err != nil {
			return nil, err
		}
		hasParams := len(m.params)+len(patches) > 0
		env.Check(backend.ListingCheck{Backends: []string{name}, Run: func(catalogs map[string]*backend.Catalog, warn func(error)) {
			catalog := catalogs[name]
			if !selector.IsGlob(candidate.Model) && !catalog.Contains(candidate.Model) {
				// Not fatal: the backend may list it later, e.g. Open WebUI once
				// it has fetched this gateway's own virtual models. Until then it
				// never shows as loaded.
				warn(fmt.Errorf("models.%s: line %d: model '%s' is not listed by backend '%s', so it never counts as loaded until it is%s", entry.Name, node.Line, candidate.Model, name, didYouMean(catalog.WithTags(candidate.Model))))
			}
			noteAliases(env.Log, entry.Name, candidate.Model, catalog, hasParams)
		}})
		m.selector.Candidates = append(m.selector.Candidates, candidate)
		m.backendNames = append(m.backendNames, name)
		m.backends = append(m.backends, b)
		m.catalogs = append(m.catalogs, env.Catalogs[name])
		m.patches = append(m.patches, patches)
	}
	m.notePassthrough(env.Log, entry.Name)
	return m, nil
}

// notePassthrough warns when candidates span several backends that forward
// the client's own key: a client's key is valid on one of them at most, so
// requests fail or succeed depending on which is loaded, and the key reaches
// services the client didn't mean it for.
func (m *candidates) notePassthrough(log *slog.Logger, model string) {
	seen := map[string]bool{}
	for i, name := range m.backendNames {
		p, ok := m.backends[i].(interface{ PassthroughAuth() bool })
		if !seen[name] && ok && p.PassthroughAuth() {
			m.passthrough = append(m.passthrough, name)
		}
		seen[name] = true
	}
	if len(m.passthrough) < 2 {
		m.passthrough = nil
		return
	}
	log.Warn("candidates span several passthroughAuth backends; a client's key is sent to whichever one wins", "model", model, "backends", m.passthrough)
}

// noteAliases logs when a candidate can resolve to a backend alias while
// params are in play: the backend applies the alias's own params after the
// gateway's (llama-swap's setParamsByID replaces top-level keys), so they may
// override what the config asked for.
func noteAliases(log *slog.Logger, model, candidate string, catalog *backend.Catalog, hasParams bool) {
	if !hasParams {
		return
	}
	const msg = "candidate can resolve to a backend alias; the backend applies the alias's own params after the gateway's and may override them"
	if base, ok := catalog.AliasOf[candidate]; ok {
		log.Info(msg, "model", model, "candidate", candidate, "aliasOf", base)
		return
	}
	if selector.IsGlob(candidate) {
		if _, aliases := selector.Matches(candidate, catalog.Models); len(aliases) > 0 {
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
	cur := Current{Model: sel.Target, AliasOf: sel.AliasOf, Reason: sel.Reason}
	if sel.Index >= 0 {
		cur.Backend = m.selector.Candidates[sel.Index].BackendName
	}
	return cur, nil
}

func (m *candidates) Chat(ctx context.Context, endpoint backend.Endpoint, body []byte, client backend.Client) (Result, error) {
	sel, err := m.selector.Select(ctx, body, m.backend)
	res := Result{Target: sel.Target, AliasOf: sel.AliasOf, Checked: sel.Checked, Reason: sel.Reason}
	if err != nil {
		return res, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: m.stateError(err)}
	}
	if sel.Index < 0 {
		return res, &Error{Status: http.StatusServiceUnavailable, Type: "no_candidate_loaded", Message: "no candidate model is loaded"}
	}
	res.Backend = m.selector.Candidates[sel.Index].BackendName
	for _, p := range append(append([]mergepatch.Patch(nil), m.params...), m.patches[sel.Index]...) {
		if body, err = p.Apply(body); err != nil {
			return res, &Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
	}
	if body, err = sjson.SetBytes(body, "model", sel.Target); err != nil {
		return res, &Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
	}
	req := backend.ChatRequest{Endpoint: endpoint, Body: body, Stream: gjson.GetBytes(body, "stream").Bool(), Client: client}
	if res.Chat, err = m.backends[sel.Index].Chat(ctx, req); err != nil {
		return res, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': %v", m.backendNames[sel.Index], err)}
	}
	return res, nil
}

// stateError reports that no candidate's backend state could be read.
func (m *candidates) stateError(err error) string {
	var names []string
	for _, name := range m.backendNames {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 1 {
		return fmt.Sprintf("backend '%s' state unavailable: %v", names[0], err)
	}
	return fmt.Sprintf("backends %s state unavailable: %v", strings.Join(names, ", "), err)
}

func (m *candidates) Describe(w io.Writer) {
	if m.backendName != "" {
		fmt.Fprintf(w, "  backend: %s\n", m.backendName)
	}
	if m.description != "" {
		fmt.Fprintf(w, "  description: %s\n", m.description)
	}
	if m.passthrough != nil {
		fmt.Fprintf(w, "  note: candidates span several passthroughAuth backends (%s); a client's key is sent to whichever one wins\n", strings.Join(m.passthrough, ", "))
	}
	for i, c := range m.selector.Candidates {
		note := ""
		if c.LoadIfNone {
			note = " (loadIfNone)"
		}
		fmt.Fprintf(w, "  %d. %s%s\n", i+1, c.On(c.Model), note)
		patches := append(append([]mergepatch.Patch(nil), m.params...), m.patches[i]...)
		for _, line := range describeAliases(c.Model, m.backendNames[i], m.catalogs[i], len(patches) > 0) {
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
// override note only applies when the candidate has params. A pattern on a
// backend that couldn't be listed says so, since "none" would read as what
// the backend answered.
func describeAliases(candidate, backendName string, catalog *backend.Catalog, hasParams bool) []string {
	if catalog.Unavailable {
		if selector.IsGlob(candidate) {
			return []string{"base models and aliases: unknown; backend '" + backendName + "' could not be listed"}
		}
		return nil
	}
	if selector.IsGlob(candidate) {
		bases, aliases := selector.Matches(candidate, catalog.Models)
		lines := []string{"base models: " + listOrNone(bases), "aliases: " + listOrNone(aliases)}
		if len(aliases) > 0 && hasParams {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	if base, ok := catalog.AliasOf[candidate]; ok {
		lines := []string{"alias of " + base}
		if hasParams {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	return nil
}

// didYouMean suggests listed IDs for a candidate written without its tag,
// e.g. Ollama's "llama3" for "llama3:latest".
func didYouMean(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return "; did you mean " + ids[0] + "?"
	}
	return "; did you mean one of " + strings.Join(ids, ", ") + "?"
}

func listOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
