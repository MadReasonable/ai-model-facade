//go:build unix

package configfile

import "syscall"

// writeOK is access(2)'s W_OK, the same on every Unix.
const writeOK = 0x2

// canWriteDir reports whether the gateway's user may create a file in dir,
// as a save does to rename a new file into place, by asking access(2): it
// answers from the folder's permissions and ACLs, and with EROFS for a
// read-only mount, and changes nothing on disk, so a status read leaves the
// folder as it was. A security module, such as AppArmor, can still refuse a
// write it allows; the save then fails at its first write, before it replaces
// anything.
func canWriteDir(dir string) error {
	return syscall.Access(dir, writeOK)
}

// canWriteFile reports whether the gateway's user may write path itself.
func canWriteFile(path string) error {
	return syscall.Access(path, writeOK)
}
