// Package ownership provides durable operation evidence, not resource adoption.
// Callers must also fence their processes and verify their actual resources.
package ownership

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	journalVersion = 1
	maxJournalSize = 4 << 20
	lockName       = "owner.lock"
	journalName    = "journal.json"
)

var ErrBlocked = errors.New("RECOVERY_BLOCKED")

// Domain identifies the installation and immutable node-local ownership scope.
// ConfigDigest must cover the complete resource mapping, not just device names.
type Domain struct {
	InstallationUID  string
	NodeUID          string
	BootID           string
	NetworkNamespace string
	ConfigDigest     string
}

func (d Domain) validate() error {
	for _, value := range []string{d.InstallationUID, d.NodeUID, d.BootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil {
			return fmt.Errorf("installation, node and boot UUIDs are required")
		}
	}
	if strings.TrimSpace(d.NetworkNamespace) == "" || d.NetworkNamespace != strings.TrimSpace(d.NetworkNamespace) {
		return fmt.Errorf("network namespace identity required")
	}
	b, err := hex.DecodeString(d.ConfigDigest)
	if err != nil || len(b) != sha256.Size || strings.ToLower(d.ConfigDigest) != d.ConfigDigest {
		return fmt.Errorf("configuration SHA-256 required")
	}
	return nil
}

type Intent struct {
	Sequence  uint64
	Operation string
	Payload   json.RawMessage
}

type record struct {
	Version  int
	Domain   Domain
	Sequence uint64
	State    json.RawMessage
	Pending  *Intent
}

type envelope struct {
	Record json.RawMessage
	SHA256 string
}

// Journal holds a lock on a never-replaced inode. Begin must succeed durably
// before executing an operation; Commit durably records its verified result.
// A pending intent is deliberately not automatically resolved on Open.
type Journal struct {
	mu      sync.Mutex
	dir     *os.File
	lock    *os.File
	record  record
	blocked error
	closed  bool
	// Tests inject failures at each persistence boundary, never replace real IO.
	fault func(string) error
}

// Open initializes only a newly created directory when create is true.
// An existing directory with missing lock or journal always blocks, even when
// create is true: missing metadata must never become implicit fresh ownership.
// An existing journal is integrity-checked and must match the domain.
func Open(directory string, domain Domain, create bool) (*Journal, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("%w: ownership journals require root", ErrBlocked)
	}
	if err := domain.validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid journal domain: %v", ErrBlocked, err)
	}
	dir, created, err := openDirectory(directory, create)
	if err != nil {
		return nil, fmt.Errorf("%w: journal directory: %v", ErrBlocked, err)
	}
	j := &Journal{dir: dir}
	fail := func(err error) (*Journal, error) {
		return nil, errors.Join(err, j.Close())
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	freshLock := false
	var fd int
	if created {
		fd, err = unix.Openat(int(dir.Fd()), lockName, flags|unix.O_CREAT|unix.O_EXCL, 0600)
		freshLock = err == nil
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(int(dir.Fd()), lockName, flags, 0)
		}
	} else {
		fd, err = unix.Openat(int(dir.Fd()), lockName, flags, 0)
	}
	if err != nil {
		return fail(fmt.Errorf("%w: open journal lock: %v", ErrBlocked, err))
	}
	j.lock = os.NewFile(uintptr(fd), lockName)
	if err := secureFile(fd); err != nil {
		return fail(fmt.Errorf("%w: journal lock: %v", ErrBlocked, err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("%w: journal owner still active or lock unavailable: %v", ErrBlocked, err))
	}
	if err := j.checkLock(); err != nil {
		return fail(err)
	}
	if freshLock {
		if err := j.lock.Sync(); err != nil {
			return fail(fmt.Errorf("%w: sync initial ownership lock: %v", ErrBlocked, err))
		}
	}
	fd, err = unix.Openat(int(dir.Fd()), journalName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) && create && freshLock {
		j.record = record{Version: journalVersion, Domain: domain, State: json.RawMessage(`null`)}
		if err := j.persist(j.record); err != nil {
			return fail(err)
		}
		return j, nil
	}
	if err != nil {
		return fail(fmt.Errorf("%w: open ownership journal: %v", ErrBlocked, err))
	}
	f := os.NewFile(uintptr(fd), journalName)
	if err := secureFile(fd); err != nil {
		f.Close()
		return fail(fmt.Errorf("%w: ownership journal: %v", ErrBlocked, err))
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxJournalSize+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fail(fmt.Errorf("%w: read ownership journal: %v", ErrBlocked, err))
	}
	if len(data) > maxJournalSize {
		return fail(fmt.Errorf("%w: oversized ownership journal", ErrBlocked))
	}
	var e envelope
	if err := decodeStrict(data, &e); err != nil {
		return fail(fmt.Errorf("%w: invalid journal envelope: %v", ErrBlocked, err))
	}
	digest := sha256.Sum256(e.Record)
	if e.SHA256 != hex.EncodeToString(digest[:]) {
		return fail(fmt.Errorf("%w: ownership journal checksum mismatch", ErrBlocked))
	}
	if err := decodeStrict(e.Record, &j.record); err != nil {
		return fail(fmt.Errorf("%w: invalid ownership record: %v", ErrBlocked, err))
	}
	r := j.record
	if r.Version != journalVersion || !reflect.DeepEqual(r.Domain, domain) || len(r.State) == 0 ||
		(r.Pending != nil && (r.Pending.Sequence != r.Sequence || r.Sequence == 0 ||
			strings.TrimSpace(r.Pending.Operation) == "" || len(r.Pending.Payload) == 0)) {
		return fail(fmt.Errorf("%w: incompatible journal version, domain or operation sequence", ErrBlocked))
	}
	return j, nil
}

func decodeStrict(data []byte, target interface{}) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

// Snapshot returns independent copies; status consumers cannot mutate evidence.
// Pending is evidence of an unresolved operation, not a successful receipt.
func (j *Journal) Snapshot() (json.RawMessage, *Intent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.check(); err != nil {
		return nil, nil, err
	}
	var pending *Intent
	if j.record.Pending != nil {
		p := *j.record.Pending
		p.Payload = append(json.RawMessage(nil), p.Payload...)
		pending = &p
	}
	return append(json.RawMessage(nil), j.record.State...), pending, nil
}

func (j *Journal) Begin(operation string, payload interface{}) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.check(); err != nil {
		return 0, err
	}
	if j.record.Pending != nil {
		return 0, fmt.Errorf("%w: unresolved %s at sequence %d; operation must be verified before acknowledgement",
			ErrBlocked, j.record.Pending.Operation, j.record.Pending.Sequence)
	}
	if strings.TrimSpace(operation) == "" || operation != strings.TrimSpace(operation) {
		return 0, fmt.Errorf("%w: explicit operation required", ErrBlocked)
	}
	if j.record.Sequence == ^uint64(0) {
		return 0, fmt.Errorf("%w: operation sequence exhausted", ErrBlocked)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	next := j.record
	next.Sequence++
	next.Pending = &Intent{Sequence: next.Sequence, Operation: operation, Payload: data}
	if err := j.persist(next); err != nil {
		return 0, err
	}
	j.record = next
	return next.Sequence, nil
}

// Commit requires the exact outstanding operation. The caller must independently
// establish its result (including absence); matching attributes are not proof.
func (j *Journal) Commit(sequence uint64, state interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.check(); err != nil {
		return err
	}
	if j.record.Pending == nil || j.record.Pending.Sequence != sequence {
		return fmt.Errorf("%w: stale or missing operation acknowledgement", ErrBlocked)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	next := j.record
	next.Pending, next.State = nil, data
	if err := j.persist(next); err != nil {
		return err
	}
	j.record = next
	return nil
}

func (j *Journal) check() error {
	if j.closed {
		return fmt.Errorf("%w: journal closed", ErrBlocked)
	}
	if j.blocked != nil {
		return j.blocked
	}
	if err := j.checkLock(); err != nil {
		j.blocked = err
		return err
	}
	return nil
}

func (j *Journal) checkLock() error {
	current, _, err := openDirectory(j.dir.Name(), false)
	if err != nil {
		return fmt.Errorf("%w: ownership directory unavailable: %v", ErrBlocked, err)
	}
	var heldDir, pathDir unix.Stat_t
	statErr := errors.Join(unix.Fstat(int(j.dir.Fd()), &heldDir), unix.Fstat(int(current.Fd()), &pathDir))
	if err := errors.Join(statErr, current.Close()); err != nil {
		return fmt.Errorf("%w: stat ownership directory: %v", ErrBlocked, err)
	}
	if heldDir.Dev != pathDir.Dev || heldDir.Ino != pathDir.Ino {
		return fmt.Errorf("%w: ownership directory inode changed", ErrBlocked)
	}
	var held, path unix.Stat_t
	if err := unix.Fstat(int(j.lock.Fd()), &held); err != nil {
		return fmt.Errorf("%w: stat held lock: %v", ErrBlocked, err)
	}
	if err := unix.Fstatat(int(j.dir.Fd()), lockName, &path, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%w: ownership lock unavailable: %v", ErrBlocked, err)
	}
	if err := secureFile(int(j.lock.Fd())); err != nil {
		return fmt.Errorf("%w: unsafe held lock: %v", ErrBlocked, err)
	}
	if held.Dev != path.Dev || held.Ino != path.Ino || path.Nlink != 1 || path.Mode != held.Mode {
		return fmt.Errorf("%w: ownership lock inode changed; never unlink an active lock", ErrBlocked)
	}
	return nil
}

func secureFile(fd int) error {
	var s unix.Stat_t
	if err := unix.Fstat(fd, &s); err != nil {
		return err
	}
	if s.Mode&unix.S_IFMT != unix.S_IFREG || s.Uid != 0 || s.Mode&07777 != 0600 || s.Nlink != 1 {
		return fmt.Errorf("expected a single-link root-owned regular file mode 0600")
	}
	return nil
}

func (j *Journal) boundary(name string) error {
	if j.fault != nil {
		return j.fault(name)
	}
	return nil
}

func (j *Journal) persist(next record) (err error) {
	defer func() {
		if err != nil {
			j.blocked = fmt.Errorf("%w: durable journal write failed; mutations must stop: %v", ErrBlocked, err)
			err = j.blocked
		}
	}()
	if err = j.checkLock(); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	wrapped, err := json.Marshal(envelope{Record: data, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return err
	}
	if len(wrapped) > maxJournalSize {
		return fmt.Errorf("ownership journal exceeds size bound")
	}
	if err = j.boundary("before-create"); err != nil {
		return err
	}
	name := ".journal-" + uuid.NewString()
	fd, err := unix.Openat(int(j.dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		// Only the specific temporary file created by this call is removable.
		if unlinkErr := unix.Unlinkat(int(j.dir.Fd()), name, 0); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			err = errors.Join(err, unlinkErr)
		}
	}()
	if err = secureFile(fd); err != nil {
		return err
	}
	if _, err = f.Write(wrapped); err != nil {
		return err
	}
	if err = j.boundary("before-file-sync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err = j.boundary("before-rename"); err != nil {
		return err
	}
	if err = unix.Renameat(int(j.dir.Fd()), name, int(j.dir.Fd()), journalName); err != nil {
		return err
	}
	if err = j.boundary("before-directory-sync"); err != nil {
		return err
	}
	if err = j.dir.Sync(); err != nil {
		return err
	}
	return j.boundary("after-directory-sync")
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	var err error
	if j.lock != nil {
		err = j.lock.Close()
	}
	if j.dir != nil {
		err = errors.Join(err, j.dir.Close())
	}
	return err
}

func openDirectory(path string, create bool) (*os.File, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, false, fmt.Errorf("explicit clean absolute journal directory required")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	created := false
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create && i == len(parts)-1 {
			if err := unix.Mkdirat(fd, part, 0700); err != nil {
				unix.Close(fd)
				return nil, false, err
			}
			created = true
			if err := unix.Fsync(fd); err != nil {
				unix.Close(fd)
				return nil, false, err
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(fd)
		if openErr != nil {
			return nil, false, openErr
		}
		fd = next
		var s unix.Stat_t
		if err := unix.Fstat(fd, &s); err != nil {
			unix.Close(fd)
			return nil, false, err
		}
		final := i == len(parts)-1
		// Root-owned sticky ancestors (e.g. /tmp) cannot rename root-owned
		// children. The final state directory is always private, never sticky.
		if s.Uid != 0 || (!final && s.Mode&0022 != 0 && s.Mode&unix.S_ISVTX == 0) ||
			(final && s.Mode&07777 != 0700) {
			unix.Close(fd)
			return nil, false, fmt.Errorf("unsafe ownership/permissions on journal path component %q", part)
		}
	}
	return os.NewFile(uintptr(fd), path), created, nil
}
