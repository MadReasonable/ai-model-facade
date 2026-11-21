package config

import (
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Anchor is a value defined once and shared, by alias or by merge, as GET
// /amf/config-anchors reports it.
type Anchor struct {
	Name string `json:"name"` // "" for a map merged in place, which has no anchor
	Path Path   `json:"path"` // where it is defined
	// Users are the paths that read it, as GET /amf/config shows them. An
	// edit at or below one is refused, since it would change every other
	// user; an edit at Path changes them all.
	Users []AnchorUser `json:"users"`
}

// AnchorUser is one path that reads an anchor.
type AnchorUser struct {
	Path Path   `json:"path"`
	Via  string `json:"via"` // alias, or merge for a key a merge ('<<') brings
}

// Anchors returns the anchors in d, in the order the config defines them,
// each with its users, and before them each map merged in place that some
// key reads. A list item's path ends in its index.
func (d *Document) Anchors() []Anchor {
	u := d.unify()
	var anchors []*Anchor
	byNode := map[*yaml.Node]*Anchor{}
	anchorOf := func(n *yaml.Node) *Anchor {
		if a := byNode[n]; a != nil {
			return a
		}
		a := &Anchor{Name: n.Anchor, Users: []AnchorUser{}}
		if p := u.pathTo(n); p != nil {
			a.Path = p
		}
		byNode[n] = a
		anchors = append(anchors, a)
		return a
	}
	u.eachPath(func(p Path, n *yaml.Node) {
		if n.Anchor != "" && n.Kind != yaml.AliasNode {
			anchorOf(n)
		}
	})
	var walk func(Path, *yaml.Node)
	walk = func(p Path, n *yaml.Node) {
		if r := u.restoreOf(n); r != nil && r.was != nil {
			n = r.was // an aliased entry unify copied to hold a secret
		}
		switch n.Kind {
		case yaml.AliasNode:
			a := anchorOf(n.Alias)
			a.Users = append(a.Users, AnchorUser{p, "alias"})
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Tag != "!!merge" {
					walk(append(slices.Clone(p), n.Content[i].Value), n.Content[i+1])
				}
			}
			for _, pair := range mergedPairs(n) {
				key := pair[0].Value
				if pairIndex(n, key) >= 0 {
					continue
				}
				a := anchorOf(mergeSource(n, key))
				a.Users = append(a.Users, AnchorUser{append(slices.Clone(p), key), "merge"})
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(append(slices.Clone(p), strconv.Itoa(i)), c)
			}
		}
	}
	walk(nil, u.root)
	out := make([]Anchor, len(anchors))
	for i, a := range anchors {
		if a.Path == nil {
			a.Path = Path{}
		}
		out[i] = *a
	}
	return out
}

// mergeSource returns the map a merge in m brings key from, as the anchor
// an alias names, or the map itself when it is written in place.
func mergeSource(m *yaml.Node, key string) *yaml.Node {
	i := mergeOf(m, key)
	v := m.Content[i+1]
	sources := []*yaml.Node{v}
	if resolve(v).Kind == yaml.SequenceNode {
		sources = resolve(v).Content
	}
	for _, src := range sources {
		if _, ok := lookup(src, key); ok {
			if src.Kind == yaml.AliasNode {
				return src.Alias
			}
			return src
		}
	}
	return v
}

// pathTo returns the path of n in the tree, without following aliases; nil
// if n isn't in it.
func (u *unified) pathTo(n *yaml.Node) Path {
	var found Path
	u.eachPath(func(p Path, m *yaml.Node) {
		if m == n && found == nil {
			found = p
		}
	})
	return found
}
