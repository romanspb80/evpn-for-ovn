package evpn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

func TestWithdrawalFencePreservesForeignRoutesAcrossRestart(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	speaker, adapter := newType2TestSpeaker(t)
	if err := speaker.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	peer := &testVerifiedSpeaker{id: "speaker"}
	open := func() *ExportRecovery {
		t.Helper()
		r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	recovery := open()
	if _, err := recovery.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	owned := mustProviderRoute(t, sampleEndpoint())
	if _, err := recovery.Add(ctx, owned); err != nil {
		t.Fatal(err)
	}
	foreignEndpoint := sampleEndpoint()
	foreignEndpoint.EndpointID = "foreign"
	foreignEndpoint.MACAddress = "aa:bb:cc:dd:ee:01"
	foreignEndpoint.IPAddresses = []string{"10.251.1.12"}
	foreign := mustProviderRoute(t, foreignEndpoint)
	foreignID, err := adapter.Add(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.RequireWithdrawal(); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
	recovery = open()
	defer recovery.Close()
	if !recovery.WithdrawalRequired() {
		t.Fatal("withdrawal fence lost after restart")
	}
	exporter := &EndpointExporter{Observer: fixedLocalEndpoints{owned.Endpoint}, Gate: peer, Continuity: peer, Source: adapter, Applier: recovery, Recovery: recovery}
	peer.err = errors.New("speaker unavailable")
	if _, err := exporter.WithdrawAll(ctx); err == nil {
		t.Fatal("withdrawal accepted without speaker attestation")
	}
	peer.err = nil
	if _, err := exporter.Reconcile(ctx); err == nil {
		t.Fatal("fenced exporter accepted a non-empty desired set")
	}
	result, err := exporter.WithdrawAll(ctx)
	if err != nil || len(result.Applied) != 0 {
		t.Fatalf("owned withdrawal: %+v %v", result, err)
	}
	if _, err := recovery.Add(ctx, owned); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("fenced exporter accepted new advertisement: %v", err)
	}
	paths, err := adapter.Snapshot(ctx)
	if err != nil || len(paths) != 1 {
		t.Fatalf("foreign route not preserved: %v %v", paths, err)
	}
	if err := adapter.Delete(ctx, OwnedType2Route{Endpoint: foreign.Endpoint, PathUUID: foreignID}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingWithdrawalFenceCheckpointSurvivesRestart(t *testing.T) {
	cfg := testExportRecoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	speaker, adapter := newType2TestSpeaker(t)
	if err := speaker.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	peer := &testVerifiedSpeaker{id: "speaker"}
	r, err := OpenExportRecovery(cfg, adapter, adapter, peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	pending := r.state
	pending.WithdrawalRequired = true
	if _, err := r.journal.Begin("export-checkpoint", pending); err != nil {
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
	if !r.WithdrawalRequired() {
		t.Fatal("pending fence checkpoint was rolled back")
	}
}
