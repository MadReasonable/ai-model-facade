// Package config loads the gateway config: parse to a yaml.Node tree, expand
// ${NAME} in scalar string values, then decode strictly so unknown keys are
// startup errors. Backend and virtual model entries are kept as nodes; each
// type decodes its own schema with DecodeStrict.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   Server    `yaml:"server"`
	Logging  Logging   `yaml:"logging"`
	API      API       `yaml:"api"`
	Vars     yaml.Node `yaml:"vars"` // ignored; a home for anchors
	Backends yaml.Node `yaml:"backends"`
	Models   yaml.Node `yaml:"models"`
	// Notes are what loading found that is likely a mistake but has a defined
	// meaning, such as one key listed under two names. They fail neither serve
	// nor check: serve logs them, check prints them.
	Notes []string `yaml:"-"`
	// Source is the Document the Config was parsed from, which the admin
	// endpoints show and a save writes.
	Source *Document `yaml:"-"`
}

type Server struct {
	Listen       string    `yaml:"listen"`
	APIKeys      []APIKey  `yaml:"apiKeys"`
	UnknownKeys  string    `yaml:"unknownKeys"` // reject (default) | allow
	TargetHeader string    `yaml:"targetHeader"`
	Responses    Responses `yaml:"responses"`
	// Groups names sets of keys, by the names of their entries in APIKeys.
	Groups map[string][]string `yaml:"groups"`
	// AdminAPI configures the endpoints that read and change the config.
	AdminAPI AdminAPI `yaml:"adminApi"`
	// keys finds a listed key's name by the key's SHA-256; see indexKeys.
	keys map[[sha256.Size]byte]string
	// groupsOf holds each listed key's groups; see indexGroups.
	groupsOf map[string][]string
}

// Responses limits the conversations the gateway keeps for Responses clients
// that name an earlier response in previous_response_id.
type Responses struct {
	MaxBytes      int64   `yaml:"maxBytes"`      // default 256 MiB
	MaxAgeMinutes float64 `yaml:"maxAgeMinutes"` // since a conversation's last turn; default 1440
}

// APIKey is one entry of server.apiKeys: a name for a caller, and either the
// key itself or its hash, exactly one of the two. They are pointers so that
// one given as an empty value, as a variable set to nothing expands to, is
// told apart from one left out.
type APIKey struct {
	Name string  `yaml:"name"`
	Key  *string `yaml:"key"`
	Hash *string `yaml:"hash"` // "sha256:" and 64 hex characters, as hash-key prints
}

type Logging struct {
	Level        string `yaml:"level"`
	LogSelection bool   `yaml:"logSelection"`
}

// SlogLevel returns Level as a slog level, info when it is left out.
func (l Logging) SlogLevel() (slog.Level, error) {
	switch l.Level {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("logging.level: unknown level '%s' (debug | info | warning | error)", l.Level)
}

// API configures what the gateway presents to clients of each API family.
type API struct {
	// Default is the family served at the root, bare /v1, so that clients
	// and configs written before families keep working. Default "openAi".
	Default   string    `yaml:"default"`
	OpenAI    APIFamily `yaml:"openAi"`
	Anthropic APIFamily `yaml:"anthropic"`
}

// The API families. A family owns a base path and a model-list format, and
// holds one or more protocols.
const (
	FamilyOpenAI    = "openAi"
	FamilyAnthropic = "anthropic"
)

// APIFamily configures one API family's endpoints.
type APIFamily struct {
	// BasePath is the path in front of the family's own paths, /v1 included.
	// Families overlap under /v1 (both have /v1/models, with different
	// shapes), so each needs its own. Default "/openai" and "/anthropic".
	BasePath string `yaml:"basePath"`
	// AddCurrent adds `current` to the family's model objects. Off by default,
	// since it makes a listing read backend state.
	AddCurrent bool `yaml:"addCurrent"`
	// Protocols are the family's protocols the gateway serves; nil means
	// every one, and an empty list turns the family off.
	Protocols []string `yaml:"protocols"`
}

// applyDefaults fills in the defaults and checks what clients would reach.
func (a *API) applyDefaults() error {
	switch a.Default {
	case "":
		a.Default = FamilyOpenAI
	case FamilyOpenAI, FamilyAnthropic:
	default:
		return fmt.Errorf("api.default: unknown family '%s'; use '%s' or '%s'", a.Default, FamilyOpenAI, FamilyAnthropic)
	}
	if a.OpenAI.BasePath == "" {
		a.OpenAI.BasePath = "/openai"
	}
	if a.Anthropic.BasePath == "" {
		a.Anthropic.BasePath = "/anthropic"
	}
	if err := checkBasePath(FamilyOpenAI, a.OpenAI.BasePath); err != nil {
		return err
	}
	if err := checkBasePath(FamilyAnthropic, a.Anthropic.BasePath); err != nil {
		return err
	}
	if a.OpenAI.BasePath == a.Anthropic.BasePath {
		return fmt.Errorf("api: families '%s' and '%s' both have basePath '%s'", FamilyOpenAI, FamilyAnthropic, a.OpenAI.BasePath)
	}
	return nil
}

// checkBasePath refuses a base path that can't be a route, or that would
// shadow the root's /v1 or the gateway's own endpoints.
func checkBasePath(family, basePath string) error {
	if !strings.HasPrefix(basePath, "/") || basePath == "/" || path.Clean(basePath) != basePath || strings.ContainsAny(basePath, "{} ") {
		return fmt.Errorf("api.%s.basePath: '%s' is not a path such as '/%s'", family, basePath, strings.ToLower(family))
	}
	for _, reserved := range []string{"/v1", "/amf", "/health"} {
		if basePath == reserved || strings.HasPrefix(basePath, reserved+"/") {
			return fmt.Errorf("api.%s.basePath: '%s' is under '%s', which the gateway serves itself", family, basePath, reserved)
		}
	}
	return nil
}

func (r *Responses) applyDefaults() error {
	if r.MaxBytes < 0 {
		return fmt.Errorf("server.responses.maxBytes: %d is negative", r.MaxBytes)
	}
	if r.MaxAgeMinutes < 0 {
		return fmt.Errorf("server.responses.maxAgeMinutes: %g is negative", r.MaxAgeMinutes)
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = 256 << 20
	}
	if r.MaxAgeMinutes == 0 {
		r.MaxAgeMinutes = 24 * 60
	}
	return nil
}

// KeepBackups returns how many backups of the config files a save keeps:
// server.adminApi.configBackups, 10 by default. With 0, a save removes its
// own backup once it succeeds.
func (a AdminAPI) KeepBackups() int {
	if a.ConfigBackups == nil {
		return 10
	}
	return *a.ConfigBackups
}

// UnknownClient is the client name logged for a key not in server.apiKeys.
const UnknownClient = "unknown"

// Entry is one named item under `backends:` or `models:`.
type Entry struct {
	Name string
	Type string // the entry's `type` key; "" when absent
	Node *yaml.Node
}

// Load reads, expands and strictly decodes the config file at path, with
// the secrets file at secretsPath merged in, or none if it is "". An error
// names the file it is in.
func Load(path, secretsPath string) (*Config, error) {
	d, err := ReadFiles(path, secretsPath)
	if err != nil {
		return nil, err
	}
	cfg, err := d.Parse(os.LookupEnv)
	if err != nil {
		return nil, nameFile(err, path, secretsPath)
	}
	return cfg, nil
}

// ReadFiles reads the config file at path, and the secrets file at
// secretsPath unless it is "", as a Document with the Origin of the bytes
// read, without expanding or checking it. An error names the file it is in.
func ReadFiles(path, secretsPath string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var secretsData []byte
	if secretsPath != "" {
		if secretsData, err = os.ReadFile(secretsPath); err != nil {
			return nil, err
		}
	}
	d, err := ReadDocument(data, secretsData)
	if err != nil {
		return nil, nameFile(err, path, secretsPath)
	}
	d.Origin = &Origin{Main: sha256.Sum256(data)}
	if secretsData != nil {
		d.Origin.Secrets = sha256.Sum256(secretsData)
	}
	return d, nil
}

// nameFile puts the name of the file an error is in before it: the secrets
// file for a *SecretsError, else the config.
func nameFile(err error, path, secretsPath string) error {
	var inSecrets *SecretsError
	if errors.As(err, &inSecrets) {
		return fmt.Errorf("%s: %w", secretsPath, err)
	}
	return fmt.Errorf("%s: %w", path, err)
}

// Listen reads server.listen from the config file at path, with ${NAME}
// expanded, and nothing else, for the health command: it runs without the
// secrets file, so it must not fail on what the rest of the config needs
// that file for, such as an access list naming a key defined there.
func Listen(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", fmt.Errorf("%s: config is empty", path)
	}
	server, _ := lookup(doc.Content[0], "server")
	var listen *yaml.Node
	if server != nil {
		listen, _ = lookup(server, "listen")
	}
	if listen == nil || resolve(listen).Kind != yaml.ScalarNode || resolve(listen).Value == "" {
		return "", fmt.Errorf("%s: server.listen is required", path)
	}
	value := *resolve(listen) // a copy, since expanding changes it
	if err := expandEnv(&value, os.LookupEnv); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return value.Value, nil
}

// Parse is Load without the file read or a secrets file, with an injectable
// environment.
func Parse(data []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	return ParseWithSecrets(data, nil, lookupEnv)
}

// ParseWithSecrets is Parse with a secrets file merged in, nil for none. An
// error in the secrets file itself is a *SecretsError.
func ParseWithSecrets(data, secretsData []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	d, err := ReadDocument(data, secretsData)
	if err != nil {
		return nil, err
	}
	return d.Parse(lookupEnv)
}

// Entries lists a section's named entries in file order. A repeated name is an
// error: callers keep entries both in order and in a map by name, and the two
// would disagree on which definition the name means.
func Entries(section *yaml.Node, sectionName string) ([]Entry, error) {
	section = resolve(section)
	if section.Kind == 0 || isNull(section) {
		return nil, nil
	}
	if section.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: %s must be a map", section.Line, sectionName)
	}
	var entries []Entry
	seen := map[string]int{}
	for i := 0; i < len(section.Content); i += 2 {
		k, v := section.Content[i], resolve(section.Content[i+1])
		if first, ok := seen[k.Value]; ok {
			return nil, fmt.Errorf("line %d: %s.%s is defined again (first at line %d)", k.Line, sectionName, k.Value, first)
		}
		seen[k.Value] = k.Line
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: %s.%s must be a map", v.Line, sectionName, k.Value)
		}
		e := Entry{Name: k.Value, Node: v}
		for j := 0; j < len(v.Content); j += 2 {
			if v.Content[j].Value == "type" {
				e.Type = resolve(v.Content[j+1]).Value
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} in scalar string values. Alias nodes are skipped:
// their targets are expanded where the anchor is defined.
func expandEnv(n *yaml.Node, lookupEnv func(string) (string, bool)) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := expandEnv(n.Content[i], lookupEnv); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := expandEnv(c, lookupEnv); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" || !strings.Contains(n.Value, "${") {
			return nil
		}
		var missing string
		n.Value = envPattern.ReplaceAllStringFunc(n.Value, func(m string) string {
			name := m[2 : len(m)-1]
			v, ok := lookupEnv(name)
			if !ok && missing == "" {
				missing = name
			}
			return v
		})
		if missing != "" {
			return fmt.Errorf("line %d: environment variable %s is not defined", n.Line, missing)
		}
	}
	return nil
}

func resolve(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
