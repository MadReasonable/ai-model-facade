package openwebui

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// With the llamaSwap block, llama-swap models take the named backend's exact
// state, prefix removed, instead of their `status`, which says loaded while
// starting and stopping; a preset takes its base's.
func TestLlamaSwapBlock_UsesLlamaSwapsExactState(t *testing.T) {
	swap, ls := newSwap(t, "", testkit.SwapModel{ID: "a", Aliases: []string{"a:Fast"}}, testkit.SwapModel{ID: "b"})
	swap.SetState("a", "starting")
	swap.SetState("b", "stopping")
	f := testkit.NewOpenWebUI(t,
		testkit.SwapEntry("swap.a", "loaded"),
		testkit.SwapPresetEntry("swap.a:Fast", "a", "loaded"),
		testkit.SwapEntry("swap.b", "loaded"),
	)
	b := newBackend(t, f, ", llamaSwap: {backend: ls, prefixId: swap}")
	if warned, err := link(t, b, map[string]backend.Backend{"ls": ls}); err != nil || warned != nil {
		t.Fatal(err, warned)
	}
	got, err := b.LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// a is starting, with its preset folded in; b is stopping, so it doesn't count.
	if len(got) != 1 || got[0].ID != "swap.a" || !got[0].Starting || strings.Join(got[0].Aliases, ",") != "swap.a:Fast" {
		t.Errorf("got %+v", got)
	}
}

// A reference to a backend that doesn't exist, or isn't llama-swap's, fails;
// llama-swap models the named backend doesn't list only warn, since that
// depends on both being up and current.
func TestLlamaSwapBlock_ChecksItsReference(t *testing.T) {
	_, ls := newSwap(t, "", testkit.SwapModel{ID: "a"})
	f := testkit.NewOpenWebUI(t, testkit.SwapEntry("swap.a", "loaded"))
	cases := []struct {
		name, extra, want string
		fatal             bool
	}{
		{"wrong prefix", ", llamaSwap: {backend: ls, prefixId: other}", `'swap.a' is a llama-swap model`, false},
		{"no prefix", ", llamaSwap: {backend: ls}", `'swap.a' is not listed`, false},
		{"undefined backend", ", llamaSwap: {backend: nope}", `'nope' is not defined`, true},
		{"not a llama-swap backend", ", llamaSwap: {backend: other}", "is not a llamaSwap backend", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBackend(t, f, c.extra)
			warned, err := link(t, b, map[string]backend.Backend{"ls": ls, "other": b})
			got := strings.Join(warned, "; ")
			if c.fatal && err != nil {
				got = err.Error()
			}
			if (err != nil) != c.fatal || !strings.Contains(got, c.want) {
				t.Errorf("got error %v, warnings %q; want %q (fatal %v)", err, warned, c.want, c.fatal)
			}
		})
	}
}

// If the named backend can't be read, llama-swap models fall back to their
// `status`, as without the block; going down and coming back are each
// logged once.
func TestLlamaSwapBlock_FallsBackToStatusWhileLlamaSwapIsDown(t *testing.T) {
	swap, ls := newSwap(t, "", testkit.SwapModel{ID: "a"}, testkit.SwapModel{ID: "b"})
	swap.SetState("a", "starting")
	swap.FailState(true)
	f := testkit.NewOpenWebUI(t, testkit.SwapEntry("a", "loaded"), testkit.SwapEntry("b", "loaded"))
	var logs bytes.Buffer
	b := newBackendLogging(t, f, ", llamaSwap: {backend: ls}", slog.New(slog.NewTextHandler(&logs, nil)))
	if err := b.Link(map[string]backend.Backend{"ls": ls}, func(backend.ListingCheck) {}); err != nil {
		t.Fatal(err)
	}
	if got, err := b.LoadedModels(context.Background()); err != nil || len(got) != 2 || got[0].Starting || got[1].Starting {
		t.Errorf("down: got %+v, %v", got, err)
	}
	swap.FailState(false)
	var got []backend.ModelInfo
	for range 2 {
		got, _ = b.LoadedModels(context.Background())
	}
	if len(got) != 1 || got[0].ID != "a" || !got[0].Starting {
		t.Errorf("up: got %+v", got)
	}
	if out := logs.String(); strings.Count(out, "using llama-swap's status as Open WebUI reports it") != 1 || strings.Count(out, "answers again") != 1 {
		t.Errorf("log:\n%s", out)
	}
}

// A gateway behind Open WebUI can read the llamaSwap backend's state while
// answering the adapter's /api/models read, as a request of its own that
// arrives after the outer one. The outer request reuses that state instead
// of asking llama-swap again, even with stateCacheSeconds 0.
func TestLlamaSwapBlock_ReusesAReadMadeWhileOpenWebUIAnswered(t *testing.T) {
	swap, ls := newSwap(t, "", testkit.SwapModel{ID: "a"})
	swap.SetState("a", "ready")
	f := testkit.NewOpenWebUI(t, testkit.SwapEntry("a", "loaded"))
	b := newBackend(t, f, ", llamaSwap: {backend: ls}")
	if _, err := link(t, b, map[string]backend.Backend{"ls": ls}); err != nil {
		t.Fatal(err)
	}
	f.OnModels(func() { ls.LoadedModels(backend.WithRequestStart(context.Background(), time.Now())) })
	swap.Reset()
	got, err := b.LoadedModels(backend.WithRequestStart(context.Background(), time.Now()))
	if err != nil || len(got) != 1 || got[0].ID != "a" || got[0].Starting {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n := swap.Count("GET /running"); n != 1 {
		t.Errorf("llama-swap's /running was read %d times, want 1", n)
	}
}

// Each read applies the llamaSwap backend's state of that moment: a load
// started through that backend shows as starting, and its end as ready,
// while Open WebUI's own list stays cached.
func TestLlamaSwapBlock_AppliesLlamaSwapsStateOnEveryRead(t *testing.T) {
	swap, ls := newSwap(t, ", stateCacheSeconds: 60", testkit.SwapModel{ID: "a"})
	f := testkit.NewOpenWebUI(t, testkit.SwapEntry("a", "unloaded"))
	b := newBackend(t, f, ", stateCacheSeconds: 60, llamaSwap: {backend: ls}")
	if _, err := link(t, b, map[string]backend.Backend{"ls": ls}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(must(b.LoadedModels(context.Background()))); got["a"] != "" {
		t.Fatalf("before: %v", got)
	}
	f.Reset()
	swap.Reset()
	load := swap.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := ls.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a"}`)})
		done <- err
	}()
	swap.WaitFor(t, "POST /v1/chat/completions", 1)
	if got := stateOf(must(b.LoadedModels(context.Background()))); got["a"] != "starting" {
		t.Errorf("during a load through the llamaSwap backend: %v", got)
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := stateOf(must(b.LoadedModels(context.Background()))); got["a"] != "ready" {
		t.Errorf("after the load: %v", got)
	}
	if f.Count("GET /api/models") != 0 || swap.Count("GET /running") != 1 {
		t.Errorf("Open WebUI asked %d times (want 0), llama-swap's /running %d times (want 1)", f.Count("GET /api/models"), swap.Count("GET /running"))
	}
}

// A request through Open WebUI to a llama-swap preset counts as pending for
// its base model in the llamaSwap backend's cache, so both backends show it
// starting, even if Open WebUI doesn't list the base; its release drops that
// cache, not Open WebUI's.
func TestLlamaSwapBlock_CountsRequestsThroughOpenWebUIInLlamaSwap(t *testing.T) {
	swap, ls := newSwap(t, ", stateCacheSeconds: 60", testkit.SwapModel{ID: "a", Aliases: []string{"a:Fast"}})
	f := testkit.NewOpenWebUI(t, testkit.SwapPresetEntry("a:Fast", "a", "unloaded"))
	steps := make(chan struct{})
	f.SetChat(testkit.Chat{Contents: []string{"", "hi"}, PollGate: steps})
	b := newBackend(t, f, ", stateCacheSeconds: 60, llamaSwap: {backend: ls}")
	if _, err := link(t, b, map[string]backend.Backend{"ls": ls}); err != nil {
		t.Fatal(err)
	}
	both := func() (llamaSwap, openWebUI string) {
		return stateOf(must(ls.LoadedModels(context.Background())))["a"], stateOf(must(b.LoadedModels(context.Background())))["a:Fast"]
	}
	if s, w := both(); s != "" || w != "" {
		t.Fatalf("before: llama-swap %q, Open WebUI %q", s, w)
	}
	f.Reset()
	swap.Reset()
	done := make(chan error)
	go func() {
		_, err := chat(t, b, `{"model":"a:Fast","messages":[{"role":"user","content":"x"}]}`, false)
		done <- err
	}()
	steps <- struct{}{} // first poll: no output yet
	if s, w := both(); s != "starting" || w != "starting" {
		t.Errorf("during the run: llama-swap %q, Open WebUI %q", s, w)
	}
	swap.SetState("a", "ready")
	steps <- struct{}{} // second poll: output
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s, w := both(); s != "ready" || w != "ready" {
		t.Errorf("after the first output: llama-swap %q, Open WebUI %q", s, w)
	}
	if f.Count("GET /api/models") != 0 || swap.Count("GET /running") != 1 {
		t.Errorf("Open WebUI asked %d times (want 0), llama-swap's /running %d times (want 1)", f.Count("GET /api/models"), swap.Count("GET /running"))
	}
}

func must(models []backend.ModelInfo, err error) []backend.ModelInfo {
	if err != nil {
		panic(err)
	}
	return models
}
