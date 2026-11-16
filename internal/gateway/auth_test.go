package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/config"
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

// A key entry may give the key's hash instead of the key; a client sending
// the key is matched and named the same either way.
func TestAuth_AcceptsAKeyByItsHash(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap, "{name: openWebUi, key: secret}", "{name: openWebUi, hash: "+config.HashKey("secret")+"}")
	if r := gw.chat(t, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", config.HashKey("secret"), `{"model":"task","messages":[]}`); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("the hash itself as a key: got %d", r.StatusCode)
	}
	if out := gw.logs.String(); !strings.Contains(out, "client=openWebUi") {
		t.Errorf("log:\n%s", out)
	}
}

// One key under two names loads with a warning in the log, and requests with
// it are named after the first entry.
func TestAuth_AKeyListedTwiceIsNamedForTheFirstEntry(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap, "{name: openWebUi, key: secret}", "{name: openWebUi, key: secret}\n    - {name: other, key: secret}")
	gw.chat(t, `{"model":"task","messages":[]}`)
	out := gw.logs.String()
	if !strings.Contains(out, "key of 'other' is the same as 'openWebUi'") || !strings.Contains(out, "client=openWebUi") {
		t.Errorf("log:\n%s", out)
	}
}

// keyMap sends each caller's request to a backend with the key the map gives
// its name or group, so a backend with accounts of its own, such as Open
// WebUI, sees each caller as themselves without their gateway key.
func TestAuth_SendsTheKeyMapsKeyForEachCaller(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap,
		"    - {name: openWebUi, key: secret}", "    - {name: openWebUi, key: secret}\n    - {name: alice, key: alices-key}\n    - {name: bob, key: bobs-key}\n  groups: {family: [alice, bob]}",
		`llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}`, `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc, keyMap: {alice: alices-swap-key, family: family-swap-key}}`,
	)
	for key, want := range map[string]string{"alices-key": "Bearer alices-swap-key", "bobs-key": "Bearer family-swap-key", "secret": "Bearer svc"} {
		gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"task","messages":[]}`)
		if req, _ := swap.Last("POST /v1/chat/completions"); req.Header.Get("Authorization") != want {
			t.Errorf("%s: backend got %q, want %q", key, req.Header.Get("Authorization"), want)
		}
	}
	if out := gw.logs.String(); strings.Contains(out, "swap-key") {
		t.Errorf("log leaks a mapped key:\n%s", out)
	}
}

// A key in several groups with keyMap entries is sent the key of the entry
// listed first, whatever order the key's groups are in, and a key's own entry
// comes before its groups' wherever it is written.
func TestAuth_SendsTheFirstMappedGroupsKey(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap,
		"    - {name: openWebUi, key: secret}", "    - {name: openWebUi, key: secret}\n    - {name: carol, key: carols-key}\n    - {name: dave, key: daves-key}\n  groups: {adults: [carol, dave], family: [carol, dave]}",
		`llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}`, `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc, keyMap: {family: family-swap-key, adults: adults-swap-key, dave: daves-swap-key}}`,
	)
	for key, want := range map[string]string{"carols-key": "Bearer family-swap-key", "daves-key": "Bearer daves-swap-key"} {
		if r := gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d %s", key, r.StatusCode, r.body)
		}
		if req, _ := swap.Last("POST /v1/chat/completions"); req.Header.Get("Authorization") != want {
			t.Errorf("%s: backend got %q, want %q", key, req.Header.Get("Authorization"), want)
		}
	}
}

// Secrets from a secrets file work as the config's own would: a key listed
// there is accepted, and a backend's apiKey and keyMap there reach the
// backend.
func TestAuth_UsesASecretsFile(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	secrets := `
server: {apiKeys: [{name: alice, key: alices-key}]}
backends: {llamaSwap: {apiKey: svc, keyMap: {alice: alices-swap-key}}}
`
	cfg, err := config.ParseWithSecrets([]byte(taskConfig), []byte(secrets), func(n string) (string, bool) { return swap.URL, n == "SWAP_URL" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	t.Cleanup(srv.Close)
	gw := &gateway{Server: srv, g: g}
	swap.Reset()
	for key, want := range map[string]string{"alices-key": "Bearer alices-swap-key", "secret": "Bearer svc"} {
		if r := gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d %s", key, r.StatusCode, r.body)
		}
		if req, _ := swap.Last("POST /v1/chat/completions"); req.Header.Get("Authorization") != want {
			t.Errorf("%s: backend got %q, want %q", key, req.Header.Get("Authorization"), want)
		}
	}
}

// A secrets file's keyMap entries come after the config's, in the file's
// order, so a key in two mapped groups is sent the config's group entry over
// the secrets file's, and between two in the secrets file, the one it lists
// first, though their names sort the other way.
func TestAuth_OrdersTheSecretsFilesKeyMapAfterTheConfigs(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	src := edit(t, taskConfig,
		"    - {name: openWebUi, key: secret}\n", "    - {name: openWebUi, key: secret}\n    - {name: carol, key: carols-key}\n    - {name: erin, key: erins-key}\n  groups: {family: [carol], adults: [carol], zeta: [erin], alpha: [erin]}\n",
		`llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}`, `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", keyMap: {family: family-swap-key}}`,
	)
	secrets := "backends: {llamaSwap: {apiKey: svc, keyMap: {adults: adults-swap-key, zeta: zeta-swap-key, alpha: alpha-swap-key}}}"
	cfg, err := config.ParseWithSecrets([]byte(src), []byte(secrets), func(n string) (string, bool) { return swap.URL, n == "SWAP_URL" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	t.Cleanup(srv.Close)
	gw := &gateway{Server: srv, g: g}
	for key, want := range map[string]string{"carols-key": "Bearer family-swap-key", "erins-key": "Bearer zeta-swap-key"} {
		if r := gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d %s", key, r.StatusCode, r.body)
		}
		if req, _ := swap.Last("POST /v1/chat/completions"); req.Header.Get("Authorization") != want {
			t.Errorf("%s: backend got %q, want %q", key, req.Header.Get("Authorization"), want)
		}
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
