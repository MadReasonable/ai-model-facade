package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// A key that holds a slash is one path segment sent as %2F.
func TestAdmin_KeyWithASlash(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	facade := `{"backend": "llamaSwap", "candidates": [{"model": "m", "loadIfNone": true}]}`
	a.expect(t, a.send(t, "PUT", "/amf/config/models/team%2Fhelper", "alices-key", facade), http.StatusCreated, "a facade named team/helper")
	if r := a.send(t, "GET", "/amf/config/models/team%2Fhelper/backend", "bobs-key", ""); r.StatusCode != http.StatusOK || r.body != "\"llamaSwap\"\n" {
		t.Errorf("read: got %d %s", r.StatusCode, r.body)
	}
	if r := a.send(t, "GET", "/amf/config/models", "bobs-key", ""); !gjson.Get(r.body, `team\/helper`).Exists() {
		t.Errorf("models: %s", r.body)
	}
}

// DELETE honours If-Match; a PUT body that is neither JSON nor YAML is
// refused; a PUT of what GET shows for the entire draft leaves it as it was;
// a backend entry wrong by itself is refused when it is written.
func TestAdmin_EditEdges(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	helper := `{"backend": "llamaSwap", "candidates": [{"model": "m", "loadIfNone": true}]}`
	a.expect(t, a.send(t, "PUT", "/amf/config/models/helper", "alices-key", helper), http.StatusCreated, "add")
	tag := a.send(t, "GET", "/amf/config/models/helper", "bobs-key", "").Header.Get("ETag")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/helper/description", "alices-key", `"changed"`), http.StatusCreated, "edit")
	a.expect(t, a.send(t, "DELETE", "/amf/config/models/helper", "alices-key", "", "If-Match", tag), http.StatusPreconditionFailed, "a delete with a stale If-Match")
	tag = a.send(t, "GET", "/amf/config/models/helper", "bobs-key", "").Header.Get("ETag")
	a.expect(t, a.send(t, "DELETE", "/amf/config/models/helper", "alices-key", "", "If-Match", tag), http.StatusOK, "a delete with the current If-Match")

	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", "x", "Content-Type", "text/plain"), http.StatusUnsupportedMediaType, "a text body")

	r := a.send(t, "GET", "/amf/config", "bobs-key", "", "Accept", "application/yaml")
	a.expect(t, a.send(t, "PUT", "/amf/config", "alices-key", r.body, "Content-Type", "application/yaml"), http.StatusOK, "a PUT of the entire draft")
	if after := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("ETag"); after != r.Header.Get("ETag") {
		t.Errorf("a PUT of what GET showed changed the draft: ETag %s, was %s", after, r.Header.Get("ETag"))
	}

	if r := a.send(t, "PUT", "/amf/config/backends/other", "alices-key", `{"type": "nope"}`); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "unknown type 'nope'") {
		t.Errorf("a wrong backend: got %d %s", r.StatusCode, r.body)
	}
}

// Edits, applies, reloads, reverts and reads at once each see a whole copy,
// and leave the draft, the running config and the files consistent; run
// with -race, this checks the locks around them.
func TestAdmin_ConcurrentChanges(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	var wg sync.WaitGroup
	statuses := make(chan string, 200)
	run := func(method, path, body string) {
		defer wg.Done()
		for range 10 {
			r := a.send(t, method, path, "alices-key", body)
			if r.StatusCode >= 500 {
				statuses <- method + " " + path + ": " + r.body
			}
		}
	}
	wg.Add(6)
	go run("PUT", "/amf/config/models/task/description", `"concurrent"`)
	go run("POST", "/amf/config-apply", "")
	go run("POST", "/amf/reload", "")
	go run("POST", "/amf/config-revert", "")
	go run("GET", "/amf/config", "")
	go run("GET", "/amf/config-status", "")
	wg.Wait()
	close(statuses)
	for s := range statuses {
		t.Error(s)
	}
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "a last reload")
	status := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body
	if !gjson.Get(status, "draft.live").Bool() || !gjson.Get(status, "live.saved").Bool() || !gjson.Get(status, "draft.saved").Bool() {
		t.Errorf("after a reload, the copies differ: %s", status)
	}
}

// Reading the copies, and changing only the draft, never wait on a reload or
// an apply, which hold the build lock for as long as listing a slow backend
// takes.
func TestAdmin_ReadsDontWaitOnABuild(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.live.mu.Lock() // as a build in progress holds it
	defer a.live.mu.Unlock()
	done := make(chan string)
	go func() {
		for _, req := range []struct{ method, path, body string }{
			{"GET", "/amf/config", ""},
			{"GET", "/amf/config/models/task?live", ""},
			{"GET", "/amf/config-status", ""},
			{"PUT", "/amf/config/models/task/description", `"during a build"`},
			{"POST", "/amf/config-revert", ""},
			{"POST", "/amf/config-load", ""},
		} {
			if r := a.send(t, req.method, req.path, "alices-key", req.body); r.StatusCode != http.StatusOK {
				done <- req.method + " " + req.path + ": " + r.body
				return
			}
		}
		done <- ""
	}()
	select {
	case failed := <-done:
		if failed != "" {
			t.Error(failed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read or a draft change waited on the build lock")
	}
}
