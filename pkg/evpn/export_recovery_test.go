package evpn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testVerifiedSpeaker struct {
	id  string
	err error
}

func (s *testVerifiedSpeaker) Check(context.Context) error  { return s.err }
func (s *testVerifiedSpeaker) Incarnation() (string, error) { return s.id, s.err }

func testExportRecoveryConfig(t *testing.T) ExportRecoveryConfig {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("durable exporter receipts require root; run compiled focused tests with sudo")
	}
	return ExportRecoveryConfig{
		StateDirectory: filepath.Join(t.TempDir(), "export-v1"),
		Domain: ownership.Domain{
			InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(), BootID: uuid.NewString(),
			NetworkNamespace: "test-exclusive-netns",
			ConfigDigest:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
}

func TestExportRecoveryCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"add-intent", "add-executed", "add-ack", "delete-intent", "delete-executed", "delete-ack"} {
		t.Run(boundary, func(t *testing.T) {
			cfg := testExportRecoveryConfig(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, adapter := newType2TestSpeaker(t)
			if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
				t.Fatal(err)
			}
			peer := &testVerifiedSpeaker{id: "attested-speaker-one"}
			open := func() *ExportRecovery {
				r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			r := open()
			if _, err := r.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			route := mustProviderRoute(t, sampleEndpoint())
			isDelete := boundary[:6] == "delete"
			var id string
			var err error
			if isDelete {
				id, err = r.Add(ctx, route)
				if err != nil {
					t.Fatal(err)
				}
			}
			crash := errors.New("injected process death boundary")
			r.fault = func(stage string) error {
				if stage == boundary {
					return crash
				}
				return nil
			}
			if isDelete {
				err = r.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: id})
			} else {
				_, err = r.Add(ctx, route)
			}
			if !errors.Is(err, crash) {
				t.Fatalf("did not reach boundary: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = open()
			defer r.Close()
			receipts, err := r.Recover(ctx)
			if boundary == "add-executed" {
				if !errors.Is(err, ownership.ErrBlocked) {
					t.Fatalf("retained ambiguous AddPath must block: %v", err)
				}
				paths, err := adapter.Snapshot(ctx)
				if err != nil || len(paths) != 1 {
					t.Fatalf("ambiguous route was damaged: %v %v", paths, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if boundary == "add-ack" {
				want = 1
			}
			if len(receipts) != want {
				t.Fatalf("receipts=%d want=%d", len(receipts), want)
			}
			for i := 0; i < 3; i++ {
				if _, err := r.Recover(ctx); err != nil {
					t.Fatalf("repeated recovery: %v", err)
				}
			}
			paths, err := adapter.Snapshot(ctx)
			if err != nil || len(paths) != want {
				t.Fatalf("RIB=%v err=%v", paths, err)
			}
		})
	}
}

func TestExportRecoveryIncarnationAndForeignConflicts(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, adapter := newType2TestSpeaker(t)
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	peer := &testVerifiedSpeaker{id: "first"}
	r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	route := mustProviderRoute(t, sampleEndpoint())
	id, err := r.Add(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	peer.err = errors.New("runtime inspection unavailable")
	if _, err := r.Recover(ctx); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("unavailable attestation accepted: %v", err)
	}
	peer.err = nil
	peer.id = "replacement"
	if _, err := r.Recover(ctx); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("new process retained old RIB accepted: %v", err)
	}
	peer.id = "first"
	if err := adapter.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: id}); err != nil {
		t.Fatal(err)
	}
	peer.id = "replacement"
	if receipts, err := r.Recover(ctx); err != nil || len(receipts) != 0 {
		t.Fatalf("verified incarnation plus authoritative absence: %v %v", receipts, err)
	}
	id, err = r.Add(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: id}); err != nil {
		t.Fatal(err)
	}
	foreignID, err := adapter.Add(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, route); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("foreign matching path adopted: %v", err)
	}
	if err := adapter.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: foreignID}); err != nil {
		t.Fatalf("foreign UUID not preserved: %v", err)
	}
}

func TestExportRecoveryMetadataAndConcurrency(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	peer := &testVerifiedSpeaker{id: "first"}
	_, adapter := newType2TestSpeaker(t)
	r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := OpenExportRecovery(cfg, adapter, adapter, peer); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("overlapping journal owners allowed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDirectory, "journal.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExportRecovery(cfg, adapter, adapter, peer); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("corrupt exporter metadata accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExportRecovery(cfg, adapter, adapter, peer); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("missing exporter metadata initialized silently: %v", err)
	}
}

func TestExportRecoverySIGKILL(t *testing.T) {
	for _, boundary := range []string{"add-intent", "add-executed", "add-ack", "delete-intent", "delete-executed", "delete-ack"} {
		t.Run(boundary, func(t *testing.T) {
			cfg := testExportRecoveryConfig(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, adapter, address := newType2TestSpeakerAddress(t)
			if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
				t.Fatal(err)
			}
			peer := &testVerifiedSpeaker{id: "sigkill-speaker"}
			r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExportRecoveryCrashChild$")
			child.Env = append(os.Environ(), "EVPN_EXPORT_CRASH_CONFIG="+string(data),
				"EVPN_EXPORT_CRASH_TARGET="+address, "EVPN_EXPORT_CRASH_BOUNDARY="+boundary)
			output, err := child.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("child not killed at boundary: %v %s", err, output)
			}
			r, err = OpenExportRecovery(cfg, adapter, adapter, peer)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			receipts, err := r.Recover(ctx)
			if boundary == "add-executed" {
				if !errors.Is(err, ownership.ErrBlocked) {
					t.Fatalf("SIGKILL after accepted AddPath not blocked: %v", err)
				}
				return
			}
			want := 0
			if boundary == "add-ack" {
				want = 1
			}
			if err != nil || len(receipts) != want {
				t.Fatalf("SIGKILL recovery receipts=%v err=%v", receipts, err)
			}
		})
	}
}

func TestExportRecoveryCrashChild(t *testing.T) {
	if os.Getenv("EVPN_EXPORT_CRASH_CONFIG") == "" {
		t.Skip("subprocess-only exporter crash helper")
	}
	var cfg ExportRecoveryConfig
	if err := json.Unmarshal([]byte(os.Getenv("EVPN_EXPORT_CRASH_CONFIG")), &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(os.Getenv("EVPN_EXPORT_CRASH_TARGET"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	adapter := &GoBGPType2Adapter{Client: api.NewGoBgpServiceClient(conn)}
	r, err := OpenExportRecovery(cfg, adapter, adapter, &testVerifiedSpeaker{id: "sigkill-speaker"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	boundary := os.Getenv("EVPN_EXPORT_CRASH_BOUNDARY")
	route := mustProviderRoute(t, sampleEndpoint())
	var id string
	if boundary[:6] == "delete" {
		id, err = r.Add(ctx, route)
		if err != nil {
			t.Fatal(err)
		}
	}
	r.fault = func(stage string) error {
		if stage == boundary {
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				return err
			}
			select {}
		}
		return nil
	}
	if id == "" {
		_, err = r.Add(ctx, route)
	} else {
		err = r.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: id})
	}
	t.Fatalf("missed crash boundary: %v", err)
}
