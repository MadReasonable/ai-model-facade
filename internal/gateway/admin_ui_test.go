package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
)

// The admin UI needs no key to load, since it holds no secret and signs in
// to the endpoints it calls, and is off, as they are, when no admin list
// turns them on.
func TestAdminUI_ServedWhileTheAdminAPIIsOn(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	r := a.send(t, "GET", "/amf/ui/", "", "")
	a.expect(t, r, http.StatusOK, "the page")
	if !strings.Contains(r.body, `<script type="module" src="./app.mjs">`) || r.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("the page: %v\n%s", r.Header, r.body)
	}
	if r := a.send(t, "GET", "/amf/ui/app.mjs", "", ""); r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("a module: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(a.URL + "/amf/ui")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/amf/ui/" {
		t.Errorf("/amf/ui: %d to %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	off := startAdmin(t, swap.URL, strings.Replace(adminMain, "  adminApi:\n    read: {allowedKeys: [bob]}\n    write: {allowedKeys: [alice]}\n", "", 1), adminSecrets)
	if r := off.send(t, "GET", "/amf/ui/", "", ""); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "server.adminApi.read") {
		t.Errorf("with the admin API off: %d %s", r.StatusCode, r.body)
	}
}

// GET /amf/config-status says who the caller is and whether it may change
// the config, so the admin UI offers a reader nothing to edit.
func TestAdmin_StatusNamesTheCaller(t *testing.T) {
	a := startAdmin(t, newSwap(t).URL, adminMain, adminSecrets)
	for key, want := range map[string]string{"alices-key": `{"name":"alice","write":true}`, "bobs-key": `{"name":"bob","write":false}`} {
		r := a.send(t, "GET", "/amf/config-status", key, "")
		if got := gjson.Get(r.body, "caller").Raw; got != want {
			t.Errorf("%s: caller %s, want %s", key, got, want)
		}
	}
}

// With server.adminApi.listen set, server.listen serves the client APIs
// and that address the admin endpoints and the UI, each without the
// other's; both serve /health and /amf/models. A reload can't move it.
func TestAdminListen_SplitsTheEndpoints(t *testing.T) {
	swap := newSwap(t)
	main := strings.Replace(adminMain, "    write: {allowedKeys: [alice]}\n", "    write: {allowedKeys: [alice]}\n    listen: 127.0.0.1:0\n", 1)
	a := startAdmin(t, swap.URL, main, adminSecrets)
	admin := httptest.NewServer(a.live.Admin())
	t.Cleanup(admin.Close)
	get := func(base, path, key string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		path        string
		client, adm int
	}{
		{"/health", 200, 200},
		{"/amf/models", 200, 200},
		{"/v1/models", 200, 404},
		{"/amf/config", 404, 200},
		{"/amf/config-status", 404, 200},
		{"/amf/ui/", 404, 200},
	} {
		if got := get(a.URL, c.path, "bobs-key"); got != c.client {
			t.Errorf("server.listen %s: %d, want %d", c.path, got, c.client)
		}
		if got := get(admin.URL, c.path, "bobs-key"); got != c.adm {
			t.Errorf("server.adminApi.listen %s: %d, want %d", c.path, got, c.adm)
		}
	}
	// A reload that drops the address keeps the endpoints where they are
	// until a restart.
	files := configfile.Files{Config: a.main, Secrets: a.secrets}
	cfg, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.AdminAPI.Listen = ""
	if _, err := a.live.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if get(a.URL, "/amf/config", "bobs-key") != 404 || get(admin.URL, "/amf/config", "bobs-key") != 200 {
		t.Error("the admin endpoints moved before a restart")
	}
	if got := a.live.Restart(); len(got) != 1 || got[0].Key != "server.adminApi.listen" || got[0].Running != "" {
		t.Errorf("restart: %+v", got)
	}
}

// A changed server.listen can be applied and saved; the gateway goes on
// listening where it started, and the replies, the status and the log say
// a restart is needed, and whether the files hold the new address yet.
func TestAdmin_ListenChangeWaitsForARestart(t *testing.T) {
	a := startAdmin(t, newSwap(t).URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/server/listen", "alices-key", `"127.0.0.1:9"`), http.StatusOK, "edit")
	r := a.send(t, "POST", "/amf/config-apply", "alices-key", "")
	a.expect(t, r, http.StatusOK, "apply")
	if !strings.Contains(r.body, "server.listen is '127.0.0.1:9' now, but the gateway listens on ':0' until it restarts") {
		t.Errorf("apply: %s", r.body)
	}
	status := func() string {
		return gjson.Get(a.send(t, "GET", "/amf/config-status", "bobs-key", "").body, "restart").Raw
	}
	if got := status(); got != `[{"key":"server.listen","listening":":0","running":"127.0.0.1:9","saved":false}]` {
		t.Errorf("after apply: %s", got)
	}
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save")
	if got := gjson.Get(status(), "0.saved").Raw; got != "true" {
		t.Errorf("after save: %s", status())
	}
	if !strings.Contains(readFile(t, a.main), "listen: 127.0.0.1:9") {
		t.Errorf("the file doesn't hold the new address")
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/server/listen", "alices-key", `":0"`), http.StatusOK, "put it back")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply it back")
	if got := status(); got != `[]` {
		t.Errorf("with the address back: %s", got)
	}
	if !strings.Contains(logText(a), `msg="restart needed" key=server.listen listening=:0 running=127.0.0.1:9`) {
		t.Errorf("no restart line in the log")
	}
}

// The admin endpoints can't listen where the client APIs do: the address
// would be bound twice.
func TestAdminListen_MustDifferFromServerListen(t *testing.T) {
	_, err := config.Parse([]byte("server:\n  listen: 127.0.0.1:8088\n  adminApi: {listen: 127.0.0.1:8088}\n"), func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "server.adminApi.listen is server.listen") {
		t.Errorf("err = %v", err)
	}
}

// logText is the text of every line a's gateway has logged.
func logText(a *adminLive) string {
	var b strings.Builder
	for _, e := range a.logs.Read(loghub.Filter{Level: slog.LevelDebug}) {
		b.Write(e.Text)
	}
	return b.String()
}

// server.adminApi.uiBasePath moves the admin UI, and the admin endpoints
// stay at /amf. At the root, on a listener of its own, the UI is served
// there. Set to the root while the gateway still listens on server.listen
// alone, which serves the client APIs there, it stays where it was until a
// restart, or at the default if the config now routes that path itself,
// and /amf/config-status says so.
func TestAdminUI_ServedUnderUIBasePath(t *testing.T) {
	swap := newSwap(t)
	get := func(base, path string) (int, string) {
		t.Helper()
		noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := noRedirect.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusMovedPermanently {
			return resp.StatusCode, resp.Header.Get("Location")
		}
		return resp.StatusCode, string(body)
	}
	page := func(base, path string) bool {
		t.Helper()
		status, body := get(base, path)
		return status == http.StatusOK && strings.Contains(body, `src="./app.mjs"`)
	}

	moved := startAdmin(t, swap.URL, strings.Replace(adminMain, "  adminApi:\n", "  adminApi:\n    uiBasePath: /admin\n", 1), adminSecrets)
	if !page(moved.URL, "/admin/") || page(moved.URL, "/amf/ui/") {
		t.Error("with uiBasePath /admin: the page is not at /admin/ alone")
	}
	if status, to := get(moved.URL, "/admin"); status != http.StatusMovedPermanently || to != "/admin/" {
		t.Errorf("/admin: %d to %s", status, to)
	}
	if status, _ := get(moved.URL, "/admin/app.mjs"); status != http.StatusOK {
		t.Errorf("a module under /admin: %d", status)
	}

	root := startAdmin(t, swap.URL, strings.Replace(adminMain, "  adminApi:\n", "  adminApi:\n    listen: 127.0.0.1:0\n    uiBasePath: /\n", 1), adminSecrets)
	admin := httptest.NewServer(root.live.Admin())
	t.Cleanup(admin.Close)
	if !page(admin.URL, "/") || page(admin.URL, "/amf/ui/") || page(root.URL, "/") {
		t.Error("with uiBasePath /: the page is not at the admin listener's root alone")
	}
	if status, _ := get(admin.URL, "/app.mjs"); status != http.StatusOK {
		t.Errorf("a module at the root: %d", status)
	}
	if r := root.send(t, "GET", "/amf/config-status", "bobs-key", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("server.listen serves the admin endpoints: %d", r.StatusCode)
	}

	shared := startAdmin(t, swap.URL, strings.Replace(adminMain, "  adminApi:\n", "  adminApi:\n    uiBasePath: /admin\n", 1), adminSecrets)
	shared.expect(t, shared.send(t, "PUT", "/amf/config/server/adminApi/listen", "alices-key", `"127.0.0.1:0"`), http.StatusCreated, "edit")
	shared.expect(t, shared.send(t, "PUT", "/amf/config/server/adminApi/uiBasePath", "alices-key", `"/"`), http.StatusOK, "edit")
	r := shared.send(t, "POST", "/amf/config-apply", "alices-key", "")
	shared.expect(t, r, http.StatusOK, "apply")
	if !page(shared.URL, "/admin/") || page(shared.URL, "/amf/ui/") || page(shared.URL, "/") {
		t.Error("before a restart: the page is not where it was")
	}
	if !strings.Contains(r.body, "server.adminApi.uiBasePath is '/' now, but the admin UI stays at '/admin' until a restart") {
		t.Errorf("apply's notes: %s", r.body)
	}
	status := shared.send(t, "GET", "/amf/config-status", "bobs-key", "").body
	if got := gjson.Get(status, `restart.#(key=="server.adminApi.uiBasePath")`).Raw; got != `{"key":"server.adminApi.uiBasePath","listening":"/admin","running":"/","saved":false}` {
		t.Errorf("restart: %s", gjson.Get(status, "restart").Raw)
	}
	// A path the new config gives a client API leaves the UI at the
	// default.
	shared.expect(t, shared.send(t, "PUT", "/amf/config/api", "alices-key", `{"openAi": {"basePath": "/admin"}}`), http.StatusCreated, "edit")
	shared.expect(t, shared.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	if !page(shared.URL, "/amf/ui/") || page(shared.URL, "/admin/") {
		t.Error("with /admin a client API's: the page is not at the default")
	}
}
