package testkit

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The live checks run against the real backends that dev-env/ runs, through
// its control service (dev-env/README.md), when AMF_LIVE names control's
// URL; without it, they skip. A backend's host port defaults to
// .env.example's and is overridden by the same variable, such as
// LLAMA_SWAP_PORT, as in dev-env/.env.

// LiveTimeout bounds a live check's waits for real loads.
const LiveTimeout = 3 * time.Minute

// LiveControl is the AMF_LIVE variable: control's URL, or "" for no live
// checks.
func LiveControl() string { return os.Getenv("AMF_LIVE") }

// LiveBackend is a real backend, which control knows by Name and the host
// reaches at BaseURL.
type LiveBackend struct {
	Control, Name, BaseURL string
	Key                    string // the API key its setup made: Open WebUI's
}

// StartLive starts the backend that control knows by name and the host
// reaches on port (portVar, else its default), unless it runs, and returns
// once control reports it ready with its setup done; without AMF_LIVE, it
// skips t.
func StartLive(t testing.TB, name, portVar, port string) *LiveBackend {
	t.Helper()
	control := LiveControl()
	if control == "" {
		t.Skip("set AMF_LIVE to dev-env's control URL, such as http://127.0.0.1:18090, to check the real backend")
	}
	if p := os.Getenv(portVar); p != "" {
		port = p
	}
	b := &LiveBackend{Control: control, Name: name, BaseURL: "http://127.0.0.1:" + port}
	b.call(t, http.MethodPost, "/start", nil)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var status struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
			Setup  struct {
				Ready bool   `json:"ready"`
				Key   string `json:"key"`
			} `json:"setup"`
		}
		b.call(t, http.MethodGet, "", &status)
		switch {
		case status.State == "ready" && status.Setup.Ready:
			b.Key = status.Setup.Key
			return b
		case status.State == "failed":
			t.Fatalf("backend '%s' failed to start through control: %s", name, status.Detail)
		case time.Now().After(deadline):
			t.Fatalf("backend '%s' wasn't ready through control within 5 minutes: %s, %s", name, status.State, status.Detail)
		}
		time.Sleep(time.Second)
	}
}

// LiveModel is a model as control's GET .../models reports it, its state in
// its backend's own words.
type LiveModel struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Models returns the backend's models through control, or nil while control
// can't list them.
func (b *LiveBackend) Models(t testing.TB) []LiveModel {
	t.Helper()
	resp, err := http.Get(b.Control + "/api/backends/" + b.Name + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var models []LiveModel
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&models) != nil {
		return nil
	}
	return models
}

// Reset unloads, through control, every model the backend reports in any
// state but unloaded; control answers once the backend no longer reports it
// loaded.
func (b *LiveBackend) Reset(t testing.TB) {
	t.Helper()
	for _, m := range b.Models(t) {
		if m.State != "unloaded" {
			b.call(t, http.MethodPost, "/models/"+m.ID+"/unload", nil)
		}
	}
}

// Load loads model through control and waits until control reports it
// loaded.
func (b *LiveBackend) Load(t testing.TB, model string) {
	t.Helper()
	b.call(t, http.MethodPost, "/models/"+model+"/load", nil)
	deadline := time.Now().Add(LiveTimeout)
	for time.Now().Before(deadline) {
		if b.Loaded(t, model) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("model '%s' of '%s' wasn't loaded within %v", model, b.Name, LiveTimeout)
}

// Loaded reports whether control reports model loaded.
func (b *LiveBackend) Loaded(t testing.TB, model string) bool {
	t.Helper()
	for _, m := range b.Models(t) {
		if m.ID == model && (m.State == "loaded" || m.State == "ready") {
			return true
		}
	}
	return false
}

// call sends a request to the backend's resource in control's API and
// decodes the answer into out, if out isn't nil.
func (b *LiveBackend) call(t testing.TB, method, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(method, b.Control+"/api/backends/"+b.Name+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("control at %s: %v", b.Control, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("control: %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatal(fmt.Errorf("control: %s %s: %w", method, path, err))
		}
	}
}
