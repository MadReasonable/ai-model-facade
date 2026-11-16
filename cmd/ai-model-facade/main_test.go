package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
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
  openWebUi: {type: openWebUi, baseUrl: "`+webui.URL+`", llamaSwap: {backend: llamaSwap}}
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
