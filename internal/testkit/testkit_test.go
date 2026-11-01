package testkit

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// LocalAI lists an alias as a model of its own and maps it to its target in
// /api/aliases; a request through the alias loads the target, which /system
// lists, under the target's ID, only once the load ends.
func TestLocalAI_ListsAliasesAndModelsOnlyOnceLoaded(t *testing.T) {
	forEachLocalAI(t, func(t *testing.T, b Driver) {
		if models := get(t, b.URL()+"/v1/models"); !gjson.Get(models, `data.#(id=="localai-llm-1:alias")`).Exists() {
			t.Errorf("models: %s", models)
		}
		if aliases := get(t, b.URL()+"/api/aliases"); gjson.Get(aliases, `#(name=="localai-llm-1:alias").target`).String() != "localai-llm-1" {
			t.Errorf("aliases: %s", aliases)
		}
		b.Reset(t)
		release := b.HoldLoads(t)
		chat := chatInBackground(t, b, "localai-llm-1:alias")
		during(t, chat.done, "nothing in /system", func() bool { return localAILoaded(t, b) == "" })
		release()
		<-chat.done
		if loaded := localAILoaded(t, b); chat.status != http.StatusOK || loaded != "localai-llm-1" {
			t.Errorf("after the load: %d %s, loaded %q", chat.status, chat.body, loaded)
		}
	})
}

// With a limit on loaded models, even of two, a request for a model that
// isn't loaded during a load, the loading one too, waits for it, then evicts
// the model it loaded: the request that started the load fails with 500, and
// the later request loads its model and answers. LocalAI shows no load under
// way, so the claim gives each request 300 ms to arrive before the next
// step; a real load takes longer.
func TestLocalAI_EvictsALoadForAnyModelNotLoaded(t *testing.T) {
	for _, second := range []string{"localai-llm-2", "localai-llm-1"} {
		t.Run(second, func(t *testing.T) {
			forEachLocalAI(t, func(t *testing.T, b Driver) {
				b.Reset(t)
				release := b.HoldLoads(t)
				first := chatInBackground(t, b, "localai-llm-1")
				time.Sleep(300 * time.Millisecond)
				later := chatInBackground(t, b, second)
				time.Sleep(300 * time.Millisecond)
				release()
				<-first.done
				<-later.done
				if first.status != http.StatusInternalServerError {
					t.Errorf("the evicted request: %d %s", first.status, first.body)
				}
				if loaded := localAILoaded(t, b); later.status != http.StatusOK || loaded != second {
					t.Errorf("the request that evicted it: %d %s, loaded %q", later.status, later.body, loaded)
				}
			})
		})
	}
}

// localAILoaded returns the models LocalAI lists as loaded, comma-separated.
func localAILoaded(t *testing.T, b Driver) string {
	var ids []string
	for _, m := range gjson.Get(get(t, b.URL()+"/system"), "loaded_models.#.id").Array() {
		ids = append(ids, m.String())
	}
	return strings.Join(ids, ",")
}

// OpenAI lists each model with its object and owner, answers a chat for a
// listed model, and refuses one it doesn't list with 404 and the code
// model_not_found, which the gateway passes on.
func TestOpenAI_ListsModelsAndRefusesAnUnknownOne(t *testing.T) {
	forEachOpenAI(t, func(t *testing.T, b HostedDriver) {
		auth := map[string]string{"Authorization": "Bearer " + b.Key()}
		_, models := hostedCall(t, b, http.MethodGet, "/v1/models", "", auth)
		entry := gjson.Get(models, `data.#(id==`+strconv.Quote(b.Model())+`)`)
		if gjson.Get(models, "object").String() != "list" || entry.Get("object").String() != "model" || !entry.Get("owned_by").Exists() {
			t.Errorf("models: %.500s", models)
		}
		chat := `{"model":` + strconv.Quote(b.Model()) + `,"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":16}`
		if status, body := hostedCall(t, b, http.MethodPost, "/v1/chat/completions", chat, auth); status != http.StatusOK {
			t.Errorf("a chat: %d %.500s", status, body)
		}
		unknown := `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`
		if status, body := hostedCall(t, b, http.MethodPost, "/v1/chat/completions", unknown, auth); status != http.StatusNotFound || gjson.Get(body, "error.code").String() != "model_not_found" {
			t.Errorf("an unknown model: %d %.500s", status, body)
		}
	})
}

// The Claude API takes a bearer token on both endpoints the gateway uses. It
// lists its models only with an anthropic-version header, a page at a time,
// each page saying whether another follows and which model the next one
// starts after, and each model its output cap as max_tokens.
func TestAnthropic_PagesItsModelsWithTheirOutputCaps(t *testing.T) {
	forEachAnthropic(t, func(t *testing.T, b HostedDriver) {
		auth := map[string]string{"Authorization": "Bearer " + b.Key()}
		if status, body := hostedCall(t, b, http.MethodGet, "/v1/models", "", auth); status != http.StatusBadRequest || !strings.Contains(body, "anthropic-version") {
			t.Errorf("models without anthropic-version: %d %.500s", status, body)
		}
		auth["Anthropic-Version"] = "2023-06-01"
		_, first := hostedCall(t, b, http.MethodGet, "/v1/models?limit=1", "", auth)
		last := gjson.Get(first, "last_id").String()
		if gjson.Get(first, "data.#").Int() != 1 || gjson.Get(first, "data.0.type").String() != "model" || gjson.Get(first, "data.0.max_tokens").Int() <= 0 || !gjson.Get(first, "has_more").Bool() || last != gjson.Get(first, "data.0.id").String() {
			t.Errorf("first page: %.500s", first)
		}
		_, second := hostedCall(t, b, http.MethodGet, "/v1/models?limit=1&after_id="+last, "", auth)
		if id := gjson.Get(second, "data.0.id").String(); id == "" || id == last {
			t.Errorf("second page: %.500s", second)
		}
	})
}

// The Messages API needs anthropic-version too, refuses a field it doesn't
// know and a missing max_tokens with 400 and a model it doesn't list with
// 404, in its error shape. It answers a chat with a message of content
// blocks, a stop reason and usage, or, streamed, with message_start first,
// a text delta, message_delta with the stop reason, and message_stop last.
func TestAnthropic_AnswersMessagesWholeOrStreamed(t *testing.T) {
	forEachAnthropic(t, func(t *testing.T, b HostedDriver) {
		auth := map[string]string{"Authorization": "Bearer " + b.Key()}
		model := strconv.Quote(b.Model())
		chat := `{"model":` + model + `,"messages":[{"role":"user","content":"Say ok."}],"max_tokens":256}`
		refusals := []struct {
			what, body, header string
			status             int
			want               string
		}{
			{"no anthropic-version", chat, "", http.StatusBadRequest, "anthropic-version"},
			{"an unknown field", `{"model":` + model + `,"max_tokens":5,"seed":1,"messages":[{"role":"user","content":"hi"}]}`, "2023-06-01", http.StatusBadRequest, "seed: Extra inputs are not permitted"},
			{"no max_tokens", `{"model":` + model + `,"messages":[{"role":"user","content":"hi"}]}`, "2023-06-01", http.StatusBadRequest, "max_tokens: Field required"},
			{"an unknown model", `{"model":"claude-nope","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, "2023-06-01", http.StatusNotFound, "not_found_error"},
		}
		for _, r := range refusals {
			delete(auth, "Anthropic-Version")
			if r.header != "" {
				auth["Anthropic-Version"] = r.header
			}
			status, body := hostedCall(t, b, http.MethodPost, "/v1/messages", r.body, auth)
			if status != r.status || gjson.Get(body, "type").String() != "error" || !strings.Contains(body, r.want) {
				t.Errorf("%s: %d %.500s", r.what, status, body)
			}
		}
		auth["Anthropic-Version"] = "2023-06-01"
		status, body := hostedCall(t, b, http.MethodPost, "/v1/messages", chat, auth)
		if status != http.StatusOK || gjson.Get(body, "type").String() != "message" || gjson.Get(body, `content.#(type=="text").text`).String() == "" || gjson.Get(body, "stop_reason").String() == "" || !gjson.Get(body, "usage.output_tokens").Exists() {
			t.Errorf("a chat: %d %.500s", status, body)
		}
		streamed := strings.Replace(chat, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
		status, body = hostedCall(t, b, http.MethodPost, "/v1/messages", streamed, auth)
		var events []string
		var stopReason string
		text := false
		for _, line := range strings.Split(body, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				event := gjson.Parse(data)
				events = append(events, event.Get("type").String())
				text = text || event.Get("delta.type").String() == "text_delta"
				if event.Get("type").String() == "message_delta" {
					stopReason = event.Get("delta.stop_reason").String()
				}
			}
		}
		if status != http.StatusOK || len(events) < 2 || events[0] != "message_start" || events[len(events)-1] != "message_stop" || !text || stopReason == "" {
			t.Errorf("a streamed chat: %d, events %v, stop reason %q", status, events, stopReason)
		}
	})
}

// Open WebUI lists each model of its connections as the entry builders
// build it, with its connection's state: Ollama's with Ollama's loaded flag,
// and llama-swap's base models and presets with llama-swap's status; and a
// workspace model with its base.
func TestOpenWebUI_ListsModelsAsTheEntryBuildersDo(t *testing.T) {
	forEachOpenWebUI(t, func(t *testing.T, b OpenWebUIDriver) {
		b.LlamaSwap().Reset(t)
		b.Ollama().Reset(t)
		b.LlamaSwap().Load(t, "llamaSwap-llm-1")
		b.Ollama().Load(t, "ollama-llm-1:latest")
		models := gjson.Get(b.Models(t), "data")
		for _, entry := range []string{
			SwapEntry("llamaSwap-llm-1", "loaded"),
			SwapPresetEntry("llamaSwap-llm-1:NoThink", "llamaSwap-llm-1", "loaded"),
			SwapEntry("llamaSwap-llm-2", "unloaded"),
			OllamaEntry("ollama-llm-1:latest", true),
			OllamaEntry("ollama-llm-2:latest", false),
			WorkspaceEntry("openWebUi-llm-1:NoThink", "ollama-llm-1:latest", OllamaEntry("ollama-llm-1:latest", true)),
		} {
			want := gjson.Parse(entry)
			got := models.Get(`#(id==` + strconv.Quote(want.Get("id").String()) + `)`)
			if !holds(got, want) {
				t.Errorf("entry %s: listed as %s", entry, got.Raw)
			}
		}
	})
}

// holds reports whether got has every field of want, with want's value.
func holds(got, want gjson.Result) bool {
	if !want.IsObject() {
		return got.Exists() && got.Value() == want.Value()
	}
	ok := true
	want.ForEach(func(key, value gjson.Result) bool {
		ok = holds(got.Get(gjson.Escape(key.String())), value)
		return ok
	})
	return ok
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
