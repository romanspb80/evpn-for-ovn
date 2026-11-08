package evpn

import "testing"

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
	if route.NLRI.RD != ep.RouteDistinguisher || route.NLRI.MAC != ep.MACAddress ||
		route.NLRI.IP != "10.251.1.11" || len(route.NLRI.Labels) != 1 || route.NLRI.Labels[0] != ep.VNI {
		t.Fatalf("unexpected Type-2 NLRI: %+v", route.NLRI)
	}
	if route.PathAttributes.Origin != Type2OriginIGP || route.PathAttributes.NextHop != ep.LocalVTEP {
		t.Fatalf("unexpected Type-2 path attributes: %+v", route.PathAttributes)
	}
	wantCommunities := []Type2ExtendedCommunity{
		{Kind: Type2RouteTargetCommunity, Value: ep.RouteTargets[0]},
		{Kind: Type2EncapsulationCommunity, Value: "vxlan"},
	}
	if !equalType2Communities(route.PathAttributes.ExtendedCommunities, wantCommunities) {
		t.Fatalf("unexpected route communities: %+v", route.PathAttributes.ExtendedCommunities)
	}
}

func TestRIBPathMatchesEndpointAllowsGoBGPMobilityCommunity(t *testing.T) {
	route, err := BuildType2Route(sampleEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	attrs := route.PathAttributes
	attrs.ExtendedCommunities = append(append([]Type2ExtendedCommunity(nil), attrs.ExtendedCommunities...),
		Type2ExtendedCommunity{Kind: Type2MACMobilityCommunity})
	path := Type2RIBPath{
		Local:          true,
		NLRI:           &route.NLRI,
		PathAttributes: attrs,
	}
	if !ribPathMatchesEndpoint(path, sampleEndpoint()) {
		t.Fatal("canonical zero MAC-mobility community should not invalidate the owned route")
	}

	invalidMobility := attrs
	invalidMobility.ExtendedCommunities = append([]Type2ExtendedCommunity(nil), attrs.ExtendedCommunities...)
	invalidMobility.ExtendedCommunities[len(invalidMobility.ExtendedCommunities)-1].Sequence = 1
	path.PathAttributes = invalidMobility
	if ribPathMatchesEndpoint(path, sampleEndpoint()) {
		t.Fatal("nonzero MAC-mobility sequence must not match the owned route")
	}

	unexpectedCommunity := attrs
	unexpectedCommunity.ExtendedCommunities = append([]Type2ExtendedCommunity(nil), attrs.ExtendedCommunities...)
	unexpectedCommunity.ExtendedCommunities = append(unexpectedCommunity.ExtendedCommunities,
		Type2ExtendedCommunity{Kind: Type2EncapsulationCommunity, Value: "mpls"})
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

func TestRouteReconcilerDoesNotAdvertiseUnapprovedObservation(t *testing.T) {
	reconciler := NewRouteReconciler()
	endpoint := sampleEndpoint()
	endpoint.Advertise = false

	changes, err := reconciler.Reconcile([]LocalEndpoint{endpoint})
	if err != nil {
		t.Fatalf("reconcile unapproved observation: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("unapproved endpoint observation produced route changes: %#v", changes)
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
