package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netns"
)

func lifecycleFixture(t *testing.T) (config, ownership.Domain, *lifecycle) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("run the root-owned offline test binary as root for real ownership journals")
	}
	c := config{State: t.TempDir(), InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(),
		Endpoint: evpn.LocalEndpoint{NetworkID: "ext2", EndpointID: uuid.NewString(), Incarnation: "a" + uuid.NewString()[1:],
			MACAddress: "02:00:00:02:00:66", IPAddresses: []string{"10.100.2.101"}, VNI: 2000,
			RouteDistinguisher: "198.18.20.3:2000", RouteTargets: []string{"64513:2000"},
			LocalVTEP: "198.18.20.4", Advertise: true},
		Import: evpn.RemoteImportConfig{LocalVTEP: "198.18.20.4", Networks: []evpn.RemoteImportNetwork{{
			NetworkID: "ext2", VNI: 2000, ImportRouteTargets: []string{"64513:2000"}, AllowedVTEPs: []string{"198.18.20.2"}}}}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	d := ownership.Domain{InstallationUID: c.InstallationUID, NodeUID: c.NodeUID,
		BootID: uuid.NewString(), NetworkNamespace: "offline-fixture-netns", ConfigDigest: hex.EncodeToString(sum[:])}
	l, err := openLifecycle(c, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.journal.Close() })
	return c, d, l
}

func requireError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("want error containing %q, got %v", contains, err)
	}
}

func publish(t *testing.T, path string, value any) {
	t.Helper()
	if err := write(path, value); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleRetireRecreateAndIndependentImporter(t *testing.T) {
	c, d, l := lifecycleFixture(t)
	importer, err := ownership.Open(filepath.Join(c.State, "independent-import"), d, true)
	if err != nil {
		t.Fatal(err)
	}
	defer importer.Close()
	sentinel := map[string]string{"owned-fdb": "acknowledged", "foreign-fdb": "quarantined", "foreign-neighbor": "untouched"}
	seq, err := importer.Begin("offline-import-checkpoint", sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if err := importer.Commit(seq, sentinel); err != nil {
		t.Fatal(err)
	}
	before, _, _ := importer.Snapshot()
	observations := 0
	observe := func(_ context.Context, next config) ([]evpn.LocalEndpoint, error) {
		observations++
		return []evpn.LocalEndpoint{next.Endpoint}, nil
	}
	ctx := context.Background()
	if got, err := l.desired(ctx, c, observe); err != nil || len(got) != 1 {
		t.Fatalf("initial endpoint: %v %v", got, err)
	}
	// Active observation failures cannot become deletion authority.
	_, err = l.desired(ctx, c, func(context.Context, config) ([]evpn.LocalEndpoint, error) {
		return nil, errors.New("missing netns/device")
	})
	requireError(t, err, "missing netns")
	publish(t, filepath.Join(c.State, "endpoint.json"), endpointRequest{1, c.Endpoint.Incarnation, true})
	if got, err := l.desired(ctx, c, observe); err != nil || len(got) != 0 || observations != 1 || l.state.Phase != "retiring" {
		t.Fatalf("explicit retirement depended on endpoint observation: %v %v %+v", got, err, l.state)
	}
	requireError(t, l.accept(endpointRequest{2, uuid.NewString(), false}), "acknowledged retirement")
	if err := l.acknowledge(evpn.ExportResult{}, errors.New("withdrawal unverified")); err != nil || l.state.Phase != "retiring" {
		t.Fatal("failed withdrawal acknowledged")
	}
	requireError(t, l.acknowledge(evpn.ExportResult{Applied: []evpn.OwnedType2Route{{PathUUID: uuid.NewString()}}}, nil), "drainage")
	if err := l.acknowledge(evpn.ExportResult{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.State, "endpoint.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := l.desired(ctx, c, observe); err != nil || len(got) != 0 || observations != 1 {
		t.Fatal("removed retirement request resurrected old generation")
	}
	next := uuid.NewString()
	publish(t, filepath.Join(c.State, "endpoint.json"), endpointRequest{2, next, false})
	got, err := l.desired(ctx, c, observe)
	if err != nil || len(got) != 1 || got[0].Incarnation != next || got[0].EndpointID != c.Endpoint.EndpointID {
		t.Fatalf("new generation not observed: %v %v", got, err)
	}
	requireError(t, l.accept(endpointRequest{1, c.Endpoint.Incarnation, false}), "stale")
	requireError(t, l.accept(endpointRequest{2, uuid.NewString(), false}), "within generation")
	if err := l.accept(endpointRequest{2, next, true}); err != nil {
		t.Fatal(err)
	}
	if err := l.acknowledge(evpn.ExportResult{}, nil); err != nil {
		t.Fatal(err)
	}
	requireError(t, l.accept(endpointRequest{3, c.Endpoint.Incarnation, false}), "cannot reappear")
	requireError(t, l.accept(endpointRequest{3, strings.ToUpper(c.Endpoint.Incarnation), false}), "canonical")
	requireError(t, l.accept(endpointRequest{3, strings.ReplaceAll(c.Endpoint.Incarnation, "-", ""), false}), "canonical")
	after, pending, err := importer.Snapshot()
	if err != nil || pending != nil || string(before) != string(after) {
		t.Fatal("local endpoint generation changed independent importer history/foreign witnesses")
	}
	if err := l.journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openLifecycle(c, d)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.journal.Close()
	if !reflect.DeepEqual(reopened.state, l.state) {
		t.Fatal("durable generation retirement changed after projector restart")
	}
}

func TestLifecycleRejectsNoncanonicalInitialIncarnation(t *testing.T) {
	c, domain, l := lifecycleFixture(t)
	j, err := ownership.Open(filepath.Join(t.TempDir(), "fresh"), domain, true)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	requireError(t, (&lifecycle{journal: j}).restore(strings.ReplaceAll(l.state.Incarnation, "-", "")), "canonical")
	invalidState := l.state
	invalidState.Generation = 2
	invalidState.Incarnation = strings.ToUpper(l.state.Incarnation)
	invalidState.Used = append([]string{l.state.Incarnation}, invalidState.Incarnation)
	if err := l.persist(invalidState); err != nil {
		t.Fatal(err)
	}
	requireError(t, l.restore(c.Endpoint.Incarnation), "invalid or reused")
}

func TestLifecycleLegacyRetirementBeforeMissingObservation(t *testing.T) {
	c, _, l := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(c.State, "retire"), []byte(c.Endpoint.EndpointID+"/"+c.Endpoint.Incarnation+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := l.desired(context.Background(), c, func(context.Context, config) ([]evpn.LocalEndpoint, error) {
		t.Fatal("retirement attempted to observe deleted netns")
		return nil, nil
	})
	if err != nil || len(got) != 0 || l.state.Phase != "retiring" {
		t.Fatalf("retirement not authoritative: %v %v", got, err)
	}
}

func TestRetiringGenerationRemainsFencedAcrossProjectorRestart(t *testing.T) {
	c, domain, ledger := lifecycleFixture(t)
	if err := ledger.accept(endpointRequest{1, c.Endpoint.Incarnation, true}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openLifecycle(c, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.journal.Close()
	next := endpointRequest{2, uuid.NewString(), false}
	requireError(t, reopened.accept(next), "acknowledged retirement")
	for _, result := range []evpn.ExportResult{
		{OwnershipUnverified: true},
		{Applied: []evpn.OwnedType2Route{{PathUUID: uuid.NewString()}}},
	} {
		if err := reopened.acknowledge(result, nil); err == nil {
			t.Fatal("interrupted or unverified withdrawal acknowledged")
		}
		requireError(t, reopened.accept(next), "acknowledged retirement")
	}
	if err := reopened.acknowledge(evpn.ExportResult{}, errors.New("withdrawal interrupted")); err != nil {
		t.Fatal(err)
	}
	requireError(t, reopened.accept(next), "acknowledged retirement")
	if err := reopened.acknowledge(evpn.ExportResult{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := reopened.accept(next); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleIntegrityDomainPendingAndOverlappingWriter(t *testing.T) {
	for _, mode := range []string{"overlap", "domain", "corrupt", "missing", "pending"} {
		t.Run(mode, func(t *testing.T) {
			c, d, l := lifecycleFixture(t)
			if mode == "overlap" {
				other, err := openLifecycle(c, d)
				if other != nil {
					other.journal.Close()
				}
				requireError(t, err, "owner still active")
				return
			}
			if mode == "pending" {
				if _, err := l.journal.Begin("unacknowledged-lifecycle", l.state); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.journal.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.State, "lifecycle", "journal.json")
			switch mode {
			case "domain":
				d.ConfigDigest = strings.Repeat("a", 64)
			case "corrupt":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			other, err := openLifecycle(c, d)
			if other != nil {
				other.journal.Close()
			}
			if err == nil {
				t.Fatalf("%s accepted", mode)
			}
		})
	}
}

func TestLifecycleMissingOriginalRecoveryAndPartialInitializationCleanup(t *testing.T) {
	c, d, l := lifecycleFixture(t)
	if err := l.bootstrap(); err != nil {
		t.Fatal(err)
	}
	l.journal.Close()
	if _, err := openLifecycle(c, d); err == nil {
		t.Fatal("bootstrapped missing exporter became fresh ownership")
	}
	// Rejected startup must release its lock; inspect the original journal again.
	j, err := ownership.Open(filepath.Join(c.State, "lifecycle"), d, false)
	if err != nil {
		t.Fatalf("failed startup leaked lifecycle lock: %v", err)
	}
	j.Close()
}

func TestLifecycleImmutableConfigurationAndProtectedRequests(t *testing.T) {
	c, _, l := lifecycleFixture(t)
	path := filepath.Join(c.State, "config.json")
	data := []byte(`{"initialIncarnation":"` + c.Endpoint.Incarnation + `"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := immutableConfig(path, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	requireError(t, immutableConfig(path, data), "immutable")
	requestPath := filepath.Join(c.State, "endpoint.json")
	if err := os.Symlink(path, requestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := l.desired(context.Background(), c, observeEndpoint); err == nil {
		t.Fatal("symlink lifecycle request accepted")
	}
}

func TestRunFailedAttestationReleasesPartialInitialization(t *testing.T) {
	c, _, _ := lifecycleFixture(t)
	c.State = t.TempDir()
	c.EndpointNamespace, c.EndpointDevice = "remote2-work", "r2-work"
	c.Netlink.LogicalSwitch = "linux-bridge:br2000"
	c.Attestation = filepath.Join(c.State, "missing-attestation.json")
	c.Socket = "unix://" + filepath.Join(c.State, "missing.sock")
	path := filepath.Join(c.State, "remote2.json")
	publish(t, path, c)
	if err := run(path); err == nil {
		t.Fatal("missing attestation accepted")
	}
	data, err := protectedRead(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	d := ownership.Domain{InstallationUID: c.InstallationUID, NodeUID: c.NodeUID,
		BootID: strings.TrimSpace(string(boot)), NetworkNamespace: ns.UniqueId(),
		ConfigDigest: hex.EncodeToString(sum[:])}
	ledger, err := openLifecycle(c, d)
	if err != nil {
		t.Fatalf("partial runtime initialization leaked journal lock: %v", err)
	}
	defer ledger.journal.Close()
	if ledger.state.Bootstrapped {
		t.Fatal("failed attestation acknowledged runtime bootstrap")
	}
}
