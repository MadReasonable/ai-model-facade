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
	Type        string      `yaml:"type" doc:"The facade type; candidates is the only one." schema:"default=candidates"`
	Backend     string      `yaml:"backend" doc:"The backend of every candidate that doesn't name its own. Required unless every candidate names one." schema:"ref=backends"`
	Description string      `yaml:"description" doc:"Shown in the model lists and /amf/models."`
	Protocols   []string    `yaml:"protocols" doc:"The protocols this facade serves; a request in another gets 404. Naming one a candidate's backend can't accept is an error. Left out, all of them." schema:"ref=protocols"` // default: every protocol
	Params      yaml.Node   `yaml:"params" doc:"Merge patches applied to every request for this facade, before the winning candidate's: in all for every protocol, or in a block named for the protocol the backend is sent." schema:"kind=params"`
	Candidates  []yaml.Node `yaml:"candidates" doc:"The models to choose from, in order of preference: the first loaded wins, else the first loading, else the first with loadIfNone." schema:"required"`

	// Access says who may ask for the facade.
	config.Access `yaml:",inline"`
}

type candidateConfig struct {
	Model      string    `yaml:"model" doc:"A model ID or alias the backend lists, or a pattern using *, ? or [...]." schema:"required"`
	Backend    string    `yaml:"backend" doc:"The backend this candidate is on. Left out, the facade's." schema:"ref=backends"` // default: the virtual model's backend
	LoadIfNone bool      `yaml:"loadIfNone" doc:"If no candidate is loaded or loading, the first loadIfNone candidate is used, and its backend loads it. Must be an exact ID."`
	Params     yaml.Node `yaml:"params" doc:"Merge patches applied after the facade's when this candidate wins." schema:"kind=params"`
	// ResponsesHistory says who keeps the Responses conversations the
	// candidate serves: the gateway (default) or the service.
	ResponsesHistory string `yaml:"responsesHistory" doc:"Who keeps the Responses conversations this candidate serves: the gateway, or the service, on a backend that keeps them, openAi." schema:"enum=gateway|service,default=gateway"`
	// UseProtocol is the protocol the backend receives for this candidate, one
	// it accepts; default: the client's, else the first it can be translated
	// to.
	UseProtocol string `yaml:"useProtocol" doc:"The protocol the backend is sent for this candidate, one it accepts, such as openAiResponses for a model OpenAI serves only through Responses. Left out, the client's own, else the first the request can be translated to." schema:"ref=protocols"`
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
	pipeline     *pipeline.Pipeline
	// services holds, per candidate, its backend when the candidate keeps its
	// Responses conversations there, else nil.
	services []backend.ResponsesKeeper
	access   config.Access   // who may ask for the facade
	allowed  []config.Access // per candidate: who may use its backend
}

func newCandidates(entry config.Entry, env Env) (*candidates, error) {
	var c candidatesConfig
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	m := &candidates{name: entry.Name, log: env.Log, backendName: c.Backend, description: c.Description, selector: &selector.FirstLoaded{}, pipeline: env.Pipeline, access: c.Access}
	if c.Backend != "" {
		if m.backend = env.Backends[c.Backend]; m.backend == nil {
			return nil, fmt.Errorf("line %d: backend %s is not defined", entry.Node.Line, config.QuoteName(c.Backend))
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
			return nil, fmt.Errorf("line %d: backend %s is not defined", node.Line, config.QuoteName(name))
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
		service, err := responsesService(cc.ResponsesHistory, b, name, node.Line)
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
		m.services = append(m.services, service)
		m.allowed = append(m.allowed, env.Access[name])
	}
	m.notePassthrough(env.Log, entry.Name)
	return m, nil
}

// checkCandidates is the part of newCandidates that needs no backend.
func checkCandidates(entry config.Entry) error {
	var c candidatesConfig
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return err
	}
	if _, err := parseProtocols(c.Protocols, entry.Node.Line); err != nil {
		return err
	}
	if _, err := compileParams(&c.Params); err != nil {
		return err
	}
	if len(c.Candidates) == 0 {
		return fmt.Errorf("line %d: candidates is required", entry.Node.Line)
	}
	for i := range c.Candidates {
		var cc candidateConfig
		if err := config.DecodeStrict(&c.Candidates[i], &cc); err != nil {
			return err
		}
		if _, err := compileParams(&cc.Params); err != nil {
			return err
		}
	}
	return nil
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

// responsesService returns the backend that keeps a candidate's Responses
// conversations under responsesHistory, nil when the gateway keeps them.
func responsesService(keeper string, b backend.Backend, backendName string, line int) (backend.ResponsesKeeper, error) {
	switch keeper {
	case "", "gateway":
		return nil, nil
	case "service":
		service, ok := b.(backend.ResponsesKeeper)
		if !ok {
			return nil, fmt.Errorf("line %d: responsesHistory is 'service', but backend '%s' keeps no Responses conversations; use 'gateway'", line, backendName)
		}
		return service, nil
	}
	return nil, fmt.Errorf("line %d: responsesHistory '%s' is not 'gateway' or 'service'", line, keeper)
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

func (m *candidates) Access() config.Access {
	return m.access
}

func (m *candidates) Current(ctx context.Context, client backend.Client) (Current, error) {
	if !m.usableBy(client) {
		return Current{Reason: noUsableBackend}, nil
	}
	sel, err := m.selector.SelectAmong(ctx, m.backend, m.deniedTo(client))
	if err != nil {
		return Current{}, err
	}
	return m.currentOf(sel), nil
}

// deniedTo returns, for SelectAmong, why client can't use candidate i: its
// backend's access list leaves client out.
func (m *candidates) deniedTo(client backend.Client) func(i int) string {
	return func(i int) string {
		if m.allowed[i].Allows(client.Name, client.Groups) {
			return ""
		}
		return "this client may not use backend '" + m.backendNames[i] + "'"
	}
}

// noUsableBackend is the reason when client may use the facade but none of
// its candidates' backends.
const noUsableBackend = "no backend this client may use"

// usableBy reports whether client may use the backend of any candidate.
func (m *candidates) usableBy(client backend.Client) bool {
	return slices.ContainsFunc(m.allowed, func(a config.Access) bool { return a.Allows(client.Name, client.Groups) })
}

func (m *candidates) currentOf(sel selector.Selection) Current {
	cur := Current{Model: sel.Target, AliasOf: sel.AliasOf, Reason: sel.Reason}
	if sel.Index >= 0 {
		cur.Backend = m.selector.Candidates[sel.Index].BackendName
	}
	return cur
}

func (m *candidates) Inspect(ctx context.Context, client backend.Client) (Inspection, error) {
	if !m.usableBy(client) {
		return Inspection{Current: &Current{Reason: noUsableBackend}}, nil
	}
	denied := m.deniedTo(client)
	sel, err := m.selector.SelectAmong(ctx, m.backend, denied)
	var inspection Inspection
	if err == nil {
		cur := m.currentOf(sel)
		inspection.Current = &cur
	}
	for i, c := range m.selector.Inspect(ctx, m.backend) {
		if denied(i) != "" {
			continue // a caller sees only the backends it may use
		}
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
	sel, res, err := m.choose(ctx, protocol, client)
	if err != nil {
		return res, err
	}
	reply, err := m.pipeline.Serve(ctx, protocol, body, client, m.selected(sel))
	res.Chat, res.Dropped = reply.Chat, reply.Dropped
	return res, err
}

// choose runs selection for a request in protocol from client, among the
// candidates whose backend it can reach and client may use, and returns the
// winner with the Result filled as far as selection got.
func (m *candidates) choose(ctx context.Context, protocol backend.Protocol, client backend.Client) (selector.Selection, Result, error) {
	if !m.Serves(protocol) {
		return selector.Selection{}, Result{}, &pipeline.Error{Status: http.StatusNotFound, Type: "model_not_found", Message: fmt.Sprintf("this model is not served over %s; it serves %s", protocol, joinProtocols(m.protocols))}
	}
	reachable := m.reachable(protocol)
	if !slices.Contains(reachable, true) {
		return selector.Selection{}, Result{}, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("no candidate of this model can serve %s; its backends accept %s", protocol, m.acceptedProtocols())}
	}
	denied := m.deniedTo(client)
	skip := func(i int) string {
		if !reachable[i] {
			return "its backend can't serve this protocol"
		}
		return denied(i)
	}
	var checked []string
	for i, c := range m.selector.Candidates {
		reason := skip(i)
		if reason == "" {
			checked = nil
			break
		}
		checked = append(checked, c.On(c.Model)+": "+reason)
	}
	if checked != nil {
		// Unlike 503, this is no state that may change before a retry.
		return selector.Selection{}, Result{Checked: checked, Reason: noUsableBackend}, &pipeline.Error{Status: http.StatusForbidden, Type: "permission_error", Message: fmt.Sprintf("this client may not use the backend of any candidate of this model that can serve %s", protocol)}
	}
	sel, err := m.selector.SelectAmong(ctx, m.backend, skip)
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
		Service:     m.services[i],
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
	sel, res, err := m.choose(ctx, backend.AnthropicMessages, client)
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
	for _, line := range m.access.Describe() {
		fmt.Fprintf(w, "  %s\n", line)
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
		if m.services[i] != nil {
			fmt.Fprintf(w, "     responses history: kept by the service\n")
		}
		if m.useProtocols[i] != "" {
			fmt.Fprintf(w, "     use protocol: %s\n", m.useProtocols[i])
		}
		for _, line := range m.allowed[i].Describe() {
			fmt.Fprintf(w, "     backend '%s' %s\n", m.backendNames[i], line)
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
