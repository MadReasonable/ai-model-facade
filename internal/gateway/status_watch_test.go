package gateway

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// followStatusStream opens GET /amf/config-status?follow with key and
// returns a function that waits for the next event holding want, skipping
// the others, and the function that closes the stream.
func followStatusStream(t *testing.T, a *adminLive, key string) (next func(want string) string, closeStream func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", a.URL+"/amf/config-status?follow", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d, Content-Type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
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
	next = func(want string) string {
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
	return next, func() { resp.Body.Close() }
}

// statusOf is the status in an event of the stream.
func statusOf(event string) gjson.Result {
	return gjson.Parse(strings.TrimPrefix(event, "data: "))
}

// The status carries the ETag GET /amf/config gives each copy, so a client
// can tell from it alone whether to read a copy again.
func TestStatus_ETagsAreTheConfigs(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	check := func(what string) {
		t.Helper()
		status := gjson.Parse(a.send(t, "GET", "/amf/config-status", "bobs-key", "").body)
		if draft := a.send(t, "GET", "/amf/config", "bobs-key", ""); status.Get("draft.etag").String() != draft.Header.Get("ETag") {
			t.Errorf("%s: draft.etag %s, GET /amf/config %s", what, status.Get("draft.etag"), draft.Header.Get("ETag"))
		}
		if live := a.send(t, "GET", "/amf/config?live", "bobs-key", ""); status.Get("live.etag").String() != live.Header.Get("ETag") {
			t.Errorf("%s: live.etag %s, GET /amf/config?live %s", what, status.Get("live.etag"), live.Header.Get("ETag"))
		}
	}
	check("at start")
	before := gjson.Get(a.send(t, "GET", "/amf/config-status", "bobs-key", "").body, "draft.etag").String()
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`), http.StatusOK, "edit")
	check("after an edit")
	if after := gjson.Get(a.send(t, "GET", "/amf/config-status", "bobs-key", "").body, "draft.etag").String(); after == before {
		t.Errorf("draft.etag stayed %s after an edit", after)
	}
}

// A status stream sends the status at once, then again after each change
// made through the gateway, and ends once a reload stops the running config
// letting the caller read the config.
func TestStatus_FollowSendsEachChange(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	next, closeStream := followStatusStream(t, a, "bobs-key")
	defer closeStream()

	first := statusOf(next(`"live":true`))
	if first.Get("caller.name").String() != "bob" || first.Get("caller.write").Bool() {
		t.Errorf("caller %s", first.Get("caller"))
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`), http.StatusOK, "edit")
	edited := statusOf(next(`"edited":true`))
	if edited.Get("draft.live").Bool() || edited.Get("draft.etag").String() == first.Get("draft.etag").String() {
		t.Errorf("after an edit: %s", edited.Get("draft"))
	}
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	applied := statusOf(next(`"source":"files","edited":true,"live":true`))
	if applied.Get("live.etag").String() != edited.Get("draft.etag").String() {
		t.Errorf("after an apply, live.etag %s, want the draft's %s", applied.Get("live.etag"), edited.Get("draft.etag"))
	}

	writeFile(t, a.main, strings.Replace(readFile(t, a.main), "    read: {allowedKeys: [bob]}\n", "", 1), 0o644)
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if e := next("event: end"); !strings.Contains(e, "no longer lets this client read the config") {
		t.Errorf("end: %s", e)
	}
}

// An edit to the files on the host passes nothing through the gateway, so
// the periodic check is what finds it.
func TestStatus_FollowFindsAHostEdit(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.live.watch.interval = 50 * time.Millisecond
	next, closeStream := followStatusStream(t, a, "bobs-key")
	defer closeStream()
	next(`"changedOnDisk":false`)
	writeFile(t, a.main, readFile(t, a.main)+"# a host edit\n", 0o644)
	if files := statusOf(next(`"changedOnDisk":true`)).Get("files.#.changedOnDisk").String(); files != "[true,false]" {
		t.Errorf("changedOnDisk %s, want the config file's only", files)
	}
}

// A reload that lets the caller change the config is sent, though the
// status reads the same, since the event says what the caller may do.
func TestStatus_FollowSendsWhatTheCallerMayDo(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	next, closeStream := followStatusStream(t, a, "bobs-key")
	defer closeStream()
	next(`"write":false`)
	writeFile(t, a.main, strings.Replace(readFile(t, a.main), "write: {allowedKeys: [alice]}", "write: {allowedKeys: [alice, bob]}", 1), 0o644)
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	next(`"write":true`)
}

// The check runs only while a stream is open, and EndStreams ends every
// stream, including one opened after it.
func TestStatus_CheckStopsWithTheLastStream(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	streams := func() (int, bool) {
		a.live.watch.mu.Lock()
		defer a.live.watch.mu.Unlock()
		return a.live.watch.streams, a.live.watch.poke != nil
	}
	next, closeStream := followStatusStream(t, a, "bobs-key")
	next(`"caller"`)
	if n, checking := streams(); n != 1 || !checking {
		t.Fatalf("with a stream open: %d streams, checking %t", n, checking)
	}
	closeStream()
	deadline := time.Now().Add(5 * time.Second)
	for n, checking := streams(); n != 0 || checking; n, checking = streams() {
		if time.Now().After(deadline) {
			t.Fatalf("after the stream closed: %d streams, checking %t", n, checking)
		}
		time.Sleep(10 * time.Millisecond)
	}

	next, closeStream = followStatusStream(t, a, "bobs-key")
	defer closeStream()
	next(`"caller"`)
	a.live.EndStreams()
	if e := next("event: end"); !strings.Contains(e, "shutting down") {
		t.Errorf("end: %s", e)
	}
	next, closeLate := followStatusStream(t, a, "bobs-key")
	defer closeLate()
	next("event: end")
}
