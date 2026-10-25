package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// With apiKeys set, a request needs a listed key: a missing or unlisted one
// gets 401, on the completion endpoints and the model list alike.
func TestAuth_RequiresAListedKey(t *testing.T) {
	gw := startTask(t, newSwap(t))
	cases := []struct{ method, path, key string }{
		{http.MethodPost, "/v1/chat/completions", ""},
		{http.MethodPost, "/v1/chat/completions", "nope"},
		{http.MethodPost, "/v1/completions", "nope"},
		{http.MethodGet, "/v1/models", ""},
		{http.MethodGet, "/v1/models/task", ""},
	}
	for _, c := range cases {
		if r := gw.do(t, c.method, c.path, c.key, `{"model":"task"}`); r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.body, `"error"`) {
			t.Errorf("%s %s with key %q: got %d %s", c.method, c.path, c.key, r.StatusCode, r.body)
		}
	}
}

// With unknownKeys: allow, any key is accepted: a listed one is logged by its
// name, an unlisted one as unknown with a fingerprint, and no key itself is
// ever logged.
func TestAuth_AllowsUnlistedKeysWhenConfigured(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap, "logging:", "  unknownKeys: allow\nlogging:")
	for _, key := range []string{"secret", "some-other-users-key", ""} {
		if r := gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusOK {
			t.Errorf("key %q: got %d %s", key, r.StatusCode, r.body)
		}
	}
	out := gw.logs.String()
	sum := sha256.Sum256([]byte("some-other-users-key"))
	if !strings.Contains(out, "client=openWebUi") || !strings.Contains(out, "client=unknown fingerprint="+hex.EncodeToString(sum[:4])) {
		t.Errorf("log:\n%s", out)
	}
	if strings.Contains(out, "some-other-users-key") || strings.Contains(out, "secret") {
		t.Errorf("log leaks a key:\n%s", out)
	}
}

func TestAuth_RejectsAnUnknownUnknownKeysValue(t *testing.T) {
	src := edit(t, taskConfig, "logging:", "  unknownKeys: maybe\nlogging:")
	if _, err := parseConfigErr(src); err == nil || !strings.Contains(err.Error(), "unknownKeys") {
		t.Errorf("got %v", err)
	}
}

// With passthroughAuth, the client's own key reaches the backend through the
// gateway; without it, the backend's apiKey does.
func TestAuth_ForwardsTheKeyAsTheBackendIsConfigured(t *testing.T) {
	cases := []struct{ name, backend, want string }{
		{"apiKey", `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc}`, "Bearer svc"},
		{"passthrough", `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc, passthroughAuth: true}`, "Bearer secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swap := newSwap(t)
			swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
			gw := startTask(t, swap, `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}`, c.backend)
			gw.chat(t, `{"model":"task","messages":[]}`)
			if req, _ := swap.Last("POST /v1/chat/completions"); req.Header.Get("Authorization") != c.want {
				t.Errorf("backend got %q, want %q", req.Header.Get("Authorization"), c.want)
			}
		})
	}
}
