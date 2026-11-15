package evpn

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

type type2GoldenFixture struct {
	Cases []struct {
		Name     string        `json:"name"`
		Endpoint LocalEndpoint `json:"endpoint"`
		Expected struct {
			OwnershipKey     string                   `json:"ownershipKey"`
			EndpointRouteKey string                   `json:"endpointRouteKey"`
			RouteIdentity    string                   `json:"routeIdentity"`
			RD               string                   `json:"rd"`
			ESI              string                   `json:"esi"`
			EthernetTag      uint32                   `json:"ethernetTag"`
			MAC              string                   `json:"mac"`
			IP               string                   `json:"ip"`
			Labels           []uint32                 `json:"labels"`
			Origin           string                   `json:"origin"`
			NextHop          string                   `json:"nextHop"`
			Communities      []Type2ExtendedCommunity `json:"communities"`
			NLRIHex          string                   `json:"nlriHex"`
			AttributesHex    []string                 `json:"attributesHex"`
		} `json:"expected"`
	} `json:"cases"`
}

func readType2GoldenFixture(t *testing.T) type2GoldenFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/type2-domain-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture type2GoldenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestType2GoldenDomainAndWireContract(t *testing.T) {
	fixture := readType2GoldenFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ep := tc.Endpoint
			if got := ep.OwnershipKey(); got != tc.Expected.OwnershipKey {
				t.Fatalf("ownership key: got %q want %q", got, tc.Expected.OwnershipKey)
			}
			if got := ep.RouteKey(); got != tc.Expected.EndpointRouteKey {
				t.Fatalf("endpoint route key: got %q want %q", got, tc.Expected.EndpointRouteKey)
			}
			route, err := BuildType2Route(ep)
			if err != nil {
				t.Fatal(err)
			}
			if route.NLRI.Key() != tc.Expected.RouteIdentity ||
				route.NLRI.RD != tc.Expected.RD ||
				route.NLRI.ESI != tc.Expected.ESI ||
				route.NLRI.EthernetTag != tc.Expected.EthernetTag ||
				route.NLRI.MAC != tc.Expected.MAC ||
				route.NLRI.IP != tc.Expected.IP ||
				!reflect.DeepEqual(route.NLRI.Labels, tc.Expected.Labels) {
				t.Fatalf("NLRI differs from baseline: %+v", route.NLRI)
			}
			if route.PathAttributes.Origin != tc.Expected.Origin ||
				route.PathAttributes.NextHop != tc.Expected.NextHop ||
				!reflect.DeepEqual(route.PathAttributes.ExtendedCommunities, tc.Expected.Communities) {
				t.Fatalf("path attributes differ from baseline: %+v", route.PathAttributes)
			}
			nlriHex, attributesHex, err := type2WirePayload(ep)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(nlriHex); got != tc.Expected.NLRIHex {
				t.Fatalf("serialized NLRI: got %s want %s", got, tc.Expected.NLRIHex)
			}
			if len(attributesHex) != len(tc.Expected.AttributesHex) {
				t.Fatalf("serialized attributes count: got %d want %d", len(attributesHex), len(tc.Expected.AttributesHex))
			}
			for i, attr := range attributesHex {
				if got := hex.EncodeToString(attr); got != tc.Expected.AttributesHex[i] {
					t.Fatalf("serialized attribute %d: got %s want %s", i, got, tc.Expected.AttributesHex[i])
				}
			}
		})
	}
}

func TestType2GoldenNonzeroESIAndEthernetTagRoundTrip(t *testing.T) {
	route, err := BuildType2Route(sampleEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	route.NLRI.ESI = "0102030405060708090a"
	route.NLRI.EthernetTag = 17
	nlri, _, err := type2RouteToGoBGP(route)
	if err != nil {
		t.Fatal(err)
	}
	got, err := type2NLRIFromGoBGP(nlri)
	if err != nil {
		t.Fatal(err)
	}
	if got.ESI != "0102030405060708090a" || got.EthernetTag != 17 {
		t.Fatalf("nonzero ESI/tag changed in GoBGP conversion: %+v", got)
	}
}

func TestType2GoldenReconciliationPlanning(t *testing.T) {
	api := &memoryType2API{}
	reconciler := syncedReconciler(t, api)
	first, second := sampleEndpoint(), sampleEndpoint()
	first.MACAddress, first.EndpointID = "02:00:00:00:00:02", "endpoint-2"
	second.MACAddress, second.EndpointID = "02:00:00:00:00:01", "endpoint-1"

	changes, err := reconciler.Reconcile([]LocalEndpoint{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].NLRI.MAC != second.MACAddress || changes[1].NLRI.MAC != first.MACAddress {
		t.Fatalf("adds are not stable by wire identity: %+v", changes)
	}
	reversed, err := reconciler.Reconcile([]LocalEndpoint{second, first})
	if err != nil || !reflect.DeepEqual(changeContract(changes), changeContract(reversed)) {
		t.Fatalf("input ordering changed plan: first=%+v reversed=%+v err=%v", changes, reversed, err)
	}
	duplicates, err := reconciler.Reconcile([]LocalEndpoint{first, first})
	if err != nil || len(duplicates) != 1 {
		t.Fatalf("equivalent duplicate observations: changes=%+v err=%v", duplicates, err)
	}
	conflict := first
	conflict.Incarnation = "next"
	if _, err := reconciler.Reconcile([]LocalEndpoint{first, conflict}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("conflicting duplicate owner did not preserve baseline rejection: %v", err)
	}

	applyType2(t, reconciler, api, reconcilerPlan(t, reconciler, []LocalEndpoint{first, second}))
	if noOp, err := reconciler.Reconcile([]LocalEndpoint{second, first}); err != nil || len(noOp) != 0 {
		t.Fatalf("equivalent applied state was not a no-op: changes=%+v err=%v", noOp, err)
	}

	updated := first
	updated.LocalVTEP = "192.0.2.20"
	changes, err = reconciler.Reconcile([]LocalEndpoint{updated})
	if err != nil || len(changes) != 3 ||
		changes[0].Action != RouteWithdraw || changes[0].NLRI.MAC != second.MACAddress ||
		changes[1].Action != RouteWithdraw || changes[1].NLRI.MAC != first.MACAddress ||
		changes[2].Action != RouteAdvertise || changes[2].NLRI.MAC != first.MACAddress {
		t.Fatalf("update/withdraw plan differs from baseline: changes=%+v err=%v", changes, err)
	}
}

func changeContract(changes []Type2Route) []string {
	out := make([]string, len(changes))
	for i, change := range changes {
		out[i] = string(change.Action) + ":" + change.NLRI.Key() + ":" + change.Endpoint.OwnershipKey()
	}
	return out
}

func reconcilerPlan(t *testing.T, reconciler *RouteReconciler, desired []LocalEndpoint) []Type2Route {
	t.Helper()
	changes, err := reconciler.Reconcile(desired)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestType2GoldenInvalidInputsRemainRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LocalEndpoint)
	}{
		{"missing-ip", func(ep *LocalEndpoint) { ep.IPAddresses = nil }},
		{"invalid-mac", func(ep *LocalEndpoint) { ep.MACAddress = "not-a-mac" }},
		{"invalid-rd", func(ep *LocalEndpoint) { ep.RouteDistinguisher = "bad" }},
		{"invalid-rt", func(ep *LocalEndpoint) { ep.RouteTargets = []string{"bad"} }},
		{"zero-vni", func(ep *LocalEndpoint) { ep.VNI = 0 }},
		{"invalid-vtep", func(ep *LocalEndpoint) { ep.LocalVTEP = "not-an-ip" }},
		{"mac-only-with-ip", func(ep *LocalEndpoint) { ep.MACOnly = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := sampleEndpoint()
			tc.mutate(&ep)
			if _, err := BuildType2Route(ep); err == nil {
				t.Fatal("baseline-invalid Type-2 input was accepted")
			}
		})
	}
}

func TestType2GoldenGoBGPNLRIUsesEVPNNextHopAttribute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	speaker, adapter := newType2TestSpeaker(t)
	if err := speaker.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRouteReconciler()
	if err := reconciler.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	applyType2(t, reconciler, adapter, planType2(t, reconciler, sampleEndpoint()))
	stream, err := adapter.Client.ListPath(ctx, &api.ListPathRequest{
		TableType:        api.TableType_TABLE_TYPE_GLOBAL,
		Family:           &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
		EnableOnlyBinary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Destination == nil || len(response.Destination.Paths) == 0 {
		t.Fatal("GoBGP returned no Type-2 path")
	}
	path := response.Destination.Paths[0]
	nativeAttrs, err := apiutil.GetNativePathAttributes(path)
	if err != nil {
		t.Fatal(err)
	}
	domainAttrs, err := type2PathAttributesFromGoBGP(nativeAttrs)
	if err != nil {
		t.Fatal(err)
	}
	if domainAttrs.NextHop != "192.0.2.10" {
		t.Fatalf("GoBGP EVPN next hop changed: %+v", domainAttrs)
	}
	hasMPReach := false
	for _, attr := range nativeAttrs {
		if _, ok := attr.(*bgp.PathAttributeMpReachNLRI); ok {
			hasMPReach = true
		}
	}
	if !hasMPReach {
		t.Fatal("GoBGP EVPN path did not expose next hop through MP_REACH")
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("expected completed single-path RIB stream, got %v", err)
	}
}
