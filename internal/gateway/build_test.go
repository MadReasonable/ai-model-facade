package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// A config that is wrong fails to build, whatever the backends say.
func TestBuild_RejectsAWrongConfig(t *testing.T) {
	cases := []struct{ name, from, to, want string }{
		{"undefined backend", "    backend: llamaSwap\n    description", "    backend: nope\n    description", `backend 'nope' is not defined`},
		{"undefined candidate backend", "model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b:q8_0\n        backend: nope", `backend 'nope' is not defined`},
		{"no backend at all", "    backend: llamaSwap\n    description", "    description", "backend is required, on the model or on the candidate"},
		{"unknown facade key", "    description:", "    descripton:", `unknown key 'descripton'`},
		{"unknown candidate key", "loadIfNone: true", "loadIfNoneed: true", `unknown key 'loadIfNoneed'`},
		{"loadIfNone on a pattern", "model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b:*", "needs an exact model ID"},
		{"unknown facade type", "    backend: llamaSwap\n    description", "    type: magic\n    backend: llamaSwap\n    description", `unknown type 'magic'`},
		{"unknown backend type", "type: llamaSwap,", "type: vllm,", `unknown type 'vllm'`},
		{"negative retry", `baseUrl: "${SWAP_URL}"`, `baseUrl: "${SWAP_URL}", listRetrySeconds: -1`, "listRetrySeconds must not be negative"},
	}
	swap := newSwap(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := edit(t, taskConfig, c.from, c.to)
			cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { return swap.URL, n == "SWAP_URL" })
			if err == nil {
				_, err = Build(context.Background(), cfg, discard)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// What depends on a backend being up and current only warns, so a backend
// that is down or not ready yet doesn't stop the gateway: an exact candidate
// it doesn't list (suggesting tagged IDs for an untagged one), or a backend
// that can't be listed.
func TestBuild_OnlyWarnsAboutWhatDependsOnABackend(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	cases := []struct{ name, from, to, want string }{
		{"unlisted ID", "model: qwen2.5-coder-3b:q8_0", "model: qwen9:q1", `'qwen9:q1' is not listed`},
		{"missing tag", "model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b", "did you mean qwen2.5-coder-3b:q8_0?"},
		{"missing tag, several", "model: qwen3.8-27b:*", "model: qwen3.8-27b", "did you mean one of qwen3.8-27b:Dreamer, qwen3.8-27b:q6_k?"},
		{"backend down", `baseUrl: "${SWAP_URL}"`, `baseUrl: "` + down.URL + `"`, "backends.llamaSwap: listing models"},
	}
	swap := newSwap(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := Build(context.Background(), parseConfig(t, edit(t, taskConfig, c.from, c.to), map[string]string{"SWAP_URL": swap.URL}), discard)
			if err != nil {
				t.Fatalf("got error %v, want a warning", err)
			}
			if !strings.Contains(fmt.Sprint(g.Warnings), c.want) {
				t.Errorf("warnings %v, want %q", g.Warnings, c.want)
			}
		})
	}
}

// Backends are listed once each and at the same time, so backends that hang
// delay startup by one stateTimeoutSeconds together; a backend that answers
// is still checked, and each one that is down is warned about once.
func TestBuild_ListsBackendsAtTheSameTime(t *testing.T) {
	var hung atomic.Int32
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		<-r.Context().Done()
	}))
	defer hang.Close()
	src := `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${HANG_URL}", stateTimeoutSeconds: 0.25}
  openWebUi: {type: openWebUi, baseUrl: "${HANG_URL}", stateTimeoutSeconds: 0.25, llamaSwap: {backend: llamaSwap}}
  healthy: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
models:
  task:
    backend: healthy
    candidates: [{model: not-a-model:q1}]
  tools:
    backend: openWebUi
    candidates: [{model: x}]
`
	start := time.Now()
	g, err := Build(context.Background(), parseConfig(t, src, map[string]string{"HANG_URL": hang.URL, "SWAP_URL": newSwap(t).URL}), discard)
	if err != nil {
		t.Fatal(err)
	}
	// One after the other, the two hung backends would take at least 0.5 s.
	if elapsed := time.Since(start); elapsed > 450*time.Millisecond {
		t.Errorf("took %v", elapsed)
	}
	warnings := fmt.Sprint(g.Warnings)
	if len(g.Warnings) != 3 || strings.Count(warnings, "listing models") != 2 || !strings.Contains(warnings, "'not-a-model:q1' is not listed by backend 'healthy'") {
		t.Errorf("warnings: %v", g.Warnings)
	}
	if hung.Load() != 2 || len(g.pending) != 2 {
		t.Errorf("%d requests to the hung backends, %d pending checks", hung.Load(), len(g.pending))
	}
}

// Checks against a backend that was down at startup run once, when it
// answers; meanwhile check says a pattern's matches are unknown rather than
// listing none.
func TestBuild_RunsPendingChecksOnceTheBackendAnswers(t *testing.T) {
	swap := newSwap(t)
	swap.SetDown(true)
	src := edit(t, taskConfig,
		"model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1",
		`baseUrl: "${SWAP_URL}"`, `baseUrl: "${SWAP_URL}", listRetrySeconds: 0.01`)
	gw := startGateway(t, src, map[string]string{"SWAP_URL": swap.URL})
	if len(gw.g.Warnings) != 1 || !strings.Contains(gw.g.Warnings[0].Error(), "listing models") || len(gw.g.pending) != 3 {
		t.Fatalf("warnings %v, %d pending checks", gw.g.Warnings, len(gw.g.pending))
	}
	if desc := describe(gw); !strings.Contains(desc, "qwen3.8-27b:*\n     base models and aliases: unknown; backend 'llamaSwap' could not be listed\n") || strings.Contains(desc, "aliases: none") {
		t.Errorf("describe:\n%s", desc)
	}
	// runPending, which start runs in the background, returns once every
	// check has run, so the log is complete when it does.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { gw.g.runPending(ctx, slog.New(slog.NewTextHandler(gw.logs, nil))); close(done) }()
	swap.SetDown(false)
	select {
	case <-done:
	case <-time.After(testkit.Timeout):
		t.Fatalf("the checks never ran:\n%s", gw.logs.String())
	}
	const want = "'not-a-model:q1' is not listed by backend 'llamaSwap'"
	if out := gw.logs.String(); strings.Count(out, want) != 1 || strings.Count(out, "backends.llamaSwap answers") != 1 {
		t.Errorf("log:\n%s", out)
	}
}
