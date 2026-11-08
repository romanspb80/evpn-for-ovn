package ovnexport

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type OVNLogicalSwitchPort struct {
	UUID          string
	Name          string
	LogicalSwitch string
	Addresses     []string
	ExternalIDs   map[string]string
}

type OVNPortBinding struct {
	UUID        string
	LogicalPort string
	Chassis     string
	Up          *bool
}

type OVNStateSource interface {
	ListLogicalSwitchPorts(context.Context) ([]OVNLogicalSwitchPort, error)
	ListPortBindings(context.Context) ([]OVNPortBinding, error)
}

// FrameworkEndpointObserver adapts verified local OVN endpoint state into
// provider-neutral EVPN candidates. A local, ready SB binding and exact managed
// identity are required before a candidate is authorized for advertisement.
type FrameworkEndpointObserver struct {
	Source       OVNStateSource
	LocalNode    string
	LocalChassis string
	Networks     map[string]evpn.EVPNNetworkConfig
}

func parseFrameworkAddressPair(addresses []string) (mac string, ip string, ok bool) {
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

func normalizeFrameworkExternalIDs(ids map[string]string) map[string]string {
	out := make(map[string]string, len(ids))
	for key, value := range ids {
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func (o *FrameworkEndpointObserver) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
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

	bindingByName := make(map[string]OVNPortBinding, len(bindings))
	for _, binding := range bindings {
		name := strings.TrimSpace(binding.LogicalPort)
		if name == "" {
			return nil, fmt.Errorf("OVN Port_Binding has no logical port")
		}
		if _, exists := bindingByName[name]; exists {
			return nil, fmt.Errorf("duplicate OVN Port_Binding for %q", name)
		}
		bindingByName[name] = binding
	}

	resultByKey := map[string]evpn.LocalEndpoint{}
	seenLSPNames, seenLSPUUIDs := map[string]bool{}, map[string]bool{}
	for _, lsp := range ports {
		lspName := strings.TrimSpace(lsp.Name)
		ids := normalizeFrameworkExternalIDs(lsp.ExternalIDs)
		networkID := ids[attachment.ExternalIDNetworkID]
		cfg, ok := o.Networks[networkID]
		if !ok || !cfg.Enabled || cfg.ExportSource() != "framework" {
			continue
		}
		if lspName == "" || strings.TrimSpace(lsp.UUID) == "" {
			return nil, fmt.Errorf("framework LSP for network %q has incomplete identity", networkID)
		}
		if seenLSPNames[lspName] || seenLSPUUIDs[lsp.UUID] {
			return nil, fmt.Errorf("framework OVN snapshot has duplicate LSP identity")
		}
		seenLSPNames[lspName], seenLSPUUIDs[lsp.UUID] = true, true
		if strings.TrimSpace(lsp.LogicalSwitch) != cfg.LogicalSwitch {
			continue
		}
		endpointID := ids[attachment.ExternalIDEndpointID]
		incarnation := ids[attachment.ExternalIDIncarnation]
		attachmentID := ids[attachment.ExternalIDAttachID]
		workloadUID := ids[attachment.ExternalIDWorkloadUID]
		if endpointID == "" || incarnation == "" || attachmentID == "" || workloadUID == "" ||
			endpointID != string(endpoint.NewEndpointID(networkID, attachmentID, workloadUID)) ||
			ids[attachment.ExternalIDManagedBy] != attachment.OwnerValue ||
			ids[attachment.ExternalIDIfaceID] != lspName {
			continue
		}
		binding, bound := bindingByName[lspName]
		if !bound || strings.TrimSpace(binding.Chassis) != strings.TrimSpace(o.LocalChassis) {
			continue
		}
		if binding.Up == nil {
			return nil, fmt.Errorf("framework Port_Binding %q readiness is uncertain", lspName)
		}
		if !*binding.Up {
			continue
		}
		mac, ip, ok := parseFrameworkAddressPair(lsp.Addresses)
		if !ok {
			continue
		}

		ep := evpn.LocalEndpoint{
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
		if _, exists := resultByKey[ep.RouteKey()]; exists {
			return nil, fmt.Errorf("multiple framework LSPs identify the same EVPN endpoint route")
		}
		resultByKey[ep.RouteKey()] = ep
	}

	result := make([]evpn.LocalEndpoint, 0, len(resultByKey))
	for _, ep := range resultByKey {
		result = append(result, ep)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RouteKey() < result[j].RouteKey() })
	return result, nil
}
