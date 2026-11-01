//go:build unix

package testkit

import (
	"fmt"
	"os"
	"syscall"
)

// liveLockFile keeps the lock file open, and so locked, until the process
// exits; an os.File nothing refers to is closed when it is collected.
var liveLockFile *os.File

// lockLive takes an exclusive flock on path, waiting while another process
// holds it, and says so through logf. The kernel releases it when the
// process exits, however it exits, so a crashed test leaves nothing held.
func lockLive(path string, logf func(format string, args ...any)) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		logf("waiting for another test process using dev-env to exit; it holds '%s'", path)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return fmt.Errorf("locking '%s': %w", path, err)
		}
	}
	liveLockFile = f
	return nil
}
