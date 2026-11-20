//go:build windows

package configfile

import "os"

// canWriteDir reports whether the gateway's user may create a file in dir,
// as a save does to rename a new file into place. Windows has no access(2)
// and decides by ACLs it doesn't model, so this creates a file there and
// removes it.
func canWriteDir(dir string) error {
	f, err := os.CreateTemp(dir, ".amf-write-check-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// canWriteFile reports whether the gateway's user may write path itself, by
// opening it for writing without writing.
func canWriteFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
