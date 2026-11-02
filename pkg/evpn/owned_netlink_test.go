package evpn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type memoryOwnedKernel struct {
	links               map[string]netlink.Link
	entries             []netlink.Neigh
	addError, delError  error
	failFamily          int
	listError           error
	mutations           int
	deletes             []netlink.Neigh
	omitAdd, omitDelete bool
}

func (k *memoryOwnedKernel) Close() error { return nil }
func (k *memoryOwnedKernel) LinkByName(name string) (netlink.Link, error) {
	link, ok := k.links[name]
	if !ok {
		return nil, fmt.Errorf("missing link %s", name)
	}
	return link, nil
}
func (k *memoryOwnedKernel) NeighList(index, family int) ([]netlink.Neigh, error) {
	if k.listError != nil {
		return nil, k.listError
	}
	var result []netlink.Neigh
	for _, n := range k.entries {
		if n.LinkIndex == index && n.Family == family {
			result = append(result, n)
		}
	}
	return result, nil
}
func (k *memoryOwnedKernel) NeighAdd(n *netlink.Neigh) error {
	if k.addError != nil && (k.failFamily == 0 || k.failFamily == n.Family) {
		return k.addError
	}
	if k.omitAdd {
		return nil
	}
	for _, old := range k.entries {
		if sameKernelSlot(kernelEntry(old), kernelEntry(*n)) {
			return unix.EEXIST
		}
	}
	copy := *n
	if copy.Type == 0 && copy.Family != unix.AF_BRIDGE {
		copy.Type = unix.RTN_BROADCAST
	}
	k.entries = append(k.entries, copy)
	k.mutations++
	return nil
}
func (k *memoryOwnedKernel) NeighDel(n *netlink.Neigh) error {
	if k.delError != nil && (k.failFamily == 0 || k.failFamily == n.Family) {
		return k.delError
	}
	if k.omitDelete {
		return nil
	}
	k.deletes = append(k.deletes, *n)
	for idx, old := range k.entries {
		if sameKernelSlot(kernelEntry(old), kernelEntry(*n)) {
			k.entries = append(k.entries[:idx], k.entries[idx+1:]...)
			k.mutations++
			return nil
		}
	}
	return unix.ENOENT
}

func remoteOwnedFixture(t *testing.T) (*RemoteType2Importer, *OVNNetlinkBackend, *memoryOwnedKernel, *importTestSource) {
	t.Helper()
	k := &memoryOwnedKernel{links: map[string]netlink.Link{
		"test-br": &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "test-br", Index: 2}},
		"test-vx": &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "test-vx", Index: 3, MasterIndex: 2}, VxlanId: 100},
	}}
	s := &ownedNetlinkState{kernel: k, controller: "controller-test", target: "boot/netns-test",
		config: RemoteNetlinkConfig{Enabled: true, ExclusiveWriter: true, Networks: []RemoteNetlinkNetwork{{
			NetworkID: "remote-test", LogicalSwitch: "test-ls", VNI: 100, BridgeDevice: "test-br", VXLANDevice: "test-vx"}}},
		devices: map[string]RemoteDeviceIdentity{}, receipts: map[string]*RemoteNetlinkReceipt{},
		latest: map[string]uint64{}, updates: make(chan netlink.LinkUpdate, 16), done: make(chan struct{})}
	for name := range k.links {
		id, err := s.device(name)
		if err != nil {
			t.Fatal(err)
		}
		s.devices[name] = id
	}
	b := &OVNNetlinkBackend{owned: s}
	source := &importTestSource{}
	reader, err := NewReadOnlyType2Importer(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		t.Fatal(err)
	}
	return i, b, k, source
}

func ownedSnapshot(t *testing.T, i *RemoteType2Importer, paths ...*api.Path) RemoteImportSnapshot {
	t.Helper()
	source := i.verified.source.(*importTestSource)
	source.snap = RemoteRIBSnapshot{Paths: paths, Complete: true}
	return confirmedImport(t, i.verified)
}

func applyOwned(t *testing.T, i *RemoteType2Importer, snapshot RemoteImportSnapshot) {
	t.Helper()
	if err := i.applyValidatedSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedNetlinkMergesMACOnlyAndMACIPIntoOneEndpoint(t *testing.T) {
	i, b, k, _ := remoteOwnedFixture(t)
	macIP := importPath(t, nil)
	macOnly := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.IPAddressLength = 0
		n.IPAddress = netip.Addr{}
	})
	snapshot := ownedSnapshot(t, i, macOnly, macIP)
	applyOwned(t, i, snapshot)
	if len(k.entries) != 2 || len(b.Receipts()) != 1 {
		t.Fatalf("compatible route variants created duplicate owned kernel slots: entries=%+v receipts=%+v", k.entries, b.Receipts())
	}
	receipt := b.Receipts()[0]
	if receipt.Endpoint.IP == "" || receipt.Endpoint.IP != snapshot.Endpoints[0].IP || len(receipt.Endpoint.Routes) != 2 ||
		!receipt.FDB.ACK || receipt.Neighbor == nil || !receipt.Neighbor.ACK {
		t.Fatalf("merged endpoint was not applied with a single FDB and neighbor receipt: %+v", receipt)
	}
	mutations := k.mutations
	applyOwned(t, i, snapshot)
	if k.mutations != mutations {
		t.Fatal("reapplying merged route variants mutated owned kernel state")
	}
}

func TestOwnedNetlinkLifecycle(t *testing.T) {
	for _, onlyMAC := range []bool{false, true} {
		t.Run(fmt.Sprint("mac-only=", onlyMAC), func(t *testing.T) {
			i, b, k, _ := remoteOwnedFixture(t)
			p := importPath(t, nil)
			if onlyMAC {
				p = importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
					n.IPAddressLength = 0
					n.IPAddress = netip.Addr{}
				})
			}
			s := ownedSnapshot(t, i, p)
			applyOwned(t, i, s)
			want := 2
			if onlyMAC {
				want = 1
			}
			if len(k.entries) != want || b.Receipts()[0].Status != "applied" {
				t.Fatal(k.entries, b.Receipts())
			}
			count := k.mutations
			applyOwned(t, i, s)
			if k.mutations != count {
				t.Fatal("idempotent apply mutated")
			}
			receipt := b.Receipts()[0]
			if receipt.FDB.Request.IP != "192.0.2.80" || receipt.Endpoint.Routes[0].Route.Source.NeighborIP == "192.0.2.80" ||
				!receipt.FDB.ACK {
				t.Fatal(receipt)
			}
			applyOwned(t, i, ownedSnapshot(t, i))
			if len(k.entries) != 0 || len(b.Receipts()) != 0 {
				t.Fatal("withdrawal incomplete")
			}
			if !onlyMAC && k.deletes[0].Family != unix.AF_INET {
				t.Fatal("FDB removed before neighbor")
			}
		})
	}
}

// Exercise the backend's IPv6 representation without widening the unchanged
// 3.3a import policy, which deliberately rejects IPv6 endpoints.
func backendIPv6Fixture(seed ImportedType2Route) ImportedType2Route {
	ep := *seed.validated
	ep.Routes = append([]ValidatedRemoteRoute(nil), ep.Routes...)
	ep.IP = "2001:db8:77::80"
	ep.Key = remoteHash([]string{ep.NetworkID, ep.MAC, ep.IP})
	for n := range ep.Routes {
		ep.Routes[n].Route.IP = ep.IP
	}
	seed.Key, seed.IP, seed.validated = ep.Key, ep.IP, &ep
	return seed
}

func TestOwnedNetlinkIPv6Backend(t *testing.T) {
	i, b, k, _ := remoteOwnedFixture(t)
	snapshot := ownedSnapshot(t, i, importPath(t, nil))
	applyOwned(t, i, snapshot)
	seed := i.active[snapshot.Endpoints[0].Key]
	applyOwned(t, i, ownedSnapshot(t, i))
	route := backendIPv6Fixture(seed)
	if err := b.Apply(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	r := b.Receipts()[0]
	if r.Status != "applied" || !r.Neighbor.ACK || r.Neighbor.Observed.Family != unix.AF_INET6 ||
		r.Neighbor.Observed.IP != route.IP {
		t.Fatal(r)
	}
	if err := b.Withdraw(context.Background(), route); err != nil || len(k.entries) != 0 {
		t.Fatal(err, k.entries)
	}
}

func TestOwnedNetlinkReplacementAndStaleReceipt(t *testing.T) {
	i, b, k, _ := remoteOwnedFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	old := i.active[b.Receipts()[0].Endpoint.Key]
	p = importPath(t, func(_ *bgp.EVPNMacIPAdvertisementRoute, a *[]bgp.PathAttributeInterface) {
		(*a)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
	})
	applyOwned(t, i, ownedSnapshot(t, i, p))
	if b.Receipts()[0].FDB.Request.IP != "192.0.2.81" {
		t.Fatal(b.Receipts())
	}
	count := k.mutations
	if err := b.Withdraw(context.Background(), old); err == nil || k.mutations != count {
		t.Fatal("stale receipt deleted replacement")
	}
	if err := b.Apply(context.Background(), old); err == nil || k.mutations != count {
		t.Fatal("stale apply accepted")
	}
}

func TestOwnedNetlinkForeignAndPartial(t *testing.T) {
	for _, family := range []int{unix.AF_BRIDGE, unix.AF_INET} {
		t.Run(fmt.Sprint("foreign-", family), func(t *testing.T) {
			i, b, k, _ := remoteOwnedFixture(t)
			s := ownedSnapshot(t, i, importPath(t, nil))
			ep := s.Endpoints[0]
			if family == unix.AF_BRIDGE {
				k.entries = []netlink.Neigh{{Family: family, LinkIndex: 3, HardwareAddr: mustMAC(t, ep.MAC),
					IP: net.ParseIP(ep.RemoteVTEP), State: netlink.NUD_PERMANENT, Flags: netlink.NTF_SELF | netlink.NTF_EXT_LEARNED}}
			} else {
				k.entries = []netlink.Neigh{{Family: family, LinkIndex: 2, HardwareAddr: mustMAC(t, "02:00:00:00:ff:ff"),
					IP: net.ParseIP(ep.IP), State: netlink.NUD_NOARP, Flags: netlink.NTF_EXT_LEARNED}}
			}
			foreign := kernelEntry(k.entries[0])
			if err := i.applyValidatedSnapshot(context.Background(), s); err == nil {
				t.Fatal("foreign entry adopted")
			}
			if kernelEntry(k.entries[0]) != foreign || len(k.deletes) != 0 {
				t.Fatal("foreign state mutated")
			}
			if b.Receipts()[0].Status == "applied" {
				t.Fatal("false full receipt")
			}
		})
	}
	t.Run("partial-apply-retry", func(t *testing.T) {
		i, b, k, _ := remoteOwnedFixture(t)
		k.addError, k.failFamily = unix.EPERM, unix.AF_INET
		s := ownedSnapshot(t, i, importPath(t, nil))
		if err := i.applyValidatedSnapshot(context.Background(), s); err == nil {
			t.Fatal("expected failure")
		}
		r := b.Receipts()[0]
		if r.Status != "partially-applied" || r.FDB.Status != "confirmed" || r.Neighbor.Status != "failed" {
			t.Fatal(r)
		}
		k.addError = nil
		applyOwned(t, i, s)
		if len(k.entries) != 2 || k.mutations != 2 {
			t.Fatal("FDB duplicated", k.entries)
		}
	})
	t.Run("partial-withdraw-retry", func(t *testing.T) {
		i, b, k, _ := remoteOwnedFixture(t)
		applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
		k.delError, k.failFamily = unix.EPERM, unix.AF_BRIDGE
		empty := ownedSnapshot(t, i)
		if err := i.applyValidatedSnapshot(context.Background(), empty); err == nil {
			t.Fatal("expected failure")
		}
		r := b.Receipts()[0]
		if r.Status != "partially-withdrawn" || r.Neighbor.Status != "absent-confirmed" || r.FDB.Status != "confirmed" {
			t.Fatal(r)
		}
		k.delError = nil
		applyOwned(t, i, empty)
		if len(k.entries) != 0 || len(k.deletes) != 2 {
			t.Fatal("withdraw retry repeated completed step")
		}
	})
}

func TestOwnedNetlinkHazardAndFences(t *testing.T) {
	for _, name := range []string{"foreign-replacement", "device-recreation", "lost-receipt", "restart", "observation-loss", "unknown-ack"} {
		t.Run(name, func(t *testing.T) {
			i, b, k, _ := remoteOwnedFixture(t)
			s := ownedSnapshot(t, i, importPath(t, nil))
			if name == "unknown-ack" {
				k.addError = errors.New("timeout after send")
				if err := i.applyValidatedSnapshot(context.Background(), s); err == nil {
					t.Fatal("unknown success")
				}
				if b.Receipts()[0].FDB.Status != "outcome-unknown" {
					t.Fatal(b.Receipts())
				}
				k.addError = nil
			} else {
				applyOwned(t, i, s)
			}
			switch name {
			case "foreign-replacement":
				for idx, n := range k.entries {
					if n.Family == unix.AF_INET {
						k.entries[idx].HardwareAddr = mustMAC(t, "02:00:00:00:ff:ff")
					}
				}
			case "device-recreation":
				k.links["test-vx"].Attrs().Index = 9
			case "lost-receipt":
				b.owned.receipts = map[string]*RemoteNetlinkReceipt{}
			case "restart":
				b.owned.controller = "new-controller"
			case "observation-loss":
				b.owned.lost.Store(true)
			}
			count := k.mutations
			if err := i.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, i)); err == nil {
				t.Fatal("fence not applied")
			}
			if k.mutations != count || len(k.deletes) != 0 {
				t.Fatal("unsafe stale IP-only deletion issued")
			}
		})
	}
}

func TestOwnedNetlinkAuthorityAndMultiplePaths(t *testing.T) {
	i, b, k, source := remoteOwnedFixture(t)
	p := importPath(t, nil)
	source.snap = RemoteRIBSnapshot{Paths: []*api.Path{p}, Complete: true}
	if err := i.ReconcileOnce(context.Background()); err == nil || k.mutations != 0 {
		t.Fatal("pending snapshot applied")
	}
	if err := i.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := k.mutations
	second := importPath(t, nil)
	second.Identifier = 2
	s := ownedSnapshot(t, i, p, second)
	applyOwned(t, i, s)
	if len(b.Receipts()[0].Endpoint.Routes) != 2 {
		t.Fatal("multiple source references lost")
	}
	applyOwned(t, i, ownedSnapshot(t, i, p))
	count = k.mutations
	source.err = errors.New("GoBGP API interrupted")
	if err := i.ReconcileOnce(context.Background()); err == nil || k.mutations != count || i.LastSnapshot().Authoritative {
		t.Fatal("API error mutated")
	}
	source.err = nil
	if err := i.applyValidatedSnapshot(context.Background(), RemoteImportSnapshot{}); err == nil || k.mutations != count {
		t.Fatal("uncertain mass withdrawal")
	}
	if err := i.applyValidatedSnapshot(context.Background(), s); err == nil {
		t.Fatal("old generation accepted")
	}
	invalid := ownedSnapshot(t, i, p)
	invalid.Endpoints[0].RemoteVTEP = "192.0.2.99"
	invalid.Generation++
	if err := i.applyValidatedSnapshot(context.Background(), invalid); err == nil || k.mutations != count {
		t.Fatal("invalid endpoint reached kernel")
	}
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOwnedNetlinkDisabledAndMarker(t *testing.T) {
	if err := (&OVNNetlinkBackend{}).Apply(context.Background(), ImportedType2Route{}); err == nil {
		t.Fatal("default enabled")
	}
	if os.Geteuid() != 0 {
		t.Skip("root-owned marker test")
	}
	dir := t.TempDir()
	if err := acquireRemoteMarker(dir, "one", "netns"); err != nil {
		t.Fatal(err)
	}
	if err := acquireRemoteMarker(dir, "two", "netns"); err == nil {
		t.Fatal("restart adopted marker")
	}
}

func TestOwnedNetlinkRetiringRouteReappears(t *testing.T) {
	i, b, k, _ := remoteOwnedFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	old := i.active[b.Receipts()[0].Endpoint.Key]
	k.delError, k.failFamily = unix.EPERM, unix.AF_BRIDGE
	if err := i.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, i)); err == nil {
		t.Fatal("withdrawal fault not exercised")
	}
	k.delError = nil
	applyOwned(t, i, ownedSnapshot(t, i, p))
	if b.Receipts()[0].Incarnation <= old.incarnation {
		t.Fatal("retired receipt revived")
	}
	if err := b.Withdraw(context.Background(), old); err == nil {
		t.Fatal("old withdrawal removed reappeared route")
	}
}

func TestOwnedNetlinkAdditionalTransitions(t *testing.T) {
	for _, name := range []string{"changed-mac", "changed-ip", "same-nlri-new-path"} {
		t.Run(name, func(t *testing.T) {
			i, b, k, _ := remoteOwnedFixture(t)
			applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
			old := b.Receipts()[0].Incarnation
			p := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
				if name == "changed-mac" {
					n.MacAddress = mustMAC(t, "02:00:00:00:00:55")
				}
				if name == "changed-ip" {
					n.IPAddress = netip.MustParseAddr("10.77.0.55")
				}
			})
			if name == "same-nlri-new-path" {
				p.Identifier = 42
			}
			applyOwned(t, i, ownedSnapshot(t, i, p))
			if b.Receipts()[0].Incarnation <= old || len(k.entries) != 2 || len(b.Withdrawals()) != 1 {
				t.Fatal("replacement not fenced")
			}
			for _, r := range b.Withdrawals() {
				if !r.FDB.DeleteACK || r.FDB.Status != "absent-confirmed" ||
					!r.Neighbor.DeleteACK || r.Neighbor.Status != "absent-confirmed" {
					t.Fatal(r)
				}
			}
		})
	}
	for _, name := range []string{"add-readback-missing", "delete-readback-present", "dump-failure", "link-event", "changed-vni", "ipv6-policy"} {
		t.Run(name, func(t *testing.T) {
			i, b, k, _ := remoteOwnedFixture(t)
			s := ownedSnapshot(t, i, importPath(t, nil))
			if name == "add-readback-missing" {
				k.omitAdd = true
				if err := i.applyValidatedSnapshot(context.Background(), s); err == nil {
					t.Fatal("missing readback accepted")
				}
				r := b.Receipts()[0].FDB
				if !r.ACK || r.Status != "outcome-unknown" {
					t.Fatal(r)
				}
				return
			}
			if name == "ipv6-policy" {
				i, _, k, _ := remoteOwnedFixture(t)
				p := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
					n.IPAddressLength = 128
					n.IPAddress = netip.MustParseAddr("2001:db8::80")
				})
				s := ownedSnapshot(t, i, p)
				applyOwned(t, i, s)
				if k.mutations != 0 || len(s.Endpoints) != 0 || len(s.Issues) != 1 {
					t.Fatal("IPv6 policy bypass")
				}
				return
			}
			applyOwned(t, i, s)
			count := k.mutations
			switch name {
			case "delete-readback-present":
				k.omitDelete = true
			case "dump-failure":
				k.listError = errors.New("dump interrupted")
			case "link-event":
				b.owned.updates <- netlink.LinkUpdate{Link: k.links["test-vx"], Header: unix.NlMsghdr{Type: unix.RTM_DELLINK}}
			case "changed-vni":
				s.Generation++
				s.Endpoints[0].VNI = 200
				if err := i.applyValidatedSnapshot(context.Background(), s); err == nil || k.mutations != count {
					t.Fatal("mapping change mutated")
				}
				return
			}
			if err := i.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, i)); err == nil {
				t.Fatal("loss/unknown outcome accepted")
			}
			if k.mutations != count {
				t.Fatal("foreign or unverified mutation")
			}
		})
	}
}

// This test changes only a newly allocated namespace on its locked OS thread.
func TestOwnedNetlinkKernel(t *testing.T) {
	if os.Getenv("EVPN_OWNED_KERNEL") != "disposable" {
		t.Skip("requires disposable privileged runtime")
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
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "owned-br", HardwareAddr: mustMAC(t, "02:00:00:00:00:01")}}
	if err := h.LinkAdd(bridge); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(bridge); err != nil {
		t.Fatal(err)
	}
	vx := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "owned-vx", MasterIndex: bridge.Index}, VxlanId: 100, Port: 60100}
	if err := h.LinkAdd(vx); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(vx); err != nil {
		t.Fatal(err)
	}
	// Let fixture link notifications settle before the backend subscribes.
	time.Sleep(100 * time.Millisecond)
	cfg := RemoteNetlinkConfig{Enabled: true, ExclusiveWriter: true, StateDirectory: t.TempDir(),
		Networks: []RemoteNetlinkNetwork{{NetworkID: "remote-test", LogicalSwitch: "owned-ls", VNI: 100, BridgeDevice: "owned-br", VXLANDevice: "owned-vx"}}}
	b, err := NewOwnedOVNNetlinkBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		t.Fatal(err)
	}
	s := ownedSnapshot(t, i, importPath(t, nil))
	applyOwned(t, i, s)
	seed := i.active[s.Endpoints[0].Key]
	t.Logf("kernel-applied receipts=%+v", b.Receipts())
	entries, err := h.NeighList(bridge.Index, unix.AF_INET)
	if err != nil {
		t.Fatal(entries, err)
	}
	var managed []netlink.Neigh
	for _, n := range entries {
		if n.IP.String() == s.Endpoints[0].IP {
			managed = append(managed, n)
		}
	}
	if len(managed) != 1 || managed[0].Flags&netlink.NTF_EXT_LEARNED == 0 || managed[0].State != netlink.NUD_NOARP {
		t.Fatal(entries)
	}
	applyOwned(t, i, s)
	applyOwned(t, i, ownedSnapshot(t, i))
	entries, err = h.NeighList(vx.Index, unix.AF_BRIDGE)
	if err != nil {
		t.Fatal(entries, err)
	}
	for _, n := range entries {
		if n.HardwareAddr.String() == s.Endpoints[0].MAC {
			t.Fatal("owned FDB remains", n)
		}
	}
	ipv6 := backendIPv6Fixture(seed)
	if err := b.Apply(context.Background(), ipv6); err != nil {
		t.Fatal(err)
	}
	ipv6Entries, err := h.NeighList(bridge.Index, unix.AF_INET6)
	if err != nil {
		t.Fatal(err)
	}
	ipv6Confirmed := false
	for _, entry := range ipv6Entries {
		ipv6Confirmed = ipv6Confirmed || (entry.IP.String() == ipv6.IP &&
			entry.HardwareAddr.String() == ipv6.MAC && entry.Flags&netlink.NTF_EXT_LEARNED != 0)
	}
	if !ipv6Confirmed {
		t.Fatal("IPv6 backend neighbor missing", ipv6Entries)
	}
	if err := b.Withdraw(context.Background(), ipv6); err != nil {
		t.Fatal(err)
	}
	t.Log("real-kernel backend-only MAC/IPv6 apply and exact withdrawal=PASS; importer IPv6 policy unchanged")
	if _, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
		t.Fatal("restart marker bypass")
	}
	// Recreate historical IP-only hazard, then prove a receipted withdrawal
	// detects the replacement before issuing any deletion.
	applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	r := b.Receipts()[0]
	foreign := r.Neighbor.Request.neigh()
	foreign.HardwareAddr = mustMAC(t, "02:00:00:00:ff:ff")
	if err := h.NeighSet(foreign); err != nil {
		t.Fatal(err)
	}
	if err := i.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, i)); err == nil {
		t.Fatal("foreign replacement deleted")
	}
	got, err := h.NeighList(bridge.Index, unix.AF_INET)
	preserved := false
	for _, n := range got {
		preserved = preserved || (n.IP.Equal(foreign.IP) && reflect.DeepEqual(n.HardwareAddr, foreign.HardwareAddr))
	}
	if err != nil || !preserved {
		t.Fatal(got, err)
	}
	t.Log("real-kernel stale-receipt conflict preserves foreign replacement=PASS")
	if err := h.NeighDel(&netlink.Neigh{Family: unix.AF_INET, LinkIndex: bridge.Index, IP: foreign.IP}); err != nil {
		t.Fatal(err)
	}
	got, err = h.NeighList(bridge.Index, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range got {
		if n.IP.Equal(foreign.IP) {
			t.Fatal("historical IP-only hazard not reproduced")
		}
	}
	t.Log("fixture-only historical IP-only delete removes foreign replacement=REPRODUCED")

	// Reset only the exact remaining fixture-owned FDB, then exercise an
	// attribute omitted by netlink.Neigh in a new controller incarnation.
	if err := h.NeighDel(r.FDB.Observed.neigh()); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.StateDirectory = t.TempDir()
	next, err := NewOwnedOVNNetlinkBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	nextImporter, err := NewValidatedRemoteType2Importer(reader, next)
	if err != nil {
		t.Fatal(err)
	}
	applyOwned(t, nextImporter, ownedSnapshot(t, nextImporter, importPath(t, nil)))
	fdb := next.Receipts()[0].FDB.Observed.neigh()
	request := nl.NewNetlinkRequest(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: next.owned.kernel.(*ownedKernelHandle).dump}
	request.AddData(&netlink.Ndmsg{Family: unix.AF_BRIDGE, Index: uint32(vx.Index),
		State: uint16(fdb.State), Flags: uint8(fdb.Flags), Type: uint8(fdb.Type)})
	request.AddData(nl.NewRtAttr(netlink.NDA_LLADDR, fdb.HardwareAddr))
	request.AddData(nl.NewRtAttr(netlink.NDA_DST, fdb.IP.To4()))
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 60101)
	request.AddData(nl.NewRtAttr(netlink.NDA_PORT, port))
	if _, err := request.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		t.Fatal(err)
	}
	if err := nextImporter.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, nextImporter)); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("unsupported neighbor attribute %d", netlink.NDA_PORT)) {
		t.Fatal("unrepresented destination port not detected", err)
	}
	// The typed dump hides NDA_PORT, but must still show the preserved MAC/VTEP.
	got, err = h.NeighList(vx.Index, unix.AF_BRIDGE)
	preserved = false
	for _, n := range got {
		preserved = preserved || (n.IP.Equal(fdb.IP) && reflect.DeepEqual(n.HardwareAddr, fdb.HardwareAddr))
	}
	if err != nil || !preserved {
		t.Fatal("foreign FDB with port override removed", got, err)
	}
	t.Log("real-kernel unsupported NDA_PORT fences withdrawal and preserves foreign FDB=PASS")
}

func TestOwnedNeighborAttributes(t *testing.T) {
	for _, kind := range []int{netlink.NDA_PORT, netlink.NDA_PROTOCOL, netlink.NDA_NH_ID,
		netlink.NDA_SRC_VNI, netlink.NDA_LINK_NETNSID, netlink.NDA_FDB_EXT_ATTRS, 99} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			if err := validateOwnedNeighborAttributes(nl.NewRtAttr(kind, []byte{0, 0, 0, 0}).Serialize(), 7); err == nil {
				t.Fatal("unrepresented attribute accepted")
			}
		})
	}
	if err := validateOwnedNeighborAttributes(nl.NewRtAttr(netlink.NDA_FLAGS_EXT, []byte{1}).Serialize(), 7); err == nil {
		t.Fatal("truncated attribute accepted")
	}
	var attributes []byte
	for _, kind := range []int{netlink.NDA_PROBES, netlink.NDA_VNI, netlink.NDA_FLAGS_EXT} {
		attributes = append(attributes, nl.NewRtAttr(kind, []byte{0, 0, 0, 0}).Serialize()...)
	}
	if err := validateOwnedNeighborAttributes(attributes, 7); err != nil {
		t.Fatal(err)
	}
}
