package gateway

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// The live checks serve dev-env/gateway.yaml in-process against the real
// backends that dev-env/ runs, and put those backends into each check's
// state through dev-env's control service (testkit.LiveBackend); without
// AMF_LIVE they skip. Run them with dev-env/ up:
//
//	AMF_LIVE=http://127.0.0.1:18090 go test ./internal/gateway/ -run Live

// devPorts is each of dev-env's backends' host port setting and its
// default, from dev-env/.env.example, as gateway.yaml's baseUrls use them.
var devPorts = map[string][2]string{
	"llamaRouter": {"LLAMA_SERVER_ROUTER_PORT", "18080"},
	"llamaServer": {"LLAMA_SERVER_SINGLE_PORT", "18081"},
	"llamaSwap":   {"LLAMA_SWAP_PORT", "18082"},
	"ollama":      {"OLLAMA_PORT", "18083"},
	"openWebUi":   {"OPEN_WEBUI_PORT", "18084"},
	"lmStudio":    {"LM_STUDIO_PORT", "18085"},
}

// liveGateway is dev-env/gateway.yaml served in-process, with the backends
// a check started.
type liveGateway struct {
	*gateway
	backends map[string]*testkit.LiveBackend
}

// startDevGateway starts the named backends through control, then serves
// dev-env/gateway.yaml with these facades added to its models, its baseUrls
// pointing at the started backends. The backends it doesn't start are
// still configured, as gateway.yaml lists them, and only warned about if
// they don't run.
func startDevGateway(t *testing.T, facades string, names ...string) *liveGateway {
	t.Helper()
	src, err := os.ReadFile("../../dev-env/gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config := string(src) + facades
	backends := map[string]*testkit.LiveBackend{}
	vars := map[string]string{"OPEN_WEBUI_API_KEY": "unused"}
	for _, name := range names {
		port := devPorts[name]
		b := testkit.StartLive(t, name, port[0], port[1])
		backends[name] = b
		config = edit(t, config, "http://127.0.0.1:"+port[1]+"\n", b.BaseURL+"\n")
		if name == "openWebUi" {
			vars["OPEN_WEBUI_API_KEY"] = b.Key
		}
	}
	return &liveGateway{gateway: startGateway(t, config, vars), backends: backends}
}

const liveChat = `{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`

// ask sends a one-token chat to facade.
func (gw *liveGateway) ask(t *testing.T, facade string) response {
	t.Helper()
	return gw.do(t, http.MethodPost, "/v1/chat/completions", "", fmt.Sprintf(liveChat, facade))
}

// backgroundAsk is a chat sent in the background. Once done is closed, resp
// is its answer, with a nil Response if it got none.
type backgroundAsk struct {
	done chan struct{}
	resp response
}

// askInBackground sends a one-token chat to facade without waiting for it.
func (gw *liveGateway) askInBackground(t *testing.T, facade string) *backgroundAsk {
	ask := &backgroundAsk{done: make(chan struct{})}
	go func() {
		defer close(ask.done)
		resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(fmt.Sprintf(liveChat, facade)))
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		ask.resp = response{resp, string(body)}
	}()
	return ask
}

// A facade on one backend uses its first candidate when that is loaded, and
// loads its loadIfNone fallback when nothing is, on each kind of backend;
// webui does it through Open WebUI's connection to Ollama.
func TestLive_UsesTheLoadedCandidateElseLoadsTheFallback(t *testing.T) {
	cases := []struct {
		facade, backend string
		through         []string // backends the facade reaches its models through
		first, fallback string
	}{
		{"swap", "llamaSwap", nil, "llamaSwap-llm-1", "llamaSwap-llm-2"},
		{"router", "llamaRouter", nil, "llamaRouter-llm-1", "llamaRouter-llm-2"},
		{"lmstudio", "lmStudio", nil, "lmstudio-llm-1", "lmstudio-llm-2"},
		{"ollama", "ollama", nil, "ollama-llm-1:latest", "ollama-llm-2:latest"},
		{"webui", "ollama", []string{"openWebUi"}, "ollama-llm-1:latest", "ollama-llm-2:latest"},
	}
	for _, c := range cases {
		t.Run(c.facade, func(t *testing.T) {
			gw := startDevGateway(t, "", append([]string{c.backend}, c.through...)...)
			b := gw.backends[c.backend]
			b.Reset(t)
			b.Load(t, c.first)
			if r := gw.ask(t, c.facade); r.StatusCode != http.StatusOK || r.target() != c.first {
				t.Errorf("with %s loaded: %d, target %q: %s", c.first, r.StatusCode, r.target(), r.body)
			}
			b.Reset(t)
			if r := gw.ask(t, c.facade); r.StatusCode != http.StatusOK || r.target() != c.fallback {
				t.Errorf("with nothing loaded: %d, target %q: %s", r.StatusCode, r.target(), r.body)
			}
			if !b.Loaded(t, c.fallback) {
				t.Errorf("%s not loaded after the fallback was used", c.fallback)
			}
		})
	}
}

// The single-model llama-server holds its one model while it runs.
func TestLive_SingleModelServerAnswersWithItsModel(t *testing.T) {
	gw := startDevGateway(t, "", "llamaServer")
	if r := gw.ask(t, "single"); r.StatusCode != http.StatusOK || r.target() != "llamaServer-llm-1" {
		t.Errorf("%d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
}

// A facade across backends uses llama-swap's model when it is loaded, and
// otherwise loads its fallback on Ollama.
func TestLive_SpansBackends(t *testing.T) {
	gw := startDevGateway(t, "", "llamaSwap", "ollama")
	swap, ollama := gw.backends["llamaSwap"], gw.backends["ollama"]
	swap.Reset(t)
	ollama.Reset(t)
	swap.Load(t, "llamaSwap-llm-1")
	if r := gw.ask(t, "either"); r.StatusCode != http.StatusOK || r.target() != "llamaSwap-llm-1" {
		t.Errorf("with llama-swap's loaded: %d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
	swap.Reset(t)
	if r := gw.ask(t, "either"); r.StatusCode != http.StatusOK || r.target() != "ollama-llm-1:latest" {
		t.Errorf("with nothing loaded: %d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
	if !ollama.Loaded(t, "ollama-llm-1:latest") {
		t.Error("Ollama's fallback not loaded after it was used")
	}
}

// An Open WebUI workspace model is forwarded under its own ID, and a
// request for it loads its base model on Ollama.
func TestLive_WorkspaceModelLoadsItsBase(t *testing.T) {
	gw := startDevGateway(t, "", "ollama", "openWebUi")
	ollama := gw.backends["ollama"]
	ollama.Reset(t)
	if r := gw.ask(t, "webui-alias"); r.StatusCode != http.StatusOK || r.target() != "openWebUi-llm-1:NoThink" {
		t.Errorf("%d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
	if !ollama.Loaded(t, "ollama-llm-1:latest") {
		t.Error("the workspace model's base not loaded after a request for it")
	}
}

// While LM Studio loads a model for one of the gateway's requests, the
// gateway sends it no request for another model, which would abort the
// load: a facade with only another model there finds no candidate loaded,
// and one with the loading model among its candidates uses it, waiting for
// the load.
func TestLive_LMStudioLoadIsNotAborted(t *testing.T) {
	gw := startDevGateway(t, `
  lmstudio-3:
    backend: lmStudio
    candidates:
      - model: lmstudio-llm-3
        loadIfNone: true
`, "lmStudio")
	b := gw.backends["lmStudio"]
	b.Reset(t)
	first := gw.askInBackground(t, "lmstudio")
	// LM Studio lists the model it is loading from the moment the load
	// begins, so the load is under way once it is listed.
	deadline := time.Now().Add(testkit.LiveTimeout)
	for !b.Loaded(t, "lmstudio-llm-2") {
		select {
		case <-first.done:
			t.Fatal("the load ended before LM Studio listed it")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("lmstudio-llm-2 not listed within %v", testkit.LiveTimeout)
		}
	}
	if r := gw.ask(t, "lmstudio-3"); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(r.body, "no_candidate_loaded") {
		t.Errorf("another model during the load: %d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
	if r := gw.ask(t, "lmstudio"); r.StatusCode != http.StatusOK || r.target() != "lmstudio-llm-2" {
		t.Errorf("the loading model during the load: %d, target %q: %s", r.StatusCode, r.target(), r.body)
	}
	<-first.done
	if r := first.resp; r.Response == nil || r.StatusCode != http.StatusOK || r.target() != "lmstudio-llm-2" {
		t.Errorf("the request that started the load: %+v", r)
	}
}
