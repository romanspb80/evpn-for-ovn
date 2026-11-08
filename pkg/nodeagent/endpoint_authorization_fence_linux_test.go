//go:build linux

package nodeagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

func TestEndpointAuthorizationFenceRecoversInterruptedAcceptanceBeforePublication(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify durable ownership journal permissions")
	}
	directory := filepath.Join(t.TempDir(), "authorization-fence")
	policy, err := endpointauth.NewSnapshot(endpointauth.Document{
		Version: 1, Revision: 21, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []endpointauth.EndpointRecord{},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := &standaloneExportTestStore{}
	temporaryFencer, err := endpointauth.OpenFencer(memory, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporaryFencer.Accept(policy); err != nil {
		t.Fatal(err)
	}
	if err := temporaryFencer.Close(); err != nil {
		t.Fatal(err)
	}

	domain, err := endpointAuthorizationFenceDomain(policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ownership.Open(directory, domain, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Begin(endpointAuthorizationFenceOperation, json.RawMessage(memory.data)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenEndpointAuthorizationFencer(directory, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	accepted, err := recovered.Accept(policy)
	if err != nil || accepted == nil || !accepted.IsCurrent() {
		t.Fatalf("interrupted durable acceptance was not recovered before publication: accepted=%v err=%v",
			accepted != nil, err)
	}
}

func TestEndpointAuthorizationFenceRestartAndRouteReceiptIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify durable ownership journal permissions")
	}
	root := t.TempDir()
	routeDirectory := filepath.Join(root, "route-receipts")
	authDirectory := filepath.Join(root, "authorization-fence")
	routeDomain := ownership.Domain{
		InstallationUID:  "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID:          "46b08606-9b26-4098-90d7-02c14098122b",
		BootID:           "8a63e552-b620-445f-9ab6-8d924321ccfe",
		NetworkNamespace: "test-network-namespace",
		ConfigDigest:     strings.Repeat("a", 64),
	}
	routeJournal, err := ownership.Open(routeDirectory, routeDomain, true)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := routeJournal.Begin("test-route-receipt", map[string]string{"uuid": "route-owned-uuid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := routeJournal.Commit(sequence, map[string]string{"receipt": "route-owned-uuid"}); err != nil {
		t.Fatal(err)
	}
	before, pending, err := routeJournal.Snapshot()
	if err != nil || pending != nil {
		t.Fatalf("route receipt snapshot failed: pending=%v err=%v", pending, err)
	}
	if err := routeJournal.Close(); err != nil {
		t.Fatal(err)
	}

	policy, err := endpointauth.NewSnapshot(endpointauth.Document{
		Version: 1, Revision: 12, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []endpointauth.EndpointRecord{{
			NetworkID: "net-a", EndpointID: "endpoint-1", AttachmentID: "attachment-1",
			AttachmentGeneration: 4, LogicalSwitchPort: "lsp-1",
			LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000123",
			MAC:                   "02:00:00:00:00:01", IP: "192.0.2.10",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fencer, err := OpenEndpointAuthorizationFencer(authDirectory, "installation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fencer.Accept(policy); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenEndpointAuthorizationFencer(authDirectory, "installation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := restarted.Accept(policy)
	if err != nil || accepted == nil || !accepted.IsCurrent() {
		t.Fatalf("restart did not recover accepted policy: accepted=%v err=%v", accepted != nil, err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	routeJournal, err = ownership.Open(routeDirectory, routeDomain, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := routeJournal.Close(); err != nil {
			t.Error(err)
		}
	})
	after, pending, err := routeJournal.Snapshot()
	if err != nil || pending != nil || string(before) != string(after) {
		t.Fatalf("authorization fence changed independent route receipts: before=%s after=%s pending=%v err=%v",
			before, after, pending, err)
	}
}
