package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type observedRIB struct {
	snapshot evpn.RemoteRIBSnapshot
	err      error
}

func (s observedRIB) ReadRemoteRIB(context.Context) (evpn.RemoteRIBSnapshot, error) {
	return s.snapshot, s.err
}

func TestIncompleteObservationPreservesActualInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observation")
	if err := os.WriteFile(path, []byte("incomplete\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := fixtureRIBSource{observedRIB{snapshot: evpn.RemoteRIBSnapshot{Complete: true}}, path}
	got, err := s.ReadRemoteRIB(context.Background())
	if err != nil || got.Complete {
		t.Fatalf("incomplete observation became authoritative: %+v %v", got, err)
	}
}

func TestRealReadFailureCannotBeReplacedBySeam(t *testing.T) {
	want := errors.New("incomplete actual stream")
	s := fixtureRIBSource{observedRIB{err: want}, filepath.Join(t.TempDir(), "observation")}
	if _, err := s.ReadRemoteRIB(context.Background()); !errors.Is(err, want) {
		t.Fatalf("real source failure discarded: %v", err)
	}
}

func TestInvalidOrMissingConflictInputBlocks(t *testing.T) {
	for _, mode := range []string{"invalid\n", "conflict\n"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observation")
			if err := os.WriteFile(path, []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			s := fixtureRIBSource{observedRIB{snapshot: evpn.RemoteRIBSnapshot{Complete: true}}, path}
			if _, err := s.ReadRemoteRIB(context.Background()); err == nil {
				t.Fatal("invalid/conflicting observation accepted")
			}
		})
	}
}

func TestNoFabricatedLinuxEndpointConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(path); err == nil {
		t.Fatal("empty endpoint/kernel contract accepted")
	}
}

func TestImportRootNeverInitializesExistingEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import")
	if err := openImportRoot(path); err != nil {
		t.Fatal(err)
	}
	if err := openImportRoot(path); err == nil {
		t.Fatal("existing empty importer root accepted")
	}
}

func TestImportRootRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "import")
	if err := os.Symlink(dir, path); err != nil {
		t.Fatal(err)
	}
	if err := openImportRoot(path); err == nil {
		t.Fatal("symlink importer root accepted")
	}
}
