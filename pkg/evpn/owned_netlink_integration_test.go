package evpn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestOwnedNetlinkNative_Live(t *testing.T) {
	if os.Getenv("EVPN_OWNED_NATIVE") != "disposable" {
		t.Skip("requires fresh test-owned kind, pinned k8gobgp and native OVN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	directory := "/tmp/phase21"
	capture := func(label string, value interface{}) {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, label+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s: %v", args, out, err)
		}
		return string(out)
	}
	native := func(label, mac, ip, hop string, present bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			fdb := command("ovn-appctl", "-t", "ovn-controller", "evpn/vtep-fdb-list")
			arp := command("ovn-appctl", "-t", "ovn-controller", "evpn/vtep-arp-list")
			vtep := command("ovn-appctl", "-t", "ovn-controller", "evpn/remote-vtep-list")
			flows := command("ovs-ofctl", "dump-flows", "br-int")
			ok := strings.Contains(fdb, mac) == present && strings.Contains(vtep, hop) == present
			if ip != "" {
				ok = ok && strings.Contains(arp, ip) == present
			}
			ok = ok && strings.Contains(flows, mac) == present
			if ok {
				for name, value := range map[string]string{"fdb": fdb, "arp": arp, "vtep": vtep, "flows": flows,
					"kernel-fdb":   command("bridge", "-j", "fdb", "show", "dev", "probe-vx"),
					"kernel-neigh": command("ip", "-j", "neigh", "show", "dev", "probe-br")} {
					if err := os.WriteFile(filepath.Join(directory, label+"-"+name+".txt"), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Logf("%s native-cache-and-flow present=%v PASS", label, present)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s native evidence timed out", label)
	}
	conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	config := importConfig()
	config.LocalVTEP = os.Getenv("EVPN_IMPORT_LOCAL_VTEP")
	source := &GoBGPRemoteRIBSource{Client: client, Gate: SpeakerReadiness{Client: client, ASN: 64512, RouterID: config.LocalVTEP}}
	reader, err := NewReadOnlyType2Importer(source, config)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(directory, "owner")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := RemoteNetlinkConfig{Enabled: true, ExclusiveWriter: true, StateDirectory: state,
		Networks: []RemoteNetlinkNetwork{{NetworkID: "remote-test", LogicalSwitch: "contract-ls", VNI: 100, BridgeDevice: "probe-br", VXLANDevice: "probe-vx"}}}
	backend, err := NewOwnedOVNNetlinkBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	importer, err := NewValidatedRemoteType2Importer(reader, backend)
	if err != nil {
		t.Fatal(err)
	}
	remote := server.NewBgpServer()
	go remote.Serve()
	defer remote.Stop()
	if err := remote.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 64513, RouterId: "192.0.2.13",
		ListenPort: reserveTCPPort(t), ListenAddresses: []string{"127.0.0.2"}}}); err != nil {
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
	add := func(macOnly bool) uuid.UUID {
		t.Helper()
		p := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.MacAddress = mustMAC(t, "02:00:00:00:00:80")
			if macOnly {
				n.IPAddressLength = 0
				n.IPAddress = netip.Addr{}
			} else {
				n.IPAddressLength = 32
				n.IPAddress = netip.MustParseAddr("10.77.0.80")
			}
		})
		n, err := apiutil.GetNativeNlri(p)
		if err != nil {
			t.Fatal(err)
		}
		a, err := apiutil.GetNativePathAttributes(p)
		if err != nil {
			t.Fatal(err)
		}
		out, err := remote.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: n, Attrs: a, Age: time.Now().Unix(), RemoteID: 1}}})
		if err != nil || len(out) != 1 || out[0].Error != nil {
			t.Fatalf("AddPath: %+v %v", out, err)
		}
		return out[0].UUID
	}
	wait := func(label string, count int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		var err error
		var lastError string
		for time.Now().Before(deadline) {
			err = importer.ReconcileOnce(ctx)
			if err != nil && err.Error() != lastError {
				value, _ := json.Marshal(backend.Receipts())
				t.Logf("%s pending: %v receipts=%s", label, err, value)
				lastError = err.Error()
			}
			s := importer.LastSnapshot()
			if err == nil && s.Authoritative && len(s.Endpoints) == count && len(backend.Receipts()) == count {
				capture(label+"-snapshot", s)
				capture(label+"-receipts", backend.Receipts())
				raw, e := source.ReadRemoteRIB(ctx)
				if e != nil {
					t.Fatal(e)
				}
				capture(label+"-rib", raw)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s reconcile failed: %v snapshot=%+v receipts=%+v", label, err, importer.LastSnapshot(), backend.Receipts())
	}
	// A foreign slot outside desired state must remain byte-for-byte untouched.
	h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	vx, err := h.LinkByName("probe-vx")
	if err != nil {
		t.Fatal(err)
	}
	foreign := &netlink.Neigh{Family: unix.AF_BRIDGE, LinkIndex: vx.Attrs().Index, HardwareAddr: mustMAC(t, "02:00:00:00:00:fe"),
		IP: netip.MustParseAddr("192.0.2.99").AsSlice(), State: netlink.NUD_NOARP | netlink.NUD_PERMANENT, Flags: netlink.NTF_SELF | netlink.NTF_EXT_LEARNED}
	if err := h.NeighAdd(foreign); err != nil {
		t.Fatal(err)
	}
	for _, macOnly := range []bool{true, false} {
		label := fmt.Sprintf("mac-only-%v", macOnly)
		id := add(macOnly)
		wait(label+"-applied", 1)
		r := backend.Receipts()[0]
		if r.Endpoint.RemoteVTEP != "192.0.2.80" || r.Endpoint.Routes[0].Route.Source.NeighborIP != "127.0.0.2" {
			t.Fatal("peer substituted as VTEP")
		}
		ip := ""
		if !macOnly {
			ip = "10.77.0.80"
		}
		native(label+"-added", "02:00:00:00:00:80", ip, "192.0.2.80", true)
		// Closing the real API transport must freeze receipts and kernel state.
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := importer.ReconcileOnce(ctx); err == nil || importer.LastSnapshot().Authoritative || len(backend.Receipts()) != 1 {
			t.Fatal("API uncertainty cleanup")
		}
		capture(label+"-api-frozen", importer.LastSnapshot())
		native(label+"-frozen", "02:00:00:00:00:80", ip, "192.0.2.80", true)
		conn, err = grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		client = api.NewGoBgpServiceClient(conn)
		source.Client, source.Gate = client, SpeakerReadiness{Client: client, ASN: 64512, RouterID: config.LocalVTEP}
		wait(label+"-reconnected", 1)
		if err := remote.DeletePath(apiutil.DeletePathRequest{UUIDs: []uuid.UUID{id}}); err != nil {
			t.Fatal(err)
		}
		wait(label+"-withdrawn", 0)
		capture(label+"-withdrawal-receipts", backend.Withdrawals())
		native(label+"-removed", "02:00:00:00:00:80", ip, "192.0.2.80", false)
	}
	entries, err := h.NeighList(vx.Attrs().Index, unix.AF_BRIDGE)
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, n := range entries {
		preserved = preserved || (n.HardwareAddr.String() == foreign.HardwareAddr.String() && n.IP.Equal(foreign.IP))
	}
	if !preserved {
		t.Fatal("foreign FDB lost")
	}
	if _, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
		t.Fatal("restart adopted ownership")
	}
	t.Log("real-BGP-k8gobgp-owned-Netlink-native-OVN-withdrawal-foreign-preservation-restart-fence=PASS")
}
