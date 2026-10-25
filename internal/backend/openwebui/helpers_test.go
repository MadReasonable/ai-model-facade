package openwebui

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newBackend builds an openWebUi backend for f; extra is appended to its
// config entry, such as `, chats: keep`.
func newBackend(t *testing.T, f *testkit.OpenWebUI, extra string) *Backend {
	t.Helper()
	return newBackendLogging(t, f, extra, discard)
}

func newBackendLogging(t *testing.T, f *testkit.OpenWebUI, extra string, log *slog.Logger) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "openWebUi", `{type: openWebUi, baseUrl: "`+f.URL+`", apiKey: svc, pollIntervalSeconds: 0.001`+extra+`}`), log)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newSwap starts a fake llama-swap with these models and a llamaSwap backend
// for it; extra is appended to the backend's config entry.
func newSwap(t *testing.T, extra string, models ...testkit.SwapModel) (*testkit.LlamaSwap, *llamaswap.Backend) {
	t.Helper()
	f := testkit.NewLlamaSwap(t, models...)
	ls, err := llamaswap.New(testkit.Entry(t, "ls", `{type: llamaSwap, baseUrl: "`+f.URL+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return f, ls
}

// link links b to the named backends and runs the checks it asks for, as the
// gateway does once every backend has been listed, and returns their
// warnings.
func link(t *testing.T, b *Backend, backends map[string]backend.Backend) ([]string, error) {
	t.Helper()
	var checks []backend.ListingCheck
	if err := b.Link(backends, func(c backend.ListingCheck) { checks = append(checks, c) }); err != nil {
		return nil, err
	}
	catalogs := map[string]*backend.Catalog{}
	for name, other := range map[string]backend.Backend{b.name: b, "ls": backends["ls"]} {
		models, err := other.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		catalogs[name] = backend.NewCatalog(models)
	}
	var warned []string
	for _, c := range checks {
		c.Run(catalogs, func(err error) { warned = append(warned, err.Error()) })
	}
	return warned, nil
}

// chatBody is a client's request with what the chat flow has to handle: a
// system message, history, a last user message with text and image parts,
// and fields the gateway doesn't know.
const chatBody = `{"model":"qwen3:1.7b","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"My name is Zorblax."},{"role":"assistant","content":"Hi."},{"role":"user","content":[{"type":"text","text":"What is"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"my name?"}]}],"x_vendor":{"a":1},"features":{"web_search":true}}`

// chat sends body through b, from a client that has passed two gateways.
func chat(t *testing.T, b *Backend, body string, stream bool) (backend.ChatResult, error) {
	t.Helper()
	return b.Chat(context.Background(), backend.ChatRequest{
		Endpoint: backend.ChatCompletions,
		Body:     []byte(body),
		Stream:   stream,
		Client:   backend.Client{Route: []string{"gw1", "gw2"}},
	})
}

// deleted waits until the chat has been deleted, which happens in the
// background after the result is returned, and returns the chat flow's calls.
func deleted(t *testing.T, f *testkit.OpenWebUI) []string {
	t.Helper()
	f.WaitFor(t, "DELETE /api/v1/chats/chat-1", 1)
	return f.ChatCalls()
}

// stoppedThenDeleted reports whether calls stop the task, then end by
// deleting the chat. A poll may be recorded between the two: one the client
// gave up on when it left can still reach the fake's handler after the stop.
func stoppedThenDeleted(calls []string) bool {
	stop := slices.Index(calls, "POST /api/tasks/chat/chat-1/stop")
	return stop >= 0 && stop < len(calls)-1 && calls[len(calls)-1] == "DELETE /api/v1/chats/chat-1"
}
