package evpn

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type EVPNImportNetwork struct {
	Name           string
	VNI            uint32
	RouteTarget    string
	VXLANDevice    string
	BridgeDevice   string
	NeighborDevice string
}

// Type2RouteObservation contains the provider-neutral fields used by import
// policy and reconciliation. Provider-specific attributes must be normalized
// by the snapshot source before they reach this contract.
type Type2RouteObservation struct {
	MAC          string
	IP           string
	RD           string
	Labels       []uint32
	NextHop      string
	RouteTargets []string
	targetMatch  func(string) (bool, error)
}

// Type2RouteSnapshot is usable only when Complete is true.
type Type2RouteSnapshot struct {
	Routes   []Type2RouteObservation
	Complete bool
}

// Type2RouteSnapshotSource observes a complete, authoritative Type-2 route
// snapshot. Errors and incomplete snapshots must not authorize withdrawal.
type Type2RouteSnapshotSource interface {
	SnapshotType2(context.Context) (Type2RouteSnapshot, error)
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

type Type2RouteValidator struct {
	LocalVTEP string
	Networks  []EVPNImportNetwork
}

func (v *Type2RouteValidator) ValidateObservation(route Type2RouteObservation) (*ImportedType2Route, bool, error) {
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
		matchRT := false
		if route.targetMatch != nil {
			var err error
			matchRT, err = route.targetMatch(network.RouteTarget)
			if err != nil {
				return nil, false, err
			}
		} else {
			for _, target := range route.RouteTargets {
				if target == network.RouteTarget {
					matchRT = true
					break
				}
			}
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

// DataplaneBackend is the per-route mutation contract used by the simple
// Type-2 snapshot importer. Snapshot completeness is checked before invoking
// it; backend-specific ownership and resource identity remain the backend's
// responsibility. The verified Netlink path additionally requires validated
// route evidence, generation, and incarnation before mutation.
type DataplaneBackend interface {
	Apply(ctx context.Context, route ImportedType2Route) error
	Withdraw(ctx context.Context, route ImportedType2Route) error
}

type LinuxVXLANBackend struct{}

var _ DataplaneBackend = (*LinuxVXLANBackend)(nil)

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

var _ DataplaneBackend = (*OVNNetlinkBackend)(nil)

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
	source       Type2RouteSnapshotSource
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

// NewRemoteType2Importer adapts a legacy provider-specific route source.
//
// Deprecated: use NewRemoteType2ImporterFromSnapshot.
func NewRemoteType2Importer(source RouteSnapshotSource, validator *Type2RouteValidator, backend DataplaneBackend) *RemoteType2Importer {
	return NewRemoteType2ImporterFromSnapshot(legacyType2RouteSnapshotSource{source: source}, validator, backend)
}

func NewRemoteType2ImporterFromSnapshot(source Type2RouteSnapshotSource, validator *Type2RouteValidator, backend DataplaneBackend) *RemoteType2Importer {
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
	if i.source == nil || i.validator == nil || i.backend == nil {
		return fmt.Errorf("Type-2 snapshot source, validator and dataplane backend are required")
	}
	snapshot, err := i.source.SnapshotType2(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Complete {
		return fmt.Errorf("incomplete Type-2 route snapshot")
	}
	desired := make(map[string]ImportedType2Route, len(snapshot.Routes))
	for _, route := range snapshot.Routes {
		parsed, ok, err := i.validator.ValidateObservation(route)
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

	keys := make([]string, 0, len(i.active))
	for key := range i.active {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		current := i.active[key]
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

	keys = keys[:0]
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := desired[key]
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
