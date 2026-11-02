package evpn

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type localKernelReader interface {
	LinkByName(string) (netlink.Link, error)
	NeighList(int, int) ([]netlink.Neigh, error)
	Close() error
}

// LocalOVNNetlinkObserver projects complete native dumps. Ports supplies only
// validated OVN port identity and allowed IPs, never sufficient evidence to
// advertise: every advertised MAC/IP must also exist in a native kernel dump.
type LocalOVNNetlinkObserver struct {
	mu       sync.Mutex
	ports    EndpointObserver
	config   ExporterConfig
	kernel   localKernelReader
	devices  map[string]RemoteDeviceIdentity
	updates  chan netlink.LinkUpdate
	done     chan struct{}
	lost     atomic.Bool
	blocked  error
	closed   bool
	previous []LocalEndpoint
	pending  bool
}

func NewLocalOVNNetlinkObserver(cfg ExporterConfig, ports EndpointObserver) (*LocalOVNNetlinkObserver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled || ports == nil {
		return nil, fmt.Errorf("enabled exporter and OVN local-port provenance source required")
	}
	ns, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer ns.Close()
	kernel, err := newOwnedKernelHandle(ns)
	if err != nil {
		return nil, err
	}
	o := &LocalOVNNetlinkObserver{ports: ports, config: cfg, kernel: kernel,
		devices: map[string]RemoteDeviceIdentity{}, updates: make(chan netlink.LinkUpdate, 256), done: make(chan struct{})}
	err = netlink.LinkSubscribeWithOptions(o.updates, o.done, netlink.LinkSubscribeOptions{
		Namespace: &ns, ReceiveBufferSize: 1 << 20, ErrorCallback: func(error) { o.lost.Store(true) },
	})
	if err == nil {
		err = o.checkDevices(true)
	}
	if err != nil {
		o.Close()
		return nil, err
	}
	return o, nil
}

func (o *LocalOVNNetlinkObserver) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	close(o.done)
	return o.kernel.Close()
}

func (o *LocalOVNNetlinkObserver) checkDevices(initial bool) error {
	if o.closed {
		return fmt.Errorf("local observer closed")
	}
	if o.blocked != nil {
		return o.blocked
	}
	if o.lost.Load() {
		o.blocked = fmt.Errorf("local device lifetime notifications lost; controlled restart required")
		return o.blocked
	}
	for {
		select {
		case update, ok := <-o.updates:
			if !ok || update.Link == nil {
				o.blocked = fmt.Errorf("local device lifetime stream incomplete")
				return o.blocked
			}
			for name, prior := range o.devices {
				if update.Attrs().Name == name || update.Attrs().Index == prior.Ifindex {
					if linkLifetimeChanged(update, prior) {
						o.blocked = fmt.Errorf("local device %s changed lifetime/topology; no withdrawal", name)
						return o.blocked
					}
				}
			}
		default:
			goto drained
		}
	}
drained:
	for _, cfg := range o.config.Networks {
		if !cfg.Enabled || cfg.ExportSource() != "ovn-netlink" {
			continue
		}
		for _, name := range []string{cfg.BridgeDevice, cfg.VXLANDevice, cfg.AdvertiseDevice} {
			link, err := o.kernel.LinkByName(name)
			if err != nil {
				return fmt.Errorf("local device %s unavailable: %w", name, err)
			}
			id := remoteDeviceIdentity(link)
			if initial {
				o.devices[name] = id
			} else if id != o.devices[name] {
				o.blocked = fmt.Errorf("local device %s identity changed; no withdrawal", name)
				return o.blocked
			}
			if link.Attrs().Flags&unix.IFF_UP == 0 {
				return fmt.Errorf("local device %s is down; observation non-authoritative", name)
			}
		}
		br, vx, adv := o.devices[cfg.BridgeDevice], o.devices[cfg.VXLANDevice], o.devices[cfg.AdvertiseDevice]
		if br.Kind != "bridge" || vx.Kind != "vxlan" || adv.Kind != "dummy" ||
			vx.VNI != int(cfg.VNI) || vx.Local != cfg.LocalVTEP ||
			vx.MasterIndex != br.Ifindex || adv.MasterIndex != br.Ifindex {
			return fmt.Errorf("network %s native device/VNI/local VTEP mismatch", cfg.NetworkID)
		}
	}
	return nil
}

func (o *LocalOVNNetlinkObserver) DesiredLocalEndpoints(ctx context.Context) (result []LocalEndpoint, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	keepCandidate := false
	defer func() {
		if err != nil && !keepCandidate {
			o.pending = false
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = o.checkDevices(false); err != nil {
		return nil, err
	}
	before, err := o.ports.DesiredLocalEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	for _, cfg := range o.config.Networks {
		if !cfg.Enabled || cfg.ExportSource() != "ovn-netlink" {
			continue
		}
		ports := map[string]LocalEndpoint{}
		allowedIPs := map[string]map[string]bool{}
		for _, ep := range before {
			if ep.NetworkID != cfg.NetworkID {
				continue
			}
			if _, exists := ports[ep.MACAddress]; exists {
				return nil, fmt.Errorf("ambiguous local MAC ownership in network %s", cfg.NetworkID)
			}
			ports[ep.MACAddress] = ep
			allowedIPs[ep.MACAddress] = map[string]bool{}
			for _, ip := range ep.IPAddresses {
				allowedIPs[ep.MACAddress][ip] = true
			}
		}
		adv, br := o.devices[cfg.AdvertiseDevice], o.devices[cfg.BridgeDevice]
		fdb, err := o.kernel.NeighList(adv.Ifindex, unix.AF_BRIDGE)
		if err != nil {
			return nil, fmt.Errorf("native FDB dump incomplete: %w", err)
		}
		local := map[string]LocalEndpoint{}
		for _, entry := range fdb {
			// The bridge also exposes VLAN duplicates and permanent own/multicast
			// entries. Only OVN's untagged, NOARP, non-external master entry is local.
			if entry.Vlan != 0 || entry.State != netlink.NUD_NOARP ||
				entry.Flags&netlink.NTF_EXT_LEARNED != 0 || entry.FlagsExt != 0 ||
				entry.Flags&^netlink.NTF_MASTER != 0 || len(entry.IP) != 0 {
				continue
			}
			mac := entry.HardwareAddr.String()
			ep, ok := ports[mac]
			if !ok {
				continue
			}
			if ep.VNI != cfg.VNI || ep.LocalVTEP != cfg.LocalVTEP {
				return nil, fmt.Errorf("invalid native local-port provenance")
			}
			ep.MACOnly, ep.IPAddresses = true, nil
			if err := ep.Validate(); err != nil {
				return nil, err
			}
			local[mac] = ep
		}
		for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
			entries, err := o.kernel.NeighList(br.Ifindex, family)
			if err != nil {
				return nil, fmt.Errorf("native neighbor dump incomplete: %w", err)
			}
			for _, entry := range entries {
				if entry.State != netlink.NUD_NOARP || entry.Flags != 0 || entry.FlagsExt != 0 {
					continue
				}
				ep, ok := local[entry.HardwareAddr.String()]
				if !ok {
					continue
				}
				if entry.IP == nil || entry.IP.IsUnspecified() || entry.IP.IsMulticast() ||
					(family == unix.AF_INET) != (entry.IP.To4() != nil) {
					return nil, fmt.Errorf("invalid native local IP neighbor")
				}
				if !allowedIPs[entry.HardwareAddr.String()][entry.IP.String()] {
					return nil, fmt.Errorf("native neighbor does not match local OVN static IP provenance")
				}
				ep.MACOnly, ep.IPAddresses = false, []string{entry.IP.String()}
				result = append(result, ep)
			}
		}
		for _, ep := range local {
			result = append(result, ep)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RouteKey() < result[j].RouteKey() })
	after, err := o.ports.DesiredLocalEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("OVN local-port provenance changed during native snapshot")
	}
	if err := o.checkDevices(false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Every tick takes complete dumps, so neighbor notification loss needs no
	// separate event replay. Two successful equal ticks confirm removal.
	confirmed := o.pending && reflect.DeepEqual(o.previous, result)
	o.previous = make([]LocalEndpoint, len(result))
	for i := range result {
		o.previous[i] = cloneLocalEndpoint(result[i])
	}
	if len(result) == 0 {
		o.previous = nil
	}
	o.pending = true
	if !confirmed {
		// Keep the candidate while returning an explicitly non-authoritative read.
		keepCandidate = true
		return nil, fmt.Errorf("native snapshot awaiting consecutive complete confirmation")
	}
	return result, nil
}
