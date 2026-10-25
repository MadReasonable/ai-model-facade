package backend

import (
	"context"
	"testing"
	"time"
)

// sinceProvider records how its state was read.
type sinceProvider struct {
	plain int
	since []time.Time
}

func (p *sinceProvider) LoadedModels(ctx context.Context) ([]ModelInfo, error) {
	p.plain++
	return nil, nil
}

func (p *sinceProvider) LoadedModelsSince(ctx context.Context, since time.Time) ([]ModelInfo, error) {
	p.since = append(p.since, since)
	return nil, nil
}

// Under a snapshot, a backend that can reuse a recent state is read once,
// with the snapshot's start; without one, it is read plainly.
func TestSnapshotReadsSinceItsStart(t *testing.T) {
	p := &sinceProvider{}
	before := time.Now()
	ctx := WithStateSnapshot(context.Background())
	after := time.Now()
	LoadedModels(ctx, p)
	LoadedModels(ctx, p)
	if p.plain != 0 || len(p.since) != 1 || p.since[0].Before(before) || p.since[0].After(after) {
		t.Errorf("under a snapshot: %d plain reads, since %v (snapshot began between %v and %v)", p.plain, p.since, before, after)
	}
	LoadedModels(context.Background(), p)
	if p.plain != 1 || len(p.since) != 1 {
		t.Errorf("without a snapshot: %d plain reads, %d since reads", p.plain, len(p.since))
	}
}
