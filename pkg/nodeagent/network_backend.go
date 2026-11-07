package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	ifNameMaxLen = 15
)

var errAttachmentNotFound = errors.New("attachment not found")

type networkState struct {
	HostIfName      string
	ContainerIfName string
	MACAddress      string
	StaticIPCIDR    string
}

type networkBackend interface {
	Ensure(ctx context.Context, key string, rec *endpointRecord) (networkState, bool, error)
	Delete(ctx context.Context, key string, rec *endpointRecord) error
	Check(ctx context.Context, key string, rec *endpointRecord) (networkState, error)
}

type inMemoryNetworkBackend struct {
	mu      sync.RWMutex
	records map[string]networkState
}

func newInMemoryNetworkBackend() *inMemoryNetworkBackend {
	return &inMemoryNetworkBackend{
		records: make(map[string]networkState),
	}
}

func (b *inMemoryNetworkBackend) Ensure(_ context.Context, key string, rec *endpointRecord) (networkState, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	desired := networkState{
		HostIfName:      rec.endpointSpec.OVSInterfaceName,
		ContainerIfName: strings.TrimSpace(rec.runtime.GetIfName()),
		MACAddress:      strings.TrimSpace(rec.attachment.GetStaticMac()),
		StaticIPCIDR:    strings.TrimSpace(rec.attachment.GetStaticIpCidr()),
	}
	if existing, ok := b.records[key]; ok {
		if existing != desired {
			return networkState{}, false, fmt.Errorf("network attachment conflict for %q", key)
		}
		return existing, false, nil
	}
	b.records[key] = desired
	return desired, true, nil
}

func (b *inMemoryNetworkBackend) Delete(_ context.Context, key string, _ *endpointRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.records, key)
	return nil
}

func (b *inMemoryNetworkBackend) Check(_ context.Context, key string, _ *endpointRecord) (networkState, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	state, ok := b.records[key]
	if !ok {
		return networkState{}, errAttachmentNotFound
	}
	return state, nil
}

type linuxVethBackend struct {
	mu      sync.RWMutex
	records map[string]networkState
}

func newLinuxVethBackend() *linuxVethBackend {
	return &linuxVethBackend{
		records: make(map[string]networkState),
	}
}

func NewLinuxVethBackend() *linuxVethBackend {
	return newLinuxVethBackend()
}

func (b *linuxVethBackend) Ensure(_ context.Context, key string, rec *endpointRecord) (networkState, bool, error) {
	runtimeCtx := rec.runtime
	attach := rec.attachment
	netnsPath := strings.TrimSpace(runtimeCtx.GetNetnsPath())
	containerIfName := strings.TrimSpace(runtimeCtx.GetIfName())
	mac := strings.TrimSpace(attach.GetStaticMac())
	ipCIDR := strings.TrimSpace(attach.GetStaticIpCidr())
	if netnsPath == "" {
		return networkState{}, false, fmt.Errorf("runtime.netns_path is required for linux backend")
	}
	if containerIfName == "" {
		return networkState{}, false, fmt.Errorf("runtime.if_name is required for linux backend")
	}
	if mac == "" {
		return networkState{}, false, fmt.Errorf("attachment.static_mac is required for linux backend")
	}
	if ipCIDR == "" {
		return networkState{}, false, fmt.Errorf("attachment.static_ip_cidr is required for linux backend")
	}
	if _, _, err := net.ParseCIDR(ipCIDR); err != nil {
		return networkState{}, false, fmt.Errorf("attachment.static_ip_cidr invalid: %w", err)
	}
	if _, err := net.ParseMAC(mac); err != nil {
		return networkState{}, false, fmt.Errorf("attachment.static_mac invalid: %w", err)
	}

	b.mu.RLock()
	existing, exists := b.records[key]
	b.mu.RUnlock()
	if exists {
		desired := networkState{HostIfName: hostVethNameForKey(key), ContainerIfName: containerIfName, MACAddress: mac, StaticIPCIDR: ipCIDR}
		if existing != desired {
			return networkState{}, false, fmt.Errorf("network attachment conflict for %q", key)
		}
	}

	hostIfName := hostVethNameForKey(key)
	peerIfName := peerVethNameForKey(key)
	if hostIfName == peerIfName {
		peerIfName = peerIfName + "p"
		if len(peerIfName) > ifNameMaxLen {
			peerIfName = peerIfName[:ifNameMaxLen]
		}
	}
	owner := ownerAliasForKey(key)
	created := false

	// Cleanup only resources created by this invocation.
	cleanup := func() {
		if !created {
			return
		}
		if link, err := netlink.LinkByName(hostIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	}

	hostLink, err := netlink.LinkByName(hostIfName)
	if err == nil {
		if hostLink.Attrs() == nil || hostLink.Attrs().Alias != owner {
			return networkState{}, false, fmt.Errorf("host interface %q exists and is not owned by endpoint", hostIfName)
		}
		state, checkErr := b.Check(context.Background(), key, rec)
		if checkErr != nil {
			return networkState{}, false, fmt.Errorf("verify existing network attachment: %w", checkErr)
		}
		return state, false, nil
	}
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		return networkState{}, false, fmt.Errorf("find host interface %q: %w", hostIfName, err)
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:  hostIfName,
			Alias: owner,
		},
		PeerName: peerIfName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return networkState{}, false, fmt.Errorf("create veth pair %q/%q: %w", hostIfName, peerIfName, err)
	}
	created = true
	// Some runtimes do not persist LinkAttrs.Alias from LinkAdd, so set it explicitly.
	if hostLink, hostErr := netlink.LinkByName(hostIfName); hostErr == nil {
		if aliasErr := netlink.LinkSetAlias(hostLink, owner); aliasErr != nil {
			cleanup()
			return networkState{}, false, fmt.Errorf("set host interface %q alias %q: %w", hostIfName, owner, aliasErr)
		}
	} else {
		cleanup()
		return networkState{}, false, fmt.Errorf("find host interface %q after creation: %w", hostIfName, hostErr)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		cleanup()
		return networkState{}, false, fmt.Errorf("set host interface %q up: %w", hostIfName, err)
	}

	peerLink, err := netlink.LinkByName(peerIfName)
	if err != nil {
		cleanup()
		return networkState{}, false, fmt.Errorf("find peer interface %q: %w", peerIfName, err)
	}
	_ = netlink.LinkSetAlias(peerLink, owner)
	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		cleanup()
		return networkState{}, false, fmt.Errorf("open netns %q: %w", netnsPath, err)
	}
	defer targetNS.Close()
	if err := netlink.LinkSetNsFd(peerLink, int(targetNS)); err != nil {
		cleanup()
		return networkState{}, false, fmt.Errorf("move peer %q to netns %q: %w", peerIfName, netnsPath, err)
	}

	if err := withNetNSPath(netnsPath, func() error {
		link, linkErr := netlink.LinkByName(peerIfName)
		if linkErr != nil {
			return fmt.Errorf("find moved peer %q: %w", peerIfName, linkErr)
		}
		if peerIfName != containerIfName {
			if renameErr := netlink.LinkSetName(link, containerIfName); renameErr != nil {
				return fmt.Errorf("rename peer %q to %q: %w", peerIfName, containerIfName, renameErr)
			}
			link, linkErr = netlink.LinkByName(containerIfName)
			if linkErr != nil {
				return fmt.Errorf("find renamed interface %q: %w", containerIfName, linkErr)
			}
		}
		_ = netlink.LinkSetAlias(link, owner)
		hw, parseErr := net.ParseMAC(mac)
		if parseErr != nil {
			return parseErr
		}
		if setMacErr := netlink.LinkSetHardwareAddr(link, hw); setMacErr != nil {
			return fmt.Errorf("set mac on %q: %w", containerIfName, setMacErr)
		}
		addr, addrErr := netlink.ParseAddr(ipCIDR)
		if addrErr != nil {
			return fmt.Errorf("parse static ip %q: %w", ipCIDR, addrErr)
		}
		if addAddrErr := netlink.AddrAdd(link, addr); addAddrErr != nil && !strings.Contains(addAddrErr.Error(), "file exists") {
			return fmt.Errorf("set ip on %q: %w", containerIfName, addAddrErr)
		}
		if upErr := netlink.LinkSetUp(link); upErr != nil {
			return fmt.Errorf("set container interface %q up: %w", containerIfName, upErr)
		}
		return nil
	}); err != nil {
		cleanup()
		return networkState{}, false, err
	}

	state := networkState{
		HostIfName:      hostIfName,
		ContainerIfName: containerIfName,
		MACAddress:      mac,
		StaticIPCIDR:    ipCIDR,
	}
	b.mu.Lock()
	b.records[key] = state
	b.mu.Unlock()
	return state, true, nil
}

func (b *linuxVethBackend) Delete(_ context.Context, key string, _ *endpointRecord) error {
	b.mu.RLock()
	state, ok := b.records[key]
	b.mu.RUnlock()
	if !ok {
		inferredHost := hostVethNameForKey(key)
		link, err := netlink.LinkByName(inferredHost)
		if err != nil {
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				return nil
			}
			return fmt.Errorf("observe host interface: %w", err)
		}
		if link.Attrs() == nil || link.Attrs().Alias != ownerAliasForKey(key) {
			return fmt.Errorf("host interface %q is not owned by endpoint", inferredHost)
		}
		if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("delete host interface %q: %w", inferredHost, err)
		}
		return nil
	}
	link, err := netlink.LinkByName(state.HostIfName)
	if err != nil {
		var missing netlink.LinkNotFoundError
		if !errors.As(err, &missing) {
			return fmt.Errorf("observe host interface: %w", err)
		}
		// Missing interface is safe and idempotent.
		b.mu.Lock()
		delete(b.records, key)
		b.mu.Unlock()
		return nil
	}
	if link.Attrs() == nil || link.Attrs().Alias != ownerAliasForKey(key) {
		return fmt.Errorf("host interface %q is not owned by endpoint", state.HostIfName)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete host interface %q: %w", state.HostIfName, err)
	}
	b.mu.Lock()
	delete(b.records, key)
	b.mu.Unlock()
	return nil
}

func (b *linuxVethBackend) DeleteAcknowledged(ctx context.Context, key string, rec *endpointRecord, index int) error {
	if index <= 0 {
		return errors.New("missing acknowledged host veth index")
	}
	link, err := netlink.LinkByName(rec.endpointSpec.OVSInterfaceName)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	if link.Attrs() == nil || link.Attrs().Index != index {
		return errors.New("acknowledged host veth index changed")
	}
	if _, err := b.Check(ctx, key, rec); err != nil {
		return err
	}
	if err := netlink.LinkDel(link); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.records, key)
	b.mu.Unlock()
	return nil
}

func (b *linuxVethBackend) Check(_ context.Context, key string, rec *endpointRecord) (networkState, error) {
	b.mu.RLock()
	state, ok := b.records[key]
	b.mu.RUnlock()
	if !ok {
		if rec == nil || rec.runtime == nil || rec.attachment == nil {
			return networkState{}, errAttachmentNotFound
		}
		inferredIf := strings.TrimSpace(rec.runtime.GetIfName())
		if inferredIf == "" {
			return networkState{}, errAttachmentNotFound
		}
		state = networkState{
			HostIfName:      hostVethNameForKey(key),
			ContainerIfName: inferredIf,
			MACAddress:      strings.TrimSpace(rec.attachment.GetStaticMac()),
			StaticIPCIDR:    strings.TrimSpace(rec.attachment.GetStaticIpCidr()),
		}
	}
	host, err := netlink.LinkByName(state.HostIfName)
	if err != nil {
		return networkState{}, fmt.Errorf("host interface %q not found: %w", state.HostIfName, err)
	}
	if host.Attrs() == nil || host.Attrs().Alias != ownerAliasForKey(key) {
		return networkState{}, fmt.Errorf("host interface %q ownership mismatch", state.HostIfName)
	}
	if host.Type() != "veth" || host.Attrs().Flags&net.FlagUp == 0 {
		return networkState{}, fmt.Errorf("host interface %q is not an up veth", state.HostIfName)
	}
	netnsPath := strings.TrimSpace(rec.runtime.GetNetnsPath())
	if netnsPath == "" {
		return networkState{}, fmt.Errorf("runtime.netns_path is required for check")
	}
	if err := withNetNSPath(netnsPath, func() error {
		link, linkErr := netlink.LinkByName(state.ContainerIfName)
		if linkErr != nil {
			return fmt.Errorf("container interface %q not found: %w", state.ContainerIfName, linkErr)
		}
		if link.Attrs() == nil || link.Attrs().Alias != ownerAliasForKey(key) {
			return fmt.Errorf("container interface %q ownership mismatch", state.ContainerIfName)
		}
		if link.Type() != "veth" || link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("container interface %q is not an up veth", state.ContainerIfName)
		}
		if host.Attrs().ParentIndex != link.Attrs().Index || link.Attrs().ParentIndex != host.Attrs().Index {
			return fmt.Errorf("veth peer mismatch for %q/%q", state.HostIfName, state.ContainerIfName)
		}
		if strings.TrimSpace(state.MACAddress) != "" && !strings.EqualFold(link.Attrs().HardwareAddr.String(), state.MACAddress) {
			return fmt.Errorf("container mac mismatch on %q", state.ContainerIfName)
		}
		if strings.TrimSpace(state.StaticIPCIDR) != "" {
			addresses, addrErr := netlink.AddrList(link, netlink.FAMILY_ALL)
			if addrErr != nil {
				return fmt.Errorf("list addrs for %q: %w", state.ContainerIfName, addrErr)
			}
			match := false
			for _, addr := range addresses {
				if addr.IPNet != nil && addr.IPNet.String() == state.StaticIPCIDR {
					match = true
					break
				}
			}
			if !match {
				return fmt.Errorf("container ip %q missing on %q", state.StaticIPCIDR, state.ContainerIfName)
			}
		}
		return nil
	}); err != nil {
		return networkState{}, err
	}
	b.mu.Lock()
	b.records[key] = state
	b.mu.Unlock()
	return state, nil
}

func hostVethNameForKey(key string) string {
	return "evp" + shortHash(key, ifNameMaxLen-3)
}

func peerVethNameForKey(key string) string {
	return "evc" + shortHash(key, ifNameMaxLen-3)
}

func ownerAliasForKey(key string) string {
	return "evpn-node-agent|" + key
}

func shortHash(v string, chars int) string {
	h := fnv32a(v)
	full := fmt.Sprintf("%08x%08x", h, fnv32a(v+"-alt"))
	if chars <= 0 {
		return ""
	}
	if chars > len(full) {
		chars = len(full)
	}
	return full[:chars]
}

func fnv32a(v string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for i := 0; i < len(v); i++ {
		hash ^= uint32(v[i])
		hash *= prime32
	}
	return hash
}

func withNetNSPath(path string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close()

	targetNS, err := netns.GetFromPath(path)
	if err != nil {
		return fmt.Errorf("open target netns %q: %w", path, err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("switch to netns %q: %w", path, err)
	}
	defer func() {
		_ = netns.Set(origNS)
	}()

	return fn()
}
