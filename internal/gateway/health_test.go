package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8088":   "http://127.0.0.1:8088/health",
		":8088":          "http://127.0.0.1:8088/health",
		"[::]:8088":      "http://[::1]:8088/health",
		"127.0.0.1:9000": "http://127.0.0.1:9000/health",
		"gateway:8088":   "http://gateway:8088/health",
	}
	for listen, want := range cases {
		if got, err := HealthURL(listen); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := HealthURL("8088"); err == nil {
		t.Error("expected an error for an address without a port")
	}
}

// /health answers without an API key and without contacting the backend;
// CheckHealth accepts it and rejects anything else.
func TestHealthEndpoint(t *testing.T) {
	swap := &fakeSwap{}
	srv := newGateway(t, swap)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d", resp.StatusCode)
	}
	if err := CheckHealth(context.Background(), srv.URL+"/health"); err != nil {
		t.Error(err)
	}
	if err := CheckHealth(context.Background(), srv.URL+"/nope"); err == nil {
		t.Error("expected an error for a non-200 answer")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	if err := CheckHealth(context.Background(), down.URL+"/health"); err == nil {
		t.Error("expected an error when nothing is listening")
	}
}

func TestGetModel(t *testing.T) {
	srv := newGateway(t, &fakeSwap{})
	get := func(path, key string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, body := get("/v1/models/task", "secret"); status != http.StatusOK || !strings.Contains(body, `"id":"task"`) || !strings.Contains(body, `"object":"model"`) {
		t.Errorf("got %d %s", status, body)
	}
	if status, body := get("/v1/models/nope", "secret"); status != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Errorf("got %d %s", status, body)
	}
	if status, _ := get("/v1/models/task", ""); status != http.StatusUnauthorized {
		t.Errorf("without a key: got %d", status)
	}
}

// `current` reports where a request would go now: a loaded model, the
// loadIfNone fallback, or nothing.
func TestModelCurrent(t *testing.T) {
	cases := []struct {
		name   string
		loaded []string
		cfg    func(string) string
		want   string
	}{
		{"loaded", []string{"qwen3.6-35b-a3b:q8_0"}, nil, `"current":{"model":"qwen3.6-35b-a3b:q8_0","reason":"loaded"}`},
		{"would load", nil, nil, `"current":{"model":"qwen2.5-coder-3b:q8_0","reason":"loadIfNone"}`},
		{"none", nil, func(c string) string { return strings.Replace(c, "        loadIfNone: true\n", "", 1) }, `"current":{"reason":"no candidate loaded"}`},
	}
	for _, c := range cases {
		cfg := testConfig
		if c.cfg != nil {
			cfg = c.cfg(cfg)
		}
		srv, _ := buildGateway(t, &fakeSwap{loaded: c.loaded}, cfg, discard)
		for _, path := range []string{"/v1/models", "/v1/models/task"} {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), c.want) {
				t.Errorf("%s %s: got %s, want %s", c.name, path, body, c.want)
			}
		}
	}
}

// If the backend's state can't be read, the listing still succeeds, without `current`.
func TestModelCurrentOmittedWhenStateUnavailable(t *testing.T) {
	swap := &fakeSwap{}
	srv, _ := buildGateway(t, swap, testConfig, discard)
	swap.mu.Lock()
	swap.failRunning = true
	swap.mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models/task", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "current") || !strings.Contains(string(body), `"id":"task"`) {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
}
