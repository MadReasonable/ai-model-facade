package gateway

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// A starting model ranks after every ready candidate but before the
// loadIfNone fallback, and the winner gets its own candidate's params.
func TestSelection_StartingRanksBetweenReadyAndFallback(t *testing.T) {
	cases := []struct {
		name         string
		ready, start string
		target, body string
	}{
		{"ready later in the list beats starting", "qwen2.5-coder-3b:q8_0", "qwen3.8-27b:q6_k",
			"qwen2.5-coder-3b:q8_0", `{"model":"qwen2.5-coder-3b:q8_0","messages":[],"max_tokens":1000}`},
		{"starting beats loadIfNone", "", "qwen3.8-27b:q6_k",
			"qwen3.8-27b:q6_k", `{"model":"qwen3.8-27b:q6_k","messages":[],"max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}`},
		{"nothing ready or starting: loadIfNone", "", "",
			"qwen2.5-coder-3b:q8_0", `{"model":"qwen2.5-coder-3b:q8_0","messages":[],"max_tokens":1000}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swap := newSwap(t)
			if c.ready != "" {
				swap.SetState(c.ready, "ready")
			}
			if c.start != "" {
				swap.SetState(c.start, "starting")
			}
			r := startTask(t, swap).chat(t, `{"model":"task","messages":[]}`)
			req, _ := swap.Last("POST /v1/chat/completions")
			if r.StatusCode != http.StatusOK || r.target() != c.target || req.Body != c.body {
				t.Errorf("got %d %q; forwarded %s", r.StatusCode, r.target(), req.Body)
			}
		})
	}
}

// An alias candidate is chosen when its base model is ready and forwarded
// under its own ID; the startup log notes that the backend's own params for
// it may override the facade's.
func TestSelection_ForwardsAnAliasUnderItsOwnID(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap, "      - model: qwen3.6-35b-a3b:q8_0\n", "      - model: qwen3.6-35b:Slacker\n")
	if !strings.Contains(gw.logs.String(), "candidate=qwen3.6-35b:Slacker aliasOf=qwen3.6-35b-a3b:q8_0") {
		t.Errorf("startup log: %s", gw.logs.String())
	}
	// check names the alias's base, and leaves out of a pattern's matches an
	// alias whose base matches too, since the base always wins.
	if desc := describe(gw); !strings.Contains(desc, "alias of qwen3.6-35b-a3b:q8_0\n     note:") || !strings.Contains(desc, "base models: qwen3.8-27b:q6_k\n     aliases: none\n") {
		t.Errorf("check:\n%s", desc)
	}
	r := gw.chat(t, `{"model":"task","messages":[]}`)
	req, _ := swap.Last("POST /v1/chat/completions")
	if r.target() != "qwen3.6-35b:Slacker" || req.Body != `{"model":"qwen3.6-35b:Slacker","messages":[],"max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("got %q; forwarded %s", r.target(), req.Body)
	}
}

// mixedConfig has a `task` facade with candidates on llama-swap and on
// Ollama, each with params of its own.
const mixedConfig = `
server: {listen: ":0"}
logging: {logSelection: true}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}
models:
  task:
    backend: llamaSwap
    params: {max_tokens: 1000}
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
      - model: qwen3:1.7b
        backend: ollama
        params: {think: false}
      - {model: qwen2.5-coder-3b:q8_0, loadIfNone: true}
`

// One facade can pick among candidates on different backends: the winner's
// backend gets the request with the same params flow, and the log and
// `current` name a winner on another backend than the facade's own.
func TestSelection_PicksAmongCandidatesOnSeveralBackends(t *testing.T) {
	swap := newSwap(t)
	ollama := testkit.NewOllama(t, "qwen3:1.7b")
	ollama.SetLoaded("qwen3:1.7b")
	gw := startGateway(t, mixedConfig, map[string]string{"SWAP_URL": swap.URL, "OLLAMA_URL": ollama.URL})
	swap.Reset()
	r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"task","messages":[]}`)
	req, _ := ollama.Last("POST /v1/chat/completions")
	if r.target() != "qwen3:1.7b" || req.Body != `{"model":"qwen3:1.7b","messages":[],"max_tokens":1000,"think":false}` || swap.Count("POST /v1/chat/completions") != 0 {
		t.Errorf("got %d %q; Ollama saw %s; llama-swap saw %v", r.StatusCode, r.target(), req.Body, swap.Keys())
	}
	if out := gw.logs.String(); !strings.Contains(out, `checked="[qwen3.6-35b-a3b:q8_0: not loaded qwen3:1.7b on ollama: loaded]"`) || !strings.Contains(out, "backend=ollama") {
		t.Errorf("log:\n%s", out)
	}
	if list := gw.do(t, http.MethodGet, "/v1/models", "", "").body; !strings.Contains(list, `"current":{"model":"qwen3:1.7b","backend":"ollama","reason":"loaded"}`) {
		t.Errorf("models: %s", list)
	}
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"task","messages":[]}`); r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("on the facade's own backend: got %q", r.target())
	}
}

// Candidates on several passthroughAuth backends get a startup warning,
// since a client's one key is sent to whichever of them wins.
func TestSelection_WarnsAboutSeveralPassthroughBackends(t *testing.T) {
	cases := []struct {
		name       string
		swap, olla string
		warned     bool
	}{
		{"none", "", "", false},
		{"one", ", passthroughAuth: true", "", false},
		{"both", ", passthroughAuth: true", ", passthroughAuth: true", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := edit(t, mixedConfig,
				`baseUrl: "${SWAP_URL}"}`, `baseUrl: "${SWAP_URL}"`+c.swap+`}`,
				`baseUrl: "${OLLAMA_URL}"}`, `baseUrl: "${OLLAMA_URL}"`+c.olla+`}`)
			gw := startGateway(t, src, map[string]string{"SWAP_URL": newSwap(t).URL, "OLLAMA_URL": testkit.NewOllama(t, "qwen3:1.7b").URL})
			if got := strings.Contains(gw.logs.String(), "several passthroughAuth backends"); got != c.warned {
				t.Errorf("warned %v, want %v:\n%s", got, c.warned, gw.logs.String())
			}
		})
	}
}

// waitingConfig has two facades on Ollama: `first` loads a:1, and `second`
// would load b:1 unless a:1 is ready or starting.
const waitingConfig = `
server: {listen: ":0"}
backends:
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}
models:
  first:
    backend: ollama
    candidates:
      - {model: "a:1", loadIfNone: true}
  second:
    backend: ollama
    candidates:
      - {model: "b:1", loadIfNone: true}
      - model: "a:1"
`

// While one facade's request loads a model, requests for another facade wait
// for that load instead of loading their own fallback, which could evict it;
// Ollama doesn't show the load, so only the gateway's pending count does.
// Several requests arriving during the load all wait for that one load.
func TestSelection_RequestsDuringALoadWaitForIt(t *testing.T) {
	ollama := testkit.NewOllama(t, "a:1", "b:1")
	load := ollama.HoldLoads()
	gw := startGateway(t, waitingConfig, map[string]string{"OLLAMA_URL": ollama.URL})
	targets := make(chan string, 9)
	var wg sync.WaitGroup
	send := func(model string) {
		wg.Go(func() {
			r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"`+model+`","messages":[]}`)
			targets <- r.target()
		})
	}
	send("first")
	ollama.WaitFor(t, "POST /v1/chat/completions", 1)
	for range 8 {
		send("second")
	}
	ollama.WaitFor(t, "POST /v1/chat/completions", 9)
	load.Open()
	wg.Wait()
	close(targets)
	for target := range targets {
		if target != "a:1" {
			t.Errorf("a request went to %q, want a:1", target)
		}
	}
	for _, r := range ollama.Requests() {
		if r.Key() == "POST /v1/chat/completions" && gjson.Get(r.Body, "model").String() != "a:1" {
			t.Errorf("Ollama was asked for %s", gjson.Get(r.Body, "model"))
		}
	}
}

// describe returns what check prints for the gateway's first facade.
func describe(gw *gateway) string {
	var desc bytes.Buffer
	gw.g.Models[0].Handler.Describe(&desc)
	return desc.String()
}
