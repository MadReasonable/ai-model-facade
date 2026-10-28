package testkit

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A claim is what a fake says about its backend, written once and run
// twice: against the fake, and, when AMF_LIVE names dev-env/'s control
// service, against the real backend that dev-env runs. The claim sends its
// requests straight to the driver's URL; the driver only says where the
// backend is and puts it into the state the claim needs. So the fakes can't
// drift from the backends they stand for without a live run saying so.
//
// Run the live half with dev-env/ up:
//
//	AMF_LIVE=http://127.0.0.1:18090 go test ./internal/testkit/ -run /live
//
// A backend's host port defaults to .env.example's and is overridden by the
// same variable, such as LLAMA_SWAP_PORT, as in dev-env/.env.

// Driver is one backend under a claim, fake or live.
type Driver interface {
	// URL is the backend's base URL.
	URL() string
	// Reset unloads every model.
	Reset(t *testing.T)
	// Load loads model, and returns once the backend reports it loaded.
	Load(t *testing.T, model string)
	// HoldLoads makes the loads started from now wait until release is
	// called, so a claim can look at a load under way. A real backend can't
	// be held: its loads take as long as they take, and release does
	// nothing.
	HoldLoads(t *testing.T) (release func())
}

// liveControl is the AMF_LIVE variable: control's URL, or "" for no live
// runs.
func liveControl() string { return os.Getenv("AMF_LIVE") }

// forEach runs claim against the fake that fake builds, then against the
// live backend, which control knows by name and the host reaches on port
// (portVar, else its default).
func forEach(t *testing.T, name, portVar, port string, fake func(t *testing.T) Driver, claim func(t *testing.T, b Driver)) {
	t.Run("fake", func(t *testing.T) { claim(t, fake(t)) })
	t.Run("live", func(t *testing.T) { claim(t, startLive(t, name, portVar, port)) })
}

// startLive starts the live backend that control knows by name and the host
// reaches on port (portVar, else its default), or skips t without AMF_LIVE.
func startLive(t *testing.T, name, portVar, port string) *liveDriver {
	t.Helper()
	control := liveControl()
	if control == "" {
		t.Skip("set AMF_LIVE to dev-env's control URL, such as http://127.0.0.1:18090, to check the real backend")
	}
	if p := os.Getenv(portVar); p != "" {
		port = p
	}
	b := &liveDriver{control: control, name: name, url: "http://127.0.0.1:" + port}
	b.start(t)
	return b
}

// waitLimit bounds a claim's waits: Timeout for the fakes, longer when a
// live run may wait for real loads.
func waitLimit() time.Duration {
	if liveControl() != "" {
		return 3 * time.Minute
	}
	return Timeout
}

// during checks cond while a load runs, until it holds or done closes. A
// held fake load makes cond hold at once; a real load must show it before it
// ends, so a claim about a load under way fails if the load was over first.
func during(t *testing.T, done <-chan struct{}, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitLimit())
	for !cond() {
		select {
		case <-done:
			if !cond() {
				t.Errorf("%s: never seen before the load ended", what)
			}
			return
		case <-deadline:
			t.Errorf("%s: not seen within %v", what, waitLimit())
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// backgroundChat is a chat sent in the background. Once done is closed,
// status and body are its answer, status 0 if it got none.
type backgroundChat struct {
	done   chan struct{}
	status int
	body   string
}

// chatInBackground posts a chat for model to the backend.
func chatInBackground(t *testing.T, b Driver, model string) *backgroundChat {
	chat := &backgroundChat{done: make(chan struct{})}
	go func() {
		defer close(chat.done)
		if resp := post(t, b.URL()+"/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`); resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			chat.status, chat.body = resp.StatusCode, string(body)
		}
	}()
	return chat
}

// liveDriver drives a real backend through control's API.
type liveDriver struct {
	control, name, url string
	key                string // the API key its setup made: Open WebUI's
}

func (b *liveDriver) URL() string                             { return b.url }
func (b *liveDriver) HoldLoads(t *testing.T) (release func()) { return func() {} }

// start starts the backend through control, unless it runs, and waits until
// control reports it ready with its setup done.
func (b *liveDriver) start(t *testing.T) {
	t.Helper()
	b.call(t, http.MethodPost, "/start", nil)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var status struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
			Setup  struct {
				Ready bool   `json:"ready"`
				Key   string `json:"key"`
			} `json:"setup"`
		}
		b.call(t, http.MethodGet, "", &status)
		switch {
		case status.State == "ready" && status.Setup.Ready:
			b.key = status.Setup.Key
			return
		case status.State == "failed":
			t.Fatalf("backend '%s' failed to start through control: %s", b.name, status.Detail)
		case time.Now().After(deadline):
			t.Fatalf("backend '%s' wasn't ready through control within 5 minutes: %s, %s", b.name, status.State, status.Detail)
		}
		time.Sleep(time.Second)
	}
}

// liveModel is a model as control's GET .../models reports it.
type liveModel struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// models returns the backend's models through control, or nil while control
// can't list them.
func (b *liveDriver) models(t *testing.T) []liveModel {
	t.Helper()
	resp, err := http.Get(b.control + "/api/backends/" + b.name + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var models []liveModel
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&models) != nil {
		return nil
	}
	return models
}

// Reset unloads, through control, every model the backend reports in any
// state but unloaded; control answers once the backend no longer reports it
// loaded.
func (b *liveDriver) Reset(t *testing.T) {
	t.Helper()
	for _, m := range b.models(t) {
		if m.State != "unloaded" {
			b.call(t, http.MethodPost, "/models/"+m.ID+"/unload", nil)
		}
	}
}

// Load loads model through control and waits until control reports it
// loaded.
func (b *liveDriver) Load(t *testing.T, model string) {
	t.Helper()
	b.call(t, http.MethodPost, "/models/"+model+"/load", nil)
	deadline := time.Now().Add(waitLimit())
	for time.Now().Before(deadline) {
		for _, m := range b.models(t) {
			if m.ID == model && (m.State == "loaded" || m.State == "ready") {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("model '%s' of '%s' wasn't loaded within %v", model, b.name, waitLimit())
}

// call sends a request to the backend's resource in control's API and
// decodes the answer into out, if out isn't nil.
func (b *liveDriver) call(t *testing.T, method, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(method, b.control+"/api/backends/"+b.name+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("control at %s: %v", b.control, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("control: %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatal(fmt.Errorf("control: %s %s: %w", method, path, err))
		}
	}
}

// llama-swap, as dev-env/ runs it: three models, the first with the
// NoThink preset (dev-env/backend-config/llama-swap.yaml).

var devLlamaSwap = []SwapModel{
	{ID: "llamaSwap-llm-1", Aliases: []string{"llamaSwap-llm-1:NoThink"}},
	{ID: "llamaSwap-llm-2"},
	{ID: "llamaSwap-llm-3"},
}

func forEachLlamaSwap(t *testing.T, claim func(t *testing.T, b Driver)) {
	forEach(t, "llamaSwap", "LLAMA_SWAP_PORT", "18082", func(t *testing.T) Driver {
		return &fakeLlamaSwap{NewLlamaSwap(t, devLlamaSwap...)}
	}, claim)
}

type fakeLlamaSwap struct{ *LlamaSwap }

func (f *fakeLlamaSwap) URL() string { return f.LlamaSwap.URL }

func (f *fakeLlamaSwap) Reset(t *testing.T) {
	for _, m := range devLlamaSwap {
		f.SetState(m.ID, "")
	}
}

func (f *fakeLlamaSwap) Load(t *testing.T, model string) { f.SetState(model, "ready") }

func (f *fakeLlamaSwap) HoldLoads(t *testing.T) (release func()) {
	return f.LlamaSwap.HoldLoads().Open
}

// Ollama, as dev-env/ runs it: four models (dev-env/config.yaml).

var devOllama = []string{"ollama-llm-1:latest", "ollama-llm-2:latest", "ollama-llm-3:latest", "ollama-llm-4:latest"}

func forEachOllama(t *testing.T, claim func(t *testing.T, b Driver)) {
	forEach(t, "ollama", "OLLAMA_PORT", "18083", func(t *testing.T) Driver {
		return &fakeOllama{NewOllama(t, devOllama...)}
	}, claim)
}

type fakeOllama struct{ *Ollama }

func (f *fakeOllama) URL() string        { return f.Ollama.URL }
func (f *fakeOllama) Reset(t *testing.T) { f.SetLoaded() }

func (f *fakeOllama) Load(t *testing.T, model string) { f.SetLoaded(append(f.Loaded(), model)...) }

func (f *fakeOllama) HoldLoads(t *testing.T) (release func()) {
	return f.Ollama.HoldLoads().Open
}

// llama-server, as dev-env/ runs it: three models in router mode, with no
// aliases (dev-env/backend-config/llama-server-models.ini), and one in
// single-model mode (dev-env/config.yaml).

var devLlamaRouter = []ServerModel{{ID: "llamaRouter-llm-1"}, {ID: "llamaRouter-llm-2"}, {ID: "llamaRouter-llm-3"}}

func forEachLlamaRouter(t *testing.T, claim func(t *testing.T, b Driver)) {
	forEach(t, "llamaRouter", "LLAMA_SERVER_ROUTER_PORT", "18080", func(t *testing.T) Driver {
		return &fakeLlamaServer{NewLlamaServerRouter(t, devLlamaRouter...)}
	}, claim)
}

func forEachLlamaServer(t *testing.T, claim func(t *testing.T, b Driver)) {
	forEach(t, "llamaServer", "LLAMA_SERVER_SINGLE_PORT", "18081", func(t *testing.T) Driver {
		return &fakeLlamaServer{NewLlamaServerSingle(t, ServerModel{ID: "llamaServer-llm-1"})}
	}, claim)
}

type fakeLlamaServer struct{ *LlamaServer }

func (f *fakeLlamaServer) URL() string { return f.LlamaServer.URL }

// Reset unloads the router's models; a single-model server holds its model
// for as long as it runs, so its claims don't reset it.
func (f *fakeLlamaServer) Reset(t *testing.T) {
	for _, m := range f.models {
		f.SetStatus(m.ID, "unloaded")
	}
}

func (f *fakeLlamaServer) Load(t *testing.T, model string) { f.SetStatus(model, "loaded") }

func (f *fakeLlamaServer) HoldLoads(t *testing.T) (release func()) {
	return f.LlamaServer.HoldLoads().Open
}

// LM Studio, as dev-env/ runs it: three models (dev-env/config.yaml).

var devLMStudio = []string{"lmstudio-llm-1", "lmstudio-llm-2", "lmstudio-llm-3"}

func forEachLMStudio(t *testing.T, claim func(t *testing.T, b Driver)) {
	forEach(t, "lmStudio", "LM_STUDIO_PORT", "18085", func(t *testing.T) Driver {
		return &fakeLMStudio{NewLMStudio(t, devLMStudio...)}
	}, claim)
}

type fakeLMStudio struct{ *LMStudio }

func (f *fakeLMStudio) URL() string                     { return f.LMStudio.URL }
func (f *fakeLMStudio) Reset(t *testing.T)              { f.SetLoaded("") }
func (f *fakeLMStudio) Load(t *testing.T, model string) { f.SetLoaded(model) }

func (f *fakeLMStudio) HoldLoads(t *testing.T) (release func()) {
	return f.LMStudio.HoldLoads().Open
}

// Open WebUI, as dev-env/ runs it: connected to llama-swap and Ollama, with
// one workspace model on Ollama's first (dev-env/config.yaml).

// OpenWebUIDriver is Open WebUI under a claim, fake or live, with the
// backends it connects to.
type OpenWebUIDriver interface {
	// Models returns Open WebUI's model list, refreshed from its
	// connections.
	Models(t *testing.T) string
	LlamaSwap() Driver
	Ollama() Driver
}

func forEachOpenWebUI(t *testing.T, claim func(t *testing.T, b OpenWebUIDriver)) {
	t.Run("fake", func(t *testing.T) {
		claim(t, &fakeOpenWebUI{
			OpenWebUI: NewOpenWebUI(t),
			swap:      &fakeLlamaSwap{NewLlamaSwap(t, devLlamaSwap...)},
			ollama:    &fakeOllama{NewOllama(t, devOllama...)},
		})
	})
	t.Run("live", func(t *testing.T) {
		claim(t, &liveOpenWebUI{
			liveDriver: startLive(t, "openWebUi", "OPEN_WEBUI_PORT", "18084"),
			swap:       startLive(t, "llamaSwap", "LLAMA_SWAP_PORT", "18082"),
			ollama:     startLive(t, "ollama", "OLLAMA_PORT", "18083"),
		})
	})
}

type liveOpenWebUI struct {
	*liveDriver
	swap, ollama *liveDriver
}

func (b *liveOpenWebUI) LlamaSwap() Driver { return b.swap }
func (b *liveOpenWebUI) Ollama() Driver    { return b.ollama }

func (b *liveOpenWebUI) Models(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, b.url+"/api/models?refresh=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Open WebUI's model list: %s %s", resp.Status, body)
	}
	return string(body)
}

// fakeOpenWebUI lists its connections' models as the entry builders build
// them, from the fake connections' states at each read.
type fakeOpenWebUI struct {
	*OpenWebUI
	swap   *fakeLlamaSwap
	ollama *fakeOllama
}

func (f *fakeOpenWebUI) LlamaSwap() Driver { return f.swap }
func (f *fakeOpenWebUI) Ollama() Driver    { return f.ollama }

func (f *fakeOpenWebUI) Models(t *testing.T) string {
	var entries []string
	for _, m := range devLlamaSwap {
		status := "unloaded"
		if f.swap.State(m.ID) == "ready" {
			status = "loaded"
		}
		entries = append(entries, SwapEntry(m.ID, status))
		for _, alias := range m.Aliases {
			entries = append(entries, SwapPresetEntry(alias, m.ID, status))
		}
	}
	ollama := map[string]string{}
	for _, m := range devOllama {
		ollama[m] = OllamaEntry(m, slices.Contains(f.ollama.Loaded(), m))
		entries = append(entries, ollama[m])
	}
	entries = append(entries, WorkspaceEntry("openWebUi-llm-1:NoThink", "ollama-llm-1:latest", ollama["ollama-llm-1:latest"]))
	f.SetModels(entries...)
	return get(t, f.URL+"/api/models?refresh=true")
}
