package evpn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
)

// I5/I9: the same receipt set has one challenge across normal reconciliation.
func TestInventoryOwnershipDigestCanonical(t *testing.T) {
	if inventoryOwnershipDigest(nil) != inventoryOwnershipDigest([]exportReceipt{}) {
		t.Fatal("empty ownership changed challenge after reconciliation")
	}
	a := exportReceipt{Owned: OwnedType2Route{PathUUID: "a"}}
	b := exportReceipt{Owned: OwnedType2Route{PathUUID: "b"}}
	if inventoryOwnershipDigest([]exportReceipt{a, b}) != inventoryOwnershipDigest([]exportReceipt{b, a}) {
		t.Fatal("receipt order changed recovery challenge")
	}
	if inventoryOwnershipDigest([]exportReceipt{a}) == inventoryOwnershipDigest([]exportReceipt{b}) {
		t.Fatal("different ownership has the same recovery challenge")
	}
}

// I1/I4/I6/I8: READY is distinct from inventory FINAL; direct Add and
// mismatched bindings are blocked, and withdrawal still preserves foreigners.
func TestInventoryExportGrantAndForeignSafety(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, adapter := newType2TestSpeaker(t)
	if err := server.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	peer := &testVerifiedSpeaker{id: "original"}
	r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	route := mustProviderRoute(t, sampleEndpoint())
	owned, err := r.Add(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	foreignEndpoint := sampleEndpoint()
	foreignEndpoint.EndpointID = "foreign"
	foreignEndpoint.MACAddress = "aa:bb:cc:dd:ee:01"
	foreignEndpoint.IPAddresses = []string{"10.251.1.12"}
	foreign := mustProviderRoute(t, foreignEndpoint)
	if _, err := adapter.Add(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := r.RequireWithdrawal(); err != nil {
		t.Fatal(err)
	}
	e, err := r.InventoryEvidence()
	if err != nil || !e.LegacyFence {
		t.Fatalf("legacy fence lost: %+v %v", e, err)
	}
	if err := r.EnableInventory(); err != nil {
		t.Fatal(err)
	}
	binding := workerinventory.TrustDigest("inventory FINAL binding")
	if err := r.InventoryReady(ctx, binding, true); err == nil {
		t.Fatal("grant with old ownership accepted")
	}
	if err := r.Delete(ctx, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: owned}); err != nil {
		t.Fatal(err)
	}
	if err := r.InventoryReady(ctx, binding, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, route); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("unguarded Add bypassed inventory FINAL: %v", err)
	}
	if _, err := r.AddInventory(ctx, route, "wrong-worker-binding"); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatal("wrong grant accepted")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddInventory(ctx, route, binding); err != nil {
		t.Fatal(err)
	}
	paths, err := adapter.Snapshot(ctx)
	if err != nil || len(paths) != 2 {
		t.Fatalf("foreign route changed: %d %v", len(paths), err)
	}
}

// I8: replay of an interrupted export checkpoint can only invalidate READY,
// never clear withdrawal or manufacture a new grant.
func TestInventoryPendingReadyCannotGrant(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, adapter := newType2TestSpeaker(t)
	if err := server.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	peer := &testVerifiedSpeaker{id: "original"}
	r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.EnableInventory(); err != nil {
		t.Fatal(err)
	}
	if err := r.RequireWithdrawal(); err != nil {
		t.Fatal(err)
	}
	next := r.state
	next.WithdrawalRequired = false
	next.InventoryReady = workerinventory.TrustDigest("uncommitted")
	if _, err := r.journal.Begin("export-checkpoint", next); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := r.InventoryEvidence()
	if err != nil || e.Ready != "" || !r.WithdrawalRequired() {
		t.Fatalf("pending grant became authority: %+v %v", e, err)
	}
}
