package evpn

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

type EVPNImportNetwork struct {
	Name           string
	VNI            uint32
	RouteTarget    string
	VXLANDevice    string
	BridgeDevice   string
	NeighborDevice string
}

type ReceivedType2Route struct {
	MAC     string
	IP      string
	RD      string
	Labels  []uint32
	NextHop string
	Attrs   []bgp.PathAttributeInterface
}

type ImportedType2Route struct {
	Key            string
	NetworkName    string
	VNI            uint32
	MAC            string
	IP             string
	RemoteVTEP     string
	VXLANDevice    string
	BridgeDevice   string
	NeighborDevice string
	incarnation    uint64
	generation     uint64
	validated      *DesiredRemoteEndpoint
	retiring       bool
}

type RouteSnapshotSource interface {
	Snapshot(ctx context.Context) ([]ReceivedType2Route, error)
}

type GoBGPServerSnapshotSource struct {
	Server *server.BgpServer
}

func (s *GoBGPServerSnapshotSource) Snapshot(_ context.Context) ([]ReceivedType2Route, error) {
	if s == nil || s.Server == nil {
		return nil, fmt.Errorf("gobgp snapshot source is not configured")
	}

	out := make([]ReceivedType2Route, 0, 8)
	err := s.Server.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    bgp.RF_EVPN,
	}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		if prefix == nil {
			return
		}
		evpnNLRI, ok := prefix.(*bgp.EVPNNLRI)
		if !ok || evpnNLRI.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
			return
		}
		macRoute, ok := evpnNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if !ok {
			return
		}
		ip := strings.TrimSpace(macRoute.IPAddress.String())
		if ip == "<nil>" {
			ip = ""
		}

		for _, p := range paths {
			if p.Withdrawal || p.Stale {
				continue
			}
			nextHop := pathNextHop(p.Attrs)
			remoteVTEP := strings.TrimSpace(p.PeerAddress.String())
			if remoteVTEP == "" || remoteVTEP == "<nil>" || remoteVTEP == "invalid IP" || net.ParseIP(remoteVTEP) == nil {
				remoteVTEP = strings.TrimSpace(nextHop)
			}
			out = append(out, ReceivedType2Route{
				MAC:     strings.ToLower(strings.TrimSpace(macRoute.MacAddress.String())),
				IP:      ip,
				RD:      strings.TrimSpace(macRoute.RD.String()),
				Labels:  append([]uint32(nil), macRoute.Labels...),
				NextHop: remoteVTEP,
				Attrs:   p.Attrs,
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func pathNextHop(attrs []bgp.PathAttributeInterface) string {
	for _, attr := range attrs {
		switch a := attr.(type) {
		case *bgp.PathAttributeNextHop:
			return a.Value.String()
		case *bgp.PathAttributeMpReachNLRI:
			return a.Nexthop.String()
		}
	}
	return ""
}

func pathHasRouteTarget(attrs []bgp.PathAttributeInterface, wantRT string) (bool, error) {
	want, err := bgp.ParseRouteTarget(wantRT)
	if err != nil {
		return false, err
	}
	wantBytes, err := want.Serialize()
	if err != nil {
		return false, err
	}
	for _, attr := range attrs {
		ext, ok := attr.(*bgp.PathAttributeExtendedCommunities)
		if !ok {
			continue
		}
		for _, community := range ext.Value {
			gotBytes, err := community.Serialize()
			if err != nil {
				return false, err
			}
			if bytes.Equal(gotBytes, wantBytes) {
				return true, nil
			}
		}
	}
	return false, nil
}

type Type2RouteValidator struct {
	LocalVTEP string
	Networks  []EVPNImportNetwork
}

func (v *Type2RouteValidator) Validate(route ReceivedType2Route) (*ImportedType2Route, bool, error) {
	if strings.TrimSpace(route.MAC) == "" {
		return nil, false, nil
	}
	if _, err := net.ParseMAC(route.MAC); err != nil {
		return nil, false, nil
	}
	if len(route.Labels) == 0 || route.Labels[0] == 0 {
		return nil, false, nil
	}
	if route.IP == "" {
		return nil, false, nil
	}
	parsedIP := net.ParseIP(route.IP)
	if parsedIP == nil {
		return nil, false, nil
	}
	if parsedIP.To4() == nil {
		return nil, false, nil
	}
	if net.ParseIP(route.NextHop) == nil {
		return nil, false, nil
	}
	if strings.TrimSpace(v.LocalVTEP) != "" && strings.TrimSpace(route.NextHop) == strings.TrimSpace(v.LocalVTEP) {
		return nil, false, nil
	}

	for _, network := range v.Networks {
		if route.Labels[0] != network.VNI {
			continue
		}
		matchRT, err := pathHasRouteTarget(route.Attrs, network.RouteTarget)
		if err != nil {
			return nil, false, err
		}
		if !matchRT {
			continue
		}
		key := strings.Join([]string{
			network.Name,
			fmt.Sprintf("%d", network.VNI),
			strings.ToLower(route.MAC),
			route.IP,
		}, "/")
		return &ImportedType2Route{
			Key:            key,
			NetworkName:    network.Name,
			VNI:            network.VNI,
			MAC:            strings.ToLower(route.MAC),
			IP:             strings.TrimSpace(route.IP),
			RemoteVTEP:     strings.TrimSpace(route.NextHop),
			VXLANDevice:    network.VXLANDevice,
			BridgeDevice:   network.BridgeDevice,
			NeighborDevice: network.NeighborDevice,
		}, true, nil
	}
	return nil, false, nil
}

type DataplaneBackend interface {
	Apply(ctx context.Context, route ImportedType2Route) error
	Withdraw(ctx context.Context, route ImportedType2Route) error
}

type LinuxVXLANBackend struct{}

func (b *LinuxVXLANBackend) Apply(_ context.Context, route ImportedType2Route) error {
	if err := setRemoteMacFdb(route.VXLANDevice, route.MAC, route.RemoteVTEP, false); err != nil {
		return err
	}
	if err := setFloodFdb(route.VXLANDevice, route.RemoteVTEP, false); err != nil {
		return err
	}
	return nil
}

func (b *LinuxVXLANBackend) Withdraw(_ context.Context, route ImportedType2Route) error {
	if err := setRemoteMacFdb(route.VXLANDevice, route.MAC, route.RemoteVTEP, true); err != nil {
		return err
	}
	if err := setFloodFdb(route.VXLANDevice, route.RemoteVTEP, true); err != nil {
		return err
	}
	return nil
}

// OVNNetlinkBackend programs OVN-observed Linux netlink state (FDB + neighbor)
// without mutating OVS OpenFlow or SBDB directly.
type OVNNetlinkBackend struct {
	Runner      CommandRunner
	KubeContext string
	Namespace   string
	Pod         string
	Container   string
	UseSudo     bool
	owned       *ownedNetlinkState
}

func (b *OVNNetlinkBackend) Apply(ctx context.Context, route ImportedType2Route) error {
	return b.ownedApply(ctx, route)
}

func (b *OVNNetlinkBackend) Withdraw(ctx context.Context, route ImportedType2Route) error {
	return b.ownedWithdraw(ctx, route)
}

func setRemoteMacFdb(vxlanName, macAddr, remoteVTEP string, withdraw bool) error {
	args := []string{"fdb"}
	if withdraw {
		args = append(args, "del")
	} else {
		args = append(args, "replace")
	}
	args = append(args, macAddr, "dev", vxlanName, "dst", remoteVTEP, "self", "permanent")
	out, err := exec.Command("sudo", append([]string{"-n", "bridge"}, args...)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if withdraw && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("bridge %s failed: %w (%s)", strings.Join(args, " "), err, msg)
	}
	return nil
}

func setFloodFdb(vxlanName, remoteVTEP string, withdraw bool) error {
	if withdraw {
		return removeFloodEntry(vxlanName, remoteVTEP)
	}
	args := []string{"fdb"}
	args = append(args, "append")
	args = append(args, "00:00:00:00:00:00", "dev", vxlanName, "dst", remoteVTEP, "self", "permanent")
	out, err := exec.Command("sudo", append([]string{"-n", "bridge"}, args...)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "File exists") {
			return nil
		}
		return fmt.Errorf("bridge %s failed: %w (%s)", strings.Join(args, " "), err, msg)
	}
	return nil
}

func removeFloodEntry(vxlanName, remoteVTEP string) error {
	out, err := exec.Command("sudo", "-n", "bridge", "fdb", "show", "dev", vxlanName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("bridge fdb show dev %s failed: %w (%s)", vxlanName, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "00:00:00:00:00:00") || !strings.Contains(trimmed, "dst "+remoteVTEP) {
			continue
		}
		fields := strings.Fields(trimmed)
		args := []string{"fdb", "del", "00:00:00:00:00:00", "dev", vxlanName}
		for i := 1; i < len(fields); i++ {
			switch fields[i] {
			case "dst", "vlan", "via":
				if i+1 < len(fields) {
					args = append(args, fields[i], fields[i+1])
					i++
				}
			case "self", "permanent", "master", "router", "extern_learn", "static":
				args = append(args, fields[i])
			}
		}
		delOut, delErr := exec.Command("sudo", append([]string{"-n", "bridge"}, args...)...).CombinedOutput()
		if delErr != nil {
			msg := strings.TrimSpace(string(delOut))
			if strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find") {
				continue
			}
			return fmt.Errorf("bridge %s failed: %w (%s)", strings.Join(args, " "), delErr, msg)
		}
	}
	return nil
}

func setIPNeighbor(device, ipAddr, mac string, withdraw bool) error {
	if net.ParseIP(ipAddr) == nil {
		return fmt.Errorf("invalid neighbor ip %q", ipAddr)
	}
	if _, err := net.ParseMAC(mac); err != nil {
		return fmt.Errorf("invalid neighbor mac %q: %w", mac, err)
	}
	action := "replace"
	if withdraw {
		action = "del"
	}
	out, err := exec.Command("sudo", "-n", "ip", "neigh", action, ipAddr, "lladdr", mac, "dev", device, "nud", "permanent").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if withdraw && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("ip neigh %s %s on %s failed: %w (%s)", action, ipAddr, device, err, msg)
	}
	return nil
}

type RemoteType2Importer struct {
	source       RouteSnapshotSource
	validator    *Type2RouteValidator
	backend      DataplaneBackend
	pollInterval time.Duration
	maxRetries   int

	mu                  sync.Mutex
	active              map[string]ImportedType2Route
	verified            *ReadOnlyType2Importer
	ownedBackend        *OVNNetlinkBackend
	last                RemoteImportSnapshot
	epoch               uint64
	confirmedGeneration uint64
	confirmedDigest     string
}

func NewRemoteType2Importer(source RouteSnapshotSource, validator *Type2RouteValidator, backend DataplaneBackend) *RemoteType2Importer {
	return &RemoteType2Importer{
		source:       source,
		validator:    validator,
		backend:      backend,
		pollInterval: 500 * time.Millisecond,
		maxRetries:   3,
		active:       make(map[string]ImportedType2Route),
	}
}

func (i *RemoteType2Importer) ReconcileOnce(ctx context.Context) error {
	if i.verified != nil {
		return i.reconcileValidated(ctx)
	}
	received, err := i.source.Snapshot(ctx)
	if err != nil {
		return err
	}
	desired := make(map[string]ImportedType2Route, len(received))
	for _, route := range received {
		parsed, ok, err := i.validator.Validate(route)
		if err != nil {
			return err
		}
		if !ok || parsed == nil {
			continue
		}
		desired[parsed.Key] = *parsed
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	for key, current := range i.active {
		want, ok := desired[key]
		if !ok {
			if err := i.backend.Withdraw(ctx, current); err != nil {
				return err
			}
			delete(i.active, key)
			continue
		}
		if current.RemoteVTEP != want.RemoteVTEP || current.MAC != want.MAC || current.IP != want.IP {
			if err := i.backend.Withdraw(ctx, current); err != nil {
				return err
			}
			if err := i.backend.Apply(ctx, want); err != nil {
				return err
			}
			i.active[key] = want
		}
	}

	for key, want := range desired {
		if _, ok := i.active[key]; ok {
			continue
		}
		if err := i.backend.Apply(ctx, want); err != nil {
			return err
		}
		i.active[key] = want
	}
	return nil
}

func (i *RemoteType2Importer) Run(ctx context.Context) error {
	ticker := time.NewTicker(i.pollInterval)
	defer ticker.Stop()

	failures := 0
	for {
		if err := i.ReconcileOnce(ctx); err != nil {
			failures++
			if failures >= i.maxRetries {
				return err
			}
			backoff := time.Duration(failures) * 300 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
