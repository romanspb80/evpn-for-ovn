package evpn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type RemoteNetlinkNetwork struct {
	NetworkID     string
	LogicalSwitch string
	VNI           uint32
	BridgeDevice  string
	VXLANDevice   string
}

type RemoteNetlinkConfig struct {
	Enabled         bool
	ExclusiveWriter bool
	StateDirectory  string
	Networks        []RemoteNetlinkNetwork
	Recovery        *RemoteNetlinkRecovery
}

// Recovery is opt-in. The runtime binds ConfigDigest to verified OVN UUIDs and
// import policy; the backend also checks the actual boot and network namespace.
type RemoteNetlinkRecovery struct {
	Domain ownership.Domain
}

func (c RemoteNetlinkConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.ExclusiveWriter || !filepath.IsAbs(c.StateDirectory) || len(c.Networks) == 0 {
		return fmt.Errorf("remote Netlink requires explicit exclusive writer, absolute state directory and networks")
	}
	names, vnis, devices := map[string]bool{}, map[uint32]bool{}, map[string]bool{}
	for _, n := range c.Networks {
		if strings.TrimSpace(n.NetworkID) == "" || strings.TrimSpace(n.LogicalSwitch) == "" ||
			n.VNI == 0 || n.VNI > 16777215 || names[n.NetworkID] || vnis[n.VNI] {
			return fmt.Errorf("unique explicit network/switch/VNI mapping required")
		}
		for _, name := range []string{n.BridgeDevice, n.VXLANDevice} {
			if name == "" || len(name) > 15 || strings.ContainsAny(name, "/ \t\n") || devices[name] {
				return fmt.Errorf("distinct explicit Linux device names required")
			}
			devices[name] = true
		}
		names[n.NetworkID], vnis[n.VNI] = true, true
	}
	return nil
}

type RemoteDeviceIdentity struct {
	Name        string
	Ifindex     int
	Kind        string
	MasterIndex int
	VNI         int
	Local       string
	Port        int
	MAC         string
	Alias       string
}

type RemoteKernelEntry struct {
	Family   int
	Ifindex  int
	MAC      string
	IP       string
	State    int
	Flags    int
	FlagsExt int
	Type     int
	VLAN     int
	VNI      int
	Master   int
}

type RemoteObjectReceipt struct {
	Request   RemoteKernelEntry
	Observed  RemoteKernelEntry
	Status    string
	ACK       bool
	DeleteACK bool
	Error     string
}

type RemoteNetlinkReceipt struct {
	Controller  string
	Target      string
	Network     RemoteNetlinkNetwork
	Endpoint    DesiredRemoteEndpoint
	Incarnation uint64
	Generation  uint64
	Devices     []RemoteDeviceIdentity
	FDB         RemoteObjectReceipt
	Neighbor    *RemoteObjectReceipt
	Status      string
	Retiring    bool
}

type ownedKernel interface {
	LinkByName(string) (netlink.Link, error)
	NeighList(int, int) ([]netlink.Neigh, error)
	NeighAdd(*netlink.Neigh) error
	NeighDel(*netlink.Neigh) error
	Close() error
}

type ownedNetlinkState struct {
	mu                 sync.Mutex
	kernel             ownedKernel
	config             RemoteNetlinkConfig
	controller, target string
	devices            map[string]RemoteDeviceIdentity
	receipts           map[string]*RemoteNetlinkReceipt
	withdrawals        []RemoteNetlinkReceipt
	latest             map[string]uint64
	updates            chan netlink.LinkUpdate
	done               chan struct{}
	lost               atomic.Bool
	blocked            string
	closed             bool
	markerPath         string
	subscribed         bool
	journal            *ownership.Journal
	snapshot           RemoteImportSnapshot
	recoveryState      string
	// Tests stop a process at actual mutation boundaries.
	fault func(string)
}

// NewOwnedOVNNetlinkBackend binds the existing backend to this process's netns.
// The restart marker is intentionally never removed by Close.
func NewOwnedOVNNetlinkBackend(config RemoteNetlinkConfig) (*OVNNetlinkBackend, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, fmt.Errorf("remote Netlink writes are disabled")
	}
	ns, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer ns.Close()
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	kernel, err := newOwnedKernelHandle(ns)
	if err != nil {
		return nil, err
	}
	if err := kernel.SetSocketTimeout(2 * time.Second); err != nil {
		kernel.Close()
		return nil, err
	}
	s := &ownedNetlinkState{kernel: kernel, config: config, controller: uuid.NewString(),
		target:  strings.TrimSpace(string(boot)) + "/" + ns.UniqueId(),
		devices: map[string]RemoteDeviceIdentity{}, receipts: map[string]*RemoteNetlinkReceipt{},
		latest: map[string]uint64{}, updates: make(chan netlink.LinkUpdate, 256), done: make(chan struct{})}
	s.config.Networks = append([]RemoteNetlinkNetwork(nil), config.Networks...)
	err = netlink.LinkSubscribeWithOptions(s.updates, s.done, netlink.LinkSubscribeOptions{
		Namespace: &ns, ReceiveBufferSize: 1 << 20,
		ErrorCallback: func(error) { s.lost.Store(true) },
	})
	if err != nil {
		kernel.Close()
		return nil, fmt.Errorf("link lifetime subscription: %w", err)
	}
	s.subscribed = true
	b := &OVNNetlinkBackend{owned: s}
	if config.Recovery != nil {
		if err := s.openRecovery(strings.TrimSpace(string(boot)), ns.UniqueId()); err != nil {
			b.Close()
			return nil, err
		}
	}
	for _, network := range config.Networks {
		for _, name := range []string{network.BridgeDevice, network.VXLANDevice} {
			id, err := s.device(name)
			if err != nil {
				b.Close()
				return nil, err
			}
			if expected, ok := s.devices[name]; ok && expected != id {
				s.blocked = "managed device identity changed: " + name
				err := s.quarantineError()
				b.Close()
				return nil, err
			}
			s.devices[name] = id
		}
		bridge, vxlan := s.devices[network.BridgeDevice], s.devices[network.VXLANDevice]
		if bridge.Kind != "bridge" || vxlan.Kind != "vxlan" || vxlan.VNI != int(network.VNI) ||
			vxlan.MasterIndex != bridge.Ifindex {
			b.Close()
			return nil, fmt.Errorf("network %s has invalid bridge/VXLAN topology", network.NetworkID)
		}
		for _, scope := range []struct{ index, family int }{
			{vxlan.Ifindex, unix.AF_BRIDGE}, {bridge.Ifindex, unix.AF_INET}, {bridge.Ifindex, unix.AF_INET6},
		} {
			entries, err := kernel.NeighList(scope.index, scope.family)
			if err != nil {
				b.Close()
				return nil, fmt.Errorf("initial remote-domain observation: %w", err)
			}
			for _, entry := range entries {
				if entry.Flags&netlink.NTF_EXT_LEARNED != 0 ||
					(scope.family == unix.AF_BRIDGE && len(entry.IP) != 0) {
					if s.journal == nil || !s.hasConfirmedEntry(kernelEntry(entry)) {
						s.blocked = fmt.Sprintf("network %s has unowned remote kernel state; no startup adoption", network.NetworkID)
						err := s.quarantineError()
						b.Close()
						return nil, err
					}
				}
			}
		}
	}
	if err := s.check(context.Background()); err != nil {
		b.Close()
		return nil, err
	}
	if s.markerPath == "" {
		if s.journal != nil {
			if err := s.checkpoint(); err != nil {
				b.Close()
				return nil, err
			}
		}
		if err := acquireRemoteMarker(config.StateDirectory, s.controller, s.target); err != nil {
			b.Close()
			return nil, err
		}
		s.markerPath = filepath.Join(config.StateDirectory, "remote-netlink-owner")
	}
	if s.journal != nil {
		if err := s.verifyRecovered(context.Background()); err != nil {
			b.Close()
			return nil, err
		}
		s.recoveryState = "RECONCILING_STATE"
	}
	return b, nil
}

func acquireRemoteMarker(directory, controller, target string) error {
	dir, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != 0 || stat.Mode&0022 != 0 {
		return fmt.Errorf("remote state directory must be root-owned and not group/world writable")
	}
	fd, err := unix.Openat(int(dir.Fd()), "remote-netlink-owner", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("remote ownership marker unavailable; no restart adoption: %w", err)
	}
	f := os.NewFile(uintptr(fd), "remote-netlink-owner")
	_, writeErr := fmt.Fprintf(f, "%s\n%s\n", controller, target)
	syncErr := f.Sync()
	closeErr := f.Close()
	dirErr := dir.Sync()
	return errors.Join(writeErr, syncErr, closeErr, dirErr)
}

func (b *OVNNetlinkBackend) Close() error {
	if b == nil || b.owned == nil {
		return nil
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
		if s.subscribed {
			// The library's sender can be blocked on a full update channel.
			go func() {
				for range s.updates {
				}
			}()
		}
		return errors.Join(s.kernel.Close(), closeOwnershipJournal(s.journal))
	}
	return nil
}

func (b *OVNNetlinkBackend) Receipts() []RemoteNetlinkReceipt {
	if b == nil || b.owned == nil {
		return nil
	}

	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RemoteNetlinkReceipt
	for _, receipt := range s.receipts {
		// Deep copy nested route/attribute slices before exposing status.
		data, _ := json.Marshal(receipt)
		var copy RemoteNetlinkReceipt
		_ = json.Unmarshal(data, &copy)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint.Key < out[j].Endpoint.Key })
	return out
}

// Withdrawals is a bounded diagnostic journal, never restart ownership proof.
func (b *OVNNetlinkBackend) Withdrawals() []RemoteNetlinkReceipt {
	if b == nil || b.owned == nil {
		return nil
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.withdrawals)
	var copy []RemoteNetlinkReceipt
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (s *ownedNetlinkState) device(name string) (RemoteDeviceIdentity, error) {
	link, err := s.kernel.LinkByName(name)
	if err != nil {
		return RemoteDeviceIdentity{}, err
	}
	return remoteDeviceIdentity(link), nil
}

func remoteDeviceIdentity(link netlink.Link) RemoteDeviceIdentity {
	a := link.Attrs()
	id := RemoteDeviceIdentity{Name: a.Name, Ifindex: a.Index, Kind: link.Type(), MasterIndex: a.MasterIndex,
		MAC: a.HardwareAddr.String(), Alias: a.Alias}
	if v, ok := link.(*netlink.Vxlan); ok {
		id.VNI, id.Local, id.Port = v.VxlanId, v.SrcAddr.String(), v.Port
	}
	return id
}

func (s *ownedNetlinkState) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return fmt.Errorf("remote backend is closed")
	}
	if s.journal != nil {
		if _, _, err := s.journal.Snapshot(); err != nil {
			s.blocked = err.Error()
		}
	}
	if s.markerPath != "" && s.journal != nil {
		if err := s.verifyMarker(); err != nil {
			s.blocked = err.Error()
		}
	} else if s.markerPath != "" {
		f, err := os.OpenFile(s.markerPath, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			s.blocked = "ownership marker lost"
		} else {
			var value [512]byte
			n, readErr := f.Read(value[:])
			closeErr := f.Close()
			if readErr != nil || closeErr != nil || string(value[:n]) != s.controller+"\n"+s.target+"\n" {
				s.blocked = "ownership marker changed or unreadable"
			}
		}
	}
	for {
		select {
		case update, ok := <-s.updates:
			if !ok {
				s.lost.Store(true)
				goto drained
			}
			if update.Link == nil {
				s.lost.Store(true)
				continue
			}
			for name, id := range s.devices {
				if update.Attrs().Index == id.Ifindex || update.Attrs().Name == name {
					if s.blocked == "" && linkLifetimeChanged(update, id) {
						s.blocked = fmt.Sprintf("managed device lifecycle/identity event; receipts invalidated: family=%d got=%+v expected=%+v",
							update.IfInfomsg.Family, remoteDeviceIdentity(update.Link), id)
					}
				}
			}
		default:
			goto drained
		}
	}
drained:
	if s.lost.Load() && s.blocked == "" {
		s.blocked = "link observation continuity lost"
	}
	if s.blocked != "" {
		return s.quarantineError()
	}
	for name, expected := range s.devices {
		got, err := s.device(name)
		if err != nil || got != expected {
			s.blocked = fmt.Sprintf("device identity changed: %s: %v", name, err)
			return s.quarantineError()
		}
	}
	return nil
}

func linkLifetimeChanged(update netlink.LinkUpdate, prior RemoteDeviceIdentity) bool {
	if update.Header.Type == unix.RTM_DELLINK || update.Link == nil {
		return true
	}
	if update.Attrs().Index != prior.Ifindex || update.Attrs().Name != prior.Name {
		return true
	}
	// AF_BRIDGE notifications describe bridge-port state, not complete device
	// topology (VXLAN info_data is absent). A full GETLINK follows the drain.
	return update.IfInfomsg.Family != unix.AF_BRIDGE && remoteDeviceIdentity(update.Link) != prior
}

func kernelEntry(n netlink.Neigh) RemoteKernelEntry {
	ip := ""
	if len(n.IP) != 0 {
		ip = n.IP.String()
	}
	return RemoteKernelEntry{Family: n.Family, Ifindex: n.LinkIndex, MAC: n.HardwareAddr.String(),
		IP: ip, State: n.State, Flags: n.Flags, FlagsExt: n.FlagsExt, Type: n.Type,
		VLAN: n.Vlan, VNI: n.VNI, Master: n.MasterIndex}
}

func (e RemoteKernelEntry) neigh() *netlink.Neigh {
	mac, _ := net.ParseMAC(e.MAC)
	return &netlink.Neigh{LinkIndex: e.Ifindex, Family: e.Family, State: e.State, Flags: e.Flags,
		FlagsExt: e.FlagsExt, Type: e.Type, IP: net.ParseIP(e.IP), HardwareAddr: mac, Vlan: e.VLAN,
		VNI: e.VNI, MasterIndex: e.Master}
}

func sameKernelSlot(a, b RemoteKernelEntry) bool {
	if a.Family != b.Family || a.Ifindex != b.Ifindex {
		return false
	}
	if a.Family == unix.AF_BRIDGE {
		return a.MAC == b.MAC // Deliberately reserve all destinations/scopes for this MAC.
	}
	return a.IP == b.IP
}

func (s *ownedNetlinkState) slot(ctx context.Context, want RemoteKernelEntry) ([]RemoteKernelEntry, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	entries, err := s.kernel.NeighList(want.Ifindex, want.Family)
	if err != nil {
		s.blocked = "kernel observation failed: " + err.Error()
		return nil, s.quarantineError()
	}
	var found []RemoteKernelEntry
	for _, n := range entries {
		e := kernelEntry(n)
		if sameKernelSlot(e, want) {
			found = append(found, e)
		}
	}
	return found, nil
}

func (s *ownedNetlinkState) confirm(ctx context.Context, r *RemoteObjectReceipt, absent bool) error {
	found, err := s.slot(ctx, r.Request)
	if err != nil {
		return err
	}
	if absent {
		if len(found) == 0 {
			r.Status, r.Error = "absent-confirmed", ""
			return nil
		}
	} else if len(found) == 1 {
		expected := r.Request
		if r.Status == "confirmed" {
			expected = r.Observed
		} else if expected.Type == 0 && expected.Family != unix.AF_BRIDGE &&
			(found[0].Type == unix.RTN_UNICAST || found[0].Type == unix.RTN_BROADCAST) {
			// Linux derives neighbor type from the device's IP configuration.
			// Capture it after ACK, then require that exact value on reuse.
			expected.Type = found[0].Type
		}
		if found[0] == expected {
			r.Observed, r.Status, r.Error = found[0], "confirmed", ""
			return nil
		}
	}
	s.blocked = "kernel entry differs from confirmed operation"
	r.Status, r.Error = "conflict", s.blocked
	return errors.Join(s.quarantineError(), fmt.Errorf("want=%+v observed=%+v", r.Request, found))
}

func definitiveReject(err error) bool {
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case unix.EEXIST, unix.EPERM, unix.EACCES, unix.EINVAL, unix.EOPNOTSUPP, unix.ENOENT, unix.ESRCH:
		return true
	default:
		return false
	}
}

func (s *ownedNetlinkState) applyObject(ctx context.Context, r *RemoteObjectReceipt) error {
	if r.Status == "confirmed" {
		return s.confirm(ctx, r, false)
	}
	found, err := s.slot(ctx, r.Request)
	if err != nil {
		return err
	}
	if len(found) != 0 {
		r.Status, r.Error = "conflict", "preexisting kernel slot is not owned"
		s.blocked = r.Error
		return errors.Join(s.quarantineError(), fmt.Errorf("observed=%+v", found))
	}
	if err := s.check(ctx); err != nil {
		return err
	}
	r.Status = "pending"
	seq, err := s.beginMutation("netlink-add", r)
	if err != nil {
		return err
	}
	found, err = s.slot(ctx, r.Request)
	if err != nil {
		return err
	}
	if len(found) != 0 {
		s.blocked = "kernel slot changed while persisting creation intent"
		return fmt.Errorf("%s", s.blocked)
	}
	s.boundary("before-add")
	err = s.kernel.NeighAdd(r.Request.neigh())
	s.boundary("after-add")
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		if !definitiveReject(err) {
			r.Status, s.blocked = "outcome-unknown", "unacknowledged kernel creation"
		}
		if s.journal != nil {
			s.blocked, s.recoveryState = "unresolved durable Netlink add intent: "+err.Error(), "RECOVERY_BLOCKED"
		}
		return fmt.Errorf("exclusive kernel creation: %w", err)
	}
	r.ACK = true
	if err := s.confirm(ctx, r, false); err != nil {
		r.Status = "outcome-unknown"
		s.blocked = "creation ACK without verified readback"
		return err
	}
	s.boundary("before-add-ack")
	if err := s.ackMutation(seq); err != nil {
		return err
	}
	s.boundary("after-add-ack")
	return nil
}

func (s *ownedNetlinkState) withdrawObject(ctx context.Context, r *RemoteObjectReceipt) error {
	if r.Status == "absent-confirmed" {
		return nil
	}
	if r.Status == "failed" || r.Status == "unattempted" {
		found, err := s.slot(ctx, r.Request)
		if err != nil {
			return err
		}
		if len(found) != 0 {
			s.blocked = "unowned slot appeared during partial withdrawal"
			return fmt.Errorf("%s", s.blocked)
		}
		r.Status = "absent-confirmed"
		return nil
	}
	if r.Status != "confirmed" || !r.ACK {
		return fmt.Errorf("no confirmed receipt for withdrawal: %s", r.Status)
	}
	if err := s.confirm(ctx, r, false); err != nil {
		return err
	}
	if err := s.check(ctx); err != nil {
		return err
	}
	// Including the full tuple is not kernel-level ownership CAS.
	seq, err := s.beginMutation("netlink-delete", r)
	if err != nil {
		return err
	}
	if err := s.confirm(ctx, r, false); err != nil {
		return err
	}
	s.boundary("before-delete")
	err = s.kernel.NeighDel(r.Observed.neigh())
	s.boundary("after-delete")
	if err != nil {
		r.Error = err.Error()
		if !definitiveReject(err) {
			r.Status, s.blocked = "outcome-unknown", "unacknowledged kernel deletion"
		}
		if s.journal != nil {
			s.blocked, s.recoveryState = "unresolved durable Netlink delete intent: "+err.Error(), "RECOVERY_BLOCKED"
		}
		return fmt.Errorf("owned kernel deletion: %w", err)
	}
	r.DeleteACK = true
	if err := s.confirm(ctx, r, true); err != nil {
		r.Status, s.blocked = "outcome-unknown", "deletion ACK without verified absence"
		return err
	}
	s.boundary("before-delete-ack")
	if err := s.ackMutation(seq); err != nil {
		return err
	}
	s.boundary("after-delete-ack")
	return nil
}

func (s *ownedNetlinkState) route(ctx context.Context, route ImportedType2Route) (RemoteNetlinkNetwork, error) {
	if err := s.check(ctx); err != nil {
		return RemoteNetlinkNetwork{}, err
	}
	if route.validated == nil || route.incarnation == 0 || route.generation == 0 ||
		route.validated.ValidationState != "validated" || len(route.validated.Routes) == 0 {
		return RemoteNetlinkNetwork{}, fmt.Errorf("verified importer proof and route incarnation required")
	}
	ep := route.validated
	if ep.Key != route.Key || ep.NetworkID != route.NetworkName || ep.VNI != route.VNI ||
		ep.MAC != route.MAC || ep.IP != route.IP || ep.RemoteVTEP != route.RemoteVTEP {
		return RemoteNetlinkNetwork{}, fmt.Errorf("backend DTO differs from validated endpoint")
	}
	for _, n := range s.config.Networks {
		if n.NetworkID == route.NetworkName && n.VNI == route.VNI &&
			n.VXLANDevice == route.VXLANDevice && n.BridgeDevice == route.NeighborDevice {
			return n, nil
		}
	}
	return RemoteNetlinkNetwork{}, fmt.Errorf("no explicit remote network/device mapping")
}

func (b *OVNNetlinkBackend) ownedApply(ctx context.Context, route ImportedType2Route) error {
	if b == nil || b.owned == nil {
		return fmt.Errorf("remote Netlink disabled: explicit owned backend required")
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	network, err := s.route(ctx, route)
	if err != nil {
		return err
	}
	receipt, exists := s.receipts[route.Key]
	if exists && (receipt.Controller != s.controller || receipt.Target != s.target ||
		receipt.Generation != route.generation || receipt.Incarnation != route.incarnation ||
		!reflect.DeepEqual(receipt.Endpoint, *route.validated)) {
		return fmt.Errorf("receipt incarnation/payload conflict")
	}
	if !exists {
		if route.incarnation <= s.latest[route.Key] {
			return fmt.Errorf("stale route incarnation")
		}
		s.latest[route.Key] = route.incarnation
		receipt = s.newReceipt(network, route)
		s.receipts[route.Key] = receipt
	}
	receipt.Status = "partially-applied"
	if err := s.applyObject(ctx, &receipt.FDB); err != nil {
		return err
	}
	if receipt.Neighbor != nil {
		if err := s.applyObject(ctx, receipt.Neighbor); err != nil {
			return err
		}
	}
	receipt.Status = "applied"
	return s.checkpoint()
}

func (s *ownedNetlinkState) newReceipt(network RemoteNetlinkNetwork, route ImportedType2Route) *RemoteNetlinkReceipt {
	fdb := RemoteKernelEntry{Family: unix.AF_BRIDGE, Ifindex: s.devices[network.VXLANDevice].Ifindex,
		MAC: route.MAC, IP: route.RemoteVTEP, State: netlink.NUD_NOARP | netlink.NUD_PERMANENT,
		Flags: netlink.NTF_SELF | netlink.NTF_EXT_LEARNED, Type: unix.RTN_UNICAST}
	r := &RemoteNetlinkReceipt{Controller: s.controller, Target: s.target, Network: network,
		Endpoint: *route.validated, Incarnation: route.incarnation, Generation: route.generation,
		Devices: []RemoteDeviceIdentity{s.devices[network.BridgeDevice], s.devices[network.VXLANDevice]},
		FDB:     RemoteObjectReceipt{Request: fdb, Status: "unattempted"}}
	if route.IP != "" {
		family := unix.AF_INET6
		if net.ParseIP(route.IP).To4() != nil {
			family = unix.AF_INET
		}
		r.Neighbor = &RemoteObjectReceipt{Request: RemoteKernelEntry{Family: family,
			Ifindex: s.devices[network.BridgeDevice].Ifindex, MAC: route.MAC, IP: route.IP,
			State: netlink.NUD_NOARP, Flags: netlink.NTF_EXT_LEARNED}, Status: "unattempted"}
	}
	return r
}

func (b *OVNNetlinkBackend) ownedWithdraw(ctx context.Context, route ImportedType2Route) error {
	if b == nil || b.owned == nil {
		return fmt.Errorf("remote Netlink disabled: explicit owned backend required")
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.route(ctx, route); err != nil {
		return err
	}
	receipt, ok := s.receipts[route.Key]
	if !ok || receipt.Controller != s.controller || receipt.Target != s.target || receipt.Generation != route.generation ||
		receipt.Incarnation != route.incarnation ||
		!reflect.DeepEqual(receipt.Endpoint, *route.validated) || s.latest[route.Key] != route.incarnation {
		return fmt.Errorf("withdrawal requires current controller/route receipt")
	}
	receipt.Status = "partially-withdrawn"
	receipt.Retiring = true
	if err := s.checkpoint(); err != nil {
		return err
	}
	if receipt.Neighbor != nil {
		if err := s.withdrawObject(ctx, receipt.Neighbor); err != nil {
			return err
		}
	}
	if err := s.withdrawObject(ctx, &receipt.FDB); err != nil {
		return err
	}
	receipt.Status = "withdrawn"
	s.withdrawals = append(s.withdrawals, *receipt)
	if len(s.withdrawals) > 64 {
		s.withdrawals = append([]RemoteNetlinkReceipt(nil), s.withdrawals[len(s.withdrawals)-64:]...)
	}
	delete(s.receipts, route.Key)
	return s.checkpoint()
}
