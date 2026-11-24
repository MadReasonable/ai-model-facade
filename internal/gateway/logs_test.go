package gateway

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// GET /amf/logs returns the log's lines as JSON lines, or stderr's text with
// ?format=text, filtered by level and limit, for a key in server.adminApi.read.
func TestLogs_Read(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`), http.StatusOK, "edit")

	r := a.send(t, "GET", "/amf/logs", "bobs-key", "")
	a.expect(t, r, http.StatusOK, "read")
	lines := strings.Split(strings.TrimSpace(r.body), "\n")
	last := lines[len(lines)-1]
	if r.Header.Get("Content-Type") != "application/x-ndjson" || gjson.Get(last, "msg").String() != "draft edited" || gjson.Get(last, "client").String() != "alice" {
		t.Errorf("JSON lines: %s\n%s", r.Header.Get("Content-Type"), r.body)
	}
	if r := a.send(t, "GET", "/amf/logs?format=text&limit=1", "bobs-key", ""); !strings.Contains(r.body, `msg="draft edited" client=alice`) || strings.Count(r.body, "\n") != 1 {
		t.Errorf("text: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/logs", "bobs-key", "", "Accept", "text/plain"); !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("Accept: text/plain: %s", r.Header.Get("Content-Type"))
	}
	if r := a.send(t, "GET", "/amf/logs?level=error", "bobs-key", ""); r.body != "" {
		t.Errorf("errors only: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/logs?since=1m", "bobs-key", ""); !strings.Contains(r.body, "draft edited") {
		t.Errorf("since 1m: %s", r.body)
	}
	for _, query := range []string{"level=loud", "limit=0", "since=yesterday", "format=xml"} {
		a.expect(t, a.send(t, "GET", "/amf/logs?"+query, "bobs-key", ""), http.StatusBadRequest, query)
	}
	a.expect(t, a.send(t, "GET", "/amf/logs", "secret", ""), http.StatusForbidden, "read by a client")
}

// With server.adminApi.logs.maxBytes 0 the log keeps no history: a read is empty, and
// says it is streaming only.
func TestLogs_StreamingOnly(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, strings.Replace(adminMain, "  adminApi:\n", "  adminApi:\n    logs: {maxBytes: 0}\n", 1), adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`), http.StatusOK, "edit")
	if r := a.send(t, "GET", "/amf/logs", "bobs-key", ""); r.StatusCode != http.StatusOK || r.body != "" || r.Header.Get("X-Gateway-Logs") != "streaming-only" {
		t.Errorf("got %d %q, header %q", r.StatusCode, r.body, r.Header.Get("X-Gateway-Logs"))
	}
}

// A follow stream sends the lines already kept, then each new one, and ends
// once a reload stops the running config letting the caller read the log.
func TestLogs_FollowEndsWhenReadIsRevoked(t *testing.T) {
	keepAliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = 30 * time.Second })
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"first"`), http.StatusOK, "edit")

	req, _ := http.NewRequest("GET", a.URL+"/amf/logs?follow&format=text", nil)
	req.Header.Set("Authorization", "Bearer bobs-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type %s", resp.Header.Get("Content-Type"))
	}
	events := make(chan string, 100)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		var event []string
		for scanner.Scan() {
			if scanner.Text() == "" {
				events <- strings.Join(event, "\n")
				event = nil
				continue
			}
			event = append(event, scanner.Text())
		}
	}()
	next := func(want string) string {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatalf("stream closed before %q", want)
				}
				if strings.Contains(e, want) {
					return e
				}
			case <-deadline:
				t.Fatalf("no event with %q", want)
			}
		}
	}
	next(`path=models.task.description`)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/helper", "alices-key", `{"backend": "llamaSwap", "candidates": [{"model": "m", "loadIfNone": true}]}`), http.StatusCreated, "edit")
	next(`path=models.helper`)

	writeFile(t, a.main, strings.Replace(readFile(t, a.main), "    read: {allowedKeys: [bob]}\n", "", 1), 0o644)
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if e := next("event: end"); !strings.Contains(e, "no longer lets this client read") {
		t.Errorf("end: %s", e)
	}
	if _, err := os.Stat(a.main); err != nil {
		t.Fatal(err)
	}
}

// With no admin list the log endpoint is off, as the other read endpoints
// are.
func TestLogs_OffWithoutAdmins(t *testing.T) {
	swap := newSwap(t)
	noAdmins := strings.Replace(adminMain, "  adminApi:\n    read: {allowedKeys: [bob]}\n    write: {allowedKeys: [alice]}\n", "", 1)
	a := startAdmin(t, swap.URL, noAdmins, adminSecrets)
	if r := a.send(t, "GET", "/amf/logs", "alices-key", ""); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "server.adminApi.read") {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// slowClient is a response writer whose client stopped reading: each write
// waits until released.
type slowClient struct {
	header   http.Header
	entered  chan struct{}
	released chan struct{}
	once     sync.Once
	mu       sync.Mutex
	body     bytes.Buffer
}

func (c *slowClient) Header() http.Header { return c.header }
func (c *slowClient) WriteHeader(int)     {}
func (c *slowClient) Flush()              {}

func (c *slowClient) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.released
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.Write(p)
}

func (c *slowClient) received() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.String()
}

// A stream whose client falls behind drops the records its queue can't hold
// and says how many before the next one, rather than holding up logging.
func TestLogs_SlowStreamSaysWhatItDropped(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	log := slog.New(a.logs.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/amf/logs?follow&format=text", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer bobs-key")
	client := &slowClient{header: http.Header{}, entered: make(chan struct{}), released: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.live.ServeHTTP(client, req)
	}()
	for waiting := true; waiting; {
		log.Info("first")
		select {
		case <-client.entered:
			waiting = false
		case <-time.After(10 * time.Millisecond):
		}
	}
	for range 400 {
		log.Info("flood") // returns at once, though the client reads nothing
	}
	close(client.released)
	deadline := time.After(5 * time.Second)
	for !strings.Contains(client.received(), "after the flood") {
		log.Info("after the flood") // dropped too until the queue has room
		select {
		case <-deadline:
			t.Fatalf("no line after the flood:\n%s", client.received())
		case <-time.After(10 * time.Millisecond):
		}
	}
	got := client.received()
	i := strings.Index(got, "event: dropped\ndata: {\"dropped\":")
	if i < 0 || i > strings.Index(got, "after the flood") {
		t.Errorf("no dropped event before the next line:\n%s", got[max(0, len(got)-600):])
	}
	cancel()
	<-done
}

// A selection line's duration is a number of whole milliseconds, in
// durationMs, so the unit is in the key in both the text and the JSON log,
// and a JSON reader gets a number.
func TestLogs_SelectionDurationIsMilliseconds(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "POST", "/v1/chat/completions", "secret", `{"model": "task", "messages": [{"role": "user", "content": "hi"}]}`), http.StatusOK, "chat")
	var duration gjson.Result
	for _, line := range strings.Split(a.send(t, "GET", "/amf/logs?format=json", "bobs-key", "").body, "\n") {
		if gjson.Get(line, "msg").String() == "selection" {
			duration = gjson.Get(line, "durationMs")
		}
	}
	if duration.Type != gjson.Number || duration.Raw != strconv.FormatInt(duration.Int(), 10) {
		t.Errorf("durationMs: %q", duration.Raw)
	}
}
