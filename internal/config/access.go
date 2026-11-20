package config

import (
	"fmt"
	"slices"
	"strings"
)

// The built-in groups. Every caller the gateway accepts is in exactly one:
// a listed key in GroupAll, anything else in GroupUnknown, which is also the
// client name logged for an unlisted key.
const (
	GroupAll     = "all"
	GroupUnknown = UnknownClient
)

// Access says which callers may use a facade or a backend. With both lists
// left out, every caller the gateway accepts may; otherwise a caller may if
// its key is in AllowedKeys or one of its groups is in AllowedGroups, so an
// empty list allows no one.
type Access struct {
	AllowedKeys   []string `yaml:"allowedKeys"`
	AllowedGroups []string `yaml:"allowedGroups"`
}

// Allows reports whether a caller with key name name and groups may use what
// a is on. An unknown caller's name matches no AllowedKeys entry, since those
// are checked to be listed keys.
func (a Access) Allows(name string, groups []string) bool {
	if a.AllowedKeys == nil && a.AllowedGroups == nil {
		return true
	}
	if name != "" && slices.Contains(a.AllowedKeys, name) {
		return true
	}
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(a.AllowedGroups, g) })
}

// indexGroups checks server.groups against server.apiKeys and records each
// key's groups. Key names, group names and the built-in groups are one
// namespace, because access lists and key maps take a name of either kind.
func (cfg *Config) indexGroups() error {
	s := &cfg.Server
	s.groupsOf = map[string][]string{}
	for _, k := range s.APIKeys {
		s.groupsOf[k.Name] = []string{GroupAll}
	}
	names := make([]string, 0, len(s.Groups))
	for name := range s.Groups {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		switch {
		case name == "":
			return fmt.Errorf("server.groups: a group needs a name")
		case name == GroupAll || name == GroupUnknown:
			return fmt.Errorf("server.groups.%s: '%s' is a built-in group", name, name)
		case s.groupsOf[name] != nil:
			return fmt.Errorf("server.groups.%s: '%s' is already the name of a key in server.apiKeys", name, name)
		}
		for _, member := range s.Groups[name] {
			if s.groupsOf[member] == nil {
				return fmt.Errorf("server.groups.%s: '%s' is not a name in server.apiKeys", name, member)
			}
			if !slices.Contains(s.groupsOf[member], name) {
				s.groupsOf[member] = append(s.groupsOf[member], name)
			}
		}
	}
	return nil
}

// GroupsOf returns the groups of the listed key named name, GroupAll among
// them, in name order after it; nil if no key has that name.
func (s *Server) GroupsOf(name string) []string {
	return s.groupsOf[name]
}

// AdminAPI configures the admin endpoints: the callers who may use them, the
// backups a save keeps, and the copy of the log GET /amf/logs reads. Unlike
// a facade's or a backend's list, a list left out allows no one and turns
// off the endpoints it guards, since an admin grant is never implicit.
type AdminAPI struct {
	// Read guards the endpoints that show the gateway: GET /amf/config and
	// its paths, /amf/config-status, /amf/config-backups and /amf/logs.
	Read Access `yaml:"read"`
	// Write guards the endpoints that change the draft, what is running or
	// the files: PUT and DELETE on /amf/config and its paths, and POST
	// /amf/config-apply, -load, -revert and -save, and POST /amf/reload.
	Write Access `yaml:"write"`
	// ConfigBackups is how many backups of the config files a save keeps;
	// nil means the default, KeepBackups.
	ConfigBackups *int `yaml:"configBackups"`
	Logs          Logs `yaml:"logs"`
}

// CanRead reports whether a caller with key name name and groups may use the
// read endpoints: one listed in read, or in write, since a client that edits
// the draft reads it to see what it changes and to get each ETag.
func (a AdminAPI) CanRead(name string, groups []string) bool {
	return (a.Read.On() && a.Read.Allows(name, groups)) || (a.Write.On() && a.Write.Allows(name, groups))
}

// ReadOn reports whether the read endpoints are served.
func (a AdminAPI) ReadOn() bool {
	return a.Read.On() || a.Write.On()
}

// Writers reports whether some key in s.APIKeys may use the write endpoints,
// so that applying a config can't leave no admin able to change it again.
func (s *Server) Writers() bool {
	if !s.AdminAPI.Write.On() {
		return false
	}
	for _, k := range s.APIKeys {
		if s.AdminAPI.Write.Allows(k.Name, s.groupsOf[k.Name]) {
			return true
		}
	}
	return false
}

// On reports whether a lists anyone, so that the endpoints it guards are
// served; a list given empty is on and allows no one.
func (a Access) On() bool {
	return a.AllowedKeys != nil || a.AllowedGroups != nil
}

// checkAdmins checks server.adminApi like any access list, and refuses the
// unknown group, which would grant admin to a caller the gateway can't tell
// apart from any other.
func (s *Server) checkAdmins() error {
	for _, list := range []struct {
		name   string
		access Access
	}{{"read", s.AdminAPI.Read}, {"write", s.AdminAPI.Write}} {
		if err := s.CheckAccess(list.access); err != nil {
			return fmt.Errorf("server.adminApi.%s.%w", list.name, err)
		}
		if slices.Contains(list.access.AllowedGroups, GroupUnknown) {
			return fmt.Errorf("server.adminApi.%s.allowedGroups: '%s' can't be an admin; name keys from server.apiKeys or their groups", list.name, GroupUnknown)
		}
	}
	return nil
}

// CheckAccess fails if a names a key or a group the config doesn't define.
func (s *Server) CheckAccess(a Access) error {
	for _, name := range a.AllowedKeys {
		if s.groupsOf[name] == nil {
			return fmt.Errorf("allowedKeys: '%s' is not a name in server.apiKeys", name)
		}
	}
	for _, name := range a.AllowedGroups {
		if !s.isGroup(name) {
			return fmt.Errorf("allowedGroups: '%s' is not a group; use a name in server.groups, '%s' or '%s'", name, GroupAll, GroupUnknown)
		}
	}
	return nil
}

// CheckKeyMap fails if a backend's keyMap names something other than a key
// or a group of the config's own. The built-in groups can't have entries,
// since apiKey is already the key for every caller the map doesn't name, and
// an entry for `all` would come before or after every group's. It returns a
// note for each pair of groups that both have entries for keys with none of
// their own: those keys get the first one's key, by keyMap order, which is
// the one thing in a keyMap its order decides.
func (s *Server) CheckKeyMap(keyMap KeyMap) ([]string, error) {
	for _, e := range keyMap {
		switch {
		case e.Name == GroupAll || e.Name == GroupUnknown:
			return nil, fmt.Errorf("keyMap: '%s' can't have an entry; apiKey is the key for every caller keyMap doesn't name", e.Name)
		case s.groupsOf[e.Name] == nil && !s.isGroup(e.Name):
			return nil, fmt.Errorf("keyMap: '%s' is not a name in server.apiKeys or server.groups", e.Name)
		}
	}
	type pair struct{ first, other string }
	var pairs []pair
	keysOf := map[pair][]string{}
	counted := map[string]bool{}
	for _, k := range s.APIKeys {
		if counted[k.Name] {
			continue // a name with several keys is one caller
		}
		counted[k.Name] = true
		if _, own := keyMap.Key(k.Name); own {
			continue
		}
		var mapped []string
		for _, e := range keyMap {
			if slices.Contains(s.groupsOf[k.Name], e.Name) {
				mapped = append(mapped, e.Name)
			}
		}
		for _, other := range mapped[min(1, len(mapped)):] {
			p := pair{mapped[0], other}
			if keysOf[p] == nil {
				pairs = append(pairs, p)
			}
			keysOf[p] = append(keysOf[p], k.Name)
		}
	}
	var notes []string
	for _, p := range pairs {
		keys := keysOf[p]
		if len(keys) == 1 {
			notes = append(notes, fmt.Sprintf("keyMap: key '%s' is in both '%s' and '%s', with no entry of its own; '%s' comes first, so its key is sent", keys[0], p.first, p.other, p.first))
			continue
		}
		notes = append(notes, fmt.Sprintf("keyMap: %d keys are in both '%s' and '%s', with no entries of their own; '%s' comes first, so its key is sent for them", len(keys), p.first, p.other, p.first))
	}
	return notes, nil
}

func (s *Server) isGroup(name string) bool {
	_, defined := s.Groups[name]
	return defined || name == GroupAll || name == GroupUnknown
}

// Describe returns a's lists for the check command, one line each, or nothing
// when a restricts no one.
func (a Access) Describe() []string {
	var lines []string
	if a.AllowedKeys != nil {
		lines = append(lines, "allowed keys: "+listOrNone(a.AllowedKeys))
	}
	if a.AllowedGroups != nil {
		lines = append(lines, "allowed groups: "+listOrNone(a.AllowedGroups))
	}
	return lines
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
