package anthropic

import (
	"context"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.Anthropic, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "anthropic", `{type: anthropic, baseUrl: "`+f.URL+`", apiKey: test-key`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The model list is every page of the API's list, read with the backend's
// key as a bearer token and the anthropic-version header the API requires.
func TestAnthropic_ListsEveryPage(t *testing.T) {
	var ids []string
	for i := range 1001 {
		ids = append(ids, fmt.Sprintf("claude-%d", i))
	}
	f := testkit.NewAnthropic(t, ids...)
	got, err := newBackend(t, f, "").ListModels(context.Background())
	if err != nil || len(got) != 1001 || got[0].ID != "claude-0" || got[1000].ID != "claude-1000" {
		t.Fatalf("got %d models, %v", len(got), err)
	}
	if n := f.Count("GET /v1/models"); n != 2 {
		t.Errorf("/v1/models read %d times, want 2 pages", n)
	}
	if r, _ := f.Last("GET /v1/models"); r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Anthropic-Version") == "" {
		t.Errorf("Authorization %q, anthropic-version %q", r.Header.Get("Authorization"), r.Header.Get("Anthropic-Version"))
	}
}

// Every listed model is ready, and a state read after the listing calls the
// API no more.
func TestAnthropic_EveryListedModelIsReadyWithoutACall(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a", "claude-b")
	b := newBackend(t, f, "")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := b.LoadedModels(context.Background())
	if err != nil || len(got) != 2 || got[0].Starting || got[1].Starting {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n := f.Count("GET /v1/models"); n != 1 {
		t.Errorf("/v1/models read %d times, want 1", n)
	}
}

func TestAnthropic_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: anthropic}", "{type: anthropic, baseUrl: x, workspaceId: w}"} {
		if _, err := New(testkit.Entry(t, "anthropic", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}

// A Messages request goes to the API as it is, and what the API answers comes
// back unchanged, an error included, for a client that speaks Messages.
func TestAnthropic_ForwardsMessagesUnchanged(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	body := `{"model":"claude-a","temperature":0.50,"max_tokens":5,"messages":[]}`
	res, err := b.Chat(context.Background(), backend.ChatRequest{Protocol: backend.AnthropicMessages, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if req, ok := f.Last("POST /v1/messages"); !ok || req.Body != body {
		t.Errorf("sent %+v", req)
	}
	if res.Response == nil || res.Response.Status != 200 || gjson.GetBytes(res.Response.Body, "type").String() != "message" {
		t.Errorf("got %+v", res.Response)
	}
}
