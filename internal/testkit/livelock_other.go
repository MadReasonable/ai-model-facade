//go:build !unix

package testkit

// lockLive does nothing where there is no flock, so live checks of several
// packages run at once there; run each package's on its own.
func lockLive(path string, logf func(format string, args ...any)) error {
	return nil
}
