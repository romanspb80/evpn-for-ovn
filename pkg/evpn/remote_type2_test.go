package evpn

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func importConfig() RemoteImportConfig {
	return RemoteImportConfig{LocalVTEP: "192.0.2.32", Networks: []RemoteImportNetwork{{
		NetworkID: "remote-test", VNI: 100, ImportRouteTargets: []string{"64512:100"},
		AllowedVTEPs: []string{"192.0.2.80", "192.0.2.81"},
	}}}
}

func importPath(t *testing.T, mutate func(*bgp.EVPNMacIPAdvertisementRoute, *[]bgp.PathAttributeInterface)) *api.Path {
	t.Helper()
	ep := sampleEndpoint()
	ep.RouteDistinguisher, ep.RouteTargets, ep.VNI, ep.LocalVTEP = "64513:200", []string{"64512:100"}, 100, "192.0.2.80"
	route, err := BuildType2Route(ep)
	if err != nil {
		t.Fatal(err)
	}
	nativeNLRI, attrs, err := type2RouteToGoBGP(route)
	if err != nil {
		t.Fatal(err)
	}
	nativeRoute := nativeNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if mutate != nil {
		mutate(nativeRoute, &attrs)
	}
	nativeNLRI.Length = uint8(nativeRoute.Len())
	for idx, attr := range attrs {
		if hop, ok := attr.(*bgp.PathAttributeNextHop); ok {
			attrs[idx], err = bgp.NewPathAttributeMpReachNLRI(bgp.RF_EVPN, []bgp.PathNLRI{{NLRI: nativeNLRI}}, hop.Value)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	p, err := apiutil.NewPath(bgp.RF_EVPN, nativeNLRI, false, attrs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !nativeRoute.IPAddress.IsValid() {
		p.NlriBinary, err = nativeNLRI.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range attrs {
			value, err := a.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			p.PattrsBinary = append(p.PattrsBinary, value)
		}
	}
	p.NeighborIp, p.SourceId, p.SourceAsn, p.IsFromExternal = "127.0.0.2", "192.0.2.13", 64513, true
	return p
}

func TestRemoteType2DecodePolicy(t *testing.T) {
	valid := importPath(t, nil)
	r, err := DecodeRemoteType2(valid)
	if err != nil {
		t.Fatal(err)
	}
	if r.RD != "64513:200" || r.NextHop != "192.0.2.80" || r.Source.NeighborIP == r.NextHop || r.ESI != "00000000000000000000" ||
		r.Source.SourceASN != 64513 || r.Source.Key == "" || !reflect.DeepEqual(r.RouteTargets, []string{"64512:100"}) {
		t.Fatalf("%+v", r)
	}
	if _, err := importConfig().ValidateRoute(r); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*api.Path)
	}{
		{"family", func(p *api.Path) { p.Family.Afi = api.Family_AFI_IP }},
		{"missing-nlri", func(p *api.Path) { p.Nlri = nil }},
		{"missing-attrs", func(p *api.Path) { p.Pattrs = nil }},
		{"unknown-source", func(p *api.Path) { p.NeighborIp = ""; p.SourceId = ""; p.IsFromExternal = true }},
		{"ipv6-source", func(p *api.Path) { p.NeighborIp = "broken" }},
		{"duplicate-attr", func(p *api.Path) { p.Pattrs = append(p.Pattrs, p.Pattrs[0]) }},
		{"nil-attr", func(p *api.Path) { p.Pattrs = append(p.Pattrs, nil) }},
		{"invalid-ip", func(p *api.Path) { p.Nlri.GetEvpnMacadv().IpAddress = "not-an-ip" }},
		{"invalid-mac", func(p *api.Path) { p.Nlri.GetEvpnMacadv().MacAddress = "not-a-mac" }},
		{"missing-origin", func(p *api.Path) { p.Pattrs = p.Pattrs[1:] }},
		{"missing-next-hop", func(p *api.Path) { p.Pattrs = append(p.Pattrs[:1], p.Pattrs[2:]...) }},
		{"multiple-next-hops", func(p *api.Path) { p.Pattrs[1].GetMpReach().NextHops = []string{"192.0.2.80", "192.0.2.81"} }},
		{"mismatched-reach-nlri", func(p *api.Path) { p.Pattrs[1].GetMpReach().Nlris[0].GetEvpnMacadv().EthernetTag = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proto.Clone(valid).(*api.Path)
			tc.edit(p)
			if _, err := DecodeRemoteType2(p); err == nil {
				t.Fatal("invalid raw route accepted")
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*DecodedRemoteType2)
	}{
		{"local", func(r *DecodedRemoteType2) { r.Classification = "local" }},
		{"withdrawn", func(r *DecodedRemoteType2) { r.Classification = "withdrawn" }},
		{"type5", func(r *DecodedRemoteType2) { r.RouteType = 5 }},
		{"rd", func(r *DecodedRemoteType2) { r.RD = "bad" }},
		{"rt", func(r *DecodedRemoteType2) { r.RouteTargets = []string{"64513:200"} }},
		{"vni", func(r *DecodedRemoteType2) { r.Labels = []uint32{200} }},
		{"vni-range", func(r *DecodedRemoteType2) { r.Labels = []uint32{16777216} }},
		{"two-labels", func(r *DecodedRemoteType2) { r.Labels = []uint32{100, 200} }},
		{"esi", func(r *DecodedRemoteType2) { r.ESI = "01000000000000000000" }},
		{"tag", func(r *DecodedRemoteType2) { r.EthernetTag = 1 }},
		{"multicast-mac", func(r *DecodedRemoteType2) { r.MAC = "01:00:5e:00:00:01" }},
		{"zero-mac", func(r *DecodedRemoteType2) { r.MAC = "00:00:00:00:00:00" }},
		{"eui64", func(r *DecodedRemoteType2) { r.MAC = "02:00:00:00:00:00:00:01" }},
		{"ipv6", func(r *DecodedRemoteType2) { r.IP = "2001:db8::1" }},
		{"multicast-ip", func(r *DecodedRemoteType2) { r.IP = "224.0.0.1" }},
		{"missing-hop", func(r *DecodedRemoteType2) { r.NextHop = "" }},
		{"local-hop", func(r *DecodedRemoteType2) { r.NextHop = "192.0.2.32" }},
		{"disallowed-hop", func(r *DecodedRemoteType2) { r.NextHop = "192.0.2.90" }},
		{"stale", func(r *DecodedRemoteType2) { r.Stale = true }},
		{"filtered", func(r *DecodedRemoteType2) { r.Filtered = true }},
		{"invalid-hop-flag", func(r *DecodedRemoteType2) { r.NexthopInvalid = true }},
		{"no-encap", func(r *DecodedRemoteType2) { r.Encapsulation = nil }},
		{"mpls", func(r *DecodedRemoteType2) { r.Encapsulation = []string{"mpls"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := DecodeRemoteType2(valid)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&r)
			if _, err := importConfig().ValidateRoute(r); err == nil {
				t.Fatal("policy accepted invalid route")
			}
		})
	}
	macOnly := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.IPAddressLength = 0
		n.IPAddress = netip.Addr{}
	})
	if r, err := DecodeRemoteType2(macOnly); err != nil {
		t.Fatal(err)
	} else if _, err := importConfig().ValidateRoute(r); err != nil || r.IP != "" {
		t.Fatalf("MAC-only: %+v %v", r, err)
	}
	local := proto.Clone(valid).(*api.Path)
	local.NeighborIp = ""
	local.IsFromExternal = false
	if r, err := DecodeRemoteType2(local); err != nil || r.Classification != "local" {
		t.Fatalf("%+v %v", r, err)
	}
	withdraw := proto.Clone(valid).(*api.Path)
	withdraw.IsWithdraw = true
	withdraw.Pattrs = nil
	if r, err := DecodeRemoteType2(withdraw); err != nil || r.Classification != "withdrawn" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRemoteImportConfiguration(t *testing.T) {
	for _, edit := range []func(*RemoteImportConfig){
		func(c *RemoteImportConfig) { c.LocalVTEP = "" },
		func(c *RemoteImportConfig) { c.Networks[0].VNI = 0 },
		func(c *RemoteImportConfig) { c.Networks[0].VNI = 16777216 },
		func(c *RemoteImportConfig) { c.Networks[0].ImportRouteTargets = []string{"bad"} },
		func(c *RemoteImportConfig) { c.Networks[0].AllowedVTEPs = []string{c.LocalVTEP} },
		func(c *RemoteImportConfig) { c.Networks = append(c.Networks, c.Networks[0]) },
		func(c *RemoteImportConfig) { c.Networks[0].AllowedVTEPs = nil },
		func(c *RemoteImportConfig) { c.Networks[0].ImportRouteTargets = nil },
	} {
		c := importConfig()
		edit(&c)
		if _, err := NewReadOnlyType2Importer(&importTestSource{}, c); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

type importTestSource struct {
	snap RemoteRIBSnapshot
	err  error
}

func (s *importTestSource) ReadRemoteRIB(context.Context) (RemoteRIBSnapshot, error) {
	return s.snap, s.err
}

func confirmedImport(t *testing.T, i *ReadOnlyType2Importer) RemoteImportSnapshot {
	t.Helper()
	if _, err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := i.Poll(context.Background())
	if err != nil || !s.Authoritative {
		t.Fatalf("%+v %v", s, err)
	}
	return s
}

func TestReadOnlyType2ImporterSnapshots(t *testing.T) {
	p := importPath(t, nil)
	source := &importTestSource{snap: RemoteRIBSnapshot{Paths: []*api.Path{p}, Complete: true}}
	i, err := NewReadOnlyType2Importer(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := i.Poll(context.Background())
	if err != nil || pending.Authoritative || len(pending.Endpoints) != 0 {
		t.Fatalf("%+v %v", pending, err)
	}
	first := confirmedImport(t, i)
	if len(first.Endpoints) != 1 || first.Generation != 1 {
		t.Fatalf("%+v", first)
	}
	epKey := first.Endpoints[0].Key
	first.Endpoints[0].Routes[0].Route.RouteTargets[0] = "mutated-copy"
	if next := confirmedImport(t, i); next.Endpoints[0].Routes[0].Route.RouteTargets[0] != "64512:100" || next.Generation != 1 {
		t.Fatal("snapshot alias or non-idempotent")
	}
	source.snap.Paths = append(source.snap.Paths, proto.Clone(p).(*api.Path))
	if next := confirmedImport(t, i); next.Generation != 1 || len(next.Endpoints[0].Routes) != 1 {
		t.Fatal("duplicate observation")
	}
	other := proto.Clone(p).(*api.Path)
	other.NeighborIp = "127.0.0.3"
	other.Identifier = 2
	source.snap.Paths = append(source.snap.Paths, other)
	multi := confirmedImport(t, i)
	if len(multi.Endpoints[0].Routes) != 2 {
		t.Fatal("multipath collapsed")
	}
	source.snap.Paths = []*api.Path{other, p}
	if next := confirmedImport(t, i); !reflect.DeepEqual(next, multi) {
		t.Fatal("order-dependent snapshot")
	}
	source.err = errors.New("temporary transport failure")
	if next, err := i.Poll(context.Background()); err == nil || next.Authoritative || len(next.Endpoints) != 1 {
		t.Fatal("error removed confirmed state")
	}
	source.err = nil
	source.snap.Complete = false
	if next, err := i.Poll(context.Background()); err == nil || next.Authoritative || len(next.Endpoints) != 1 {
		t.Fatal("incomplete snapshot accepted")
	}
	source.snap.Complete = true
	pending, err = i.Poll(context.Background())
	if err != nil || pending.Authoritative {
		t.Fatal("reconnect published without confirmation")
	}
	confirmedImport(t, i)
	source.snap.Paths = []*api.Path{importPath(t, func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
		(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
	})}
	next := confirmedImport(t, i)
	if next.Endpoints[0].Key != epKey || next.Endpoints[0].RemoteVTEP != "192.0.2.81" {
		t.Fatal("bad replacement")
	}
	source.snap.Paths = nil
	if pending, err := i.Poll(context.Background()); err != nil || pending.Authoritative || len(pending.Endpoints) != 1 {
		t.Fatal("unconfirmed disappearance")
	}
	if next := confirmedImport(t, i); len(next.Endpoints) != 0 {
		t.Fatal("confirmed withdrawal retained")
	}
}

func TestReadOnlyType2ImporterMergesMACOnlyAndMACIP(t *testing.T) {
	for _, macOnlyFirst := range []bool{false, true} {
		name := "mac-ip-first"
		if macOnlyFirst {
			name = "mac-only-first"
		}
		t.Run(name, func(t *testing.T) {
			macIP := importPath(t, nil)
			macOnly := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
				n.IPAddressLength = 0
				n.IPAddress = netip.Addr{}
			})
			ipRoute, err := DecodeRemoteType2(macIP)
			if err != nil {
				t.Fatal(err)
			}
			paths := []*api.Path{macIP, macOnly}
			if macOnlyFirst {
				paths = []*api.Path{macOnly, macIP}
			}
			source := &importTestSource{snap: RemoteRIBSnapshot{Paths: paths, Complete: true}}
			importer, err := NewReadOnlyType2Importer(source, importConfig())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := confirmedImport(t, importer)
			if len(snapshot.Endpoints) != 1 {
				t.Fatalf("expected one endpoint for both Type-2 forms: %+v", snapshot.Endpoints)
			}
			endpoint := snapshot.Endpoints[0]
			if endpoint.MAC != ipRoute.MAC || endpoint.IP != ipRoute.IP || endpoint.RemoteVTEP != ipRoute.NextHop || len(endpoint.Routes) != 2 {
				t.Fatalf("MAC-only and MAC/IP routes were not merged: %+v", endpoint)
			}
			seenMACOnly, seenMACIP := false, false
			for _, route := range endpoint.Routes {
				seenMACOnly = seenMACOnly || route.Route.IP == ""
				seenMACIP = seenMACIP || route.Route.IP == ipRoute.IP
			}
			if !seenMACOnly || !seenMACIP {
				t.Fatalf("merged endpoint lost one advertised route form: %+v", endpoint.Routes)
			}
		})
	}
}

func TestReadOnlyType2ConflictsAndQuarantine(t *testing.T) {
	for _, change := range []string{"vtep", "ip", "mac", "path-identity", "malformed"} {
		t.Run(change, func(t *testing.T) {
			p := importPath(t, nil)
			source := &importTestSource{snap: RemoteRIBSnapshot{Paths: []*api.Path{p}, Complete: true}}
			i, err := NewReadOnlyType2Importer(source, importConfig())
			if err != nil {
				t.Fatal(err)
			}

			before := confirmedImport(t, i)
			other := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
				switch change {
				case "vtep", "path-identity":
					(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
				case "ip":
					n.IPAddress = netip.MustParseAddr("10.10.0.99")
				case "mac":
					n.MacAddress = net.HardwareAddr{2, 0, 0, 0, 0, 99}
				}
			})
			if change != "path-identity" {
				other.NeighborIp = "127.0.0.3"
			}
			if change == "malformed" {
				other.Nlri = nil
			}
			source.snap.Paths = append(source.snap.Paths, other)
			if next, err := i.Poll(context.Background()); err == nil || next.Authoritative || !reflect.DeepEqual(next.Endpoints, before.Endpoints) {
				t.Fatalf("conflict replaced valid state: %+v %v", next, err)
			}
		})
	}
	p := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.ESI = bgp.EthernetSegmentIdentifier{Value: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}}
	})
	source := &importTestSource{snap: RemoteRIBSnapshot{Paths: []*api.Path{p}, Complete: true}}
	i, err := NewReadOnlyType2Importer(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := confirmedImport(t, i)
	if len(s.Endpoints) != 0 || len(s.Issues) != 1 {
		t.Fatal("nonzero ESI not quarantined")
	}
}

func TestReadOnlyType2UncertainAndRejectedReplacement(t *testing.T) {
	for _, scenario := range []string{"stale", "filtered", "invalid-next-hop", "disallowed-replacement", "unsupported-collision"} {
		t.Run(scenario, func(t *testing.T) {
			p := importPath(t, nil)
			source := &importTestSource{snap: RemoteRIBSnapshot{Paths: []*api.Path{p}, Complete: true}}
			i, err := NewReadOnlyType2Importer(source, importConfig())
			if err != nil {
				t.Fatal(err)
			}
			before := confirmedImport(t, i)
			switch scenario {
			case "stale":
				p.Stale = true
			case "filtered":
				p.Filtered = true
			case "invalid-next-hop":
				p.IsNexthopInvalid = true
			default:
				p = importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
					if scenario == "disallowed-replacement" {
						(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.90"))
					} else {
						n.ESI = bgp.EthernetSegmentIdentifier{Value: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}}
					}
				})
			}
			source.snap.Paths = []*api.Path{p}
			for count := 0; count < 2; count++ {
				got, err := i.Poll(context.Background())
				if err == nil || got.Authoritative || !reflect.DeepEqual(got.Endpoints, before.Endpoints) {
					t.Fatalf("uncertain route replaced confirmed state: %+v %v", got, err)
				}
			}
		})
	}
}

type remoteReadTestServer struct {
	api.UnimplementedGoBgpServiceServer
	mu    sync.Mutex
	paths []*api.Path
	fail  bool
	empty bool
}

func (s *remoteReadTestServer) ListPath(_ *api.ListPathRequest, stream grpc.ServerStreamingServer[api.ListPathResponse]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.empty {
		return stream.Send(&api.ListPathResponse{})
	}
	for _, p := range s.paths {
		if err := stream.Send(&api.ListPathResponse{Destination: &api.Destination{Paths: []*api.Path{p}}}); err != nil {
			return err
		}
	}
	if s.fail {
		return status.Error(codes.Unavailable, "partial stream")
	}
	return nil
}

func TestGoBGPRemoteRIBCompleteStream(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	backend := &remoteReadTestServer{paths: []*api.Path{importPath(t, nil)}, fail: true}
	api.RegisterGoBgpServiceServer(srv, backend)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///read-only", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	source := &GoBGPRemoteRIBSource{Client: api.NewGoBgpServiceClient(conn), Gate: &exportTestGate{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s, err := source.ReadRemoteRIB(ctx); err == nil || s.Complete || len(s.Paths) != 0 || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("%+v %v", s, err)
	}
	backend.mu.Lock()
	backend.fail = false
	backend.mu.Unlock()
	if s, err := source.ReadRemoteRIB(ctx); err != nil || !s.Complete || len(s.Paths) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	source.Gate = &exportTestGate{err: io.ErrUnexpectedEOF}
	if s, err := source.ReadRemoteRIB(ctx); err == nil || s.Complete {
		t.Fatalf("%+v %v", s, err)
	}
	source.Gate = &interruptedExportGate{fail: 2}
	if s, err := source.ReadRemoteRIB(ctx); err == nil || s.Complete || len(s.Paths) != 0 {
		t.Fatalf("readiness loss after EOF published partial state: %+v %v", s, err)
	}
	source.Gate = &exportTestGate{}
	backend.mu.Lock()
	backend.empty = true
	backend.mu.Unlock()
	if s, err := source.ReadRemoteRIB(ctx); err == nil || s.Complete {
		t.Fatalf("incomplete destination accepted: %+v %v", s, err)
	}
}
