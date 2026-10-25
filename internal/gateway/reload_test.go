package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// startLive serves a reloadable gateway built from the task config on swap.
func startLive(t *testing.T, swapURL string) (*Live, *gateway) {
	t.Helper()
	g, err := Build(context.Background(), parseConfig(t, taskConfig, map[string]string{"SWAP_URL": swapURL}), discard)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(g, discard)
	srv := httptest.NewServer(live)
	t.Cleanup(srv.Close)
	return live, &gateway{Server: srv, g: g}
}

func facadeIDs(t *testing.T, gw *gateway) string {
	t.Helper()
	return strings.Trim(gjson.Get(gw.get(t, "/v1/models").body, "data.#.id").Raw, "[]")
}

// A reload swaps in a valid config, keeps the running one if the new one is
// wrong, accepts warnings, and refuses to move server.listen.
func TestReload_SwapsInOnlyAValidConfig(t *testing.T) {
	swap := newSwap(t)
	live, gw := startLive(t, swap.URL)
	vars := map[string]string{"SWAP_URL": swap.URL}
	renamed := edit(t, taskConfig, "  task:\n", "  renamed:\n")
	if _, err := live.Reload(context.Background(), parseConfig(t, renamed, vars)); err != nil || facadeIDs(t, gw) != `"renamed"` {
		t.Errorf("valid: %v, facades %s", err, facadeIDs(t, gw))
	}
	bad := edit(t, renamed, "loadIfNone: true", "loadIfNoneed: true")
	if _, err := live.Reload(context.Background(), parseConfig(t, bad, vars)); err == nil || !strings.Contains(err.Error(), "loadIfNoneed") || facadeIDs(t, gw) != `"renamed"` {
		t.Errorf("wrong: %v, facades %s", err, facadeIDs(t, gw))
	}
	unlisted := edit(t, taskConfig, "model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1")
	g, err := live.Reload(context.Background(), parseConfig(t, unlisted, vars))
	if err != nil || len(g.Warnings) != 1 || !strings.Contains(g.Warnings[0].Error(), "not-a-model") || facadeIDs(t, gw) != `"task"` {
		t.Errorf("only warnings: %v, warnings %v, facades %s", err, g, facadeIDs(t, gw))
	}
	moved := edit(t, renamed, "listen: :0", "listen: :9999")
	if _, err := live.Reload(context.Background(), parseConfig(t, moved, vars)); err == nil || !strings.Contains(err.Error(), "restart") || live.Gateway().Config.Server.Listen != ":0" {
		t.Errorf("listen change: %v, listen %s", err, live.Gateway().Config.Server.Listen)
	}
}

// A request in flight during a reload finishes on the gateway that accepted
// it; the next request uses the new config.
func TestReload_LetsARequestInFlightFinishOnTheOldConfig(t *testing.T) {
	swap := newSwap(t)
	load := swap.HoldLoads()
	live, gw := startLive(t, swap.URL)
	done := make(chan response)
	go func() { done <- gw.chat(t, `{"model":"task","messages":[]}`) }()
	swap.WaitFor(t, "POST /v1/chat/completions", 1)
	renamed := edit(t, taskConfig, "  task:\n", "  renamed:\n")
	if _, err := live.Reload(context.Background(), parseConfig(t, renamed, map[string]string{"SWAP_URL": swap.URL})); err != nil {
		t.Fatal(err)
	}
	load.Open()
	if r := <-done; r.StatusCode != http.StatusOK || r.target() != "qwen2.5-coder-3b:q8_0" {
		t.Errorf("in flight: got %d %q %s", r.StatusCode, r.target(), r.body)
	}
	if r := gw.chat(t, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("after the reload, the old facade: got %d", r.StatusCode)
	}
}
