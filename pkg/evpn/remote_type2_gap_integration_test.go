package evpn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRemoteType2GapNegatives_Live(t *testing.T) {
	if os.Getenv("EVPN_IMPORT_LIVE") != "disposable" || os.Getenv("EVPN_GAP_CLOSURE") != "1" {
		t.Skip("requires the owned live importer fixture and explicit gap-closure opt-in")
	}
	directory := os.Getenv("EVPN_GAP_EVIDENCE_DIR")
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("an existing owned gap evidence directory is required", err)
	}
	directory, err = os.MkdirTemp(directory, "route-negatives-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("attempt evidence directory:", directory)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	capture := func(label string, value interface{}) {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "gap-negative-"+label+".json"), append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s evidence=%s", label, data)
	}
	conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	config := importConfig()
	source := &GoBGPRemoteRIBSource{Client: client, Gate: SpeakerReadiness{
		Client: client, ASN: 64512, RouterID: os.Getenv("EVPN_IMPORT_LOCAL_VTEP"),
	}}
	reader, err := NewReadOnlyType2ImporterFromSnapshot(source, config)
	if err != nil {
		t.Fatal(err)
	}
	localKeys := func(raw RemoteRIBSnapshot) []string {
		var keys []string
		for _, path := range raw.Paths {
			if path.NeighborIp == "" && !path.IsFromExternal {
				keys = append(keys, remoteHash(struct {
					NLRI  []byte
					Attrs [][]byte
				}{path.NlriBinary, path.PattrsBinary}))
			}
		}
		sort.Strings(keys)
		return keys
	}
	initial, err := source.ReadRemoteRIB(ctx)
	if err != nil || !initial.Complete {
		t.Fatal("live receiver unavailable", err)
	}
	originalLocal := localKeys(initial)
	if len(originalLocal) < 3 {
		t.Fatal("fixture must retain two owned local routes and a foreign local route", originalLocal)
	}
	readLocalReceipts := func() []OwnedType2Route {
		t.Helper()
		data, err := os.ReadFile("/run/evpn-node-agent/export-status.json")
		if err != nil {
			t.Fatal(err)
		}
		var status struct {
			Error  string
			Result ExportResult
		}
		if err := json.Unmarshal(data, &status); err != nil {
			t.Fatal(err)
		}
		if status.Error != "" || status.Result.OwnershipUnverified || len(status.Result.Applied) != 2 {
			t.Fatal("local exporter no longer has two verified applied receipts:", string(data))
		}
		return status.Result.Applied
	}
	originalReceipts := readLocalReceipts()
	remote := server.NewBgpServer()
	go remote.Serve()
	defer remote.Stop()
	if err := remote.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 64513, RouterId: "192.0.2.13", ListenPort: reserveTCPPort(t),
		ListenAddresses: []string{"127.0.0.2"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := remote.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: "127.0.0.2", RemotePort: 1179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		AfiSafis: []*api.AfiSafi{{Config: &api.AfiSafiConfig{
			Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true,
		}, AddPaths: &api.AddPaths{Config: &api.AddPathsConfig{SendMax: 4}}}},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEstablished(t, remote, 64512, 30*time.Second)
	add := func(edit func(*bgp.EVPNMacIPAdvertisementRoute, *[]bgp.PathAttributeInterface), id uint32) uuid.UUID {
		t.Helper()
		path := importPath(t, edit)
		nlri, err := apiutil.GetNativeNlri(path)
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := apiutil.GetNativePathAttributes(path)
		if err != nil {
			t.Fatal(err)
		}
		out, err := remote.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{
			Family: bgp.RF_EVPN, Nlri: nlri, Attrs: attrs, Age: time.Now().Unix(), RemoteID: id,
		}}})
		if err != nil || len(out) != 1 || out[0].Error != nil {
			t.Fatalf("owned remote AddPath: %v %+v", err, out)
		}
		return out[0].UUID
	}
	del := func(id uuid.UUID) {
		t.Helper()
		if err := remote.DeletePath(apiutil.DeletePathRequest{UUIDs: []uuid.UUID{id}}); err != nil {
			t.Fatal(err)
		}
	}
	// Dial/establish the real wire fixture before switching this thread's kernel namespace.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	namespace, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			panic(err)
		}
	}()
	handle, err := netlink.NewHandleAt(namespace, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
		Name: "gap-br", HardwareAddr: mustMAC(t, "02:00:00:00:00:01"),
	}}
	if err := handle.LinkAdd(bridge); err != nil {
		t.Fatal(err)
	}
	if err := handle.LinkSetUp(bridge); err != nil {
		t.Fatal(err)
	}
	addTestLocalVTEP(t, handle)
	vxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
		Name: "gap-vx", MasterIndex: bridge.Index,
	}, VxlanId: 100, Port: 60100, SrcAddr: net.ParseIP("192.0.2.32")}
	if err := handle.LinkAdd(vxlan); err != nil {
		t.Fatal(err)
	}
	if err := handle.LinkSetUp(vxlan); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	backend, err := NewOwnedOVNNetlinkBackend(RemoteNetlinkConfig{
		Enabled: true, ExclusiveWriter: true, StateDirectory: t.TempDir(),
		Networks: []RemoteNetlinkNetwork{{
			NetworkID: "remote-test", LogicalSwitch: "gap-owned-ls", VNI: 100,
			BridgeDevice: "gap-br", VXLANDevice: "gap-vx",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := netns.Set(original); err != nil {
		t.Fatal(err)
	}
	importer, err := NewValidatedRemoteType2Importer(reader, backend)
	if err != nil {
		t.Fatal(err)
	}
	foreignFDB := netlink.Neigh{
		Family: unix.AF_BRIDGE, LinkIndex: vxlan.Index,
		HardwareAddr: mustMAC(t, "02:00:00:00:00:fe"), IP: net.ParseIP("192.0.2.99"),
		State: netlink.NUD_NOARP | netlink.NUD_PERMANENT,
		Flags: netlink.NTF_SELF | netlink.NTF_EXT_LEARNED,
	}
	foreignNeighbor := netlink.Neigh{
		Family: unix.AF_INET, LinkIndex: bridge.Index,
		HardwareAddr: foreignFDB.HardwareAddr, IP: net.ParseIP("10.251.1.254"),
		State: netlink.NUD_PERMANENT,
	}
	for _, entry := range []*netlink.Neigh{&foreignFDB, &foreignNeighbor} {
		if err := handle.NeighAdd(entry); err != nil {
			t.Fatal(err)
		}
	}
	kernel := func() []string {
		t.Helper()
		var result []string
		for _, slot := range [][2]int{{vxlan.Index, unix.AF_BRIDGE}, {bridge.Index, unix.AF_INET}} {
			entries, err := handle.NeighList(slot[0], slot[1])
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				result = append(result, fmt.Sprintf("%d/%d/%s/%s/%d/%d/%d/%d/%d",
					entry.Family, entry.LinkIndex, entry.IP, entry.HardwareAddr, entry.State,
					entry.Flags, entry.FlagsExt, entry.VNI, entry.Vlan))
			}
		}
		sort.Strings(result)
		return result
	}
	type observation struct {
		ObservedAt            string
		RIB                   RemoteRIBSnapshot
		Snapshot              RemoteImportSnapshot
		Receipts              []RemoteNetlinkReceipt
		Kernel                []string
		Error                 string
		LocalExporterReceipts []OwnedType2Route
	}
	observe := func() observation {
		t.Helper()
		reconcileErr := importer.ReconcileOnce(ctx)
		raw, err := source.ReadRemoteRIB(ctx)
		if err != nil || !raw.Complete {
			t.Fatal("receiver snapshot incomplete", err)
		}
		if !reflect.DeepEqual(localKeys(raw), originalLocal) {
			t.Fatal("owned or foreign receiver-local routes changed")
		}
		localReceipts := readLocalReceipts()
		if !reflect.DeepEqual(localReceipts, originalReceipts) {
			t.Fatal("local exporter ownership receipts changed")
		}
		o := observation{
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RIB: raw,
			Snapshot: importer.LastSnapshot(), Receipts: backend.Receipts(), Kernel: kernel(),
			LocalExporterReceipts: localReceipts,
		}
		if reconcileErr != nil {
			o.Error = reconcileErr.Error()
		}
		return o
	}
	wait := func(label string, accept func(observation) bool) observation {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		var last observation
		for time.Now().Before(deadline) {
			last = observe()
			if accept(last) {
				capture(label, last)
				return last
			}
			time.Sleep(150 * time.Millisecond)
		}
		capture(label+"-failed", last)
		t.Fatalf("%s did not meet exact live acceptance assertions", label)
		return last
	}
	healthy := func(o observation) bool {
		return o.Error == "" && o.Snapshot.Authoritative && len(o.Snapshot.Endpoints) == 1 &&
			len(o.Receipts) == 1 && len(o.Snapshot.Issues) == 0 &&
			len(o.Snapshot.Endpoints[0].Routes) == 1
	}
	foreignKernel := kernel()
	validID := add(nil, 1)
	baseline := wait("valid-baseline", healthy)
	if len(baseline.Kernel) < 4 {
		t.Fatal("valid owned plus foreign FDB/neighbor controls not established", baseline.Kernel)
	}
	stable := func(label string, o observation) {
		t.Helper()
		if !reflect.DeepEqual(o.Kernel, baseline.Kernel) || len(o.Receipts) != 1 {
			capture(label+"-unauthorized-mutation", o)
			t.Fatal(label, "changed valid/foreign kernel or ownership state")
		}
		got, want := o.Receipts[0], baseline.Receipts[0]
		// Source-set changes advance incarnation without changing owned kernel slots.
		got.Generation, want.Generation = 0, 0
		got.Endpoint.Routes, want.Endpoint.Routes = nil, nil
		if label == "duplicate" && got.Incarnation >= want.Incarnation {
			got.Incarnation, want.Incarnation = 0, 0
		}
		if !reflect.DeepEqual(got, want) {
			capture(label+"-unauthorized-mutation", o)
			t.Fatal(label, "changed valid/foreign kernel or ownership state")
		}
	}
	for _, scenario := range []struct {
		id   string
		edit func(*bgp.EVPNMacIPAdvertisementRoute, *[]bgp.PathAttributeInterface)
	}{
		{"M004", func(_ *bgp.EVPNMacIPAdvertisementRoute, attrs *[]bgp.PathAttributeInterface) {
			rt, err := bgp.ParseRouteTarget("64513:999")
			if err != nil {
				t.Fatal(err)
			}
			(*attrs)[2] = bgp.NewPathAttributeExtendedCommunities([]bgp.ExtendedCommunityInterface{
				rt, &bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_VXLAN},
			})
		}},
		{"M005", func(_ *bgp.EVPNMacIPAdvertisementRoute, attrs *[]bgp.PathAttributeInterface) {
			(*attrs)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.90"))
		}},
		{"M006", func(nlri *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			nlri.Labels = []uint32{200}
		}},
	} {
		func() {
			edit := func(nlri *bgp.EVPNMacIPAdvertisementRoute, attrs *[]bgp.PathAttributeInterface) {
				nlri.MacAddress = mustMAC(t, "02:00:00:00:00:81")
				nlri.IPAddressLength, nlri.IPAddress = 32, netip.MustParseAddr("10.251.1.12")
				scenario.edit(nlri, attrs)
			}
			badID := add(edit, 1)
			defer del(badID)
			observed := wait(scenario.id+"-rejected", func(o observation) bool {
				seen := false
				for _, path := range o.RIB.Paths {
					route, err := DecodeRemoteType2(path)
					if err == nil && route.MAC == "02:00:00:00:00:81" && route.IP == "10.251.1.12" {
						switch scenario.id {
						case "M004":
							seen = reflect.DeepEqual(route.RouteTargets, []string{"64513:999"})
						case "M005":
							seen = route.NextHop == "192.0.2.90"
						case "M006":
							seen = reflect.DeepEqual(route.Labels, []uint32{200})
						}
					}
				}
				return seen && o.Error == "" && o.Snapshot.Authoritative &&
					len(o.Snapshot.Endpoints) == 1 && len(o.Snapshot.Issues) == 1
			})
			stable(scenario.id, observed)
			for attempt := 0; attempt < 3; attempt++ {
				time.Sleep(200 * time.Millisecond)
				stable(scenario.id, observe())
			}
			t.Log(scenario.id + "=PASS live rejected route observed; valid+foreign RIB/kernel ownership unchanged")
		}()
		wait(scenario.id+"-removed", healthy)
	}
	func() {
		duplicate := add(nil, 2)
		duplicated := wait("M007-duplicate", func(o observation) bool {
			return o.Error == "" && o.Snapshot.Authoritative && len(o.Snapshot.Endpoints) == 1 &&
				len(o.Snapshot.Endpoints[0].Routes) == 2 && len(o.Receipts) == 1
		})
		stable("duplicate", duplicated)
		del(duplicate)
		remaining := wait("M007-one-path-removed", healthy)
		stable("duplicate", remaining)
		baseline = remaining
		conflict := add(func(_ *bgp.EVPNMacIPAdvertisementRoute, attrs *[]bgp.PathAttributeInterface) {
			(*attrs)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
		}, 2)
		conflicted := wait("M007-conflicting-vtep", func(o observation) bool {
			seen := false
			for _, path := range o.RIB.Paths {
				route, err := DecodeRemoteType2(path)
				seen = seen || (err == nil && route.NextHop == "192.0.2.81")
			}
			return seen && strings.Contains(o.Error, "conflicting remote VTEPs") &&
				!o.Snapshot.Authoritative && len(o.Snapshot.Endpoints) == 1
		})
		stable("conflict", conflicted)
		del(conflict)
		wait("M007-recovered", healthy)
		t.Log("M007=PASS actual AddPaths duplicate aggregated; conflicting VTEP freezes exact owned state; recovery preserves foreign controls")
	}()
	del(validID)
	last := wait("final-withdrawal", func(o observation) bool {
		return o.Error == "" && o.Snapshot.Authoritative && len(o.Snapshot.Endpoints) == 0 &&
			len(o.Receipts) == 0
	})
	if !reflect.DeepEqual(last.Kernel, foreignKernel) {
		t.Fatal("exact owned withdrawal did not preserve only foreign controls", last.Kernel)
	}
	capture("result", map[string]interface{}{
		"sourceSHA": "ae07988afdd08b860eb45221ed50c01e1995769f",
		"status":    "PASS", "cases": []string{"M004", "M005", "M006", "M007"},
		"topology":    "real upstream GoBGP TCP EVPN -> pinned receiver; actual validated importer -> isolated Linux namespace owned backend",
		"limitations": "Receiver/route import and real kernel integration; no negative-case workload dataplane or OVSDB backend parity claim",
	})
}
