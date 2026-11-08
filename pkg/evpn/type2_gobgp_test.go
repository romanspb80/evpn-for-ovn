package evpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func newType2TestSpeaker(t *testing.T, bgpPorts ...int32) (*server.BgpServer, *GoBGPType2Adapter) {
	s, adapter, _ := newType2TestSpeakerAddress(t, bgpPorts...)
	return s, adapter
}

func newType2TestSpeakerAddress(t *testing.T, bgpPorts ...int32) (*server.BgpServer, *GoBGPType2Adapter, string) {
	t.Helper()
	port := reserveTCPPort(t)
	for attempts := 0; slices.Contains(bgpPorts, port); attempts++ {
		if attempts == 32 {
			t.Fatal("cannot reserve a separate GoBGP gRPC port")
		}
		port = reserveTCPPort(t)
	}
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	s := server.NewBgpServer(server.GrpcListenAddress(address))
	go s.Serve()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		s.Stop()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ctx, state) {
			t.Fatal("local GoBGP gRPC server did not become ready")
		}
	}
	return s, &GoBGPType2Adapter{Client: api.NewGoBgpServiceClient(conn)}, address
}

type failingType2Client struct {
	api.GoBgpServiceClient
	addErr         error
	deleteErr      error
	addReplyErr    error
	deleteReplyErr error
}

func (c *failingType2Client) AddPath(ctx context.Context, request *api.AddPathRequest, options ...grpc.CallOption) (*api.AddPathResponse, error) {
	if c.addErr != nil {
		return nil, c.addErr
	}
	response, err := c.GoBgpServiceClient.AddPath(ctx, request, options...)
	if err == nil && c.addReplyErr != nil {
		return nil, c.addReplyErr
	}
	return response, err
}

func (c *failingType2Client) DeletePath(ctx context.Context, request *api.DeletePathRequest, options ...grpc.CallOption) (*api.DeletePathResponse, error) {
	if len(request.Uuid) != 16 || request.Path != nil {
		return nil, errors.New("unsafe DeletePath: expected only exact UUID")
	}
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	response, err := c.GoBgpServiceClient.DeletePath(ctx, request, options...)
	if err == nil && c.deleteReplyErr != nil {
		return nil, c.deleteReplyErr
	}
	return response, err
}

func TestType2GoBGPConfirmedLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, adapter := newType2TestSpeaker(t)
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	client := &failingType2Client{GoBgpServiceClient: adapter.Client}
	adapter.Client = client
	r := NewRouteReconciler()
	if err := r.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	ep := sampleEndpoint()
	client.addErr = errors.New("injected AddPath failure")
	if err := r.Apply(ctx, planType2(t, r, ep)[0], adapter); !errors.Is(err, client.addErr) {
		t.Fatalf("AddPath error not propagated: %v", err)
	}
	client.addErr = nil
	applyType2(t, r, adapter, planType2(t, r, ep))
	receipts := r.AppliedRoutes()
	if _, err := uuid.Parse(receipts[0].PathUUID); err != nil {
		t.Fatalf("not a GoBGP UUID: %v", err)
	}
	t.Logf("acknowledged GoBGP UUID: %s", receipts[0].PathUUID)

	restarted := NewRouteReconciler()
	if err := restarted.Resync(ctx, adapter, receipts); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("GoBGP v4.5.0 RIB without UUID must fail closed, got %v", err)
	}
	t.Log("GoBGP v4.5.0 ListPath omits UUID: restart adoption explicitly refused")
	unknown := NewRouteReconciler()
	if err := unknown.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := unknown.Reconcile([]LocalEndpoint{ep}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("unclaimed UUID adopted: %v", err)
	}

	foreign := ep
	foreign.RouteDistinguisher = "65000:9999"
	foreignRoute, err := BuildType2Route(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignID, err := adapter.Add(ctx, foreignRoute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Add(ctx, foreignRoute); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("foreign local route overwritten: %v", err)
	}
	client.deleteErr = errors.New("injected DeletePath failure")
	if err := r.Apply(ctx, planType2(t, r)[0], adapter); !errors.Is(err, client.deleteErr) {
		t.Fatalf("DeletePath error not propagated: %v", err)
	}
	client.deleteErr = nil
	applyType2(t, r, adapter, planType2(t, r))
	paths, err := adapter.Snapshot(ctx)
	if err != nil || len(paths) != 1 || paths[0].NLRI == nil || !paths[0].NLRI.Equal(foreignRoute.NLRI) {
		t.Fatalf("foreign path not preserved: paths=%v err=%v", paths, err)
	}
	if err := adapter.Delete(ctx, receipts[0]); err != nil {
		t.Fatalf("retry exact already-absent UUID: %v", err)
	}
	if err := adapter.Delete(ctx, OwnedType2Route{Endpoint: foreign}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("empty UUID accepted: %v", err)
	}
	if err := adapter.Delete(ctx, OwnedType2Route{Endpoint: foreign, PathUUID: foreignID}); err != nil {
		t.Fatal(err)
	}
	t.Log("retry, exact UUID withdrawal, foreign-path preservation and fail-closed restart passed")
}

func TestType2GoBGPOldUUIDCannotDeleteReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, adapter := newType2TestSpeaker(t)
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	r := NewRouteReconciler()
	if err := r.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	ep := sampleEndpoint()
	applyType2(t, r, adapter, planType2(t, r, ep))
	old := r.AppliedRoutes()[0]
	next := ep
	next.Incarnation = "inc-2"
	deltas := planType2(t, r, next)
	applyType2(t, r, adapter, deltas)
	newID := r.AppliedRoutes()[0].PathUUID
	if newID == old.PathUUID {
		t.Fatal("replacement reused an old path UUID")
	}
	// Even bypassing the planner, the adapter cannot delete the replacement UUID.
	if err := adapter.Delete(ctx, old); err == nil {
		t.Fatal("GoBGP accepted an obsolete UUID")
	}
	paths, err := adapter.Snapshot(ctx)
	if err != nil || len(paths) != 1 {
		t.Fatalf("old UUID deleted replacement: paths=%v err=%v", paths, err)
	}
	applyType2(t, r, adapter, planType2(t, r))
}

func TestType2GoBGPDomainConversionsPreserveRouteValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*LocalEndpoint)
	}{
		{name: "ipv4"},
		{name: "ipv6", edit: func(ep *LocalEndpoint) { ep.IPAddresses = []string{"2001:db8::10"} }},
		{name: "mac-only", edit: func(ep *LocalEndpoint) { ep.MACOnly, ep.IPAddresses = true, nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := sampleEndpoint()
			if tc.edit != nil {
				tc.edit(&ep)
			}
			route, err := BuildType2Route(ep)
			if err != nil {
				t.Fatal(err)
			}
			route.PathAttributes.ExtendedCommunities = append(route.PathAttributes.ExtendedCommunities,
				Type2ExtendedCommunity{Kind: Type2MACMobilityCommunity, Sequence: 7, Sticky: true})
			nlri, attrs, err := type2RouteToGoBGP(route)
			if err != nil {
				t.Fatal(err)
			}
			gotNLRI, err := type2NLRIFromGoBGP(nlri)
			if err != nil {
				t.Fatal(err)
			}
			gotAttrs, err := type2PathAttributesFromGoBGP(attrs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotNLRI, route.NLRI) || !reflect.DeepEqual(gotAttrs, route.PathAttributes) {
				t.Fatalf("GoBGP round trip changed route values: NLRI=%+v attrs=%+v", gotNLRI, gotAttrs)
			}
		})
	}
}

func TestType2GoBGPConversionRejectsIncompleteObservations(t *testing.T) {
	if _, err := type2NLRIFromGoBGP(nil); err == nil {
		t.Fatal("nil Type-2 observation accepted")
	}
	route, err := BuildType2Route(sampleEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	valid, _, err := type2RouteToGoBGP(route)
	if err != nil {
		t.Fatal(err)
	}
	malformed := *valid
	malformedData := *(valid.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute))
	malformedData.IPAddressLength = 24
	malformed.RouteTypeData = &malformedData
	if _, err := type2NLRIFromGoBGP(&malformed); err == nil {
		t.Fatal("inconsistent GoBGP IP length accepted")
	}
	route.NLRI.Labels = nil
	if _, _, err := type2RouteToGoBGP(route); err == nil {
		t.Fatal("Type-2 route without labels accepted")
	}
	want, err := BuildType2Route(sampleEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	observed := Type2RIBPath{NLRI: &Type2NLRI{
		RD: want.NLRI.RD, ESI: want.NLRI.ESI, EthernetTag: want.NLRI.EthernetTag,
		MAC: want.NLRI.MAC, IP: want.NLRI.IP, Labels: []uint32{want.Endpoint.VNI + 1},
	}, PathAttributes: want.PathAttributes}
	if ribPathMatchesEndpoint(observed, want.Endpoint) {
		t.Fatal("incomplete/mismatched route attributes matched an owned endpoint")
	}
	observed.NLRI = &want.NLRI
	observed.PathAttributes = Type2PathAttributes{}
	if ribPathMatchesEndpoint(observed, want.Endpoint) {
		t.Fatal("missing required path attributes matched an owned endpoint")
	}
}

func TestType2WirePayloadMatchesLegacyGoBGPEncoding(t *testing.T) {
	endpoints := []LocalEndpoint{sampleEndpoint()}
	ipv6 := sampleEndpoint()
	ipv6.IPAddresses = []string{"2001:db8::10"}
	endpoints = append(endpoints, ipv6)
	macOnly := sampleEndpoint()
	macOnly.MACOnly, macOnly.IPAddresses = true, nil
	endpoints = append(endpoints, macOnly)
	for _, endpoint := range endpoints {
		gotNLRI, gotAttrs, err := type2WirePayload(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		wantNLRI, wantAttrs := legacyType2WirePayload(t, endpoint)
		if !bytes.Equal(gotNLRI, wantNLRI) || !reflect.DeepEqual(gotAttrs, wantAttrs) {
			t.Fatalf("durable wire payload changed for MAC-only=%t", endpoint.MACOnly)
		}
	}
}

func legacyType2WirePayload(t *testing.T, endpoint LocalEndpoint) ([]byte, [][]byte) {
	t.Helper()
	rd, err := bgp.ParseRouteDistinguisher(endpoint.RouteDistinguisher)
	if err != nil {
		t.Fatal(err)
	}
	var nlri *bgp.EVPNNLRI
	if endpoint.MACOnly {
		mac, err := net.ParseMAC(endpoint.MACAddress)
		if err != nil {
			t.Fatal(err)
		}
		nlri = bgp.NewEVPNNLRI(bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT, &bgp.EVPNMacIPAdvertisementRoute{
			RD: rd, MacAddressLength: 48, MacAddress: mac, Labels: []uint32{endpoint.VNI},
		})
	} else {
		ip, err := netip.ParseAddr(primaryIP(endpoint.IPAddresses))
		if err != nil {
			t.Fatal(err)
		}
		nlri, err = bgp.NewEVPNMacIPAdvertisementRoute(rd, bgp.EthernetSegmentIdentifier{}, 0,
			endpoint.MACAddress, ip, []uint32{endpoint.VNI})
		if err != nil {
			t.Fatal(err)
		}
	}
	communities := make([]bgp.ExtendedCommunityInterface, 0, len(endpoint.RouteTargets)+1)
	for _, value := range endpoint.RouteTargets {
		community, err := bgp.ParseRouteTarget(value)
		if err != nil {
			t.Fatal(err)
		}
		communities = append(communities, community)
	}
	communities = append(communities, &bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_VXLAN})
	nextHop, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(endpoint.LocalVTEP))
	if err != nil {
		t.Fatal(err)
	}
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP),
		nextHop,
		bgp.NewPathAttributeExtendedCommunities(communities),
	}
	wireNLRI, err := nlri.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	wireAttrs := make([][]byte, 0, len(attrs))
	for _, attr := range attrs {
		encoded, err := attr.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		wireAttrs = append(wireAttrs, encoded)
	}
	return wireNLRI, wireAttrs
}

func TestType2GoBGPForeignReplacementPreserved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, adapter := newType2TestSpeaker(t)
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1,
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewRouteReconciler()
	if err := r.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	ep := sampleEndpoint()
	applyType2(t, r, adapter, planType2(t, r, ep))
	old := r.AppliedRoutes()[0]
	route, _ := BuildType2Route(ep)
	// Simulate a different API writer replacing this NLRI outside our adapter.
	nlri, attrs, err := type2RouteToGoBGP(route)
	if err != nil {
		t.Fatal(err)
	}
	path, err := apiutil.NewPath(bgp.RF_EVPN, nlri, false, attrs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response, err := adapter.Client.AddPath(ctx, &api.AddPathRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := uuid.FromBytes(response.Uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(ctx, old); err == nil {
		t.Fatal("GoBGP accepted old UUID after foreign replacement")
	}
	paths, err := adapter.Snapshot(ctx)
	if err != nil || len(paths) != 1 {
		t.Fatalf("foreign replacement deleted: %v %v", paths, err)
	}
	if err := r.Resync(ctx, adapter, []OwnedType2Route{old}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("stale receipt adopted a foreign replacement: %v", err)
	}
	if _, err := adapter.Client.DeletePath(ctx, &api.DeletePathRequest{Uuid: newID[:]}); err != nil {
		t.Fatalf("foreign UUID was not preserved: %v", err)
	}
}

func TestType2GoBGPLostReplies(t *testing.T) {
	for _, operation := range []string{"add", "delete"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, adapter := newType2TestSpeaker(t)
			if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
				Asn: 65000, RouterId: "192.0.2.1", ListenPort: -1,
			}}); err != nil {
				t.Fatal(err)
			}
			client := &failingType2Client{GoBgpServiceClient: adapter.Client}
			adapter.Client = client
			r := NewRouteReconciler()
			if err := r.Resync(ctx, adapter, nil); err != nil {
				t.Fatal(err)
			}
			ep := sampleEndpoint()
			lostReply := errors.New("server committed operation but reply was lost")
			if operation == "add" {
				client.addReplyErr = lostReply
				delta := planType2(t, r, ep)[0]
				if err := r.Apply(ctx, delta, adapter); !errors.Is(err, lostReply) {
					t.Fatalf("lost AddPath reply: %v", err)
				}
				client.addReplyErr = nil
				if len(r.AppliedRoutes()) != 0 {
					t.Fatal("unacknowledged AddPath invented ownership")
				}
				if err := r.Apply(ctx, delta, adapter); !errors.Is(err, ErrRouteOwnership) {
					t.Fatalf("ambiguous AddPath silently adopted/overwritten: %v", err)
				}
				paths, err := adapter.Snapshot(ctx)
				if err != nil || len(paths) != 1 {
					t.Fatalf("ambiguous AddPath was corrupted: %v %v", paths, err)
				}
				return
			}
			applyType2(t, r, adapter, planType2(t, r, ep))
			client.deleteReplyErr = lostReply
			delta := planType2(t, r)[0]
			if err := r.Apply(ctx, delta, adapter); !errors.Is(err, lostReply) {
				t.Fatalf("lost DeletePath reply: %v", err)
			}
			if len(r.AppliedRoutes()) != 1 {
				t.Fatal("unacknowledged DeletePath changed applied state")
			}
			client.deleteReplyErr = nil
			if err := r.Apply(ctx, delta, adapter); err != nil {
				t.Fatalf("successful RIB absence check did not resolve DeletePath retry: %v", err)
			}
			if len(r.AppliedRoutes()) != 0 {
				t.Fatal("confirmed absence did not clear applied state")
			}
		})
	}
}
