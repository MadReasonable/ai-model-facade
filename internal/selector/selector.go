// Package selector picks the concrete backend model for a request.
package selector

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// Selection is the outcome of Select. Index is -1 when no candidate matched.
type Selection struct {
	Index   int
	Target  string   // concrete backend model ID, or alias
	AliasOf string   // the base model when Target is an alias
	Reason  string   // why the winner won, or why nothing did
	Checked []string // "model: outcome" for each candidate examined, in order
}

type Selector interface {
	// Requires lists the backend capabilities Select needs; checked at config load.
	Requires() []backend.Capability
	Select(ctx context.Context, body []byte, b backend.Backend) (Selection, error)
}

type Candidate struct {
	Model      string // exact ID or path.Match glob
	LoadIfNone bool   // eligible even when not loaded; the backend loads it if it wins
}

// IsGlob reports whether model is a pattern rather than an exact ID.
func IsGlob(model string) bool {
	return strings.ContainsAny(model, `*?[\`)
}

// FirstLoaded picks the first candidate the backend reports ready. An alias
// is ready when its base model is. A glob matches ready base models first,
// then their aliases, each in sorted order, so `qwen3.8-27b:*` picks the
// model itself over its `qwen3.8-27b:Dreamer` preset. Only when no
// candidate is ready does the first LoadIfNone candidate win, wherever it is
// in the list, and the backend loads it.
type FirstLoaded struct {
	Candidates []Candidate
}

// Validate checks what can be checked without the backend.
func (c Candidate) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("model is required")
	}
	if !IsGlob(c.Model) {
		return nil
	}
	if _, err := path.Match(c.Model, ""); err != nil {
		return fmt.Errorf("bad pattern %q: %w", c.Model, err)
	}
	if c.LoadIfNone {
		return fmt.Errorf("loadIfNone needs an exact model ID, not pattern %q", c.Model)
	}
	return nil
}

func (s *FirstLoaded) Requires() []backend.Capability {
	return []backend.Capability{backend.ModelState}
}

func (s *FirstLoaded) Select(ctx context.Context, body []byte, b backend.Backend) (Selection, error) {
	sel := Selection{Index: -1}
	loadedModels, err := b.(backend.ModelStateProvider).LoadedModels(ctx)
	if err != nil {
		sel.Reason = "backend state unavailable"
		return sel, err
	}
	bases, aliases, aliasOf := ranked(loadedModels)
	fallback := -1 // first LoadIfNone candidate, used if nothing is loaded
	for i, c := range s.Candidates {
		if IsGlob(c.Model) {
			if id, ok := firstMatch(c.Model, bases); ok {
				return winner(sel, i, id, "", c.Model+": loaded as "+id, "loaded"), nil
			}
			if id, ok := firstMatch(c.Model, aliases); ok {
				return winner(sel, i, id, aliasOf[id], c.Model+": loaded as "+id+" (alias of "+aliasOf[id]+")", "loaded"), nil
			}
			sel.Checked = append(sel.Checked, c.Model+": no match loaded")
			continue
		}
		if slices.Contains(bases, c.Model) {
			return winner(sel, i, c.Model, "", c.Model+": loaded", "loaded"), nil
		}
		if base, ok := aliasOf[c.Model]; ok {
			return winner(sel, i, c.Model, base, c.Model+": loaded (alias of "+base+")", "loaded"), nil
		}
		if c.LoadIfNone && fallback < 0 {
			fallback = i
		}
		sel.Checked = append(sel.Checked, c.Model+": not loaded")
	}
	if fallback >= 0 {
		model := s.Candidates[fallback].Model
		return winner(sel, fallback, model, "", model+": loading (loadIfNone)", "loadIfNone"), nil
	}
	sel.Reason = "no candidate loaded"
	return sel, nil
}

func winner(sel Selection, index int, target, aliasOf, checked, reason string) Selection {
	sel.Index = index
	sel.Target = target
	sel.AliasOf = aliasOf
	sel.Checked = append(sel.Checked, checked)
	sel.Reason = reason
	return sel
}

// Matches returns the IDs among models that pattern matches, in the order
// Select would try them: base models first, then aliases, each sorted. An
// alias whose base model also matches is left out: the two are loaded
// together and the base is preferred, so that alias can never win.
func Matches(pattern string, models []backend.ModelInfo) (bases, aliases []string) {
	allBases, allAliases, aliasOf := ranked(models)
	bases = allMatches(pattern, allBases)
	for _, a := range allMatches(pattern, allAliases) {
		if !slices.Contains(bases, aliasOf[a]) {
			aliases = append(aliases, a)
		}
	}
	return bases, aliases
}

// ranked splits models into sorted base IDs and sorted alias IDs.
func ranked(models []backend.ModelInfo) (bases, aliases []string, aliasOf map[string]string) {
	aliasOf = map[string]string{}
	for _, m := range models {
		bases = append(bases, m.ID)
		for _, a := range m.Aliases {
			aliases = append(aliases, a)
			aliasOf[a] = m.ID
		}
	}
	slices.Sort(bases)
	slices.Sort(aliases)
	return bases, aliases, aliasOf
}

func firstMatch(pattern string, ids []string) (string, bool) {
	for _, id := range ids {
		if ok, _ := path.Match(pattern, id); ok {
			return id, true
		}
	}
	return "", false
}

func allMatches(pattern string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if ok, _ := path.Match(pattern, id); ok {
			out = append(out, id)
		}
	}
	return out
}
