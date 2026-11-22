package gateway

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// followModelsStream opens GET /amf/models?follow with key and returns a
// function that waits for the next event holding want, skipping the others,
// and the function that closes the stream.
func followModelsStream(t *testing.T, a *adminLive, key string) (next func(want string) string, closeStream func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", a.URL+"/amf/models?follow", nil)
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

// stateReads counts the state reads the fake llama-swap has answered.
func stateReads(swap *testkit.LlamaSwap) int {
	n := 0
	for _, r := range swap.Requests() {
		if r.Path == "/running" {
			n++
		}
	}
	return n
}

// A models stream sends the list at once, then again after a periodic check
// finds a change no request made, such as a load another client started.
func TestModels_FollowFindsALoadElsewhere(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.live.models.interval = 50 * time.Millisecond
	next, closeStream := followModelsStream(t, a, "secret")
	defer closeStream()
	next(`"current":{"model":"qwen2.5-coder-3b:q8_0"`)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	next(`"current":{"model":"qwen3.6-35b-a3b:q8_0"`)
}

// A load the gateway starts shows at once, without waiting for the periodic
// check: the backend's pending count tells the stream, both when the
// request is sent and when the load ends.
func TestModels_FollowShowsALoadTheGatewayStarts(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.live.models.interval, a.live.models.gap = time.Hour, 10*time.Millisecond
	next, closeStream := followModelsStream(t, a, "secret")
	defer closeStream()
	next(`"state":"not loaded"`)

	gate := swap.HoldLoads()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.send(t, "POST", "/v1/chat/completions", "secret", `{"model": "task", "messages": [{"role": "user", "content": "hi"}]}`)
	}()
	next(`"model":"qwen2.5-coder-3b:q8_0","backend":"llamaSwap","loadIfNone":true,"state":"starting"`)
	gate.Open()
	<-done
	next(`"model":"qwen2.5-coder-3b:q8_0","backend":"llamaSwap","loadIfNone":true,"state":"loaded"`)
}

// Two streams list under one check, so the backend's state is read once for
// both, and each event says only what its caller may use.
func TestModels_StreamsShareOneRead(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.live.models.interval = time.Hour
	first, closeFirst := followModelsStream(t, a, "secret")
	defer closeFirst()
	second, closeSecond := followModelsStream(t, a, "alices-key")
	defer closeSecond()
	first(`"models"`)
	second(`"models"`)

	before := stateReads(swap)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	a.live.modelsChanged()
	first(`"current":{"model":"qwen3.6-35b-a3b:q8_0"`)
	second(`"current":{"model":"qwen3.6-35b-a3b:q8_0"`)
	if n := stateReads(swap) - before; n != 1 {
		t.Errorf("%d state reads for two streams, want 1", n)
	}
}

// A stream ends once a reload stops the gateway accepting the caller's key,
// and the check stops with the last stream.
func TestModels_FollowEndsWhenTheKeyGoes(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	streams := func() (int, bool) {
		a.live.models.mu.Lock()
		defer a.live.models.mu.Unlock()
		return a.live.models.streams, a.live.models.poke != nil
	}
	next, closeStream := followModelsStream(t, a, "bobs-key")
	defer closeStream()
	next(`"models"`)
	if n, checking := streams(); n != 1 || !checking {
		t.Fatalf("with a stream open: %d streams, checking %t", n, checking)
	}
	writeFile(t, a.secrets, strings.Replace(adminSecrets, "    - {name: bob, key: bobs-key}\n", "", 1), 0o600)
	writeFile(t, a.main, strings.Replace(readFile(t, a.main), "    read: {allowedKeys: [bob]}\n", "", 1), 0o644)
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if e := next("event: end"); !strings.Contains(e, "no longer accepts this client's key") {
		t.Errorf("end: %s", e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n, checking := streams(); n != 0 || checking; n, checking = streams() {
		if time.Now().After(deadline) {
			t.Fatalf("after the stream ended: %d streams, checking %t", n, checking)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A gateway served without a Live, through Gateway.Handler, has no check
// for a stream to list under, so ?follow is refused.
func TestModels_FollowNeedsALive(t *testing.T) {
	swap := newSwap(t)
	gw := startTask(t, swap)
	if r := gw.do(t, "GET", "/amf/models?follow", "secret", ""); r.StatusCode != http.StatusBadRequest || !strings.Contains(r.body, "needs a gateway run by serve") {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// EndStreams ends the models streams with the status streams.
func TestModels_EndStreams(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	next, closeStream := followModelsStream(t, a, "secret")
	defer closeStream()
	next(`"models"`)
	a.live.EndStreams()
	if e := next("event: end"); !strings.Contains(e, "shutting down") {
		t.Errorf("end: %s", e)
	}
}
