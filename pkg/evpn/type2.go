package evpn

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// RouteAction describes the lifecycle transition for a locally-owned EVPN Type-2 route.
type RouteAction string

const (
	RouteAdvertise RouteAction = "advertise"
	RouteWithdraw  RouteAction = "withdraw"
)

// LocalEndpoint is the provider-neutral representation of a local endpoint that may be
// advertised as an EVPN Type-2 MAC/IP route.
type LocalEndpoint struct {
	NetworkID          string
	EndpointID         string
	Incarnation        string
	MACAddress         string
	IPAddresses        []string
	VNI                uint32
	RouteDistinguisher string
	RouteTargets       []string
	LocalVTEP          string
	NodeName           string
	Chassis            string
	// Advertise is set only after platform-specific identity, readiness, and
	// authorization checks succeed. False candidates are never advertised.
	Advertise bool
	MACOnly   bool
}

// Type2NLRI is the package-owned value representation of an EVPN Type-2 NLRI.
type Type2NLRI struct {
	RD          string
	ESI         string
	EthernetTag uint32
	MAC         string
	IP          string
	Labels      []uint32
}

// Key is the Type-2 route prefix identity; labels are replacement attributes, not key fields.
func (n Type2NLRI) Key() string {
	return fmt.Sprintf("%q|%q|%d|%q|%q", n.RD, n.ESI, n.EthernetTag, n.MAC, n.IP)
}

// Equal compares the route prefix identity and intentionally ignores labels.
func (n Type2NLRI) Equal(other Type2NLRI) bool {
	return n.RD == other.RD && n.ESI == other.ESI && n.EthernetTag == other.EthernetTag &&
		n.MAC == other.MAC && n.IP == other.IP
}

func sameType2NLRIValue(lhs, rhs Type2NLRI) bool {
	if !lhs.Equal(rhs) || len(lhs.Labels) != len(rhs.Labels) {
		return false
	}
	for i := range lhs.Labels {
		if lhs.Labels[i] != rhs.Labels[i] {
			return false
		}
	}
	return true
}

func (n Type2NLRI) Validate() error {
	if _, err := bgp.ParseRouteDistinguisher(n.RD); err != nil {
		return fmt.Errorf("invalid Type-2 RD: %w", err)
	}
	esi, err := hex.DecodeString(n.ESI)
	if err != nil || len(esi) != 10 {
		return fmt.Errorf("invalid Type-2 ESI")
	}
	mac, err := net.ParseMAC(n.MAC)
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("invalid Type-2 MAC")
	}
	if n.IP != "" {
		if _, err := netip.ParseAddr(n.IP); err != nil {
			return fmt.Errorf("invalid Type-2 IP: %w", err)
		}
	}
	if len(n.Labels) == 0 {
		return fmt.Errorf("Type-2 labels required")
	}
	return nil
}

type Type2ExtendedCommunityKind string

const (
	Type2RouteTargetCommunity   Type2ExtendedCommunityKind = "route-target"
	Type2EncapsulationCommunity Type2ExtendedCommunityKind = "encapsulation"
	Type2MACMobilityCommunity   Type2ExtendedCommunityKind = "mac-mobility"
	Type2OpaqueCommunity        Type2ExtendedCommunityKind = "opaque"
	Type2OriginIGP                                         = "igp"
)

// Type2ExtendedCommunity preserves the semantics relevant to Type-2 routes.
// Opaque values are retained by the GoBGP observer as serialized bytes in hex.
type Type2ExtendedCommunity struct {
	Kind     Type2ExtendedCommunityKind
	Value    string
	Sequence uint32
	Sticky   bool
}

type Type2PathAttributes struct {
	Origin              string
	NextHop             string
	ExtendedCommunities []Type2ExtendedCommunity
}

// Type2Route is a package-owned route value consumed by reconciliation.
type Type2Route struct {
	Endpoint       LocalEndpoint
	Action         RouteAction
	NLRI           Type2NLRI
	PathAttributes Type2PathAttributes
	operation      *routeOperation
}

// RouteReconciler plans deltas against confirmed applied state. Call Apply to
// execute and acknowledge each delta; planning alone never changes applied state.
type RouteReconciler struct {
	mu           sync.Mutex
	applied      map[string]OwnedType2Route
	foreign      map[string]bool
	pending      map[*routeOperation]bool
	synchronized bool
}

func NewRouteReconciler() *RouteReconciler {
	return &RouteReconciler{applied: make(map[string]OwnedType2Route)}
}

func (ep LocalEndpoint) OwnershipKey() string {
	return strings.TrimSpace(ep.EndpointID) + "/" + strings.TrimSpace(ep.Incarnation)
}

func (ep LocalEndpoint) RouteKey() string {
	return strings.Join([]string{
		strings.TrimSpace(ep.NetworkID),
		strings.TrimSpace(ep.EndpointID),
		strings.TrimSpace(ep.Incarnation),
		strings.TrimSpace(ep.RouteDistinguisher),
		strings.TrimSpace(ep.MACAddress),
		primaryIP(ep.IPAddresses),
		fmt.Sprintf("%d", ep.VNI),
		strings.TrimSpace(ep.LocalVTEP),
	}, "/")
}

func (ep LocalEndpoint) Equivalent(other LocalEndpoint) bool {
	return ep.NetworkID == other.NetworkID &&
		ep.EndpointID == other.EndpointID &&
		ep.Incarnation == other.Incarnation &&
		ep.RouteDistinguisher == other.RouteDistinguisher &&
		ep.MACAddress == other.MACAddress &&
		ep.VNI == other.VNI &&
		ep.MACOnly == other.MACOnly &&
		primaryIP(ep.IPAddresses) == primaryIP(other.IPAddresses) &&
		ep.LocalVTEP == other.LocalVTEP &&
		equalStringSet(ep.RouteTargets, other.RouteTargets)
}

func (ep LocalEndpoint) Validate() error {
	if strings.TrimSpace(ep.NetworkID) == "" {
		return fmt.Errorf("network id is required")
	}
	if strings.TrimSpace(ep.EndpointID) == "" {
		return fmt.Errorf("endpoint id is required")
	}
	if strings.TrimSpace(ep.Incarnation) == "" {
		return fmt.Errorf("incarnation is required")
	}
	if strings.TrimSpace(ep.MACAddress) == "" {
		return fmt.Errorf("mac address is required")
	}
	if _, err := netip.ParseAddr(ep.LocalVTEP); err != nil {
		return fmt.Errorf("invalid local vtep %q: %w", ep.LocalVTEP, err)
	}
	if ep.VNI == 0 {
		return fmt.Errorf("vni must be non-zero")
	}
	if strings.TrimSpace(ep.RouteDistinguisher) == "" {
		return fmt.Errorf("route distinguisher is required")
	}
	if _, err := bgp.ParseRouteDistinguisher(ep.RouteDistinguisher); err != nil {
		return fmt.Errorf("invalid route distinguisher %q: %w", ep.RouteDistinguisher, err)
	}
	if len(ep.RouteTargets) == 0 {
		return fmt.Errorf("at least one route target is required")
	}
	for _, rt := range ep.RouteTargets {
		if _, err := bgp.ParseRouteTarget(rt); err != nil {
			return fmt.Errorf("invalid route target %q: %w", rt, err)
		}
	}
	if _, err := bgp.ParseRouteTarget(ep.RouteTargets[0]); err != nil {
		return err
	}
	if _, err := net.ParseMAC(ep.MACAddress); err != nil {
		return fmt.Errorf("invalid mac address %q: %w", ep.MACAddress, err)
	}
	if ep.MACOnly {
		if len(ep.IPAddresses) != 0 {
			return fmt.Errorf("MAC-only advertisement cannot contain IP addresses")
		}
	} else if primaryIP(ep.IPAddresses) == "" {
		return fmt.Errorf("at least one ip address is required for EVPN Type-2 advertisement")
	}
	return nil
}

func primaryIP(addresses []string) string {
	for _, addr := range addresses {
		trimmed := strings.TrimSpace(addr)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/") {
			ip, _, err := net.ParseCIDR(trimmed)
			if err == nil {
				return ip.String()
			}
		}
		if parsed := net.ParseIP(trimmed); parsed != nil {
			return parsed.String()
		}
	}
	return ""
}

func parseSelectedIP(addresses []string) (netip.Addr, error) {
	ipStr := primaryIP(addresses)
	if ipStr == "" {
		return netip.Addr{}, fmt.Errorf("no host ip available")
	}
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr, nil
}

func BuildType2Route(ep LocalEndpoint) (Type2Route, error) {
	if err := ep.Validate(); err != nil {
		return Type2Route{}, err
	}

	var ipAddr netip.Addr
	if !ep.MACOnly {
		var err error
		ipAddr, err = parseSelectedIP(ep.IPAddresses)
		if err != nil {
			return Type2Route{}, err
		}
	}
	vtep, err := netip.ParseAddr(ep.LocalVTEP)
	if err != nil {
		return Type2Route{}, err
	}
	rd, err := bgp.ParseRouteDistinguisher(ep.RouteDistinguisher)
	if err != nil {
		return Type2Route{}, err
	}
	routeTargets := make([]Type2ExtendedCommunity, 0, len(ep.RouteTargets)+1)
	for _, rt := range ep.RouteTargets {
		parsed, err := bgp.ParseRouteTarget(rt)
		if err != nil {
			return Type2Route{}, err
		}
		routeTargets = append(routeTargets, Type2ExtendedCommunity{Kind: Type2RouteTargetCommunity, Value: parsed.String()})
	}
	routeTargets = append(routeTargets, Type2ExtendedCommunity{Kind: Type2EncapsulationCommunity, Value: "vxlan"})

	mac, err := net.ParseMAC(ep.MACAddress)
	if err != nil {
		return Type2Route{}, err
	}
	nlri := Type2NLRI{RD: rd.String(), ESI: strings.Repeat("0", 20), MAC: mac.String(), Labels: []uint32{ep.VNI}}
	if !ep.MACOnly {
		nlri.IP = ipAddr.String()
	}

	return Type2Route{
		Endpoint:       ep,
		Action:         RouteAdvertise,
		NLRI:           nlri,
		PathAttributes: Type2PathAttributes{Origin: Type2OriginIGP, NextHop: vtep.String(), ExtendedCommunities: routeTargets},
	}, nil
}

func (r *RouteReconciler) Reconcile(desired []LocalEndpoint) ([]Type2Route, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = make(map[*routeOperation]bool)
	desiredMap := make(map[string]LocalEndpoint, len(desired))
	for _, endpoint := range desired {
		if !endpoint.Advertise {
			continue
		}
		route, err := BuildType2Route(endpoint)
		if err != nil {
			return nil, err
		}
		key := route.NLRI.Key()
		if other, ok := desiredMap[key]; ok && !other.Equivalent(endpoint) {
			return nil, fmt.Errorf("%w: conflicting desired owners or attributes for %s", ErrRouteOwnership, key)
		}
		if r.foreign[key] {
			return nil, fmt.Errorf("%w: unclaimed local path for %s", ErrRouteOwnership, key)
		}
		desiredMap[key] = cloneLocalEndpoint(endpoint)
	}

	changes := make([]Type2Route, 0, len(desiredMap)+len(r.applied))
	plan := func(action RouteAction, ep LocalEndpoint, key, uuid string) error {
		route, err := BuildType2Route(cloneLocalEndpoint(ep))
		if err != nil {
			return err
		}
		op := &routeOperation{action: action, endpoint: cloneLocalEndpoint(ep), wireKey: key, pathUUID: uuid}
		route.Action, route.operation = action, op
		r.pending[op] = true
		changes = append(changes, route)
		return nil
	}
	keys := make([]string, 0, len(r.applied))
	for key := range r.applied {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		existing := r.applied[key]
		if want, ok := desiredMap[key]; ok && existing.Endpoint.Equivalent(want) {
			continue
		}
		if err := plan(RouteWithdraw, existing.Endpoint, key, existing.PathUUID); err != nil {
			return nil, err
		}
	}
	keys = keys[:0]
	for key := range desiredMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		endpoint := desiredMap[key]
		if existing, ok := r.applied[key]; ok && existing.Endpoint.Equivalent(endpoint) {
			continue
		}
		if err := plan(RouteAdvertise, endpoint, key, ""); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func equalStringSet(lhs, rhs []string) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	left := append([]string(nil), lhs...)
	right := append([]string(nil), rhs...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
