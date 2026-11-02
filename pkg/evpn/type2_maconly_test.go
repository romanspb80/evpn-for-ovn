package evpn

import (
	"context"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func TestMACOnlyPinnedGoBGPAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	speaker, adapter := newType2TestSpeaker(t)
	if err := speaker.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 64512, RouterId: "192.0.2.1", ListenPort: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	ep := sampleEndpoint()
	ep.MACOnly, ep.IPAddresses = true, nil
	client := &failingType2Client{GoBgpServiceClient: adapter.Client}
	adapter.Client = client
	r := NewRouteReconciler()
	if err := r.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	applyType2(t, r, adapter, planType2(t, r, ep))
	rib, err := adapter.Snapshot(ctx)
	if err != nil || len(rib) != 1 || !rib[0].Local {
		t.Fatalf("binary MAC-only API roundtrip: %+v %v", rib, err)
	}
	n := rib[0].NLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if n.IPAddressLength != 0 {
		t.Fatal("MAC-only API changed NLRI IP length")
	}
	applyType2(t, r, adapter, planType2(t, r))
	rib, err = adapter.Snapshot(ctx)
	if err != nil || len(rib) != 0 {
		t.Fatalf("MAC-only exact UUID withdrawal: %+v %v", rib, err)
	}
}

func TestExplicitMACOnlyType2(t *testing.T) {
	ep := sampleEndpoint()
	ep.IPAddresses = nil
	if _, err := BuildType2Route(ep); err == nil {
		t.Fatal("legacy invalid endpoint silently converted to MAC-only")
	}
	ep.MACOnly = true
	route, err := BuildType2Route(ep)
	if err != nil {
		t.Fatal(err)
	}
	got := route.NLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if got.IPAddressLength != 0 || got.IPAddress.IsValid() || got.ETag != 0 {
		t.Fatal("invalid MAC-only encoding")
	}
	wire, err := got.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	decoded := &bgp.EVPNMacIPAdvertisementRoute{}
	if err := decoded.DecodeFromBytes(wire); err != nil || decoded.String() != got.String() {
		t.Fatalf("MAC-only roundtrip: %v", err)
	}
	ep.IPAddresses = []string{"10.77.0.11"}
	if _, err := BuildType2Route(ep); err == nil {
		t.Fatal("conflicting MAC-only and IP accepted")
	}
}
