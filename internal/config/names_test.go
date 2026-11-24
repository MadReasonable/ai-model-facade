package config

import (
	"strings"
	"testing"
)

// A name is letters, combining marks and digits of any script, and @ - _ . :,
// starting with a letter; a facade name may also contain /, for model IDs such
// as org/model.
func TestCheckName_AcceptsNamesInAnyScript(t *testing.T) {
	cases := []struct {
		name, value string
		facade      bool
	}{
		{"ascii", "task", false},
		{"punctuation", "a@b_c-d.e:f", false},
		{"model ID with a tag", "llama3.1:8b", false},
		{"precomposed accent", "caf\u00e9", false},
		{"decomposed accent", "cafe\u0301", false},
		{"stacked marks", "e\u0323\u0302", false},
		{"devanagari with vowel marks", "हिन्दी", false},
		{"han", "東京", false},
		{"hebrew", "עברית", false},
		{"arabic with a digit", "مدل-1", false},
		{"arabic-indic digit", "مدل٣", false},
		{"persian with a zero-width non-joiner", "می\u200cخواهم", false},
		{"longest", strings.Repeat("a", maxNameLength), false},
		{"longest, counted in code points", strings.Repeat("\u0915", maxNameLength), false},
		{"facade with a slash", "openai/gpt-4o", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkName(c.value, c.facade); err != nil {
				t.Errorf("got %v", err)
			}
		})
	}
}

// A name that breaks the rule is refused with the character that breaks it,
// written so that it shows even if invisible, and never raw in the message.
func TestCheckName_RefusesWithTheCharacter(t *testing.T) {
	cases := []struct {
		name, value string
		facade      bool
		want        string
	}{
		{"empty", "", false, "a name can't be empty"},
		{"leading digit", "1abc", false, "'1abc' starts with '1'; a name starts with a letter"},
		{"leading punctuation", "_x", false, "'_x' starts with '_'"},
		{"leading mark", "\u0301a", false, "'\\u0301a' starts with U+0301"},
		{"space", "a b", false, "'a b' contains U+0020; a name contains only letters, combining marks, digits and @ - _ . :"},
		{"escape", "a\x1b[2Jb", false, "'a\\x1b[2Jb' contains U+001B"},
		{"bidi override", "a\u202eb", false, "'a\\u202eb' contains U+202E"},
		{"zero-width space", "a\u200bb", false, "'a\\u200bb' contains U+200B"},
		{"non-joiner between latin letters", "a\u200cb", false, "'a\\u200cb' contains U+200C, a zero-width non-joiner, which a name can contain only between two Arabic-script letters"},
		{"non-joiner at the end", "می\u200c", false, "contains U+200C"},
		{"non-joiner before a latin letter", "م\u200ca", false, "contains U+200C"},
		{"superscript digit", "a\u00b2", false, "'a\u00b2' contains '\u00b2'"},
		{"slash in a backend name", "org/model", false, "'org/model' contains '/'"},
		{"slash listed for a facade", "a b", true, "digits and @ - _ . : /"},
		{"too long", strings.Repeat("a", maxNameLength+1), false, "is 65 characters long; a name is at most 64"},
		{"too long, counted in code points", strings.Repeat("\u0915", maxNameLength+1), false, "is 65 characters long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkName(c.value, c.facade)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.ContainsAny(err.Error(), "\x1b\u202e\u200b\u200c\u0301") {
				t.Errorf("message contains a character raw: %q", err)
			}
		})
	}
}

// QuoteName shows a valid name as it is, and escapes what isn't visible on
// its own, so a wrong reference in an error can't act on a terminal.
func TestQuoteName(t *testing.T) {
	cases := map[string]string{
		"task":          "'task'",
		"עברית":         "'עברית'",
		"a b":           "'a b'",
		"a\x1bb":        `'a\x1bb'`,
		"a\u00a0b":      `'a\u00a0b'`,
		"a\u200bb":      `'a\u200bb'`,
		"it's a\\b":     `'it\'s a\\b'`,
		"\U000e0001tag": `'\U000e0001tag'`,
	}
	for in, want := range cases {
		if got := QuoteName(in); got != want {
			t.Errorf("QuoteName(%q) = %s, want %s", in, got, want)
		}
	}
}

// Every place the config defines a name checks it, with the place in the
// error; only models, the facades, take '/'.
func TestParse_DefinedNamesAreChecked(t *testing.T) {
	cases := []struct{ name, from, to, want string }{
		{"key", "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: 9a, key: k}]", "server.apiKeys[0].name: '9a' starts with '9'"},
		{"group", "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  groups: {\"g\\x1b\": [a]}", `server.groups: 'g\x1b' contains U+001B`},
		{"backend", "  llamaSwap: {type", "  org/swap: {type: llamaSwap, baseUrl: \"http://y\"}\n  llamaSwap: {type", "line 5: backends: 'org/swap' contains '/'"},
		{"facade", "  task: {backend", "  \"t\\u200bask\": {backend: llamaSwap}\n  task: {backend", `line 7: models: 't\u200bask' contains U+200B`},
		{"empty group", "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  groups: {\"\": [a]}", "server.groups: a name can't be empty"},
		{"empty facade", "  task: {backend", "  \"\": {backend: llamaSwap}\n  task: {backend", "line 7: models: a name can't be empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimal, c.from, c.to, 1)
			d, err := ReadDocument([]byte(src), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = d.CheckSchema(env(nil), func(string, Entry) error { return nil })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	src := strings.Replace(minimal, "  task: {backend", "  org/model: {backend: llamaSwap}\n  task: {backend", 1)
	d, err := ReadDocument([]byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckSchema(env(nil), func(string, Entry) error { return nil }); err != nil {
		t.Errorf("facade named org/model: %v", err)
	}
}

// A reference is checked only against the names defined, so the error for
// one that matches none quotes it with its invisible characters escaped,
// wherever the config refers to a key or a group.
func TestParse_AWrongReferenceIsQuotedEscaped(t *testing.T) {
	cases := []struct{ name, server, want string }{
		{"group member", `groups: {g: ["a\u200b"]}`, `server.groups.g: 'a\u200b' is not a name in server.apiKeys`},
		{"allowedKeys", `adminApi: {write: {allowedKeys: ["a\e"]}}`, `server.adminApi.write.allowedKeys: 'a\x1b' is not a name in server.apiKeys`},
		{"allowedGroups", `adminApi: {write: {allowedKeys: [a]}, read: {allowedGroups: ["g\u202e"]}}`, `server.adminApi.read.allowedGroups: 'g\u202e' is not a group`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: k}]\n  "+c.server, 1)
			if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}
