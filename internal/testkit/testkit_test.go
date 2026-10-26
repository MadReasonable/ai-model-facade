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
// a load shows as starting in /running until it finishes; /running never
// lists aliases.
func TestLlamaSwap_ListsAliasesAndShowsLoads(t *testing.T) {
	f := NewLlamaSwap(t, SwapModel{ID: "a", Aliases: []string{"a:Fast"}})
	models := get(t, f.URL+"/v1/models")
	if gjson.Get(models, `data.#(id=="a").meta.llamaswap.aliases.0`).String() != "a:Fast" || gjson.Get(models, `data.#(id=="a:Fast").meta.llamaswap.modelID`).String() != "a" {
		t.Fatalf("models: %s", models)
	}
	load := f.HoldLoads()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"a:Fast"}`); resp != nil {
			resp.Body.Close()
		}
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if running := get(t, f.URL+"/running"); running != `{"running":[{"model":"a","state":"starting"}]}`+"\n" {
		t.Errorf("during the load: %s", running)
	}
	load.Open()
	<-done
	if f.State("a") != "ready" {
		t.Errorf("after the load: %q", f.State("a"))
	}
}

// Ollama lists a model in /api/ps only once it has loaded.
func TestOllama_ListsModelOnlyOnceLoaded(t *testing.T) {
	f := NewOllama(t, "m:1")
	load := f.HoldLoads()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"m:1"}`); resp != nil {
			resp.Body.Close()
		}
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if ps := get(t, f.URL+"/api/ps"); !strings.Contains(ps, `"models":[]`) {
		t.Errorf("during the load: %s", ps)
	}
	load.Open()
	<-done
	if ps := get(t, f.URL+"/api/ps"); !strings.Contains(ps, `"name":"m:1"`) {
		t.Errorf("after the load: %s", ps)
	}
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
