package testkit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// OpenWebUI is a fake Open WebUI (0.12.3): its model list, and the chat flow
// the gateway runs, one chat at a time, always chat-1.
//
//   - GET /api/models?refresh=true answers the entries set with SetModels;
//     without refresh=true it answers 400, so a test fails if the adapter
//     reads the cached list. OnModels runs while it answers, the way Open
//     WebUI asks its connections, such as a gateway, during a refresh.
//   - POST /api/v1/chats/new creates chat-1.
//   - POST /api/chat/completions starts a background task and answers with
//     its task_ids, unless Chat says otherwise.
//   - GET /api/v1/chats/chat-1 shows the assistant message as Chat describes
//     it at each poll; the last poll's message is done.
//   - GET /api/tasks/chat/chat-1 lists the task until every poll is done.
//   - Stopping, deleting and archiving chat-1 answer true.
type OpenWebUI struct {
	*httptest.Server
	Recorder

	mu       sync.Mutex
	models   string
	onModels func()
	chat     Chat
	polls    int
}

// Chat is how a fake Open WebUI runs a chat. The zero Chat answers "ok" on
// the first poll.
type Chat struct {
	Contents  []string   // the assistant message's content at each poll; default {"ok"}
	Reasoning [][]string // if set, the text of the reasoning items at each poll
	Error     string     // if set, the finished message carries this error

	StartStatus int    // if set, the start answers this status with an error body
	StartBody   string // if set, the start answers 200 with this body, without task_ids

	// PollGate, if set, makes each poll wait for one value, so a test steps
	// through the polls, or for the channel to be closed. StopGate, if set,
	// makes the stop call wait until it is closed.
	PollGate chan struct{}
	StopGate chan struct{}
}

func (c Chat) contents() []string {
	if len(c.Contents) == 0 {
		return []string{"ok"}
	}
	return c.Contents
}

// NewOpenWebUI starts a fake Open WebUI listing these /api/models entries
// (see the entry builders below), closed when t ends.
func NewOpenWebUI(t testing.TB, entries ...string) *OpenWebUI {
	f := &OpenWebUI{models: strings.Join(entries, ",")}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetModels replaces the /api/models entries.
func (f *OpenWebUI) SetModels(entries ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = strings.Join(entries, ",")
}

// OnModels sets a function to run while /api/models answers.
func (f *OpenWebUI) OnModels(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onModels = fn
}

// SetChat sets how the next chat runs.
func (f *OpenWebUI) SetChat(c Chat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chat = c
	f.polls = 0
}

// ChatCalls returns the key of every request but the model list, in order:
// the chat flow as Open WebUI saw it.
func (f *OpenWebUI) ChatCalls() []string {
	var calls []string
	for _, k := range f.Keys() {
		if k != "GET /api/models" {
			calls = append(calls, k)
		}
	}
	return calls
}

func (f *OpenWebUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	f.mu.Lock()
	chat := f.chat
	f.mu.Unlock()
	switch path := r.URL.Path; {
	case path == "/api/models":
		if r.URL.Query().Get("refresh") != "true" {
			http.Error(w, "expected refresh=true", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		models, onModels := f.models, f.onModels
		f.mu.Unlock()
		if onModels != nil {
			onModels()
		}
		io.WriteString(w, `{"data":[`+models+`]}`)
	case path == "/api/v1/chats/new":
		io.WriteString(w, `{"id":"chat-1","title":"x"}`)
	case path == "/api/chat/completions":
		switch {
		case chat.StartStatus != 0:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(chat.StartStatus)
			io.WriteString(w, `{"detail":"Model not found"}`)
		case chat.StartBody != "":
			io.WriteString(w, chat.StartBody)
		default:
			io.WriteString(w, `{"status":true,"task_ids":["t1"],"chat_id":"chat-1"}`)
		}
	case path == "/api/v1/chats/chat-1" && r.Method == http.MethodGet:
		if chat.PollGate != nil {
			select {
			case <-chat.PollGate:
			case <-r.Context().Done():
				return
			}
		}
		f.servePoll(w, chat)
	case path == "/api/tasks/chat/chat-1":
		f.mu.Lock()
		running := f.polls < len(chat.contents())
		f.mu.Unlock()
		if running {
			io.WriteString(w, `{"task_ids":["t1"]}`)
			return
		}
		io.WriteString(w, `{"task_ids":[]}`)
	case path == "/api/tasks/chat/chat-1/stop":
		if chat.StopGate != nil {
			<-chat.StopGate
		}
		io.WriteString(w, `true`)
	case path == "/api/v1/chats/chat-1" && r.Method == http.MethodDelete, path == "/api/v1/chats/chat-1/archive":
		io.WriteString(w, `true`)
	default:
		http.NotFound(w, r)
	}
}

// servePoll answers one poll of chat-1 with the assistant message the start
// request named.
func (f *OpenWebUI) servePoll(w http.ResponseWriter, chat Chat) {
	start, _ := f.Last("POST /api/chat/completions")
	id := gjson.Get(start.Body, "id").String()
	contents := chat.contents()
	f.mu.Lock()
	i := min(f.polls, len(contents)-1)
	f.polls++
	f.mu.Unlock()
	done := i == len(contents)-1
	msg := map[string]any{"id": id, "role": "assistant", "content": contents[i], "done": done}
	if chat.Reasoning != nil {
		// One reasoning item per model call, next to the other output items.
		output := []any{map[string]any{"type": "message", "content": []any{}}}
		for _, text := range chat.Reasoning[i] {
			output = append(output, map[string]any{"type": "reasoning", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "text": text}}})
		}
		msg["output"] = output
	}
	if done {
		msg["usage"] = map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15, "eval_count": 3}
		if chat.Error != "" {
			msg["error"] = map[string]any{"content": chat.Error}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"chat": map[string]any{"history": map[string]any{"messages": map[string]any{id: msg}}}})
}

// The entry builders return one /api/models entry each, as Open WebUI lists
// a model of each kind of connection.

// OllamaEntry is a model of an Ollama connection, with Ollama's loaded flag.
func OllamaEntry(id string, loaded bool) string {
	return mustJSON(map[string]any{"id": id, "owned_by": "ollama", "loaded": loaded})
}

// SwapEntry is a llama-swap base model of an OpenAI connection, with
// llama-swap's status ("loaded" or "unloaded").
func SwapEntry(id, status string) string {
	return mustJSON(map[string]any{"id": id, "owned_by": "openai", "status": map[string]any{"value": status}, "meta": map[string]any{"llamaswap": map[string]any{"type": "model"}}})
}

// SwapPresetEntry is a llama-swap preset whose base model has llama-swap ID
// base, with the base's status.
func SwapPresetEntry(id, base, status string) string {
	return mustJSON(map[string]any{"id": id, "owned_by": "openai", "status": map[string]any{"value": status}, "meta": map[string]any{"llamaswap": map[string]any{"type": "alias", "modelID": base}}})
}

// FacadeEntry is a facade of a gateway connection, with its current reason;
// model is omitted when empty.
func FacadeEntry(id, model, reason string) string {
	current := map[string]any{"reason": reason}
	if model != "" {
		current["model"] = model
	}
	return mustJSON(map[string]any{"id": id, "owned_by": "openai", "current": current})
}

// WorkspaceEntry is a workspace model built on base.
func WorkspaceEntry(id, base string) string {
	return mustJSON(map[string]any{"id": id, "owned_by": "openai", "info": map[string]any{"base_model_id": base}})
}

// PlainEntry is a model of an OpenAI connection that reports no state, such
// as a hosted provider's.
func PlainEntry(id string) string {
	return mustJSON(map[string]any{"id": id, "owned_by": "openai"})
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
