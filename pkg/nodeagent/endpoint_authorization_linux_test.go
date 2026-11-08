//go:build linux

package nodeagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"golang.org/x/sys/unix"
)

func TestLoadEndpointAuthorizationFilesystemSecurity(t *testing.T) {
	requireAuthorizationTestRoot(t)
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, path string)
	}{
		{"missing file", func(t *testing.T, _, _ string) {}},
		{"symlinked file", func(t *testing.T, dir, path string) {
			target := filepath.Join(dir, "target.json")
			writeProtectedAuthorization(t, target, 1)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked parent", func(t *testing.T, dir, path string) {
			realDir := filepath.Join(dir, "real")
			if err := os.Mkdir(realDir, 0700); err != nil {
				t.Fatal(err)
			}
			writeProtectedAuthorization(t, filepath.Join(realDir, "policy.json"), 1)
			link := filepath.Join(dir, "linked")
			if err := os.Symlink(realDir, link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := LoadEndpointAuthorization(filepath.Join(link, "policy.json")); err == nil {
					t.Error("symlinked parent was accepted")
				}
			})
		}},
		{"group writable file", func(t *testing.T, _, path string) {
			writeProtectedAuthorization(t, path, 1)
			if err := os.Chmod(path, 0620); err != nil {
				t.Fatal(err)
			}
		}},
		{"world writable file", func(t *testing.T, _, path string) {
			writeProtectedAuthorization(t, path, 1)
			if err := os.Chmod(path, 0602); err != nil {
				t.Fatal(err)
			}
		}},
		{"group writable parent", func(t *testing.T, dir, path string) {
			writeProtectedAuthorization(t, path, 1)
			if err := os.Chmod(dir, 0770); err != nil {
				t.Fatal(err)
			}
		}},
		{"world writable parent", func(t *testing.T, dir, path string) {
			writeProtectedAuthorization(t, path, 1)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatal(err)
			}
		}},
		{"incorrect owner", func(t *testing.T, _, path string) {
			writeProtectedAuthorization(t, path, 1)
			if err := os.Chown(path, 65534, 65534); err != nil {
				t.Fatal(err)
			}
		}},
		{"non-regular file", func(t *testing.T, _, path string) {
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized file", func(t *testing.T, _, path string) {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(MaxEndpointAuthorizationFileSize + 1); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"incomplete update", func(t *testing.T, _, path string) {
			if err := os.WriteFile(path, []byte(`{"version":1,"revision":`), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := newProtectedTestDir(t)
			path := filepath.Join(dir, "policy.json")
			test.setup(t, dir, path)
			if _, err := LoadEndpointAuthorization(path); err == nil {
				t.Fatal("unsafe or missing authorization file was accepted")
			}
		})
	}
}

func TestLoadEndpointAuthorizationAcceptsProtectedAtomicSnapshots(t *testing.T) {
	requireAuthorizationTestRoot(t)
	dir := newProtectedTestDir(t)
	path := filepath.Join(dir, "policy.json")
	writeProtectedAuthorization(t, path, 1)

	snapshot, err := LoadEndpointAuthorization(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision() != 1 {
		t.Fatalf("revision=%d, want 1", snapshot.Revision())
	}

	writeAtomicAuthorization(t, dir, path, 2)
	snapshot, err = LoadEndpointAuthorization(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision() != 2 {
		t.Fatalf("revision=%d, want 2", snapshot.Revision())
	}
}

func TestLoadEndpointAuthorizationRejectsHardLinks(t *testing.T) {
	requireAuthorizationTestRoot(t)
	dir := newProtectedTestDir(t)
	path := filepath.Join(dir, "policy.json")
	linkPath := filepath.Join(dir, "policy-link.json")
	writeProtectedAuthorization(t, path, 1)

	if _, err := LoadEndpointAuthorization(path); err != nil {
		t.Fatalf("load before hard link: %v", err)
	}
	if err := os.Link(path, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpointAuthorization(path); err == nil {
		t.Fatal("original path was accepted while its inode had multiple links")
	}
	if _, err := LoadEndpointAuthorization(linkPath); err == nil {
		t.Fatal("hard-linked path was accepted")
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpointAuthorization(path); err != nil {
		t.Fatalf("load after removing additional hard link: %v", err)
	}
}

func TestOpenProtectedParentRemainsPinnedAfterDirectoryReplacement(t *testing.T) {
	requireAuthorizationTestRoot(t)
	root := newProtectedTestDir(t)
	active := filepath.Join(root, "active")
	spare := filepath.Join(root, "spare")
	parked := filepath.Join(root, "parked")
	for _, dir := range []string{active, spare} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(active, "policy.json")
	writeProtectedAuthorization(t, path, 1)
	writeProtectedAuthorization(t, filepath.Join(spare, "policy.json"), 2)

	dirFD, name, err := openProtectedParent(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.Close(dirFD); err != nil {
			t.Errorf("close pinned directory descriptor: %v", err)
		}
	}()
	if err := os.Rename(active, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(spare, active); err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open policy relative to pinned directory: %v", err)
	}
	file := os.NewFile(uintptr(fd), name)
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read pinned policy: read=%v close=%v", readErr, closeErr)
	}
	snapshot, err := endpointauth.Decode(data)
	if err != nil || snapshot.Revision() != 1 {
		t.Fatalf("pinned directory did not yield its complete original policy: snapshot=%v err=%v", snapshot, err)
	}
	if _, err := LoadEndpointAuthorization(path); err == nil {
		t.Fatal("configured path followed the replacement symlink")
	}
}

func TestLoadEndpointAuthorizationRejectsUnsafePaths(t *testing.T) {
	requireAuthorizationTestRoot(t)
	for _, path := range []string{"relative/policy.json", "/root/../etc/policy.json", "/root//policy.json", "/"} {
		t.Run(path, func(t *testing.T) {
			if _, err := LoadEndpointAuthorization(path); err == nil {
				t.Fatalf("unsafe path %q was accepted", path)
			}
		})
	}
}

func TestLoadEndpointAuthorizationConcurrentAtomicReplacement(t *testing.T) {
	requireAuthorizationTestRoot(t)
	dir := newProtectedTestDir(t)
	path := filepath.Join(dir, "policy.json")
	writeProtectedAuthorization(t, path, 1)
	payloads := [][]byte{authorizationJSON(t, 1), authorizationJSON(t, 2)}

	start := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		<-start
		for i := 0; i < 100; i++ {
			tmp := filepath.Join(dir, fmt.Sprintf("next-%d.json", i))
			if err := os.WriteFile(tmp, payloads[i%2], 0600); err != nil {
				t.Errorf("write replacement: %v", err)
				return
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Errorf("replace policy: %v", err)
				return
			}
		}
	}()

	close(start)
	for i := 0; i < 100; i++ {
		snapshot, err := LoadEndpointAuthorization(path)
		if err != nil {
			t.Fatalf("load during atomic replacement: %v", err)
		}
		if snapshot.Revision() != 1 && snapshot.Revision() != 2 {
			t.Fatalf("observed partial policy revision %d", snapshot.Revision())
		}
		if snapshot.Digest() != mustAuthorizationDigest(t, snapshot.Revision()) {
			t.Fatal("loaded digest does not match a complete policy")
		}
	}
	writers.Wait()
}

func TestLoadEndpointAuthorizationConcurrentDirectoryReplacement(t *testing.T) {
	requireAuthorizationTestRoot(t)
	root := newProtectedTestDir(t)
	active := filepath.Join(root, "active")
	spare := filepath.Join(root, "spare")
	parked := filepath.Join(root, "parked")
	for _, dir := range []string{active, spare} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(active, "policy.json")
	writeProtectedAuthorization(t, path, 1)
	writeProtectedAuthorization(t, filepath.Join(spare, "policy.json"), 2)

	if err := os.Rename(active, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(spare, active); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpointAuthorization(path); err == nil {
		t.Fatal("directory path replaced by a symlink was followed")
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(spare, active); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, spare); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := LoadEndpointAuthorization(path); err != nil || snapshot.Revision() != 2 {
		t.Fatalf("replacement protected directory did not produce its complete policy: snapshot=%v err=%v", snapshot, err)
	}

	fdCountBefore, err := authorizationOpenFDCount()
	if err != nil {
		t.Fatal(err)
	}
	const swaps = 100
	start := make(chan struct{})
	writerDone := make(chan struct{})
	writerErrors := make(chan error, 1)
	go func() {
		defer close(writerDone)
		<-start
		for i := 0; i < swaps; i++ {
			if err := os.Rename(active, parked); err != nil {
				writerErrors <- fmt.Errorf("park active directory: %w", err)
				return
			}
			if err := os.Rename(spare, active); err != nil {
				writerErrors <- fmt.Errorf("install replacement directory: %w", err)
				return
			}
			if err := os.Rename(parked, spare); err != nil {
				writerErrors <- fmt.Errorf("restore spare directory: %w", err)
				return
			}
		}
	}()

	close(start)
	for i := 0; i < swaps*10; i++ {
		snapshot, err := LoadEndpointAuthorization(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected load error during directory replacement: %v", err)
			}
			continue
		}
		if snapshot.Revision() != 1 && snapshot.Revision() != 2 {
			t.Fatalf("directory replacement returned unexpected policy revision %d", snapshot.Revision())
		}
		if snapshot.Digest() != mustAuthorizationDigest(t, snapshot.Revision()) {
			t.Fatalf("directory replacement returned a mixed policy for revision %d", snapshot.Revision())
		}
	}
	<-writerDone
	select {
	case err := <-writerErrors:
		t.Fatal(err)
	default:
	}
	fdCountAfter, err := authorizationOpenFDCount()
	if err != nil {
		t.Fatal(err)
	}
	if fdCountAfter != fdCountBefore {
		t.Fatalf("directory replacement leaked file descriptors: before=%d after=%d", fdCountBefore, fdCountAfter)
	}
}

func authorizationOpenFDCount() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, fmt.Errorf("read open file descriptors: %w", err)
	}
	return len(entries), nil
}

func requireAuthorizationTestRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("protected-file security tests require root to verify ownership and permissions")
	}
}

func newProtectedTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/root", "endpointauth-test-")
	if err != nil {
		t.Skipf("root-owned test directory unavailable: %v", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func writeProtectedAuthorization(t *testing.T, path string, revision uint64) {
	t.Helper()
	if err := os.WriteFile(path, authorizationJSON(t, revision), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeAtomicAuthorization(t *testing.T, dir, path string, revision uint64) {
	t.Helper()
	tmp, err := os.CreateTemp(dir, ".policy-")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	t.Cleanup(func() { _ = os.Remove(tmpPath) })
	if err := tmp.Chmod(0600); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			t.Errorf("close temporary policy: %v", closeErr)
		}
		t.Fatal(err)
	}
	if _, err := tmp.Write(authorizationJSON(t, revision)); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			t.Errorf("close temporary policy: %v", closeErr)
		}
		t.Fatal(err)
	}
	if err := tmp.Sync(); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			t.Errorf("close temporary policy: %v", closeErr)
		}
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}
}

func authorizationJSON(t *testing.T, revision uint64) []byte {
	t.Helper()
	document := endpointauth.Document{
		Version: 1, Revision: revision, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []endpointauth.EndpointRecord{{
			NetworkID: "net-a", EndpointID: "endpoint-123", AttachmentID: "attachment-456",
			AttachmentGeneration: 4, LogicalSwitchPort: "lsp-123",
			LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000123",
			MAC:                   "02:00:00:00:00:01", IP: "192.0.2.10",
		}},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustAuthorizationDigest(t *testing.T, revision uint64) string {
	t.Helper()
	snapshot, err := endpointauth.Decode(authorizationJSON(t, revision))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Digest()
}

func TestSameFileSnapshotDetectsInPlaceChange(t *testing.T) {
	before := unix.Stat_t{Dev: 1, Ino: 2, Uid: 0, Mode: syscall.S_IFREG | 0600, Nlink: 1, Size: 10}
	after := before
	after.Mtim.Nsec++
	if sameFileSnapshot(before, after) {
		t.Fatal("in-place file change was not detected")
	}
}

func TestSameFileSnapshotAllowsAtomicUnlink(t *testing.T) {
	before := unix.Stat_t{Dev: 1, Ino: 2, Uid: 0, Mode: syscall.S_IFREG | 0600, Nlink: 1, Size: 10}
	after := before
	after.Nlink = 0
	after.Ctim.Nsec++
	if !sameFileSnapshot(before, after) {
		t.Fatal("atomic replacement unlink was treated as an in-place file change")
	}
	after.Mode = syscall.S_IFREG | 0620
	if sameFileSnapshot(before, after) {
		t.Fatal("permission change during read was accepted")
	}
	after = before
	after.Nlink = 2
	after.Ctim.Nsec++
	if sameFileSnapshot(before, after) {
		t.Fatal("hard-link creation during read was accepted")
	}
}

func TestEndpointAuthorizationErrorsAreNotEmptyPolicy(t *testing.T) {
	requireAuthorizationTestRoot(t)
	dir := newProtectedTestDir(t)
	path := filepath.Join(dir, "missing.json")
	_, err := LoadEndpointAuthorization(path)
	if err == nil || errors.Is(err, os.ErrNotExist) == false {
		t.Fatalf("missing authorization file should fail explicitly with not-exist, got %v", err)
	}
}
