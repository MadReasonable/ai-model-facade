// Package selector picks the concrete backend model for a request.
package selector

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

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
	// Backend serves this candidate; nil means the backend passed to Select.
	Backend backend.Backend
	// BackendName, if set, is shown with the model in Checked: a candidate
	// on another backend than its virtual model's own.
	BackendName string
}

// On names a model ID as this candidate's log lines show it.
func (c Candidate) On(id string) string {
	if c.BackendName == "" {
		return id
	}
	return id + " on " + c.BackendName
}

func (c Candidate) backendOr(b backend.Backend) backend.Backend {
	if c.Backend != nil {
		return c.Backend
	}
	return b
}

// IsGlob reports whether model is a pattern rather than an exact ID.
func IsGlob(model string) bool {
	return strings.ContainsAny(model, `*?[\`)
}

// FirstLoaded picks, in order of preference:
//
//  1. the first candidate the backend reports ready;
//  2. otherwise the first candidate that is starting: the request waits for
//     a load already under way instead of triggering another;
//  3. otherwise the first LoadIfNone candidate, which the backend loads.
//
// Each step walks the whole list, so a LoadIfNone candidate can sit anywhere.
// A model whose state the backend can't read counts as not loaded.
// Candidates may be on different backends; each backend's state is read
// once, concurrently. If some backends' state can't be read, their
// candidates count as not loaded; if none can be, Select fails.
// An alias is ready or starting when its base model is. A glob matches base
// models first, then their aliases, each in sorted order, so `qwen3.8-27b:*`
// picks the model itself over its `qwen3.8-27b:Dreamer` preset.
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
		return fmt.Errorf("bad pattern '%s': %w", c.Model, err)
	}
	if c.LoadIfNone {
		return fmt.Errorf("loadIfNone needs an exact model ID, not pattern '%s'", c.Model)
	}
	return nil
}

func (s *FirstLoaded) Requires() []backend.Capability {
	return []backend.Capability{backend.ModelState}
}

func (s *FirstLoaded) Select(ctx context.Context, body []byte, b backend.Backend) (Selection, error) {
	sel := Selection{Index: -1}
	states := s.states(ctx, b)
	var firstErr error
	for _, c := range s.Candidates {
		if err := states[c.backendOr(b)].err; err == nil {
			firstErr = nil
			break
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		sel.Reason = "backend state unavailable"
		return sel, firstErr
	}
	wait := -1     // first starting candidate, used if nothing is ready
	fallback := -1 // first LoadIfNone candidate, used if nothing is ready or starting
	var waitID, waitBase string
	for i, c := range s.Candidates {
		st := states[c.backendOr(b)]
		if st.err != nil {
			sel.Checked = append(sel.Checked, c.On(c.Model)+": state unavailable")
		} else if id, base, ok := st.ready.match(c.Model); ok {
			return winner(sel, i, id, base, describe(c, id, base, "loaded"), "loaded"), nil
		} else if id, base, ok := st.starting.match(c.Model); ok {
			sel.Checked = append(sel.Checked, describe(c, id, base, "starting"))
			if wait < 0 {
				wait, waitID, waitBase = i, id, base
			}
		} else if id, _, ok := st.unknown.match(c.Model); ok {
			// Selects like not loaded, but the log shouldn't claim it isn't.
			if IsGlob(c.Model) {
				sel.Checked = append(sel.Checked, c.On(c.Model)+": no match loaded; state unknown for "+id)
			} else {
				sel.Checked = append(sel.Checked, c.On(c.Model)+": state unknown")
			}
		} else if IsGlob(c.Model) {
			sel.Checked = append(sel.Checked, c.On(c.Model)+": no match loaded")
		} else {
			sel.Checked = append(sel.Checked, c.On(c.Model)+": not loaded")
		}
		if c.LoadIfNone && fallback < 0 {
			fallback = i
		}
	}
	if wait >= 0 {
		return winner(sel, wait, waitID, waitBase, s.Candidates[wait].On(waitID)+": waiting for it to finish starting", "starting"), nil
	}
	if fallback >= 0 {
		c := s.Candidates[fallback]
		return winner(sel, fallback, c.Model, "", c.On(c.Model)+": loading (loadIfNone)", "loadIfNone"), nil
	}
	sel.Reason = "no candidate loaded"
	return sel, nil
}

// backendState is one backend's models, sorted by state, or why they
// couldn't be read.
type backendState struct {
	ready, starting, unknown modelSet
	err                      error
}

// states reads the state of every backend the candidates use, once each and
// concurrently, so a mixed list costs its slowest backend, not their sum.
func (s *FirstLoaded) states(ctx context.Context, b backend.Backend) map[backend.Backend]*backendState {
	states := map[backend.Backend]*backendState{}
	var wg sync.WaitGroup
	for _, c := range s.Candidates {
		cb := c.backendOr(b)
		if states[cb] != nil {
			continue
		}
		st := &backendState{}
		states[cb] = st
		wg.Add(1)
		go func() {
			defer wg.Done()
			loadedModels, err := cb.(backend.ModelStateProvider).LoadedModels(ctx)
			if err != nil {
				st.err = err
				return
			}
			var readyModels, startingModels, unknownModels []backend.ModelInfo
			for _, m := range loadedModels {
				switch {
				case m.Unknown:
					unknownModels = append(unknownModels, m)
				case m.Starting:
					startingModels = append(startingModels, m)
				default:
					readyModels = append(readyModels, m)
				}
			}
			st.ready, st.starting, st.unknown = newModelSet(readyModels), newModelSet(startingModels), newModelSet(unknownModels)
		}()
	}
	wg.Wait()
	return states
}

// modelSet is a group of backend models in the order a candidate tries them:
// sorted base IDs, then sorted aliases.
type modelSet struct {
	bases, aliases []string
	aliasOf        map[string]string
}

func newModelSet(models []backend.ModelInfo) modelSet {
	bases, aliases, aliasOf := ranked(models)
	return modelSet{bases: bases, aliases: aliases, aliasOf: aliasOf}
}

// match returns the ID a candidate resolves to in the set, and its base model
// when that ID is an alias.
func (s modelSet) match(candidate string) (id, base string, ok bool) {
	if IsGlob(candidate) {
		if id, ok := firstMatch(candidate, s.bases); ok {
			return id, "", true
		}
		if id, ok := firstMatch(candidate, s.aliases); ok {
			return id, s.aliasOf[id], true
		}
		return "", "", false
	}
	if slices.Contains(s.bases, candidate) {
		return candidate, "", true
	}
	if base, ok := s.aliasOf[candidate]; ok {
		return candidate, base, true
	}
	return "", "", false
}

// describe is a candidate's line in Selection.Checked, e.g.
// "qwen3.8-27b:*: loaded as qwen3.8-27b:q6_k" or "m:Slacker: starting (alias of m)".
func describe(c Candidate, id, base, state string) string {
	s := c.On(c.Model) + ": " + state
	if IsGlob(c.Model) {
		s += " as " + id
	}
	if base != "" {
		s += " (alias of " + base + ")"
	}
	return s
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
