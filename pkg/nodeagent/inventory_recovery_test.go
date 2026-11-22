package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

type inventoryTestSpeaker struct {
	paths       []evpn.Type2RIBPath
	adds        int
	snapshotErr error
}

func TestInventoryMixedFailuresRemainSecurityFenced(t *testing.T) {
	canceled := &inventoryFailure{Cause: "observation-canceled", Err: context.Canceled}
	integrity := &inventoryFailure{Cause: "publication-integrity", Security: true, Err: errors.New("equivocation")}
	for _, failure := range []error{
		errors.Join(canceled, integrity), errors.Join(integrity, canceled),
		errors.Join(canceled, errors.New("unclassified identity failure")),
	} {
		if _, security := inventoryFailureClass(failure); !security || inventoryObservationCanceled(failure) {
			t.Fatal("cancellation or temporary cause masked a conflicting failure")
		}
	}
}

func TestInventoryRecoveryRequestRejectsAmbiguousKeys(t *testing.T) {
	for _, raw := range []string{
		`{"ID":"first","ID":"second"}`, `{"ID":"first","\u0049D":"second"}`,
		`{"ID":"first","id":"second"}`, `{"Binding":{"Generation":1,"Generation":2}}`,
		`{"Binding":{"generation":1}}`, `{"Binding":{"NodeUID":[]}}`, `null`, `{} {}`,
	} {
		if err := validateRecoveryRequestKeys([]byte(raw)); err == nil {
			t.Fatalf("ambiguous request accepted: %s", raw)
		}
	}
	raw, err := json.Marshal(workerinventory.RecoveryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRecoveryRequestKeys(raw); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryChassisConnectionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failure := errors.New("listdbs failure with lost error identity")
	if inventoryObservationCanceled(inventoryChassisConnectionFailure(ctx, failure)) {
		t.Fatal("live-context connection failure misclassified as shutdown")
	}
	cancel()
	if err := inventoryChassisConnectionFailure(ctx, failure); !inventoryObservationCanceled(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted connection setup lost cancellation: %v", err)
	}
}

// Shutdown cancellation denies this observation, not the independent identity.
// A simultaneous real identity error must still create an administrative fence.
func TestInventoryCanceledObservationPreservesSecurityFailures(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected inventory journal requires root")
	}
	for _, security := range []bool{false, true} {
		t.Run(map[bool]string{false: "shutdown", true: "identity-fault"}[security], func(t *testing.T) {
			w, cfg, remote, cni, opts, cm := inventoryTestFixture(t)
			opts.RecoveryFile = "/run/not-read-by-this-test"
			fence, err := workerinventory.OpenFence(filepath.Join(t.TempDir(), "inventory"),
				opts.ClusterUID, opts.InstallationUID, w.NodeName, w.NodeUID)
			if err != nil {
				t.Fatal(err)
			}
			defer fence.Close()
			if err := fence.EnableTrust(opts.Namespace + "/" + inventoryConfigMap + "/" + opts.ConfigMapUID); err != nil {
				t.Fatal(err)
			}
			gate := &InventoryGate{source: fake.NewSimpleClientset(cm, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: w.NodeName, UID: types.UID(w.NodeUID)}}).CoreV1(),
				options: opts, nodeName: w.NodeName, nodeUID: w.NodeUID, fence: fence, continuity: cni, importer: remote,
				loadExport: func(string) (evpn.ExporterConfig, error) { return cfg, nil },
				loadImport: func(string) (RemoteRuntimeConfig, error) { return remote, nil },
				loadCNI:    func(string) (CNIContinuityConfig, error) { return cni, nil },
				checkVTEP:  func(evpn.ExporterConfig) error { return nil },
				chassis:    func(context.Context, string) error { return nil }}
			ctx, cancel := context.WithCancel(context.Background())
			gate.speaker = func(context.Context, workerinventory.Worker) error {
				cancel()
				if security {
					return errors.New("independent identity mismatch")
				}
				return context.Canceled
			}
			defer cancel()
			err = gate.CheckExporter(ctx, cfg)
			if err == nil {
				t.Fatal("canceled/invalid observation authorized export")
			}
			state, err := fence.Trust()
			if err != nil || state.Security != security {
				t.Fatalf("incorrect shutdown classification: %+v %v", state, err)
			}
			if !security && (state.Denial != 0 || state.Phase != workerinventory.TrustAuthorized) {
				t.Fatal("canceled observation manufactured a trust incident")
			}
			if !security {
				if err := fence.Deny("publication-integrity", true); err != nil {
					t.Fatal(err)
				}
				prior, _ := fence.Trust()
				err := gate.CheckExporter(ctx, cfg)
				if !inventoryObservationCanceled(err) {
					t.Fatalf("caller cancellation was not explicit: %v", err)
				}
				if err := gate.RecordFailure(err, nil); err != nil {
					t.Fatal(err)
				}
				after, _ := fence.Trust()
				if workerinventory.TrustDigest(prior) != workerinventory.TrustDigest(after) {
					t.Fatal("canceled observation changed an existing administrative challenge")
				}
			}
		})
	}
}

func (s *inventoryTestSpeaker) Check(context.Context) error  { return nil }
func (s *inventoryTestSpeaker) Incarnation() (string, error) { return "attested-test-peer", nil }
func (s *inventoryTestSpeaker) Snapshot(context.Context) ([]evpn.Type2RIBPath, error) {
	return s.paths, s.snapshotErr
}
func (s *inventoryTestSpeaker) Add(_ context.Context, route evpn.Type2Route) (string, error) {
	id := uuid.NewString()
	s.adds++
	s.paths = append(s.paths, evpn.Type2RIBPath{PathUUID: id, Local: true, NLRI: &route.NLRI, PathAttributes: route.PathAttributes})
	return id, nil
}
func (s *inventoryTestSpeaker) Delete(_ context.Context, route evpn.OwnedType2Route) error {
	for i, path := range s.paths {
		if path.PathUUID == route.PathUUID {
			s.paths = append(s.paths[:i], s.paths[i+1:]...)
			return nil
		}
	}
	return ownership.ErrBlocked
}

type inventoryTestEndpoints []evpn.LocalEndpoint

func (e inventoryTestEndpoints) DesiredLocalEndpoints(context.Context) ([]evpn.LocalEndpoint, error) {
	return e, nil
}

// I1/I4/I8/I10: neither journal alone grants Add, denial survives all restart
// cuts, and a committed transaction resumes only with current endpoint evidence.
func TestInventoryTwoJournalRecoveryCuts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("two protected recovery journals require root")
	}
	for _, cut := range []string{"prepared", "ready", "final-before-add", "new-denial", "ready-mismatch",
		"deleted-endpoint", "transient-rib", "invalid-rib", "canceled-rib"} {
		t.Run(cut, func(t *testing.T) {
			ctx := context.Background()
			w, cfg, remote, cni, opts, cm := inventoryTestFixture(t)
			opts.JournalDirectory = filepath.Join(t.TempDir(), "inventory")
			requestDir, err := os.MkdirTemp("/run", "evpn-recovery-cut-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(requestDir) })
			opts.RecoveryFile = filepath.Join(requestDir, "request.json")
			source := fake.NewSimpleClientset(cm, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.NodeName, UID: types.UID(w.NodeUID)}})
			peer := &inventoryTestSpeaker{}
			recoveryConfig := evpn.ExportRecoveryConfig{StateDirectory: filepath.Join(t.TempDir(), "export"),
				Domain: ownership.Domain{InstallationUID: opts.InstallationUID, NodeUID: w.NodeUID,
					BootID: uuid.NewString(), NetworkNamespace: "test-netns", ConfigDigest: strings.Repeat("a", 64)}}
			open := func() (*InventoryGate, *evpn.ExportRecovery) {
				t.Helper()
				fence, err := workerinventory.OpenFence(opts.JournalDirectory, opts.ClusterUID, opts.InstallationUID, w.NodeName, w.NodeUID)
				if err != nil {
					t.Fatal(err)
				}
				if err := fence.EnableTrust(opts.Namespace + "/" + inventoryConfigMap + "/" + opts.ConfigMapUID); err != nil {
					t.Fatal(err)
				}
				gate := &InventoryGate{source: source.CoreV1(), options: opts, nodeName: w.NodeName, nodeUID: w.NodeUID,
					continuity: cni, importer: remote, fence: fence,
					loadExport: func(string) (evpn.ExporterConfig, error) { return cfg, nil },
					loadImport: func(string) (RemoteRuntimeConfig, error) { return remote, nil },
					loadCNI:    func(string) (CNIContinuityConfig, error) { return cni, nil },
					checkVTEP:  func(evpn.ExporterConfig) error { return nil },
					chassis:    func(context.Context, string) error { return nil },
					speaker:    func(context.Context, workerinventory.Worker) error { return nil }}
				r, err := evpn.OpenExportRecovery(recoveryConfig, peer, peer, peer)
				if err != nil {
					t.Fatal(err)
				}
				return gate, r
			}
			gate, recovery := open()
			if err := gate.CheckExporter(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := recovery.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := recovery.EnableInventory(); err != nil {
				t.Fatal(err)
			}
			if err := gate.fence.Deny("publication-integrity", true); err != nil {
				t.Fatal(err)
			}
			_, target := gate.RecoveryStatus(recovery)
			if target == nil {
				t.Fatal("no scoped challenge")
			}
			request := &workerinventory.RecoveryRequest{Version: 1, ID: uuid.NewString(),
				Action: "authorize-verified-recovery", Reason: "verified test recovery", Binding: *target}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(opts.RecoveryFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			e, err := recovery.InventoryEvidence()
			if err != nil {
				t.Fatal(err)
			}
			binding, err := gate.fence.Prepare(e.Domain, e.Incarnation, e.Ownership, request)
			if err != nil {
				t.Fatal(err)
			}
			if cut != "prepared" {
				if err := recovery.InventoryReady(ctx, workerinventory.TrustDigest(binding), true); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "final-before-add" || cut == "ready-mismatch" || cut == "deleted-endpoint" {
				if err := gate.fence.Finalize(binding); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "new-denial" {
				if err := gate.fence.Deny("new-integrity-incident", true); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "ready-mismatch" {
				if err := recovery.InventoryReady(ctx, workerinventory.TrustDigest("other-transaction"), true); err != nil {
					t.Fatal(err)
				}
			}
			if err := gate.Close(); err != nil {
				t.Fatal(err)
			}
			if err := recovery.Close(); err != nil {
				t.Fatal(err)
			}
			gate, recovery = open()
			defer gate.Close()
			defer recovery.Close()
			endpoint := evpn.LocalEndpoint{NetworkID: "n24", EndpointID: "endpoint", Incarnation: "current-workload",
				MACAddress: "02:00:00:00:00:11", IPAddresses: []string{"10.251.1.11"}, VNI: 100,
				RouteDistinguisher: "192.0.2.32:100", RouteTargets: []string{"64512:100"}, LocalVTEP: w.LocalVTEP,
				NodeName: w.NodeName, Chassis: w.Chassis, Advertise: true}
			endpoints := inventoryTestEndpoints{endpoint}
			if cut == "deleted-endpoint" {
				endpoints = nil
			}
			applier := inventoryRecoveryApplier{gate: gate, recovery: recovery, observer: endpoints}
			route, err := evpn.BuildType2Route(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applier.Add(ctx, route)
			if cut == "final-before-add" {
				if err != nil || peer.adds != 1 {
					t.Fatalf("committed recovery failed fresh Add: %v adds=%d", err, peer.adds)
				}
				return
			}
			if err == nil || peer.adds != 0 {
				t.Fatalf("restart cut admitted unauthorized Add: %v adds=%d", err, peer.adds)
			}
			exporter := &evpn.EndpointExporter{Gate: peer, Continuity: peer, Source: peer, Applier: recovery, Recovery: recovery}
			if cut == "canceled-rib" {
				before, _ := gate.fence.Trust()
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				peer.snapshotErr = status.Error(codes.Canceled, "caller aborted RIB read")
				err := gate.CoordinateRecovery(canceled, exporter, recovery)
				after, stateErr := gate.fence.Trust()
				if err == nil || stateErr != nil || !inventoryObservationCanceled(err) ||
					before.Denial != after.Denial || before.Incident != after.Incident || after.Phase != before.Phase {
					t.Fatalf("aborted RIB observation mutated trust: %+v -> %+v: %v %v", before, after, err, stateErr)
				}
				return
			}
			if cut == "transient-rib" {
				peer.snapshotErr = &inventoryFailure{Cause: "attested-speaker-rpc-unavailable", Err: context.DeadlineExceeded}
			} else if cut == "invalid-rib" {
				peer.snapshotErr = errors.New("incomplete or invalid authoritative RIB")
			}
			err = gate.CoordinateRecovery(ctx, exporter, recovery)
			if cut == "transient-rib" || cut == "invalid-rib" {
				state, stateErr := gate.fence.Trust()
				// This fixture begins with an administrative security denial:
				// a later transport failure must never downgrade it.
				if err == nil || stateErr != nil || !state.Security || state.Phase != workerinventory.TrustFenced {
					t.Fatalf("failed RIB observation erased security denial: %+v %v %v", state, err, stateErr)
				}
				if cut == "transient-rib" && state.Causes[len(state.Causes)-1] != "attested-speaker-rpc-unavailable" {
					t.Fatal("typed transport cause was flattened")
				}
				return
			}
			if cut == "new-denial" || cut == "ready-mismatch" {
				if err == nil || peer.adds != 0 {
					t.Fatal("new denial or mismatched journals was repaired by health")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			state, _ := gate.fence.Trust()
			if state.Phase != workerinventory.TrustFinal || state.Generation != 1 || state.Binding.Transaction != binding.Transaction {
				t.Fatalf("crash retry lost transaction or consumed another generation: %+v", state)
			}
			_, err = applier.Add(ctx, route)
			if cut == "deleted-endpoint" {
				if err == nil || peer.adds != 0 {
					t.Fatal("deleted endpoint resurrected after grant")
				}
			} else if err != nil || peer.adds != 1 {
				t.Fatalf("verified pending transaction did not resume: %v adds=%d", err, peer.adds)
			}
		})
	}
}

// I4/I5: only the independent protected host-admin channel supplies a request.
func TestInventoryRecoveryRequestProtection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("administrator request protection requires root")
	}
	dir, err := os.MkdirTemp("/run", "evpn-recovery-request-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "request.json")
	request := workerinventory.RecoveryRequest{Version: 1, Action: "authorize-verified-recovery", Reason: "test"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(raw)
	if got, err := loadInventoryRecoveryRequest(path); err != nil || got == nil || *got != request {
		t.Fatalf("protected request unreadable: %+v %v", got, err)
	}
	for _, invalid := range []string{"null", "[]", "{}", `{"unexpected":true}`, string(raw) + "\n{}", strings.Repeat(" ", 65537)} {
		write([]byte(invalid))
		got, err := loadInventoryRecoveryRequest(path)
		// Empty fields are rejected by scoped admission, not file authentication.
		if invalid != "{}" && (err == nil || got != nil) {
			t.Fatalf("invalid request accepted: %q", invalid[:min(len(invalid), 80)])
		}
	}
	write(raw)
	for _, mode := range []os.FileMode{0644, 0660, 0400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInventoryRecoveryRequest(path); err == nil {
			t.Fatalf("mode %o accepted", mode)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInventoryRecoveryRequest(path); err == nil {
		t.Fatal("hard-linked authorization accepted")
	}
	if err := os.Remove(path + ".link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+".symlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInventoryRecoveryRequest(path + ".symlink"); err == nil {
		t.Fatal("symlink authorization accepted")
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInventoryRecoveryRequest(path); err == nil {
		t.Fatal("writable ancestor supplied authorization")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInventoryRecoveryRequest(path); err == nil {
		t.Fatal("non-administrator authorization accepted")
	}
	// Missing requests are normal while fenced. Repeated polling must close
	// the verified ancestor descriptors, including every error exit.
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 128; n++ {
		if got, err := loadInventoryRecoveryRequest(filepath.Join(dir, "missing")); got != nil || err != nil {
			t.Fatalf("missing request: %+v %v", got, err)
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil || len(after) != len(before) {
		t.Fatalf("administrator path polling leaked descriptors: before=%d after=%d err=%v", len(before), len(after), err)
	}
}
