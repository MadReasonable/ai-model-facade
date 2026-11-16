package config

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeKeyMap decodes src's keyMap as a backend entry's is, through
// DecodeStrict, so the strict check passes over it too.
func decodeKeyMap(t *testing.T, src string) (KeyMap, error) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Shared yaml.Node `yaml:"shared"` // a place for an anchor
		KeyMap KeyMap    `yaml:"keyMap"`
	}
	err := DecodeStrict(doc.Content[0], &v)
	return v.KeyMap, err
}

// A keyMap keeps the order its entries are written in, which decides between
// a key's groups.
func TestKeyMap_KeepsTheWrittenOrder(t *testing.T) {
	m, err := decodeKeyMap(t, "keyMap: {zeta: kz, alpha: ka, mid: km}")
	if err != nil {
		t.Fatal(err)
	}
	want := KeyMap{{Name: "zeta", Key: "kz"}, {Name: "alpha", Key: "ka"}, {Name: "mid", Key: "km"}}
	if !slices.Equal(m, want) {
		t.Errorf("got %v", m)
	}
	if key, ok := m.Key("alpha"); !ok || key != "ka" {
		t.Errorf("Key: got %q, %v", key, ok)
	}
}

// Entries merged with `<<:` take the merge's place, and an entry the mapping
// gives itself overrides a merged one, as decoding a map would.
func TestKeyMap_PlacesMergedEntriesWhereTheMergeIs(t *testing.T) {
	m, err := decodeKeyMap(t, `
shared: &shared {family: shared-family, adults: shared-adults}
keyMap:
  alice: ka
  <<: *shared
  family: own-family
`)
	if err != nil {
		t.Fatal(err)
	}
	want := KeyMap{{Name: "alice", Key: "ka"}, {Name: "adults", Key: "shared-adults"}, {Name: "family", Key: "own-family"}}
	if !slices.Equal(m, want) {
		t.Errorf("got %v", m)
	}
}

// A name listed twice, or a value that isn't a key, is a config error; a null
// value is no key, which the client refuses as empty.
func TestKeyMap_RefusesWhatIsntAMapOfKeys(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"a name twice", "keyMap: {alice: k1, alice: k2}", "keyMap.alice is listed twice"},
		{"a list value", "keyMap: {alice: [k1]}", "keyMap.alice must be a key"},
		{"not a map", "keyMap: [alice]", "keyMap must map key and group names to keys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decodeKeyMap(t, c.src); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	m, err := decodeKeyMap(t, "keyMap: {alice: ~, bob: null}")
	if err != nil || !slices.Equal(m, KeyMap{{Name: "alice"}, {Name: "bob"}}) {
		t.Errorf("null values: got %v, %v", m, err)
	}
}
