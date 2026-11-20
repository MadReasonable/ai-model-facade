package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Redacted stands for a secret in what the admin endpoints show, and, sent
// back in an edit, for the value already there.
const Redacted = "<redacted>"

// A Path names a node by the keys from the config's root to it, as the
// segments of a /amf/config/** URL do. The empty Path is the config itself.
type Path []string

func (p Path) String() string {
	if len(p) == 0 {
		return "the config"
	}
	return strings.Join(p, ".")
}

// The kinds of EditError, each a reason an edit is refused.
const (
	EditNotFound = iota + 1 // the path, or its parent, doesn't exist
	EditConflict            // the edit would change a value shared through an anchor
	EditInvalid             // the body, or the config it would make, is wrong
)

// EditError is an edit, or a read of a path, that was refused.
type EditError struct {
	Kind int
	Err  error
}

func (e *EditError) Error() string { return e.Err.Error() }
func (e *EditError) Unwrap() error { return e.Err }

func editError(kind int, format string, args ...any) error {
	return &EditError{kind, fmt.Errorf(format, args...)}
}

// Format is the encoding of an admin endpoint's body.
type Format int

const (
	JSON Format = iota
	YAML
)

// The secrets the secrets file may hold, by where they are in the config:
// server.apiKeys entries, and a backend's apiKey and keyMap values.
var apiKeysPath = Path{"server", "apiKeys"}

// unified is a Document as the admin endpoints show it: one tree, a copy of
// the main config with the secrets file's values put where the config's own
// would be, as loading merges them. Edits change the tree, and split takes
// the secrets file's values back out, so the client never sees which file
// holds a value.
type unified struct {
	doc  *Document
	root *yaml.Node // a mapping
	// fromSecrets holds the secrets file's own nodes that were put in root:
	// server.apiKeys entries, and apiKey and keyMap values.
	fromSecrets map[*yaml.Node]bool
	// restores undo, in reverse order, what putting the secrets in changed
	// besides adding them, so a config the edit didn't touch is saved as it
	// was written.
	restores []restore
	// secretAt says, for each backend, which secrets the secrets file held
	// before the edit, so a changed value is written back to that file.
	secretAt map[string]map[string]bool // backend -> "apiKey" or "keyMap.<name>"
	// mainAt says the same of the main config.
	mainAt map[string]map[string]bool
	// fit, if set, places every secret by the layout of the gateway's files
	// alone, for a copy written with another layout: fitToSecrets moves
	// each literal secret into the secrets file, fitToMain merges the
	// secrets file into the main config. See Fit.
	fit int
}

// restore is a change unify made to hold a secret: a container it created,
// or a value it copied out of an alias or a merge to add to it. It is undone
// by path and by value, since an edit may have replaced the nodes with equal
// ones, as a PUT of what GET showed does.
type restore struct {
	path  Path       // of the value
	was   *yaml.Node // the alias the copy replaced, or nil when the pair is new
	start *yaml.Node // the value before the secrets were added
	copy  *yaml.Node // the value unify put there
}

func (d *Document) unify() *unified {
	u := &unified{doc: d, root: copyTree(d.Main.Content[0]), fromSecrets: map[*yaml.Node]bool{}, secretAt: map[string]map[string]bool{}, mainAt: map[string]map[string]bool{}}
	for name, entry := range u.backends() {
		u.mainAt[name] = map[string]bool{}
		if _, ok := lookup(entry, "apiKey"); ok {
			u.mainAt[name]["apiKey"] = true
		}
		if km, ok := lookup(entry, "keyMap"); ok && resolve(km).Kind == yaml.MappingNode {
			for i := 0; i+1 < len(resolve(km).Content); i += 2 {
				u.mainAt[name]["keyMap."+resolve(km).Content[i].Value] = true
			}
		}
	}
	if d.Secrets == nil {
		return u
	}
	sroot := resolve(d.Secrets.Content[0])
	if server, ok := lookup(sroot, "server"); ok {
		if keys, ok := lookup(server, "apiKeys"); ok && resolve(keys).Kind == yaml.SequenceNode {
			seq := u.container(u.container(u.root, nil, "server", yaml.MappingNode), Path{"server"}, "apiKeys", yaml.SequenceNode)
			for _, e := range resolve(keys).Content {
				seq.Content = append(seq.Content, e)
				u.fromSecrets[e] = true
			}
		}
	}
	backends, ok := lookup(sroot, "backends")
	if !ok || resolve(backends).Kind != yaml.MappingNode {
		return u
	}
	section := u.container(u.root, nil, "backends", yaml.MappingNode)
	for i := 0; i+1 < len(resolve(backends).Content); i += 2 {
		name, values := resolve(backends).Content[i].Value, resolve(resolve(backends).Content[i+1])
		u.secretAt[name] = map[string]bool{}
		entry := u.container(section, Path{"backends"}, name, yaml.MappingNode)
		for j := 0; j+1 < len(values.Content); j += 2 {
			k, v := values.Content[j], values.Content[j+1]
			switch k.Value {
			case "apiKey":
				entry.Content = append(entry.Content, k, v)
				u.fromSecrets[v] = true
				u.secretAt[name]["apiKey"] = true
			case "keyMap":
				keyMap := u.container(entry, Path{"backends", name}, "keyMap", yaml.MappingNode)
				for m := 0; m+1 < len(resolve(v).Content); m += 2 {
					mk, mv := resolve(v).Content[m], resolve(v).Content[m+1]
					keyMap.Content = append(keyMap.Content, mk, mv)
					u.fromSecrets[mv] = true
					u.secretAt[name]["keyMap."+mk.Value] = true
				}
			}
		}
	}
	return u
}

// container returns the value of key in mapping parent, which is at path, as
// a node that pairs can be added to: as it is if parent holds it directly; a
// copy, if it is an alias or comes from a merge, so the anchor is unchanged;
// or a new node of kind. Each copy and new node is recorded, so split can
// undo it.
func (u *unified) container(parent *yaml.Node, path Path, key string, kind yaml.Kind) *yaml.Node {
	at := append(slices.Clone(path), key)
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k, v := parent.Content[i], parent.Content[i+1]
		if k.Value != key || k.Tag == "!!merge" {
			continue
		}
		if v.Kind != yaml.AliasNode {
			return v
		}
		c := *resolve(v)
		c.Anchor = ""
		c.Content = slices.Clone(c.Content)
		parent.Content[i+1] = &c
		u.restores = append(u.restores, restore{path: at, was: v, start: copyTree(&c), copy: &c})
		return &c
	}
	c := &yaml.Node{Kind: kind}
	if merged, ok := lookup(parent, key); ok {
		// Only through a merge: the pairs it brings, plus the secrets.
		c = &yaml.Node{Kind: resolve(merged).Kind, Tag: resolve(merged).Tag, Content: slices.Clone(resolve(merged).Content)}
	}
	if c.Tag == "" {
		c.Tag = map[yaml.Kind]string{yaml.MappingNode: "!!map", yaml.SequenceNode: "!!seq"}[c.Kind]
	}
	parent.Content = append(parent.Content, scalar(key), c)
	u.restores = append(u.restores, restore{path: at, start: copyTree(c), copy: c})
	return c
}

// backends returns the backend entries of the main config, by name.
func (u *unified) backends() map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	section, ok := lookup(u.root, "backends")
	if !ok || resolve(section).Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(resolve(section).Content); i += 2 {
		out[resolve(section).Content[i].Value] = resolve(resolve(section).Content[i+1])
	}
	return out
}

// View returns the node at path in d as the admin endpoints show it: the
// secrets file merged in, a literal key in server.apiKeys as its hash, and
// every literal apiKey and keyMap value as Redacted. ${NAME} is shown as
// written. The node is a copy.
func (d *Document) View(path Path) (*yaml.Node, error) {
	u := d.unify()
	u.redact(u.root, nil)
	// A secret's comment comes with it into an entry the config may write
	// in flow style, which can't hold a comment cleanly; only the view
	// changes, so the file keeps its style.
	u.eachNode(func(n *yaml.Node) {
		if n.Style&yaml.FlowStyle == 0 || u.fromSecrets[n] {
			return // a node of the secrets file's own is the Document's, and left as it is
		}
		for _, c := range n.Content {
			if c.HeadComment != "" || c.LineComment != "" || c.FootComment != "" {
				n.Style &^= yaml.FlowStyle
				return
			}
		}
	})
	return u.find(path)
}

// ETag returns the entity tag of the node at path: a hash of the node as
// held, secrets included, so any change to it changes the tag. The tag of
// the config itself covers both files. ok is false if there is no node at
// path.
func (d *Document) ETag(path Path) (tag string, ok bool) {
	u := d.unify()
	n, err := u.find(path)
	if err != nil {
		return "", false
	}
	data, err := encodeYAML(n)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:12]) + `"`, true
}

// find returns the node at path for a read, following aliases and merges.
func (u *unified) find(path Path) (*yaml.Node, error) {
	n := u.root
	for i, seg := range path {
		n = resolve(n)
		switch n.Kind {
		case yaml.MappingNode:
			v, ok := lookup(n, seg)
			if !ok {
				return nil, editError(EditNotFound, "%s has no key '%s'", path[:i], seg)
			}
			n = v
		case yaml.SequenceNode:
			return nil, editError(EditNotFound, "%s is a list, whose items have no path of their own; read the list", path[:i])
		default:
			return nil, editError(EditNotFound, "%s is not a map", path[:i])
		}
	}
	return n, nil
}

// redact replaces, in the tree at n, every secret with what the admin
// endpoints show for it. It replaces nodes rather than changing them, since
// some belong to the Document. Aliases aren't followed: their anchors are
// redacted where they are defined.
func (u *unified) redact(n *yaml.Node, at Path) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.Value == "apiKey" && isSecret(v):
				n.Content[i+1] = redactedScalar(v)
			case k.Value == "keyMap" && v.Kind == yaml.MappingNode:
				keyMap := *v
				keyMap.Content = slices.Clone(v.Content)
				for j := 1; j < len(keyMap.Content); j += 2 {
					if isSecret(keyMap.Content[j]) {
						keyMap.Content[j] = redactedScalar(keyMap.Content[j])
					}
				}
				n.Content[i+1] = &keyMap
			default:
				u.redact(v, append(slices.Clone(at), k.Value))
			}
		}
	case yaml.SequenceNode:
		if slices.Equal(at, apiKeysPath) {
			for i, e := range n.Content {
				n.Content[i] = keyAsHash(e)
			}
			return
		}
		for _, c := range n.Content {
			u.redact(c, at)
		}
	}
}

// isSecret reports whether a value would be shown as Redacted: a literal,
// not a ${NAME}, which holds no secret.
func isSecret(v *yaml.Node) bool {
	return v.Kind == yaml.ScalarNode && v.Tag != "!!null" && !envPattern.MatchString(v.Value)
}

func redactedScalar(v *yaml.Node) *yaml.Node {
	r := *v
	r.Value, r.Style, r.Tag = Redacted, 0, "!!str"
	return &r
}

// keyAsHash returns a server.apiKeys entry with a literal key replaced by its
// hash, or the entry itself if it holds none.
func keyAsHash(e *yaml.Node) *yaml.Node {
	if e.Kind != yaml.MappingNode {
		return e
	}
	for i := 0; i+1 < len(e.Content); i += 2 {
		k, v := e.Content[i], e.Content[i+1]
		if k.Value == "key" && isSecret(v) {
			c := *e
			c.Content = slices.Clone(e.Content)
			hk := *k
			hk.Value = "hash"
			hv := *v
			hv.Value, hv.Style, hv.Tag = HashKey(v.Value), 0, "!!str"
			c.Content[i], c.Content[i+1] = &hk, &hv
			return &c
		}
	}
	return e
}

// keyIdentity names the key a server.apiKeys entry holds, the same whether it
// gives the key or its hash, so an entry sent back can be matched to the one
// it came from: its name, and the key's hash, or a ${NAME} as written.
func keyIdentity(e *yaml.Node) string {
	if e.Kind != yaml.MappingNode {
		return ""
	}
	var name, id string
	for i := 0; i+1 < len(e.Content); i += 2 {
		k, v := e.Content[i], resolve(e.Content[i+1])
		switch k.Value {
		case "name":
			name = v.Value
		case "hash":
			id = "hash " + v.Value
		case "key":
			if isSecret(v) {
				id = "hash " + HashKey(v.Value)
			} else {
				id = "key " + v.Value
			}
		}
	}
	if id == "" {
		return ""
	}
	return name + "\x00" + id
}

// Put returns a copy of d with the node at path replaced by body, or added if
// the map that holds it has no such key; created says which. notes are what
// the edit did that the client should know, such as a secret written to the
// main config. The edit is refused if it would change a value shared through
// an anchor by way of one of its users, and if the result can't be read back.
// It checks nothing else of the config; see CheckSchema.
func (d *Document) Put(path Path, body []byte, format Format) (doc *Document, created bool, notes []string, err error) {
	u := d.unify()
	if len(path) == 0 {
		value, err := u.parseBody(body, format, u.root)
		if err != nil {
			return nil, false, nil, err
		}
		if value.Kind != yaml.MappingNode {
			return nil, false, nil, editError(EditInvalid, "the config must be a map")
		}
		value, err = u.keep(value, u.root, nil)
		if err != nil {
			return nil, false, nil, err
		}
		if value, err = u.keepShared(value, u.root, nil); err != nil {
			return nil, false, nil, err
		}
		old := u.root
		u.root = value
		if err := u.reanchor(old, value, nil); err != nil {
			return nil, false, nil, err
		}
	} else {
		parent, err := u.walk(path[:len(path)-1], path)
		if err != nil {
			return nil, false, nil, err
		}
		key := path[len(path)-1]
		i := pairIndex(parent, key)
		var old *yaml.Node
		if i >= 0 {
			old = parent.Content[i+1]
		} else if merged, ok := lookup(parent, key); ok {
			old = merged // a value given here overrides the merge's
		}
		value, err := u.parseBody(body, format, old)
		if err != nil {
			return nil, false, nil, err
		}
		if value, err = u.keep(value, old, path); err != nil {
			return nil, false, nil, err
		}
		if i >= 0 {
			if value, err = u.keepShared(value, old, path); err != nil {
				return nil, false, nil, err
			}
			keepComments(old, value)
			parent.Content[i+1] = value
			if err := u.reanchor(old, value, path); err != nil {
				return nil, false, nil, err
			}
		} else {
			parent.Content = append(parent.Content, scalar(key), value)
			created = old == nil
		}
	}
	doc, notes, err = u.split()
	if err != nil {
		return nil, false, nil, err
	}
	return doc, created, notes, nil
}

// Delete returns a copy of d without the node at path.
func (d *Document) Delete(path Path) (*Document, error) {
	if len(path) == 0 {
		return nil, editError(EditInvalid, "the config itself can't be deleted")
	}
	u := d.unify()
	parent, err := u.walk(path[:len(path)-1], path)
	if err != nil {
		return nil, err
	}
	key := path[len(path)-1]
	i := pairIndex(parent, key)
	if i < 0 {
		if merged, ok := lookup(parent, key); ok {
			return nil, editError(EditConflict, "%s comes from a merge ('<<') of %s, which other entries may share; delete it there", path, u.pathOf(merged))
		}
		return nil, editError(EditNotFound, "%s has no key '%s'", path[:len(path)-1], key)
	}
	if users := u.aliasesOf(parent.Content[i+1]); len(users) > 0 {
		return nil, editError(EditConflict, "%s is the anchor '%s', which %s uses; remove those uses first", path, parent.Content[i+1].Anchor, strings.Join(users, ", "))
	}
	parent.Content = slices.Delete(slices.Clone(parent.Content), i, i+2)
	doc, _, err := u.split()
	return doc, err
}

// Rename returns a copy of d with the key at path given the name to, in its
// place, its value kept as the file holds it, comments and anchors
// included, and its secrets in the file that holds them. Nothing that
// refers to the old name, such as a candidate naming a backend, changes.
// notes are as Put's.
func (d *Document) Rename(path Path, to string) (doc *Document, notes []string, err error) {
	if len(path) == 0 {
		return nil, nil, editError(EditInvalid, "the config itself has no name to change")
	}
	if to == "" {
		return nil, nil, editError(EditInvalid, "%s: the new name is empty", path)
	}
	u := d.unify()
	parent, err := u.walk(path[:len(path)-1], path)
	if err != nil {
		return nil, nil, err
	}
	key := path[len(path)-1]
	i := pairIndex(parent, key)
	if i < 0 {
		if merged, ok := lookup(parent, key); ok {
			return nil, nil, editError(EditConflict, "%s comes from a merge ('<<') of %s, which other entries may share; rename it there", path, u.pathOf(merged))
		}
		return nil, nil, editError(EditNotFound, "%s has no key '%s'", path[:len(path)-1], key)
	}
	if to == key {
		return nil, nil, editError(EditInvalid, "%s is already named '%s'", path, to)
	}
	if _, ok := lookup(parent, to); ok {
		return nil, nil, editError(EditConflict, "%s already has a key '%s'", path[:len(path)-1], to)
	}
	renamed := *parent.Content[i]
	renamed.Value = to
	parent.Content[i] = &renamed
	// What unify did to hold the secrets is undone by path, and a backend's
	// secrets are placed by its name and their keyMap names, so all of it
	// follows the rename.
	newPath := append(slices.Clone(path[:len(path)-1]), to)
	for j := range u.restores {
		if r := u.restores[j].path; len(r) >= len(path) && slices.Equal(r[:len(path)], path) {
			u.restores[j].path = append(slices.Clone(newPath), r[len(path):]...)
		}
	}
	switch {
	case len(path) == 2 && path[0] == "backends":
		u.secretAt[to], u.mainAt[to] = u.secretAt[key], u.mainAt[key]
		delete(u.secretAt, key)
		delete(u.mainAt, key)
	case len(path) == 4 && path[0] == "backends" && path[2] == "keyMap":
		for _, at := range []map[string]map[string]bool{u.secretAt, u.mainAt} {
			if fields := at[path[1]]; fields != nil && fields["keyMap."+key] {
				delete(fields, "keyMap."+key)
				fields["keyMap."+to] = true
			}
		}
	}
	return u.split()
}

// walk returns the mapping at path for an edit of one of its keys; target is
// the full path edited, for messages. It refuses a path that passes through
// an alias or a merge, since an edit there would change the anchor every
// other user of it shares, and one into a list, whose items are edited by
// replacing the list.
func (u *unified) walk(path, target Path) (*yaml.Node, error) {
	n := u.root
	for i := 0; ; i++ {
		if n.Kind == yaml.AliasNode {
			return nil, u.shared(path[:i], n.Alias)
		}
		if r := u.restoreOf(n); r != nil && r.was != nil && !secretField(target, i) {
			return nil, u.shared(path[:i], r.was.Alias)
		}
		switch n.Kind {
		case yaml.MappingNode:
		case yaml.SequenceNode:
			return nil, editError(EditNotFound, "%s is a list; a list is edited by replacing it, with a PUT of %s", path[:i], path[:i])
		default:
			return nil, editError(EditNotFound, "%s is not a map", path[:i])
		}
		if i == len(path) {
			return n, nil
		}
		j := pairIndex(n, path[i])
		if j < 0 {
			if merged, ok := lookup(n, path[i]); ok {
				return nil, editError(EditConflict, "%s comes from a merge ('<<') of %s, which other entries may share; edit it there, or give %s a value of its own with a PUT of %s", path[:i+1], u.pathOf(merged), path[:i+1], path[:i+1])
			}
			return nil, editError(EditNotFound, "%s has no key '%s'", path[:i], path[i])
		}
		n = n.Content[j+1]
	}
}

// secretField reports whether the edit of target goes past segment i into a
// backend's apiKey or keyMap, which unify copies an aliased backend entry to
// hold, so it can be edited without changing the anchor.
func secretField(target Path, i int) bool {
	return len(target) > i && i == 2 && target[0] == "backends" && (target[2] == "apiKey" || target[2] == "keyMap")
}

func (u *unified) shared(at Path, anchor *yaml.Node) error {
	return editError(EditConflict, "%s is the anchor '%s', defined at %s and shared with its other users; edit it there, or give %s a value of its own with a PUT of %s", at, anchor.Anchor, u.pathOf(anchor), at, at)
}

func (u *unified) restoreOf(n *yaml.Node) *restore {
	for i := range u.restores {
		if u.restores[i].copy == n {
			return &u.restores[i]
		}
	}
	return nil
}

// reanchor gives value, which has replaced old in the tree, each anchor
// defined in old that the rest of the tree uses, and points those uses at
// it, so the aliases that shared old's values share value's. An anchor goes
// to the node in value that defines it again, as a YAML body may, or else
// to the node at the same place in value, since a JSON body can't hold
// anchors. An anchor with neither is refused, as deleting it would be.
func (u *unified) reanchor(old, value *yaml.Node, at Path) error {
	defined := map[*yaml.Node]Path{} // old's anchors, by their place in old
	eachPathOf(old, func(p Path, n *yaml.Node) {
		if n.Kind != yaml.AliasNode && n.Anchor != "" {
			defined[n] = p
		}
	})
	moved := map[*yaml.Node]*yaml.Node{}
	var err error
	u.eachNode(func(n *yaml.Node) {
		if err != nil || n.Kind != yaml.AliasNode {
			return
		}
		place, ok := defined[n.Alias]
		if !ok {
			return
		}
		to, ok := moved[n.Alias]
		if !ok {
			if to, err = u.anchorTarget(n.Alias, value, at, place); err != nil {
				return
			}
			moved[n.Alias] = to
		}
		n.Alias = to
	})
	return err
}

// anchorTarget returns the node in value that takes over anchor, which was
// at place in the node value replaced at path at; see reanchor.
func (u *unified) anchorTarget(anchor, value *yaml.Node, at, place Path) (*yaml.Node, error) {
	var named *yaml.Node
	eachNode(value, func(n *yaml.Node) {
		if named == nil && n.Kind != yaml.AliasNode && n.Anchor == anchor.Anchor {
			named = n
		}
	})
	if named != nil {
		return named, nil
	}
	where := append(slices.Clone(at), place...)
	target := nodeAt(value, place)
	switch {
	case target != nil && target.Kind == yaml.AliasNode:
		return nil, editError(EditInvalid, "%s is the anchor '%s', which other values use, so it can't be an alias itself", where, anchor.Anchor)
	case target == nil || target.Anchor != "":
		return nil, editError(EditConflict, "%s is the anchor '%s', which %s uses; keep a value there, define the anchor again in the body, or remove those uses first", where, anchor.Anchor, strings.Join(u.aliasesOf(anchor), ", "))
	}
	target.Anchor = anchor.Anchor
	return target, nil
}

// nodeAt returns the node at path under n, without following aliases or
// merges; nil if there is none.
func nodeAt(n *yaml.Node, path Path) *yaml.Node {
	for _, seg := range path {
		switch n.Kind {
		case yaml.MappingNode:
			i := pairIndex(n, seg)
			if i < 0 {
				return nil
			}
			n = n.Content[i+1]
		case yaml.SequenceNode:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n.Content) {
				return nil
			}
			n = n.Content[i]
		default:
			return nil
		}
	}
	return n
}

// aliasesOf returns the paths of the aliases of n.
func (u *unified) aliasesOf(n *yaml.Node) []string {
	var users []string
	u.eachPath(func(p Path, m *yaml.Node) {
		if m.Kind == yaml.AliasNode && m.Alias == n {
			users = append(users, p.String())
		}
	})
	return users
}

// pathOf returns where n is in the tree, for messages.
func (u *unified) pathOf(n *yaml.Node) string {
	found := "an anchor"
	u.eachPath(func(p Path, m *yaml.Node) {
		if m == n && found == "an anchor" {
			found = p.String()
		}
	})
	return found
}

func (u *unified) eachNode(f func(*yaml.Node)) {
	u.eachPath(func(_ Path, n *yaml.Node) { f(n) })
}

// eachPath calls f for every node in the tree with its path, without
// following aliases; a list item's path ends in its index.
func (u *unified) eachPath(f func(Path, *yaml.Node)) {
	eachPathOf(u.root, f)
}

// eachPathOf is eachPath for the tree at root, with paths from it.
func eachPathOf(root *yaml.Node, f func(Path, *yaml.Node)) {
	var walk func(Path, *yaml.Node)
	walk = func(p Path, n *yaml.Node) {
		f(p, n)
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(append(slices.Clone(p), n.Content[i].Value), n.Content[i+1])
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(append(slices.Clone(p), strconv.Itoa(i)), c)
			}
		}
	}
	walk(nil, root)
}

// anchors returns the anchors defined in the tree, by name.
func (u *unified) anchors() map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	u.eachNode(func(n *yaml.Node) {
		if n.Anchor != "" && n.Kind != yaml.AliasNode {
			out[n.Anchor] = n
		}
	})
	return out
}

func pairIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i].Tag != "!!merge" {
			return i
		}
	}
	return -1
}

// keep returns value, sent for the node at path whose current value is old
// (nil if none), with what stands for a value already there replaced by
// it: Redacted for a secret, and a server.apiKeys entry shown by its hash
// for that entry. A literal key in a new server.apiKeys entry becomes its
// hash, so the key itself is never held.
func (u *unified) keep(value, old *yaml.Node, path Path) (*yaml.Node, error) {
	if old != nil {
		old = resolve(old)
	}
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag != "!!str" || value.Value != Redacted {
			return value, nil
		}
		if old == nil || old.Kind != yaml.ScalarNode {
			return nil, editError(EditInvalid, "%s: '%s' keeps the value already there, and there is none", path, Redacted)
		}
		return old, nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(value.Content); i += 2 {
			k := value.Content[i]
			var oldChild *yaml.Node
			if old != nil && old.Kind == yaml.MappingNode {
				oldChild, _ = lookup(old, k.Value)
			}
			kept, err := u.keep(value.Content[i+1], oldChild, append(slices.Clone(path), k.Value))
			if err != nil {
				return nil, err
			}
			value.Content[i+1] = kept
		}
	case yaml.SequenceNode:
		if slices.Equal(path, apiKeysPath) {
			return u.keepKeys(value, old)
		}
		for i, item := range value.Content {
			var oldChild *yaml.Node
			if old != nil && old.Kind == yaml.SequenceNode && i < len(old.Content) {
				oldChild = old.Content[i]
			}
			kept, err := u.keep(item, oldChild, append(slices.Clone(path), strconv.Itoa(i)))
			if err != nil {
				return nil, err
			}
			value.Content[i] = kept
		}
	}
	return value, nil
}

// keepShared returns value, sent for the node at path whose current value is
// old, with each part of it that reads the same as the part of old in its
// place kept as old holds it: an alias stays an alias, a merge stays a
// merge, and a node kept keeps its comments and style. JSON has no anchors
// and GET's JSON shows them resolved, so a client that sends back what it
// read, with some of it changed, changes only that. A list item that
// matches an item elsewhere in old's list is kept too, so a list can be
// reordered.
func (u *unified) keepShared(value, old *yaml.Node, path Path) (*yaml.Node, error) {
	if sameAsRead(value, old) {
		return old, nil
	}
	if old.Kind == yaml.AliasNode {
		return value, nil // a value of its own now; the anchor's nodes stay the anchor's
	}
	switch {
	case value.Kind == yaml.MappingNode && old.Kind == yaml.MappingNode:
		return u.keepSharedPairs(value, old, path)
	case value.Kind == yaml.SequenceNode && old.Kind == yaml.SequenceNode:
		used := make([]bool, len(old.Content))
		for i, item := range value.Content {
			j := -1 // the old item it matches, the one in its place first
			for k := range old.Content {
				if !used[k] && sameAsRead(item, old.Content[k]) && (j < 0 || k == i) {
					j = k
				}
			}
			if j >= 0 {
				value.Content[i], used[j] = old.Content[j], true
				continue
			}
			if i < len(old.Content) && !used[i] {
				kept, err := u.keepShared(item, old.Content[i], append(slices.Clone(path), strconv.Itoa(i)))
				if err != nil {
					return nil, err
				}
				value.Content[i] = kept
			}
		}
	}
	return value, nil
}

// keepSharedPairs is keepShared for two maps. A key that a merge in old
// brings and value gives the same is left to the merge again; one value
// gives differently overrides it, as a key beside a merge does. A key the
// merge brings that value leaves out is refused, since the merge would
// still bring it.
func (u *unified) keepSharedPairs(value, old *yaml.Node, path Path) (*yaml.Node, error) {
	given := map[string]*yaml.Node{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		given[value.Content[i].Value] = value.Content[i+1]
	}
	inherited := map[string]bool{} // keys left to a merge
	mergeAt := map[string]int{}    // each merge's first key, to the merge's index in old
	placed := map[int]bool{}       // merges with a first key
	for _, pair := range mergedPairs(old) {
		key := pair[0].Value
		if pairIndex(old, key) >= 0 {
			continue // old's own
		}
		merge := mergeOf(old, key)
		v, ok := given[key]
		if !ok {
			return nil, editError(EditConflict, "%s comes from a merge ('<<') of %s, which other entries may share, so it can't be left out here; remove it there, or remove the merge", append(slices.Clone(path), key), u.pathOf(resolve(old.Content[merge+1])))
		}
		if !placed[merge] {
			placed[merge], mergeAt[key] = true, merge
		}
		if sameAsRead(v, pair[1]) {
			inherited[key] = true
		}
	}
	out := *value
	out.Content = nil
	for i := 0; i+1 < len(value.Content); i += 2 {
		k, v := value.Content[i], value.Content[i+1]
		if merge, ok := mergeAt[k.Value]; ok {
			out.Content = append(out.Content, old.Content[merge], old.Content[merge+1])
		}
		if inherited[k.Value] {
			continue
		}
		if j := pairIndex(old, k.Value); j >= 0 {
			kept, err := u.keepShared(v, old.Content[j+1], append(slices.Clone(path), k.Value))
			if err != nil {
				return nil, err
			}
			k, v = old.Content[j], kept
			keepComments(old.Content[j+1], v)
		}
		out.Content = append(out.Content, k, v)
	}
	return &out, nil
}

// mergeOf returns the index in m of the merge key whose merge brings key,
// the first that does, as the gateway reads it.
func mergeOf(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Tag != "!!merge" {
			continue
		}
		if _, ok := lookup(&yaml.Node{Kind: yaml.MappingNode, Content: m.Content[i : i+2]}, key); ok {
			return i
		}
	}
	return -1
}

// sameAsRead reports whether a and b hold the same value as the gateway
// reads it, with aliases and merges followed; a scalar's tag counts, so 1
// and "1" differ.
func sameAsRead(a, b *yaml.Node) bool {
	a, b = resolve(a), resolve(b)
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.Tag == b.Tag && a.Value == b.Value
	case yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !sameAsRead(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	case yaml.MappingNode:
		pa, pb := mergedPairs(a), mergedPairs(b)
		if len(pa) != len(pb) {
			return false
		}
		for i := range pa {
			if pa[i][0].Value != pb[i][0].Value || !sameAsRead(pa[i][1], pb[i][1]) {
				return false
			}
		}
		return true
	}
	return true
}

// keepKeys matches each server.apiKeys entry sent to one already there by its
// name and key, and keeps that one, in whichever file and form it is written.
// A name may have several keys, and the client may reorder the list, so
// neither the name nor the position alone can say which entry is which.
func (u *unified) keepKeys(value, old *yaml.Node) (*yaml.Node, error) {
	var existing []*yaml.Node
	if old != nil && old.Kind == yaml.SequenceNode {
		existing = slices.Clone(old.Content)
	}
	for i, e := range value.Content {
		if e.Kind != yaml.MappingNode {
			continue // the schema check reports it
		}
		if k, ok := lookup(e, "key"); ok && resolve(k).Value == Redacted {
			return nil, editError(EditInvalid, "server.apiKeys.%d: '%s' can't stand for a key; send the entry's hash, as GET shows it", i, Redacted)
		}
		e = keyAsHash(e)
		value.Content[i] = e
		id := keyIdentity(e)
		for j, o := range existing {
			if o != nil && id != "" && keyIdentity(resolve(o)) == id {
				value.Content[i], existing[j] = o, nil
				break
			}
		}
	}
	return value, nil
}

// parseBody parses a PUT's body for the node old, nil for a new one. A YAML
// body may use the config's anchors, as a node read with GET shows them, so
// it is parsed after a list that defines each one, and its aliases are then
// pointed at the config's own. Anchors inside old aren't offered, since the
// body replaces them, and the body may define them again.
func (u *unified) parseBody(body []byte, format Format, old *yaml.Node) (*yaml.Node, error) {
	anchors := u.anchors()
	if old != nil {
		eachNode(old, func(n *yaml.Node) {
			if n.Kind != yaml.AliasNode && anchors[n.Anchor] == n {
				delete(anchors, n.Anchor)
			}
		})
	}
	var value *yaml.Node
	if format == JSON {
		if !json.Valid(body) {
			return nil, editError(EditInvalid, "the body is not JSON; send YAML with Content-Type: application/yaml")
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, editError(EditInvalid, "the body: %v", err)
		}
		value = doc.Content[0]
		blockStyle(value)
	} else {
		var err error
		if value, err = u.parseYAMLBody(body, anchors); err != nil {
			return nil, err
		}
	}
	var clash string
	eachNode(value, func(n *yaml.Node) {
		if n.Anchor != "" && n.Kind != yaml.AliasNode && anchors[n.Anchor] != nil && clash == "" {
			clash = n.Anchor
		}
	})
	if clash != "" {
		return nil, editError(EditInvalid, "the body defines the anchor '%s', which %s already defines; use another name", clash, u.pathOf(anchors[clash]))
	}
	return value, nil
}

var bodyLine = regexp.MustCompile(`line (\d+)`)

func (u *unified) parseYAMLBody(body []byte, anchors map[string]*yaml.Node) (*yaml.Node, error) {
	names := make([]string, 0, len(anchors))
	for name := range anchors {
		names = append(names, name)
	}
	slices.Sort(names)
	multiDocument := regexp.MustCompile(`(?m)^(---|\.\.\.)`).Match(body)
	var text bytes.Buffer
	offset := 0
	if len(names) > 0 && !multiDocument {
		text.WriteString("anchors:\n")
		for _, name := range names {
			fmt.Fprintf(&text, "  - &%s null\n", name)
		}
		text.WriteString("body:\n")
		offset = len(names) + 2
		for _, line := range strings.SplitAfter(string(body), "\n") {
			if line != "" {
				text.WriteString("  " + line)
			}
		}
	} else {
		text.Write(body)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(text.Bytes(), &doc); err != nil {
		msg := bodyLine.ReplaceAllStringFunc(err.Error(), func(m string) string {
			n, _ := strconv.Atoi(m[5:])
			return "line " + strconv.Itoa(max(1, n-offset))
		})
		return nil, editError(EditInvalid, "the body: %s", strings.TrimPrefix(msg, "yaml: "))
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, editError(EditInvalid, "the body is empty")
	}
	if offset == 0 {
		return doc.Content[0], nil
	}
	root := doc.Content[0]
	placeholders := map[*yaml.Node]string{}
	for _, p := range root.Content[1].Content {
		placeholders[p] = p.Anchor
	}
	value := root.Content[3]
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" && value.Value == "" {
		return nil, editError(EditInvalid, "the body is empty")
	}
	eachNode(value, func(n *yaml.Node) {
		if name, ok := placeholders[n.Alias]; ok && n.Kind == yaml.AliasNode {
			n.Alias = anchors[name]
		}
	})
	return value, nil
}

func eachNode(n *yaml.Node, f func(*yaml.Node)) {
	f(n)
	if n.Kind == yaml.AliasNode {
		return
	}
	for _, c := range n.Content {
		eachNode(c, f)
	}
}

// blockStyle clears the flow and quoting styles JSON brings, so a value sent
// as JSON is written in the file's block style; a string that would read as
// another type is still quoted, since its tag says it is a string.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// The ways Fit places secrets; 0 places them as split does after an edit.
const (
	fitToSecrets = iota + 1
	fitToMain
)

// Fit returns d with its secrets where a gateway whose files are as
// hasSecretsFile says would keep them, for a copy written with the other
// layout, such as a backup made before the gateway was given --secrets or
// after it stopped being. With a secrets file, each literal secret in the
// main config moves to it: a backend's apiKey and keyMap values, and a
// server.apiKeys entry given as its key; a ${NAME} or a hash holds no
// secret, so it stays. Without one, the secrets file is merged into the main
// config. notes name each secret moved, and each the merge put in the main
// config in plaintext. A d already in that layout is returned as it is.
func (d *Document) Fit(hasSecretsFile bool) (*Document, []string, error) {
	switch {
	case hasSecretsFile && d.Secrets == nil:
		c := &Document{Main: d.Main, Secrets: &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}}
		u := c.unify()
		u.fit = fitToSecrets
		return u.split()
	case !hasSecretsFile && d.Secrets != nil:
		u := d.unify()
		u.fit = fitToMain
		doc, notes, err := u.split()
		return doc, notes, err
	}
	return d, nil, nil
}

// hasLiteralKey reports whether a server.apiKeys entry gives its key itself,
// rather than its hash or a ${NAME}.
func hasLiteralKey(e *yaml.Node) bool {
	k, ok := pairValue(e, "key")
	return ok && isSecret(k)
}

// entryName returns the name of a server.apiKeys entry.
func entryName(e *yaml.Node) string {
	name, _ := pairValue(e, "name")
	if name == nil {
		return ""
	}
	return name.Value
}

// split takes the secrets back out of the edited tree into a Document: each
// secret goes to the file it came from, a changed one to the file that held
// it, and a new one to the secrets file if there is one, else to the main
// config, with a note. server.apiKeys entries the edit added go to the main
// config, since they hold only hashes and ${NAME}s.
func (u *unified) split() (*Document, []string, error) {
	var notes []string
	hasSecrets := u.doc.Secrets != nil && u.fit != fitToMain
	var keys []*yaml.Node
	type backendSecrets struct {
		apiKey      *yaml.Node
		keyMapPairs []*yaml.Node
	}
	secrets := map[string]*backendSecrets{}
	var order []string
	if server, ok := pairValue(u.root, "server"); ok {
		if seq, ok := pairValue(server, "apiKeys"); ok && seq.Kind == yaml.SequenceNode {
			var main []*yaml.Node
			for _, e := range seq.Content {
				switch literal := hasLiteralKey(e); {
				case u.fit == fitToSecrets && literal:
					keys = append(keys, e)
					notes = append(notes, fmt.Sprintf("server.apiKeys: the key of '%s' moves to the secrets file", entryName(e)))
				case u.fit == fitToMain && literal && u.fromSecrets[e]:
					main = append(main, e)
					notes = append(notes, fmt.Sprintf("server.apiKeys: the key of '%s' is written to the main config in plaintext, since the gateway has no secrets file", entryName(e)))
				case hasSecrets && u.fromSecrets[e]:
					keys = append(keys, e)
				default:
					main = append(main, e)
				}
			}
			seq.Content = main
		}
	}
	toSecrets := func(name, field string, v *yaml.Node) bool {
		switch {
		case u.fit == fitToSecrets:
			if isSecret(v) {
				notes = append(notes, fmt.Sprintf("backends.%s.%s moves to the secrets file", name, field))
			}
			return isSecret(v) // a ${NAME} holds no secret, so it stays where it is
		case u.fit == fitToMain:
			if isSecret(v) && u.fromSecrets[v] {
				notes = append(notes, fmt.Sprintf("backends.%s.%s is written to the main config in plaintext, since the gateway has no secrets file", name, field))
			}
			return false
		case hasSecrets && (u.fromSecrets[v] || u.secretAt[name][field]):
			return true
		case u.mainAt[name][field]:
			return false
		case hasSecrets:
			return true
		}
		if isSecret(v) {
			notes = append(notes, fmt.Sprintf("backends.%s.%s is written to the main config in plaintext, since the gateway has no secrets file", name, field))
		}
		return false
	}
	if section, ok := pairValue(u.root, "backends"); ok && section.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(section.Content); i += 2 {
			name, entry := section.Content[i].Value, section.Content[i+1]
			if entry.Kind != yaml.MappingNode {
				continue // an alias holds no secret of the secrets file
			}
			s := &backendSecrets{}
			var kept []*yaml.Node
			for j := 0; j+1 < len(entry.Content); j += 2 {
				k, v := entry.Content[j], entry.Content[j+1]
				switch {
				case k.Value == "apiKey" && k.Tag != "!!merge" && toSecrets(name, "apiKey", v):
					s.apiKey = v
					continue
				case k.Value == "keyMap" && k.Tag != "!!merge" && v.Kind == yaml.MappingNode:
					var main []*yaml.Node
					for m := 0; m+1 < len(v.Content); m += 2 {
						mk, mv := v.Content[m], v.Content[m+1]
						if toSecrets(name, "keyMap."+mk.Value, mv) {
							s.keyMapPairs = append(s.keyMapPairs, mk, mv)
						} else {
							main = append(main, mk, mv)
						}
					}
					v.Content = main
				}
				kept = append(kept, k, v)
			}
			entry.Content = kept
			if s.apiKey != nil || len(s.keyMapPairs) > 0 {
				secrets[name] = s
				order = append(order, name)
			}
		}
	}
	inTree := map[*yaml.Node]bool{}
	u.eachNode(func(n *yaml.Node) { inTree[n] = true })
	for i := len(u.restores) - 1; i >= 0; i-- {
		r := u.restores[i]
		parent := u.root
		for _, seg := range r.path[:len(r.path)-1] {
			if parent.Kind != yaml.MappingNode {
				parent = nil
				break
			}
			j := pairIndex(parent, seg)
			if j < 0 {
				parent = nil
				break
			}
			parent = parent.Content[j+1]
		}
		if parent == nil || parent.Kind != yaml.MappingNode {
			continue
		}
		j := pairIndex(parent, r.path[len(r.path)-1])
		if j < 0 || !sameValue(parent.Content[j+1], r.start) {
			continue // the edit changed it, so it stays as it is now
		}
		if r.was != nil {
			anchor := r.was.Alias
			if !inTree[anchor] {
				// A body that replaced the anchor's node defines it again.
				if anchor = u.anchors()[anchor.Anchor]; anchor == nil || !sameValue(anchor, r.start) {
					continue // the edit removed or changed the anchor, so the copy stays
				}
			}
			alias := *r.was
			alias.Alias = anchor
			parent.Content[j+1] = &alias
		} else {
			parent.Content = slices.Delete(parent.Content, j, j+2)
		}
	}
	keepStyles(u.root, u.doc.Main.Content[0])
	main := *u.doc.Main
	main.Content = []*yaml.Node{u.root}
	doc := &Document{Main: &main}
	if hasSecrets {
		doc.Secrets = u.secretsDocument(keys, order, func(name string) (*yaml.Node, []*yaml.Node) {
			return secrets[name].apiKey, secrets[name].keyMapPairs
		})
	}
	data, err := encodeYAML(doc.Main)
	if err == nil {
		var check yaml.Node
		err = yaml.Unmarshal(data, &check)
	}
	if err != nil {
		return nil, nil, editError(EditInvalid, "the edit leaves the config unreadable: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	return doc, notes, nil
}

// secretsDocument builds the secrets file from the secrets the edited tree
// holds, keeping the old file's nodes, and so its comments, where they stay.
func (u *unified) secretsDocument(keys []*yaml.Node, order []string, values func(string) (*yaml.Node, []*yaml.Node)) *yaml.Node {
	oldRoot := resolve(u.doc.Secrets.Content[0])
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: oldRoot.Style, HeadComment: oldRoot.HeadComment, LineComment: oldRoot.LineComment, FootComment: oldRoot.FootComment}
	section := func(key string) (*yaml.Node, *yaml.Node) {
		if i := pairIndex(oldRoot, key); i >= 0 {
			k, v := *oldRoot.Content[i], *resolve(oldRoot.Content[i+1])
			v.Content = nil
			return &k, &v
		}
		return scalar(key), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if len(keys) > 0 {
		k, server := section("server")
		oldServer, _ := pairValue(oldRoot, "server")
		var seqKey *yaml.Node = scalar("apiKeys")
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		if oldServer != nil {
			if i := pairIndex(oldServer, "apiKeys"); i >= 0 {
				c := *oldServer.Content[i]
				seqKey = &c
				s := *resolve(oldServer.Content[i+1])
				seq = &s
			}
		}
		seq.Content = keys
		server.Content = []*yaml.Node{seqKey, seq}
		root.Content = append(root.Content, k, server)
	}
	if len(order) > 0 {
		k, backends := section("backends")
		oldBackends, _ := pairValue(oldRoot, "backends")
		for _, name := range order {
			apiKey, keyMapPairs := values(name)
			entryKey, entry := scalar(name), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			var oldEntry *yaml.Node
			if oldBackends != nil {
				if i := pairIndex(oldBackends, name); i >= 0 {
					c := *oldBackends.Content[i]
					entryKey = &c
					oldEntry = resolve(oldBackends.Content[i+1])
					e := *oldEntry
					e.Content = nil
					entry = &e
				}
			}
			field := func(key string) *yaml.Node {
				if oldEntry != nil {
					if i := pairIndex(oldEntry, key); i >= 0 {
						return oldEntry.Content[i]
					}
				}
				return scalar(key)
			}
			if apiKey != nil {
				entry.Content = append(entry.Content, field("apiKey"), apiKey)
			}
			if len(keyMapPairs) > 0 {
				keyMap := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				if oldEntry != nil {
					if old, ok := pairValue(oldEntry, "keyMap"); ok {
						c := *old
						keyMap = &c
					}
				}
				keyMap.Content = keyMapPairs
				entry.Content = append(entry.Content, field("keyMap"), keyMap)
			}
			backends.Content = append(backends.Content, entryKey, entry)
		}
		root.Content = append(root.Content, k, backends)
	}
	doc := *u.doc.Secrets
	doc.Content = []*yaml.Node{root}
	return &doc
}

// pairValue returns the value of key in mapping n itself, an alias resolved,
// ignoring merges.
func pairValue(n *yaml.Node, key string) (*yaml.Node, bool) {
	n = resolve(n)
	if n.Kind != yaml.MappingNode {
		return nil, false
	}
	if i := pairIndex(n, key); i >= 0 {
		return resolve(n.Content[i+1]), true
	}
	return nil, false
}

// sameValue reports whether a and b hold the same value as the gateway reads
// it: aliases resolved, and styles and comments ignored.
func sameValue(a, b *yaml.Node) bool {
	a, b = resolve(a), resolve(b)
	if a.Kind != b.Kind || len(a.Content) != len(b.Content) {
		return false
	}
	if a.Kind == yaml.ScalarNode {
		return a.Tag == b.Tag && a.Value == b.Value
	}
	for i := range a.Content {
		if !sameValue(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// keepComments gives a scalar that replaces another the comments of the one
// it replaces, when it brings none of its own, so a changed value keeps the
// note written beside it.
func keepComments(old, value *yaml.Node) {
	if old.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode || value.HeadComment != "" || value.LineComment != "" || value.FootComment != "" {
		return
	}
	value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
}

// keepStyles gives each map and list in edited whose value is unchanged from
// the node at the same path in original the style original has, so an edit
// that changes nothing there, such as a PUT of what GET showed, or of the
// same value in JSON, which has no styles, leaves the file's layout as it
// was.
func keepStyles(edited, original *yaml.Node) {
	if edited.Kind == yaml.AliasNode || original.Kind == yaml.AliasNode || edited.Kind != original.Kind {
		return
	}
	switch edited.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(edited.Content); i += 2 {
			if j := pairIndex(original, edited.Content[i].Value); j >= 0 {
				keepStyles(edited.Content[i+1], original.Content[j+1])
			}
		}
	case yaml.SequenceNode:
		for i := range min(len(edited.Content), len(original.Content)) {
			keepStyles(edited.Content[i], original.Content[i])
		}
	default:
		return
	}
	if sameValue(edited, original) {
		edited.Style = original.Style
	}
}
