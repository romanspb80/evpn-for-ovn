package evpn

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
)

type OVNStateSource interface {
	ListLogicalSwitchPorts(ctx context.Context) ([]OVNLogicalSwitchPort, error)
	ListPortBindings(ctx context.Context) ([]OVNPortBinding, error)
}

// OVNEndpointObserver supports package-level EVPN integration tests. Runtime
// endpoint observation is wired through Node-Agent.
type OVNEndpointObserver struct {
	Source       OVNStateSource
	LocalNode    string
	LocalChassis string
	Networks     map[string]EVPNNetworkConfig
}

func parseAddressPair(addresses []string) (mac string, ip string, ok bool) {
	for _, raw := range addresses {
		parts := strings.Fields(strings.TrimSpace(raw))
		if len(parts) < 2 {
			continue
		}
		candidateMAC := strings.TrimSpace(parts[0])
		candidateIP := strings.TrimSpace(parts[1])
		if _, err := net.ParseMAC(candidateMAC); err != nil {
			continue
		}
		if net.ParseIP(candidateIP) == nil {
			parsedIP, _, err := net.ParseCIDR(candidateIP)
			if err != nil || parsedIP == nil {
				continue
			}
			candidateIP = parsedIP.String()
		}
		return candidateMAC, candidateIP, true
	}
	return "", "", false
}

func normalizeExternalIDs(ids map[string]string) map[string]string {
	out := make(map[string]string, len(ids))
	for key, value := range ids {
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func (o *OVNEndpointObserver) DesiredLocalEndpoints(ctx context.Context) ([]LocalEndpoint, error) {
	if o.Source == nil {
		return nil, fmt.Errorf("ovn source is required")
	}
	if strings.TrimSpace(o.LocalChassis) == "" {
		return nil, fmt.Errorf("local chassis is required")
	}
	ports, err := o.Source.ListLogicalSwitchPorts(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := o.Source.ListPortBindings(ctx)
	if err != nil {
		return nil, err
	}

	bindingByPort := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if strings.TrimSpace(binding.LogicalPort) == "" {
			continue
		}
		bindingByPort[strings.TrimSpace(binding.LogicalPort)] = strings.TrimSpace(binding.Chassis)
	}

	resultByKey := map[string]LocalEndpoint{}
	for _, lsp := range ports {
		lspName := strings.TrimSpace(lsp.Name)
		if lspName == "" || strings.TrimSpace(bindingByPort[lspName]) != strings.TrimSpace(o.LocalChassis) {
			continue
		}
		ids := normalizeExternalIDs(lsp.ExternalIDs)
		networkID := ids[attachment.ExternalIDNetworkID]
		cfg, ok := o.Networks[networkID]
		if !ok || !cfg.Enabled {
			continue
		}
		endpointID := ids[attachment.ExternalIDEndpointID]
		incarnation := ids[attachment.ExternalIDIncarnation]
		attachmentID := ids[attachment.ExternalIDAttachID]
		workloadUID := ids[attachment.ExternalIDWorkloadUID]
		if endpointID == "" || incarnation == "" || attachmentID == "" || workloadUID == "" {
			continue
		}
		mac, ip, ok := parseAddressPair(lsp.Addresses)
		if !ok {
			continue
		}
		ep := LocalEndpoint{
			NetworkID:          networkID,
			EndpointID:         endpointID,
			Incarnation:        incarnation,
			MACAddress:         mac,
			IPAddresses:        []string{ip},
			VNI:                cfg.VNI,
			RouteDistinguisher: cfg.RouteDistinguisher,
			RouteTargets:       append([]string(nil), cfg.RouteTargets...),
			LocalVTEP:          cfg.LocalVTEP,
			NodeName:           strings.TrimSpace(o.LocalNode),
			Chassis:            strings.TrimSpace(o.LocalChassis),
			Advertise:          true,
		}
		resultByKey[ep.RouteKey()] = ep
	}
	result := make([]LocalEndpoint, 0, len(resultByKey))
	for _, ep := range resultByKey {
		result = append(result, ep)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RouteKey() < result[j].RouteKey() })
	return result, nil
}
