package testkit

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Error(err)
		return nil
	}
	return resp
}

// llama-swap lists aliases as their own entries pointing at their base, and
// a load shows as starting in /running until it finishes, then as ready;
// /running lists base models only, never aliases.
func TestLlamaSwap_ListsAliasesAndShowsLoads(t *testing.T) {
	forEachLlamaSwap(t, func(t *testing.T, b Driver) {
		models := get(t, b.URL()+"/v1/models")
		if gjson.Get(models, `data.#(id=="llamaSwap-llm-1").meta.llamaswap.aliases.0`).String() != "llamaSwap-llm-1:NoThink" || gjson.Get(models, `data.#(id=="llamaSwap-llm-1:NoThink").meta.llamaswap.modelID`).String() != "llamaSwap-llm-1" {
			t.Fatalf("models: %s", models)
		}
		b.Reset(t)
		release := b.HoldLoads(t)
		done := chatInBackground(t, b, "llamaSwap-llm-1:NoThink")
		state := func(model string) string {
			return gjson.Get(get(t, b.URL()+"/running"), `running.#(model=="`+model+`").state`).String()
		}
		during(t, done, "llamaSwap-llm-1 starting in /running", func() bool { return state("llamaSwap-llm-1") == "starting" })
		if state("llamaSwap-llm-1:NoThink") != "" {
			t.Error("/running lists the alias")
		}
		release()
		<-done
		if got := state("llamaSwap-llm-1"); got != "ready" {
			t.Errorf("after the load: %q", got)
		}
	})
}

// Ollama lists a model in /api/ps only once it has loaded.
func TestOllama_ListsModelOnlyOnceLoaded(t *testing.T) {
	forEachOllama(t, func(t *testing.T, b Driver) {
		b.Reset(t)
		release := b.HoldLoads(t)
		done := chatInBackground(t, b, "ollama-llm-1:latest")
		listed := func() bool {
			return gjson.Get(get(t, b.URL()+"/api/ps"), `models.#(name=="ollama-llm-1:latest")`).Exists()
		}
		during(t, done, "ollama-llm-1 missing from /api/ps", func() bool { return !listed() })
		release()
		<-done
		if !listed() {
			t.Errorf("after the load: %s", get(t, b.URL()+"/api/ps"))
		}
	})
}

// llama-server in router mode names its role in /props, lists each model's
// status, and shows a load as loading until it finishes; a model it doesn't
// have is a 400.
func TestLlamaServer_RouterListsStatusAndShowsLoads(t *testing.T) {
	f := NewLlamaServerRouter(t, ServerModel{ID: "a", Aliases: []string{"a-alt"}})
	if props := get(t, f.URL+"/props"); gjson.Get(props, "role").String() != "router" {
		t.Fatalf("props: %s", props)
	}
	load := f.HoldLoads()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"a-alt"}`); resp != nil {
			resp.Body.Close()
		}
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if models := get(t, f.URL+"/v1/models"); gjson.Get(models, "data.0.status.value").String() != "loading" || gjson.Get(models, "data.0.aliases.0").String() != "a-alt" {
		t.Errorf("during the load: %s", models)
	}
	load.Open()
	<-done
	if f.Status("a") != "loaded" {
		t.Errorf("after the load: %q", f.Status("a"))
	}
	if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"b"}`); resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown model: %+v", resp)
	} else {
		resp.Body.Close()
	}
}

// llama-server in single-model mode has no role in /props, lists its model
// with its own ID among the aliases, and answers every request with 503
// while the model loads.
func TestLlamaServer_SingleRefusesEverythingWhileLoading(t *testing.T) {
	f := NewLlamaServerSingle(t, ServerModel{ID: "a"})
	if props := get(t, f.URL+"/props"); gjson.Get(props, "role").Exists() || gjson.Get(props, "is_sleeping").Bool() {
		t.Errorf("props: %s", props)
	}
	if models := get(t, f.URL+"/v1/models"); gjson.Get(models, "data.0.aliases.0").String() != "a" {
		t.Errorf("models: %s", models)
	}
	f.SetStatus("a", "loading")
	for _, path := range []string{"/props", "/v1/models"} {
		resp, err := http.Get(f.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s while loading: %d", path, resp.StatusCode)
		}
	}
}

// LM Studio lists a model as loaded from the moment its load begins, and a
// second request for it waits for the load; the loaded model it replaces
// is listed no longer.
func TestLMStudio_ListsALoadAtOnce(t *testing.T) {
	f := NewLMStudio(t, "a", "b")
	f.SetLoaded("a")
	load := f.HoldLoads()
	chat := func(done chan<- int) {
		if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"b"}`); resp != nil {
			resp.Body.Close()
			done <- resp.StatusCode
		}
	}
	first, second := make(chan int, 1), make(chan int, 1)
	go chat(first)
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	go chat(second)
	f.WaitFor(t, "POST /v1/chat/completions", 2)
	models := get(t, f.URL+"/api/v1/models")
	if gjson.Get(models, `models.#(key=="b").loaded_instances.#`).Int() != 1 || gjson.Get(models, `models.#(key=="a").loaded_instances.#`).Int() != 0 {
		t.Errorf("during the load: %s", models)
	}
	select {
	case code := <-second:
		t.Fatalf("the second request was answered during the load: %d", code)
	default:
	}
	load.Open()
	if a, b := <-first, <-second; a != http.StatusOK || b != http.StatusOK {
		t.Errorf("after the load: %d and %d", a, b)
	}
}

// A request for another model during a load aborts it: the request that
// started it fails with LM Studio's 400, and the new model loads instead.
func TestLMStudio_AbortsALoadForAnotherModel(t *testing.T) {
	f := NewLMStudio(t, "a", "b")
	load := f.HoldLoads()
	first := make(chan *http.Response, 1)
	go func() { first <- post(t, f.URL+"/v1/chat/completions", `{"model":"a"}`) }()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	second := make(chan int, 1)
	go func() {
		if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"b"}`); resp != nil {
			resp.Body.Close()
			second <- resp.StatusCode
		}
	}()
	resp := <-first
	if resp == nil {
		t.Fatal("no answer to the first request")
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "aborted") {
		t.Errorf("the aborted request: %d %s", resp.StatusCode, body)
	}
	load.Open()
	if code := <-second; code != http.StatusOK || f.Loaded() != "b" {
		t.Errorf("the request that aborted it: %d, loaded %q", code, f.Loaded())
	}
}

// While LM Studio unpacks its engines after starting, its list and every
// completion request answer 500.
func TestLMStudio_RefusesEverythingWhileUnpacking(t *testing.T) {
	f := NewLMStudio(t, "a")
	f.SetUnpacking(true)
	resp, err := http.Get(f.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("list while unpacking: %d", resp.StatusCode)
	}
	if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"a"}`); resp != nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("a request while unpacking: %d", resp.StatusCode)
		}
	}
}

// Open WebUI answers the model list only with refresh=true, and runs
// OnModels while answering it.
func TestOpenWebUI_ModelListNeedsRefresh(t *testing.T) {
	f := NewOpenWebUI(t, OllamaEntry("m:1", true))
	var called atomic.Bool
	f.OnModels(func() { called.Store(true) })
	if body := get(t, f.URL+"/api/models"); !strings.Contains(body, "expected refresh=true") {
		t.Errorf("without refresh: %s", body)
	}
	if body := get(t, f.URL+"/api/models?refresh=true"); gjson.Get(body, "data.0.id").String() != "m:1" || !called.Load() {
		t.Errorf("with refresh: %s, OnModels called %v", body, called.Load())
	}
}

// A refusal carries its status and extra headers.
func TestReply_Refusal(t *testing.T) {
	f := NewLlamaSwap(t, SwapModel{ID: "a"})
	f.SetState("a", "ready")
	f.SetReply(Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "7"}, Body: `{"error":"busy"}`})
	resp := post(t, f.URL+"/v1/chat/completions", `{"model":"a"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "7" {
		t.Errorf("got %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}
