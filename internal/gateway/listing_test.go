package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// /v1/models lists only the facades, with their descriptions; /v1/models/{id}
// returns one, or 404.
func TestListing_ListsOnlyFacades(t *testing.T) {
	gw := startTask(t, newSwap(t))
	list := gw.get(t, "/v1/models")
	if gjson.Get(list.body, "data.#").Int() != 1 || gjson.Get(list.body, "data.0.id").String() != "task" || gjson.Get(list.body, "data.0.description").String() != "Background tasks" {
		t.Errorf("list: %s", list.body)
	}
	if r := gw.get(t, "/v1/models/task"); r.StatusCode != http.StatusOK || gjson.Get(r.body, "id").String() != "task" || gjson.Get(r.body, "object").String() != "model" {
		t.Errorf("one: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.get(t, "/v1/models/nope"); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "model_not_found") {
		t.Errorf("unknown: got %d %s", r.StatusCode, r.body)
	}
}

// `current` says where a request would go now, in the list and for one
// model alike: a ready model, a starting one, the loadIfNone fallback, or
// nothing.
func TestListing_ReportsWhereARequestWouldGoNow(t *testing.T) {
	cases := []struct {
		name, ready, starting string
		edits                 []string
		want                  string
	}{
		{"ready", "qwen3.6-35b-a3b:q8_0", "", nil, `{"model":"qwen3.6-35b-a3b:q8_0","reason":"loaded"}`},
		{"starting", "", "qwen3.8-27b:q6_k", nil, `{"model":"qwen3.8-27b:q6_k","reason":"starting"}`},
		{"would load", "", "", nil, `{"model":"qwen2.5-coder-3b:q8_0","reason":"loadIfNone"}`},
		{"nothing", "", "", []string{"        loadIfNone: true\n", ""}, `{"reason":"no candidate loaded"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swap := newSwap(t)
			if c.ready != "" {
				swap.SetState(c.ready, "ready")
			}
			if c.starting != "" {
				swap.SetState(c.starting, "starting")
			}
			gw := startTask(t, swap, c.edits...)
			if got := gjson.Get(gw.get(t, "/v1/models").body, "data.0.current").Raw; got != c.want {
				t.Errorf("list: got %s, want %s", got, c.want)
			}
			if got := gjson.Get(gw.get(t, "/v1/models/task").body, "current").Raw; got != c.want {
				t.Errorf("one: got %s, want %s", got, c.want)
			}
		})
	}
}

// If the backend's state can't be read, the model is still listed, without
// `current`.
func TestListing_OmitsCurrentWithoutState(t *testing.T) {
	swap := newSwap(t)
	gw := startTask(t, swap)
	swap.FailState(true)
	if r := gw.get(t, "/v1/models/task"); r.StatusCode != http.StatusOK || gjson.Get(r.body, "current").Exists() || gjson.Get(r.body, "id").String() != "task" {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// A listing reads each backend's state once, however many facades use it,
// and judges every facade against that state.
func TestListing_ReadsEachBackendOnce(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.8-27b:q6_k", "ready")
	gw := startGateway(t, taskConfig+"  other:\n    backend: llamaSwap\n    candidates: [{model: qwen3.8-27b:q6_k}]\n", map[string]string{"SWAP_URL": swap.URL})
	swap.Reset()
	list := gw.get(t, "/v1/models").body
	if gjson.Get(list, "data.#.current.model").Raw != `["qwen3.8-27b:q6_k","qwen3.8-27b:q6_k"]` {
		t.Errorf("list: %s", list)
	}
	if n := swap.Count("GET /running"); n != 1 {
		t.Errorf("/running read %d times, want 1", n)
	}
}

// loopConfig has a facade on Open WebUI, whose llama-swap models take their
// state from the gateway's llamaSwap backend, and a facade on that backend.
const loopConfig = `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}", llamaSwap: {backend: llamaSwap}}
models:
  ask:
    backend: webui
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
  task:
    backend: llamaSwap
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
`

// callsBack makes webui list the gateway's models while it answers its own
// model list, the way Open WebUI refreshes a gateway connection.
func callsBack(t *testing.T, webui *testkit.OpenWebUI, gw *gateway) {
	webui.OnModels(func() {
		if resp, err := http.Get(gw.URL + "/v1/models"); err == nil {
			resp.Body.Close()
		}
	})
}

// One request reads a backend's state once, even through a loop: the listing
// reads Open WebUI for `ask`, Open WebUI lists this gateway's models, and that
// nested request reads llama-swap, which the outer request then reuses, with
// stateCacheSeconds 0.
func TestListing_ReadsEachBackendOnceThroughOpenWebUI(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	webui := testkit.NewOpenWebUI(t, testkit.SwapEntry("qwen3.6-35b-a3b:q8_0", "loaded"))
	gw := startGateway(t, loopConfig, map[string]string{"SWAP_URL": swap.URL, "WEBUI_URL": webui.URL})
	callsBack(t, webui, gw)
	swap.Reset()
	list := gw.get(t, "/v1/models").body
	if gjson.Get(list, "data.#.current.reason").Raw != `["loaded","loaded"]` {
		t.Errorf("list: %s", list)
	}
	if n := swap.Count("GET /running"); n != 1 {
		t.Errorf("llama-swap's /running read %d times for one listing, want 1", n)
	}
}

// When Open WebUI lists this gateway's models while the gateway is reading
// Open WebUI's state, the nested listing doesn't wait for the read that is
// waiting for it.
func TestListing_DoesNotWaitForItselfThroughOpenWebUI(t *testing.T) {
	webui := testkit.NewOpenWebUI(t, testkit.OllamaEntry("qwen3:1.7b", true))
	gw := startGateway(t, `
server: {listen: ":0"}
backends:
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}"}
models:
  assistant:
    backend: webui
    candidates:
      - model: qwen3:1.7b
`, map[string]string{"WEBUI_URL": webui.URL})
	callsBack(t, webui, gw)
	webui.Reset()
	start := time.Now()
	list := gw.get(t, "/v1/models").body
	if time.Since(start) > time.Second || webui.Count("GET /api/models") != 1 || gjson.Get(list, "data.0.current").Raw != `{"model":"qwen3:1.7b","reason":"loaded"}` {
		t.Errorf("after %v and %d model-list reads: %s", time.Since(start), webui.Count("GET /api/models"), list)
	}
}
