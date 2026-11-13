package pipeline

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/mergepatch"
)

// fakeBackend accepts protocols and answers every request with reply,
// keeping what it was sent.
type fakeBackend struct {
	protocols []backend.Protocol
	reply     string
	sent      backend.ChatRequest
}

func (f *fakeBackend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) { return nil, nil }
func (f *fakeBackend) Protocols() []backend.Protocol                               { return f.protocols }
func (f *fakeBackend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	f.sent = req
	return backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusOK, Body: []byte(f.reply)}}, nil
}

// hook records what it was given and what its turn saw.
type hook struct {
	prepared []byte
	winner   Winner
	recorded []byte
}

func (h *hook) Prepare(ctx context.Context, body []byte, winner Winner) ([]byte, Turn, []string, error) {
	h.prepared, h.winner = body, winner
	return append([]byte(nil), body...), h, []string{"fromHook"}, nil
}

func (h *hook) Record(result backend.ChatResult) backend.ChatResult {
	h.recorded = result.Response.Body
	return result
}

func patches(t *testing.T, src string) []mergepatch.Patch {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatal(err)
	}
	compiled, err := mergepatch.Compile(node.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

// A request runs in the pipeline's order: the client protocol's hook sees the
// client's body, the translator runs, params apply in the protocol the
// backend receives, the model is rewritten last, and the hook's turn sees the
// reply translated back, with every dropped field reported once.
func TestServe_RunsTheStepsInOrder(t *testing.T) {
	claude := &fakeBackend{protocols: []backend.Protocol{backend.AnthropicMessages}, reply: `{"id":"m","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`}
	h := &hook{}
	p := &Pipeline{Hooks: map[backend.Protocol]Hook{backend.OpenAIChat: h}, Backends: map[string]backend.Backend{"claude": claude}}
	params := patches(t, `{thinking: {type: adaptive}, model: overridden}`)
	sel := Selected{
		Backend:     claude,
		BackendName: "claude",
		Model:       "claude-a",
		Accepted:    claude.protocols,
		Params: func(protocol backend.Protocol) []mergepatch.Patch {
			if protocol != backend.AnthropicMessages {
				t.Errorf("params asked for %s", protocol)
			}
			return params
		},
	}
	body := `{"model":"facade","seed":1,"messages":[{"role":"user","content":"hi"}]}`
	reply, err := p.Serve(context.Background(), backend.OpenAIChat, []byte(body), backend.Client{Name: "c"}, sel)
	if err != nil {
		t.Fatal(err)
	}
	if string(h.prepared) != body || h.winner.Backend != "claude" || h.winner.Native || h.winner.Client.Name != "c" {
		t.Errorf("hook prepared %s for %+v", h.prepared, h.winner)
	}
	sent := gjson.ParseBytes(claude.sent.Body)
	if claude.sent.Protocol != backend.AnthropicMessages || sent.Get("model").String() != "claude-a" || sent.Get("thinking.type").String() != "adaptive" || sent.Get("messages.0.content.0.text").String() != "hi" {
		t.Errorf("backend sent %s %s", claude.sent.Protocol, claude.sent.Body)
	}
	if gjson.GetBytes(h.recorded, "object").String() != "chat.completion" || gjson.GetBytes(reply.Chat.Response.Body, "choices.0.message.content").String() != "ok" {
		t.Errorf("recorded %s, replied %s", h.recorded, reply.Chat.Response.Body)
	}
	if len(reply.Dropped) != 2 || reply.Dropped[0] != "fromHook" || reply.Dropped[1] != "seed" {
		t.Errorf("dropped %v", reply.Dropped)
	}
}

// A request the backend accepts as it is reaches it untranslated, and a
// hook's refusal is the request's error.
func TestServe_PassesANativeRequestOnAndAHooksRefusal(t *testing.T) {
	openAI := &fakeBackend{protocols: []backend.Protocol{backend.OpenAIChat}, reply: `{"x":1}`}
	sel := Selected{Backend: openAI, BackendName: "openAi", Model: "gpt", Accepted: openAI.protocols, Params: func(backend.Protocol) []mergepatch.Patch { return nil }}
	p := &Pipeline{}
	reply, err := p.Serve(context.Background(), backend.OpenAIChat, []byte(`{"model":"f","temperature":0.50}`), backend.Client{}, sel)
	if err != nil || string(openAI.sent.Body) != `{"model":"gpt","temperature":0.50}` || string(reply.Chat.Response.Body) != `{"x":1}` {
		t.Errorf("sent %s, replied %+v, %v", openAI.sent.Body, reply, err)
	}
	refusing := &Pipeline{Hooks: map[backend.Protocol]Hook{backend.OpenAIChat: refuser{}}}
	_, err = refusing.Serve(context.Background(), backend.OpenAIChat, []byte(`{}`), backend.Client{}, sel)
	var failed *Error
	if !errors.As(err, &failed) || failed.Status != http.StatusBadRequest {
		t.Errorf("got %v", err)
	}
}

type refuser struct{}

func (refuser) Prepare(ctx context.Context, body []byte, winner Winner) ([]byte, Turn, []string, error) {
	return nil, nil, nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "no"}
}
