package evpn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func recoveryRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned production journals")
	}
}

func recoveryDomain() ownership.Domain {
	return ownership.Domain{InstallationUID: "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID: "46b08606-9b26-4098-90d7-02c14098122b", BootID: "8a63e552-b620-445f-9ab6-8d924321ccfe",
		NetworkNamespace: "test-netns", ConfigDigest: strings.Repeat("a", 64)}
}

func persistentFixture(t *testing.T) (*RemoteType2Importer, *OVNNetlinkBackend, *memoryOwnedKernel) {
	t.Helper()
	recoveryRoot(t)
	i, b, k, _ := remoteOwnedFixture(t)
	s := b.owned
	d := recoveryDomain()
	s.controller = d.NodeUID
	s.target = d.BootID + "/" + d.NetworkNamespace
	s.config.StateDirectory = t.TempDir()
	s.config.Recovery = &RemoteNetlinkRecovery{Domain: d}
	if err := s.openRecovery(d.BootID, d.NetworkNamespace); err != nil {
		t.Fatal(err)
	}
	if err := s.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := acquireRemoteMarker(s.config.StateDirectory, s.controller, s.target); err != nil {
		t.Fatal(err)
	}
	s.markerPath = filepath.Join(s.config.StateDirectory, "remote-netlink-owner")
	t.Cleanup(func() { b.Close() })
	return i, b, k
}

func reopenMemory(t *testing.T, old *OVNNetlinkBackend, k *memoryOwnedKernel) (*RemoteType2Importer, *OVNNetlinkBackend, error) {
	t.Helper()
	cfg := old.owned.config
	old.Close()
	s := &ownedNetlinkState{kernel: k, config: cfg, controller: old.owned.controller,
		target: old.owned.target, devices: map[string]RemoteDeviceIdentity{}, receipts: map[string]*RemoteNetlinkReceipt{},
		latest: map[string]uint64{}, updates: make(chan netlink.LinkUpdate), done: make(chan struct{})}
	b := &OVNNetlinkBackend{owned: s}
	fail := func(err error) (*RemoteType2Importer, *OVNNetlinkBackend, error) { b.Close(); return nil, b, err }
	d := cfg.Recovery.Domain
	if err := s.openRecovery(d.BootID, d.NetworkNamespace); err != nil {
		return fail(err)
	}
	for name, expected := range s.devices {
		id, err := s.device(name)
		if err != nil || id != expected {
			return fail(fmt.Errorf("device identity changed: %s", name))
		}
	}
	for _, network := range cfg.Networks {
		if err := s.scanStartupKernelState(context.Background(), network); err != nil {
			return fail(err)
		}
	}
	if s.schemaMigration {
		if err := s.checkpoint(); err != nil {
			return fail(err)
		}
		s.schemaMigration = false
	}
	if err := s.verifyRecovered(context.Background()); err != nil {
		return fail(err)
	}
	reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
	if err != nil {
		return fail(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		return fail(err)
	}
	t.Cleanup(func() { b.Close() })
	return i, b, nil
}

func TestImporterRecoveryRepeatedRestart(t *testing.T) {
	i, b, k := persistentFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	original := b.Receipts()[0]
	mutations := k.mutations
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		i, b, err = reopenMemory(t, b, k)
		if err != nil {
			t.Fatal(err)
		}
		source := importTestSourceFor(t, i.verified)
		source.snap = RemoteRIBSnapshot{Complete: true, Paths: []*api.Path{p}}
		if err := i.ReconcileOnce(context.Background()); err == nil || k.mutations != mutations {
			t.Fatal("recovered snapshot mutated before two fresh observations")
		}
		if err := i.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if k.mutations != mutations || !reflect.DeepEqual(original, b.Receipts()[0]) {
			t.Fatal("restart duplicated or changed owned receipts")
		}
	}
	applyOwned(t, i, ownedSnapshot(t, i))
	if len(k.entries) != 0 || len(b.Receipts()) != 0 {
		t.Fatal("obsolete recovered entries not withdrawn")
	}
}

func TestImporterRecoveryPreservesForeignSlotsAndRestoresOwnedSlots(t *testing.T) {
	i, b, k := persistentFixture(t)
	p1 := importPath(t, nil)
	p2 := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.MacAddress = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x99}
		n.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	applyOwned(t, i, ownedSnapshot(t, i, p1, p2))
	foreignFDB := netlink.Neigh{Family: unix.AF_BRIDGE, LinkIndex: 3,
		IP: net.ParseIP("192.0.2.99"), HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 0xfa},
		State: netlink.NUD_PERMANENT, Type: unix.RTN_UNICAST}
	foreignNeighbor := netlink.Neigh{Family: unix.AF_INET, LinkIndex: 2,
		IP: net.ParseIP("10.250.0.99"), HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 0xfb},
		State: netlink.NUD_PERMANENT}
	k.entries = append(k.entries, foreignFDB, foreignNeighbor)
	mutations := k.mutations
	i, b, err := reopenMemory(t, b, k)
	if err != nil {
		t.Fatal(err)
	}
	if b.RecoveryState() != "RECOVERY_PARTIAL" || len(b.Quarantines()) != 1 || k.mutations != mutations {
		t.Fatalf("foreign slots did not remain isolated from owned recovery: state=%s quarantine=%+v mutations=%d/%d",
			b.RecoveryState(), b.Quarantines(), mutations, k.mutations)
	}
	for attempt := 0; attempt < 2; attempt++ {
		source := importTestSourceFor(t, i.verified)
		source.snap = RemoteRIBSnapshot{Complete: true, Paths: []*api.Path{p1, p2}}
		err := i.ReconcileOnce(context.Background())
		if attempt == 0 && err == nil {
			t.Fatal("recovered routes were reconciled before two fresh observations")
		}
		if attempt == 1 && err != nil {
			t.Fatal(err)
		}
	}
	if k.mutations != mutations || len(k.entries) != 6 {
		t.Fatalf("independent receipts were not recovered idempotently: mutations=%d/%d entries=%+v", k.mutations, mutations, k.entries)
	}
	for _, want := range []netlink.Neigh{foreignFDB, foreignNeighbor} {
		found := false
		for _, got := range k.entries {
			found = found || (got.Family == want.Family && got.LinkIndex == want.LinkIndex &&
				got.IP.Equal(want.IP) && reflect.DeepEqual(got.HardwareAddr, want.HardwareAddr))
		}
		if !found {
			t.Fatalf("foreign kernel state was changed or removed: %+v", want)
		}
	}
	if len(b.Quarantines()) != 1 || b.Quarantines()[0].Observed[0].IP != "192.0.2.99" {
		t.Fatalf("foreign FDB quarantine was not durably retained: %+v", b.Quarantines())
	}
}

func TestImporterRecoveryQuarantinesConflictAndRestoresIndependentRoute(t *testing.T) {
	i, b, k := persistentFixture(t)
	p1 := importPath(t, nil)
	p2 := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.MacAddress = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x99}
		n.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	applyOwned(t, i, ownedSnapshot(t, i, p1, p2))
	var p1MAC string
	var p2MAC string
	var p2IP string
	for _, receipt := range b.Receipts() {
		switch receipt.Endpoint.IP {
		case "10.251.1.11":
			p1MAC = receipt.Endpoint.MAC
		case "10.251.1.12":
			p2MAC, p2IP = receipt.Endpoint.MAC, receipt.Endpoint.IP
		}
	}
	if p1MAC == "" || p2MAC == "" || p2IP == "" {
		t.Fatal("test routes did not produce distinct endpoint receipts", b.Receipts())
	}
	var retained []netlink.Neigh
	for _, entry := range k.entries {
		switch {
		case entry.Family == unix.AF_BRIDGE && entry.HardwareAddr.String() == p1MAC:
			entry.IP = net.ParseIP("192.0.2.81")
			retained = append(retained, entry)
		case entry.Family == unix.AF_INET && entry.IP.String() == p2IP:
		case entry.Family == unix.AF_BRIDGE && entry.HardwareAddr.String() == p2MAC:
		default:
			retained = append(retained, entry)
		}
	}
	k.entries = retained
	mutations := k.mutations
	i, b, err := reopenMemory(t, b, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Quarantines()) != 1 || b.Quarantines()[0].Slot.Key != p1MAC || k.mutations != mutations {
		t.Fatalf("same-MAC conflict was not isolated: quarantine=%+v mutations=%d/%d", b.Quarantines(), mutations, k.mutations)
	}
	snapshot := ownedSnapshot(t, i, p1, p2)
	if err := i.applyValidatedSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("conflicting FDB slot was treated as mutable owned state")
	}
	if k.mutations != mutations+2 {
		t.Fatalf("independent missing route did not recover exactly two objects: mutations=%d/%d", k.mutations, mutations)
	}
	foreignPreserved, independentRestored := false, false
	for _, entry := range k.entries {
		foreignPreserved = foreignPreserved || (entry.Family == unix.AF_BRIDGE && entry.HardwareAddr.String() == p1MAC && entry.IP.String() == "192.0.2.81")
		independentRestored = independentRestored || (entry.Family == unix.AF_BRIDGE && entry.HardwareAddr.String() == p2MAC && entry.IP.String() == "192.0.2.80")
	}
	if !foreignPreserved || !independentRestored {
		t.Fatalf("conflicting slot or independent owned route has wrong state: %+v", k.entries)
	}
	mutations = k.mutations
	if err := i.applyValidatedSnapshot(context.Background(), ownedSnapshot(t, i)); err == nil {
		t.Fatal("withdrawal of a conflicted slot was not blocked")
	}
	if k.mutations != mutations+3 || len(b.Receipts()) != 1 || !b.Receipts()[0].Retiring {
		t.Fatalf("exact withdrawal did not stop at the protected FDB slot: mutations=%d/%d receipts=%+v", k.mutations, mutations, b.Receipts())
	}
	foreignPreserved, independentRemains := false, false
	for _, entry := range k.entries {
		foreignPreserved = foreignPreserved || (entry.Family == unix.AF_BRIDGE && entry.HardwareAddr.String() == p1MAC && entry.IP.String() == "192.0.2.81")
		independentRemains = independentRemains || entry.HardwareAddr.String() == p2MAC
	}
	if !foreignPreserved || independentRemains {
		t.Fatalf("withdrawal modified a protected slot or retained independent state: %+v", k.entries)
	}
}

func TestImporterQuarantineOrderingIsDeterministic(t *testing.T) {
	_, b, k := persistentFixture(t)
	one := netlink.Neigh{Family: unix.AF_BRIDGE, LinkIndex: 3, IP: net.ParseIP("192.0.2.99"),
		HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 0xfa}, State: netlink.NUD_PERMANENT}
	two := one
	two.IP = net.ParseIP("192.0.2.98")
	k.entries = []netlink.Neigh{one, two}
	_, b, err := reopenMemory(t, b, k)
	if err != nil || len(b.Quarantines()) != 1 {
		t.Fatalf("initial conflict classification failed: q=%+v err=%v", b.Quarantines(), err)
	}
	want := b.Quarantines()[0]
	k.entries[0], k.entries[1] = k.entries[1], k.entries[0]
	_, b, err = reopenMemory(t, b, k)
	if err != nil || len(b.Quarantines()) != 1 || !reflect.DeepEqual(want, b.Quarantines()[0]) {
		t.Fatalf("dump order changed durable quarantine representation: want=%+v got=%+v err=%v", want, b.Quarantines(), err)
	}
}

func TestImporterQuarantineRequiresExplicitResolution(t *testing.T) {
	_, b, k := persistentFixture(t)
	foreign := netlink.Neigh{Family: unix.AF_BRIDGE, LinkIndex: 3, IP: net.ParseIP("192.0.2.99"),
		HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 0xfa}, State: netlink.NUD_PERMANENT}
	k.entries = append(k.entries, foreign)
	_, b, err := reopenMemory(t, b, k)
	if err != nil || len(b.Quarantines()) != 1 {
		t.Fatalf("startup quarantine failed: %+v %v", b.Quarantines(), err)
	}
	k.entries = nil
	_, b, err = reopenMemory(t, b, k)
	if err != nil || len(b.Quarantines()) != 1 || b.Quarantines()[0].Resolved {
		t.Fatalf("an absent slot automatically cleared durable quarantine: %+v %v", b.Quarantines(), err)
	}
	q := b.Quarantines()[0]
	if err := b.ResolveQuarantinedSlot(context.Background(), q.Slot, "operator verified foreign entry removal"); err != nil {
		t.Fatal(err)
	}
	if len(b.Quarantines()) != 1 || !b.Quarantines()[0].Resolved {
		t.Fatalf("evidence-backed resolution was not persisted: %+v", b.Quarantines())
	}
	mutations := k.mutations
	_, b, err = reopenMemory(t, b, k)
	if err != nil || b.RecoveryState() == "RECOVERY_BLOCKED" || !b.Quarantines()[0].Resolved || k.mutations != mutations {
		t.Fatalf("resolved quarantine did not survive restart idempotently: state=%s q=%+v err=%v", b.RecoveryState(), b.Quarantines(), err)
	}
}

func TestImporterRecoveryMigratesV1WithoutClearingGlobalQuarantine(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) {
			_, b, k := persistentFixture(t)
			state := b.owned.durableState()
			state.Version = 1
			state.Quarantines = nil
			if global {
				state.Quarantine = "legacy global uncertainty"
			}
			seq, err := b.owned.journal.Begin("legacy-v1-state", state)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.owned.journal.Commit(seq, state); err != nil {
				t.Fatal(err)
			}
			_, reopened, err := reopenMemory(t, b, k)
			if global {
				if err == nil {
					t.Fatalf("legacy global quarantine was weakened: state=%s", reopened.RecoveryState())
				}
				return
			}
			if err != nil || reopened.RecoveryState() == "RECOVERY_BLOCKED" {
				t.Fatalf("valid v1 journal did not migrate safely: state=%s err=%v", reopened.RecoveryState(), err)
			}
			var migrated remoteReceiptState
			raw, pending, err := reopened.owned.journal.Snapshot()
			if err != nil || pending != nil {
				t.Fatalf("migrated journal is not committed: pending=%+v err=%v", pending, err)
			}
			if err := json.Unmarshal(raw, &migrated); err != nil {
				t.Fatal(err)
			}
			if migrated.Version != 2 || migrated.Quarantines == nil || migrated.Quarantine != "" {
				t.Fatalf("v1 migration did not produce explicit v2 state: %+v", migrated)
			}
		})
	}
}

func TestImporterRecoveryAbsenceForeignAndCorruption(t *testing.T) {
	for _, mode := range []string{"missing-owned", "foreign-matching-unattempted", "foreign-neighbor-unattempted", "foreign-changed", "dump-failure", "corrupt", "marker-only", "marker-changed", "domain", "device"} {
		t.Run(mode, func(t *testing.T) {
			i, b, k := persistentFixture(t)
			p := importPath(t, nil)
			applyOwned(t, i, ownedSnapshot(t, i, p))
			switch mode {
			case "missing-owned":
				k.entries = nil
			case "foreign-matching-unattempted", "foreign-neighbor-unattempted":
				// Exact tuple equality cannot turn an unattempted receipt into ownership.
				r := b.owned.receipts[b.Receipts()[0].Endpoint.Key]
				o := &r.FDB
				if mode == "foreign-neighbor-unattempted" {
					o = r.Neighbor
				}
				o.Status, o.ACK = "unattempted", false
				o.Observed = RemoteKernelEntry{}
				if err := b.owned.checkpoint(); err != nil {
					t.Fatal(err)
				}
			case "foreign-changed":
				k.entries[0].IP = net.ParseIP("192.0.2.199")
			case "dump-failure":
				k.listError = fmt.Errorf("unavailable kernel dump")
			case "corrupt":
				if err := os.WriteFile(filepath.Join(b.owned.config.StateDirectory, "receipts-v1", "journal.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker-only":
				if err := os.Rename(filepath.Join(b.owned.config.StateDirectory, "receipts-v1"), filepath.Join(b.owned.config.StateDirectory, "archived")); err != nil {
					t.Fatal(err)
				}
			case "marker-changed":
				if err := os.WriteFile(b.owned.markerPath, []byte("foreign\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "domain":
				b.owned.config.Recovery.Domain.NodeUID = b.owned.config.Recovery.Domain.InstallationUID
			case "device":
				b.owned.devices["test-vx"] = RemoteDeviceIdentity{Name: "other"}
				if err := b.owned.checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			mutations := k.mutations
			i, b, err := reopenMemory(t, b, k)
			quarantined := mode == "foreign-matching-unattempted" ||
				mode == "foreign-neighbor-unattempted" || mode == "foreign-changed"
			if quarantined {
				if err != nil || len(b.Quarantines()) == 0 || k.mutations != mutations {
					t.Fatalf("slot conflict did not recover independently and remain untouched: quarantines=%+v err=%v", b.Quarantines(), err)
				}
				return
			}
			if mode != "missing-owned" {
				if err == nil || k.mutations != mutations {
					t.Fatalf("unsafe recovery accepted or mutated: %v", err)
				}
				return
			}
			if err != nil || len(k.entries) != 0 {
				t.Fatal("verified absence could not recover without startup mutation", err)
			}
			applyOwned(t, i, ownedSnapshot(t, i, p))
			if len(k.entries) != 2 || k.mutations != mutations+2 {
				t.Fatal("missing owned state did not converge after fresh snapshot")
			}
			if b.Receipts()[0].Incarnation == 0 {
				t.Fatal("receipt incarnation lost")
			}
		})
	}
}

func TestImporterRecoveryRetiringRouteReappears(t *testing.T) {
	i, b, k := persistentFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	old := b.Receipts()[0]
	r := b.owned.receipts[old.Endpoint.Key]
	r.Retiring, r.Status = true, "partially-withdrawn"
	if err := b.owned.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := b.owned.withdrawObject(context.Background(), r.Neighbor); err != nil {
		t.Fatal(err)
	}
	i, b, err := reopenMemory(t, b, k)
	if err != nil {
		t.Fatal(err)
	}
	applyOwned(t, i, ownedSnapshot(t, i, p))
	current := b.Receipts()[0]
	if current.Incarnation <= old.Incarnation || current.Retiring || current.Status != "applied" ||
		len(k.entries) != 2 || len(b.Withdrawals()) != 1 {
		t.Fatal("retiring receipt revived instead of withdrawn and replaced", current)
	}
	if err := b.Withdraw(context.Background(), receiptRoute(&old)); err == nil {
		t.Fatal("old incarnation deleted recovered replacement")
	}
}

func TestImporterRecoveryDurableQuarantine(t *testing.T) {
	i, b, k := persistentFixture(t)
	applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	route := i.active[b.Receipts()[0].Endpoint.Key]
	b.owned.updates <- netlink.LinkUpdate{Link: k.links["test-vx"], Header: unix.NlMsghdr{Type: unix.RTM_DELLINK}}
	if err := b.Apply(context.Background(), route); err == nil {
		t.Fatal("known device lifecycle loss not blocked")
	}
	// Device names, indices, tuples and all current observations match again.
	// A new process must still retain the previous owner's observed conflict.
	if _, _, err := reopenMemory(t, b, k); err == nil {
		t.Fatal("restart erased a durably observed ownership quarantine")
	}
}

func kernelRecoveryFixture(t *testing.T) (RemoteNetlinkConfig, netns.NsHandle, *netlink.Handle) {
	t.Helper()
	recoveryRoot(t)
	if os.Getenv("EVPN_OWNED_KERNEL") != "disposable" {
		t.Skip("requires disposable privileged runtime")
	}
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "recover-br"}}
	if err := h.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(br); err != nil {
		t.Fatal(err)
	}
	addTestLocalVTEP(t, h)
	vx := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "recover-vx", MasterIndex: br.Index}, VxlanId: 100, Port: 60100, SrcAddr: net.ParseIP("192.0.2.32")}
	if err := h.LinkAdd(vx); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(vx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.Close()
		if err := netns.Set(original); err != nil {
			panic(err)
		}
		ns.Close()
		original.Close()
		runtime.UnlockOSThread()
	})
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	d := recoveryDomain()
	d.BootID, d.NetworkNamespace = strings.TrimSpace(string(boot)), ns.UniqueId()
	return RemoteNetlinkConfig{Enabled: true, ExclusiveWriter: true, StateDirectory: t.TempDir(),
		Recovery: &RemoteNetlinkRecovery{Domain: d},
		Networks: []RemoteNetlinkNetwork{{NetworkID: "remote-test", LogicalSwitch: "test-ls", VNI: 100,
			BridgeDevice: "recover-br", VXLANDevice: "recover-vx"}}}, ns, h
}

func kernelImporter(t *testing.T, cfg RemoteNetlinkConfig) (*RemoteType2Importer, *OVNNetlinkBackend) {
	t.Helper()
	b, err := NewOwnedOVNNetlinkBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	return i, b
}

func TestImporterRecoveryKernelRestart(t *testing.T) {
	cfg, _, h := kernelRecoveryFixture(t)
	i, b := kernelImporter(t, cfg)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	before := b.Receipts()[0]
	if _, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
		t.Fatal("overlapping journal writers accepted")
	}
	b.Close()
	i, b = kernelImporter(t, cfg)
	defer b.Close()
	applyOwned(t, i, ownedSnapshot(t, i, p))
	if !reflect.DeepEqual(before, b.Receipts()[0]) {
		t.Fatal("real kernel recovery changed receipts")
	}
	applyOwned(t, i, ownedSnapshot(t, i))
	for _, scope := range []struct{ index, family int }{{before.FDB.Request.Ifindex, unix.AF_BRIDGE}, {before.Neighbor.Request.Ifindex, unix.AF_INET}} {
		entries, err := h.NeighList(scope.index, scope.family)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range entries {
			if n.HardwareAddr.String() == before.Endpoint.MAC {
				t.Fatal("obsolete owned state remains")
			}
		}
	}
}

func TestImporterRecoveryKernelIdentityChange(t *testing.T) {
	for _, mode := range []string{"namespace", "device"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, h := kernelRecoveryFixture(t)
			i, b := kernelImporter(t, cfg)
			applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
			b.Close()
			if mode == "namespace" {
				cfg.Recovery.Domain.NetworkNamespace = "different-network-namespace"
			} else {
				link, err := h.LinkByName("recover-vx")
				if err != nil {
					t.Fatal(err)
				}
				if err := h.LinkDel(link); err != nil {
					t.Fatal(err)
				}
				bridge, err := h.LinkByName("recover-br")
				if err != nil {
					t.Fatal(err)
				}
				replacement := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "recover-vx",
					MasterIndex: bridge.Attrs().Index}, VxlanId: 100, Port: 60100, SrcAddr: net.ParseIP("192.0.2.32")}
				if err := h.LinkAdd(replacement); err != nil {
					t.Fatal(err)
				}
			}
			if b, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
				b.Close()
				t.Fatal("changed namespace/device identity accepted")
			}
		})
	}
}

func TestImporterRecoveryCrashHelper(t *testing.T) {
	path := os.Getenv("EVPN_IMPORT_CRASH_CONFIG")
	if path == "" {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(netns.NsHandle(3)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg RemoteNetlinkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	i, b := kernelImporter(t, cfg)
	defer b.Close()
	boundary, occurrence := os.Getenv("EVPN_IMPORT_CRASH_BOUNDARY"), os.Getenv("EVPN_IMPORT_CRASH_OCCURRENCE")
	count := 0
	b.owned.fault = func(name string) {
		if name == boundary {
			count++
			if fmt.Sprint(count) == occurrence {
				fmt.Println("mutation-boundary-ready")
				select {}
			}
		}
	}
	if os.Getenv("EVPN_IMPORT_CRASH_DELETE") == "1" {
		applyOwned(t, i, ownedSnapshot(t, i))
	} else {
		applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	}
	t.Fatal("requested mutation boundary not reached")
}

func TestImporterRecoveryKernelSIGKILL(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		for _, boundary := range []string{"before-add", "after-add", "before-add-ack", "after-add-ack"} {
			if deletion {
				boundary = strings.ReplaceAll(boundary, "add", "delete")
			}
			for _, occurrence := range []string{"1", "2"} {
				t.Run(fmt.Sprintf("delete=%t/%s/%s", deletion, boundary, occurrence), func(t *testing.T) {
					cfg, ns, _ := kernelRecoveryFixture(t)
					i, b := kernelImporter(t, cfg)
					if deletion {
						applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
					}
					b.Close()
					data, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(t.TempDir(), "config.json")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command(os.Args[0], "-test.run=^TestImporterRecoveryCrashHelper$")
					cmd.Env = append(os.Environ(), "EVPN_IMPORT_CRASH_CONFIG="+path,
						"EVPN_IMPORT_CRASH_BOUNDARY="+boundary, "EVPN_IMPORT_CRASH_OCCURRENCE="+occurrence,
						fmt.Sprintf("EVPN_IMPORT_CRASH_DELETE=%d", map[bool]int{true: 1}[deletion]))
					fd, err := unix.Dup(int(ns))
					if err != nil {
						t.Fatal(err)
					}
					nsFile := os.NewFile(uintptr(fd), "netns")
					defer nsFile.Close()
					cmd.ExtraFiles = []*os.File{nsFile}
					out, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					cmd.Stderr = os.Stderr
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { cmd.Process.Kill() })
					ready := make(chan bool, 1)
					go func() {
						scanner := bufio.NewScanner(out)
						for scanner.Scan() {
							if scanner.Text() == "mutation-boundary-ready" {
								ready <- true
								return
							}
						}
						ready <- false
					}()
					select {
					case ok := <-ready:
						if !ok {
							t.Fatal("child did not reach actual Netlink boundary")
						}
					case <-time.After(15 * time.Second):
						t.Fatal("mutation child timed out")
					}
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					cmd.Wait()
					recovered, err := NewOwnedOVNNetlinkBackend(cfg)
					if boundary != "after-add-ack" && boundary != "after-delete-ack" {
						if err == nil {
							recovered.Close()
							t.Fatal("unacknowledged mutation recovered from tuple matching")
						}
						return
					}
					if err != nil {
						t.Fatal("durably acknowledged operation could not recover", err)
					}
					reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
					if err != nil {
						t.Fatal(err)
					}
					i, err = NewValidatedRemoteType2Importer(reader, recovered)
					if err != nil {
						t.Fatal(err)
					}
					if deletion {
						applyOwned(t, i, ownedSnapshot(t, i))
						if len(recovered.Receipts()) != 0 {
							t.Fatal("retiring receipt not completed")
						}
					} else {
						applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
						if recovered.Receipts()[0].Status != "applied" {
							t.Fatal("partial acknowledged add not completed")
						}
					}
					recovered.Close()
				})
			}
		}
	}
}
