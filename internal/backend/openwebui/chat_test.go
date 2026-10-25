package openwebui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// readStream collects a stream's chunks until it ends.
func readStream(t *testing.T, s backend.ChatStream) []gjson.Result {
	t.Helper()
	defer s.Close()
	var chunks []gjson.Result
	for {
		data, err := s.Next()
		if err == io.EOF {
			return chunks
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, gjson.ParseBytes(data))
	}
}

// A non-streamed request returns the finished message as one OpenAI
// response. The chat record holds the last user message's text; the start
// is the client's body untouched apart from the appended control fields, with
// stream on and the route carried over; afterwards the chat is deleted.
func TestChat_ReturnsTheFinishedMessage(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	f.SetChat(testkit.Chat{Contents: []string{"", "Your", "Your name is Zorblax."}})
	res, err := chat(t, newBackend(t, f, ""), chatBody, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stream != nil || res.Response.Status != http.StatusOK {
		t.Fatalf("got %+v", res)
	}
	resp := gjson.ParseBytes(res.Response.Body)
	if resp.Get("object").String() != "chat.completion" || resp.Get("model").String() != "qwen3:1.7b" || resp.Get("choices.0.message.content").String() != "Your name is Zorblax." || resp.Get("choices.0.finish_reason").String() != "stop" || resp.Get("usage.total_tokens").Int() != 15 {
		t.Errorf("response: %s", res.Response.Body)
	}
	createReq, _ := f.Last("POST /api/v1/chats/new")
	startReq, _ := f.Last("POST /api/chat/completions")
	created := gjson.Parse(createReq.Body).Get("chat")
	asst := gjson.Get(startReq.Body, "id").String()
	user := created.Get("history.messages." + asst + ".parentId").String()
	if created.Get("history.currentId").String() != asst || created.Get("history.messages."+user+".content").String() != "What is\nmy name?" || created.Get("history.messages."+asst+".done").Bool() || created.Get("title").String() != "What is my name?" {
		t.Errorf("created: %s", createReq.Body)
	}
	if !strings.HasPrefix(startReq.Body, strings.TrimSuffix(chatBody, "}")+",") {
		t.Errorf("client body not preserved:\n%s", startReq.Body)
	}
	started := gjson.Parse(startReq.Body)
	if !started.Get("stream").Bool() || started.Get("chat_id").String() != "chat-1" || !strings.HasPrefix(started.Get("session_id").String(), "ai-model-facade-") || started.Get("background_tasks.title_generation").Bool() || started.Get(backend.RouteField).Raw != `["gw1","gw2"]` {
		t.Errorf("started: %s", startReq.Body)
	}
	// The task is seen to have ended before the chat is deleted, and isn't
	// stopped.
	if calls := deleted(t, f); slices.Contains(calls, "POST /api/tasks/chat/chat-1/stop") || !slices.Equal(calls[len(calls)-2:], []string{"GET /api/tasks/chat/chat-1", "DELETE /api/v1/chats/chat-1"}) {
		t.Errorf("calls: %v", calls)
	}
}

// A streamed request turns each growth of the message into a content delta,
// then a stop chunk and, if asked for, a usage chunk.
func TestChat_StreamsTheMessageAsItGrows(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	f.SetChat(testkit.Chat{Contents: []string{"", "Your", "Your", "Your name", "Your name is Zorblax."}})
	body := strings.Replace(chatBody, `"x_vendor"`, `"stream":true,"stream_options":{"include_usage":true},"x_vendor"`, 1)
	res, err := chat(t, newBackend(t, f, ""), body, true)
	if err != nil || res.Stream == nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	var deltas []string
	var finish, usage string
	for _, c := range readStream(t, res.Stream) {
		if c.Get("object").String() != "chat.completion.chunk" {
			t.Errorf("chunk: %s", c.Raw)
		}
		if d := c.Get("choices.0.delta.content"); d.Exists() {
			deltas = append(deltas, d.String())
		}
		if r := c.Get("choices.0.finish_reason").String(); r != "" {
			finish = r
		}
		if u := c.Get("usage.total_tokens"); u.Exists() {
			usage = u.String()
		}
	}
	if strings.Join(deltas, "|") != "|Your| name| is Zorblax." || finish != "stop" || usage != "15" {
		t.Errorf("deltas %q, finish %q, usage %q", deltas, finish, usage)
	}
	deleted(t, f)
}

// Reasoning reaches the client as reasoning_content: streamed ahead of the
// content as it grows, and in the message of a non-streamed reply; a tool
// loop's reasoning items are joined in order. Without reasoning, the
// message has none.
func TestChat_ReturnsReasoningAsReasoningContent(t *testing.T) {
	reasoning := testkit.Chat{
		Contents:  []string{"", "", "", "", "It is"},
		Reasoning: [][]string{{}, {"Think"}, {"Thinking."}, {"Thinking.", "Again"}, {"Thinking.", "Again."}},
	}
	t.Run("streamed", func(t *testing.T) {
		f := testkit.NewOpenWebUI(t)
		f.SetChat(reasoning)
		res, err := chat(t, newBackend(t, f, ""), chatBody, true)
		if err != nil {
			t.Fatal(err)
		}
		var deltas []string
		for _, c := range readStream(t, res.Stream) {
			delta := c.Get("choices.0.delta")
			if r := delta.Get("reasoning_content"); r.Exists() {
				deltas = append(deltas, "r:"+r.String())
			}
			if c := delta.Get("content"); c.Exists() && c.String() != "" {
				deltas = append(deltas, "c:"+c.String())
			}
		}
		if got := strings.Join(deltas, "|"); got != "r:Think|r:ing.|r:\n\nAgain|r:.|c:It is" {
			t.Errorf("deltas %q", got)
		}
	})
	t.Run("not streamed", func(t *testing.T) {
		f := testkit.NewOpenWebUI(t)
		f.SetChat(reasoning)
		res, err := chat(t, newBackend(t, f, ""), chatBody, false)
		if msg := gjson.GetBytes(res.Response.Body, "choices.0.message"); err != nil || msg.Get("reasoning_content").String() != "Thinking.\n\nAgain." || msg.Get("content").String() != "It is" {
			t.Errorf("message: %s, %v", msg.Raw, err)
		}
	})
	t.Run("no reasoning", func(t *testing.T) {
		f := testkit.NewOpenWebUI(t)
		res, err := chat(t, newBackend(t, f, ""), chatBody, false)
		if err != nil || gjson.GetBytes(res.Response.Body, "choices.0.message.reasoning_content").Exists() {
			t.Errorf("got %s, %v", res.Response.Body, err)
		}
	})
}

// Usage comes from what Open WebUI stored: its own input and output totals
// win over the last call's prompt and completion counts, which a tool loop
// leaves behind.
func TestChat_ReportsUsage(t *testing.T) {
	cases := []struct{ name, stored, want string }{
		{"prompt and completion only", `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`, `{"completion_tokens":3,"prompt_tokens":2,"total_tokens":5}`},
		{"input tokens win", `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5,"input_tokens":4}`, `{"completion_tokens":3,"prompt_tokens":4,"total_tokens":7}`},
		{"input and output", `{"input_tokens":5845,"output_tokens":2,"total_tokens":5847,"cache_n":5841}`, `{"completion_tokens":2,"prompt_tokens":5845,"total_tokens":5847}`},
		{"total computed", `{"input_tokens":4,"output_tokens":2}`, `{"completion_tokens":2,"prompt_tokens":4,"total_tokens":6}`},
		{"tool loop", `{"input_tokens":25,"output_tokens":5,"total_tokens":30,"prompt_tokens":15,"completion_tokens":3}`, `{"completion_tokens":5,"prompt_tokens":25,"total_tokens":30}`},
		{"no counts", `{"eval_count":3}`, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := json.Marshal(usageOf(message{usage: gjson.Parse(c.stored)}))
			if string(got) != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	if usageOf(message{}) != nil {
		t.Error("no usage stored: expected nil")
	}
}

// A client that goes away mid-stream stops the task and the chat is still
// deleted; Close doesn't wait for either call.
func TestChat_ClientLeavingAStreamStopsTheTask(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	stop := make(chan struct{})
	f.SetChat(testkit.Chat{Contents: []string{"", "a"}, PollGate: make(chan struct{}), StopGate: stop})
	res, err := chat(t, newBackend(t, f, ""), chatBody, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Stream.Next(); err != nil { // the role chunk
		t.Fatal(err)
	}
	f.WaitFor(t, "GET /api/v1/chats/chat-1", 1) // a poll is waiting
	closed := make(chan struct{})
	go func() { res.Stream.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(testkit.Timeout):
		t.Fatal("Close waited for the stop call")
	}
	close(stop)
	if calls := deleted(t, f); !stoppedThenDeleted(calls) {
		t.Errorf("calls: %v", calls)
	}
}

// A non-streamed request whose client goes away returns at once; the task is
// stopped and the chat deleted afterwards.
func TestChat_ClientLeavingARequestStopsTheTask(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	stop := make(chan struct{})
	f.SetChat(testkit.Chat{Contents: []string{"", "a"}, PollGate: make(chan struct{}), StopGate: stop})
	b := newBackend(t, f, "")
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := b.Chat(ctx, backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(chatBody)})
		returned <- err
	}()
	f.WaitFor(t, "GET /api/v1/chats/chat-1", 1) // a poll is waiting
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(testkit.Timeout):
		t.Fatal("Chat waited for the stop call")
	}
	close(stop)
	if calls := deleted(t, f); !stoppedThenDeleted(calls) {
		t.Errorf("calls: %v", calls)
	}
}

// An error saved on the finished message fails the request.
func TestChat_FailsOnAMessageError(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	f.SetChat(testkit.Chat{Contents: []string{"", "x"}, Error: "context window exceeded"})
	if _, err := chat(t, newBackend(t, f, ""), chatBody, false); err == nil || !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("got %v", err)
	}
}

// A start Open WebUI refuses is passed on as the response, and the chat is
// deleted.
func TestChat_PassesARefusedStartOn(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	f.SetChat(testkit.Chat{StartStatus: http.StatusBadRequest})
	res, err := chat(t, newBackend(t, f, ""), chatBody, false)
	if err != nil || res.Response.Status != http.StatusBadRequest || !strings.Contains(string(res.Response.Body), "Model not found") {
		t.Errorf("got %+v, %v", res.Response, err)
	}
	deleted(t, f)
}

// A start that answers without task_ids is logged as an error, and the chat
// is still polled for the reply.
func TestChat_LogsAStartWithoutABackgroundTask(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	f.SetChat(testkit.Chat{Contents: []string{"hi"}, StartBody: `{"choices":[{"message":{"content":"hi"}}]}`})
	var logs bytes.Buffer
	res, err := chat(t, newBackendLogging(t, f, "", slog.New(slog.NewTextHandler(&logs, nil))), chatBody, false)
	if err != nil || !strings.Contains(string(res.Response.Body), `"content":"hi"`) {
		t.Errorf("got %+v, %v", res.Response, err)
	}
	// Waiting for the background delete first keeps a failure it might log
	// from racing with reading the log.
	deleted(t, f)
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "started no background task") {
		t.Errorf("log: %s", out)
	}
}

// Open WebUI has no legacy completions, so nothing is sent.
func TestChat_RefusesLegacyCompletions(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	_, err := newBackend(t, f, "").Chat(context.Background(), backend.ChatRequest{Endpoint: backend.Completions, Body: []byte(`{"model":"m","prompt":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "only /v1/chat/completions") || len(f.Requests()) != 0 {
		t.Errorf("got %v after %v", err, f.Keys())
	}
}

// The chat is deleted, archived or kept after each request, as configured.
func TestChat_CleansUpAsConfigured(t *testing.T) {
	cases := []struct{ setting, last string }{
		{"delete", "DELETE /api/v1/chats/chat-1"},
		{"archive", "POST /api/v1/chats/chat-1/archive"},
		{"keep", "GET /api/tasks/chat/chat-1"},
	}
	for _, c := range cases {
		t.Run(c.setting, func(t *testing.T) {
			f := testkit.NewOpenWebUI(t)
			if _, err := chat(t, newBackend(t, f, ", chats: "+c.setting), chatBody, false); err != nil {
				t.Fatal(err)
			}
			f.WaitFor(t, c.last, 1)
			if calls := f.ChatCalls(); calls[len(calls)-1] != c.last {
				t.Errorf("calls: %v", calls)
			}
		})
	}
}

// With passthroughAuth the chat flow acts as the client, while the state
// read uses the backend's own key.
func TestChat_PassthroughAuthActsAsTheClient(t *testing.T) {
	f := testkit.NewOpenWebUI(t, testkit.OllamaEntry("m", true))
	b := newBackend(t, f, ", passthroughAuth: true")
	b.LoadedModels(context.Background())
	if _, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(chatBody), Client: backend.Client{Authorization: "Bearer user"}}); err != nil {
		t.Fatal(err)
	}
	deleted(t, f)
	for _, r := range f.Requests() {
		want := "Bearer user"
		if r.Key() == "GET /api/models" {
			want = "Bearer svc"
		}
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s: got %q, want %q", r.Key(), got, want)
		}
	}
}

// A model with a run under way shows as starting until polling sees its
// first output, and a run that fails to start ends the count too.
func TestChat_CountsAsPendingUntilTheFirstOutput(t *testing.T) {
	f := testkit.NewOpenWebUI(t)
	steps := make(chan struct{})
	f.SetChat(testkit.Chat{Contents: []string{"", "hi"}, PollGate: steps})
	b := newBackend(t, f, ", stateCacheSeconds: 60")
	starting := func() bool { return stateOf(must(b.LoadedModels(context.Background())))["m"] == "starting" }
	done := make(chan error)
	go func() {
		_, err := chat(t, b, `{"model":"m","messages":[{"role":"user","content":"x"}]}`, false)
		done <- err
	}()
	steps <- struct{}{} // first poll: no output yet
	if !starting() {
		t.Error("before the first output: not starting")
	}
	steps <- struct{}{} // second poll: output
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starting() {
		t.Error("after the first output: still starting")
	}

	f.SetChat(testkit.Chat{StartStatus: http.StatusNotFound})
	if _, err := chat(t, b, `{"model":"m","messages":[{"role":"user","content":"x"}]}`, false); err != nil {
		t.Fatal(err)
	}
	if starting() {
		t.Error("after a refused start: still starting")
	}
}
