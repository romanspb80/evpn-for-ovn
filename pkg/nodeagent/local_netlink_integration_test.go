package nodeagent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type nativeTraceClient struct {
	api.GoBgpServiceClient
	deleted []string
}

func (c *nativeTraceClient) DeletePath(ctx context.Context, request *api.DeletePathRequest, options ...grpc.CallOption) (*api.DeletePathResponse, error) {
	if len(request.Uuid) != 16 || request.Path != nil {
		return nil, fmt.Errorf("native withdrawal must use UUID exclusively")
	}
	result, err := c.GoBgpServiceClient.DeletePath(ctx, request, options...)
	if err == nil {
		raw := hex.EncodeToString(request.Uuid)
		c.deleted = append(c.deleted, raw[:8]+"-"+raw[8:12]+"-"+raw[12:16]+"-"+raw[16:20]+"-"+raw[20:])
	}
	return result, err
}

func TestNativeLocalExporter_Live(t *testing.T) {
	if os.Getenv("EVPN_LOCAL_NATIVE") != "disposable" {
		t.Skip("requires owned fresh-kind native OVN and pinned k8gobgp")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := func(args ...string) string {
		t.Helper()
		value, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, value, err)
		}
		return string(value)
	}
	capture := func(label string, value interface{}) {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("/tmp/phase21", "local-"+label+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	nb, err := newOVNNBClient(ctx, "unix:/var/run/ovn/ovnnb_db.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer nb.Close()
	ovs, err := newOVSClient(ctx, "unix:/var/run/openvswitch/db.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ovs.Close()
	sb, err := newExportSBClient(ctx, "unix:/var/run/ovn/ovnsb_db.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	chassis := strings.Trim(strings.TrimSpace(command("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:system-id")), "\"")
	cfg := evpn.ExporterConfig{Enabled: true, NodeName: "native-test", LocalChassis: chassis,
		GRPCEndpoint: "unix:///var/run/gobgp/gobgp.sock", ASN: 64512, RouterID: os.Getenv("EVPN_IMPORT_LOCAL_VTEP"), ProcessContinuity: true,
		Networks: []evpn.EVPNNetworkConfig{{Enabled: true, NetworkID: "native-local", LogicalSwitch: "contract-ls",
			VNI: 100, LocalVTEP: "192.0.2.1", RouteDistinguisher: "64512:100", RouteTargets: []string{"64512:100"},
			ExporterSource: "ovn-netlink", BridgeDevice: "probe-br", VXLANDevice: "probe-vx", AdvertiseDevice: "probe-adv"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalExportConfig(cfg); err != nil {
		t.Fatal(err)
	}
	capture("config", cfg)
	authority, err := evpn.AcquireExporterAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if duplicate, err := evpn.AcquireExporterAuthority(); err == nil {
		duplicate.Close()
		t.Fatal("concurrent CNI/native exporter authority accepted")
	}
	state := liveExportState{nb: nb, ovs: ovs, sb: sb}
	native, err := evpn.NewLocalOVNNetlinkObserver(cfg, &nativeOVNPorts{source: state, config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	cni := &CNIEndpointObserver{server: NewCNIServer(nil), source: state, config: cfg}
	selected := selectedEndpointObserver{cni: cni, native: native}
	guard := &unixSpeakerContinuity{}
	defer guard.Close()
	connect := func() *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient(cfg.GRPCEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return guard.DialContext(ctx, cfg.GRPCEndpoint)
			}))
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	conn := connect()
	defer func() { conn.Close() }()
	client := api.NewGoBgpServiceClient(conn)
	trace := &nativeTraceClient{GoBgpServiceClient: client}
	adapter := &evpn.GoBGPType2Adapter{Client: trace}
	exporter := &evpn.EndpointExporter{Observer: selected, Source: adapter, Applier: adapter,
		Gate: evpn.SpeakerReadiness{Client: client, ASN: cfg.ASN, RouterID: cfg.RouterID}, Continuity: guard}

	peer := server.NewBgpServer()
	go peer.Serve()
	defer peer.Stop()
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	if err := peer.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 64513, RouterId: "192.0.2.13",
		ListenPort: int32(port), ListenAddresses: []string{"127.0.0.2"}}}); err != nil {
		t.Fatal(err)
	}
	if err := peer.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: "127.0.0.2", RemotePort: 1179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		AfiSafis: []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{
			Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true}}},
	}}); err != nil {
		t.Fatal(err)
	}
	peerPaths := func() []evpn.Type2NLRI {
		t.Helper()
		var paths []evpn.Type2NLRI
		err := peer.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_EVPN},
			func(prefix bgp.NLRI, routePaths []*apiutil.Path) {
				nlri, ok := prefix.(*bgp.EVPNNLRI)
				if !ok {
					return
				}
				n, ok := nlri.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
				if !ok || n.RD.String() != "64512:100" {
					return
				}
				for _, path := range routePaths {
					hop, rt := "", false
					for _, attr := range path.Attrs {
						switch a := attr.(type) {
						case *bgp.PathAttributeMpReachNLRI:
							hop = a.Nexthop.String()
						case *bgp.PathAttributeExtendedCommunities:
							for _, community := range a.Value {
								rt = rt || community.String() == "64512:100"
							}
						}
					}
					if hop != "192.0.2.1" || !rt || n.ETag != 0 || len(n.Labels) != 1 || n.Labels[0] != 100 {
						t.Errorf("invalid standard peer Type-2: %s hop=%s RT=%v", prefix, hop, rt)
					}
					ip := ""
					if n.IPAddressLength != 0 {
						ip = n.IPAddress.String()
					}
					esi, err := n.ESI.Serialize()
					if err != nil {
						t.Errorf("invalid Type-2 ESI: %v", err)
						return
					}
					paths = append(paths, evpn.Type2NLRI{
						RD: n.RD.String(), ESI: hex.EncodeToString(esi),
						EthernetTag: n.ETag, MAC: n.MacAddress.String(), IP: ip, Labels: append([]uint32(nil), n.Labels...),
					})
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		return paths
	}
	waitPeer := func(label string, result evpn.ExportResult) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			paths := peerPaths()
			if len(paths) == len(result.Applied) {
				matches := true
				for _, owned := range result.Applied {
					route, err := evpn.BuildType2Route(owned.Endpoint)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, path := range paths {
						found = found || path.Equal(route.NLRI)
					}
					matches = matches && found
				}
				if matches {
					capture(label+"-peer-rib", paths)
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s real remote peer RIB did not converge: %v", label, peerPaths())
	}
	var last evpn.ExportResult
	wait := func(label string, count int) evpn.ExportResult {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		var err error
		for time.Now().Before(deadline) {
			last, err = exporter.Reconcile(ctx)
			if err == nil && len(last.Applied) == count {
				capture(label+"-receipts", last)
				capture(label+"-deleted-uuids", trace.deleted)
				rib, err := adapter.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				capture(label+"-local-rib", rib)
				capture(label+"-native", map[string]string{
					"fdb":  command("bridge", "-j", "fdb", "show", "dev", "probe-adv"),
					"ipv4": command("ip", "-j", "-4", "neigh", "show", "dev", "probe-br", "nud", "all"),
					"ipv6": command("ip", "-j", "-6", "neigh", "show", "dev", "probe-br", "nud", "all"),
				})
				waitPeer(label, last)
				t.Logf("%s native-to-pinned-k8gobgp-to-real-peer count=%d PASS", label, count)
				return last
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s reconcile: %v %+v", label, err, last)
		return last
	}
	first := wait("dualstack", 3)
	for _, route := range first.Applied {
		if route.PathUUID == "" || route.Endpoint.Incarnation == "" || route.Endpoint.EndpointID == "" {
			t.Fatal("route ownership receipt missing")
		}
	}
	repeated := wait("idempotent", 3)
	for i := range first.Applied {
		if first.Applied[i].PathUUID != repeated.Applied[i].PathUUID {
			t.Fatal("repeated native observation replaced route UUID")
		}
	}
	// Foreign local routes and an abrupt second exporter cannot be adopted.
	foreign := first.Applied[0].Endpoint
	foreign.NetworkID, foreign.EndpointID, foreign.Incarnation, foreign.RouteDistinguisher = "foreign", "foreign", "foreign", "64512:999"
	route, err := evpn.BuildType2Route(foreign)
	if err != nil {
		t.Fatal(err)
	}
	id, err := adapter.Add(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	foreignReceipt := evpn.OwnedType2Route{Endpoint: foreign, PathUUID: id}
	capture("foreign", foreignReceipt)
	restarted := &evpn.EndpointExporter{Observer: selected, Source: adapter, Applier: adapter,
		Gate: evpn.SpeakerReadiness{Client: client, ASN: cfg.ASN, RouterID: cfg.RouterID}}
	if _, err := restarted.Reconcile(ctx); err == nil {
		t.Fatal("new exporter adopted old process routes")
	}
	conn.Close()
	if _, err := exporter.Reconcile(ctx); err == nil {
		t.Fatal("closed GoBGP API treated as authoritative")
	}
	conn = connect()
	client = api.NewGoBgpServiceClient(conn)
	trace.GoBgpServiceClient = client
	exporter.Gate = evpn.SpeakerReadiness{Client: client, ASN: cfg.ASN, RouterID: cfg.RouterID}
	wait("api-recovered", 3)

	command("ovn-nbctl", "--", "lsp-set-addresses", "contract-local", "02:00:00:00:00:11")
	command("ovn-nbctl", "--timeout=20", "--wait=hv", "sync")
	macOnly := wait("mac-only", 1)
	if !macOnly.Applied[0].Endpoint.MACOnly {
		t.Fatal("MAC-only native endpoint contained IP")
	}
	command("ovn-nbctl", "--", "lsp-set-addresses", "contract-local", "02:00:00:00:00:11 10.77.0.12 2001:db8:77::12")
	command("ovn-nbctl", "--timeout=20", "--wait=hv", "sync")
	wait("replaced-dualstack", 3)
	command("ovn-nbctl", "--", "lsp-del", "contract-local")
	command("ovn-nbctl", "--timeout=20", "--wait=hv", "sync")
	wait("deleted", 0)
	for _, owned := range first.Applied {
		found := false
		for _, id := range trace.deleted {
			if id == foreignReceipt.PathUUID {
				t.Fatal("exporter withdrew foreign UUID")
			}
			found = found || id == owned.PathUUID
		}
		if !found {
			t.Fatalf("original UUID %s not exactly withdrawn", owned.PathUUID)
		}
	}
	if strings.Contains(command("bridge", "fdb", "show", "dev", "probe-adv"), "02:00:00:00:00:11") {
		t.Fatal("native local removal did not remove OVN FDB")
	}
	rib, err := adapter.Snapshot(ctx)
	if err != nil || len(rib) != 1 || rib[0].NLRI == nil || !rib[0].NLRI.Equal(route.NLRI) {
		t.Fatalf("foreign GoBGP route not protected: %+v %v", rib, err)
	}
	command("ovn-nbctl", "--", "lsp-add", "contract-ls", "contract-local", "--",
		"lsp-set-addresses", "contract-local", "02:00:00:00:00:11 10.77.0.11 2001:db8:77::11")
	command("ovn-nbctl", "--timeout=20", "--wait=hv", "sync")
	wait("recreated", 3)
	drained, err := exporter.DrainNetworks(ctx, map[string]bool{"native-local": true})
	if err != nil || len(drained.Applied) != 0 {
		t.Fatalf("controlled shutdown failed: %+v %v", drained, err)
	}
	capture("drained", drained)
	capture("drained-deleted-uuids", trace.deleted)
	waitPeer("drained", drained)
	if err := adapter.Delete(ctx, foreignReceipt); err != nil {
		t.Fatal(err)
	}
	native.Close()
	authority.Close()
	// Exercise the actual optional node-agent runtime, including persistent
	// status and graceful cancellation, rather than only its lower-level parts.
	configPath := "/tmp/phase21/local-runtime-config.json"
	cfgData, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, cfgData, 0600); err != nil {
		t.Fatal(err)
	}
	runtimeCtx, stop := context.WithCancel(ctx)
	defer stop()
	statuses := make(chan ExportStatus, 128)
	finished := make(chan error, 1)
	go func() {
		finished <- NewCNIServer(nil).RunEndpointExporter(runtimeCtx,
			RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth",
				OVNNBEndpoint: "unix:/var/run/ovn/ovnnb_db.sock", OVSDBEndpoint: "unix:/var/run/openvswitch/db.sock"},
			ExportRuntimeOptions{ConfigPath: configPath, StatusPath: "/tmp/phase21/local-runtime-status.json",
				SBEndpoint: "unix:/var/run/ovn/ovnsb_db.sock", Interval: 100 * time.Millisecond, Timeout: 5 * time.Second},
			func(status ExportStatus) { statuses <- status })
	}()
	runtimeReady := false
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !runtimeReady {
		select {
		case status := <-statuses:
			runtimeReady = status.Error == "" && len(status.Result.Applied) == 3
			if runtimeReady {
				capture("runtime-applied", status)
				waitPeer("runtime-applied", status.Result)
			}
		case err := <-finished:
			t.Fatalf("native runtime exited before ready: %v", err)
		case <-deadline.C:
			t.Fatal("native runtime did not advertise")
		}
	}
	stop()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("native runtime did not finish controlled shutdown")
	}
	persisted, err := os.ReadFile("/tmp/phase21/local-runtime-status.json")
	if err != nil {
		t.Fatal(err)
	}
	var shutdown ExportStatus
	if err := json.Unmarshal(persisted, &shutdown); err != nil || shutdown.Error != "" || len(shutdown.Result.Applied) != 0 {
		t.Fatalf("persisted shutdown status: %s %v", persisted, err)
	}
	capture("runtime-shutdown", shutdown)
	waitPeer("runtime-shutdown", shutdown.Result)
	capture("continuity", guard.Evidence())
	capture("final-ovn", command("ovn-sbctl", "--format=json", "list", "Port_Binding"))
	t.Log("native-local-MAC-only-IPv4-IPv6-exact-UUID-withdrawal-isolation-foreign-preservation=PASS")
}
