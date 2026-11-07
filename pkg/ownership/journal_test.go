package ownership

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func testDomain() Domain {
	return Domain{
		InstallationUID:  "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID:          "46b08606-9b26-4098-90d7-02c14098122b",
		BootID:           "8a63e552-b620-445f-9ab6-8d924321ccfe",
		NetworkNamespace: "NS(4:4026531840)",
		ConfigDigest:     strings.Repeat("a", 64),
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify production root-owned journal permissions")
	}
}

func newJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	requireRoot(t)
	dir := filepath.Join(t.TempDir(), "state")
	j, err := Open(dir, testDomain(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	return j, dir
}

func TestJournalNonRootRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root caller assertion")
	}
	if j, err := Open(filepath.Join(t.TempDir(), "state"), testDomain(), true); err == nil {
		j.Close()
		t.Fatal("non-root journal accepted")
	}
}

func TestJournalIntentAcknowledgementAndRecovery(t *testing.T) {
	j, dir := newJournal(t)
	seq, err := j.Begin("netlink-add", map[string]string{"MAC": "02:00:00:00:00:01"})
	if err != nil || seq != 1 {
		t.Fatalf("intent: %d %v", seq, err)
	}
	if _, err := j.Begin("second-writer", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("pending operation overwritten")
	}
	if err := j.Commit(seq+1, nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("stale acknowledgement accepted")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(dir, testDomain(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	state, pending, err := j.Snapshot()
	if err != nil || string(state) != "null" || pending == nil ||
		pending.Sequence != seq || pending.Operation != "netlink-add" {
		t.Fatalf("unresolved operation evidence lost: %s %+v %v", state, pending, err)
	}
	pending.Payload[0] = 'x'
	_, again, err := j.Snapshot()
	if err != nil || !json.Valid(again.Payload) {
		t.Fatal("snapshot consumer mutated journal")
	}
	if _, err := j.Begin("netlink-delete", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("restart silently resolved pending mutation")
	}
	if err := j.Commit(seq, map[string]string{"receipt": "verified independently by caller"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(dir, testDomain(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	state, pending, err = j.Snapshot()
	if err != nil || pending != nil || !strings.Contains(string(state), "receipt") {
		t.Fatalf("durable acknowledgement lost: %s %+v %v", state, pending, err)
	}
	seq, err = j.Begin("next", nil)
	if err != nil || seq != 2 {
		t.Fatalf("sequence reset: %d %v", seq, err)
	}
}

func TestJournalRejectsMissingCorruptOrWrongDomain(t *testing.T) {
	for _, mode := range []string{"missing", "missing-lock", "corrupt", "checksum", "domain", "version", "permissions", "setuid", "setgid", "hardlink", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			j, dir := newJournal(t)
			j.Close()
			domain := testDomain()
			path := filepath.Join(dir, journalName)
			switch mode {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "missing-lock":
				if err := os.Remove(filepath.Join(dir, lockName)); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte(`{"truncated":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				var e envelope
				data, _ := os.ReadFile(path)
				if err := json.Unmarshal(data, &e); err != nil {
					t.Fatal(err)
				}
				e.SHA256 = strings.Repeat("0", 64)
				data, _ = json.Marshal(e)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "domain":
				domain.NodeUID = domain.InstallationUID
			case "version":
				j, err := Open(dir, domain, false)
				if err != nil {
					t.Fatal(err)
				}
				next := j.record
				next.Version++
				if err := j.persist(next); err != nil {
					t.Fatal(err)
				}
				j.Close()
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "setuid", "setgid":
				bit := uint32(unix.S_ISUID)
				if mode == "setgid" {
					bit = unix.S_ISGID
				}
				if err := unix.Chmod(path, 0600|bit); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(dir, "alias")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			}
			for _, create := range []bool{false, true} {
				reopened, err := Open(dir, domain, create)
				if reopened != nil {
					reopened.Close()
				}
				if !errors.Is(err, ErrBlocked) {
					t.Fatalf("unsafe journal accepted (create=%t): %v", create, err)
				}
			}
		})
	}
}

func TestJournalExclusiveLockAndReplacement(t *testing.T) {
	j, dir := newJournal(t)
	if other, err := Open(dir, testDomain(), false); err == nil {
		other.Close()
		t.Fatal("overlapping journal writers accepted")
	}
	// Malicious root is outside the threat model, but observable lock replacement
	// must fence the existing process instead of letting it continue silently.
	path := filepath.Join(dir, lockName)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin("must-not-execute", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("changed lock inode accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".old", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Snapshot(); !errors.Is(err, ErrBlocked) {
		t.Fatal("quarantine cleared after restoring lock")
	}
}

func TestJournalConcurrentIntents(t *testing.T) {
	j, _ := newJournal(t)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := j.Begin("concurrent", nil)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrBlocked) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly one durable intent, got %d", success)
	}
}

func TestJournalDirectoryReplacement(t *testing.T) {
	j, dir := newJournal(t)
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(dir, testDomain(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := j.Begin("must-not-execute", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("changed directory inode accepted by old writer")
	}
	if err := os.Rename(dir, dir+".replacement"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".old", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Snapshot(); !errors.Is(err, ErrBlocked) {
		t.Fatal("directory restoration cleared quarantine")
	}
}

func TestJournalPersistenceFailureFencesWriter(t *testing.T) {
	for _, operation := range []string{"intent", "acknowledgement"} {
		for _, boundary := range []string{"before-create", "before-file-sync", "before-rename", "before-directory-sync", "after-directory-sync"} {
			t.Run(operation+"/"+boundary, func(t *testing.T) {
				j, dir := newJournal(t)
				seq := uint64(0)
				if operation == "acknowledgement" {
					var err error
					seq, err = j.Begin("add", map[string]string{"uuid": "not yet known"})
					if err != nil {
						t.Fatal(err)
					}
				}
				j.fault = func(at string) error {
					if at == boundary {
						return unix.EIO
					}
					return nil
				}
				var err error
				if operation == "intent" {
					_, err = j.Begin("add", nil)
				} else {
					err = j.Commit(seq, map[string]string{"uuid": "acknowledged"})
				}
				if !errors.Is(err, ErrBlocked) {
					t.Fatal("persistence fault did not block writer")
				}
				j.fault = nil
				if _, err := j.Begin("unsafe-retry", nil); !errors.Is(err, ErrBlocked) {
					t.Fatal("failed persistence was silently retried")
				}
				j.Close()
				next, err := Open(dir, testDomain(), false)
				if err != nil {
					t.Fatalf("atomic write left corrupt evidence: %v", err)
				}
				defer next.Close()
				state, pending, err := next.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				renamed := boundary == "before-directory-sync" || boundary == "after-directory-sync"
				if operation == "intent" {
					if (pending != nil) != renamed || string(state) != "null" {
						t.Fatalf("unexpected intent crash image: %s %+v", state, pending)
					}
				} else if renamed {
					if pending != nil || !strings.Contains(string(state), "acknowledged") {
						t.Fatalf("durable ack image lost: %s %+v", state, pending)
					}
				} else if pending == nil {
					t.Fatal("unacknowledged operation forgotten")
				}
			})
		}
	}
}

func TestJournalCrashHelper(t *testing.T) {
	path := os.Getenv("EVPN_JOURNAL_CRASH_PATH")
	if path == "" {
		return
	}
	j, err := Open(path, testDomain(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	j.fault = func(boundary string) error {
		if boundary == os.Getenv("EVPN_JOURNAL_CRASH_BOUNDARY") {
			os.Exit(42)
		}
		return nil
	}
	if _, err := j.Begin("AddPath", map[string]string{"route": "pending-no-uuid"}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalAbruptProcessExit(t *testing.T) {
	for _, boundary := range []string{"before-file-sync", "before-rename", "before-directory-sync", "after-directory-sync"} {
		t.Run(boundary, func(t *testing.T) {
			j, dir := newJournal(t)
			j.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestJournalCrashHelper$")
			cmd.Env = append(os.Environ(), "EVPN_JOURNAL_CRASH_PATH="+dir, "EVPN_JOURNAL_CRASH_BOUNDARY="+boundary)
			out, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 42 {
				t.Fatalf("crash injection did not execute: %s %v", out, err)
			}
			j, err = Open(dir, testDomain(), false)
			if err != nil {
				t.Fatalf("dead owner lock not recoverable: %v", err)
			}
			defer j.Close()
			_, pending, err := j.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "before-directory-sync" || boundary == "after-directory-sync" {
				if pending == nil || pending.Operation != "AddPath" {
					t.Fatal("uncertain AddPath operation lost")
				}
				if _, err := j.Begin("retry", nil); !errors.Is(err, ErrBlocked) {
					t.Fatal("uncertain AddPath outcome automatically resolved")
				}
			} else if pending != nil {
				t.Fatal("temporary file was promoted without rename")
			}
		})
	}
}

func TestJournalRejectsUnsafePathAndDomain(t *testing.T) {
	requireRoot(t)
	base := t.TempDir()
	target := filepath.Join(base, "private")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "child"), "/", "relative", base + "/../escape"} {
		if j, err := Open(path, testDomain(), true); !errors.Is(err, ErrBlocked) {
			if j != nil {
				j.Close()
			}
			t.Fatalf("unsafe directory accepted: %s", path)
		}
	}
	for _, change := range []func(*Domain){
		func(d *Domain) { d.InstallationUID = "" },
		func(d *Domain) { d.NodeUID = d.NodeUID + "\n" },
		func(d *Domain) { d.BootID = "pid-123" },
		func(d *Domain) { d.NetworkNamespace = "" },
		func(d *Domain) { d.ConfigDigest = "short" },
	} {
		d := testDomain()
		change(&d)
		if j, err := Open(target, d, true); !errors.Is(err, ErrBlocked) {
			if j != nil {
				j.Close()
			}
			t.Fatal("invalid domain accepted")
		}
	}
	for _, mode := range []uint32{0755, 0700 | unix.S_ISVTX, 0700 | unix.S_ISGID, 0700 | unix.S_ISUID} {
		if err := unix.Chmod(target, mode); err != nil {
			t.Fatal(err)
		}
		if j, err := Open(target, testDomain(), true); !errors.Is(err, ErrBlocked) {
			if j != nil {
				j.Close()
			}
			t.Fatalf("unsafe state directory permissions accepted: %o", mode)
		}
	}
}
