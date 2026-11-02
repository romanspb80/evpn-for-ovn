package evpn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The fixture alone writes to its remote speaker. The receiver connection
// rejects all non-read RPCs, independently of the importer's narrower interface.
func TestRemoteType2Import_Live(t *testing.T) {
	if os.Getenv("EVPN_IMPORT_LIVE") != "disposable" {
		t.Skip("requires disposable k8gobgp receiver and test-owned remote BGP peer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var mu sync.Mutex
	methods := map[string]int{}
	record := func(method string) error {
		mu.Lock()
		defer mu.Unlock()
		name := method[strings.LastIndex(method, "/")+1:]
		if name != "GetBgp" && name != "ListPath" {
			return fmt.Errorf("read-only receiver rejected RPC %s", method)
		}
		methods[name]++
		return nil
	}
	conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if err := record(method); err != nil {
				return err
			}
			return invoke(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			if err := record(method); err != nil {
				return nil, err
			}
			return streamer(ctx, desc, cc, method, opts...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	source := &GoBGPRemoteRIBSource{Client: client, Gate: SpeakerReadiness{Client: client, ASN: 64512, RouterID: os.Getenv("EVPN_IMPORT_LOCAL_VTEP")}}
	config := importConfig()
	config.LocalVTEP = os.Getenv("EVPN_IMPORT_LOCAL_VTEP")
	importer, err := NewReadOnlyType2Importer(source, config)
	if err != nil {
		t.Fatal(err)
	}
	readReceipts := func() []OwnedType2Route {
		t.Helper()
		b, err := os.ReadFile("/run/evpn-node-agent/export-status.json")
		if err != nil {
			t.Fatal(err)
		}
		var status struct {
			Error  string
			Result ExportResult
		}
		if err := json.Unmarshal(b, &status); err != nil {
			t.Fatal(err)
		}
		if status.Error != "" || status.Result.OwnershipUnverified || len(status.Result.Applied) != 2 {
			t.Fatalf("local exporter not healthy: %s", b)
		}
		return status.Result.Applied
	}
	before := readReceipts()
	localRIB := func() string {
		t.Helper()
		s, err := source.ReadRemoteRIB(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, p := range s.Paths {
			if p.NeighborIp == "" && !p.IsFromExternal {
				keys = append(keys, remoteHash(struct {
					NLRI  string
					Attrs [][]byte
				}{fmt.Sprintf("%x", p.NlriBinary), p.PattrsBinary}))
			}
		}
		// All locally owned paths plus the foreign local control must survive.
		if len(keys) != 3 {
			t.Fatalf("local RIB=%d want 3", len(keys))
		}
		sort.Strings(keys)
		return remoteHash(keys)
	}
	originalLocal := localRIB()
	remote := server.NewBgpServer()
	go remote.Serve()
	defer remote.Stop()
	port := reserveTCPPort(t)
	if err := remote.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 64513, RouterId: "192.0.2.13", ListenPort: port, ListenAddresses: []string{"127.0.0.2"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := remote.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: "127.0.0.2", RemotePort: 1179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		AfiSafis: []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true},
			AddPaths: &api.AddPaths{Config: &api.AddPathsConfig{SendMax: 4}}}},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEstablished(t, remote, 64512, 30*time.Second)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, d := range info.Deps {
			if d.Path == "github.com/osrg/gobgp/v4" {
				t.Logf("remote upstream speaker module=%s %s", d.Path, d.Version)
			}
		}
	}
	t.Log("actual EVPN BGP session established: upstream remote 64513/127.0.0.2 -> k8gobgp 64512/127.0.0.1:1179; next-hop explicitly 192.0.2.80")
	add := func(p *api.Path, id uint32) uuid.UUID {
		t.Helper()
		n, err := apiutil.GetNativeNlri(p)
		if err != nil {
			t.Fatal(err)
		}
		a, err := apiutil.GetNativePathAttributes(p)
		if err != nil {
			t.Fatal(err)
		}
		out, err := remote.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{
			Family: bgp.RF_EVPN, Nlri: n, Attrs: a, Age: time.Now().Unix(), RemoteID: id,
		}}})
		if err != nil || len(out) != 1 || out[0].Error != nil {
			t.Fatalf("remote fixture AddPath: %v %v", out, err)
		}
		return out[0].UUID
	}
	del := func(id uuid.UUID) {
		t.Helper()
		if err := remote.DeletePath(apiutil.DeletePathRequest{UUIDs: []uuid.UUID{id}}); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(label string, count, routes, issues int) RemoteImportSnapshot {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		var s RemoteImportSnapshot
		var err error
		for time.Now().Before(deadline) {
			s, err = importer.Poll(ctx)
			if err == nil && s.Authoritative && len(s.Endpoints) == count && len(s.Issues) == issues &&
				(count == 0 || len(s.Endpoints[0].Routes) == routes) {
				b, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s=PASS snapshot=%s", label, b)
				return s
			}
			time.Sleep(200 * time.Millisecond)
		}
		raw, readErr := source.ReadRemoteRIB(ctx)
		for _, p := range raw.Paths {
			r, e := DecodeRemoteType2(p)
			t.Logf("receiver path=%+v decode=%v raw=%s", r, e, p)
		}
		t.Fatalf("%s timeout: %+v poll=%v read=%v", label, s, err, readErr)
		return s
	}
	wait("local-paths-excluded", 0, 0, 0)
	id := add(importPath(t, nil), 1)
	first := wait("remote-advertisement-decoded", 1, 1, 0)
	r := first.Endpoints[0].Routes[0].Route
	if r.NextHop != "192.0.2.80" || r.Source.NeighborIP != "127.0.0.2" || r.Source.SourceASN != 64513 ||
		r.RD != "64513:200" || r.Labels[0] != 100 || r.Source.SourceID != "192.0.2.13" {
		t.Fatalf("not proven learned remote route: %+v", r)
	}
	same := wait("repeated-observation-idempotent", 1, 1, 0)
	if !reflect.DeepEqual(first, same) {
		t.Fatal("repeated observation changed desired state")
	}
	secondID := add(importPath(t, nil), 2)
	multi := wait("actual-addpaths-multiple-paths", 1, 2, 0)
	if multi.Endpoints[0].Routes[0].Route.Source.Key == multi.Endpoints[0].Routes[1].Route.Source.Key {
		t.Fatal("lost exposed path identities")
	}
	del(secondID)
	wait("one-path-withdrawal-preserves-endpoint", 1, 1, 0)

	if err := os.WriteFile("/run/evpn-node-agent/import-api-outage-request", []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := os.Stat("/run/evpn-node-agent/import-api-outage-paused")
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatalf("API pause handshake: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	outageCtx, outageCancel := context.WithTimeout(ctx, 3*time.Second)
	frozen, outageErr := importer.Poll(outageCtx)
	outageCancel()
	if outageErr == nil || frozen.Authoritative || len(frozen.Endpoints) != 1 {
		t.Fatalf("actual API outage discarded desired state: %+v %v", frozen, outageErr)
	}
	t.Logf("actual-speaker-API-outage-retains-uncertain-snapshot=PASS error=%v generation=%d", outageErr, frozen.Generation)
	if err := os.WriteFile("/run/evpn-node-agent/import-api-outage-observed", []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	wait("actual-speaker-API-restored", 1, 1, 0)

	// Force a real transport failure by closing only this reader's connection,
	// then replace it with a newly established channel. The speaker stays alive.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := importer.Poll(ctx); err == nil || s.Authoritative || len(s.Endpoints) != 1 {
		t.Fatalf("outage lost confirmed state: %+v %v", s, err)
	}
	reconnected, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if err := record(method); err != nil {
				return err
			}
			return invoke(ctx, method, req, reply, cc, opts...)
		}), grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			if err := record(method); err != nil {
				return nil, err
			}
			return streamer(ctx, desc, cc, method, opts...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	source.Client = api.NewGoBgpServiceClient(reconnected)
	source.Gate = SpeakerReadiness{Client: api.NewGoBgpServiceClient(reconnected), ASN: 64512, RouterID: config.LocalVTEP}
	wait("real-grpc-reconnect-revalidated", 1, 1, 0)
	del(id)
	wait("remote-withdrawal", 0, 0, 0)

	for _, scenario := range []struct {
		name string
		edit func(*bgp.EVPNMacIPAdvertisementRoute, *[]bgp.PathAttributeInterface)
	}{
		{"nonmatching-import-rt", func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
			rt, _ := bgp.ParseRouteTarget("64513:999")
			(*a)[2] = bgp.NewPathAttributeExtendedCommunities([]bgp.ExtendedCommunityInterface{rt, &bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_VXLAN}})
		}},
		{"disallowed-vtep", func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
			(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.90"))
		}},
		{"unmapped-vni", func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) { n.Labels = []uint32{200} }},
		{"nonzero-esi-quarantined", func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.ESI = bgp.EthernetSegmentIdentifier{Value: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}}
		}},
		{"unsupported-encapsulation", func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
			rt, _ := bgp.ParseRouteTarget("64512:100")
			(*a)[2] = bgp.NewPathAttributeExtendedCommunities([]bgp.ExtendedCommunityInterface{rt, &bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_MPLS}})
		}},
		{"missing-encapsulation", func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
			rt, _ := bgp.ParseRouteTarget("64512:100")
			(*a)[2] = bgp.NewPathAttributeExtendedCommunities([]bgp.ExtendedCommunityInterface{rt})
		}},
		{"ipv6-endpoint-quarantined", func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.IPAddressLength, n.IPAddress = 128, netip.MustParseAddr("2001:db8::80")
		}},
		{"multicast-mac-quarantined", func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.MacAddress = net.HardwareAddr{1, 0, 0x5e, 0, 0, 1}
		}},
	} {
		badID := add(importPath(t, scenario.edit), 1)
		wait(scenario.name, 0, 0, 1)
		del(badID)
		wait(scenario.name+"-withdrawn", 0, 0, 0)
	}
	macID := add(importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.IPAddressLength = 0
		n.IPAddress = netip.Addr{}
	}), 1)
	if s := wait("optional-ip-mac-only", 1, 1, 0); s.Endpoints[0].IP != "" {
		t.Fatal("MAC-only invented IP")
	}
	del(macID)
	wait("mac-only-withdrawn", 0, 0, 0)
	id = add(importPath(t, nil), 1)
	base := wait("replacement-before", 1, 1, 0)
	// Same remote path ID/NLRI: an actual BGP attribute replacement, no receiver AddPath.
	id = add(importPath(t, func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
		(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
	}), 1)
	deadline = time.Now().Add(25 * time.Second)
	var replaced RemoteImportSnapshot
	for time.Now().Before(deadline) {
		replaced = wait("replacement-observation", 1, 1, 0)
		if replaced.Endpoints[0].RemoteVTEP == "192.0.2.81" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if replaced.Endpoints[0].RemoteVTEP != "192.0.2.81" || replaced.Endpoints[0].Key != base.Endpoints[0].Key {
		t.Fatal("replacement did not converge")
	}
	conflictID := add(importPath(t, nil), 2)
	deadline = time.Now().Add(25 * time.Second)
	foundConflict := false
	for time.Now().Before(deadline) {
		s, err := importer.Poll(ctx)
		if err != nil && strings.Contains(err.Error(), "conflicting remote VTEPs") && !s.Authoritative &&
			len(s.Endpoints) == 1 && s.Endpoints[0].RemoteVTEP == "192.0.2.81" {
			foundConflict = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !foundConflict {
		t.Fatal("conflicting real multipath silently replaced desired state")
	}
	t.Log("conflicting-real-multipath-retains-nonauthoritative-confirmed-state=PASS")
	del(conflictID)
	wait("conflict-cleared", 1, 1, 0)
	del(id)
	wait("final-withdrawal", 0, 0, 0)
	if !reflect.DeepEqual(before, readReceipts()) || originalLocal != localRIB() {
		t.Fatal("local exporter receipts or local RIB changed")
	}
	mu.Lock()
	t.Logf("receiver-RPC-audit=%v; AddPath=0 DeletePath=0 configuration-RPCs=0", methods)
	mu.Unlock()
	t.Log("local-exporter-confirmed-UUIDs-and-local-RIB-preserved=PASS")
}
