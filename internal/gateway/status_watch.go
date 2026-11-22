package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"sync"
	"time"
)

// statusCheckInterval is how often, while a status stream is open, the
// gateway works the status out again, to catch what changes without passing
// through it: an edit to the files on the host, or a folder's permissions.
const statusCheckInterval = 2 * time.Second

// statusWatch publishes the status to the streams of GET
// /amf/config-status?follow. While at least one is open, one goroutine for
// the entire gateway works the status out after each change made through
// the gateway (Live.changed) and every statusCheckInterval, so the files
// are read once per check, however many streams are open. A change made
// through the gateway is published even if the status reads the same, since
// a reload can change what a stream's caller may do; a periodic check is
// published only if the status differs. Each stream sends what it is given
// only if its event differs from the last it sent.
type statusWatch struct {
	// interval replaces statusCheckInterval if set, for a test.
	interval time.Duration
	mu       sync.Mutex
	streams  int
	stop     chan struct{} // closed once the last stream leaves
	poke     chan struct{} // a change made through the gateway; nil while no stream is open
	latest   Status
	next     chan struct{} // closed when latest is replaced
	ended    chan struct{} // closed by EndStreams
	endOnce  sync.Once
}

// followStatus starts a stream: it returns the status now, and the channel
// closed once a newer one is published, from which statusNow reads it. The
// first stream starts the goroutine that checks the status. Call
// unfollowStatus when the stream ends.
func (l *Live) followStatus() (Status, <-chan struct{}) {
	w := &l.watch
	status := l.Status()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.next == nil {
		w.next = make(chan struct{})
	}
	w.streams++
	if w.streams == 1 {
		w.stop = make(chan struct{})
		w.poke = make(chan struct{}, 1)
		go l.checkStatus(w.stop, w.poke, cmp.Or(w.interval, statusCheckInterval))
	}
	return status, w.next
}

// unfollowStatus ends a stream; the last one to end stops the goroutine.
func (l *Live) unfollowStatus() {
	w := &l.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	w.streams--
	if w.streams == 0 {
		close(w.stop)
		w.poke = nil
	}
}

// statusNow returns the status last published, and the channel closed once
// a newer one is.
func (l *Live) statusNow() (Status, <-chan struct{}) {
	w := &l.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.latest, w.next
}

// statusStreamsEnded is closed once EndStreams has ended every status
// stream.
func (l *Live) statusStreamsEnded() <-chan struct{} {
	return l.watch.endedChan()
}

func (w *statusWatch) endedChan() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended == nil {
		w.ended = make(chan struct{})
	}
	return w.ended
}

// EndStreams ends every status and models stream, which never ends by
// itself, for a server shutting down; a stream started after it ends at
// once.
func (l *Live) EndStreams() {
	ended := l.watch.endedChan()
	l.watch.endOnce.Do(func() { close(ended) })
	l.models.end()
}

// changed tells the status streams that something the status reports may
// have changed through the gateway: the draft, the running config, or the
// files by a save. It never waits.
func (l *Live) changed() {
	w := &l.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case w.poke <- struct{}{}:
	default: // a check is already due, or no stream is open (nil poke)
	}
}

// checkStatus works the status out on each poke and every interval until
// stop is closed, and publishes it as statusWatch describes.
func (l *Live) checkStatus(stop <-chan struct{}, poke <-chan struct{}, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var last []byte
	for {
		forced := false
		select {
		case <-stop:
			return
		case <-poke:
			forced = true
		case <-tick.C:
		}
		status := l.Status()
		body, _ := json.Marshal(status)
		if !forced && bytes.Equal(body, last) {
			continue
		}
		last = body
		w := &l.watch
		w.mu.Lock()
		w.latest = status
		close(w.next)
		w.next = make(chan struct{})
		w.mu.Unlock()
	}
}
