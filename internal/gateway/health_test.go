package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The health URL for a listen address: a wildcard host is checked on
// loopback, and an address without a port is an error.
func TestHealth_URLForAListenAddress(t *testing.T) {
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

// /health answers without an API key and without contacting a backend;
// CheckHealth accepts it and rejects anything else.
func TestHealth_EndpointNeedsNoKeyOrBackend(t *testing.T) {
	swap := newSwap(t)
	gw := startTask(t, swap)
	if r := gw.do(t, http.MethodGet, "/health", "", ""); r.StatusCode != http.StatusOK || len(swap.Requests()) != 0 {
		t.Errorf("got %d; backend saw %v", r.StatusCode, swap.Keys())
	}
	if err := CheckHealth(context.Background(), gw.URL+"/health"); err != nil {
		t.Error(err)
	}
	if err := CheckHealth(context.Background(), gw.URL+"/nope"); err == nil {
		t.Error("expected an error for a non-200 answer")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	if err := CheckHealth(context.Background(), down.URL+"/health"); err == nil {
		t.Error("expected an error when nothing is listening")
	}
}
