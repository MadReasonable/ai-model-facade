package virtualmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/mergepatch"
	"github.com/MadReasonable/ai-model-facade/internal/pipeline"
	"github.com/MadReasonable/ai-model-facade/internal/selector"
	"github.com/MadReasonable/ai-model-facade/internal/translate"
)

type candidatesConfig struct {
	Type        string      `yaml:"type"`
	Backend     string      `yaml:"backend"`
	Description string      `yaml:"description"`
	Protocols   []string    `yaml:"protocols"` // default: every protocol
	Params      yaml.Node   `yaml:"params"`
	Candidates  []yaml.Node `yaml:"candidates"`
}

type candidateConfig struct {
	Model      string    `yaml:"model"`
	Backend    string    `yaml:"backend"` // default: the virtual model's backend
	LoadIfNone bool      `yaml:"loadIfNone"`
	Params     yaml.Node `yaml:"params"`
	// UseProtocol is the protocol the backend receives for this candidate, one
	// it accepts; default: the client's, else the first it can be translated
	// to.
	UseProtocol string `yaml:"useProtocol"`
}

// candidates is the default type: route params, then the first eligible
// candidate's params, then the model rewritten to the candidate's concrete ID.
// Candidates may be on different backends.
type candidates struct {
	name         string
	log          *slog.Logger
	backendName  string          // the virtual model's own backend; "" if every candidate names one
	backend      backend.Backend // nil likewise
	backendNames []string        // per candidate
	backends     []backend.Backend
	// accepted holds, per candidate, the protocols its backend may be sent:
	// all it accepts, or the one useProtocol names.
	accepted     [][]backend.Protocol
	useProtocols []string           // per candidate: its useProtocol, or ""
	catalogs     []*backend.Catalog // per candidate: its backend's model list at startup
	description  string
	protocols    []backend.Protocol // the protocols the facade serves; nil means all
	params       protocolParams
	selector     *selector.FirstLoaded
	patches      []protocolParams // per candidate
	passthrough  []string         // backends with passthroughAuth, when more than one
}

func newCandidates(entry config.Entry, env Env) (*candidates, error) {
	var c candidatesConfig
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	m := &candidates{name: entry.Name, log: env.Log, backendName: c.Backend, description: c.Description, selector: &selector.FirstLoaded{}}
	if c.Backend != "" {
		if m.backend = env.Backends[c.Backend]; m.backend == nil {
			return nil, fmt.Errorf("line %d: backend '%s' is not defined", entry.Node.Line, c.Backend)
		}
	}
	var err error
	if m.protocols, err = parseProtocols(c.Protocols, entry.Node.Line); err != nil {
		return nil, err
	}
	if m.params, err = compileParams(&c.Params); err != nil {
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
		accepted, err := protocolsFor(cc.UseProtocol, b, name, node.Line)
		if err != nil {
			return nil, err
		}
		for _, p := range m.protocols {
			if !reaches(p, accepted) {
				return nil, fmt.Errorf("line %d: facade '%s' lists protocol %s, which backend '%s' doesn't accept; it accepts %s", node.Line, entry.Name, p, name, joinProtocols(accepted))
			}
		}
		patches, err := compileParams(&cc.Params)
		if err != nil {
			return nil, err
		}
		hasParams := !m.params.empty() || !patches.empty()
		if slices.Contains(accepted, backend.AnthropicMessages) {
			m.warnChatOnlyKeys(entry.Name, cc.Model, name, env.Log, patches)
		}
		env.Check(backend.ListingCheck{Backends: []string{name}, Run: func(catalogs map[string]*backend.Catalog, warn func(error)) {
			catalog := catalogs[name]
			if !selector.IsGlob(candidate.Model) && !catalog.Contains(candidate.Model) {
				// Not fatal: the backend may list it later, e.g. Open WebUI once
				// it has fetched this gateway's own virtual models. Until then it
				// never shows as loaded.
				warn(fmt.Errorf("models.%s: line %d: model '%s' is not listed by backend '%s' and won't count as loaded until it is%s", entry.Name, node.Line, candidate.Model, name, didYouMean(catalog.WithTags(candidate.Model))))
			}
			noteAliases(env.Log, entry.Name, candidate.Model, catalog, hasParams)
		}})
		m.selector.Candidates = append(m.selector.Candidates, candidate)
		m.backendNames = append(m.backendNames, name)
		m.backends = append(m.backends, b)
		m.accepted = append(m.accepted, accepted)
		m.useProtocols = append(m.useProtocols, cc.UseProtocol)
		m.catalogs = append(m.catalogs, env.Catalogs[name])
		m.patches = append(m.patches, patches)
	}
	m.notePassthrough(env.Log, entry.Name)
	return m, nil
}

// protocolsFor returns the protocols a candidate's backend may be sent: the one
// useProtocol names, which the backend must accept, or else all it accepts.
func protocolsFor(name string, b backend.Backend, backendName string, line int) ([]backend.Protocol, error) {
	if name == "" {
		return b.Protocols(), nil
	}
	protocol := backend.Protocol(name)
	if !slices.Contains(b.Protocols(), protocol) {
		return nil, fmt.Errorf("line %d: useProtocol '%s' is not a protocol backend '%s' accepts; it accepts %s", line, name, backendName, joinProtocols(b.Protocols()))
	}
	return []backend.Protocol{protocol}, nil
}

// chatOnlyKeys exist only in Chat Completions. The Messages API refuses a field
// it doesn't know, and the anthropic adapter used to translate these.
var chatOnlyKeys = []string{"reasoning_effort", "response_format", "stop"}

// warnChatOnlyKeys warns about a Chat Completions key that params send to a
// backend receiving Messages, which would refuse it. It is a narrow exception
// to nothing checking that a key suits the backend, for configs written when
// the anthropic adapter translated params.
func (m *candidates) warnChatOnlyKeys(facade, model, backendName string, log *slog.Logger, candidate protocolParams) {
	for _, patch := range append(m.params.appendFor(nil, backend.AnthropicMessages), candidate.appendFor(nil, backend.AnthropicMessages)...) {
		for _, key := range chatOnlyKeys {
			if patch.Has(key) {
				log.Warn("params set a Chat Completions key on a candidate that receives Messages, which refuses it; use its Messages spelling", "model", facade, "candidate", model, "backend", backendName, "key", key)
			}
		}
	}
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

// noteAliases logs when a candidate can resolve to a backend alias, or to a
// preset listed without its base, while params are in play: the backend
// applies the alias's own params after the gateway's (llama-swap's
// setParamsByID replaces top-level keys), so they may override what the
// config asked for.
func noteAliases(log *slog.Logger, model, candidate string, catalog *backend.Catalog, hasParams bool) {
	if !hasParams {
		return
	}
	const msg = "candidate can resolve to a backend alias; the backend applies the alias's own params after the gateway's and may override them"
	if base, ok := catalog.AliasOf[candidate]; ok {
		if catalog.OwnParams(candidate) {
			log.Info(msg, "model", model, "candidate", candidate, "aliasOf", base)
		}
		return
	}
	if catalog.OwnParams(candidate) {
		log.Info(msg, "model", model, "candidate", candidate, "preset", true)
		return
	}
	if selector.IsGlob(candidate) {
		bases, aliases := selector.Matches(candidate, catalog.Models)
		if overriding := append(ownParamsAmong(aliases, catalog), ownParamsAmong(bases, catalog)...); len(overriding) > 0 {
			log.Info(msg, "model", model, "candidate", candidate, "aliases", overriding)
		}
	}
}

// ownParamsAmong returns the IDs in ids that the backend applies params of
// its own to: among base models, the presets; among aliases, those that
// aren't plain names.
func ownParamsAmong(ids []string, catalog *backend.Catalog) []string {
	var own []string
	for _, id := range ids {
		if catalog.OwnParams(id) {
			own = append(own, id)
		}
	}
	return own
}

func (m *candidates) Description() string {
	return m.description
}

func (m *candidates) Current(ctx context.Context) (Current, error) {
	sel, err := m.selector.Select(ctx, nil, m.backend)
	if err != nil {
		return Current{}, err
	}
	return m.currentOf(sel), nil
}

func (m *candidates) currentOf(sel selector.Selection) Current {
	cur := Current{Model: sel.Target, AliasOf: sel.AliasOf, Reason: sel.Reason}
	if sel.Index >= 0 {
		cur.Backend = m.selector.Candidates[sel.Index].BackendName
	}
	return cur
}

func (m *candidates) Inspect(ctx context.Context) (Inspection, error) {
	sel, err := m.selector.Select(ctx, nil, m.backend)
	var inspection Inspection
	if err == nil {
		cur := m.currentOf(sel)
		inspection.Current = &cur
	}
	for i, c := range m.selector.Inspect(ctx, m.backend) {
		status := CandidateStatus{Model: c.Model, Backend: m.backendNames[i], LoadIfNone: c.LoadIfNone, State: c.State, AliasOf: c.AliasOf, Winner: err == nil && sel.Index == i}
		if c.ID != c.Model {
			status.Match = c.ID
		}
		inspection.Candidates = append(inspection.Candidates, status)
	}
	return inspection, err
}

func (m *candidates) OutputCap() (limit int64, ok bool) {
	for i, c := range m.selector.Candidates {
		catalog := m.catalogs[i]
		ids := []string{c.Model}
		if selector.IsGlob(c.Model) {
			bases, aliases := selector.Matches(c.Model, catalog.Models)
			ids = append(bases, aliases...)
		}
		for _, id := range ids {
			if base, isAlias := catalog.AliasOf[id]; isAlias {
				id = base
			}
			model, _ := catalog.Model(id)
			if model.OutputCap > 0 && (!ok || model.OutputCap < limit) {
				limit, ok = model.OutputCap, true
			}
		}
	}
	return limit, ok
}

func (m *candidates) Serves(protocol backend.Protocol) bool {
	return m.protocols == nil || slices.Contains(m.protocols, protocol)
}

func (m *candidates) Chat(ctx context.Context, protocol backend.Protocol, body []byte, client backend.Client) (Result, error) {
	sel, res, err := m.choose(ctx, protocol)
	if err != nil {
		return res, err
	}
	reply, err := pipeline.Serve(ctx, protocol, body, client, m.selected(sel))
	res.Chat, res.Dropped = reply.Chat, reply.Dropped
	return res, err
}

// choose runs selection for a request in protocol, among the candidates whose
// backend it can reach, and returns the winner with the Result filled as far
// as selection got.
func (m *candidates) choose(ctx context.Context, protocol backend.Protocol) (selector.Selection, Result, error) {
	if !m.Serves(protocol) {
		return selector.Selection{}, Result{}, &pipeline.Error{Status: http.StatusNotFound, Type: "model_not_found", Message: fmt.Sprintf("this model is not served over %s; it serves %s", protocol, joinProtocols(m.protocols))}
	}
	reachable := m.reachable(protocol)
	if !slices.Contains(reachable, true) {
		return selector.Selection{}, Result{}, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("no candidate of this model can serve %s; its backends accept %s", protocol, m.acceptedProtocols())}
	}
	sel, err := m.selector.SelectAmong(ctx, m.backend, func(i int) bool { return reachable[i] })
	res := Result{Target: sel.Target, AliasOf: sel.AliasOf, Checked: sel.Checked, Reason: sel.Reason}
	if err != nil {
		return sel, res, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: m.stateError(err)}
	}
	if sel.Index < 0 {
		return sel, res, &pipeline.Error{Status: http.StatusServiceUnavailable, Type: "no_candidate_loaded", Message: "no candidate model is loaded"}
	}
	res.Backend = m.selector.Candidates[sel.Index].BackendName
	return sel, res, nil
}

// selected is the winning candidate as the pipeline takes it.
func (m *candidates) selected(sel selector.Selection) pipeline.Selected {
	i := sel.Index
	return pipeline.Selected{
		Backend:     m.backends[i],
		BackendName: m.backendNames[i],
		Model:       sel.Target,
		AliasOf:     sel.AliasOf,
		Accepted:    m.accepted[i],
		Params:      func(protocol backend.Protocol) []mergepatch.Patch { return m.patchesFor(i, protocol) },
	}
}

// generationFields only shape a reply, so params may set them for a chat, while
// the Claude API refuses them when counting (verified).
var generationFields = []string{"max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata", "service_tier", "context_management"}

// CountTokens runs selection as a chat would, then asks the winner's backend,
// with params in Messages spelling applied, since they change what is counted.
// A backend that can't count gets an estimate of the client's body, in its
// response shape, and no request.
func (m *candidates) CountTokens(ctx context.Context, body []byte, client backend.Client) (Result, error) {
	sel, res, err := m.choose(ctx, backend.AnthropicMessages)
	if err != nil {
		return res, err
	}
	counter, ok := m.backends[sel.Index].(backend.TokenCounter)
	if !ok {
		count := translate.EstimateMessagesTokens(body)
		if tokenizer, ok := m.backends[sel.Index].(backend.Tokenizer); ok {
			tokenize := func(text string) (int, error) { return tokenizer.Tokenize(ctx, sel.Target, text, client) }
			if count, err = translate.CountMessagesTokens(body, tokenize); err != nil {
				return res, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': %v", m.backendNames[sel.Index], err)}
			}
		}
		data, err := json.Marshal(map[string]int{"input_tokens": count})
		if err != nil {
			return res, &pipeline.Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
		res.Estimated = true
		res.Chat = backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusOK, ContentType: "application/json", Body: data}}
		return res, nil
	}
	if body, err = pipeline.Prepare(body, m.selected(sel), backend.AnthropicMessages); err != nil {
		return res, &pipeline.Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
	}
	for _, field := range generationFields {
		if body, err = sjson.DeleteBytes(body, field); err != nil {
			return res, &pipeline.Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
	}
	resp, err := counter.CountTokens(ctx, backend.ChatRequest{Protocol: backend.AnthropicMessages, Body: body, Client: client, AliasOf: sel.AliasOf})
	if err != nil {
		return res, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': %v", m.backendNames[sel.Index], err)}
	}
	if resp.Status/100 != 2 {
		resp.Body = translate.MessagesError(resp.Status, resp.Body)
		resp.ContentType = "application/json"
	}
	res.Chat = backend.ChatResult{Response: resp}
	return res, nil
}

// reachable says, per candidate, whether its backend can serve protocol, and
// logs each one that can't at error level: a request in that protocol can
// never use it, which the config can't state unless the facade lists its
// protocols.
func (m *candidates) reachable(protocol backend.Protocol) []bool {
	reachable := make([]bool, len(m.backends))
	for i := range m.backends {
		if reachable[i] = reaches(protocol, m.accepted[i]); !reachable[i] {
			m.log.Error("candidate skipped; its backend can't serve this protocol", "model", m.name, "candidate", m.selector.Candidates[i].Model, "backend", m.backendNames[i], "protocol", protocol)
		}
	}
	return reachable
}

// reaches reports whether a request in protocol can get to a backend that
// may be sent these.
func reaches(protocol backend.Protocol, accepted []backend.Protocol) bool {
	_, _, ok := translate.Route(protocol, accepted)
	return ok
}

// acceptedProtocols lists what any candidate's backend accepts, in order.
func (m *candidates) acceptedProtocols() string {
	var accepted []backend.Protocol
	for _, protocols := range m.accepted {
		for _, p := range protocols {
			if !slices.Contains(accepted, p) {
				accepted = append(accepted, p)
			}
		}
	}
	return joinProtocols(accepted)
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
		if m.useProtocols[i] != "" {
			fmt.Fprintf(w, "     use protocol: %s\n", m.useProtocols[i])
		}
		hasParams := !m.params.empty() || !m.patches[i].empty()
		for _, line := range describeAliases(c.Model, m.backendNames[i], m.catalogs[i], hasParams) {
			fmt.Fprintf(w, "     %s\n", line)
		}
		if !m.params.hasBlocks() && !m.patches[i].hasBlocks() {
			describePatches(w, "params", m.patchesFor(i, ""))
			continue
		}
		for _, protocol := range m.accepted[i] {
			describePatches(w, fmt.Sprintf("params for %s", protocol), m.patchesFor(i, protocol))
		}
	}
}

// patchesFor returns candidate i's patches in application order, for a body
// in protocol: the facade's for every protocol, its block, then the
// candidate's two.
func (m *candidates) patchesFor(i int, protocol backend.Protocol) []mergepatch.Patch {
	return m.patches[i].appendFor(m.params.appendFor(nil, protocol), protocol)
}

func describePatches(w io.Writer, label string, patches []mergepatch.Patch) {
	if composed, ok := mergepatch.Compose(patches); ok {
		fmt.Fprintf(w, "     %s: %s\n", label, composed)
		return
	}
	fmt.Fprintf(w, "     %s (not expressible as one patch; applied in order):\n", label)
	for _, p := range patches {
		fmt.Fprintf(w, "       %s\n", p)
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
		presets := ownParamsAmong(bases, catalog)
		if len(presets) > 0 {
			lines = append(lines, "presets listed without their base model: "+listOrNone(presets))
		}
		if len(ownParamsAmong(aliases, catalog))+len(presets) > 0 && hasParams {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	if base, ok := catalog.AliasOf[candidate]; ok {
		lines := []string{"alias of " + base}
		if hasParams && catalog.OwnParams(candidate) {
			lines = append(lines, aliasOverrideNote)
		}
		return lines
	}
	if catalog.OwnParams(candidate) {
		lines := []string{"preset listed without its base model"}
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
