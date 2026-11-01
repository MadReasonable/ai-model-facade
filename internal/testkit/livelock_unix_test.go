//go:build unix

package testkit

import (
	"path/filepath"
	"testing"
	"time"
)

// A second hold on the lock file waits, saying so, until the first is let
// go. Two opens of one file conflict under flock even in one process, so
// this stands in for two test processes.
func TestLockLive_WaitsForTheHolder(t *testing.T) {
	saved := liveLockFile // this process's own hold during a live run
	defer func() { liveLockFile = saved }()
	path := filepath.Join(t.TempDir(), "live.lock")
	if err := lockLive(path, t.Logf); err != nil {
		t.Fatal(err)
	}
	first := liveLockFile

	waiting := make(chan struct{})
	held := make(chan error)
	go func() {
		held <- lockLive(path, func(string, ...any) { close(waiting) })
	}()
	select {
	case <-waiting:
	case err := <-held:
		t.Fatalf("the second hold didn't wait for the first: %v", err)
	case <-time.After(Timeout):
		t.Fatalf("the second hold neither waited nor returned within %v", Timeout)
	}
	first.Close()
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(Timeout):
		t.Fatalf("the second hold still waited %v after the first was let go", Timeout)
	}
	liveLockFile.Close()
}
