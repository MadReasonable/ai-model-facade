package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

func parseTestConfig(t *testing.T, src, swapURL string) *config.Config {
	t.Helper()
	vars := map[string]string{"GATEWAY_KEY": "secret", "SWAP_URL": swapURL}
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func modelIDs(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var ids []string
	for _, part := range strings.Split(string(b), `"id":"`)[1:] {
		ids = append(ids, part[:strings.Index(part, `"`)])
	}
	return strings.Join(ids, ",")
}

func TestLiveReload(t *testing.T) {
	backendSrv := httptest.NewServer(&fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}})
	defer backendSrv.Close()
	g, err := Build(context.Background(), parseTestConfig(t, testConfig, backendSrv.URL), discard)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(g, discard)
	srv := httptest.NewServer(live)
	defer srv.Close()
	if got := modelIDs(t, srv); got != "task" {
		t.Fatalf("before: %s", got)
	}

	// A valid change is swapped in.
	renamed := strings.Replace(testConfig, "  task:\n", "  renamed:\n", 1)
	if _, err := live.Reload(context.Background(), parseTestConfig(t, renamed, backendSrv.URL)); err != nil {
		t.Fatal(err)
	}
	if got := modelIDs(t, srv); got != "renamed" {
		t.Errorf("after reload: %s", got)
	}

	// A config that fails validation leaves the running one in place.
	bad := strings.Replace(renamed, "model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1", 1)
	if _, err := live.Reload(context.Background(), parseTestConfig(t, bad, backendSrv.URL)); err == nil || !strings.Contains(err.Error(), "not-a-model") {
		t.Errorf("bad config: got %v", err)
	}
	if got := modelIDs(t, srv); got != "renamed" {
		t.Errorf("after failed reload: %s", got)
	}

	// server.listen can't change without a restart.
	moved := strings.Replace(renamed, "listen: :0", "listen: :9999", 1)
	if _, err := live.Reload(context.Background(), parseTestConfig(t, moved, backendSrv.URL)); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Errorf("listen change: got %v", err)
	}
	if live.Gateway().Config.Server.Listen != ":0" {
		t.Errorf("listen: %s", live.Gateway().Config.Server.Listen)
	}
}
