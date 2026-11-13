package gateway

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

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
	"localAi":     {"LOCALAI_PORT", "18086"},
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

// checkTokenLimit asks facade for a long answer with this token limit, and fails
// t unless the backend stopped there, whole and streamed.
func (gw *liveGateway) checkTokenLimit(t *testing.T, facade, limit string, want int64) {
	t.Helper()
	question := `"messages":[{"role":"user","content":"Write every whole number from 1 to 500 in order, separated by spaces."}]`
	r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"`+facade+`",`+limit+`,`+question+`}`)
	if got := gjson.Get(r.body, "usage.completion_tokens").Int(); r.StatusCode != http.StatusOK || got != want {
		t.Errorf("%s, %s: got %d tokens, want %d: %d %.200s", facade, limit, got, want, r.StatusCode, r.body)
	}
	r = gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"`+facade+`",`+limit+`,"stream":true,"stream_options":{"include_usage":true},`+question+`}`)
	var got int64
	for _, line := range strings.Split(r.body, "\n") {
		if usage := gjson.Get(strings.TrimPrefix(line, "data: "), "usage.completion_tokens"); usage.Exists() {
			got = usage.Int()
		}
	}
	if r.StatusCode != http.StatusOK || got != want {
		t.Errorf("%s, %s, streamed: got %d tokens, want %d: %d", facade, limit, got, want, r.StatusCode)
	}
}

// llama-swap and llama-server count through their own endpoint, and it matches
// what the same request then reports, so the count is not an estimate.
func TestLive_MessagesCountMatchesOnLlamaBackends(t *testing.T) {
	gw := startDevGateway(t, `
  countSwap: {backend: llamaSwap, candidates: [{model: llamaSwap-llm-1, loadIfNone: true}]}
  countServer: {backend: llamaServer, candidates: [{model: llamaServer-llm-1, loadIfNone: true}]}
`, "llamaSwap", "llamaServer")
	for _, facade := range []string{"countSwap", "countServer"} {
		body := `{"model":"` + facade + `","system":"You are brief.","messages":[{"role":"user","content":"What is the weather in Paris?"}]}`
		r := messagesRequest(t, gw.gateway, "/anthropic/v1/messages/count_tokens", "", body, nil)
		count := gjson.Get(r.body, "input_tokens").Int()
		if r.StatusCode != http.StatusOK || count == 0 || r.Header.Get("X-Gateway-Estimated") != "" {
			t.Fatalf("%s count: got %d %s", facade, r.StatusCode, r.body)
		}
		real := messagesRequest(t, gw.gateway, "/anthropic/v1/messages", "", strings.Replace(body, `"system"`, `"max_tokens":1,"system"`, 1), nil)
		if got := gjson.Get(real.body, "usage.input_tokens").Int() + gjson.Get(real.body, "usage.cache_read_input_tokens").Int(); got != count {
			t.Errorf("%s: counted %d, the request used %d: %s", facade, count, got, real.body)
		}
	}
}

// Ollama ignores max_completion_tokens, so a client that uses that name gets
// no limit unless the adapter renames it, directly or through Open WebUI. With
// both names, max_completion_tokens' value is the limit.
func TestLive_TokenLimitReachesOllama(t *testing.T) {
	gw := startDevGateway(t, `
  limitedOllama: {backend: ollama, candidates: [{model: "ollama-llm-1:latest", loadIfNone: true}]}
  limitedWebUi: {backend: openWebUi, candidates: [{model: "ollama-llm-1:latest", loadIfNone: true}]}
`, "ollama", "openWebUi")
	for _, facade := range []string{"limitedOllama", "limitedWebUi"} {
		gw.checkTokenLimit(t, facade, `"max_completion_tokens":5`, 5)
		gw.checkTokenLimit(t, facade, `"max_tokens":5`, 5)
		gw.checkTokenLimit(t, facade, `"max_tokens":40,"max_completion_tokens":5`, 5)
	}
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
		{"localai", "localAi", nil, "localai-llm-1", "localai-llm-2"},
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

// While LocalAI, with its limit on loaded models, loads a model for one
// of the gateway's requests, the gateway sends it nothing, since any request
// for a model that isn't loaded, the loading one included, would evict it:
// facades on it find it busy, and the request that started the load is
// answered. LocalAI shows no load under way, so the check gives the first
// request 300 ms to start its load, which takes seconds.
func TestLive_LocalAILoadIsNotEvicted(t *testing.T) {
	gw := startDevGateway(t, `
  localai-3:
    backend: localAi
    candidates:
      - model: localai-llm-3
        loadIfNone: true
`, "localAi")
	b := gw.backends["localAi"]
	b.Reset(t)
	first := gw.askInBackground(t, "localai")
	time.Sleep(300 * time.Millisecond)
	for _, facade := range []string{"localai-3", "localai"} {
		if r := gw.ask(t, facade); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(r.body, "no_candidate_loaded") {
			t.Errorf("%s during the load: %d, target %q: %s", facade, r.StatusCode, r.target(), r.body)
		}
	}
	<-first.done
	if r := first.resp; r.Response == nil || r.StatusCode != http.StatusOK || r.target() != "localai-llm-2" {
		t.Errorf("the request that started the load: %+v", r)
	}
}

// LocalAI counts a request's text with the model's tokenizer, and the
// gateway estimates the chat template around it, so the count is marked as
// an estimate and comes within a few tokens of what the same request then
// reports: the joins between its texts, which the template has in other
// forms.
func TestLive_MessagesCountOnLocalAI(t *testing.T) {
	gw := startDevGateway(t, `
  countLocal: {backend: localAi, candidates: [{model: localai-llm-1, loadIfNone: true}]}
`, "localAi")
	for _, body := range []string{
		`{"model":"countLocal","system":"You are brief.","messages":[{"role":"user","content":"What is the weather in Paris?"}]}`,
		`{"model":"countLocal","system":"You are brief.","messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello! How can I help?"},{"role":"user","content":"Name three rivers in Europe, with their lengths."}]}`,
	} {
		r := messagesRequest(t, gw.gateway, "/anthropic/v1/messages/count_tokens", "", body, nil)
		count := gjson.Get(r.body, "input_tokens").Int()
		if r.StatusCode != http.StatusOK || count == 0 || r.Header.Get("X-Gateway-Estimated") != "true" {
			t.Fatalf("count: got %d %s", r.StatusCode, r.body)
		}
		real := messagesRequest(t, gw.gateway, "/anthropic/v1/messages", "", strings.Replace(body, `"system"`, `"max_tokens":1,"system"`, 1), nil)
		used := gjson.Get(real.body, "usage.input_tokens").Int()
		t.Logf("counted %d, the request used %d", count, used)
		if used == 0 || count < used-3 || count > used+3 {
			t.Errorf("counted %d, the request used %d: %s", count, used, real.body)
		}
	}
}

// A Responses request for a local model is translated to Chat Completions and
// back, whole or streamed, and its reasoning comes back as a reasoning item
// with the model's own text, through a function tool loop.
func TestLive_ResponsesOnALocalModel(t *testing.T) {
	gw := startDevGateway(t, `
  respondSwap: {backend: llamaSwap, candidates: [{model: llamaSwap-llm-1, loadIfNone: true}]}
`, "llamaSwap")
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		t.Run(name, func(t *testing.T) {
			if reasoning := liveFunctionLoop(t, gw.gateway, "respondSwap", stream); reasoning.Get("content.0.type").String() != "reasoning_text" {
				t.Errorf("reasoning item %s", reasoning.Raw)
			}
		})
	}
}

// The tools a client runs to a vendor's schema reach a local model as
// functions, and its calls come back as the client's own tools': OpenAI's
// shell to a Responses client, and Claude's bash to a Messages client.
func TestLive_ClientToolsReachALocalModel(t *testing.T) {
	gw := startDevGateway(t, `
  toolServer: {backend: llamaServer, candidates: [{model: llamaServer-llm-1, loadIfNone: true}]}
`, "llamaServer")
	question := "Use the shell to list the files in the current directory."
	response := liveRespond(t, gw.gateway, `{"model":"toolServer","max_output_tokens":1500,"tool_choice":"required","tools":[{"type":"shell"}],"input":`+quoted(question)+`}`, false)
	if call := gjson.Get(response, `output.#(type=="shell_call")`); len(call.Get("action.commands").Array()) == 0 {
		t.Errorf("Responses: %s", response)
	}
	r := messagesRequest(t, gw.gateway, liveMessagesPath, "", `{"model":"toolServer","max_tokens":1500,"tool_choice":{"type":"any"},"tools":[{"type":"bash_20250124","name":"bash"}],"messages":`+userTurn(question)+`}`, nil)
	if use := gjson.Get(r.body, `content.#(type=="tool_use")`); r.StatusCode != http.StatusOK || use.Get("name").String() != "bash" || use.Get("input.command").String() == "" {
		t.Errorf("Messages: got %d %s", r.StatusCode, r.body)
	}
}
