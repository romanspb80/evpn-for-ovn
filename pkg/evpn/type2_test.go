package evpn

import (
	"bytes"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func sampleEndpoint() LocalEndpoint {
	return LocalEndpoint{
		NetworkID:          "net-a",
		EndpointID:         "endpoint-1",
		Incarnation:        "inc-1",
		MACAddress:         "aa:bb:cc:dd:ee:ff",
		IPAddresses:        []string{"10.251.1.11/24", "10.251.1.11"},
		VNI:                100,
		RouteDistinguisher: "65000:1000",
		RouteTargets:       []string{"65000:100"},
		LocalVTEP:          "192.0.2.10",
		NodeName:           "kind0-control-plane",
		Chassis:            "chassis-1",
		Advertise:          true,
	}
}

func TestBuildType2RouteProducesValidMACIPAdvertisement(t *testing.T) {
	ep := sampleEndpoint()
	route, err := BuildType2Route(ep)
	if err != nil {
		t.Fatalf("BuildType2Route returned error: %v", err)
	}
	if route.Action != RouteAdvertise {
		t.Fatalf("expected advertise action, got %s", route.Action)
	}
	if route.NLRI == nil {
		t.Fatal("expected EVPN NLRI route payload")
	}
	if len(route.PathAttributes) != 3 {
		t.Fatalf("expected 3 path attributes, got %d", len(route.PathAttributes))
	}

	if route.NLRI.String() == "" {
		t.Fatal("expected serialized NLRI content")
	}
	for _, attr := range route.PathAttributes {
		switch a := attr.(type) {
		case *bgp.PathAttributeNextHop:
			if got := a.Value.String(); got != "192.0.2.10" {
				t.Fatalf("unexpected next-hop: got %s want 192.0.2.10", got)
			}
		case *bgp.PathAttributeExtendedCommunities:
			if len(a.Value) != 2 {
				t.Fatalf("expected RT and VXLAN encapsulation communities, got %d", len(a.Value))
			}
			wantRT, err := bgp.ParseRouteTarget(ep.RouteTargets[0])
			if err != nil {
				t.Fatal(err)
			}
			wantRTBytes, err := wantRT.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			hasRT := false
			hasVXLAN := false
			for _, community := range a.Value {
				if encap, ok := community.(*bgp.EncapExtended); ok &&
					encap.TunnelType == bgp.TUNNEL_TYPE_VXLAN {
					hasVXLAN = true
				}
				gotBytes, err := community.Serialize()
				if err != nil {
					t.Fatal(err)
				}
				hasRT = hasRT || bytes.Equal(gotBytes, wantRTBytes)
			}
			if !hasRT {
				t.Fatal("expected configured route target community")
			}
			if !hasVXLAN {
				t.Fatal("expected VXLAN encapsulation community")
			}
		}
	}
}

func TestRIBPathMatchesEndpointAllowsGoBGPMobilityCommunity(t *testing.T) {
	route, err := BuildType2Route(sampleEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	ext := route.PathAttributes[2].(*bgp.PathAttributeExtendedCommunities)
	attrs := append([]bgp.PathAttributeInterface(nil), route.PathAttributes...)
	attrs[2] = bgp.NewPathAttributeExtendedCommunities(append(
		append([]bgp.ExtendedCommunityInterface(nil), ext.Value...),
		&bgp.MacMobilityExtended{Sequence: 0},
	))
	path := Type2RIBPath{
		Local:          true,
		NLRI:           route.NLRI,
		PathAttributes: attrs,
	}
	if !ribPathMatchesEndpoint(path, sampleEndpoint()) {
		t.Fatal("canonical GoBGP MAC-mobility community should not invalidate the owned route")
	}

	invalidMobility := append([]bgp.PathAttributeInterface(nil), attrs...)
	invalidMobility[2] = bgp.NewPathAttributeExtendedCommunities(append(
		append([]bgp.ExtendedCommunityInterface(nil), ext.Value...),
		&bgp.MacMobilityExtended{Sequence: 1},
	))
	path.PathAttributes = invalidMobility
	if ribPathMatchesEndpoint(path, sampleEndpoint()) {
		t.Fatal("nonzero MAC-mobility sequence must not match the owned route")
	}

	unexpectedCommunity := append([]bgp.PathAttributeInterface(nil), attrs...)
	unexpectedCommunity[2] = bgp.NewPathAttributeExtendedCommunities(append(
		append([]bgp.ExtendedCommunityInterface(nil), ext.Value...),
		&bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_MPLS},
	))
	path.PathAttributes = unexpectedCommunity
	if ribPathMatchesEndpoint(path, sampleEndpoint()) {
		t.Fatal("unexpected encapsulation must not match the owned route")
	}
}

func TestRouteReconcilerIsIdempotentAndWithdrawsOnDelete(t *testing.T) {
	api := &memoryType2API{}
	reconciler := syncedReconciler(t, api)
	ep := sampleEndpoint()

	changes, err := reconciler.Reconcile([]LocalEndpoint{ep})
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 advertise delta, got %d", len(changes))
	}
	if changes[0].Action != RouteAdvertise {
		t.Fatalf("expected advertise action, got %s", changes[0].Action)
	}
	applyType2(t, reconciler, api, changes)

	changes, err = reconciler.Reconcile([]LocalEndpoint{ep})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no-op second reconcile, got %d changes", len(changes))
	}

	changes, err = reconciler.Reconcile(nil)
	if err != nil {
		t.Fatalf("delete reconcile failed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected one withdraw delta, got %d", len(changes))
	}
	if changes[0].Action != RouteWithdraw {
		t.Fatalf("expected withdraw action, got %s", changes[0].Action)
	}
}

func TestRouteReconcilerHandlesRelocationAndStaleIncarnation(t *testing.T) {
	api := &memoryType2API{}
	reconciler := syncedReconciler(t, api)
	old := sampleEndpoint()
	old.Incarnation = "inc-1"
	new := old
	new.Incarnation = "inc-2"
	new.LocalVTEP = "192.0.2.20"

	applyType2(t, reconciler, api, planType2(t, reconciler, old))
	changes, err := reconciler.Reconcile([]LocalEndpoint{new})
	if err != nil {
		t.Fatalf("reconcile new endpoint failed: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected withdraw+advertise for relocation, got %d changes", len(changes))
	}
	if changes[0].Action != RouteWithdraw || changes[1].Action != RouteAdvertise {
		t.Fatalf("unexpected route ordering: %#v", changes)
	}
	if changes[1].Endpoint.LocalVTEP != "192.0.2.20" {
		t.Fatalf("expected relocated VTEP to be advertised, got %s", changes[1].Endpoint.LocalVTEP)
	}
}

func TestValidateRejectsInvalidInputs(t *testing.T) {
	ep := sampleEndpoint()
	ep.RouteDistinguisher = "invalid"
	if _, err := BuildType2Route(ep); err == nil {
		t.Fatal("expected invalid RD to be rejected")
	}

	if ep.VNI == 0 {
		t.Fatal("test setup is invalid")
	}
}

func TestRouteReconcilerUnappliedAdvertisementRemainsRetryable(t *testing.T) {
	r := NewRouteReconciler()
	for attempt := 0; attempt < 2; attempt++ {
		changes, err := r.Reconcile([]LocalEndpoint{sampleEndpoint()})
		if err != nil || len(changes) != 1 || changes[0].Action != RouteAdvertise {
			t.Fatalf("attempt %d lost unapplied advertisement: changes=%v err=%v", attempt, changes, err)
		}
	}
}

func TestRouteReconcilerRDParticipatesInIdentity(t *testing.T) {
	old := sampleEndpoint()
	next := old
	next.RouteDistinguisher = "65000:2000"
	if old.Equivalent(next) {
		t.Fatal("different RDs compare equivalent")
	}
	if old.RouteKey() == next.RouteKey() {
		t.Fatal("different RDs have the same route key")
	}
}
