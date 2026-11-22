package gateway

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// modelsCheckInterval is how often, while a models stream is open, the
// gateway reads every backend's state, to find what changes on a backend
// without passing through the gateway, such as a load another client
// started.
const modelsCheckInterval = 15 * time.Second

// modelsCheckGap is the least time between two checks a change through the
// gateway starts, so a busy gateway reads each backend's state at most once
// that often for the streams, however many requests start and end.
const modelsCheckGap = time.Second

// modelsWatch tells the streams of GET /amf/models?follow when to list the
// facades again. While at least one is open, one goroutine for the entire
// gateway starts a check every modelsCheckInterval, and soon after each
// change made through the gateway: a reload or an apply, or a request that
// starts or ends a backend's pending count (backend.PendingWatcher). A check
// is a state snapshot (backend.WithStateSnapshot) that every stream lists
// its caller's facades under, so each backend's state is read once per
// check, however many streams are open. Each stream sends its list only if
// it differs from the last it sent.
type modelsWatch struct {
	// interval and gap replace modelsCheckInterval and modelsCheckGap if
	// set, for a test.
	interval, gap time.Duration
	mu            sync.Mutex
	streams       int
	stop          chan struct{} // closed once the last stream leaves
	poke          chan struct{} // a change made through the gateway; nil while no stream is open
	snapshot      context.Context
	next          chan struct{} // closed when snapshot is replaced
	ended         chan struct{} // closed by EndStreams
	endOnce       sync.Once
}

// followModels starts a stream: it returns a snapshot to list the facades
// under now, and the channel closed once a newer one is published, from
// which modelsNow reads it. The first stream starts the goroutine that
// checks. Call unfollowModels when the stream ends.
func (l *Live) followModels() (context.Context, <-chan struct{}) {
	w := &l.models
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.next == nil {
		w.next = make(chan struct{})
	}
	w.streams++
	if w.streams == 1 {
		w.stop = make(chan struct{})
		w.poke = make(chan struct{}, 1)
		go l.checkModels(w.stop, w.poke, cmp.Or(w.interval, modelsCheckInterval), cmp.Or(w.gap, modelsCheckGap))
	}
	return backend.WithStateSnapshot(context.Background()), w.next
}

// unfollowModels ends a stream; the last one to end stops the goroutine.
func (l *Live) unfollowModels() {
	w := &l.models
	w.mu.Lock()
	defer w.mu.Unlock()
	w.streams--
	if w.streams == 0 {
		close(w.stop)
		w.poke = nil
	}
}

// modelsNow returns the snapshot last published, and the channel closed
// once a newer one is.
func (l *Live) modelsNow() (context.Context, <-chan struct{}) {
	w := &l.models
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshot, w.next
}

// modelsStreamsEnded is closed once EndStreams has ended every models
// stream.
func (l *Live) modelsStreamsEnded() <-chan struct{} {
	return l.models.endedChan()
}

func (w *modelsWatch) endedChan() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended == nil {
		w.ended = make(chan struct{})
	}
	return w.ended
}

func (w *modelsWatch) end() {
	ended := w.endedChan()
	w.endOnce.Do(func() { close(ended) })
}

// modelsChanged tells the models streams that what a facade routes to may
// have changed through the gateway. It never waits, so a backend's pending
// count can call it.
func (l *Live) modelsChanged() {
	w := &l.models
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case w.poke <- struct{}{}:
	default: // a check is already due, or no stream is open (nil poke)
	}
}

// checkModels publishes a new snapshot every interval, and on each poke, no
// sooner than gap after the last check, until stop is closed.
func (l *Live) checkModels(stop <-chan struct{}, poke <-chan struct{}, interval, gap time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-stop:
			return
		case <-poke:
			if wait := gap - time.Since(last); wait > 0 {
				select {
				case <-stop:
					return
				case <-time.After(wait):
				}
			}
		case <-tick.C:
		}
		last = time.Now()
		w := &l.models
		w.mu.Lock()
		w.snapshot = backend.WithStateSnapshot(context.Background())
		close(w.next)
		w.next = make(chan struct{})
		w.mu.Unlock()
	}
}
