package config

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

const minimal = `
server:
  listen: 0.0.0.0:8090
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "http://x"}
models:
  task: {backend: llamaSwap}
`

func TestParse_MinimalConfig(t *testing.T) {
	cfg, err := Parse([]byte(minimal), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TargetHeader != "X-Gateway-Target" {
		t.Errorf("default target header: %q", cfg.Server.TargetHeader)
	}
	entries, err := Entries(&cfg.Backends, "backends")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "llamaSwap" || entries[0].Type != "llamaSwap" {
		t.Errorf("entries: %+v", entries)
	}
}

func TestParse_ExpandsEnvironmentVariables(t *testing.T) {
	src := minimal + `
  other:
    backend: llamaSwap
    params:
      key: "${SECRET}"
      port: ${PORT}
      mixed: a-${PORT}-b
`
	cfg, err := Parse([]byte(src), env(map[string]string{"SECRET": "s3", "PORT": "8090"}))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := Entries(&cfg.Models, "models")
	var params struct {
		Params map[string]any `yaml:"params"`
	}
	if err := entries[1].Node.Decode(&params); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"key": "s3", "port": "8090", "mixed": "a-8090-b"}
	for k, v := range want {
		if params.Params[k] != v {
			t.Errorf("%s: got %#v, want %#v", k, params.Params[k], v)
		}
	}
}

func TestParse_UndefinedVariableIsAnErrorWithItsLine(t *testing.T) {
	src := minimal + "  other: {backend: \"${NOPE}\"}\n"
	_, err := Parse([]byte(src), env(nil))
	if err == nil || !strings.Contains(err.Error(), "NOPE") || !strings.Contains(err.Error(), "line 8") {
		t.Errorf("got %v", err)
	}
}

func TestParse_RejectsDuplicateNames(t *testing.T) {
	cases := map[string]struct {
		src, section, want string
	}{
		"backends": {strings.Replace(minimal, "models:", "  llamaSwap: {type: llamaSwap, baseUrl: \"http://y\"}\nmodels:", 1), "backends", "line 6: backends.llamaSwap is defined again (first at line 5)"},
		"models":   {minimal + "  task: {backend: llamaSwap}\n", "models", "line 8: models.task is defined again (first at line 7)"},
	}
	for name, c := range cases {
		cfg, err := Parse([]byte(c.src), env(nil))
		if err != nil {
			t.Fatal(err)
		}
		section := &cfg.Backends
		if c.section == "models" {
			section = &cfg.Models
		}
		if _, err := Entries(section, c.section); err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestParse_RejectsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"top level": minimal + "extra: 1\n",
		"server":    strings.Replace(minimal, "listen:", "listn: x\n  listen:", 1),
		"api key":   strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: b, scope: c}]", 1),
		"logging":   minimal + "logging: {levl: info}\n",
		"api":       minimal + "api: {openAi: {curent: true}}\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestDecodeStrict_RejectsUnknownNestedKeys(t *testing.T) {
	type candidate struct {
		Model string `yaml:"model"`
	}
	type entry struct {
		Backend    string      `yaml:"backend"`
		Params     yaml.Node   `yaml:"params"`
		Candidates []candidate `yaml:"candidates"`
	}
	var doc yaml.Node
	src := "backend: b\nparams: {anything: [1, {goes: true}]}\ncandidates:\n  - model: a\n  - modl: b\n"
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	err := DecodeStrict(doc.Content[0], &e)
	if err == nil || !strings.Contains(err.Error(), `line 5: unknown key 'modl'`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrict_FollowsAnchorsAndMerges(t *testing.T) {
	type limits struct {
		A int `yaml:"a"`
	}
	type root struct {
		Vars yaml.Node `yaml:"vars"`
		X    limits    `yaml:"x"`
		Y    limits    `yaml:"y"`
	}
	src := "vars:\n  base: &base {a: 1, b: 2}\nx: *base\ny: {<<: *base}\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var r root
	err := DecodeStrict(doc.Content[0], &r)
	if err == nil || !strings.Contains(err.Error(), `unknown key 'b'`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrict_AcceptsInlinedStructKeys(t *testing.T) {
	type common struct {
		A int `yaml:"a"`
	}
	type entry struct {
		Common common `yaml:",inline"`
		B      int    `yaml:"b"`
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\nb: 2"), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := DecodeStrict(doc.Content[0], &e); err != nil {
		t.Errorf("inlined keys rejected: %v", err)
	}
	if err := yaml.Unmarshal([]byte("a: 1\nc: 3"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key 'c'`) {
		t.Errorf("got %v", err)
	}
	// The inline field's own name must not be accepted as a key.
	if err := yaml.Unmarshal([]byte("common: {a: 1}"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key 'common'`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrict_AcceptsInlinedMapKeys(t *testing.T) {
	type inner struct {
		X int `yaml:"x"`
	}
	type entry struct {
		B     int              `yaml:"b"`
		Extra map[string]inner `yaml:",inline"`
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("b: 1\nanything: {x: 2}"), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := DecodeStrict(doc.Content[0], &e); err != nil {
		t.Errorf("inline map keys rejected: %v", err)
	}
	// Values caught by the inline map are still checked against its type.
	if err := yaml.Unmarshal([]byte("b: 1\nanything: {y: 2}"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key 'y'`) {
		t.Errorf("got %v", err)
	}
}

// A key entry gives exactly one of the key or its hash, and a hash must be
// one the gateway can match, so a mistyped one is an error rather than a key
// that never matches.
func TestParse_APIKeyEntriesAreChecked(t *testing.T) {
	hash := HashKey("secret")
	cases := []struct{ name, entries, want string }{
		{"no key", "[{name: a}]", "key or hash is required"},
		{"empty key", "[{name: a, key: \"${EMPTY}\"}]", "key is empty; if it comes from an environment variable, that variable is set to nothing"},
		{"empty hash", "[{name: a, hash: \"${EMPTY}\"}]", "hash is empty"},
		{"both", "[{name: a, key: secret, hash: " + hash + "}]", "not both"},
		{"no name", "[{key: secret}]", "name is required"},
		{"reserved name", "[{name: unknown, key: secret}]", "reserved"},
		{"no prefix", "[{name: a, hash: " + strings.TrimPrefix(hash, "sha256:") + "}]", "followed by 64 hex characters"},
		{"short", "[{name: a, hash: " + hash[:len(hash)-2] + "}]", "followed by 64 hex characters"},
		{"long", "[{name: a, hash: " + hash + "00}]", "followed by 64 hex characters"},
		{"not hex", "[{name: a, hash: " + hash[:len(hash)-1] + "z}]", "followed by 64 hex characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: "+c.entries, 1)
			if _, err := Parse([]byte(src), env(map[string]string{"EMPTY": ""})); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}

// Every entry is indexed by its key's SHA-256, whether it gives the key or
// the hash, uppercase hex included, so both forms match the same way.
func TestParse_IndexesKeysAndHashesAlike(t *testing.T) {
	upper := "sha256:" + strings.ToUpper(strings.TrimPrefix(HashKey("bobs-key"), "sha256:"))
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: alice, key: alices-key}, {name: bob, hash: "+upper+"}]", 1)
	cfg, err := Parse([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"alices-key": "alice", "bobs-key": "bob", "nobodys-key": ""} {
		if name, _ := cfg.Server.KeyName(sha256.Sum256([]byte(key))); name != want {
			t.Errorf("%s: got %q, want %q", key, name, want)
		}
	}
}

// One key under two names has a defined meaning, the first name, so it loads
// with a note instead of failing; the note names the entries, never the key.
func TestParse_AKeyListedTwiceIsANote(t *testing.T) {
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: alice, key: secret}, {name: bob, hash: "+HashKey("secret")+"}]", 1)
	cfg, err := Parse([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := cfg.Server.KeyName(sha256.Sum256([]byte("secret"))); name != "alice" {
		t.Errorf("matched %q, want the first entry", name)
	}
	want := []string{"server.apiKeys: key of 'bob' is the same as 'alice'; requests with it are named 'alice'"}
	if !slices.Equal(cfg.Notes, want) {
		t.Errorf("notes: %q", cfg.Notes)
	}
}

// One key listed twice under one name is a note too, worded for that case,
// since naming the first entry would name the same caller.
func TestParse_AKeyListedTwiceUnderOneNameIsANote(t *testing.T) {
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: alice, key: secret}, {name: alice, hash: "+HashKey("secret")+"}]", 1)
	cfg, err := Parse([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"server.apiKeys: 'alice' has the same key in two entries; the first one will be used"}
	if !slices.Equal(cfg.Notes, want) {
		t.Errorf("notes: %q", cfg.Notes)
	}
}

// A name with several entries is one caller with several keys, such as an old
// and a new key while clients change over, whether the entries are in one
// file or in the config and the secrets file.
func TestParse_ANameCanHaveSeveralKeys(t *testing.T) {
	src := strings.Replace(secretsBase, "apiKeys: [{name: alice, key: alices-key}]", "apiKeys: [{name: alice, key: alices-key}, {name: alice, hash: "+HashKey("alices-phone-key")+"}]", 1)
	secretsFile := "server: {apiKeys: [{name: alice, key: alices-new-key}]}"
	cfg, err := ParseWithSecrets([]byte(src), []byte(secretsFile), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alices-key", "alices-phone-key", "alices-new-key"} {
		if name, _ := cfg.Server.KeyName(sha256.Sum256([]byte(key))); name != "alice" {
			t.Errorf("%s: matched %q, want 'alice'", key, name)
		}
	}
	if len(cfg.Notes) != 0 {
		t.Errorf("notes: %q", cfg.Notes)
	}
}

// Key names, group names and the built-in groups are one namespace, since
// access lists take a name of either kind, and a group's members are keys.
func TestParse_GroupsAreChecked(t *testing.T) {
	cases := []struct{ name, server, want string }{
		{"built-in group", "groups: {all: [a]}", "'all' is a built-in group"},
		{"unknown group", "groups: {unknown: [a]}", "'unknown' is a built-in group"},
		{"a key's name", "groups: {a: [a]}", "already the name of a key"},
		{"unlisted member", "groups: {g: [a, z]}", "'z' is not a name in server.apiKeys"},
		{"key named all", "apiKeys: [{name: all, key: k}]", "reserved for a built-in group"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  "+c.server, 1)
			src = strings.Replace(src, "apiKeys: [{name: a, key: k}]\n  apiKeys:", "apiKeys:", 1)
			if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}

// server.adminApi names only keys and groups the config defines, and never
// the unknown group; a list left out turns the endpoints off.
func TestParse_AdminsAreChecked(t *testing.T) {
	cases := []struct{ name, server, want string }{
		{"undefined key", "adminApi: {write: {allowedKeys: [z]}}", "server.adminApi.write.allowedKeys: 'z' is not a name in server.apiKeys"},
		{"undefined group", "adminApi: {write: {allowedGroups: [g]}}", "server.adminApi.write.allowedGroups: 'g' is not a group"},
		{"unknown group", "adminApi: {write: {allowedGroups: [unknown]}}", "'unknown' can't be an admin"},
		{"unknown key", "adminApi: {writes: {allowedKeys: [a]}}", "unknown key 'writes'"},
		{"a list", "adminApi: {write: [a]}", "write must be an access list, a map with allowedKeys or allowedGroups, such as {allowedKeys: [a]}"},
		{"a name", "adminApi: {write: a}", "write must be an access list, a map with allowedKeys or allowedGroups, such as {allowedKeys: [a]}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  "+c.server, 1)
			if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	for server, on := range map[string]bool{
		"":                                     false,
		"adminApi: {write: {}}":                false,
		"adminApi: {write: {allowedKeys: []}}": true,
		"adminApi: {write: {allowedGroups: [all]}}": true,
	} {
		src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  "+server, 1)
		cfg, err := Parse([]byte(src), env(nil))
		if err != nil {
			t.Fatalf("%q: %v", server, err)
		}
		if got := cfg.Server.AdminAPI.Write.On(); got != on {
			t.Errorf("%q: on %v, want %v", server, got, on)
		}
	}
}

// A listed key is in "all" and its own groups; GroupsOf lists them in that
// order, so the client's groups are the same on every request.
func TestParse_RecordsEachKeysGroups(t *testing.T) {
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k1}, {name: b, key: k2}]\n  groups: {zeta: [a], alpha: [a, b], empty: []}", 1)
	cfg, err := Parse([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{"a": {"all", "alpha", "zeta"}, "b": {"all", "alpha"}, "nobody": nil} {
		if got := cfg.Server.GroupsOf(name); !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	if err := cfg.Server.CheckAccess(Access{AllowedKeys: []string{"a"}, AllowedGroups: []string{"empty", "all", "unknown"}}); err != nil {
		t.Errorf("defined names: %v", err)
	}
	if err := cfg.Server.CheckAccess(Access{AllowedKeys: []string{"alpha"}}); err == nil || !strings.Contains(err.Error(), "'alpha' is not a name in server.apiKeys") {
		t.Errorf("a group in allowedKeys: %v", err)
	}
	if err := cfg.Server.CheckAccess(Access{AllowedGroups: []string{"a"}}); err == nil || !strings.Contains(err.Error(), "'a' is not a group") {
		t.Errorf("a key in allowedGroups: %v", err)
	}
}

// A list left out allows every caller; otherwise a caller is allowed by its
// key's name or by one of its groups, and an empty list allows no one.
func TestAccess_Allows(t *testing.T) {
	alice := []string{"all", "family"}
	unknown := []string{"unknown"}
	cases := []struct {
		name   string
		access Access
		caller string
		groups []string
		want   bool
	}{
		{"left out, listed key", Access{}, "alice", alice, true},
		{"left out, unknown caller", Access{}, "unknown", unknown, true},
		{"by name", Access{AllowedKeys: []string{"alice"}}, "alice", alice, true},
		{"by group", Access{AllowedGroups: []string{"family"}}, "alice", alice, true},
		{"by all", Access{AllowedGroups: []string{"all"}}, "alice", alice, true},
		{"all leaves out unknown callers", Access{AllowedGroups: []string{"all"}}, "unknown", unknown, false},
		{"by unknown", Access{AllowedGroups: []string{"unknown"}}, "", unknown, true},
		{"another key", Access{AllowedKeys: []string{"bob"}}, "alice", alice, false},
		{"empty list", Access{AllowedGroups: []string{}}, "alice", alice, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.access.Allows(c.caller, c.groups); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// Each family has a base path of its own, and the root serves the OpenAI
// family unless the config says otherwise.
func TestParse_APIDefaultsAndChecksBasePaths(t *testing.T) {
	cfg, err := Parse([]byte("server: {listen: x}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.API; a.Default != FamilyOpenAI || a.OpenAI.BasePath != "/openai" || a.Anthropic.BasePath != "/anthropic" {
		t.Errorf("got %+v", a)
	}
	for api, want := range map[string]string{
		"{default: gemini}":                                   "unknown family 'gemini'",
		"{openAi: {basePath: openai}}":                        "is not a path",
		"{openAi: {basePath: /}}":                             "is not a path",
		"{openAi: {basePath: /openai/}}":                      "is not a path",
		"{anthropic: {basePath: /v1/claude}}":                 "is under '/v1'",
		"{anthropic: {basePath: /amf}}":                       "is under '/amf'",
		"{openAi: {basePath: /x}, anthropic: {basePath: /x}}": "both have basePath '/x'",
	} {
		if _, err := Parse([]byte("server: {listen: x}\napi: "+api+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", api, err, want)
		}
	}
}

func TestParse_ExampleConfig(t *testing.T) {
	if _, err := Load("../../config.example.yaml", ""); err != nil {
		t.Fatal(err)
	}
}

// secretsBase is a config whose secrets a secrets file can supply: two
// backends sharing an anchored entry, one key, and a keyMap entry.
const secretsBase = `
server:
  listen: x
  apiKeys: [{name: alice, key: alices-key}]
vars:
  swapBase: &swapBase {type: llamaSwap, baseUrl: "http://x"}
backends:
  one: *swapBase
  two:
    <<: *swapBase
    keyMap: {alice: alices-two-key}
models:
  task: {backend: one}
`

// backendValue decodes the field of a backend entry, as an adapter would.
func backendValue(t *testing.T, cfg *Config, backend string) (apiKey string, keyMap map[string]string) {
	t.Helper()
	entries, err := Entries(&cfg.Backends, "backends")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name != backend {
			continue
		}
		var v struct {
			APIKey string            `yaml:"apiKey"`
			KeyMap map[string]string `yaml:"keyMap"`
		}
		if err := e.Node.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v.APIKey, v.KeyMap
	}
	t.Fatalf("no backend %s", backend)
	return "", nil
}

// A secrets file's values reach the config as if it held them: a backend's
// apiKey and keyMap entries in that backend's entry, beside the config's own,
// and its keys after the config's. An entry it adds to is copied first, so a
// shared anchor still holds what the config wrote, for every other entry.
func TestParse_MergesASecretsFile(t *testing.T) {
	secretsFile := `
server:
  apiKeys: [{name: bob, key: "${BOBS_KEY}"}]
backends:
  one: {apiKey: ones-key}
  two: {apiKey: twos-key, keyMap: {family: family-two-key}}
`
	cfg, err := ParseWithSecrets([]byte(secretsBase), []byte(secretsFile), env(map[string]string{"BOBS_KEY": "bobs-key"}))
	if err != nil {
		t.Fatal(err)
	}
	if apiKey, _ := backendValue(t, cfg, "one"); apiKey != "ones-key" {
		t.Errorf("one: apiKey %q", apiKey)
	}
	apiKey, keyMap := backendValue(t, cfg, "two")
	if apiKey != "twos-key" || keyMap["alice"] != "alices-two-key" || keyMap["family"] != "family-two-key" || len(keyMap) != 2 {
		t.Errorf("two: apiKey %q, keyMap %v", apiKey, keyMap)
	}
	if names := []string{cfg.Server.APIKeys[0].Name, cfg.Server.APIKeys[1].Name}; !slices.Equal(names, []string{"alice", "bob"}) {
		t.Errorf("keys in order %v", names)
	}
	if name, _ := cfg.Server.KeyName(sha256.Sum256([]byte("bobs-key"))); name != "bob" {
		t.Errorf("bob's key from the environment matched %q", name)
	}
	var anchor struct {
		SwapBase map[string]any `yaml:"swapBase"`
	}
	if err := cfg.Vars.Decode(&anchor); err != nil || anchor.SwapBase["apiKey"] != nil {
		t.Errorf("the shared anchor changed: %v, %v", anchor.SwapBase, err)
	}
}

// A secrets file's keyMap entries follow the config's, in the file's order,
// since a keyMap's order decides between the entries of a key's groups.
func TestParse_KeepsTheSecretsFilesKeyMapOrder(t *testing.T) {
	secretsFile := "backends: {two: {keyMap: {family: kf, adults: ka}}}"
	cfg, err := ParseWithSecrets([]byte(secretsBase), []byte(secretsFile), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Entries(&cfg.Backends, "backends")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name != "two" {
			continue
		}
		var v struct {
			KeyMap KeyMap `yaml:"keyMap"`
		}
		if err := e.Node.Decode(&v); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range v.KeyMap {
			names = append(names, entry.Name)
		}
		if !slices.Equal(names, []string{"alice", "family", "adults"}) {
			t.Errorf("keyMap %v", v.KeyMap)
		}
	}
}

// A secrets file holds only secrets, each backend value lives in one of the
// two files, and it can't name a backend the config doesn't define. Errors in
// the file itself are SecretsErrors, so Load names that file.
func TestParse_SecretsFileErrors(t *testing.T) {
	cases := []struct {
		name, secrets, want string
		inSecrets           bool
	}{
		{"a setting", "server: {listen: y}", "unknown key 'listen'", true},
		{"a backend setting", "backends: {one: {baseUrl: y}}", "unknown key 'baseUrl'", true},
		{"a section", "models: {}", "unknown key 'models'", true},
		{"an undefined backend", "backends: {three: {apiKey: k}}", "the config defines no backend 'three'", true},
		{"an undefined variable", "backends: {one: {apiKey: \"${NOPE}\"}}", "environment variable NOPE is not defined", true},
		{"a bad hash", "server: {apiKeys: [{name: bob, hash: nope}]}", "server.apiKeys[0] ('bob'): hash must be", true},
		{"an apiKey in both", "backends: {one: {apiKey: a}}", "", false},
		{"a keyMap entry in both", "backends: {two: {keyMap: {alice: other}}}", "backends.two.keyMap.alice is in both", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := secretsBase
			if c.name == "an apiKey in both" {
				base = strings.Replace(base, "  one: *swapBase", "  one: {<<: *swapBase, apiKey: own}", 1)
				c.want = "backends.one.apiKey is in both"
			}
			_, err := ParseWithSecrets([]byte(base), []byte(c.secrets), env(nil))
			var inSecrets *SecretsError
			if err == nil || !strings.Contains(err.Error(), c.want) || errors.As(err, &inSecrets) != c.inSecrets {
				t.Errorf("got %v (in the secrets file: %v), want %q", err, errors.As(err, &inSecrets), c.want)
			}
		})
	}
}

// Load names the file an error is in: the secrets file for an error there,
// the config for one in the config.
func TestLoad_NamesTheFileAnErrorIsIn(t *testing.T) {
	dir := t.TempDir()
	configPath, secretsPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "secrets.yaml")
	os.WriteFile(configPath, []byte(secretsBase), 0o600)
	os.WriteFile(secretsPath, []byte("backends: {one: {baseUrl: y}}"), 0o600)
	if _, err := Load(configPath, secretsPath); err == nil || !strings.HasPrefix(err.Error(), secretsPath+": ") {
		t.Errorf("got %v", err)
	}
	os.WriteFile(secretsPath, []byte("backends: {one: {apiKey: k}}"), 0o600)
	if _, err := Load(configPath, secretsPath); err != nil {
		t.Errorf("a good secrets file: %v", err)
	}
	if _, err := Load(configPath, filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing secrets file loaded")
	}
}

// A single name where a list of names goes is refused with the key and the
// list it should be, not Decode's "cannot unmarshal !!str into []string".
func TestDecodeStrict_OneNameWhereAListGoes(t *testing.T) {
	var facade struct {
		Access `yaml:",inline"`
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("allowedKeys: alice\n"), &node); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(node.Content[0], &facade); err == nil || err.Error() != "line 1: allowedKeys must be a list, such as [alice]" {
		t.Errorf("inline access list: err = %v", err)
	}
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: alice, key: k}]\n  groups: {family: alice}", 1)
	if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.Contains(err.Error(), "family must be a list, such as [alice]") {
		t.Errorf("group: err = %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(src, "family: alice", "family: [alice]", 1)), env(nil)); err != nil {
		t.Errorf("a list: %v", err)
	}
}

// A backend's entry from BackendEntry is the one a gateway builds an
// adapter from, so testing that adapter tests what serve would run: an
// alias and a merge read through, ${NAME} expanded, and the secrets file's
// apiKey and keyMap in place. A name the config doesn't define is
// EditNotFound, which the admin API answers with 404.
func TestBackendEntry_IsTheEntryAGatewayBuilds(t *testing.T) {
	main := `
server:
  listen: x
  apiKeys: [{name: alice, key: alices-key}]
vars:
  host: &host "http://${SWAP_HOST}:8080"
backends:
  swap:
    type: llamaSwap
    baseUrl: *host
    keyMap: {alice: alices-swap-key}
models:
  task: {backend: swap}
`
	secrets := `
backends:
  swap: {apiKey: swaps-key, keyMap: {bob: bobs-swap-key}}
`
	d, err := ReadDocument([]byte(main), []byte(secrets))
	if err != nil {
		t.Fatal(err)
	}
	e, err := d.BackendEntry("swap", env(map[string]string{"SWAP_HOST": "swap"}))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		BaseURL string            `yaml:"baseUrl"`
		APIKey  string            `yaml:"apiKey"`
		KeyMap  map[string]string `yaml:"keyMap"`
	}
	if err := e.Node.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if e.Name != "swap" || e.Type != "llamaSwap" || v.BaseURL != "http://swap:8080" || v.APIKey != "swaps-key" || v.KeyMap["alice"] != "alices-swap-key" || v.KeyMap["bob"] != "bobs-swap-key" {
		t.Errorf("entry %s of type %s: %+v", e.Name, e.Type, v)
	}
	var edit *EditError
	if _, err := d.BackendEntry("other", env(map[string]string{"SWAP_HOST": "swap"})); !errors.As(err, &edit) || edit.Kind != EditNotFound {
		t.Errorf("an undefined backend: %v", err)
	}
}

// The admin UI's path defaults to /amf/ui, and may be any path that no
// other route overlaps, on whichever listener serves it, since a change of
// server.adminApi.listen waits for a restart. The root needs a listener of
// its own, since server.listen serves the client APIs there.
func TestParse_UIBasePath(t *testing.T) {
	base := strings.Replace(minimal, "  listen: 0.0.0.0:8090\n", "  listen: 0.0.0.0:8090\n  adminApi: {%s}\n", 1)
	for _, c := range []struct {
		name, adminAPI, want, err string
	}{
		{"left out", ``, "/amf/ui", ""},
		{"a path of its own", `uiBasePath: /admin`, "/admin", ""},
		{"the root on a listener of its own", `listen: 0.0.0.0:8091, uiBasePath: /`, "/", ""},
		{"the default, given", `uiBasePath: /amf/ui`, "/amf/ui", ""},
		{"the root on server.listen", `uiBasePath: /`, "", "'/' needs server.adminApi.listen"},
		{"a trailing slash", `uiBasePath: /admin/`, "", "is not a path"},
		{"no leading slash", `uiBasePath: admin`, "", "is not a path"},
		{"under /amf", `uiBasePath: /amf/admin`, "", "under '/amf'"},
		{"under /v1", `uiBasePath: /v1/ui`, "", "overlaps '/v1'"},
		{"a client API's basePath", `uiBasePath: /openai`, "", "overlaps '/openai'"},
		{"above a client API's basePath", `uiBasePath: /x`, "", "overlaps '/x/anthropic'"},
		{"/health", `uiBasePath: /health`, "", "overlaps '/health'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(base, "%s", c.adminAPI, 1)
			if c.name == "above a client API's basePath" {
				src += "api: {anthropic: {basePath: /x/anthropic}}\n"
			}
			cfg, err := Parse([]byte(src), env(nil))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("got %v, want an error containing %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.AdminAPI.UIBasePath != c.want {
				t.Errorf("uiBasePath %q, want %q", cfg.Server.AdminAPI.UIBasePath, c.want)
			}
		})
	}
}
