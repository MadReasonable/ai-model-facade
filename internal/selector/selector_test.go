package selector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

type fakeBackend struct {
	loaded   []string
	starting []string
	unknown  []string            // state can't be read
	aliases  map[string][]string // base model ID -> aliases
	err      error
	calls    int // LoadedModels calls
}

func (f *fakeBackend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return nil, nil
}

func (f *fakeBackend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	return backend.ChatResult{}, nil
}

func (f *fakeBackend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	f.calls++
	var models []backend.ModelInfo
	for _, id := range f.loaded {
		models = append(models, backend.ModelInfo{ID: id, Aliases: f.aliases[id]})
	}
	for _, id := range f.starting {
		models = append(models, backend.ModelInfo{ID: id, Aliases: f.aliases[id], Starting: true})
	}
	for _, id := range f.unknown {
		models = append(models, backend.ModelInfo{ID: id, Aliases: f.aliases[id], Unknown: true})
	}
	return models, f.err
}

func TestStartingOrder(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{
		{Model: "a"},
		{Model: "b", LoadIfNone: true},
		{Model: "qwen3.6-35b:Slacker"},
		{Model: "c"},
	}}
	cases := []struct {
		name              string
		loaded, starting  []string
		target, aliasOf   string
		reason, lastCheck string
	}{
		{"ready anywhere beats starting", []string{"c"}, []string{"a"}, "c", "", "loaded", "c: loaded"},
		{"first starting wins", nil, []string{"c", "a"}, "a", "", "starting", "a: waiting for it to finish starting"},
		{"starting alias", nil, []string{"qwen3.6-35b-a3b:q8_0"}, "qwen3.6-35b:Slacker", "qwen3.6-35b-a3b:q8_0", "starting", "qwen3.6-35b:Slacker: waiting for it to finish starting"},
		{"starting beats loadIfNone", nil, []string{"c"}, "c", "", "starting", "c: waiting for it to finish starting"},
		{"neither: loadIfNone", nil, nil, "b", "", "loadIfNone", "b: loading (loadIfNone)"},
	}
	for _, c := range cases {
		sel, err := s.Select(context.Background(), nil, &fakeBackend{loaded: c.loaded, starting: c.starting, aliases: qwen36})
		if err != nil {
			t.Fatal(err)
		}
		last := sel.Checked[len(sel.Checked)-1]
		if sel.Target != c.target || sel.AliasOf != c.aliasOf || sel.Reason != c.reason || last != c.lastCheck {
			t.Errorf("%s: got %q (alias of %q, %s), last check %q; checked %q", c.name, sel.Target, sel.AliasOf, sel.Reason, last, sel.Checked)
		}
	}
	// Starting candidates are noted as such while the list is walked.
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{starting: []string{"a"}})
	if got := strings.Join(sel.Checked, "|"); got != "a: starting|b: not loaded|qwen3.6-35b:Slacker: not loaded|c: not loaded|a: waiting for it to finish starting" {
		t.Errorf("checked %q", got)
	}
}

func TestStartingGlob(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "qwen3.8-27b:*"}}}
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{starting: []string{"qwen3.8-27b:q6_k"}, aliases: qwen36})
	if sel.Target != "qwen3.8-27b:q6_k" || sel.Reason != "starting" || sel.Checked[0] != "qwen3.8-27b:*: starting as qwen3.8-27b:q6_k" {
		t.Errorf("got %+v", sel)
	}
}

// The qwen3.6 base model with its llama-swap preset aliases, as /v1/models lists them.
var qwen36 = map[string][]string{
	"qwen3.6-35b-a3b:q8_0": {"qwen3.6-35b:Dreamer", "qwen3.6-35b:Philosopher", "qwen3.6-35b:Slacker", "qwen3.6-35b:Thinker"},
	"qwen3.8-27b:q6_k":     {"qwen3.8-27b:Dreamer", "qwen3.8-27b:Slacker"},
}

func TestAliasCandidateIsLoadedWhenItsBaseIs(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "qwen3.6-35b:Slacker"}}}
	sel, err := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"qwen3.6-35b-a3b:q8_0"}, aliases: qwen36})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Target != "qwen3.6-35b:Slacker" || sel.AliasOf != "qwen3.6-35b-a3b:q8_0" || sel.Reason != "loaded" {
		t.Errorf("got %+v", sel)
	}
	if sel.Checked[0] != "qwen3.6-35b:Slacker: loaded (alias of qwen3.6-35b-a3b:q8_0)" {
		t.Errorf("checked %q", sel.Checked)
	}
	// Not loaded when the base model isn't.
	sel, _ = s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"qwen3.8-27b:q6_k"}, aliases: qwen36})
	if sel.Index != -1 {
		t.Errorf("got %+v", sel)
	}
}

// `qwen3.8-27b:*` must pick the quant, not the `qwen3.8-27b:Dreamer` preset
// that sorts before it.
func TestGlobPrefersBaseModelsOverAliases(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "qwen3.8-27b:*"}}}
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"qwen3.8-27b:q6_k"}, aliases: qwen36})
	if sel.Target != "qwen3.8-27b:q6_k" || sel.AliasOf != "" {
		t.Errorf("got %+v", sel)
	}
}

func TestGlobMatchesAliasesWhenNoBaseDoes(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "*:Slacker"}}}
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"qwen3.8-27b:q6_k", "qwen3.6-35b-a3b:q8_0"}, aliases: qwen36})
	if sel.Target != "qwen3.6-35b:Slacker" || sel.AliasOf != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("got %+v", sel)
	}
	if sel.Checked[0] != "*:Slacker: loaded as qwen3.6-35b:Slacker (alias of qwen3.6-35b-a3b:q8_0)" {
		t.Errorf("checked %q", sel.Checked)
	}
}

func TestMatchesOrdersBasesThenAliases(t *testing.T) {
	models := []backend.ModelInfo{
		{ID: "qwen3.8-27b:q6_k", Aliases: qwen36["qwen3.8-27b:q6_k"]},
		{ID: "qwen3.8-27b:iq4_xs"},
	}
	// Aliases of a matching base model are left out: the base always wins.
	bases, aliases := Matches("qwen3.8-27b:*", models)
	if strings.Join(bases, ",") != "qwen3.8-27b:iq4_xs,qwen3.8-27b:q6_k" || len(aliases) != 0 {
		t.Errorf("got bases %v, aliases %v", bases, aliases)
	}
	bases, aliases = Matches("*:Slacker", models)
	if len(bases) != 0 || strings.Join(aliases, ",") != "qwen3.8-27b:Slacker" {
		t.Errorf("got bases %v, aliases %v", bases, aliases)
	}
}

// The candidate list from config.example.yaml's `task` model.
var task = &FirstLoaded{Candidates: []Candidate{
	{Model: "qwen3.6-35b-a3b:q8_0"},
	{Model: "qwen3.8-flash-next:iq3_s"},
	{Model: "qwen3.8-27b:*"},
	{Model: "qwen2.5-coder-3b:q8_0", LoadIfNone: true},
}}

// The matrix sets from docs/design.md §6.
func TestTaskAgainstMatrixSets(t *testing.T) {
	cases := []struct {
		set    string
		loaded []string
		target string
		reason string
	}{
		{"daily", []string{"qwen2.5-coder-3b:q8_0", "qwen3.8-27b:q6_k", "qwen3.6-35b-a3b:q8_0"}, "qwen3.6-35b-a3b:q8_0", "loaded"},
		{"coding", []string{"qwen3.8-flash-next:iq3_s", "qwen2.5-coder-3b:q8_0"}, "qwen3.8-flash-next:iq3_s", "loaded"},
		{"testing", []string{"qwen3.8-27b:iq4_xs", "qwen2.5-coder-3b:q8_0"}, "qwen3.8-27b:iq4_xs", "loaded"},
		{"large", []string{"qwen2.5-coder-3b:q8_0", "big-model"}, "qwen2.5-coder-3b:q8_0", "loaded"},
		{"medium", []string{"flux2-dev:q8"}, "qwen2.5-coder-3b:q8_0", "loadIfNone"},
	}
	for _, c := range cases {
		sel, err := task.Select(context.Background(), nil, &fakeBackend{loaded: c.loaded})
		if err != nil {
			t.Fatal(err)
		}
		if sel.Target != c.target || sel.Reason != c.reason {
			t.Errorf("%s: got %q (%s), want %q (%s); checked %v", c.set, sel.Target, sel.Reason, c.target, c.reason, sel.Checked)
		}
	}
}

// A loadIfNone candidate in the middle of the list keeps its place when it is
// loaded, but is only loaded on demand after every candidate has been checked.
func TestLoadIfNoneInMiddleIsFallback(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{
		{Model: "a"},
		{Model: "b", LoadIfNone: true},
		{Model: "c"},
		{Model: "d", LoadIfNone: true},
	}}
	cases := []struct {
		loaded []string
		target string
		reason string
	}{
		{[]string{"a", "b", "c"}, "a", "loaded"},
		{[]string{"b", "c"}, "b", "loaded"},
		{[]string{"c"}, "c", "loaded"},
		{[]string{"d"}, "d", "loaded"},
		{[]string{"x"}, "b", "loadIfNone"}, // the first loadIfNone candidate, not d
	}
	for _, c := range cases {
		sel, err := s.Select(context.Background(), nil, &fakeBackend{loaded: c.loaded})
		if err != nil {
			t.Fatal(err)
		}
		if sel.Target != c.target || sel.Reason != c.reason {
			t.Errorf("loaded %v: got %q (%s), want %q (%s); checked %v", c.loaded, sel.Target, sel.Reason, c.target, c.reason, sel.Checked)
		}
	}
	// Every candidate is checked before falling back.
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"x"}})
	want := "a: not loaded|b: not loaded|c: not loaded|d: not loaded|b: loading (loadIfNone)"
	if got := strings.Join(sel.Checked, "|"); got != want {
		t.Errorf("checked %q, want %q", got, want)
	}
}

func TestGlobPicksFirstSortedMatch(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "m:*"}}}
	sel, _ := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"m:b", "m:a", "x"}})
	if sel.Target != "m:a" || sel.Index != 0 {
		t.Errorf("got %+v", sel)
	}
}

func TestNoMatch(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "a"}, {Model: "b:*"}}}
	sel, err := s.Select(context.Background(), nil, &fakeBackend{loaded: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Index != -1 || sel.Reason != "no candidate loaded" || len(sel.Checked) != 2 {
		t.Errorf("got %+v", sel)
	}
}

// A model whose state is unknown selects like one that isn't loaded, but is
// logged as unknown; it can still win as the loadIfNone fallback.
func TestUnknownStateSelectsAsNotLoaded(t *testing.T) {
	s := &FirstLoaded{Candidates: []Candidate{{Model: "u"}, {Model: "w:*"}, {Model: "r"}, {Model: "u", LoadIfNone: true}}}
	b := &fakeBackend{loaded: []string{"r"}, unknown: []string{"u", "w:1"}}
	sel, _ := s.Select(context.Background(), nil, b)
	if sel.Target != "r" || strings.Join(sel.Checked, "; ") != "u: state unknown; w:*: no match loaded; state unknown for w:1; r: loaded" {
		t.Errorf("got %+v", sel)
	}
	b.loaded = nil
	sel, _ = s.Select(context.Background(), nil, b)
	if sel.Target != "u" || sel.Reason != "loadIfNone" {
		t.Errorf("fallback: got %+v", sel)
	}
}

// Candidates on different backends are walked in list order as one list;
// each backend's state is read once, and candidates on another backend than
// the default are labelled with it.
func TestMixedBackends(t *testing.T) {
	def := &fakeBackend{starting: []string{"a"}}
	other := &fakeBackend{loaded: []string{"b"}}
	s := &FirstLoaded{Candidates: []Candidate{
		{Model: "x"},
		{Model: "b", Backend: other, BackendName: "other"},
		{Model: "a"},
		{Model: "c", Backend: other, BackendName: "other", LoadIfNone: true},
	}}
	sel, err := s.Select(context.Background(), nil, def)
	if err != nil || sel.Index != 1 || sel.Target != "b" || strings.Join(sel.Checked, "; ") != "x: not loaded; b on other: loaded" {
		t.Errorf("got %+v, %v", sel, err)
	}
	if def.calls != 1 || other.calls != 1 {
		t.Errorf("state reads: default %d, other %d", def.calls, other.calls)
	}
	// Ready beats starting across backends, starting beats loadIfNone.
	other.loaded = nil
	sel, _ = s.Select(context.Background(), nil, def)
	if sel.Index != 2 || sel.Reason != "starting" {
		t.Errorf("starting: got %+v", sel)
	}
	def.starting = nil
	sel, _ = s.Select(context.Background(), nil, def)
	if sel.Index != 3 || sel.Reason != "loadIfNone" || sel.Checked[len(sel.Checked)-1] != "c on other: loading (loadIfNone)" {
		t.Errorf("fallback: got %+v", sel)
	}
}

// A backend whose state can't be read makes its candidates count as not
// loaded while others still serve; only if no backend can be read does the
// selection fail.
func TestMixedBackendStateFailure(t *testing.T) {
	def := &fakeBackend{err: errors.New("down")}
	other := &fakeBackend{loaded: []string{"b"}}
	s := &FirstLoaded{Candidates: []Candidate{{Model: "a"}, {Model: "b", Backend: other, BackendName: "other"}}}
	sel, err := s.Select(context.Background(), nil, def)
	if err != nil || sel.Target != "b" || sel.Checked[0] != "a: state unavailable" {
		t.Errorf("got %+v, %v", sel, err)
	}
	other.err = errors.New("also down")
	if sel, err = s.Select(context.Background(), nil, def); err == nil || sel.Reason != "backend state unavailable" {
		t.Errorf("all down: got %+v, %v", sel, err)
	}
}

// slowBackend reports loaded as ready, but only once release is closed.
type slowBackend struct {
	fakeBackend
	release chan struct{}
}

func (s *slowBackend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	<-s.release
	var models []backend.ModelInfo
	for _, id := range s.loaded {
		models = append(models, backend.ModelInfo{ID: id})
	}
	return models, nil
}

// selectAsync runs Select and delivers its result on the returned channel.
func selectAsync(s *FirstLoaded, b backend.Backend) <-chan Selection {
	out := make(chan Selection, 1)
	go func() {
		sel, _ := s.Select(context.Background(), nil, b)
		out <- sel
	}()
	return out
}

// A slow backend holds up selection only when one of its candidates comes
// before the winner, or when nothing is ready.
func TestSlowBackend(t *testing.T) {
	slow := &slowBackend{fakeBackend: fakeBackend{loaded: []string{"b"}}, release: make(chan struct{})}

	// Ready before any candidate on the slow backend: no wait.
	after := &FirstLoaded{Candidates: []Candidate{{Model: "a"}, {Model: "b", Backend: slow, BackendName: "slow"}}}
	select {
	case sel := <-selectAsync(after, &fakeBackend{loaded: []string{"a"}}):
		if sel.Target != "a" || sel.Reason != "loaded" {
			t.Errorf("ready first: got %+v", sel)
		}
	case <-time.After(time.Second):
		t.Fatal("ready first: waited for the slow backend")
	}

	// A candidate on the slow backend comes first, or only a starting
	// candidate is ready to choose: both wait, then the slow one wins.
	cases := map[string]*FirstLoaded{
		"slow first":        {Candidates: []Candidate{{Model: "b", Backend: slow, BackendName: "slow"}, {Model: "a"}}},
		"starting vs ready": {Candidates: []Candidate{{Model: "s"}, {Model: "b", Backend: slow, BackendName: "slow"}}},
	}
	results := map[string]<-chan Selection{}
	for name, s := range cases {
		results[name] = selectAsync(s, &fakeBackend{loaded: []string{"a"}, starting: []string{"s"}})
	}
	time.Sleep(50 * time.Millisecond)
	for name, out := range results {
		select {
		case sel := <-out:
			t.Errorf("%s: chose %+v before the slow backend answered", name, sel)
		default:
		}
	}
	close(slow.release)
	for name, out := range results {
		if sel := <-out; sel.Target != "b" || sel.Reason != "loaded" {
			t.Errorf("%s: got %+v", name, sel)
		}
	}
}

func TestStateErrorPropagates(t *testing.T) {
	_, err := task.Select(context.Background(), nil, &fakeBackend{err: errors.New("down")})
	if err == nil {
		t.Error("expected an error")
	}
}

func TestValidate(t *testing.T) {
	bad := []Candidate{
		{Model: ""},
		{Model: "a["},
		{Model: "a:*", LoadIfNone: true},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v: expected an error", c)
		}
	}
	for _, c := range task.Candidates {
		if err := c.Validate(); err != nil {
			t.Error(err)
		}
	}
}
