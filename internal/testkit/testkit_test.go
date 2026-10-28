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
		chat := chatInBackground(t, b, "llamaSwap-llm-1:NoThink")
		state := func(model string) string {
			return gjson.Get(get(t, b.URL()+"/running"), `running.#(model=="`+model+`").state`).String()
		}
		during(t, chat.done, "llamaSwap-llm-1 starting in /running", func() bool { return state("llamaSwap-llm-1") == "starting" })
		if state("llamaSwap-llm-1:NoThink") != "" {
			t.Error("/running lists the alias")
		}
		release()
		<-chat.done
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
		chat := chatInBackground(t, b, "ollama-llm-1:latest")
		listed := func() bool {
			return gjson.Get(get(t, b.URL()+"/api/ps"), `models.#(name=="ollama-llm-1:latest")`).Exists()
		}
		during(t, chat.done, "ollama-llm-1 missing from /api/ps", func() bool { return !listed() })
		release()
		<-chat.done
		if !listed() {
			t.Errorf("after the load: %s", get(t, b.URL()+"/api/ps"))
		}
	})
}

// llama-server in router mode says so in /props, lists each model with its
// status, loading while a request loads it and loaded once it has, and
// refuses a model it doesn't have with 400.
func TestLlamaServer_RouterListsStatusAndShowsLoads(t *testing.T) {
	forEachLlamaRouter(t, func(t *testing.T, b Driver) {
		if props := get(t, b.URL()+"/props"); gjson.Get(props, "role").String() != "router" {
			t.Fatalf("props: %s", props)
		}
		b.Reset(t)
		release := b.HoldLoads(t)
		chat := chatInBackground(t, b, "llamaRouter-llm-1")
		status := func() string {
			return gjson.Get(get(t, b.URL()+"/v1/models"), `data.#(id=="llamaRouter-llm-1").status.value`).String()
		}
		during(t, chat.done, "llamaRouter-llm-1 loading in /v1/models", func() bool { return status() == "loading" })
		release()
		<-chat.done
		if got := status(); got != "loaded" {
			t.Errorf("after the load: %q", got)
		}
		if resp := post(t, b.URL()+"/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`); resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unknown model: %+v", resp)
		} else {
			resp.Body.Close()
		}
	})
}

// A request through a router alias loads its base model, and the alias is
// listed with it. Fake only: dev-env's presets define no alias.
func TestLlamaServer_RouterResolvesAliases(t *testing.T) {
	f := NewLlamaServerRouter(t, ServerModel{ID: "a", Aliases: []string{"a-alt"}})
	if resp := post(t, f.URL+"/v1/chat/completions", `{"model":"a-alt"}`); resp != nil {
		resp.Body.Close()
	}
	if models := get(t, f.URL+"/v1/models"); gjson.Get(models, `data.#(id=="a").aliases.0`).String() != "a-alt" || f.Status("a") != "loaded" {
		t.Errorf("models: %s", models)
	}
}

// llama-server in single-model mode has no role in /props and lists its
// model with its own ID among the aliases.
func TestLlamaServer_SingleListsItsModel(t *testing.T) {
	forEachLlamaServer(t, func(t *testing.T, b Driver) {
		if props := get(t, b.URL()+"/props"); gjson.Get(props, "role").Exists() || gjson.Get(props, "is_sleeping").Bool() {
			t.Errorf("props: %s", props)
		}
		if models := get(t, b.URL()+"/v1/models"); !gjson.Get(models, `data.#(id=="llamaServer-llm-1").aliases.#(=="llamaServer-llm-1")`).Exists() {
			t.Errorf("models: %s", models)
		}
	})
}

// llama-server in single-model mode answers every request with 503 while
// its model loads (b11580 source, middleware_server_state; seen on /health
// and /props). Fake only: dev-env's model loads faster than the server's
// port starts answering, so a live run can't catch the 503s.
func TestLlamaServer_SingleRefusesEverythingWhileLoading(t *testing.T) {
	f := NewLlamaServerSingle(t, ServerModel{ID: "a"})
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

// LM Studio lists a model as loaded from the moment its load begins, no
// longer listing the model it replaces, and a second request for it during
// the load is answered once the load ends, as the first is.
func TestLMStudio_ListsALoadAtOnce(t *testing.T) {
	forEachLMStudio(t, func(t *testing.T, b Driver) {
		b.Load(t, "lmstudio-llm-1")
		release := b.HoldLoads(t)
		first := chatInBackground(t, b, "lmstudio-llm-2")
		during(t, first.done, "lmstudio-llm-2 listed in place of lmstudio-llm-1", func() bool {
			return lmStudioLoaded(t, b) == "lmstudio-llm-2"
		})
		second := chatInBackground(t, b, "lmstudio-llm-2")
		release()
		<-first.done
		<-second.done
		if first.status != http.StatusOK || second.status != http.StatusOK {
			t.Errorf("the two requests: %d %s; %d %s", first.status, first.body, second.status, second.body)
		}
	})
}

// A request for another model during a load aborts it: the request that
// started it fails with LM Studio's 400, and the new model loads instead.
func TestLMStudio_AbortsALoadForAnotherModel(t *testing.T) {
	forEachLMStudio(t, func(t *testing.T, b Driver) {
		b.Reset(t)
		release := b.HoldLoads(t)
		first := chatInBackground(t, b, "lmstudio-llm-1")
		during(t, first.done, "lmstudio-llm-1 listed", func() bool { return lmStudioLoaded(t, b) == "lmstudio-llm-1" })
		second := chatInBackground(t, b, "lmstudio-llm-2")
		<-first.done
		if first.status != http.StatusBadRequest || !strings.Contains(first.body, "aborted") {
			t.Errorf("the aborted request: %d %s", first.status, first.body)
		}
		release()
		<-second.done
		if loaded := lmStudioLoaded(t, b); second.status != http.StatusOK || loaded != "lmstudio-llm-2" {
			t.Errorf("the request that aborted it: %d %s, loaded %q", second.status, second.body, loaded)
		}
	})
}

// lmStudioLoaded returns the model LM Studio lists as loaded, "" if none.
func lmStudioLoaded(t *testing.T, b Driver) string {
	return gjson.Get(get(t, b.URL()+"/api/v1/models"), `models.#(loaded_instances.#>0).key`).String()
}

// While LM Studio unpacks its engines after starting, its list and every
// completion request answer 500 (llmster 0.0.25-1, verified). Fake only:
// control reports LM Studio ready only once its engines are unpacked, so a
// live run never sees it unpacking.
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
