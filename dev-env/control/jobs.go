package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// Step states, as the API reports them.
const (
	stateDone    = "done"
	statePending = "pending"
	stateRunning = "running"
	stateFailed  = "failed"
)

// job is one run of a long action, such as a download or a start. It runs
// detached from the request that began it, so a client that leaves doesn't
// stop it, and other requests can watch it.
type job struct {
	done     chan struct{} // closed when err is set
	err      error
	progress progress
}

// progress counts bytes for a job that has a size, such as a download.
type progress struct {
	done, total atomic.Int64
	mu          sync.Mutex
	detail      string // what the work is doing now, for people
}

func (p *progress) setDetail(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.detail = detail
}

func (p *progress) getDetail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.detail
}

// jobs runs at most one job per key at a time: a request for a key whose
// job is running joins it rather than starting a second. The last finished
// job per key is kept, so a failure stays visible until the next run.
type jobs struct {
	ctx  context.Context // ends when the service stops
	mu   sync.Mutex
	runs map[string]*job
}

func newJobs(ctx context.Context) *jobs {
	return &jobs{ctx: ctx, runs: map[string]*job{}}
}

// start runs fn under key unless a job for key is already running, and
// returns that job either way.
func (j *jobs) start(key string, fn func(ctx context.Context, p *progress) error) *job {
	j.mu.Lock()
	defer j.mu.Unlock()
	if run := j.runs[key]; run != nil && !run.finished() {
		return run
	}
	run := &job{done: make(chan struct{})}
	j.runs[key] = run
	go func() {
		run.err = fn(j.ctx, &run.progress)
		close(run.done)
	}()
	return run
}

// last returns the running or most recent job for key, or nil.
func (j *jobs) last(key string) *job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.runs[key]
}

func (run *job) finished() bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

// wait returns run's error once it finishes, or ctx's if ctx ends first.
func (run *job) wait(ctx context.Context) error {
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
