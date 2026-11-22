package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

func TestInventoryStartupWithdrawalCannotAdd(t *testing.T) {
	peer := &inventoryTestSpeaker{}
	applier := withdrawalOnlyApplier{peer}
	if _, err := applier.Add(context.Background(), evpn.Type2Route{}); err == nil || peer.adds != 0 {
		t.Fatal("withdrawal-only startup allowed an Add")
	}
	peer.paths = []evpn.Type2RIBPath{{PathUUID: "owned"}, {PathUUID: "foreign"}}
	if err := applier.Delete(context.Background(), evpn.OwnedType2Route{PathUUID: "owned"}); err != nil ||
		len(peer.paths) != 1 || peer.paths[0].PathUUID != "foreign" {
		t.Fatalf("withdrawal-only adapter changed Delete scope: %+v %v", peer.paths, err)
	}
}

func TestInventoryStartupWithdrawalRejectsNonLiveConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled.json")
	if err := os.WriteFile(path, []byte(`{"Enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := ExportRuntimeOptions{ConfigPath: path, RequireLocalChassis: true,
		InventoryGate: func(context.Context, evpn.ExporterConfig) error {
			calls++
			return errors.New("publication remains invalid")
		}}
	result, err := WithdrawInventoryExports(context.Background(),
		RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"}, opts, errors.New("denied"))
	if err == nil || !strings.Contains(err.Error(), "live durable") || !result.OwnershipUnverified ||
		result.RecoveryState != "RECOVERY_BLOCKED" || calls != 0 {
		t.Fatalf("denied inventory was used as cleanup permission or cleanup was falsely confirmed: %+v %v calls=%d", result, err, calls)
	}
}

func TestInventoryWithdrawalDoesNotRequireAssignedVTEP(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected exporter configuration requires root")
	}
	_, cfg, _, _, _, _ := inventoryTestFixture(t)
	t.Setenv("NODE_NAME", cfg.NodeName)
	cfg.Networks[0].RouteDistinguisher = "192.0.2.32:100"
	cfg.Networks[0].RouteTargets = []string{"64512:100"}
	if err := validateLocalExportConfig(cfg); err == nil {
		t.Skip("test VTEP is assigned on this host")
	}
	dir := t.TempDir()
	cfg.Recovery.StateDirectory = dir
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "export.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := WithdrawInventoryExports(ctx, RuntimeOptions{Mode: RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth", OVNNBEndpoint: "unix:" + filepath.Join(dir, "missing-nb.sock")},
		ExportRuntimeOptions{ConfigPath: config, RequireLocalChassis: true,
			InventoryGate: func(context.Context, evpn.ExporterConfig) error { return errors.New("denied") }},
		errors.New("VTEP no longer assigned"))
	if err == nil || !strings.Contains(err.Error(), "startup withdrawal domain verification") || !result.OwnershipUnverified {
		t.Fatalf("withdrawal was blocked by the advertisement-only VTEP condition: %+v %v", result, err)
	}
}

func TestInventoryStartupWithdrawalRetainsDurableScopeAcrossRestart(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected startup withdrawal journal requires root")
	}
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "speaker-retained", true: "RIB-unavailable"}[unavailable], func(t *testing.T) {
			ctx := context.Background()
			peer := &inventoryTestSpeaker{}
			cfg := evpn.ExportRecoveryConfig{StateDirectory: filepath.Join(t.TempDir(), "export"),
				Domain: ownership.Domain{InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(),
					BootID: uuid.NewString(), NetworkNamespace: "startup-test", ConfigDigest: strings.Repeat("a", 64)}}
			recovery, err := evpn.OpenExportRecovery(cfg, peer, peer, peer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recovery.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			endpoint := evpn.LocalEndpoint{NetworkID: "n24", EndpointID: "owned", Incarnation: "current-workload",
				MACAddress: "02:00:00:00:00:11", IPAddresses: []string{"10.251.1.11"}, VNI: 100,
				RouteDistinguisher: "192.0.2.32:100", RouteTargets: []string{"64512:100"}, LocalVTEP: "192.0.2.32",
				NodeName: "worker", Chassis: "chassis", Advertise: true}
			route, err := evpn.BuildType2Route(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			owned, err := recovery.Add(ctx, route)
			if err != nil {
				t.Fatal(err)
			}
			foreignEndpoint := endpoint
			foreignEndpoint.EndpointID, foreignEndpoint.MACAddress = "foreign", "02:00:00:00:00:22"
			foreignEndpoint.IPAddresses = []string{"10.251.1.22"}
			foreignRoute, err := evpn.BuildType2Route(foreignEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := peer.Add(ctx, foreignRoute)
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.Close(); err != nil {
				t.Fatal(err)
			}
			// Only the agent's journal owner restarts; the speaker and its RIB survive.
			recovery, err = evpn.OpenExportRecovery(cfg, peer, withdrawalOnlyApplier{peer}, peer)
			if err != nil {
				t.Fatal(err)
			}
			defer recovery.Close()
			if unavailable {
				peer.snapshotErr = errors.New("RIB unavailable")
			}
			exporter := &evpn.EndpointExporter{Gate: peer, Continuity: peer, Source: peer, Applier: recovery, Recovery: recovery}
			result, err := withdrawFencedExport(ctx, exporter, recovery)
			if !recovery.WithdrawalRequired() || peer.adds != 2 {
				t.Fatal("startup cleanup cleared the barrier or added a route")
			}
			if unavailable {
				if err == nil || !result.OwnershipUnverified || len(peer.paths) != 2 {
					t.Fatal("unobserved RIB cleanup claimed success or deleted routes")
				}
				if err := recovery.Close(); err != nil {
					t.Fatal(err)
				}
				recovery, err = evpn.OpenExportRecovery(cfg, peer, withdrawalOnlyApplier{peer}, peer)
				if err != nil {
					t.Fatal(err)
				}
				defer recovery.Close()
				if !recovery.WithdrawalRequired() {
					t.Fatal("failed cleanup lost its durable barrier across restart")
				}
				peer.snapshotErr = nil
				exporter.Applier, exporter.Recovery = recovery, recovery
				result, err = withdrawFencedExport(ctx, exporter, recovery)
			}
			if err != nil || len(result.Applied) != 0 || len(peer.paths) != 1 || peer.paths[0].PathUUID != foreign ||
				peer.paths[0].PathUUID == owned {
				t.Fatalf("startup cleanup failed exact owned withdrawal: %+v %+v %v", result, peer.paths, err)
			}
			if _, err := recovery.Add(ctx, route); err == nil {
				t.Fatal("startup cleanup authorized re-advertisement")
			}
		})
	}
}
