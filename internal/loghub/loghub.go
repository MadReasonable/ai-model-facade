// Package loghub keeps the gateway's own copy of its log, which GET /amf/logs
// reads and streams. The Hub is an slog.Handler beside the one that writes
// stderr, and first in line, so a record is in memory before a stderr write
// that stalls can hold it up, and its level may be below stderr's. It holds:
//
//  1. a ring of the newest records, up to server.adminApi.logs.maxBytes, each kept as
//     the JSON line and the text line slog's own handlers write for it; the
//     newest is kept even past the limit, so the last line logged can be read
//     when nothing else can;
//  2. a bounded queue per open stream, which a record that finds full is
//     dropped from, counted, so a slow reader never holds up logging.
//
// With a ring of 0 the Hub keeps no history and takes no record while no
// stream is open. The ring is only a window onto the running process: stderr
// stays the durable log, and a restart starts the ring empty.
package loghub

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Entry is one logged record.
type Entry struct {
	Time  time.Time
	Level slog.Level
	JSON  []byte // the record as slog's JSON handler writes it, newline included
	Text  []byte // the same in the text format stderr uses
}

func (e Entry) size() int64 {
	return int64(len(e.JSON) + len(e.Text) + 64) // 64 for the Entry itself
}

// off is the level of a Hub that takes nothing: above every record's.
const off = slog.Level(math.MaxInt32)

// Hub is the gateway's copy of its log. Its zero value isn't usable; use New.
type Hub struct {
	// takes is the lowest level the Hub takes a record at, or off: its
	// level, or with no ring the lowest level an open stream wants, if
	// that is higher. Enabled reads it without the lock, on every log call.
	takes atomic.Int64
	mu    sync.Mutex // guards the rest, and the scratch buffers while a record is formatted
	// json and text are where the formatting handlers write a record, so
	// Handle can keep both forms of it.
	json, text bytes.Buffer
	level      slog.Level // the lowest level kept, or off
	maxBytes   int64
	entries    []Entry // oldest first
	size       int64
	streams    map[*Stream]bool
}

// New returns a Hub that takes nothing until Configure turns it on.
func New() *Hub {
	h := &Hub{level: off, streams: map[*Stream]bool{}}
	h.takes.Store(int64(off))
	return h
}

// Configure applies cfg: the Hub keeps records from server.adminApi.logs.level, or
// logging.level, up to server.adminApi.logs.maxBytes, if server.adminApi lets anyone
// read them. Otherwise it takes nothing, drops what it holds and ends every
// stream, so a log no one may read costs nothing.
func (h *Hub) Configure(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !cfg.Server.AdminAPI.ReadOn() {
		h.level = off
		h.entries, h.size = nil, 0
		h.endStreams()
		h.updateTakes()
		return
	}
	stderr, err := cfg.Logging.SlogLevel()
	if err != nil {
		stderr = slog.LevelInfo // checked at load, so a config that gets here has a valid one
	}
	h.level = cfg.Server.AdminAPI.Logs.SlogLevel(stderr)
	h.maxBytes = cfg.Server.AdminAPI.Logs.KeepBytes()
	if h.maxBytes == 0 {
		h.entries, h.size = nil, 0
	}
	h.trim()
	h.updateTakes()
}

// updateTakes sets takes from the level, the ring and the open streams. The
// caller holds h.mu.
func (h *Hub) updateTakes() {
	takes := h.level
	if h.maxBytes == 0 && takes != off {
		floor := off
		for s := range h.streams {
			floor = min(floor, s.level)
		}
		takes = max(takes, floor)
	}
	h.takes.Store(int64(takes))
}

// KeepsHistory reports whether the Hub keeps a ring, rather than only
// streaming.
func (h *Hub) KeepsHistory() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxBytes > 0
}

// Handler returns the slog.Handler that feeds the Hub.
func (h *Hub) Handler() slog.Handler {
	all := &slog.HandlerOptions{Level: slog.Level(math.MinInt32)} // the Hub decides what it takes
	return &handler{
		hub:  h,
		json: slog.NewJSONHandler(scratch{&h.json}, all),
		text: slog.NewTextHandler(scratch{&h.text}, all),
	}
}

// scratch is where a formatting handler writes, into one of the Hub's
// buffers, which Handle reads back under the Hub's lock.
type scratch struct{ buf *bytes.Buffer }

func (s scratch) Write(p []byte) (int, error) { return s.buf.Write(p) }

// handler is a Hub's slog.Handler. It formats each record with slog's own
// JSON and text handlers, so attributes and groups added with With and
// WithGroup come out as stderr shows them.
type handler struct {
	hub        *Hub
	json, text slog.Handler
}

func (x *handler) Enabled(_ context.Context, level slog.Level) bool {
	return int64(level) >= x.hub.takes.Load()
}

func (x *handler) Handle(ctx context.Context, r slog.Record) error {
	h := x.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if int64(r.Level) < h.takes.Load() {
		return nil // turned off, or its last stream stopped, since Enabled
	}
	h.json.Reset()
	h.text.Reset()
	if err := x.json.Handle(ctx, r); err != nil {
		return err
	}
	if err := x.text.Handle(ctx, r); err != nil {
		return err
	}
	e := Entry{Time: r.Time, Level: r.Level, JSON: bytes.Clone(h.json.Bytes()), Text: bytes.Clone(h.text.Bytes())}
	if h.maxBytes > 0 {
		h.entries = append(h.entries, e)
		h.size += e.size()
		h.trim()
	}
	for s := range h.streams {
		s.offer(e)
	}
	return nil
}

func (x *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{hub: x.hub, json: x.json.WithAttrs(attrs), text: x.text.WithAttrs(attrs)}
}

func (x *handler) WithGroup(name string) slog.Handler {
	return &handler{hub: x.hub, json: x.json.WithGroup(name), text: x.text.WithGroup(name)}
}

// trim drops the oldest entries past maxBytes, keeping the newest even if it
// alone is larger. The caller holds h.mu.
func (h *Hub) trim() {
	drop := 0
	for h.size > h.maxBytes && drop < len(h.entries)-1 {
		h.size -= h.entries[drop].size()
		drop++
	}
	if drop > 0 {
		h.entries = clipFront(h.entries[drop:])
	}
}

// clipFront copies a slice that lost its front once the backing array is
// mostly dropped entries, so their memory is freed rather than held.
func clipFront(entries []Entry) []Entry {
	if cap(entries) <= 2*len(entries) {
		return entries
	}
	return append([]Entry(nil), entries...)
}

// Filter selects entries: at Level or above, logged at Since or later, and,
// with Limit above 0, only the newest Limit of those.
type Filter struct {
	Level slog.Level
	Since time.Time
	Limit int
}

// selectEntries returns the entries f selects, oldest first. The caller
// holds h.mu.
func (h *Hub) selectEntries(f Filter) []Entry {
	var out []Entry
	for _, e := range h.entries {
		if e.Level >= f.Level && !e.Time.Before(f.Since) {
			out = append(out, e)
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out
}

// Read returns the entries f selects, oldest first.
func (h *Hub) Read(f Filter) []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.selectEntries(f)
}

// Stream is one reader following the log: the records logged after it
// started, at its level or above.
type Stream struct {
	level slog.Level
	queue chan Item
	ended chan struct{}
	// dropped counts the records that found the queue full since the last
	// one that went in; guarded by the Hub's lock.
	dropped int
}

// Item is a record for a stream, with the number of records dropped from the
// stream just before it.
type Item struct {
	Entry
	Dropped int
}

// queueSize is how many records a stream holds for a reader that hasn't
// taken them yet.
const queueSize = 256

// Follow starts a stream of the records at f's level or above, and returns
// with it the entries f selects from the ring, as one step, so no record is
// both in the backlog and on the stream, or in neither. A Hub that is off
// returns a stream already ended.
func (h *Hub) Follow(f Filter) ([]Entry, *Stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &Stream{level: f.Level, queue: make(chan Item, queueSize), ended: make(chan struct{})}
	if h.level == off {
		close(s.ended)
		return nil, s
	}
	h.streams[s] = true
	h.updateTakes()
	return h.selectEntries(f), s
}

// offer queues e for the stream, or counts it dropped if the queue is full.
// The caller holds the Hub's lock.
func (s *Stream) offer(e Entry) {
	if e.Level < s.level {
		return
	}
	select {
	case s.queue <- Item{Entry: e, Dropped: s.dropped}:
		s.dropped = 0
	default:
		s.dropped++
	}
}

// Items are the stream's records, in order.
func (s *Stream) Items() <-chan Item { return s.queue }

// Ended is closed when the Hub ends the stream: when Configure turns the Hub
// off, or on EndStreams.
func (s *Stream) Ended() <-chan struct{} { return s.ended }

// Stop removes s, for a reader that is done with it.
func (h *Hub) Stop(s *Stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, s)
	h.updateTakes()
}

// EndStreams ends every stream, as the server shuts down, so no stream holds
// shutdown open.
func (h *Hub) EndStreams() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endStreams()
	h.updateTakes()
}

func (h *Hub) endStreams() {
	for s := range h.streams {
		close(s.ended)
		delete(h.streams, s)
	}
}
