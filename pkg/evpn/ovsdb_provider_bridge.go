package evpn

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

const (
	ovsdbBridgeMappingsKey  = "ovn-bridge-mappings"
	ovsdbNetworkIDKey       = "evpn4ovn-network-id"
	ovsdbLogicalSwitchKey   = "evpn4ovn-logical-switch"
	ovsdbPhysicalNetworkKey = "evpn4ovn-physical-network"
	ovsdbProviderBridgeKey  = "evpn4ovn-provider-bridge"
	ovsdbBridgeRole         = "provider-bridge"
	ovsdbBridgeLocalRole    = "provider-bridge-local"
)

func validOVSDBBridgeMapping(physicalNetwork, bridge string) bool {
	if strings.TrimSpace(physicalNetwork) == "" || strings.TrimSpace(bridge) == "" {
		return false
	}
	if len(bridge) > 15 || bridge == "." || bridge == ".." || bridge == "br-int" {
		return false
	}
	for _, value := range []string{physicalNetwork, bridge} {
		if strings.ContainsAny(value, ":,") || strings.ContainsAny(value, " \t\r\n") {
			return false
		}
	}
	return true
}

func validateOVSDBNetwork(network Model1Network) error {
	if strings.TrimSpace(network.NetworkID) == "" ||
		strings.TrimSpace(network.LogicalSwitch) == "" ||
		!validOVSDBBridgeMapping(network.PhysicalNetwork, network.ProviderBridge) ||
		network.VNI == 0 || network.VNI > 16777215 {
		return fmt.Errorf("invalid OVSDB-Programmed provider network identity")
	}
	return nil
}

func ovsdbNetworkExternalIDs(network Model1Network) map[string]string {
	return map[string]string{
		ovsdbManagedByKey:       ovsdbOwner,
		ovsdbNetworkIDKey:       network.NetworkID,
		ovsdbLogicalSwitchKey:   network.LogicalSwitch,
		ovsdbPhysicalNetworkKey: network.PhysicalNetwork,
		ovsdbProviderBridgeKey:  network.ProviderBridge,
		"evpn4ovn-vni":          fmt.Sprint(network.VNI),
	}
}

func ovsdbProviderBridgeExternalIDs(network Model1Network) map[string]string {
	ids := ovsdbNetworkExternalIDs(network)
	ids[ovsdbRoleKey] = ovsdbBridgeRole
	return ids
}

func ovsdbProviderBridgeLocalExternalIDs(network Model1Network) map[string]string {
	return map[string]string{
		ovsdbManagedByKey:      ovsdbOwner,
		ovsdbRoleKey:           ovsdbBridgeLocalRole,
		ovsdbNetworkIDKey:      network.NetworkID,
		ovsdbProviderBridgeKey: network.ProviderBridge,
	}
}

func ovsdbNetworkPortName(network Model1Network) string {
	return "m1ln-" + ovsdbTunnelID(Model1TunnelKey{
		NetworkID: network.NetworkID, LogicalSwitch: network.LogicalSwitch, VNI: network.VNI,
	})[:10]
}

func ovsdbBridgeMappingOwnerKey(physicalNetwork string) string {
	return "evpn4ovn-model1-bridge-map-" + ovsdbTunnelID(Model1TunnelKey{
		NetworkID: physicalNetwork, LogicalSwitch: "provider", VNI: 1,
	})[:12]
}

func (p *LibovsdbModel1Programmer) EnsureModel1ProviderBridge(ctx context.Context, network Model1Network) error {
	if err := validateOVSDBNetwork(network); err != nil {
		return err
	}
	if err := p.validateOVSDBBridgeMappingAvailability(ctx, network); err != nil {
		return err
	}

	var bridges []vswitch.Bridge
	var ports []vswitch.Port
	var interfaces []vswitch.Interface
	if err := p.ovs.List(ctx, &bridges); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &ports); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &interfaces); err != nil {
		return err
	}
	var bridge *vswitch.Bridge
	for i := range bridges {
		if bridges[i].Name != network.ProviderBridge {
			continue
		}
		if bridge != nil {
			return fmt.Errorf("provider bridge name is not unique")
		}
		bridge = &bridges[i]
	}
	if bridge != nil {
		if !ovsdbIDsMatch(bridge.ExternalIDs, ovsdbProviderBridgeExternalIDs(network)) {
			return fmt.Errorf("provider bridge %q exists but is not owned by this OVSDB-Programmed network", bridge.Name)
		}
		if bridge.FailMode == nil || *bridge.FailMode != vswitch.BridgeFailModeStandalone || len(bridge.Controller) != 0 {
			return fmt.Errorf("owned OVSDB-Programmed provider bridge %q has conflicting controller or fail-mode configuration", bridge.Name)
		}
		root, err := ovsdbOpenvSwitchRoot(ctx, p)
		if err != nil {
			return err
		}
		if !ovsdbContains(root.Bridges, bridge.UUID) {
			return fmt.Errorf("owned OVSDB-Programmed provider bridge %q is not attached to Open_vSwitch", bridge.Name)
		}
		return validateOVSDBProviderBridgeLocalPort(*bridge, ports, interfaces, network)
	}
	for i := range ports {
		if ports[i].Name == network.ProviderBridge {
			return fmt.Errorf("provider bridge local port %q already exists without its owned bridge", network.ProviderBridge)
		}
	}
	for i := range interfaces {
		if interfaces[i].Name == network.ProviderBridge {
			return fmt.Errorf("provider bridge local interface %q already exists without its owned bridge", network.ProviderBridge)
		}
	}
	root, err := ovsdbOpenvSwitchRoot(ctx, p)
	if err != nil {
		return err
	}
	iface := &vswitch.Interface{
		UUID: uuid.NewString(), Name: network.ProviderBridge, Type: "internal",
		ExternalIDs: ovsdbProviderBridgeLocalExternalIDs(network),
	}
	port := &vswitch.Port{
		UUID: uuid.NewString(), Name: network.ProviderBridge,
		Interfaces: []string{iface.UUID}, ExternalIDs: ovsdbProviderBridgeLocalExternalIDs(network),
	}
	failMode := vswitch.BridgeFailModeStandalone
	bridge = &vswitch.Bridge{
		UUID: uuid.NewString(), Name: network.ProviderBridge, FailMode: &failMode,
		Ports: []string{port.UUID}, ExternalIDs: ovsdbProviderBridgeExternalIDs(network),
	}
	createIface, err := p.ovs.Create(iface)
	if err != nil {
		return err
	}
	createPort, err := p.ovs.Create(port)
	if err != nil {
		return err
	}
	createBridge, err := p.ovs.Create(bridge)
	if err != nil {
		return err
	}
	attachBridge, err := p.ovs.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Mutate(root, model.Mutation{
		Field: &root.Bridges, Mutator: ovsdb.MutateOperationInsert, Value: []string{bridge.UUID},
	})
	if err != nil {
		return err
	}
	ops := append(createIface, createPort...)
	ops = append(ops, createBridge...)
	ops = append(ops, attachBridge...)
	return transactOVSDB(ctx, p.ovs, ops...)
}

func validateOVSDBProviderBridgeLocalPort(
	bridge vswitch.Bridge,
	ports []vswitch.Port,
	interfaces []vswitch.Interface,
	network Model1Network,
) error {
	var port *vswitch.Port
	for i := range ports {
		if ports[i].Name != network.ProviderBridge {
			continue
		}
		if port != nil {
			return fmt.Errorf("provider bridge local port name is not unique")
		}
		port = &ports[i]
	}
	if port == nil || !ovsdbIDsMatch(port.ExternalIDs, ovsdbProviderBridgeLocalExternalIDs(network)) ||
		len(port.Interfaces) != 1 || !ovsdbContains(bridge.Ports, port.UUID) {
		return fmt.Errorf("provider bridge %q has no correctly owned local port", network.ProviderBridge)
	}
	var iface *vswitch.Interface
	for i := range interfaces {
		if interfaces[i].Name != network.ProviderBridge {
			continue
		}
		if iface != nil {
			return fmt.Errorf("provider bridge local interface name is not unique")
		}
		iface = &interfaces[i]
	}
	if iface == nil || iface.UUID != port.Interfaces[0] || iface.Type != "internal" ||
		!ovsdbIDsMatch(iface.ExternalIDs, ovsdbProviderBridgeLocalExternalIDs(network)) {
		return fmt.Errorf("provider bridge %q has no correctly owned internal interface", network.ProviderBridge)
	}
	return nil
}

func (p *LibovsdbModel1Programmer) DeleteModel1ProviderBridge(ctx context.Context, network Model1Network) error {
	if err := validateOVSDBNetwork(network); err != nil {
		return err
	}
	if err := p.ensureOVSDBNetworkHasNoTunnels(ctx, network); err != nil {
		return err
	}
	root, err := ovsdbOpenvSwitchRoot(ctx, p)
	if err != nil {
		return err
	}
	mappings, err := parseOVSDBBridgeMappings(root.ExternalIDs[ovsdbBridgeMappingsKey])
	if err != nil {
		return err
	}
	for physnet, bridge := range mappings {
		if bridge == network.ProviderBridge {
			return fmt.Errorf("refusing to delete provider bridge %q while physical network %q maps to it", bridge, physnet)
		}
	}
	var bridges []vswitch.Bridge
	var ports []vswitch.Port
	var interfaces []vswitch.Interface
	if err := p.ovs.List(ctx, &bridges); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &ports); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &interfaces); err != nil {
		return err
	}
	var bridge *vswitch.Bridge
	for i := range bridges {
		if bridges[i].Name != network.ProviderBridge {
			continue
		}
		if bridge != nil {
			return fmt.Errorf("provider bridge name is not unique")
		}
		bridge = &bridges[i]
	}
	if bridge == nil {
		for i := range ports {
			if ports[i].Name == network.ProviderBridge {
				return fmt.Errorf("provider bridge %q is absent but a same-name OVS port remains", network.ProviderBridge)
			}
		}
		for i := range interfaces {
			if interfaces[i].Name == network.ProviderBridge {
				return fmt.Errorf("provider bridge %q is absent but a same-name OVS interface remains", network.ProviderBridge)
			}
		}
		return nil
	}
	if !ovsdbIDsMatch(bridge.ExternalIDs, ovsdbProviderBridgeExternalIDs(network)) ||
		bridge.FailMode == nil || *bridge.FailMode != vswitch.BridgeFailModeStandalone ||
		len(bridge.Controller) != 0 || !ovsdbContains(root.Bridges, bridge.UUID) {
		return fmt.Errorf("refusing to delete changed, unowned, or detached provider bridge %q", network.ProviderBridge)
	}
	if len(bridge.Ports) != 1 {
		return fmt.Errorf("provider bridge %q still has attached ports; withdraw the localnet path and tunnels before retrying",
			network.ProviderBridge)
	}
	if err := validateOVSDBProviderBridgeLocalPort(*bridge, ports, interfaces, network); err != nil {
		return fmt.Errorf("refusing to delete provider bridge %q: %w", network.ProviderBridge, err)
	}
	localPort := portsByName(ports, network.ProviderBridge)
	localInterface := interfacesByName(interfaces, network.ProviderBridge)
	var ops []ovsdb.Operation
	detach, err := p.ovs.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Mutate(root, model.Mutation{
		Field: &root.Bridges, Mutator: ovsdb.MutateOperationDelete, Value: []string{bridge.UUID},
	})
	if err != nil {
		return err
	}
	ops = append(ops, detach...)
	deleteBridge, err := p.ovs.Where(&vswitch.Bridge{UUID: bridge.UUID}).Delete()
	if err != nil {
		return err
	}
	deletePort, err := p.ovs.Where(&vswitch.Port{UUID: localPort.UUID}).Delete()
	if err != nil {
		return err
	}
	deleteInterface, err := p.ovs.Where(&vswitch.Interface{UUID: localInterface.UUID}).Delete()
	if err != nil {
		return err
	}
	ops = append(ops, deleteBridge...)
	ops = append(ops, deletePort...)
	ops = append(ops, deleteInterface...)
	return transactOVSDB(ctx, p.ovs, ops...)
}

func portsByName(rows []vswitch.Port, name string) *vswitch.Port {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return &vswitch.Port{}
}

func interfacesByName(rows []vswitch.Interface, name string) *vswitch.Interface {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return &vswitch.Interface{}
}

func (p *LibovsdbModel1Programmer) EnsureModel1Network(ctx context.Context, network Model1Network) error {
	if err := validateOVSDBNetwork(network); err != nil {
		return err
	}
	if err := p.EnsureModel1ProviderBridge(ctx, network); err != nil {
		return fmt.Errorf("ensure OVSDB-Programmed provider bridge: %w", err)
	}
	return runOVSDBDatabaseSteps("ensure provider network", "OVSDB", "NBDB",
		"retry EnsureModel1Network with the same network specification",
		func() error { return p.ensureOVSDBBridgeMapping(ctx, network) },
		func() error { return p.ensureOVSDBLocalnetLSP(ctx, network) })
}

func (p *LibovsdbModel1Programmer) DeleteModel1Network(ctx context.Context, network Model1Network) error {
	if err := validateOVSDBNetwork(network); err != nil {
		return err
	}
	if err := p.ensureOVSDBNetworkHasNoTunnels(ctx, network); err != nil {
		return err
	}
	return runOVSDBDatabaseSteps("delete provider network", "NBDB", "OVSDB",
		"retry DeleteModel1Network with the same network specification",
		func() error { return p.deleteOVSDBLocalnetLSP(ctx, network) },
		func() error { return p.deleteOVSDBBridgeMapping(ctx, network) })
}

func (p *LibovsdbModel1Programmer) ensureOVSDBBridgeMapping(ctx context.Context, network Model1Network) error {
	var bridges []vswitch.Bridge
	if err := p.ovs.List(ctx, &bridges); err != nil {
		return err
	}
	var bridge *vswitch.Bridge
	for i := range bridges {
		if bridges[i].Name != network.ProviderBridge {
			continue
		}
		if bridge != nil {
			return fmt.Errorf("provider bridge name is not unique")
		}
		bridge = &bridges[i]
	}
	if bridge == nil {
		return fmt.Errorf("dedicated OVSDB-Programmed provider bridge %q must be provisioned before activation", network.ProviderBridge)
	}
	if len(bridge.Controller) != 0 {
		return fmt.Errorf("OVSDB-Programmed provider bridge %q must not have an OpenFlow controller", bridge.Name)
	}
	if bridge.FailMode != nil && *bridge.FailMode != vswitch.BridgeFailModeStandalone {
		return fmt.Errorf("OVSDB-Programmed provider bridge %q must use standalone fail mode", bridge.Name)
	}

	root, err := ovsdbOpenvSwitchRoot(ctx, p)
	if err != nil {
		return err
	}
	mappings, err := parseOVSDBBridgeMappings(root.ExternalIDs[ovsdbBridgeMappingsKey])
	if err != nil {
		return err
	}
	if err := validateOVSDBBridgeMappings(root, mappings, network); err != nil {
		return err
	}
	ownerKey := ovsdbBridgeMappingOwnerKey(network.PhysicalNetwork)
	if owner, exists := root.ExternalIDs[ownerKey]; exists && owner != network.ProviderBridge {
		return fmt.Errorf("physical network %q bridge mapping has conflicting OVSDB-Programmed ownership", network.PhysicalNetwork)
	}
	if mappings[network.PhysicalNetwork] == network.ProviderBridge {
		return nil
	}
	mappings[network.PhysicalNetwork] = network.ProviderBridge
	root.ExternalIDs = copyStringMap(root.ExternalIDs)
	root.ExternalIDs[ovsdbBridgeMappingsKey] = formatOVSDBBridgeMappings(mappings)
	root.ExternalIDs[ownerKey] = network.ProviderBridge
	ops, err := p.ovs.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Update(root, &root.ExternalIDs)
	if err != nil {
		return err
	}
	return transactOVSDB(ctx, p.ovs, ops...)
}

func (p *LibovsdbModel1Programmer) validateOVSDBBridgeMappingAvailability(
	ctx context.Context,
	network Model1Network,
) error {
	root, err := ovsdbOpenvSwitchRoot(ctx, p)
	if err != nil {
		return err
	}
	mappings, err := parseOVSDBBridgeMappings(root.ExternalIDs[ovsdbBridgeMappingsKey])
	if err != nil {
		return err
	}
	return validateOVSDBBridgeMappings(root, mappings, network)
}

func validateOVSDBBridgeMappings(root *vswitch.OpenvSwitch, mappings map[string]string, network Model1Network) error {
	if mappedBridge, exists := mappings[network.PhysicalNetwork]; exists && mappedBridge != network.ProviderBridge {
		return fmt.Errorf("physical network %q is already mapped to bridge %q", network.PhysicalNetwork, mappedBridge)
	}
	for physicalNetwork, mappedBridge := range mappings {
		if physicalNetwork != network.PhysicalNetwork && mappedBridge == network.ProviderBridge {
			return fmt.Errorf("provider bridge %q is already assigned to physical network %q",
				network.ProviderBridge, physicalNetwork)
		}
	}
	ownerKey := ovsdbBridgeMappingOwnerKey(network.PhysicalNetwork)
	if owner, exists := root.ExternalIDs[ownerKey]; exists && owner != network.ProviderBridge {
		return fmt.Errorf("physical network %q bridge mapping has conflicting OVSDB-Programmed ownership", network.PhysicalNetwork)
	}
	return nil
}

func (p *LibovsdbModel1Programmer) deleteOVSDBBridgeMapping(ctx context.Context, network Model1Network) error {
	root, err := ovsdbOpenvSwitchRoot(ctx, p)
	if err != nil {
		return err
	}
	ownerKey := ovsdbBridgeMappingOwnerKey(network.PhysicalNetwork)
	owner, owned := root.ExternalIDs[ownerKey]
	if !owned {
		return nil
	}
	if owner != network.ProviderBridge {
		return fmt.Errorf("refusing to remove physical network mapping owned by another provider bridge")
	}
	mappings, err := parseOVSDBBridgeMappings(root.ExternalIDs[ovsdbBridgeMappingsKey])
	if err != nil {
		return err
	}
	if mappedBridge := mappings[network.PhysicalNetwork]; mappedBridge != "" {
		if mappedBridge != network.ProviderBridge {
			return fmt.Errorf("refusing to remove changed physical network mapping to %q", mappedBridge)
		}
		delete(mappings, network.PhysicalNetwork)
	}
	root.ExternalIDs = copyStringMap(root.ExternalIDs)
	delete(root.ExternalIDs, ownerKey)
	if len(mappings) == 0 {
		delete(root.ExternalIDs, ovsdbBridgeMappingsKey)
	} else {
		root.ExternalIDs[ovsdbBridgeMappingsKey] = formatOVSDBBridgeMappings(mappings)
	}
	ops, err := p.ovs.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Update(root, &root.ExternalIDs)
	if err != nil {
		return err
	}
	return transactOVSDB(ctx, p.ovs, ops...)
}

func (p *LibovsdbModel1Programmer) ensureOVSDBLocalnetLSP(ctx context.Context, network Model1Network) error {
	var switches []nbdb.LogicalSwitch
	if err := p.nb.List(ctx, &switches); err != nil {
		return err
	}
	var logicalSwitch *nbdb.LogicalSwitch
	for i := range switches {
		if switches[i].Name != network.LogicalSwitch {
			continue
		}
		if logicalSwitch != nil {
			return fmt.Errorf("logical switch name is not unique")
		}
		logicalSwitch = &switches[i]
	}
	if logicalSwitch == nil {
		return fmt.Errorf("logical switch %q not found", network.LogicalSwitch)
	}
	var ports []nbdb.LogicalSwitchPort
	if err := p.nb.List(ctx, &ports); err != nil {
		return err
	}
	var existing *nbdb.LogicalSwitchPort
	name := ovsdbNetworkPortName(network)
	for i := range ports {
		if ports[i].Name != name {
			continue
		}
		if existing != nil {
			return fmt.Errorf("OVSDB-Programmed localnet port name is not unique")
		}
		existing = &ports[i]
	}
	if existing == nil {
		port := ovsdbLocalnetLSP(network)
		create, err := p.nb.Create(port)
		if err != nil {
			return err
		}
		mutate, err := p.nb.Where(&nbdb.LogicalSwitch{UUID: logicalSwitch.UUID}).Mutate(logicalSwitch, model.Mutation{
			Field: &logicalSwitch.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{port.UUID},
		})
		if err != nil {
			return err
		}
		return transactOVSDB(ctx, p.nb, append(create, mutate...)...)
	}
	if !ovsdbIDsMatch(existing.ExternalIDs, ovsdbNetworkExternalIDs(network)) ||
		existing.Type != "localnet" ||
		!ovsdbMapsEqual(existing.Options, ovsdbLocalnetOptions(network)) ||
		!sameOVSDBAddresses(existing.Addresses, []string{"unknown"}) || existing.Tag != nil {
		return fmt.Errorf("logical switch port %q is not the expected owned OVSDB-Programmed localnet port", name)
	}
	owners := 0
	for i := range switches {
		if !ovsdbContains(switches[i].Ports, existing.UUID) {
			continue
		}
		owners++
		if switches[i].Name != network.LogicalSwitch {
			return fmt.Errorf("OVSDB-Programmed localnet port belongs to another logical switch")
		}
	}
	if owners > 1 {
		return fmt.Errorf("OVSDB-Programmed localnet port is attached to multiple logical switches")
	}
	if owners == 1 {
		return nil
	}
	mutate, err := p.nb.Where(&nbdb.LogicalSwitch{UUID: logicalSwitch.UUID}).Mutate(logicalSwitch, model.Mutation{
		Field: &logicalSwitch.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{existing.UUID},
	})
	if err != nil {
		return err
	}
	return transactOVSDB(ctx, p.nb, mutate...)
}

func (p *LibovsdbModel1Programmer) deleteOVSDBLocalnetLSP(ctx context.Context, network Model1Network) error {
	var switches []nbdb.LogicalSwitch
	var ports []nbdb.LogicalSwitchPort
	if err := p.nb.List(ctx, &switches); err != nil {
		return err
	}
	if err := p.nb.List(ctx, &ports); err != nil {
		return err
	}
	name := ovsdbNetworkPortName(network)
	var port *nbdb.LogicalSwitchPort
	for i := range ports {
		if ports[i].Name != name {
			continue
		}
		if port != nil {
			return fmt.Errorf("OVSDB-Programmed localnet port name is not unique")
		}
		port = &ports[i]
	}
	if port == nil {
		return nil
	}
	if !ovsdbIDsMatch(port.ExternalIDs, ovsdbNetworkExternalIDs(network)) ||
		port.Type != "localnet" ||
		!ovsdbMapsEqual(port.Options, ovsdbLocalnetOptions(network)) ||
		!sameOVSDBAddresses(port.Addresses, []string{"unknown"}) || port.Tag != nil {
		return fmt.Errorf("refusing to delete changed or unowned OVSDB-Programmed localnet port %q", name)
	}
	var ops []ovsdb.Operation
	for i := range switches {
		if !ovsdbContains(switches[i].Ports, port.UUID) {
			continue
		}
		if switches[i].Name != network.LogicalSwitch {
			return fmt.Errorf("refusing to detach OVSDB-Programmed localnet port from another logical switch")
		}
		mutate, err := p.nb.Where(&nbdb.LogicalSwitch{UUID: switches[i].UUID}).Mutate(&switches[i], model.Mutation{
			Field: &switches[i].Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{port.UUID},
		})
		if err != nil {
			return err
		}
		ops = append(ops, mutate...)
	}
	deleteOps, err := p.nb.Where(&nbdb.LogicalSwitchPort{UUID: port.UUID}).Delete()
	if err != nil {
		return err
	}
	return transactOVSDB(ctx, p.nb, append(ops, deleteOps...)...)
}

func (p *LibovsdbModel1Programmer) ensureOVSDBNetworkHasNoTunnels(ctx context.Context, network Model1Network) error {
	var interfaces []vswitch.Interface
	if err := p.ovs.List(ctx, &interfaces); err != nil {
		return err
	}
	for i := range interfaces {
		if interfaces[i].ExternalIDs[ovsdbManagedByKey] == ovsdbOwner &&
			interfaces[i].ExternalIDs[ovsdbRoleKey] == ovsdbTunnelRole &&
			interfaces[i].ExternalIDs[ovsdbNetworkIDKey] == network.NetworkID {
			return fmt.Errorf("refusing to delete provider network while OVSDB-Programmed tunnel %q remains", interfaces[i].Name)
		}
	}
	return nil
}

func ovsdbLocalnetLSP(network Model1Network) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		UUID:        uuid.NewString(),
		Name:        ovsdbNetworkPortName(network),
		Type:        "localnet",
		Addresses:   []string{"unknown"},
		Options:     ovsdbLocalnetOptions(network),
		ExternalIDs: ovsdbNetworkExternalIDs(network),
	}
}

func ovsdbLocalnetOptions(network Model1Network) map[string]string {
	return map[string]string{"network_name": network.PhysicalNetwork}
}

func ovsdbOpenvSwitchRoot(ctx context.Context, programmer *LibovsdbModel1Programmer) (*vswitch.OpenvSwitch, error) {
	var roots []vswitch.OpenvSwitch
	if err := programmer.ovs.List(ctx, &roots); err != nil {
		return nil, err
	}
	if len(roots) != 1 {
		return nil, fmt.Errorf("expected exactly one Open_vSwitch root row, got %d", len(roots))
	}
	return &roots[0], nil
}

func parseOVSDBBridgeMappings(raw string) (map[string]string, error) {
	mappings := make(map[string]string)
	if raw == "" {
		return mappings, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || !validOVSDBBridgeMapping(parts[0], parts[1]) {
			return nil, fmt.Errorf("invalid existing ovn-bridge-mappings entry %q", entry)
		}
		if _, exists := mappings[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate existing ovn-bridge-mappings entry for %q", parts[0])
		}
		mappings[parts[0]] = parts[1]
	}
	return mappings, nil
}

func formatOVSDBBridgeMappings(mappings map[string]string) string {
	keys := make([]string, 0, len(mappings))
	for key := range mappings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, key+":"+mappings[key])
	}
	return strings.Join(entries, ",")
}

func copyStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
