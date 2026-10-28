package openwebui

import (
	"context"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// stateOf summarises a state read as model ID -> ready, starting or unknown;
// models it leaves out are unloaded.
func stateOf(models []backend.ModelInfo) map[string]string {
	states := map[string]string{}
	for _, m := range models {
		switch {
		case m.Unknown:
			states[m.ID] = "unknown"
		case m.Starting:
			states[m.ID] = "starting"
		default:
			states[m.ID] = "ready"
		}
	}
	return states
}

// Each kind of model in Open WebUI's list gets its state from what its
// connection reports: Ollama's loaded flag, llama-swap's status, a facade's
// current selection; a workspace model has its base's; anything else is
// unknown.
func TestOpenWebUI_ReadsEachKindOfModelsState(t *testing.T) {
	facadeLoaded := testkit.FacadeEntry("facade-loaded", "m", "loaded")
	f := testkit.NewOpenWebUI(t,
		testkit.OllamaEntry("ollama-up", true),
		testkit.OllamaEntry("ollama-down", false),
		testkit.SwapEntry("swap-up", "loaded"),
		testkit.SwapEntry("swap-down", "unloaded"),
		facadeLoaded,
		testkit.FacadeEntry("facade-starting", "m", "starting"),
		testkit.FacadeEntry("facade-fallback", "m", "loadIfNone"),
		testkit.FacadeEntry("facade-none", "", "no candidate loaded"),
		testkit.WorkspaceEntry("workspace", "facade-loaded", facadeLoaded),
		testkit.WorkspaceEntry("orphan", "gone", ""),
		testkit.PlainEntry("hosted"),
	)
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ollama-up": "ready", "swap-up": "ready", "facade-loaded": "ready", "facade-starting": "starting", "orphan": "unknown", "hosted": "unknown"}
	states := stateOf(got)
	if len(states) != len(want) {
		t.Errorf("got %v, want %v", states, want)
	}
	for id, s := range want {
		if states[id] != s {
			t.Errorf("%s: got %q, want %q", id, states[id], s)
		}
	}
}

// Workspace models fold into the model they're built on, through a chain of
// workspace models; one whose base isn't listed stands alone.
func TestOpenWebUI_FoldsWorkspaceModelsIntoTheirBase(t *testing.T) {
	base := testkit.OllamaEntry("base", true)
	f := testkit.NewOpenWebUI(t,
		base,
		testkit.WorkspaceEntry("workspace", "base", base),
		testkit.WorkspaceEntry("workspace-of-workspace", "workspace", ""),
		testkit.WorkspaceEntry("orphan", "gone", ""),
	)
	b := newBackend(t, f, "")
	listed, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	catalog := backend.NewCatalog(listed)
	if len(listed) != 2 || catalog.AliasOf["workspace"] != "base" || catalog.AliasOf["workspace-of-workspace"] != "base" || catalog.AliasOf["orphan"] != "" {
		t.Errorf("listed %+v", listed)
	}
	loaded, _ := b.LoadedModels(context.Background())
	if len(loaded) != 2 || loaded[0].ID != "base" || strings.Join(loaded[0].Aliases, ",") != "workspace,workspace-of-workspace" {
		t.Errorf("loaded %+v", loaded)
	}
}

// llama-swap presets fold into their base model when Open WebUI lists it,
// exactly or behind a connection prefix, and stand alone, marked as presets,
// when it doesn't or when the prefix leaves more than one candidate. A
// workspace model built on a preset folds through it into the base.
func TestOpenWebUI_FoldsLlamaSwapPresetsIntoTheirBase(t *testing.T) {
	fast := testkit.SwapPresetEntry("a:Fast", "a", "unloaded")
	f := testkit.NewOpenWebUI(t,
		testkit.SwapEntry("a", "unloaded"),
		fast,
		testkit.SwapPresetEntry("a:Slow", "a", "unloaded"),
		testkit.SwapPresetEntry("b:Fast", "b", "unloaded"), // base not listed
		testkit.WorkspaceEntry("w", "a:Fast", fast),
		testkit.SwapEntry("p.c", "unloaded"),
		testkit.SwapPresetEntry("p.c:X", "c", "unloaded"),
		testkit.SwapEntry("q.d", "unloaded"),
		testkit.SwapEntry("r.d", "unloaded"),
		testkit.SwapPresetEntry("s.d:Y", "d", "unloaded"), // two prefixed bases match
	)
	got, err := newBackend(t, f, "").ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, m := range got {
		s := m.ID + "[" + strings.Join(m.Aliases, ",") + "]"
		if m.Preset {
			s += "*"
		}
		summary = append(summary, s)
	}
	if want := "a[a:Fast,a:Slow,w] b:Fast[]* p.c[p.c:X] q.d[] r.d[] s.d:Y[]*"; strings.Join(summary, " ") != want {
		t.Errorf("got %s, want %s", strings.Join(summary, " "), want)
	}
}

// A request for a folded preset or a workspace alias counts under its base
// model's llama-swap ID, like a request for the base itself.
func TestOpenWebUI_FindsTheLlamaSwapModelBehindAnAlias(t *testing.T) {
	list := &swapList{bases: []swapBase{{id: "p.a", swapID: "a", aliases: []string{"p.a:Fast", "w"}}, {id: "x"}}}
	for id, want := range map[string]string{"p.a": "a", "p.a:Fast": "a", "w": "a", "x": "", "unlisted": ""} {
		if got := list.swapIDOf(id); got != want {
			t.Errorf("%s: got %q, want %q", id, got, want)
		}
	}
}

func TestOpenWebUI_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{
		"{type: openWebUi}",
		"{type: openWebUi, baseUrl: x, chats: forget}",
		"{type: openWebUi, baseUrl: x, pollIntervalSeconds: -1}",
		"{type: openWebUi, baseUrl: x, llamaSwap: {prefixId: p}}",
		"{type: openWebUi, baseUrl: x, llamaSwap: {backend: ls, prefix: p}}",
	} {
		if _, err := New(testkit.Entry(t, "openWebUi", src), discard); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
