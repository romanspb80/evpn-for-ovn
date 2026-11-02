package nodeagent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

// nativeOVNPorts supplies chassis-local static VIF provenance and an IP
// allowlist. NB/SB IPs alone never suffice for advertisement.
type nativeOVNPorts struct {
	source exportStateSource
	config evpn.ExporterConfig
}

func (o *nativeOVNPorts) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	data, err := o.source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	chassis := ""
	for _, row := range data.Chassis {
		if row.Name == o.config.LocalChassis {
			if chassis != "" {
				return nil, fmt.Errorf("ambiguous local OVN chassis")
			}
			chassis = row.UUID
		}
	}
	if chassis == "" {
		return nil, fmt.Errorf("local OVN chassis absent")
	}
	var result []evpn.LocalEndpoint
	for _, cfg := range o.config.Networks {
		if !cfg.Enabled || cfg.ExportSource() != "ovn-netlink" {
			continue
		}
		members := map[string]bool{}
		switchUUID := ""
		for _, sw := range data.Switches {
			if sw.Name != cfg.LogicalSwitch {
				continue
			}
			if switchUUID != "" {
				return nil, fmt.Errorf("ambiguous native logical switch")
			}
			switchUUID = sw.UUID
			for key, expected := range map[string]string{
				"dynamic-routing-vni":              strconv.FormatUint(uint64(cfg.VNI), 10),
				"dynamic-routing-bridge-ifname":    cfg.BridgeDevice,
				"dynamic-routing-vxlan-ifname":     cfg.VXLANDevice,
				"dynamic-routing-advertise-ifname": cfg.AdvertiseDevice,
			} {
				if sw.OtherConfig[key] != expected {
					return nil, fmt.Errorf("network %s native OVN mapping differs at %s", cfg.NetworkID, key)
				}
			}
			if !strings.Contains(","+sw.OtherConfig["dynamic-routing-redistribute"]+",", ",fdb,") {
				return nil, fmt.Errorf("native local FDB redistribution is not enabled")
			}
			for _, id := range sw.Ports {
				members[id] = true
			}
		}
		if switchUUID == "" {
			return nil, fmt.Errorf("native logical switch %s absent; observation non-authoritative", cfg.LogicalSwitch)
		}
		for _, lsp := range data.LSPs {
			if !members[lsp.UUID] {
				continue
			}
			for _, pb := range data.Bindings {
				if pb.LogicalPort != lsp.Name || pb.Chassis == nil || *pb.Chassis != chassis {
					continue
				}
				if pb.Up == nil || !*pb.Up {
					return nil, fmt.Errorf("local native port %s readiness uncertain", lsp.Name)
				}
				if pb.Type != "" || pb.Options["dynamic-routing-advertise"] == "false" {
					continue
				}
				dpValid := false
				for _, dp := range data.Datapaths {
					if dp.UUID == pb.Datapath && dp.ExternalIDs["logical-switch"] == switchUUID {
						dpValid = true
					}
				}
				if !dpValid || pb.UUID == "" || lsp.UUID == "" {
					return nil, fmt.Errorf("native PB datapath/identity mismatch")
				}
				if len(lsp.Addresses) != 1 || len(pb.MAC) != 1 || lsp.Addresses[0] != pb.MAC[0] {
					return nil, fmt.Errorf("native port %s requires a single converged static MAC binding", lsp.Name)
				}
				parts := strings.Fields(pb.MAC[0])
				if len(parts) == 0 {
					return nil, fmt.Errorf("native port %s MAC absent", lsp.Name)
				}
				mac, err := net.ParseMAC(parts[0])
				if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
					return nil, fmt.Errorf("native port %s invalid unicast MAC", lsp.Name)
				}
				var allowedIPs []string
				for _, raw := range parts[1:] {
					ip, err := netip.ParseAddr(raw)
					if err != nil {
						prefix, prefixErr := netip.ParsePrefix(raw)
						if prefixErr != nil {
							return nil, fmt.Errorf("native port %s invalid static IP %q", lsp.Name, raw)
						}
						ip = prefix.Addr()
					}
					if ip.IsUnspecified() || ip.IsMulticast() {
						return nil, fmt.Errorf("native port %s requires unicast static IPs", lsp.Name)
					}
					allowedIPs = append(allowedIPs, ip.Unmap().String())
				}
				result = append(result, evpn.LocalEndpoint{
					NetworkID: cfg.NetworkID, EndpointID: "ovn-netlink/" + lsp.UUID, Incarnation: pb.UUID,
					MACAddress: mac.String(), MACOnly: len(allowedIPs) == 0, IPAddresses: allowedIPs, VNI: cfg.VNI,
					RouteDistinguisher: cfg.RouteDistinguisher, RouteTargets: append([]string(nil), cfg.RouteTargets...),
					LocalVTEP: cfg.LocalVTEP, NodeName: o.config.NodeName, Chassis: o.config.LocalChassis, Advertise: true,
				})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RouteKey() < result[j].RouteKey() })
	return result, nil
}

type selectedEndpointObserver struct {
	cni       evpn.EndpointObserver
	framework evpn.EndpointObserver
	native    evpn.EndpointObserver
}

func (o selectedEndpointObserver) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	var endpoints []evpn.LocalEndpoint
	for _, observer := range []evpn.EndpointObserver{o.cni, o.framework, o.native} {
		if observer == nil {
			continue
		}
		observed, err := observer.DesiredLocalEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, observed...)
	}
	return endpoints, nil
}
