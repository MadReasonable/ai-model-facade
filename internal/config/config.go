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
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   Server    `yaml:"server" doc:"How the gateway listens, which keys it accepts, and who may use the admin API."`
	Logging  Logging   `yaml:"logging" doc:"What the gateway logs to stderr."`
	API      API       `yaml:"api" doc:"What the gateway presents to the clients of each API family."`
	Vars     yaml.Node `yaml:"vars" doc:"YAML anchors that entries use explicitly with an alias or a merge; the gateway reads nothing here itself." schema:"kind=anchors"` // ignored; a home for anchors
	Backends yaml.Node `yaml:"backends" doc:"The servers that run models, each named, with type selecting its adapter." schema:"kind=backends"`
	Models   yaml.Node `yaml:"models" doc:"The facades: model names that exist only here, each choosing a real model from its candidates." schema:"kind=models"`
	// Notes are what loading found that is likely a mistake but has a defined
	// meaning, such as one key listed under two names. They fail neither serve
	// nor check: serve logs them, check prints them.
	Notes []string `yaml:"-"`
	// Source is the Document the Config was parsed from, which the admin
	// endpoints show and a save writes.
	Source *Document `yaml:"-"`
}

type Server struct {
	Listen       string    `yaml:"listen" doc:"Address and port to listen on. 127.0.0.1 accepts local connections only; 0.0.0.0 or a specific IP accepts them from other machines, so set apiKeys too. A change takes effect on restart." schema:"required"`
	APIKeys      []APIKey  `yaml:"apiKeys" doc:"The keys clients may send, each with a name that appears in the log and in access lists. Left out, every request is accepted."`
	UnknownKeys  string    `yaml:"unknownKeys" doc:"What happens to a key not in apiKeys: reject answers 401; allow accepts it and logs it as unknown with a fingerprint." schema:"enum=reject|allow,default=reject"` // reject (default) | allow
	TargetHeader string    `yaml:"targetHeader" doc:"The response header that names the model that answered." schema:"default=X-Gateway-Target"`
	Responses    Responses `yaml:"responses" doc:"Limits on the Responses conversations the gateway keeps for clients that send previous_response_id."`
	// Groups names sets of keys, by the names of their entries in APIKeys.
	Groups map[string][]string `yaml:"groups" doc:"Named groups of keys, each a list of names from apiKeys, for access lists. all and unknown are built in." schema:"ref=keys"`
	// TLS is the HTTPS of the side listen serves.
	TLS TLS `yaml:"tls" doc:"HTTPS support for the client APIs, and the admin endpoints and admin UI unless adminApi.listen moves them."`
	// AdminAPI configures the endpoints that read and change the config and
	// read the log.
	AdminAPI AdminAPI `yaml:"adminApi" doc:"Who may read and change the config and read the log over /amf, and where those endpoints listen."`
	// keys finds a listed key's name by the key's SHA-256; see indexKeys.
	keys map[[sha256.Size]byte]string
	// groupsOf holds each listed key's groups; see indexGroups.
	groupsOf map[string][]string
}

// Responses limits the conversations the gateway keeps for Responses clients
// that name an earlier response in previous_response_id.
type Responses struct {
	MaxBytes      int64   `yaml:"maxBytes" doc:"The most the kept conversations may hold; past it, the least recently used are evicted." schema:"default=268435456,min=0,unit=bytes"` // default 256 MiB
	MaxAgeMinutes float64 `yaml:"maxAgeMinutes" doc:"How long a conversation is kept after its last turn." schema:"default=1440,min=0,unit=minutes"`                                  // since a conversation's last turn; default 1440
}

// APIKey is one entry of server.apiKeys: a name for a caller, and either the
// key itself or its hash, exactly one of the two. They are pointers so that
// one given as an empty value, as a variable set to nothing expands to, is
// told apart from one left out.
type APIKey struct {
	Name string  `yaml:"name" doc:"The caller's name, in the log and in access lists. A name may have several entries, one per key; all and unknown are reserved." schema:"required"`
	Key  *string `yaml:"key" doc:"The key itself, which the gateway hashes when the config loads. Give key or hash, not both." schema:"secret"`
	Hash *string `yaml:"hash" doc:"The key's hash, as hash-key prints it: sha256: and 64 hex characters. The hash can't be used as a key."` // "sha256:" and 64 hex characters, as hash-key prints
}

// Logs configures the gateway's own copy of its log, which GET /amf/logs
// reads and streams.
type Logs struct {
	// Level is the lowest level the copy keeps, which may be below
	// logging.level, so the endpoint can show debug lines while stderr
	// doesn't. Left out, it is logging.level.
	Level string `yaml:"level" doc:"The lowest level the copy keeps. It may be below logging.level, so the log endpoint shows debug lines while stderr doesn't. Left out, it is logging.level." schema:"enum=debug|info|warning|error"`
	// MaxBytes is the most the copy holds; past it, the oldest lines are
	// dropped, though the newest is always kept, so the last line logged can
	// be read when nothing else can. 0 keeps none, so the endpoint only
	// streams. nil means the default, 1 MiB; see KeepBytes.
	MaxBytes *int64 `yaml:"maxBytes" doc:"The most the copy holds; past it, the oldest lines are dropped, but the newest is always kept. 0 keeps no history, so the endpoint only streams." schema:"default=1048576,min=0,unit=bytes"`
}

// KeepBytes returns the most the copy of the log holds:
// server.adminApi.logs.maxBytes, 1 MiB by default, 0 for none.
func (l Logs) KeepBytes() int64 {
	if l.MaxBytes == nil {
		return 1 << 20
	}
	return *l.MaxBytes
}

func (l *Logs) applyDefaults() error {
	if l.MaxBytes != nil && *l.MaxBytes < 0 {
		return fmt.Errorf("server.adminApi.logs.maxBytes: %d is negative", *l.MaxBytes)
	}
	if l.Level != "" {
		if _, err := parseLevel(l.Level); err != nil {
			return fmt.Errorf("server.adminApi.logs.level: %w", err)
		}
	}
	return nil
}

// SlogLevel returns Level as a slog level, or fallback, logging.level's,
// when it is left out.
func (l Logs) SlogLevel(fallback slog.Level) slog.Level {
	if l.Level == "" {
		return fallback
	}
	level, _ := parseLevel(l.Level) // checked at load
	return level
}

type Logging struct {
	Level        string `yaml:"level" doc:"The lowest level written to stderr." schema:"enum=debug|info|warning|error,default=info"`
	LogSelection bool   `yaml:"logSelection" doc:"Log one line per request: facade, candidates checked, winner and reason. Failed requests are always logged."`
}

// SlogLevel returns Level as a slog level, info when it is left out.
func (l Logging) SlogLevel() (slog.Level, error) {
	level, err := parseLevel(l.Level)
	if err != nil {
		return 0, fmt.Errorf("logging.level: %w", err)
	}
	return level, nil
}

// parseLevel returns the slog level a config names, info for "".
func parseLevel(name string) (slog.Level, error) {
	switch name {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown level '%s' (debug | info | warning | error)", name)
}

// API configures what the gateway presents to clients of each API family.
type API struct {
	// Default is the family served at the root, bare /v1, so that clients
	// and configs written before families keep working. Default "openAi".
	Default   string    `yaml:"default" doc:"The API served at the root, bare /v1, so clients written before API families keep working." schema:"enum=openAi|anthropic,default=openAi"`
	OpenAI    APIFamily `yaml:"openAi" doc:"OpenAI's API: Chat Completions, legacy completions and Responses."`
	Anthropic APIFamily `yaml:"anthropic" doc:"Anthropic's API: Messages."`
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
	BasePath string `yaml:"basePath" doc:"The path in front of the API's own paths, /v1 included. Default /openai and /anthropic."`
	// AddCurrent adds `current` to the family's model objects. Off by default,
	// since it makes a listing read backend state.
	AddCurrent bool `yaml:"addCurrent" doc:"Add current, where a request would go now, to the API's model list. Off, the list reads no backend's state."`
	// Protocols are the family's protocols the gateway serves; nil means
	// every one, and an empty list turns the family off.
	Protocols []string `yaml:"protocols" doc:"The API's protocols the gateway serves; a protocol left out has no endpoint, and an empty list turns the API off. Left out, all of them." schema:"ref=protocols"`
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

// applyUIDefault fills in the admin UI's path and refuses one that would
// overlap another route: /v1, /health, either client API's basePath, in
// either direction, or /amf, where the admin endpoints are, other than the
// default. Those hold whichever listener serves the UI, since a change of
// listen waits for a restart; / is the exception, allowed only with listen,
// and not served until the gateway listens there (Live.uiPathFor).
func (a *AdminAPI) applyUIDefault(api API) error {
	p := a.UIBasePath
	if p == "" {
		a.UIBasePath = DefaultUIBasePath
		return nil
	}
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "{} ") {
		return fmt.Errorf("server.adminApi.uiBasePath: '%s' is not a path such as '/admin', or '/'", p)
	}
	if p == "/" {
		if a.Listen == "" {
			return fmt.Errorf("server.adminApi.uiBasePath: '/' needs server.adminApi.listen, since server.listen serves the client APIs at its root")
		}
		return nil
	}
	if err := CheckUIBasePath(p, api); err != nil {
		return fmt.Errorf("server.adminApi.uiBasePath: %w", err)
	}
	return nil
}

// CheckUIBasePath refuses a path for the admin UI, other than the root,
// that overlaps a route of the gateway's own on a listener that also serves
// the client APIs of api.
func CheckUIBasePath(p string, api API) error {
	if p != DefaultUIBasePath && (p == "/amf" || strings.HasPrefix(p, "/amf/")) {
		return fmt.Errorf("'%s' is under '/amf', where the admin endpoints are; only the default, '%s', is allowed there", p, DefaultUIBasePath)
	}
	for _, taken := range []string{"/v1", "/health", api.OpenAI.BasePath, api.Anthropic.BasePath} {
		if p == taken || strings.HasPrefix(p, taken+"/") || strings.HasPrefix(taken, p+"/") {
			return fmt.Errorf("'%s' overlaps '%s', which the gateway serves itself", p, taken)
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
// the secrets file at secretsPath merged in, or none if it is "", and reads
// the TLS key pairs it names, from path's folder for a relative path. An
// error names the file it is in.
func Load(path, secretsPath string) (*Config, error) {
	d, err := ReadFiles(path, secretsPath)
	if err != nil {
		return nil, err
	}
	cfg, err := d.Parse(os.LookupEnv)
	if err != nil {
		return nil, nameFile(err, path, secretsPath)
	}
	if err := cfg.LoadTLS(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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

// HealthAddress reads where a local health check reaches the gateway at
// path: server.listen over plain HTTP, or, where server.tls serves only
// HTTPS, its HTTPS address, with https set. It reads those keys, with
// ${NAME} expanded, and nothing else, for the health command: it runs
// without the secrets file, so it must not fail on what the rest of the
// config needs that file for, such as an access list naming a key defined
// there.
func HealthAddress(path string) (addr string, https bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", false, fmt.Errorf("%s: config is empty", path)
	}
	server, _ := lookup(doc.Content[0], "server")
	listen, err := scalarAt(server, "listen")
	if err != nil {
		return "", false, fmt.Errorf("%s: server.listen: %w", path, err)
	}
	if listen == "" {
		return "", false, fmt.Errorf("%s: server.listen is required", path)
	}
	var tlsBlock *yaml.Node
	if server != nil {
		tlsBlock, _ = lookup(server, "tls")
	}
	values := map[string]string{}
	for _, key := range []string{"cert", "serve", "listen"} {
		if values[key], err = scalarAt(tlsBlock, key); err != nil {
			return "", false, fmt.Errorf("%s: server.tls.%w", path, err)
		}
	}
	if values["cert"] != "" && values["serve"] == ServeHTTPS {
		return firstSet(values["listen"], listen), true, nil
	}
	return listen, false, nil
}

// scalarAt is the value of key in mapping, with ${NAME} expanded, "" if
// mapping or key is absent. An error starts with key.
func scalarAt(mapping *yaml.Node, key string) (string, error) {
	if mapping == nil || resolve(mapping).Kind != yaml.MappingNode {
		return "", nil
	}
	n, _ := lookup(resolve(mapping), key)
	if n == nil {
		return "", nil
	}
	if resolve(n).Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%s is not a single value", key)
	}
	value := *resolve(n) // a copy, since expanding changes it
	if err := expandEnv(&value, os.LookupEnv); err != nil {
		return "", err
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
		if err := checkName(k.Value, sectionName == "models"); err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", k.Line, sectionName, err)
		}
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
