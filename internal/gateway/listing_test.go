package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// currentOn turns on `current` in the OpenAI family's model objects.
const currentOn = "api: {openAi: {addCurrent: true}}\n"

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
			gw := startGateway(t, currentOn+edit(t, taskConfig, c.edits...), map[string]string{"SWAP_URL": swap.URL})
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
	gw := startGateway(t, currentOn+taskConfig, map[string]string{"SWAP_URL": swap.URL})
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
	gw := startGateway(t, currentOn+taskConfig+"  other:\n    backend: llamaSwap\n    candidates: [{model: qwen3.8-27b:q6_k}]\n", map[string]string{"SWAP_URL": swap.URL})
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
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}", loopsBack: true, llamaSwap: {backend: llamaSwap}}
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
	gw := startGateway(t, currentOn+loopConfig, map[string]string{"SWAP_URL": swap.URL, "WEBUI_URL": webui.URL})
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
// waiting for it. Leaving `current` out there is the loop broken as
// designed, so it is logged at INFO, not as a warning.
func TestListing_DoesNotWaitForItselfThroughOpenWebUI(t *testing.T) {
	webui := testkit.NewOpenWebUI(t, testkit.OllamaEntry("qwen3:1.7b", true))
	gw := startGateway(t, currentOn+`
server: {listen: ":0"}
backends:
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}", loopsBack: true}
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
	if out := gw.logs.String(); !strings.Contains(out, `level=INFO msg="current selection unavailable" model=assistant`) || strings.Contains(out, "level=WARN") {
		t.Errorf("log:\n%s", out)
	}
}

// With api.openAi.addCurrent off, the default, an OpenAI listing leaves `current`
// out and reads no backend's state, so it answers from the config alone.
func TestListing_CurrentIsOffByDefault(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap)
	if list := gw.get(t, "/v1/models").body; gjson.Get(list, "data.0.current").Exists() || gjson.Get(list, "data.0.id").String() != "task" {
		t.Errorf("list: %s", list)
	}
	if one := gw.get(t, "/v1/models/task").body; gjson.Get(one, "current").Exists() {
		t.Errorf("one: %s", one)
	}
	if n := swap.Count("GET /running"); n != 0 {
		t.Errorf("/running read %d times, want 0", n)
	}
}

// /amf/models always reports `current` and every candidate, whatever
// api.openAi.addCurrent says, and reads each backend once however many facades
// use it.
func TestAmf_ListsFacadesWithCurrentAndCandidates(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.8-27b:q6_k", "ready")
	gw := startGateway(t, taskConfig+"  other:\n    backend: llamaSwap\n    candidates: [{model: qwen3.8-27b:q6_k}]\n", map[string]string{"SWAP_URL": swap.URL})
	swap.Reset()
	list := gw.get(t, "/amf/models").body
	if gjson.Get(list, "models.#.id").Raw != `["task","other"]` || gjson.Get(list, "models.#.current.model").Raw != `["qwen3.8-27b:q6_k","qwen3.8-27b:q6_k"]` || gjson.Get(list, "models.0.description").String() != "Background tasks" {
		t.Errorf("list: %s", list)
	}
	if got := gjson.Get(list, "models.#.candidates.#").Raw; got != `[3,1]` {
		t.Errorf("candidates per facade: %s, in %s", got, list)
	}
	if n := swap.Count("GET /running"); n != 1 {
		t.Errorf("/running read %d times for one listing, want 1", n)
	}
}

// /amf/models/{id} returns every candidate with its state, including those
// after the winner, which selection never reaches. It sends no request, so it
// never triggers a load.
func TestAmf_ReturnsEveryCandidate(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.8-27b:q6_k", "ready")
	gw := startTask(t, swap)
	one := gw.get(t, "/amf/models/task").body
	want := `[{"model":"qwen3.6-35b-a3b:q8_0","backend":"llamaSwap","state":"not loaded"},` +
		`{"model":"qwen3.8-27b:*","backend":"llamaSwap","state":"loaded","match":"qwen3.8-27b:q6_k","winner":true},` +
		`{"model":"qwen2.5-coder-3b:q8_0","backend":"llamaSwap","loadIfNone":true,"state":"not loaded"}]`
	if got := gjson.Get(one, "candidates").Raw; got != want {
		t.Errorf("candidates:\n got %s\nwant %s", got, want)
	}
	if got := gjson.Get(one, "current").Raw; got != `{"model":"qwen3.8-27b:q6_k","reason":"loaded"}` {
		t.Errorf("current: %s", got)
	}
	if n := swap.Count("POST /v1/chat/completions"); n != 0 {
		t.Errorf("sent %d chat requests, want 0", n)
	}
	if n := swap.Count("GET /running"); n != 1 {
		t.Errorf("/running read %d times, want 1", n)
	}
}

// A facade reads the same in the list and alone.
func TestAmf_ListEntryMatchesTheFacadeAlone(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.8-27b:q6_k", "ready")
	gw := startTask(t, swap)
	entry := gjson.Get(gw.get(t, "/amf/models").body, "models.0").Raw
	if alone := strings.TrimSpace(gw.get(t, "/amf/models/task").body); entry != alone {
		t.Errorf("list entry:\n %s\nalone:\n %s", entry, alone)
	}
}

// /amf is the gateway's own, called by clients of either API, so it takes the
// key from x-api-key as Claude's clients send it, or from Authorization; a
// wrong key in either is refused.
func TestAmf_TakesTheKeyFromEitherHeader(t *testing.T) {
	gw := startTask(t, newSwap(t))
	for name, header := range map[string]map[string]string{
		"x-api-key":     {"x-api-key": "secret"},
		"Authorization": {"Authorization": "Bearer secret"},
	} {
		if r := headerRequest(t, gw, http.MethodGet, "/amf/models", header); r.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d %s", name, r.StatusCode, r.body)
		}
	}
	if r := headerRequest(t, gw, http.MethodGet, "/amf/models", map[string]string{"x-api-key": "wrong"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong key: got %d", r.StatusCode)
	}
}

// headerRequest sends a request with these headers and no other key.
func headerRequest(t *testing.T, gw *gateway, method, path string, header map[string]string) response {
	t.Helper()
	req, _ := http.NewRequest(method, gw.URL+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{resp, string(body)}
}

// An alias candidate names its base model as aliasOf, in `current` and in the
// candidate alike.
func TestAmf_NamesAnAliasBaseModel(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap, "      - model: qwen3.6-35b-a3b:q8_0\n", "      - model: qwen3.6-35b:Slacker\n")
	one := gw.get(t, "/amf/models/task").body
	if got := gjson.Get(one, "current").Raw; got != `{"model":"qwen3.6-35b:Slacker","aliasOf":"qwen3.6-35b-a3b:q8_0","reason":"loaded"}` {
		t.Errorf("current: %s", got)
	}
	if got := gjson.Get(one, "candidates.0").Raw; got != `{"model":"qwen3.6-35b:Slacker","backend":"llamaSwap","state":"loaded","aliasOf":"qwen3.6-35b-a3b:q8_0","winner":true}` {
		t.Errorf("candidate: %s", got)
	}
}

// If nothing is loaded, the loadIfNone candidate is the winner though its
// state is not loaded, since that is where a request would go.
func TestAmf_MarksTheFallbackAsWinner(t *testing.T) {
	gw := startTask(t, newSwap(t))
	one := gw.get(t, "/amf/models/task").body
	if got := gjson.Get(one, "candidates.#(winner==true).model").String(); got != "qwen2.5-coder-3b:q8_0" {
		t.Errorf("winner %q: %s", got, one)
	}
	if got := gjson.Get(one, "current.reason").String(); got != "loadIfNone" {
		t.Errorf("current: %s", one)
	}
}

// If the backend's state can't be read, the facade is still returned with its
// candidates, marked unavailable, and without `current` or a winner.
func TestAmf_ReportsUnreadableState(t *testing.T) {
	swap := newSwap(t)
	gw := startTask(t, swap)
	swap.FailState(true)
	r := gw.get(t, "/amf/models/task")
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "current").Exists() || gjson.Get(r.body, "candidates.#(winner==true)").Exists() {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
	if got := gjson.Get(r.body, "candidates.#.state").Raw; got != `["unavailable","unavailable","unavailable"]` {
		t.Errorf("states: %s", got)
	}
}

// An unknown facade is a 404, and /amf needs the same key as the rest of the
// API.
func TestAmf_RefusesUnknownFacadesAndKeys(t *testing.T) {
	gw := startTask(t, newSwap(t))
	if r := gw.get(t, "/amf/models/nope"); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "model_not_found") {
		t.Errorf("unknown: got %d %s", r.StatusCode, r.body)
	}
	for _, path := range []string{"/amf/models", "/amf/models/task"} {
		if r := gw.do(t, http.MethodGet, path, "wrong", ""); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a wrong key: got %d", path, r.StatusCode)
		}
	}
}

// /amf/models may wait on a state read in flight, because the /v1/models call
// Open WebUI makes during that read never waits on it, with `current` on.
func TestAmf_DoesNotDeadlockThroughOpenWebUI(t *testing.T) {
	webui := testkit.NewOpenWebUI(t, testkit.OllamaEntry("qwen3:1.7b", true))
	gw := startGateway(t, currentOn+`
server: {listen: ":0"}
backends:
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}", loopsBack: true}
models:
  assistant:
    backend: webui
    candidates:
      - model: qwen3:1.7b
`, map[string]string{"WEBUI_URL": webui.URL})
	callsBack(t, webui, gw)
	webui.Reset()
	start := time.Now()
	list := gw.get(t, "/amf/models").body
	if time.Since(start) > time.Second || gjson.Get(list, "models.0.current").Raw != `{"model":"qwen3:1.7b","reason":"loaded"}` {
		t.Errorf("after %v: %s", time.Since(start), list)
	}
}
