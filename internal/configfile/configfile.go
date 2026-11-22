// Package configfile reads and writes the config files a gateway was started
// with: the config, and the secrets file if it was given one. A save
// replaces each file it changes by renaming a new one over it, so a file is
// never left half-written, after copying both files into a backup in
// backups/ in the config's folder, so a failed save can put the files back
// and an earlier config can be loaded again later.
//
// A save needs to create files in each changed file's folder, and that
// folder must be on the host for the result to last, so a file mounted into
// a container by itself, which can't be renamed over, isn't saveable.
package configfile

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// rename renames a new file over the one it replaces; a var so tests can
// make one fail.
var rename = os.Rename

// Files are the config files a gateway was started with.
type Files struct {
	Config  string
	Secrets string // "" when the gateway has no secrets file
}

// Load reads and parses the files, as startup and a reload do.
func (f Files) Load() (*config.Config, error) {
	return config.Load(f.Config, f.Secrets)
}

// Dir is the config file's folder, which a relative path in it, such as a
// TLS certificate's, is from.
func (f Files) Dir() string {
	return filepath.Dir(f.Config)
}

// Read reads the files as they are on disk now, as a Document with their
// Origin, for POST /amf/config-load.
func (f Files) Read() (*config.Document, error) {
	return config.ReadFiles(f.Config, f.Secrets)
}

// The roles of the files, as the status and the backups name them.
const (
	RoleConfig  = "config"
	RoleSecrets = "secrets"
)

type file struct {
	role string
	path string
	// sum is what the file held when the gateway last read or wrote it.
	sum [sha256.Size]byte
}

func (f Files) files(saved *config.Document) []file {
	var origin config.Origin
	if saved != nil && saved.Origin != nil {
		origin = *saved.Origin
	}
	out := []file{{RoleConfig, f.Config, origin.Main}}
	if f.Secrets != "" {
		out = append(out, file{RoleSecrets, f.Secrets, origin.Secrets})
	}
	return out
}

// FileStatus is what a save would find of one file.
type FileStatus struct {
	Role string `json:"role"`
	Path string `json:"path"`
	// ChangedOnDisk is whether the file differs from what the gateway last
	// read from it or wrote to it, so a save would refuse to overwrite it.
	ChangedOnDisk bool `json:"changedOnDisk"`
	// FolderWritable is whether the gateway can create a file in the file's
	// folder, which a save needs to rename a new file into place.
	FolderWritable bool `json:"folderWritable"`
	// FileWritable is whether the gateway could open the file itself for
	// writing; a save doesn't need it, but a file it can't write is often on
	// a read-only mount.
	FileWritable bool `json:"fileWritable"`
	// MountedAlone is whether the file is a mount point of its own, as a
	// file mounted into a container by itself is; renaming over one fails.
	MountedAlone bool   `json:"mountedAlone"`
	Saveable     bool   `json:"saveable"`
	Reason       string `json:"reason,omitempty"` // why it isn't saveable
}

// BackupsStatus is what a save would find of the backups folder.
type BackupsStatus struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Reason   string `json:"reason,omitempty"`
	Keep     int    `json:"keep"`
}

// Status returns what a save would find of each file, given the files as the
// gateway last read or wrote them, and of the backups folder.
func (f Files) Status(saved *config.Document, keep int) ([]FileStatus, BackupsStatus) {
	backups := f.backupsStatus(keep)
	var out []FileStatus
	for _, file := range f.files(saved) {
		out = append(out, file.status(backups))
	}
	return out, backups
}

// status is what a save would find of file. A file is saveable only if the
// backups folder can be written too, since a save backs up both files before
// it replaces either.
func (file file) status(backups BackupsStatus) FileStatus {
	s := FileStatus{Role: file.role, Path: file.path}
	if data, err := os.ReadFile(file.path); err == nil {
		s.ChangedOnDisk = sha256.Sum256(data) != file.sum
	} else {
		s.ChangedOnDisk = true
	}
	dir := filepath.Dir(file.path)
	folderErr := canWriteDir(dir)
	s.FolderWritable = folderErr == nil
	s.FileWritable = canWriteFile(file.path) == nil
	s.MountedAlone = isMountPoint(file.path)
	switch {
	case s.MountedAlone:
		s.Reason = fmt.Sprintf("'%s' is mounted by itself, and a file can't be renamed over a mount point; mount its folder instead", file.path)
	case folderErr != nil:
		s.Reason = fmt.Sprintf("can't create a file in '%s': %v", dir, unwrapPath(folderErr))
	case !backups.Writable:
		s.Reason = "a save backs both files up first, and the backups folder can't be written: " + backups.Reason
	default:
		s.Saveable = true
	}
	return s
}

func (f Files) backupsDir() string {
	return filepath.Join(filepath.Dir(f.Config), "backups")
}

func (f Files) backupsStatus(keep int) BackupsStatus {
	s := BackupsStatus{Path: f.backupsDir(), Keep: keep}
	dir := s.Path
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		dir = filepath.Dir(dir) // created by the first save
	}
	if err := canWriteDir(dir); err != nil {
		s.Reason = fmt.Sprintf("can't create a file in '%s': %v", dir, unwrapPath(err))
		return s
	}
	s.Writable = true
	return s
}

func unwrapPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// isMountPoint reports whether path is a mount point of its own, from
// /proc/self/mountinfo; false where that can't be read, as off Linux.
func isMountPoint(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == abs {
			return true
		}
	}
	return false
}

// unescapeMount undoes the octal escapes mountinfo writes for spaces, tabs,
// newlines and backslashes in a path.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// SaveError is a save refused before it changed anything (Conflict), or one
// that failed while writing and put the files back (Conflict false).
type SaveError struct {
	Conflict bool
	Err      error
}

func (e *SaveError) Error() string { return e.Err.Error() }
func (e *SaveError) Unwrap() error { return e.Err }

// Saved is the outcome of a save.
type Saved struct {
	// Document is running as now saved, with the Origin of what was
	// written: what the files hold now.
	Document *config.Document
	Written  []string // the roles of the files written, none if nothing differed
	Backup   string   // the ID of the backup made, "" if none is kept
	Notes    []string
}

// Save writes running to the files, given the files as the gateway last
// read or wrote them in saved. Only the files whose content differs are
// written. It refuses, before anything changes, a file changed on disk since
// saved unless force is set, and a file it can't save. It copies both files
// into a backup first, and if a rename fails it puts back every file
// already replaced from there. After a save, the oldest
// backups past keep are removed; with keep 0, the save's own too.
func (f Files) Save(running, saved *config.Document, force bool, keep int) (*Saved, error) {
	main, secrets, err := running.Encode()
	if err != nil {
		return nil, err
	}
	savedMain, savedSecrets, err := saved.Encode()
	if err != nil {
		return nil, err
	}
	content := map[string][]byte{RoleConfig: main, RoleSecrets: secrets}
	differs := map[string]bool{RoleConfig: !bytes.Equal(main, savedMain), RoleSecrets: !bytes.Equal(secrets, savedSecrets)}
	backups := f.backupsStatus(keep)
	var changed []file
	for _, file := range f.files(saved) {
		if differs[file.role] {
			changed = append(changed, file)
		}
	}
	result := &Saved{Document: running.Copy(), Written: []string{}}
	result.Document.Origin = saved.Origin
	if len(changed) == 0 {
		return result, nil
	}
	for _, file := range changed {
		s := file.status(backups)
		if s.ChangedOnDisk && !force {
			return nil, &SaveError{true, fmt.Errorf("'%s' changed on disk since the gateway read it; reload to take the change, load it into the draft with POST /amf/config-load to review it, or save with ?force to overwrite it", file.path)}
		}
		if !s.Saveable {
			return nil, &SaveError{true, fmt.Errorf("can't save '%s': %s", file.path, s.Reason)}
		}
	}
	id, err := f.backup()
	if err != nil {
		return nil, &SaveError{true, fmt.Errorf("can't back up the files, so nothing was saved: %w", err)}
	}
	var replaced []file
	for _, file := range changed {
		if err := replace(file.path, content[file.role], file.role == RoleSecrets); err != nil {
			for _, done := range replaced {
				if backupData, readErr := os.ReadFile(f.backupPath(done.role, id)); readErr == nil {
					replace(done.path, backupData, done.role == RoleSecrets)
				}
			}
			return nil, &SaveError{false, fmt.Errorf("saving '%s' failed, so the files were put back as they were: %w", file.path, unwrapPath(err))}
		}
		replaced = append(replaced, file)
		result.Written = append(result.Written, file.role)
	}
	// A file not written keeps the checksum of what the gateway read from
	// it, since that is what it still holds; the re-encoded content would
	// differ from a file laid out by hand, and make it look changed on disk.
	var origin config.Origin
	if saved.Origin != nil {
		origin = *saved.Origin
	}
	for _, role := range result.Written {
		if role == RoleConfig {
			origin.Main = sha256.Sum256(main)
		} else {
			origin.Secrets = sha256.Sum256(secrets)
		}
	}
	result.Document.Origin = &origin
	result.Backup = id
	if err := f.prune(keep); err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("removing old backups failed: %v", err))
	}
	if keep == 0 {
		result.Backup = ""
	}
	return result, nil
}

// replace writes data to a new file beside path, with the mode of the file
// it replaces, and renames it over path.
func replace(path string, data []byte, secret bool) error {
	mode := fs.FileMode(0o644)
	if secret {
		mode = 0o600
	}
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing to remove once it is renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}

// A backup is a folder in backups/ named by its ID: the time of the save
// that made it, with a count after it for a second save in the same second.
// It holds both files as they were just before that save, whichever the save
// changed, so each backup is a whole config by itself, and each file is named
// by its role, so renaming the files doesn't hide their backups.
var backupID = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)

// unfinished starts the name of a backup's folder until it holds both
// files; the listing never sees one.
const unfinished = ".backup-"

func (f Files) backupPath(role, id string) string {
	return filepath.Join(f.backupsDir(), id, role+".yaml")
}

// backup copies the files into a new backup. It writes them into a folder of
// its own and renames that to the backup's ID once both are in, so a backup
// is whole or absent; one a crash left unfinished is removed first. The
// folders are mode 0700 and a secrets file's copy 0600, so a backup is no
// more readable than the files it copies.
func (f Files) backup() (string, error) {
	dir := f.backupsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), unfinished) {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	tmp, err := os.MkdirTemp(dir, unfinished+"*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // nothing to remove once it is renamed
	for _, file := range f.files(nil) {
		data, err := os.ReadFile(file.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // nothing to back up
		}
		if err != nil {
			return "", err
		}
		mode := fs.FileMode(0o644)
		if info, err := os.Stat(file.path); err == nil {
			mode = info.Mode().Perm()
		}
		if file.role == RoleSecrets {
			mode &= 0o600
		}
		if err := os.WriteFile(filepath.Join(tmp, file.role+".yaml"), data, mode); err != nil {
			return "", err
		}
	}
	stamp := time.Now().Format("20060102-150405")
	id := stamp
	for n := 2; ; n++ {
		if _, err := os.Lstat(filepath.Join(dir, id)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		id = fmt.Sprintf("%s-%d", stamp, n)
	}
	if err := os.Rename(tmp, filepath.Join(dir, id)); err != nil {
		return "", err
	}
	return id, nil
}

// Backup is one save's copy of the files, as they were before it.
type Backup struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Files []string  `json:"files"` // the roles of the files it holds
}

// Backups returns the backups in the backups folder, newest first; none if
// the folder doesn't exist.
func (f Files) Backups() ([]Backup, error) {
	entries, err := os.ReadDir(f.backupsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, e := range entries {
		if !e.IsDir() || !backupID.MatchString(e.Name()) {
			continue // an unfinished backup, or not one
		}
		t, _ := time.ParseInLocation("20060102-150405", e.Name()[:15], time.Local)
		b := Backup{ID: e.Name(), Time: t, Files: []string{}}
		for _, role := range []string{RoleConfig, RoleSecrets} {
			if _, err := os.Stat(f.backupPath(role, b.ID)); err == nil {
				b.Files = append(b.Files, role)
			}
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Backup) int { return -compareIDs(a.ID, b.ID) })
	return out, nil
}

// compareIDs orders IDs by time, then by the count after it.
func compareIDs(a, b string) int {
	if c := strings.Compare(a[:15], b[:15]); c != 0 {
		return c
	}
	count := func(id string) int {
		var n int
		fmt.Sscanf(strings.TrimPrefix(id[15:], "-"), "%d", &n)
		return n
	}
	return count(a) - count(b)
}

// ErrNoBackup is ReadBackup's error for a backup that doesn't exist.
var ErrNoBackup = errors.New("no such backup")

// backupIndex is a backup named by its place from the newest: -1 for the
// newest, -2 for the one before it.
var backupIndex = regexp.MustCompile(`^-[1-9][0-9]*$`)

// ReadBackup returns the ID of the backup ref names, and its files as a
// Document. ref is an ID, or an index counted
// back from the newest, -1 for the newest, so a client can go back one save
// without listing the backups first. The Document is as the backup holds
// it: with a secrets file or without, whatever the gateway has now.
func (f Files) ReadBackup(ref string) (string, *config.Document, error) {
	id, main, secrets, err := f.readBackup(ref)
	if err != nil {
		return "", nil, err
	}
	d, err := config.ReadDocument(main, secrets)
	if err != nil {
		return "", nil, fmt.Errorf("backup '%s': %w", id, err)
	}
	return id, d, nil
}

// readBackup returns the ID of the backup ref names, and the content of its
// files: the config, and the secrets file, nil if the gateway had none.
func (f Files) readBackup(ref string) (id string, main, secrets []byte, err error) {
	id = ref
	if backupIndex.MatchString(ref) {
		backups, err := f.Backups()
		if err != nil {
			return "", nil, nil, err
		}
		var n int
		fmt.Sscanf(ref[1:], "%d", &n)
		if n > len(backups) {
			return "", nil, nil, fmt.Errorf("'%s': %w; there are %d", ref, ErrNoBackup, len(backups))
		}
		id = backups[n-1].ID
	}
	if !backupID.MatchString(id) {
		return "", nil, nil, fmt.Errorf("'%s': %w", ref, ErrNoBackup)
	}
	if info, err := os.Stat(filepath.Join(f.backupsDir(), id)); err != nil || !info.IsDir() {
		return "", nil, nil, fmt.Errorf("'%s': %w", ref, ErrNoBackup)
	}
	for _, role := range []string{RoleConfig, RoleSecrets} {
		data, err := os.ReadFile(f.backupPath(role, id))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, nil, err
		}
		if role == RoleConfig {
			main = data
		} else {
			secrets = data
		}
	}
	if main == nil {
		return "", nil, nil, fmt.Errorf("backup '%s' holds no config", id)
	}
	return id, main, secrets, nil
}

// prune removes the oldest backups past keep.
func (f Files) prune(keep int) error {
	backups, err := f.Backups()
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range backups[min(keep, len(backups)):] {
		if err := os.RemoveAll(filepath.Join(f.backupsDir(), b.ID)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
