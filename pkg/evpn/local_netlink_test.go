package evpn

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestLocalNetlinkKernel(t *testing.T) {
	if os.Getenv("EVPN_OWNED_KERNEL") != "disposable" {
		t.Skip("requires disposable privileged runtime; synthetic kernel-contract test, not native OVN evidence")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			panic(err)
		}
	}()
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ep := sampleEndpoint()
	ep.MACOnly, ep.IPAddresses = true, nil
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "local-br"}}
	if err := h.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	vx := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "local-vx", MasterIndex: br.Index},
		VxlanId: int(ep.VNI), SrcAddr: net.ParseIP(ep.LocalVTEP), Port: 60100}
	adv := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "local-adv", MasterIndex: br.Index}}
	for _, link := range []netlink.Link{vx, adv} {
		if err := h.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range []netlink.Link{br, vx, adv} {
		if err := h.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	cfg := ExporterConfig{Enabled: true, DryRun: true, NodeName: "kernel-test", LocalChassis: "kernel-test",
		ASN: 64512, RouterID: "192.0.2.1", GRPCEndpoint: "127.0.0.1:50051",
		Networks: []EVPNNetworkConfig{{Enabled: true, NetworkID: ep.NetworkID, LogicalSwitch: "kernel-ls",
			VNI: ep.VNI, LocalVTEP: ep.LocalVTEP, RouteDistinguisher: ep.RouteDistinguisher, RouteTargets: ep.RouteTargets,
			ExporterSource: "ovn-netlink", BridgeDevice: br.Name, VXLANDevice: vx.Name, AdvertiseDevice: adv.Name}}}
	provenance := ep
	provenance.MACOnly, provenance.IPAddresses = false, []string{"10.77.0.11", "2001:db8:77::11"}
	observer, err := NewLocalOVNNetlinkObserver(cfg, &exportTestObserver{desired: []LocalEndpoint{provenance}})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	mac, err := net.ParseMAC(ep.MACAddress)
	if err != nil {
		t.Fatal(err)
	}
	entries := []netlink.Neigh{
		{LinkIndex: adv.Index, Family: unix.AF_BRIDGE, HardwareAddr: mac, State: netlink.NUD_NOARP, Flags: netlink.NTF_MASTER},
		{LinkIndex: br.Index, Family: unix.AF_INET, HardwareAddr: mac, State: netlink.NUD_NOARP, IP: net.ParseIP("10.77.0.11")},
		{LinkIndex: br.Index, Family: unix.AF_INET6, HardwareAddr: mac, State: netlink.NUD_NOARP, IP: net.ParseIP("2001:db8:77::11")},
	}
	for _, entry := range entries {
		entry := entry
		if err := h.NeighSet(&entry); err != nil {
			t.Fatal(err)
		}
	}
	confirmLocal(t, observer, 3)
	remote := entries[1]
	remote.Flags = netlink.NTF_EXT_LEARNED
	if err := h.NeighSet(&remote); err != nil {
		t.Fatal(err)
	}
	confirmLocal(t, observer, 2)
	if err := h.LinkDel(adv); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.DesiredLocalEndpoints(context.Background()); err == nil {
		t.Fatal("deleted local advertise device became confirmed endpoint deletion")
	}
	t.Log("isolated-kernel-NOARP-dualstack-extern-learned-exclusion-device-lifetime=PASS")
}

func TestBridgePortEventsRequireFullTopologyRead(t *testing.T) {
	o, k, _ := localObserverFixture(t)
	partial := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "local-vx", Index: 3}}
	update := netlink.LinkUpdate{Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK}, Link: partial}
	update.Family = unix.AF_BRIDGE
	o.updates <- update
	confirmLocal(t, o, 3)
	k.links["local-vx"].(*netlink.Vxlan).VxlanId++
	o.updates <- update
	if _, err := o.DesiredLocalEndpoints(context.Background()); err == nil {
		t.Fatal("partial event bypassed full GETLINK topology fence")
	}
}
func localObserverFixture(t *testing.T) (*LocalOVNNetlinkObserver, *memoryOwnedKernel, *exportTestObserver) {
	t.Helper()
	ep := sampleEndpoint()
	ep.MACOnly, ep.IPAddresses = true, nil
	cfg := EVPNNetworkConfig{NetworkID: ep.NetworkID, Enabled: true, VNI: ep.VNI, LocalVTEP: ep.LocalVTEP,
		ExporterSource: "ovn-netlink", BridgeDevice: "local-br", VXLANDevice: "local-vx", AdvertiseDevice: "local-adv"}
	k := &memoryOwnedKernel{links: map[string]netlink.Link{
		"local-br": &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "local-br", Index: 2, Flags: net.FlagUp}},
		"local-vx": &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "local-vx", Index: 3, MasterIndex: 2, Flags: net.FlagUp},
			VxlanId: int(ep.VNI), SrcAddr: net.ParseIP(ep.LocalVTEP)},
		"local-adv": &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "local-adv", Index: 4, MasterIndex: 2, Flags: net.FlagUp}},
	}}
	mac, err := net.ParseMAC(ep.MACAddress)
	if err != nil {
		t.Fatal(err)
	}
	k.entries = []netlink.Neigh{
		{LinkIndex: 4, Family: unix.AF_BRIDGE, HardwareAddr: mac, State: netlink.NUD_NOARP, Flags: netlink.NTF_MASTER},
		{LinkIndex: 2, Family: unix.AF_INET, HardwareAddr: mac, IP: net.ParseIP("10.77.0.11"), State: netlink.NUD_NOARP},
		{LinkIndex: 2, Family: unix.AF_INET6, HardwareAddr: mac, IP: net.ParseIP("2001:db8:77::11"), State: netlink.NUD_NOARP},
	}
	ports := &exportTestObserver{desired: []LocalEndpoint{ep}}
	ports.desired[0].MACOnly = false
	ports.desired[0].IPAddresses = []string{"10.77.0.11", "2001:db8:77::11"}
	o := &LocalOVNNetlinkObserver{ports: ports, config: ExporterConfig{Networks: []EVPNNetworkConfig{cfg}}, kernel: k,
		devices: map[string]RemoteDeviceIdentity{}, updates: make(chan netlink.LinkUpdate, 16), done: make(chan struct{})}
	if err := o.checkDevices(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o, k, ports
}

func confirmLocal(t *testing.T, o *LocalOVNNetlinkObserver, count int) []LocalEndpoint {
	t.Helper()
	ctx := context.Background()
	o.DesiredLocalEndpoints(ctx)
	got, err := o.DesiredLocalEndpoints(ctx)
	if err != nil || len(got) != count {
		t.Fatalf("native confirmation count=%d: %+v %v", count, got, err)
	}
	return got
}

func TestLocalNetlinkDiscoveryAndReconciliation(t *testing.T) {
	ctx := context.Background()
	o, k, ports := localObserverFixture(t)
	a := &memoryType2API{}
	exporter := &EndpointExporter{Observer: o, Source: a, Applier: a, Gate: &exportTestGate{}}
	if _, err := exporter.Reconcile(ctx); err == nil || a.adds != 0 {
		t.Fatal("unconfirmed first snapshot mutated BGP")
	}
	first, err := exporter.Reconcile(ctx)
	if err != nil || len(first.Applied) != 3 || a.adds != 3 {
		t.Fatalf("dualstack/MAC-only reconciliation: %+v %v", first, err)
	}
	for _, ep := range first.Desired {
		route, err := BuildType2Route(ep)
		if err != nil {
			t.Fatal(err)
		}
		native := route.NLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if ep.MACOnly && (native.IPAddressLength != 0 || native.IPAddress.IsValid()) {
			t.Fatal("MAC-only route encoded an IP")
		}
		if _, err := route.NLRI.Serialize(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := exporter.Reconcile(ctx); err != nil || a.adds != 3 || a.deletes != 0 {
			t.Fatal("repeated dumps duplicated advertisement")
		}
	}
	k.listError = errors.New("ENOBUFS/interrupted dump")
	if _, err := exporter.Reconcile(ctx); err == nil || a.deletes != 0 {
		t.Fatal("incomplete dump withdrew routes")
	}
	k.listError = nil
	if _, err := exporter.Reconcile(ctx); err == nil {
		t.Fatal("observation interruption reused stale candidate")
	}
	if _, err := exporter.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	k.entries[1].IP = net.ParseIP("10.77.0.12")
	ports.desired[0].IPAddresses = []string{"10.77.0.12", "2001:db8:77::11"}
	if _, err := exporter.Reconcile(ctx); err == nil || a.deletes != 0 {
		t.Fatal("unconfirmed replacement mutated routes")
	}
	if _, err := exporter.Reconcile(ctx); err != nil || a.adds != 4 || a.deletes != 1 {
		t.Fatalf("MAC/IP replacement: adds=%d deletes=%d err=%v", a.adds, a.deletes, err)
	}
	ports.err = errors.New("SB disconnected")
	if _, err := exporter.Reconcile(ctx); err == nil || a.deletes != 1 {
		t.Fatal("lost OVN provenance withdrew routes")
	}
	ports.err, ports.desired, k.entries = nil, nil, nil
	exporter.Reconcile(ctx)
	if _, err := exporter.Reconcile(ctx); err != nil || a.deletes != 4 {
		t.Fatalf("confirmed native deletion: %v deletes=%d", err, a.deletes)
	}
	if k.mutations != 0 {
		t.Fatal("observer mutated kernel")
	}
}

func TestLocalNetlinkClassificationAndFencing(t *testing.T) {
	for _, cause := range []string{"extern-fdb", "extern-neighbor", "permanent", "vlan-duplicate", "no-local-fdb",
		"wrong-vni", "wrong-vtep", "wrong-master", "device-recreated", "link-events-lost", "duplicate-owner", "foreign-static-ip"} {
		t.Run(cause, func(t *testing.T) {
			o, k, ports := localObserverFixture(t)
			confirmLocal(t, o, 3)
			want, failed := 3, false
			switch cause {
			case "extern-fdb":
				k.entries[0].Flags |= netlink.NTF_EXT_LEARNED
				want = 0
			case "extern-neighbor":
				k.entries[1].Flags |= netlink.NTF_EXT_LEARNED
				want = 2
			case "permanent":
				k.entries[0].State = netlink.NUD_PERMANENT
				want = 0
			case "vlan-duplicate":
				n := k.entries[0]
				n.Vlan = 1
				k.entries = append(k.entries, n)
			case "no-local-fdb":
				k.entries = k.entries[1:]
				want = 0
			case "wrong-vni":
				k.links["local-vx"].(*netlink.Vxlan).VxlanId++
				failed = true
			case "wrong-vtep":
				k.links["local-vx"].(*netlink.Vxlan).SrcAddr = net.ParseIP("192.0.2.99")
				failed = true
			case "wrong-master":
				k.links["local-adv"].Attrs().MasterIndex++
				failed = true
			case "device-recreated":
				k.links["local-adv"].Attrs().Index++
				failed = true
			case "link-events-lost":
				o.lost.Store(true)
				failed = true
			case "duplicate-owner":
				ports.desired = append(ports.desired, ports.desired[0])
				failed = true
			case "foreign-static-ip":
				k.entries[1].IP = net.ParseIP("10.77.0.99")
				failed = true
			}
			if failed {
				for i := 0; i < 2; i++ {
					if _, err := o.DesiredLocalEndpoints(context.Background()); err == nil {
						t.Fatal("ambiguous native state accepted")
					}
				}
			} else {
				confirmLocal(t, o, want)
			}
		})
	}
}

func TestNativeDrainAndRestartFence(t *testing.T) {
	ctx := context.Background()
	o, _, _ := localObserverFixture(t)
	a := &memoryType2API{}
	e := &EndpointExporter{Observer: o, Source: a, Applier: a, Gate: &exportTestGate{}}
	e.Reconcile(ctx)
	result, err := e.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &EndpointExporter{Observer: o, Source: a, Applier: a, Gate: &exportTestGate{}}
	if _, err := restarted.Reconcile(ctx); err == nil || a.adds != 3 || a.deletes != 0 {
		t.Fatal("process restart adopted existing routes")
	}
	network := result.Applied[0].Endpoint.NetworkID
	if result, err := e.DrainNetworks(ctx, map[string]bool{network: true}); err != nil || len(result.Applied) != 0 || a.deletes != 3 {
		t.Fatalf("acknowledged native drain: %+v %v", result, err)
	}
}
