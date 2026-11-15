package domain

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Endpoint contains only the provider-neutral fields used by Type-2 identity
// and reconciliation. Platform identity and authorization remain adapters.
type Endpoint struct {
	NetworkID          string
	EndpointID         string
	Incarnation        string
	MACAddress         string
	IPAddresses        []string
	VNI                uint32
	RouteDistinguisher string
	RouteTargets       []string
	LocalVTEP          string
	Advertise          bool
	MACOnly            bool
}

func (ep Endpoint) OwnershipKey() string {
	return strings.TrimSpace(ep.EndpointID) + "/" + strings.TrimSpace(ep.Incarnation)
}

func (ep Endpoint) RouteKey() string {
	return strings.Join([]string{
		strings.TrimSpace(ep.NetworkID),
		strings.TrimSpace(ep.EndpointID),
		strings.TrimSpace(ep.Incarnation),
		strings.TrimSpace(ep.RouteDistinguisher),
		strings.TrimSpace(ep.MACAddress),
		PrimaryIP(ep.IPAddresses),
		fmt.Sprintf("%d", ep.VNI),
		strings.TrimSpace(ep.LocalVTEP),
	}, "/")
}

func (ep Endpoint) Equivalent(other Endpoint) bool {
	return ep.NetworkID == other.NetworkID &&
		ep.EndpointID == other.EndpointID &&
		ep.Incarnation == other.Incarnation &&
		ep.RouteDistinguisher == other.RouteDistinguisher &&
		ep.MACAddress == other.MACAddress &&
		ep.VNI == other.VNI &&
		ep.MACOnly == other.MACOnly &&
		PrimaryIP(ep.IPAddresses) == PrimaryIP(other.IPAddresses) &&
		ep.LocalVTEP == other.LocalVTEP &&
		equalStringSet(ep.RouteTargets, other.RouteTargets)
}

// PrimaryIP preserves the existing first-parseable-address selection rule.
func PrimaryIP(addresses []string) string {
	for _, address := range addresses {
		trimmed := strings.TrimSpace(address)
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

func CanonicalMAC(value string) (string, error) {
	mac, err := net.ParseMAC(value)
	if err != nil {
		return "", err
	}
	return mac.String(), nil
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
