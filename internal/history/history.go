// Package history keeps Responses API conversations, so that a client which
// names an earlier response in previous_response_id can be served by any
// candidate, whichever served the turns before.
//
// For each request in Responses:
//
//  1. Prepare, once selection has chosen the winner, turns the request into
//     one the winner can serve: unchanged without previous_response_id, with
//     it passed on to a service that keeps that response itself, or else with
//     the conversation up to it expanded into input and the field removed.
//  2. Record, on the winner's reply in Responses, keeps the new turn unless
//     the client set store to false: its input and output, or, where the
//     service keeps them, only which backend has them.
//
// The store lives in memory and outlives a reload, which builds new backends
// and facades around it, but not a restart. A conversation is evicted with every turn in
// it, least recently used first, once the store holds more than its byte limit,
// or once it has gone unused for its age limit.
package history

import (
	"container/list"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/pipeline"
)

// Limits bound what the store holds.
type Limits struct {
	MaxBytes int64
	MaxAge   time.Duration
}

// Store holds every conversation the gateway keeps. Its methods are safe for
// concurrent use, and a nil Store keeps nothing.
type Store struct {
	mu     sync.Mutex
	limits Limits
	turns  map[string]*turn // by response ID
	// recent orders the conversations by last use, the most recent first.
	recent *list.List
	bytes  int64
	now    func() time.Time
}

// turn is one response and how to get the conversation up to it.
type turn struct {
	parent  string // the response it followed; "" for a conversation's first
	backend string // where it was served
	// native means it was served over Responses, so a service that keeps
	// responses has it under its ID.
	native bool
	// kept means the store has its input and output. Otherwise the service at
	// backend keeps them, and the store only where they are.
	kept          bool
	input, output []byte // JSON arrays of items, when kept
	conversation  *list.Element
}

// conversation is a set of turns that descend from one first turn,
// branches included.
type conversation struct {
	ids   []string
	bytes int64
	used  time.Time
}

// turnOverhead approximates what a turn costs beyond its items: its ID, its
// links and the map entry.
const turnOverhead = 256

func NewStore(limits Limits) *Store {
	return &Store{limits: limits, turns: map[string]*turn{}, recent: list.New(), now: time.Now}
}

// SetLimits applies new limits, evicting at once what they no longer allow.
func (s *Store) SetLimits(limits Limits) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = limits
	s.evict()
}

// Turn is one request's place in its conversation, from Prepare to Record.
type Turn struct {
	store  *Store
	winner pipeline.Winner
	parent string
	input  []byte
	keep   bool // the client left store on
}

// Prepare returns the body the winner is sent, and the names of the client's
// fields left out of it: reasoning items from a backend other than the
// winner's, which only the backend that made them can read.
func (s *Store) Prepare(ctx context.Context, body []byte, winner pipeline.Winner) ([]byte, pipeline.Turn, []string, error) {
	req := gjson.ParseBytes(body)
	input := inputItems(req.Get("input"))
	t := &Turn{store: s, winner: winner, parent: req.Get("previous_response_id").String(), input: input, keep: req.Get("store").Type != gjson.False}
	if t.parent == "" {
		return body, t, nil, nil
	}
	chain, err := s.chain(t.parent)
	if err != nil {
		return nil, nil, nil, err
	}
	if first := chain[0]; winner.Service != nil && first.backend == winner.Backend && first.native && !first.kept {
		return body, t, nil, nil
	}
	items, dropped, err := expand(ctx, chain, winner)
	if err != nil {
		return nil, nil, nil, err
	}
	items = append(items, gjson.ParseBytes(input).Array()...)
	raw := make([]string, len(items))
	for i, item := range items {
		raw[i] = item.Raw
	}
	if body, err = sjson.SetRawBytes(body, "input", []byte("["+strings.Join(raw, ",")+"]")); err != nil {
		return nil, nil, nil, err
	}
	if body, err = sjson.DeleteBytes(body, "previous_response_id"); err != nil {
		return nil, nil, nil, err
	}
	return body, t, dropped, nil
}

// inputItems is a request's input as a JSON array of items: a string is one
// user message.
func inputItems(input gjson.Result) []byte {
	switch {
	case input.Type == gjson.String:
		return []byte(`[{"role":"user","content":` + input.Raw + `}]`)
	case input.IsArray():
		return []byte(input.Raw)
	}
	return []byte("[]")
}

// link is a turn of a chain, as the chain was when it was read.
type link struct {
	id string
	turn
}

// chain returns the turns from id back to the first that the store doesn't
// keep, or to the conversation's first turn, newest first.
func (s *Store) chain(id string) ([]link, error) {
	if s == nil {
		return nil, unknown(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var chain []link
	for id != "" {
		t := s.turns[id]
		if t == nil {
			if len(chain) == 0 {
				return nil, unknown(id)
			}
			return nil, fmt.Errorf("history: response '%s' is missing from its conversation", id)
		}
		chain = append(chain, link{id, *t})
		if !t.kept {
			break
		}
		id = t.parent
	}
	return chain, nil
}

func unknown(id string) error {
	return &pipeline.Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: fmt.Sprintf("Previous response with id '%s' not found.", id)}
}

// expand returns the conversation a chain leads to, oldest item first,
// reading a turn the store doesn't keep from the service that does.
func expand(ctx context.Context, chain []link, winner pipeline.Winner) ([]gjson.Result, []string, error) {
	var items []gjson.Result
	var dropped []string
	for i := len(chain) - 1; i >= 0; i-- {
		t := chain[i]
		var turnItems []gjson.Result
		if t.kept {
			turnItems = append(gjson.ParseBytes(t.input).Array(), gjson.ParseBytes(t.output).Array()...)
		} else {
			read, err := readService(ctx, t, winner)
			if err != nil {
				return nil, nil, err
			}
			turnItems = gjson.ParseBytes(read).Array()
		}
		for _, item := range turnItems {
			if item.Get("type").String() == "reasoning" && t.backend != winner.Backend {
				dropped = []string{"reasoning"}
				continue
			}
			items = append(items, item)
		}
	}
	return items, dropped, nil
}

// readService reads the conversation up to a turn from the service that keeps
// it.
func readService(ctx context.Context, t link, winner pipeline.Winner) ([]byte, error) {
	keeper, ok := winner.Backends[t.backend].(backend.ResponsesKeeper)
	if !ok {
		return nil, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("previous response '%s' is kept by backend '%s', which is no longer configured to return it", t.id, t.backend)}
	}
	items, err := keeper.ResponsesHistory(ctx, t.id, winner.Client)
	if err != nil {
		return nil, &pipeline.Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': reading the conversation up to response '%s': %v", t.backend, t.id, err)}
	}
	return items, nil
}

// Record returns the result as the client gets it, recording the turn as the
// response, or the stream's last event, goes by.
func (t *Turn) Record(result backend.ChatResult) backend.ChatResult {
	if t == nil || t.store == nil || !t.keep {
		return result
	}
	if result.Stream != nil {
		return backend.ChatResult{Stream: &recordingStream{ChatStream: result.Stream, turn: t}}
	}
	if result.Response != nil && result.Response.Status/100 == 2 {
		t.record(gjson.ParseBytes(result.Response.Body))
	}
	return result
}

// recordingStream records its turn from the event that ends a response.
type recordingStream struct {
	backend.ChatStream
	turn *Turn
}

func (s *recordingStream) Next() ([]byte, error) {
	data, err := s.ChatStream.Next()
	if err == nil {
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed", "response.incomplete":
			s.turn.record(gjson.GetBytes(data, "response"))
		}
	}
	return data, err
}

func (t *Turn) record(response gjson.Result) {
	id := response.Get("id").String()
	if id == "" {
		return
	}
	entry := &turn{parent: t.parent, backend: t.winner.Backend, native: t.winner.Native, kept: t.winner.Service == nil}
	size := int64(turnOverhead)
	if entry.kept {
		entry.input = t.input
		entry.output = []byte(response.Get("output").Raw)
		if len(entry.output) == 0 {
			entry.output = []byte("[]")
		}
		size += int64(len(entry.input) + len(entry.output))
	}
	s := t.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.turns[id]; old != nil {
		s.remove(id, old)
	}
	var element *list.Element
	if parent := s.turns[t.parent]; parent != nil {
		element = parent.conversation
		s.recent.MoveToFront(element)
	} else {
		element = s.recent.PushFront(&conversation{})
	}
	c := element.Value.(*conversation)
	c.ids = append(c.ids, id)
	c.bytes += size
	c.used = s.now()
	entry.conversation = element
	s.turns[id] = entry
	s.bytes += size
	s.evict()
}

// remove takes one turn out of its conversation, for a response ID recorded
// again.
func (s *Store) remove(id string, t *turn) {
	c := t.conversation.Value.(*conversation)
	for i, other := range c.ids {
		if other == id {
			c.ids = append(c.ids[:i], c.ids[i+1:]...)
			break
		}
	}
	size := int64(turnOverhead + len(t.input) + len(t.output))
	c.bytes -= size
	s.bytes -= size
	delete(s.turns, id)
}

// evict drops the least recently used conversations while the store holds
// more than MaxBytes, and every conversation unused for longer than MaxAge.
func (s *Store) evict() {
	for back := s.recent.Back(); back != nil; back = s.recent.Back() {
		c := back.Value.(*conversation)
		if s.bytes <= s.limits.MaxBytes && s.now().Sub(c.used) <= s.limits.MaxAge {
			return
		}
		for _, id := range c.ids {
			delete(s.turns, id)
		}
		s.bytes -= c.bytes
		s.recent.Remove(back)
	}
}
