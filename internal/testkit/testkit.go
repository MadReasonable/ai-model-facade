// Package testkit holds what the tests of several packages share: fake
// llama-swap, Ollama and Open WebUI servers, and the tools to wait for what
// they receive.
//
// The fakes speak the parts of each backend's API the gateway uses, as the
// versions it was verified against do (llama-swap v263, Open WebUI 0.12.3),
// so a test sets up a backend's state and behaviour rather than its wire
// format. Every fake records the requests it receives in a Recorder, and a
// test waits for a request with WaitFor instead of sleeping; a load can be
// held at a Gate until the test opens it. Every wait is bounded by Timeout,
// so a test that waits for something that never happens fails instead of
// hanging.
package testkit

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Timeout bounds every wait in the testkit.
const Timeout = 5 * time.Second

// Request is one request a fake received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// Key is the request's method and path, such as "GET /running", the form
// every Recorder method takes.
func (r Request) Key() string {
	return r.Method + " " + r.Path
}

// Recorder keeps the requests a fake received, in order.
type Recorder struct {
	mu      sync.Mutex
	reqs    []Request
	changed chan struct{} // closed on the next request; nil until someone waits
}

// record reads r's body, keeps the request and wakes anyone waiting, and
// returns the body for the handler.
func (rec *Recorder) record(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(body)})
	if rec.changed != nil {
		close(rec.changed)
		rec.changed = nil
	}
	rec.mu.Unlock()
	return body
}

// Requests returns every request received so far.
func (rec *Recorder) Requests() []Request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.reqs)
}

// Keys returns the key of every request received so far, in order.
func (rec *Recorder) Keys() []string {
	var keys []string
	for _, r := range rec.Requests() {
		keys = append(keys, r.Key())
	}
	return keys
}

// Count returns how many requests with key were received.
func (rec *Recorder) Count(key string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.countLocked(key)
}

func (rec *Recorder) countLocked(key string) int {
	n := 0
	for _, r := range rec.reqs {
		if r.Key() == key {
			n++
		}
	}
	return n
}

// Last returns the last request with key, and whether there was one.
func (rec *Recorder) Last(key string) (Request, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := len(rec.reqs) - 1; i >= 0; i-- {
		if rec.reqs[i].Key() == key {
			return rec.reqs[i], true
		}
	}
	return Request{}, false
}

// Reset forgets the requests received so far, such as those a gateway makes
// while it starts.
func (rec *Recorder) Reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = nil
}

// WaitFor waits until n requests with key have been received, and fails t
// if that takes longer than Timeout.
func (rec *Recorder) WaitFor(t testing.TB, key string, n int) {
	t.Helper()
	deadline := time.After(Timeout)
	for {
		rec.mu.Lock()
		got := rec.countLocked(key)
		if rec.changed == nil {
			rec.changed = make(chan struct{})
		}
		changed := rec.changed
		rec.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("waited %v for %d requests %s, got %d", Timeout, n, key, got)
		}
	}
}

// Gate holds the handlers that wait on it until Open is called, such as a
// fake's model load.
type Gate struct {
	once sync.Once
	ch   chan struct{}
}

func NewGate() *Gate {
	return &Gate{ch: make(chan struct{})}
}

// Open lets every waiting handler through, and every later one at once. It
// may be called more than once.
func (g *Gate) Open() {
	g.once.Do(func() { close(g.ch) })
}

// wait returns once the gate is open, true, or once ctx ends, false. A nil
// gate is always open.
func (g *Gate) wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	select {
	case <-g.ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// Entry parses one backend's config entry from YAML, for an adapter's
// constructor: Entry(t, "llamaSwap", `{type: llamaSwap, baseUrl: "..."}`).
func Entry(t testing.TB, name, src string) config.Entry {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	node := doc.Content[0]
	entry := config.Entry{Name: name, Node: node}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "type" {
			entry.Type = node.Content[i+1].Value
		}
	}
	return entry
}
