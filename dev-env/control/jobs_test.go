package main

import (
	"context"
	"errors"
	"testing"
)

// A second start while a job for the same key runs joins it, so two
// requests to download, start or load never do the work twice; once it has
// finished, a start runs it again.
func TestJobs_ConcurrentStartsShareOneRun(t *testing.T) {
	j := newJobs(context.Background())
	release := make(chan struct{})
	runs := 0
	work := func(ctx context.Context, _ *progress) error {
		runs++
		<-release
		return errors.New("boom")
	}
	first := j.start("k", work)
	if second := j.start("k", work); second != first {
		t.Fatal("second start didn't join the running job")
	}
	close(release)
	if err := first.wait(context.Background()); err == nil || j.last("k") != first {
		t.Fatalf("got %v; last %p, want %p", err, j.last("k"), first)
	}
	if again := j.start("k", func(context.Context, *progress) error { return nil }); again == first || again.wait(context.Background()) != nil {
		t.Error("a start after the job finished didn't run it again")
	}
	if runs != 1 {
		t.Errorf("the shared job ran %d times", runs)
	}
}
