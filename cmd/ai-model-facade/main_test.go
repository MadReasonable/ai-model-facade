package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata with the current output")

// TestMain runs the program itself instead of the tests when the test binary
// is started by runProgram, so a test can send it real signals.
func TestMain(m *testing.M) {
	if os.Getenv("AI_MODEL_FACADE_RUN_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// result is what one run of the program produced.
type result struct {
	code           int
	stdout, stderr string
}

func runArgs(args ...string) result {
	return runWithStdin("", args...)
}

func runWithStdin(stdin string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

// writeConfig writes src to a file in a temporary directory and returns its
// path.
func writeConfig(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (run with -update to accept it):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func TestRun_PrintsTheVersion(t *testing.T) {
	if r := runArgs("version"); r.code != 0 || strings.TrimSpace(r.stdout) == "" {
		t.Errorf("got %+v", r)
	}
}

// Without a command, the program says what it is and how to use it, and
// exits 2.
func TestRun_PrintsUsageWithoutACommand(t *testing.T) {
	for _, args := range [][]string{nil, {"check"}, {"nope", "config.yaml"}} {
		if r := runArgs(args...); r.code != 2 || !strings.Contains(r.stderr, "An LLM gateway") || !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%q: got %+v", args, r)
		}
	}
}

// check prints the example config's facades, each candidate's resolved
// params and what each pattern can route to, and exits 0.
func TestCheck_ExampleConfig(t *testing.T) {
	swap := testkit.NewLlamaSwap(t,
		testkit.SwapModel{ID: "qwen3.6-35b-a3b:q8_0", Aliases: []string{"qwen3.6-35b:Assistant"}},
		testkit.SwapModel{ID: "qwen3.8-flash-next:iq3_s", Aliases: []string{"qwen3.8-flash-next:Assistant"}},
		testkit.SwapModel{ID: "qwen3.8-27b:q6_k"},
		testkit.SwapModel{ID: "qwen2.5-coder-3b:q8_0"},
	)
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := runArgs("check", writeConfig(t, strings.Replace(string(example), "http://ai-llama-swap:8080", swap.URL, 1)))
	if r.code != 0 || r.stderr != "" {
		t.Errorf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	golden(t, "check-example.golden", r.stdout)
}

// What depends on a backend being current is an error for check, though
// serve only warns: each problem on stderr, and exit 1.
func TestCheck_FailsOnBackendProblems(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "qwen3.8-27b:q6_k"})
	r := runArgs("check", writeConfig(t, `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  task:
    backend: llamaSwap
    candidates:
      - model: qwen9:q1
      - model: qwen3.8-27b
`))
	if r.code != 1 {
		t.Errorf("exit %d", r.code)
	}
	golden(t, "check-backend-problems.golden", r.stdout+"--- stderr\n"+r.stderr)
}

// A config that is wrong fails check before it lists anything.
func TestCheck_FailsOnAWrongConfig(t *testing.T) {
	r := runArgs("check", writeConfig(t, "server: {listen: \":0\"}\nbackends: {}\nmodels:\n  task: {backend: nope, candidates: [{model: m}]}\n"))
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "error: ") || !strings.Contains(r.stderr, "'nope' is not defined") {
		t.Errorf("got %+v", r)
	}
}

// check shows which candidates are aliases and presets, through llama-swap
// and through Open WebUI, and notes that their own params may override the
// gateway's.
func TestCheck_ShowsAliasesAndPresets(t *testing.T) {
	swap := testkit.NewLlamaSwap(t,
		testkit.SwapModel{ID: "a", Aliases: []string{"a:Fast", "a:Slow"}},
		testkit.SwapModel{ID: "b", Aliases: []string{"b:Fast"}},
	)
	webui := testkit.NewOpenWebUI(t,
		testkit.SwapEntry("a", "unloaded"),
		testkit.SwapPresetEntry("a:Fast", "a", "unloaded"),
		testkit.SwapPresetEntry("b:Fast", "b", "unloaded"),
	)
	r := runArgs("check", writeConfig(t, `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
  openWebUi: {type: openWebUi, baseUrl: "`+webui.URL+`", links: [{backend: llamaSwap}]}
models:
  direct:
    backend: llamaSwap
    params: {max_tokens: 100}
    candidates:
      - model: "*:Fast"
      - model: a:Slow
      - model: "a*"
  through-open-webui:
    backend: openWebUi
    params: {max_tokens: 100}
    candidates:
      - model: a:Fast
      - model: b:Fast
`))
	if r.code != 0 || r.stderr != "" {
		t.Errorf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	golden(t, "check-aliases.golden", r.stdout)
}

// hash-key prints a new key and the hash a key entry takes in its place, and
// with --stdin hashes an existing key, so neither needs the key on the
// command line. Both hashes are what the config accepts for that key.
func TestHashKey_PrintsAKeyAndItsHash(t *testing.T) {
	r := runArgs("hash-key")
	key, hash, ok := strings.Cut(r.stdout, "\nhash: ")
	key, hasKey := strings.CutPrefix(key, "key:  ")
	if r.code != 0 || !ok || !hasKey || !strings.HasPrefix(key, "amf-") || len(key) != len("amf-")+43 {
		t.Fatalf("got %+v", r)
	}
	if hash != config.HashKey(key)+"\n" {
		t.Errorf("hash %q is not the hash of %q", hash, key)
	}
	if again := runArgs("hash-key"); again.stdout == r.stdout {
		t.Error("two runs printed the same key")
	}
	if r := runWithStdin(key+"\n", "hash-key", "--stdin"); r.code != 0 || r.stdout != "hash: "+config.HashKey(key)+"\n" {
		t.Errorf("--stdin: got %+v", r)
	}
	if r := runWithStdin("", "hash-key", "--stdin"); r.code != 1 || !strings.Contains(r.stderr, "no key") {
		t.Errorf("--stdin without a key: got %+v", r)
	}
	if r := runArgs("hash-key", "secret"); r.code != 2 {
		t.Errorf("a key as an argument: got %+v", r)
	}
}

// init writes a config that check, and so serve, accepts, with a new admin
// key it prints once and the files hold only as a hash: in the secrets file
// when one is named, which only its owner may read, else in the config.
func TestInit_WritesAConfigThatStarts(t *testing.T) {
	for _, withSecrets := range []bool{true, false} {
		dir := t.TempDir()
		files := configFiles{config: filepath.Join(dir, "config.yaml")}
		args := []string{"init", files.config}
		if withSecrets {
			files.secrets = filepath.Join(dir, "secrets.yaml")
			args = append(args, "--secrets", files.secrets)
		}
		r := runArgs(args...)
		_, after, ok := strings.Cut(r.stdout, "admin key: ")
		key, _, _ := strings.Cut(after, "\n")
		if r.code != 0 || !ok || !strings.HasPrefix(key, "amf-") {
			t.Fatalf("secrets file %t: got %+v", withSecrets, r)
		}
		main, err := os.ReadFile(files.config)
		if err != nil {
			t.Fatal(err)
		}
		secrets, _ := os.ReadFile(files.secrets) // none without --secrets
		if strings.Contains(string(main)+string(secrets), key) {
			t.Errorf("a file holds the key itself, not its hash:\n%s\n%s", main, secrets)
		}
		cfg, err := files.load()
		if err != nil {
			t.Fatal(err)
		}
		if name, ok := cfg.Server.KeyName(sha256.Sum256([]byte(key))); !ok || name != "admin" {
			t.Errorf("secrets file %t: the printed key isn't the admin's: %q", withSecrets, name)
		}
		if withSecrets {
			if strings.Contains(string(main), "sha256:") {
				t.Errorf("the config holds the hash, which goes in the secrets file:\n%s", main)
			}
			if info, err := os.Stat(files.secrets); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("secrets file: %v, mode %v", err, info.Mode())
			}
		}
		checkArgs := append([]string{"check"}, args[1:]...)
		if r := runArgs(checkArgs...); r.code != 0 {
			t.Errorf("check of what init wrote: %+v", r)
		}
	}
}

// init never replaces a file: with either already there, it writes nothing.
func TestInit_ReplacesNoFile(t *testing.T) {
	dir := t.TempDir()
	config, secrets := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(secrets, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runArgs("init", config, "--secrets", secrets)
	if r.code != 1 || !strings.Contains(r.stderr, secrets+" already exists") {
		t.Errorf("got %+v", r)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Errorf("init wrote the config beside a secrets file it refused: %v", err)
	}
	if data, _ := os.ReadFile(secrets); string(data) != "mine\n" {
		t.Errorf("the secrets file changed: %q", data)
	}
}

// One key under two names is a warning for check, as for serve: the config
// has a defined meaning, the first name, so check still exits 0.
func TestCheck_WarnsAboutAKeyListedTwice(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	r := runArgs("check", writeConfig(t, `
server:
  listen: ":0"
  apiKeys:
    - {name: alice, key: secret}
    - {name: bob, hash: "`+config.HashKey("secret")+`"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  task: {backend: llamaSwap, candidates: [{model: m}]}
`))
	if r.code != 0 || r.stderr != "warning: server.apiKeys: key of 'bob' is the same as 'alice'; requests with it are named 'alice'\n" {
		t.Errorf("got %+v", r)
	}
}

// check shows who may ask for a facade, for each list the facade sets.
func TestCheck_ShowsAFacadesAccessLists(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	r := runArgs("check", writeConfig(t, `
server:
  listen: ":0"
  apiKeys: [{name: alice, key: k1}, {name: agent, key: k2}]
  groups: {family: [alice]}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  open: {backend: llamaSwap, candidates: [{model: m}]}
  limited: {backend: llamaSwap, allowedKeys: [agent], allowedGroups: [family, unknown], candidates: [{model: m}]}
  closed: {backend: llamaSwap, allowedGroups: [], candidates: [{model: m}]}
`))
	want := "open\n  backend: llamaSwap\n  1. m\n     params: {}\n\nlimited\n  backend: llamaSwap\n  allowed keys: agent\n  allowed groups: family, unknown\n  1. m\n     params: {}\n\nclosed\n  backend: llamaSwap\n  allowed groups: none\n  1. m\n     params: {}\n"
	if r.code != 0 || r.stdout != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", r.code, r.stderr, r.stdout)
	}
}

// check shows who may use a candidate's backend, under each candidate on a
// backend that sets a list, since the lists decide which candidates a caller
// can be served by.
func TestCheck_ShowsABackendsAccessLists(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	r := runArgs("check", writeConfig(t, `
server:
  listen: ":0"
  apiKeys: [{name: alice, key: k1}, {name: agent, key: k2}]
  groups: {family: [alice]}
backends:
  open: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
  limited: {type: llamaSwap, baseUrl: "`+swap.URL+`", allowedKeys: [agent], allowedGroups: [family]}
models:
  task: {backend: limited, candidates: [{model: m}, {model: m, backend: open}]}
`))
	want := "task\n  backend: limited\n  1. m\n     backend 'limited' allowed keys: agent\n     backend 'limited' allowed groups: family\n     params: {}\n  2. m on open\n     params: {}\n"
	if r.code != 0 || r.stdout != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", r.code, r.stderr, r.stdout)
	}
}

// check shows which group's keyMap entry a key in two mapped groups gets,
// as a warning that doesn't fail it.
func TestCheck_ShowsKeysInTwoMappedGroups(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	r := runArgs("check", writeConfig(t, `
server:
  listen: ":0"
  apiKeys: [{name: alice, key: k1}]
  groups: {family: [alice], adults: [alice]}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`", keyMap: {family: kf, adults: ka}}
models:
  task: {backend: llamaSwap, candidates: [{model: m}]}
`))
	want := "warning: backends.llamaSwap: keyMap: key 'alice' is in both 'family' and 'adults', with no entry of its own; 'family' comes first, so its key is sent\n"
	if r.code != 0 || r.stderr != want {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// check and serve take a secrets file with --secrets, and check names it in
// an error that is in it; health, which reads only server.listen, doesn't
// take one.
func TestCheck_TakesASecretsFile(t *testing.T) {
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	configPath := writeConfig(t, `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  task: {backend: llamaSwap, candidates: [{model: m}]}
`)
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.yaml")
	os.WriteFile(secretsPath, []byte("backends: {llamaSwap: {apiKey: k}}\n"), 0o600)
	if r := runArgs("check", configPath, "--secrets", secretsPath); r.code != 0 || r.stderr != "" {
		t.Errorf("good secrets: got %+v", r)
	}
	os.WriteFile(secretsPath, []byte("backends: {llamaSwap: {baseUrl: x}}\n"), 0o600)
	if r := runArgs("check", configPath, "--secrets", secretsPath); r.code != 1 || !strings.Contains(r.stderr, "error: "+secretsPath+": ") || !strings.Contains(r.stderr, "unknown key 'baseUrl'") {
		t.Errorf("bad secrets: got %+v", r)
	}
	for _, args := range [][]string{{"health", configPath, "--secrets", secretsPath}, {"check", configPath, "--secret", secretsPath}, {"check", configPath, "--secrets"}} {
		if r := runArgs(args...); r.code != 2 {
			t.Errorf("%q: got %+v", args, r)
		}
	}
}

// health exits 0 if something at server.listen answers /health, and 1 if
// nothing does; it contacts no backend.
func TestHealth_ChecksTheGatewayAtServerListen(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	config := func(listen string) string {
		return writeConfig(t, "server: {listen: \""+listen+"\"}\nbackends:\n  b: {type: llamaSwap, baseUrl: \"http://unreachable.invalid\"}\nmodels:\n  m: {backend: b, candidates: [{model: x}]}\n")
	}
	if r := runArgs("health", config(strings.TrimPrefix(up.URL, "http://"))); r.code != 0 {
		t.Errorf("up: got %+v", r)
	}
	down := freeAddr(t)
	if r := runArgs("health", config(down)); r.code != 1 || !strings.Contains(r.stderr, "error: ") {
		t.Errorf("down: got %+v", r)
	}
}

// health reads only server.listen, so a config that needs its secrets file,
// which the image's health check doesn't pass, or that is wrong elsewhere,
// still gets checked at its address.
func TestHealth_ReadsOnlyServerListen(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer up.Close()
	t.Setenv("GATEWAY_LISTEN", strings.TrimPrefix(up.URL, "http://"))
	src := "server:\n  listen: ${GATEWAY_LISTEN}\n  groups: {family: [lloyd]}\nbackends: {b: {type: nope}}\nmodels: {}\n"
	if r := runArgs("health", writeConfig(t, src)); r.code != 0 {
		t.Errorf("got %+v", r)
	}
	for _, bad := range []string{"server: {port: 1}\n", "server: [\n", ""} {
		if r := runArgs("health", writeConfig(t, bad)); r.code != 1 || !strings.Contains(r.stderr, "config.yaml") {
			t.Errorf("%q: got %+v", bad, r)
		}
	}
}

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// program is the program running as its own process.
type program struct {
	cmd   *exec.Cmd
	lines chan string // stderr, line by line
}

// runProgram starts the program with args, as a process of its own.
func runProgram(t *testing.T, args ...string) *program {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "AI_MODEL_FACADE_RUN_MAIN=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &program{cmd: cmd, lines: make(chan string, 100)}
	go func() {
		defer close(p.lines)
		for s := bufio.NewScanner(stderr); s.Scan(); {
			p.lines <- s.Text()
		}
	}()
	t.Cleanup(func() { cmd.Process.Kill() })
	return p
}

// waitForLog waits for a stderr line containing s.
func (p *program) waitForLog(t *testing.T, s string) {
	t.Helper()
	deadline := time.After(testkit.Timeout)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatalf("the program exited before logging %q", s)
			}
			if strings.Contains(line, s) {
				return
			}
		case <-deadline:
			t.Fatalf("the program didn't log %q", s)
		}
	}
}

// serve answers on server.listen, reloads its config on SIGHUP, and shuts
// down cleanly on SIGTERM.
func TestServe_ReloadsOnSIGHUPAndStopsOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the program as a process")
	}
	swap := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	addr := freeAddr(t)
	src := "server: {listen: \"" + addr + "\"}\nbackends:\n  llamaSwap: {type: llamaSwap, baseUrl: \"" + swap.URL + "\"}\nmodels:\n  task: {backend: llamaSwap, candidates: [{model: m}]}\n"
	path := writeConfig(t, src)
	p := runProgram(t, "serve", path)
	p.waitForLog(t, "msg=listening")
	facades := func() string {
		resp, err := http.Get("http://" + addr + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return gjson.GetBytes(body, "data.#.id").Raw
	}
	if got := facades(); got != `["task"]` {
		t.Fatalf("before the reload: %s", got)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(src, "  task:", "  renamed:", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	p.cmd.Process.Signal(syscall.SIGHUP)
	p.waitForLog(t, `msg="config reloaded"`)
	if got := facades(); got != `["renamed"]` {
		t.Errorf("after the reload: %s", got)
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	p.waitForLog(t, `msg="shutting down"`)
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("exit: %v", err)
	}
}

// With server.adminApi.listen set, serve listens there too, for the admin
// endpoints alone, and logs both addresses.
func TestServe_ListensForTheAdminAPIOnItsOwnAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the program as a process")
	}
	addr, adminAddr := freeAddr(t), freeAddr(t)
	src := "server:\n  listen: \"" + addr + "\"\n  apiKeys: [{name: alice, key: alices-key}]\n  adminApi: {read: {allowedKeys: [alice]}, listen: \"" + adminAddr + "\"}\nbackends:\n  llamaSwap: {type: llamaSwap, baseUrl: \"http://127.0.0.1:1\"}\nmodels:\n  task: {backend: llamaSwap, candidates: [{model: m, loadIfNone: true}]}\n"
	p := runProgram(t, "serve", writeConfig(t, src))
	p.waitForLog(t, "msg=listening addr="+addr+" admin="+adminAddr)
	status := func(base, path string) int {
		req, _ := http.NewRequest("GET", "http://"+base+path, nil)
		req.Header.Set("Authorization", "Bearer alices-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(adminAddr, "/amf/config-status"); got != http.StatusOK {
		t.Errorf("the admin address: %d", got)
	}
	if got := status(addr, "/amf/config-status"); got != http.StatusNotFound {
		t.Errorf("server.listen still serves the admin endpoints: %d", got)
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("exit: %v", err)
	}
}

// stalled is a stderr whose reader has stopped: every write waits until
// released.
type stalled struct{ released chan struct{} }

func (s stalled) Write(p []byte) (int, error) {
	<-s.released
	return len(p), nil
}

// serve's logger hands a record to the copy of the log before stderr, so a
// stderr write that stalls can't keep it from GET /amf/logs.
func TestServe_LogCopyComesBeforeAStalledStderr(t *testing.T) {
	cfg, err := config.Parse([]byte("server:\n  listen: :0\n  apiKeys: [{name: a, key: k}]\n  adminApi: {read: {allowedKeys: [a]}}\nmodels: {}\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	logs := loghub.New()
	logs.Configure(cfg)
	stderr := stalled{make(chan struct{})}
	defer close(stderr.released)
	go newLogger(stderr, new(slog.LevelVar), logs).Info("before the stall")
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.Read(loghub.Filter{})) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the record didn't reach the copy of the log while stderr was stalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On SIGTERM, serve ends every log stream, so an open stream doesn't hold
// shutdown open for its grace period.
func TestServe_ShutdownEndsLogStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the program as a process")
	}
	addr := freeAddr(t)
	src := "server:\n  listen: \"" + addr + "\"\n  apiKeys: [{name: alice, key: alices-key}]\n  adminApi: {read: {allowedKeys: [alice]}}\nbackends:\n  llamaSwap: {type: llamaSwap, baseUrl: \"http://127.0.0.1:1\"}\nmodels:\n  task: {backend: llamaSwap, candidates: [{model: m, loadIfNone: true}]}\n"
	p := runProgram(t, "serve", writeConfig(t, src))
	p.waitForLog(t, "msg=listening")
	req, _ := http.NewRequest("GET", "http://"+addr+"/amf/logs?follow", nil)
	req.Header.Set("Authorization", "Bearer alices-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	p.cmd.Process.Signal(syscall.SIGTERM)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: end") {
		t.Errorf("the stream ended without an end event:\n%s", body)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("exit: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("shutdown took %v with a stream open", took)
	}
}
