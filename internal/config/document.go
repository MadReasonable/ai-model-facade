package config

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Document is the config as its files hold it, before ${NAME} is expanded:
// the main config and, if the gateway was given one, the secrets file, each a
// yaml.Node tree, which keeps comments, key order and anchors. The admin
// endpoints read and edit a Document, and a save writes one back to the
// files; Parse turns one into the Config a gateway is built from.
//
// A Document is never changed in place once made: an edit returns a new one,
// so the running config's Document can be read while a draft is edited.
type Document struct {
	Main    *yaml.Node // a document node
	Secrets *yaml.Node // a document node; nil when the gateway has no secrets file
	// Origin is the content of the files the Document was read from; nil
	// for one made in memory, such as an edited draft or a backup.
	Origin *Origin
}

// Origin identifies the bytes of the files a Document was read from, so a
// save can tell whether a file changed on disk since.
type Origin struct {
	Main    [sha256.Size]byte
	Secrets [sha256.Size]byte // zero when there is no secrets file
}

// ReadDocument parses a config, and a secrets file unless secretsData is nil.
// An error in the secrets file is a *SecretsError.
func ReadDocument(data, secretsData []byte) (*Document, error) {
	var main yaml.Node
	if err := yaml.Unmarshal(data, &main); err != nil {
		return nil, err
	}
	if main.Kind != yaml.DocumentNode || len(main.Content) == 0 {
		return nil, fmt.Errorf("config is empty")
	}
	d := &Document{Main: &main}
	if secretsData == nil {
		return d, nil
	}
	var secrets yaml.Node
	if err := yaml.Unmarshal(secretsData, &secrets); err != nil {
		return nil, &SecretsError{err}
	}
	if secrets.Kind != yaml.DocumentNode || len(secrets.Content) == 0 {
		// An empty file holds no secrets, and a save may add some.
		secrets = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	d.Secrets = &secrets
	return d, nil
}

// Copy returns a deep copy of d, with each alias pointing to the copy of its
// anchor, so the copy can be changed without changing d.
func (d *Document) Copy() *Document {
	c := &Document{Main: copyTree(d.Main), Origin: d.Origin}
	if d.Secrets != nil {
		c.Secrets = copyTree(d.Secrets)
	}
	return c
}

// Encode returns the YAML of the main config and of the secrets file, nil
// for the secrets file when there is none. It is what a save writes, so the
// layout may differ from the file read: yaml.v3 keeps comments and key order
// but not blank lines or the indentation of the original.
func (d *Document) Encode() (main, secrets []byte, err error) {
	if main, err = encodeYAML(d.Main); err != nil {
		return nil, nil, err
	}
	if d.Secrets != nil {
		if secrets, err = encodeYAML(d.Secrets); err != nil {
			return nil, nil, err
		}
	}
	return main, secrets, nil
}

// Equal reports whether d and other hold the same config, comments included,
// as they would be saved.
func (d *Document) Equal(other *Document) bool {
	m1, s1, err1 := d.Encode()
	m2, s2, err2 := other.Encode()
	return err1 == nil && err2 == nil && bytes.Equal(m1, m2) && bytes.Equal(s1, s2) && (d.Secrets == nil) == (other.Secrets == nil)
}

// Parse expands ${NAME} with lookupEnv and decodes d strictly into the Config
// a gateway is built from, checking everything the config can get wrong
// without contacting a backend. The Config's Source is d.
func (d *Document) Parse(lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg, err := d.parseSchema(lookupEnv)
	if err != nil {
		return nil, err
	}
	if err := cfg.indexGroups(); err != nil {
		return nil, err
	}
	if err := cfg.Server.checkAdmins(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// CheckSchema checks what each part of d can get wrong by itself: unknown
// keys, wrong types and values, undefined ${NAME}, and, through checkEntry,
// each backend and facade entry on its own. It leaves out what spans parts,
// such as a group naming a key or a facade naming a backend, which Parse and
// the gateway's build check, so a draft may be wrong in those ways between
// edits.
func (d *Document) CheckSchema(lookupEnv func(string) (string, bool), checkEntry func(section string, e Entry) error) error {
	cfg, err := d.parseSchema(lookupEnv)
	if err != nil {
		return err
	}
	for _, section := range []struct {
		name string
		node *yaml.Node
	}{{"backends", &cfg.Backends}, {"models", &cfg.Models}} {
		entries, err := Entries(section.node, section.name)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := checkEntry(section.name, e); err != nil {
				return fmt.Errorf("%s.%s: %w", section.name, e.Name, err)
			}
		}
	}
	return nil
}

// parseSchema is Parse without the checks that span parts of the config.
func (d *Document) parseSchema(lookupEnv func(string) (string, bool)) (*Config, error) {
	c := d.Copy() // expanding ${NAME} and merging the secrets change the tree
	root := c.Main.Content[0]
	if err := expandEnv(root, lookupEnv); err != nil {
		return nil, err
	}
	var cfg Config
	if err := DecodeStrict(root, &cfg); err != nil {
		return nil, err
	}
	cfg.Source = d
	if cfg.Server.Listen == "" {
		return nil, fmt.Errorf("server.listen is required")
	}
	if cfg.Server.AdminAPI.Listen != "" && cfg.Server.AdminAPI.Listen == cfg.Server.Listen {
		return nil, fmt.Errorf("server.adminApi.listen is server.listen, '%s'; give the admin endpoints an address of their own, or leave it out to serve them on server.listen", cfg.Server.Listen)
	}
	if cfg.Server.TargetHeader == "" {
		cfg.Server.TargetHeader = "X-Gateway-Target"
	}
	if err := cfg.API.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Server.Responses.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Server.AdminAPI.Logs.applyDefaults(); err != nil {
		return nil, err
	}
	if backups := cfg.Server.AdminAPI.ConfigBackups; backups != nil && *backups < 0 {
		return nil, fmt.Errorf("server.adminApi.configBackups: %d is negative", *backups)
	}
	if c.Secrets != nil {
		s, err := parseSecrets(c.Secrets, lookupEnv)
		if err != nil {
			return nil, err
		}
		if err := cfg.mergeSecrets(s); err != nil {
			return nil, err
		}
	}
	if err := cfg.indexKeys(); err != nil {
		return nil, err
	}
	switch cfg.Server.UnknownKeys {
	case "":
		cfg.Server.UnknownKeys = "reject"
	case "reject", "allow":
	default:
		return nil, fmt.Errorf("server.unknownKeys: '%s' is not reject or allow", cfg.Server.UnknownKeys)
	}
	return &cfg, nil
}

// encodeYAML writes n as a file holds it. A merge key keeps the tag yaml.v3
// read it with, which its encoder writes out as '!!merge <<', so the copy
// encoded has it cleared, and the key is written '<<' as it was read.
func encodeYAML(n *yaml.Node) ([]byte, error) {
	n = copyTree(n)
	eachNode(n, func(c *yaml.Node) {
		if c.Kind == yaml.ScalarNode && c.Tag == "!!merge" {
			c.Tag = ""
		}
	})
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(n); err != nil {
		return nil, err
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// copyTree returns a deep copy of n. An alias whose anchor is inside n points
// to the anchor's copy; one whose anchor is outside keeps pointing to it.
func copyTree(n *yaml.Node) *yaml.Node {
	copies := map[*yaml.Node]*yaml.Node{}
	var aliases []*yaml.Node
	var walk func(*yaml.Node) *yaml.Node
	walk = func(n *yaml.Node) *yaml.Node {
		c := *n
		copies[n] = &c
		if n.Kind == yaml.AliasNode {
			aliases = append(aliases, &c)
		}
		if n.Content != nil {
			c.Content = make([]*yaml.Node, len(n.Content))
			for i, child := range n.Content {
				c.Content[i] = walk(child)
			}
		}
		return &c
	}
	root := walk(n)
	for _, a := range aliases {
		if target, ok := copies[a.Alias]; ok {
			a.Alias = target
		}
	}
	return root
}
