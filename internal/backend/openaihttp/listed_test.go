package openaihttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// newListed returns a ListedState with maxAge on a backend listing "a",
// which fails while *fail is set, and counts its listings in *calls.
func newListed(maxAge time.Duration, fail *bool, calls *int) *ListedState {
	return &ListedState{maxAge: maxAge, list: func(context.Context) ([]backend.ModelInfo, error) {
		*calls++
		if *fail {
			return nil, errors.New("down")
		}
		return []backend.ModelInfo{{ID: "a"}}, nil
	}}
}

// A ListedState's state is its latest list: a state read lists the backend
// only while there is no list, and never because of age without a maxAge.
func TestListedState_AnswersFromTheLatestList(t *testing.T) {
	fail, calls := false, 0
	l := newListed(0, &fail, &calls)
	for range 2 {
		if got, err := l.State(context.Background()); err != nil || len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	l.latest.Load().at = time.Now().Add(-time.Hour)
	if _, err := l.State(context.Background()); err != nil || calls != 1 {
		t.Errorf("listed %d times for three state reads, want 1; %v", calls, err)
	}
}

// A list older than maxAge is read again, and a dropped one at once, so a
// backend that has gone away stops reading as ready; while its listing
// fails it reads as unavailable, and it comes back with the next listing
// that succeeds.
func TestListedState_ReadsAgainWhenOldOrDropped(t *testing.T) {
	fail, calls := false, 0
	l := newListed(time.Minute, &fail, &calls)
	if _, err := l.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.latest.Load().at = time.Now().Add(-2 * time.Minute)
	fail = true
	if _, err := l.State(context.Background()); err == nil {
		t.Error("an old list of a backend that's down: want an error")
	}
	if _, err := l.State(context.Background()); err == nil {
		t.Error("still down: want an error")
	}
	fail = false
	if got, err := l.State(context.Background()); err != nil || len(got) != 1 {
		t.Errorf("back up: %+v, %v", got, err)
	}
	l.Drop()
	l.State(context.Background())
	if calls != 5 {
		t.Errorf("listed %d times, want 5", calls)
	}
}
