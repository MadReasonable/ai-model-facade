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
