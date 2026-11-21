package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
)

// noFiles is the loader of a Live whose tests reload only from a config.
func noFiles() (*config.Config, error) {
	return nil, errors.New("this test has no config files")
}

// loadOnly is the ConfigFiles of a Live whose test reloads with a function
// and has no files to save to.
type loadOnly func() (*config.Config, error)

func (f loadOnly) Load() (*config.Config, error) { return f() }

func (loadOnly) Read() (*config.Document, error) {
	return nil, errors.New("this test has no config files")
}

func (loadOnly) Save(running, saved *config.Document, force bool, keep int) (*configfile.Saved, error) {
	return nil, &configfile.SaveError{Conflict: true, Err: errors.New("this test has no config files")}
}

func (loadOnly) Status(saved *config.Document, keep int) ([]configfile.FileStatus, configfile.BackupsStatus) {
	return []configfile.FileStatus{}, configfile.BackupsStatus{}
}

func (loadOnly) Backups() ([]configfile.Backup, error) { return []configfile.Backup{}, nil }

func (loadOnly) ReadBackup(ref string) (string, *config.Document, error) {
	return "", nil, configfile.ErrNoBackup
}

// startLive serves a reloadable gateway built from the task config on swap.
func startLive(t *testing.T, swapURL string) (*Live, *gateway) {
	t.Helper()
	g, err := Build(context.Background(), parseConfig(t, taskConfig, map[string]string{"SWAP_URL": swapURL}), discard)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(g, discard, new(slog.LevelVar), loadOnly(noFiles))
	srv := httptest.NewServer(live)
	t.Cleanup(srv.Close)
	return live, &gateway{Server: srv, g: g}
}

func facadeIDs(t *testing.T, gw *gateway) string {
	t.Helper()
	return strings.Trim(gjson.Get(gw.get(t, "/v1/models").body, "data.#.id").Raw, "[]")
}

// A reload swaps in a valid config, keeps the running one if the new one is
// wrong, accepts warnings, and refuses to move server.listen.
func TestReload_SwapsInOnlyAValidConfig(t *testing.T) {
	swap := newSwap(t)
	live, gw := startLive(t, swap.URL)
	vars := map[string]string{"SWAP_URL": swap.URL}
	renamed := edit(t, taskConfig, "  task:\n", "  renamed:\n")
	if _, err := live.Reload(context.Background(), parseConfig(t, renamed, vars)); err != nil || facadeIDs(t, gw) != `"renamed"` {
		t.Errorf("valid: %v, facades %s", err, facadeIDs(t, gw))
	}
	bad := edit(t, renamed, "loadIfNone: true", "loadIfNoneed: true")
	if _, err := live.Reload(context.Background(), parseConfig(t, bad, vars)); err == nil || !strings.Contains(err.Error(), "loadIfNoneed") || facadeIDs(t, gw) != `"renamed"` {
		t.Errorf("wrong: %v, facades %s", err, facadeIDs(t, gw))
	}
	unlisted := edit(t, taskConfig, "model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1")
	g, err := live.Reload(context.Background(), parseConfig(t, unlisted, vars))
	if err != nil || len(g.Warnings) != 1 || !strings.Contains(g.Warnings[0].Error(), "not-a-model") || facadeIDs(t, gw) != `"task"` {
		t.Errorf("only warnings: %v, warnings %v, facades %s", err, g, facadeIDs(t, gw))
	}
	// A changed address is taken, but the gateway listens where it started
	// until a restart, and says so.
	moved := edit(t, renamed, "listen: :0", "listen: :9999")
	if _, err := live.Reload(context.Background(), parseConfig(t, moved, vars)); err != nil || facadeIDs(t, gw) != `"renamed"` {
		t.Errorf("listen change: %v, facades %s", err, facadeIDs(t, gw))
	}
	if got := live.Restart(); len(got) != 1 || got[0] != (PendingRestart{Key: "server.listen", Listening: ":0", Running: ":9999"}) {
		t.Errorf("restart: %+v", got)
	}
}

// A request in flight during a reload finishes on the gateway that accepted
// it; the next request uses the new config.
func TestReload_LetsARequestInFlightFinishOnTheOldConfig(t *testing.T) {
	swap := newSwap(t)
	load := swap.HoldLoads()
	live, gw := startLive(t, swap.URL)
	done := make(chan response)
	go func() { done <- gw.chat(t, `{"model":"task","messages":[]}`) }()
	swap.WaitFor(t, "POST /v1/chat/completions", 1)
	renamed := edit(t, taskConfig, "  task:\n", "  renamed:\n")
	if _, err := live.Reload(context.Background(), parseConfig(t, renamed, map[string]string{"SWAP_URL": swap.URL})); err != nil {
		t.Fatal(err)
	}
	load.Open()
	if r := <-done; r.StatusCode != http.StatusOK || r.target() != "qwen2.5-coder-3b:q8_0" {
		t.Errorf("in flight: got %d %q %s", r.StatusCode, r.target(), r.body)
	}
	if r := gw.chat(t, `{"model":"task","messages":[]}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("after the reload, the old facade: got %d", r.StatusCode)
	}
}

// adminConfig is the task config with a second key, alice's, which alone
// may reload the config.
var adminConfig = strings.Replace(taskConfig, "    - {name: openWebUi, key: secret}\n", `    - {name: openWebUi, key: secret}
    - {name: alice, key: alices-key}
  adminApi:
    write: {allowedKeys: [alice]}
`, 1)

// fileLive is a reloadable gateway served from a config file, which a test
// edits as a host would.
type fileLive struct {
	*gateway
	live  *Live
	level *slog.LevelVar
	path  string
	swap  string
}

// startFileLive writes src, with ${SWAP_URL} filled in from swapURL, to a
// config file, and serves a reloadable gateway built from it.
func startFileLive(t *testing.T, swapURL, src string) *fileLive {
	t.Helper()
	f := &fileLive{level: new(slog.LevelVar), path: filepath.Join(t.TempDir(), "config.yaml"), swap: swapURL}
	f.write(t, src)
	files := configfile.Files{Config: f.path}
	cfg, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	f.live = NewLive(g, discard, f.level, files)
	srv := httptest.NewServer(f.live)
	t.Cleanup(srv.Close)
	f.gateway = &gateway{Server: srv, g: g}
	return f
}

func (f *fileLive) write(t *testing.T, src string) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(strings.ReplaceAll(src, "${SWAP_URL}", f.swap)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// POST /amf/reload is off without server.adminApi.write, refused to a key it
// doesn't list, and otherwise reloads from the files as SIGHUP does: a config
// that fails to load is a 422 and an unreadable file a 500, both keeping the
// running gateway, and a success reports the facades, the warnings and the
// notes, and sets the log level.
func TestReloadEndpoint_ReloadsFromTheFilesForAnAdmin(t *testing.T) {
	swap := newSwap(t)
	off := startFileLive(t, swap.URL, taskConfig)
	if r := off.do(t, "POST", "/amf/reload", "secret", ""); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "server.adminApi.write") {
		t.Errorf("no admins: got %d %s", r.StatusCode, r.body)
	}

	f := startFileLive(t, swap.URL, adminConfig)
	renamed := edit(t, adminConfig, "  task:\n", "  renamed:\n", "logging: {logSelection: true}", "logging: {logSelection: true, level: debug}")
	f.write(t, renamed)
	if r := f.do(t, "POST", "/amf/reload", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: got %d %s", r.StatusCode, r.body)
	}
	if r := f.do(t, "POST", "/amf/reload", "secret", ""); r.StatusCode != http.StatusForbidden || gjson.Get(r.body, "error.type").String() != "permission_error" || facadeIDs(t, f.gateway) != `"task"` {
		t.Errorf("not an admin: got %d %s, facades %s", r.StatusCode, r.body, facadeIDs(t, f.gateway))
	}
	r := f.do(t, "POST", "/amf/reload", "alices-key", "")
	if r.StatusCode != http.StatusOK || r.body != `{"reloaded":true,"models":1,"warnings":[],"notes":[]}`+"\n" || facadeIDs(t, f.gateway) != `"renamed"` || f.level.Level() != slog.LevelDebug {
		t.Errorf("admin: got %d %s, facades %s, level %v", r.StatusCode, r.body, facadeIDs(t, f.gateway), f.level.Level())
	}

	f.write(t, edit(t, renamed, "loadIfNone: true", "loadIfNoneed: true"))
	if r := f.do(t, "POST", "/amf/reload", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "line 23: unknown key 'loadIfNoneed'") || facadeIDs(t, f.gateway) != `"renamed"` {
		t.Errorf("wrong config: got %d %s, facades %s", r.StatusCode, r.body, facadeIDs(t, f.gateway))
	}
	f.write(t, edit(t, renamed, "level: debug", "level: loud"))
	if r := f.do(t, "POST", "/amf/reload", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "logging.level") || f.level.Level() != slog.LevelDebug {
		t.Errorf("wrong level: got %d %s, level %v", r.StatusCode, r.body, f.level.Level())
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if r := f.do(t, "POST", "/amf/reload", "alices-key", ""); r.StatusCode != http.StatusInternalServerError || !strings.Contains(r.body, "config.yaml") || facadeIDs(t, f.gateway) != `"renamed"` {
		t.Errorf("unreadable file: got %d %s, facades %s", r.StatusCode, r.body, facadeIDs(t, f.gateway))
	}

	f.write(t, edit(t, renamed, "model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1", "    - {name: alice, key: alices-key}\n", "    - {name: alice, key: alices-key}\n    - {name: bob, key: alices-key}\n"))
	r = f.do(t, "POST", "/amf/reload", "alices-key", "")
	if r.StatusCode != http.StatusOK || !strings.Contains(gjson.Get(r.body, "warnings.0").String(), "not-a-model") || !strings.Contains(gjson.Get(r.body, "notes.0").String(), "'bob' is the same as 'alice'") {
		t.Errorf("warnings and notes: got %d %s", r.StatusCode, r.body)
	}
}

// A request for a reload from the files that arrives while one is reading
// them waits, then shares the next reload with every request that arrived
// meanwhile, and only with those: one arriving once that reload has read the
// files may have edited them since, so it gets a reload of its own.
func TestReloadFiles_ShareTheNextReload(t *testing.T) {
	swap := newSwap(t)
	g, err := Build(context.Background(), parseConfig(t, taskConfig, map[string]string{"SWAP_URL": swap.URL}), discard)
	if err != nil {
		t.Fatal(err)
	}
	reads := make(chan chan struct{})
	load := func() (*config.Config, error) {
		release := make(chan struct{})
		reads <- release
		<-release
		return config.Parse([]byte(strings.ReplaceAll(taskConfig, "${SWAP_URL}", swap.URL)), func(string) (string, bool) { return "", false })
	}
	live := NewLive(g, discard, new(slog.LevelVar), loadOnly(load))
	results := make(map[string]chan *Gateway)
	reload := func(by string) {
		result := make(chan *Gateway, 1)
		results[by] = result
		go func() {
			reloaded, err := live.ReloadFiles(by)
			if err != nil {
				t.Error(by, err)
				result <- nil
				return
			}
			result <- reloaded.Gateway
		}()
	}

	reload("a")
	first := <-reads
	reload("b")
	reload("c")
	waitForJoined(t, live, "b", "c")
	close(first)
	second := <-reads
	reload("d")
	waitForJoined(t, live, "d")
	close(second)
	third := <-reads
	close(third)

	a, b, c, d := <-results["a"], <-results["b"], <-results["c"], <-results["d"]
	if a == b || b != c || d == b || d == a {
		t.Errorf("gateways: a %p, b %p, c %p, d %p; want b and c to share one, a and d each their own", a, b, c, d)
	}
	select {
	case <-reads:
		t.Error("a fourth read of the files")
	default:
	}
}

// waitForJoined waits until the reload waiting to read the files has been
// asked for by exactly by, in any order.
func waitForJoined(t *testing.T, live *Live, by ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		live.nextMu.Lock()
		var joined []string
		if live.next != nil {
			joined = slices.Sorted(slices.Values(live.next.by))
		}
		live.nextMu.Unlock()
		if slices.Equal(joined, by) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting reload asked for by %v, want %v", joined, by)
		}
		runtime.Gosched()
	}
}
