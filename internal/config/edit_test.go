package config

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const editMain = `# the gateway
server:
  listen: 127.0.0.1:8088
  apiKeys:
    - name: alice
      hash: sha256:` + "2bd806c97f0e00af1a1fc3328fa763a9269723c8db8fac4f93af71db186d6e90" + `
    - name: bob
      key: bob-key # bob's key
vars:
  noThinking: &noThinking
    chat_template_kwargs: {enable_thinking: false}
  shared: &shared
    type: openAi
    baseUrl: http://shared:8080/v1
backends:
  local:
    type: llamaSwap
    baseUrl: http://llama:8080
    apiKey: ${LOCAL_KEY}
  cloud:
    type: openAi
    baseUrl: https://cloud/v1
  mirror: *shared
models:
  assistant:
    backend: local
    params:
      openAiChat: *noThinking
    candidates: [a, b]
`

const editSecrets = `# secrets
server:
  apiKeys:
    - {name: carol, key: carol-key}
backends:
  cloud:
    apiKey: sk-cloud # cloud's key
    keyMap:
      alice: sk-alice
  mirror:
    apiKey: sk-mirror
`

func readEditDocument(t *testing.T, secrets bool) *Document {
	t.Helper()
	var secretsData []byte
	if secrets {
		secretsData = []byte(editSecrets)
	}
	d, err := ReadDocument([]byte(editMain), secretsData)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func encoded(t *testing.T, d *Document) (string, string) {
	t.Helper()
	main, secrets, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(main), string(secrets)
}

func viewYAML(t *testing.T, d *Document, path Path) string {
	t.Helper()
	n, err := d.View(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeYAML(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestView_MergesTheSecretsFileAndHidesEverySecret(t *testing.T) {
	d := readEditDocument(t, true)
	view := viewYAML(t, d, nil)
	for _, want := range []string{
		"hash: " + HashKey("bob-key"),
		HashKey("carol-key"),
		"apiKey: ${LOCAL_KEY}",
		"apiKey: <redacted> # cloud's key",
		"alice: <redacted>",
		"openAiChat: *noThinking",
		"# bob's key",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	for _, secret := range []string{"bob-key", "carol-key", "sk-cloud", "sk-alice", "sk-mirror"} {
		if strings.Contains(view, secret+"\n") || strings.Contains(view, secret+" ") {
			t.Errorf("view shows %s:\n%s", secret, view)
		}
	}
	// The aliased backend got the secrets file's apiKey through a copy, so
	// the anchor it shares is unchanged.
	if !strings.Contains(viewYAML(t, d, Path{"vars", "shared"}), "baseUrl: http://shared:8080/v1\n") || strings.Contains(viewYAML(t, d, Path{"vars", "shared"}), "apiKey") {
		t.Errorf("vars.shared changed:\n%s", viewYAML(t, d, Path{"vars", "shared"}))
	}
}

func TestMarshalJSON_ResolvesAliasesAndMerges(t *testing.T) {
	d := readEditDocument(t, false)
	n, err := d.View(Path{"models", "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalJSON(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "backend": "local",
  "params": {
    "openAiChat": {
      "chat_template_kwargs": {
        "enable_thinking": false
      }
    }
  },
  "candidates": [
    "a",
    "b"
  ]
}
`
	if string(data) != want {
		t.Errorf("JSON:\n%s\nwant:\n%s", data, want)
	}
}

func TestPut_ChangesOnlyTheNodeAndKeepsBothFiles(t *testing.T) {
	d := readEditDocument(t, true)
	mainBefore, secretsBefore := encoded(t, d)
	edited, created, notes, err := d.Put(Path{"models", "assistant", "description"}, []byte(`"Answers questions"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(notes) != 0 {
		t.Errorf("created %v, notes %v", created, notes)
	}
	main, secrets := encoded(t, edited)
	if secrets != secretsBefore {
		t.Errorf("secrets file changed:\n%s\nwas:\n%s", secrets, secretsBefore)
	}
	if want := strings.Replace(mainBefore, "    candidates: [a, b]\n", "    candidates: [a, b]\n    description: Answers questions\n", 1); main != want {
		t.Errorf("main config:\n%s\nwant:\n%s", main, want)
	}
	if mainOld, _ := encoded(t, d); mainOld != mainBefore {
		t.Error("Put changed the document it was called on")
	}
}

func TestPut_WhatGETShowsKeepsEverySecretWhereItIs(t *testing.T) {
	d := readEditDocument(t, true)
	for _, path := range []Path{nil, {"server"}, {"server", "apiKeys"}, {"backends"}, {"backends", "cloud"}, {"backends", "mirror"}} {
		for _, format := range []Format{JSON, YAML} {
			n, err := d.View(path)
			if err != nil {
				t.Fatal(err)
			}
			var body []byte
			if format == JSON {
				body, err = MarshalJSON(n)
			} else {
				body, err = encodeYAML(n)
			}
			if err != nil {
				t.Fatal(err)
			}
			edited, _, _, err := d.Put(path, body, format)
			if err != nil {
				t.Fatalf("%s, format %d: %v", path, format, err)
			}
			if format == YAML && !edited.Equal(d) {
				m1, s1 := encoded(t, edited)
				m0, s0 := encoded(t, d)
				t.Errorf("%s: a PUT of the YAML GET shows changed the config:\n%s\n%s\nwas:\n%s\n%s", path, m1, s1, m0, s0)
			}
			cfg, err := edited.Parse(func(string) (string, bool) { return "k", true })
			if err != nil {
				t.Fatalf("%s, format %d: %v", path, format, err)
			}
			if name, _ := cfg.Server.KeyName(digestOf("carol-key")); name != "carol" {
				t.Errorf("%s, format %d: carol's key is lost", path, format)
			}
			if !strings.Contains(string(mustSecrets(t, edited)), "sk-cloud") || !strings.Contains(string(mustSecrets(t, edited)), "sk-mirror") {
				t.Errorf("%s, format %d: a backend secret left the secrets file:\n%s", path, format, mustSecrets(t, edited))
			}
		}
	}
}

func mustSecrets(t *testing.T, d *Document) []byte {
	_, secrets := encoded(t, d)
	return []byte(secrets)
}

func digestOf(key string) [32]byte {
	var k APIKey
	k.Key = &key
	digest, _ := k.digest()
	return digest
}

func TestPut_NewSecretGoesToTheSecretsFile(t *testing.T) {
	d := readEditDocument(t, true)
	edited, _, notes, err := d.Put(Path{"backends", "cloud", "keyMap", "bob"}, []byte(`"sk-bob"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	main, secrets := encoded(t, edited)
	if strings.Contains(main, "sk-bob") || !strings.Contains(secrets, "bob: sk-bob") || len(notes) != 0 {
		t.Errorf("notes %v, main:\n%s\nsecrets:\n%s", notes, main, secrets)
	}
	edited, _, _, err = d.Put(Path{"backends", "cloud", "apiKey"}, []byte(`"sk-new"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, secrets := encoded(t, edited); !strings.Contains(secrets, "apiKey: sk-new # cloud's key") {
		t.Errorf("a changed secret didn't stay in its file, with its comment:\n%s", secrets)
	}
	// Without a secrets file, it goes to the main config, with a note.
	d = readEditDocument(t, false)
	edited, _, notes, err = d.Put(Path{"backends", "cloud", "apiKey"}, []byte(`"sk-new"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if main, _ := encoded(t, edited); !strings.Contains(main, "apiKey: sk-new") || len(notes) != 1 || !strings.Contains(notes[0], "plaintext") {
		t.Errorf("notes %v, main:\n%s", notes, main)
	}
}

func TestPut_LiteralKeyBecomesItsHash(t *testing.T) {
	d := readEditDocument(t, true)
	edited, _, _, err := d.Put(Path{"server", "apiKeys"}, []byte(`[{"name": "dave", "key": "dave-key"}]`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	main, secrets := encoded(t, edited)
	if strings.Contains(main, "dave-key") || !strings.Contains(main, "hash: "+HashKey("dave-key")) {
		t.Errorf("main config:\n%s", main)
	}
	if strings.Contains(secrets, "carol") {
		t.Errorf("carol's entry, left out of the list, is still in the secrets file:\n%s", secrets)
	}
}

func editKind(err error) int {
	var e *EditError
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

func TestPut_RefusesAnEditThroughAnAlias(t *testing.T) {
	d := readEditDocument(t, true)
	_, _, _, err := d.Put(Path{"models", "assistant", "params", "openAiChat", "temperature"}, []byte(`0.2`), JSON)
	if editKind(err) != EditConflict || !strings.Contains(err.Error(), "vars.noThinking") {
		t.Errorf("err = %v", err)
	}
	_, _, _, err = d.Put(Path{"backends", "mirror", "baseUrl"}, []byte(`"http://other/v1"`), JSON)
	if editKind(err) != EditConflict || !strings.Contains(err.Error(), "vars.shared") {
		t.Errorf("err = %v", err)
	}
	// Replacing the alias itself gives the facade a value of its own.
	edited, _, _, err := d.Put(Path{"models", "assistant", "params", "openAiChat"}, []byte(`{"temperature": 0.2}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if main, _ := encoded(t, edited); !strings.Contains(main, "openAiChat:\n        temperature: 0.2") || !strings.Contains(main, "noThinking: &noThinking") {
		t.Errorf("main config:\n%s", main)
	}
	// The secrets of an aliased backend can be edited; the alias stays.
	edited, _, _, err = d.Put(Path{"backends", "mirror", "apiKey"}, []byte(`"sk-mirror-2"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if main, secrets := encoded(t, edited); !strings.Contains(main, "mirror: *shared") || !strings.Contains(secrets, "apiKey: sk-mirror-2") {
		t.Errorf("main:\n%s\nsecrets:\n%s", main, secrets)
	}
}

func TestPut_YAMLBodyMayUseTheConfigsAnchors(t *testing.T) {
	d := readEditDocument(t, false)
	body := "backend: local\nparams:\n  openAiChat: *noThinking\ncandidates: [c]\n"
	edited, created, _, err := d.Put(Path{"models", "helper"}, []byte(body), YAML)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("created = false")
	}
	main, _ := encoded(t, edited)
	if !strings.Contains(main, "  helper:\n    backend: local\n    params:\n      openAiChat: *noThinking\n") {
		t.Errorf("main config:\n%s", main)
	}
	if _, _, _, err := d.Put(Path{"models", "helper"}, []byte("a: *missing\n"), YAML); editKind(err) != EditInvalid || !strings.Contains(err.Error(), "'missing'") {
		t.Errorf("err = %v", err)
	}
	if _, _, _, err := d.Put(Path{"models", "helper"}, []byte("x: &noThinking 1\n"), YAML); editKind(err) != EditInvalid || !strings.Contains(err.Error(), "vars.noThinking") {
		t.Errorf("err = %v", err)
	}
}

func TestPut_ReplacingAnAnchorKeepsItsUsers(t *testing.T) {
	d := readEditDocument(t, false)
	edited, _, _, err := d.Put(Path{"vars", "noThinking"}, []byte(`{"temperature": 0}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	main, _ := encoded(t, edited)
	if !strings.Contains(main, "noThinking: &noThinking\n    temperature: 0") || !strings.Contains(main, "openAiChat: *noThinking") {
		t.Errorf("main config:\n%s", main)
	}
	if _, err := d.Delete(Path{"vars", "noThinking"}); editKind(err) != EditConflict || !strings.Contains(err.Error(), "models.assistant.params.openAiChat") {
		t.Errorf("err = %v", err)
	}
}

func TestPut_Paths(t *testing.T) {
	d := readEditDocument(t, false)
	for _, tc := range []struct {
		path Path
		kind int
	}{
		{Path{"nothing", "here"}, EditNotFound},
		{Path{"models", "assistant", "candidates", "0"}, EditNotFound},
		{Path{"server", "listen", "port"}, EditNotFound},
	} {
		if _, _, _, err := d.Put(tc.path, []byte(`1`), JSON); editKind(err) != tc.kind {
			t.Errorf("%s: err = %v", tc.path, err)
		}
	}
	edited, created, _, err := d.Put(Path{"server", "listen"}, []byte(`"0.0.0.0:8088"`), JSON)
	if err != nil || created {
		t.Fatalf("created %v, err %v", created, err)
	}
	if main, _ := encoded(t, edited); !strings.Contains(main, `listen: 0.0.0.0:8088`) {
		t.Errorf("main config:\n%s", main)
	}
	edited, err = d.Delete(Path{"backends", "cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if main, _ := encoded(t, edited); strings.Contains(main, "cloud:") {
		t.Errorf("main config:\n%s", main)
	}
	if _, err := d.Delete(Path{"backends", "nothing"}); editKind(err) != EditNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestDelete_BackendTakesItsSecretsWithIt(t *testing.T) {
	d := readEditDocument(t, true)
	edited, err := d.Delete(Path{"backends", "cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if _, secrets := encoded(t, edited); strings.Contains(secrets, "cloud") || !strings.Contains(secrets, "sk-mirror") {
		t.Errorf("secrets file:\n%s", secrets)
	}
	if _, err := edited.Parse(func(string) (string, bool) { return "k", true }); err != nil {
		t.Error(err)
	}
}

func TestETag_ChangesWithTheNode(t *testing.T) {
	d := readEditDocument(t, true)
	before, ok := d.ETag(Path{"backends", "cloud"})
	if !ok {
		t.Fatal("no ETag")
	}
	other, _ := d.ETag(Path{"models"})
	whole, _ := d.ETag(nil)
	edited, _, _, err := d.Put(Path{"backends", "cloud", "apiKey"}, []byte(`"sk-other"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := edited.ETag(Path{"backends", "cloud"}); after == before {
		t.Error("a changed secret left its backend's ETag unchanged")
	}
	if after, _ := edited.ETag(Path{"models"}); after != other {
		t.Error("an edit of a backend changed the ETag of models")
	}
	if after, _ := edited.ETag(nil); after == whole {
		t.Error("an edit left the config's ETag unchanged")
	}
	if _, ok := d.ETag(Path{"models", "nothing"}); ok {
		t.Error("a path with no node has an ETag")
	}
}

const mergeMain = `server:
  listen: 127.0.0.1:8088
vars:
  common: &common
    backend: local
    description: Shared
    params: {temperature: 0.2}
backends:
  local: {type: llamaSwap, baseUrl: "http://llama:8080"}
models:
  assistant:
    <<: *common
    description: Mine
    candidates: [a]
`

// An edit that passes through a key a merge brings is refused, naming the
// merge, since it would change what every entry sharing it gets; a value
// given beside the merge overrides it for that entry alone.
func TestPut_RefusesAnEditThroughAMerge(t *testing.T) {
	d, err := ReadDocument([]byte(mergeMain), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = d.Put(Path{"models", "assistant", "params", "temperature"}, []byte(`0.7`), JSON)
	if editKind(err) != EditConflict || !strings.Contains(err.Error(), "merge") || !strings.Contains(err.Error(), "vars.common") {
		t.Errorf("err = %v", err)
	}
	if _, err := d.Delete(Path{"models", "assistant", "params"}); editKind(err) != EditConflict || !strings.Contains(err.Error(), "vars.common") {
		t.Errorf("delete: err = %v", err)
	}
	edited, created, _, err := d.Put(Path{"models", "assistant", "params"}, []byte(`{"temperature": 0.7}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("overriding a merged key counts as a new one")
	}
	main, _ := encoded(t, edited)
	if !strings.Contains(main, "params: {temperature: 0.2}") || !strings.Contains(main, "    params:\n      temperature: 0.7") {
		t.Errorf("main config:\n%s", main)
	}
}

// JSON has no merges, so the JSON view gives each entry the pairs a merge
// brings, in its place, with the entry's own keys winning.
func TestMarshalJSON_ExpandsMerges(t *testing.T) {
	d, err := ReadDocument([]byte(mergeMain), nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := d.View(Path{"models", "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalJSON(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"backend":"local","params":{"temperature":0.2},"description":"Mine","candidates":["a"]}`
	if compact := strings.Join(strings.Fields(string(data)), ""); compact != want {
		t.Errorf("JSON: %s\nwant: %s", compact, want)
	}
}

// A secret of a backend the edit adds goes to the secrets file, as an entry
// of its own there, and a secrets file that was empty gets its first one.
func TestPut_NewBackendsSecretsGoToTheSecretsFile(t *testing.T) {
	d := readEditDocument(t, true)
	edited, _, notes, err := d.Put(Path{"backends", "extra"}, []byte(`{"type": "openAi", "baseUrl": "https://extra/v1", "apiKey": "sk-extra", "keyMap": {"alice": "sk-extra-alice"}}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	main, secrets := encoded(t, edited)
	if strings.Contains(main, "sk-extra") || !strings.Contains(main, "baseUrl: https://extra/v1") || len(notes) != 0 {
		t.Errorf("notes %v, main config:\n%s", notes, main)
	}
	if !strings.Contains(secrets, "  extra:\n    apiKey: sk-extra\n    keyMap:\n      alice: sk-extra-alice\n") {
		t.Errorf("secrets file:\n%s", secrets)
	}
	if _, err := edited.Parse(func(string) (string, bool) { return "k", true }); err != nil {
		t.Error(err)
	}

	empty, err := ReadDocument([]byte(editMain), []byte(""))
	if err != nil {
		t.Fatal(err)
	}
	edited, _, _, err = empty.Put(Path{"backends", "cloud", "apiKey"}, []byte(`"sk-first"`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if main, secrets := encoded(t, edited); strings.Contains(main, "sk-first") || secrets != "backends:\n  cloud:\n    apiKey: sk-first\n" {
		t.Errorf("main config:\n%s\nsecrets file:\n%s", main, secrets)
	}
}

// An error in a YAML body names its line in the body as sent, though the
// body is parsed after a list of the config's anchors; a body with a
// document marker is parsed as it is.
func TestPut_YAMLBodyErrorsNameTheBodysLine(t *testing.T) {
	d := readEditDocument(t, false)
	body := []byte("backend: local\ncandidates: [a\nparams: {}\n")
	var plain yaml.Node
	want := bodyLine.FindString(yaml.Unmarshal(body, &plain).Error())
	_, _, _, err := d.Put(Path{"models", "helper"}, body, YAML)
	if editKind(err) != EditInvalid || want == "" || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v; want %q", err, want)
	}
	edited, _, _, err := d.Put(Path{"models", "helper"}, []byte("---\nbackend: local\ncandidates: [c]\n"), YAML)
	if err != nil {
		t.Fatal(err)
	}
	if main, _ := encoded(t, edited); !strings.Contains(main, "  helper:\n    backend: local\n    candidates: [c]\n") {
		t.Errorf("main config:\n%s", main)
	}
}

// An edit that leaves a map or a list as it was leaves its style too, even
// from JSON, which has none, so a no-op edit doesn't reformat the file.
func TestPut_KeepsTheStyleOfWhatItDoesntChange(t *testing.T) {
	d := readEditDocument(t, false)
	edited, _, _, err := d.Put(Path{"vars", "noThinking"}, []byte(`{"chat_template_kwargs": {"enable_thinking": false}}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if !edited.Equal(d) {
		main, _ := encoded(t, edited)
		t.Errorf("main config:\n%s", main)
	}
	edited, _, _, err = d.Put(Path{"vars", "noThinking"}, []byte(`{"chat_template_kwargs": {"enable_thinking": true}}`), JSON)
	if err != nil {
		t.Fatal(err)
	}
	if main, _ := encoded(t, edited); !strings.Contains(main, "chat_template_kwargs:\n      enable_thinking: true") {
		t.Errorf("a changed map keeps its old style:\n%s", main)
	}
}

// A copy with no secrets file, fitted to a gateway that has one, has each
// literal secret moved to the secrets file, a ${NAME} and a hash left where
// they are; one with a secrets file, fitted to a gateway without, has the
// file merged into the main config. notes name each secret placed.
func TestFit_PutsSecretsWhereTheGatewayKeepsThem(t *testing.T) {
	d := readEditDocument(t, false)
	fitted, notes, err := d.Fit(true)
	if err != nil {
		t.Fatal(err)
	}
	main, secrets := encoded(t, fitted)
	if strings.Contains(main, "bob-key") || !strings.Contains(main, "apiKey: ${LOCAL_KEY}") || !strings.Contains(main, "name: alice") {
		t.Errorf("main config:\n%s", main)
	}
	if !strings.Contains(secrets, "key: bob-key") || strings.Contains(secrets, "LOCAL_KEY") || strings.Contains(secrets, "alice") {
		t.Errorf("secrets file:\n%s", secrets)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "'bob' moves to the secrets file") {
		t.Errorf("notes: %v", notes)
	}
	if unchanged, _, _ := fitted.Fit(true); unchanged != fitted {
		t.Error("a copy already in the layout was changed")
	}

	d = readEditDocument(t, true)
	merged, notes, err := d.Fit(false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Secrets != nil {
		t.Fatal("the merged copy still has a secrets file")
	}
	main, _ = encoded(t, merged)
	for _, want := range []string{"key: carol-key", "apiKey: sk-cloud", "alice: sk-alice", "apiKey: sk-mirror"} {
		if !strings.Contains(main, want) {
			t.Errorf("main config lacks %q:\n%s", want, main)
		}
	}
	if len(notes) != 4 || !strings.Contains(strings.Join(notes, "\n"), "in plaintext") {
		t.Errorf("notes: %v", notes)
	}
	if _, err := merged.Parse(func(string) (string, bool) { return "k", true }); err != nil {
		t.Error(err)
	}
}

// server.adminApi.configBackups may be 0, to keep none, but not negative.
func TestParse_ConfigBackups(t *testing.T) {
	for value, want := range map[string]string{"0": "", "3": "", "-1": "server.adminApi.configBackups: -1 is negative"} {
		src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  adminApi: {configBackups: "+value+"}", 1)
		_, err := Parse([]byte(src), env(nil))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want %q", value, err, want)
		}
	}
}
