package evpn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	model1ManagedByKey = "evpn4ovn-managed-by"
	model1RoleKey      = "evpn4ovn-role"
	model1Owner        = "model1-nbdb"
	model1TunnelRole   = "tunnel"
)

type Model1Network struct {
	NetworkID       string
	LogicalSwitch   string
	PhysicalNetwork string
	ProviderBridge  string
	VNI             uint32
}

type Model1Config struct {
	Networks []Model1Network
	UDPPort  uint16
	// ExclusiveWriter is a caller assertion, not a distributed ownership lock.
	ExclusiveWriter bool
}

func (c Model1Config) Validate() error {
	if !c.ExclusiveWriter {
		return fmt.Errorf("OVSDB-Programmed integration requires explicit exclusive-writer ownership")
	}
	if c.UDPPort == 0 {
		return fmt.Errorf("OVSDB-Programmed integration requires an explicit VXLAN UDP port")
	}
	if len(c.Networks) != 1 {
		return fmt.Errorf("OVSDB-Programmed provider-bridge integration requires exactly one network mapping")
	}
	networks, switches, vnis, physnets := map[string]bool{}, map[string]bool{}, map[uint32]bool{}, map[string]bool{}
	for _, network := range c.Networks {
		if strings.TrimSpace(network.NetworkID) == "" ||
			strings.TrimSpace(network.LogicalSwitch) == "" ||
			!validModel1BridgeMapping(network.PhysicalNetwork, network.ProviderBridge) ||
			network.VNI == 0 || network.VNI > 16777215 ||
			networks[network.NetworkID] || switches[network.LogicalSwitch] ||
			vnis[network.VNI] || physnets[network.PhysicalNetwork] {
			return fmt.Errorf("OVSDB-Programmed network, logical switch, physnet, provider bridge, and VNI mapping must be valid and unique")
		}
		networks[network.NetworkID], switches[network.LogicalSwitch], vnis[network.VNI] = true, true, true
		physnets[network.PhysicalNetwork] = true
	}
	return nil
}

type Model1TunnelKey struct {
	NetworkID     string
	LogicalSwitch string
	VNI           uint32
	RemoteVTEP    string
}

type Model1Tunnel struct {
	Key       Model1TunnelKey
	Name      string
	Addresses []string
	UDPPort   uint16
	Bridge    string
}

type Model1TunnelProgrammer interface {
	DeleteModel1ProviderBridge(context.Context, Model1Network) error
	EnsureModel1Network(context.Context, Model1Network) error
	DeleteModel1Network(context.Context, Model1Network) error
	EnsureModel1Tunnel(context.Context, Model1Tunnel) error
	DeleteModel1Tunnel(context.Context, Model1Tunnel) error
}

type Model1PartialOperationError struct {
	Operation string
	Completed string
	Failed    string
	Recovery  string
	Err       error
}

func (e *Model1PartialOperationError) Error() string {
	return fmt.Sprintf("OVSDB-Programmed %s partially completed: %s committed, %s failed; recovery: %s: %v",
		e.Operation, e.Completed, e.Failed, e.Recovery, e.Err)
}

func (e *Model1PartialOperationError) Unwrap() error { return e.Err }

// Model1Type2Handler is an additive, opt-in OVSDB Type-2 route consumer. It is
// not part of the default deployment and requires an explicitly exclusive
// writer selection.
type Model1Type2Handler struct {
	mu         sync.Mutex
	reader     *ReadOnlyType2Importer
	programmer Model1TunnelProgrammer
	config     Model1Config
	active     map[string]Model1Tunnel
	generation uint64
	digest     string
}

func NewModel1Type2Handler(reader *ReadOnlyType2Importer, programmer Model1TunnelProgrammer, config Model1Config) (*Model1Type2Handler, error) {
	if reader == nil || programmer == nil {
		return nil, fmt.Errorf("validated GoBGP reader and OVSDB-Programmed tunnel programmer are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := reader.config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid shared remote import policy: %w", err)
	}
	for _, network := range config.Networks {
		matched := false
		for _, imported := range reader.config.Networks {
			if imported.NetworkID == network.NetworkID && imported.VNI == network.VNI {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("OVSDB-Programmed network %s/VNI %d is absent from the validated import policy", network.NetworkID, network.VNI)
		}
	}
	return &Model1Type2Handler{
		reader: reader, programmer: programmer, config: config,
		active: make(map[string]Model1Tunnel),
	}, nil
}

func (h *Model1Type2Handler) ReconcileOnce(ctx context.Context) error {
	snapshot, err := h.reader.Poll(ctx)
	if err != nil {
		return fmt.Errorf("OVSDB-Programmed remote route observation failed: %w", err)
	}
	return h.ReconcileSnapshot(ctx, snapshot)
}

func (h *Model1Type2Handler) ReconcileSnapshot(ctx context.Context, snapshot RemoteImportSnapshot) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !snapshot.Authoritative || snapshot.Generation == 0 {
		return fmt.Errorf("non-authoritative or uninitialized snapshot cannot mutate OVSDB-Programmed state")
	}
	digest := remoteHash(snapshot.Endpoints)
	if snapshot.Generation < h.generation ||
		(snapshot.Generation == h.generation && h.digest != "" && digest != h.digest) {
		return fmt.Errorf("stale or conflicting OVSDB-Programmed snapshot generation")
	}

	networks := make(map[string]Model1Network, len(h.config.Networks))
	for _, network := range h.config.Networks {
		networks[network.NetworkID] = network
	}
	desired := make(map[string]Model1Tunnel)
	macs, ips := make(map[string]DesiredRemoteEndpoint), make(map[string]DesiredRemoteEndpoint)
	for _, endpoint := range snapshot.Endpoints {
		if endpoint.ValidationState != "validated" || len(endpoint.Routes) == 0 ||
			endpoint.Key != remoteHash([]string{endpoint.NetworkID, endpoint.MAC, endpoint.IP}) {
			return fmt.Errorf("invalid validated OVSDB-Programmed endpoint identity")
		}
		network, ok := networks[endpoint.NetworkID]
		if !ok || network.VNI != endpoint.VNI || !unicastIPv4(endpoint.RemoteVTEP) {
			return fmt.Errorf("endpoint has no matching OVSDB-Programmed network/VNI mapping")
		}
		if _, err := net.ParseMAC(endpoint.MAC); err != nil {
			return fmt.Errorf("endpoint has invalid MAC: %w", err)
		}
		ipConfirmed := endpoint.IP == ""
		for _, route := range endpoint.Routes {
			validated, err := h.reader.config.ValidateRoute(route.Route)
			if err != nil || validated.NetworkID != endpoint.NetworkID || validated.VNI != endpoint.VNI ||
				route.NetworkID != endpoint.NetworkID || route.VNI != endpoint.VNI ||
				route.Route.MAC != endpoint.MAC || (route.Route.IP != endpoint.IP && route.Route.IP != "") ||
				route.Route.NextHop != endpoint.RemoteVTEP || route.Route.Source.Key == "" {
				return fmt.Errorf("endpoint differs from validated Type-2 route evidence: %v", err)
			}
			ipConfirmed = ipConfirmed || route.Route.IP == endpoint.IP
		}
		if !ipConfirmed {
			return fmt.Errorf("endpoint IP lacks matching Type-2 source evidence")
		}
		macSlot := endpoint.NetworkID + "/" + endpoint.MAC
		if previous, exists := macs[macSlot]; exists &&
			(previous.IP != endpoint.IP || previous.RemoteVTEP != endpoint.RemoteVTEP) {
			return fmt.Errorf("conflicting OVSDB-Programmed endpoint for MAC %s", endpoint.MAC)
		}
		macs[macSlot] = endpoint
		if endpoint.IP != "" {
			ipSlot := endpoint.NetworkID + "/" + endpoint.IP
			if previous, exists := ips[ipSlot]; exists &&
				(previous.MAC != endpoint.MAC || previous.RemoteVTEP != endpoint.RemoteVTEP) {
				return fmt.Errorf("conflicting OVSDB-Programmed endpoint for IP %s", endpoint.IP)
			}
			ips[ipSlot] = endpoint
		}

		key := Model1TunnelKey{
			NetworkID: endpoint.NetworkID, LogicalSwitch: network.LogicalSwitch,
			VNI: endpoint.VNI, RemoteVTEP: endpoint.RemoteVTEP,
		}
		tunnelID := model1TunnelID(key)
		tunnel, exists := desired[tunnelID]
		if !exists {
			tunnel = Model1Tunnel{
				Key: key, Name: model1TunnelName(key), UDPPort: h.config.UDPPort,
				Bridge: network.ProviderBridge,
			}
		} else if tunnel.Key != key {
			return fmt.Errorf("OVSDB-Programmed tunnel identifier collision")
		}
		address := endpoint.MAC
		if endpoint.IP != "" {
			address += " " + endpoint.IP
		}
		tunnel.Addresses = append(tunnel.Addresses, address)
		desired[tunnelID] = tunnel
	}
	for id, tunnel := range desired {
		sort.Strings(tunnel.Addresses)
		tunnel.Addresses = compactStrings(tunnel.Addresses)
		desired[id] = tunnel
	}
	remoteVTEPs := make(map[string]bool)
	for _, tunnel := range desired {
		remoteVTEPs[tunnel.Key.RemoteVTEP] = true
	}
	if len(remoteVTEPs) > 1 {
		return fmt.Errorf("OVSDB-Programmed provider-bridge integration supports one remote VTEP per network")
	}
	network := h.config.Networks[0]
	if len(desired) > 0 {
		if err := h.programmer.EnsureModel1Network(ctx, network); err != nil {
			return fmt.Errorf("ensure OVSDB-Programmed provider network %s: %w", network.NetworkID, err)
		}
	}

	// Withdraw an obsolete static tunnel before creating its replacement.
	for _, id := range sortedTunnelKeys(h.active) {
		if _, stillDesired := desired[id]; stillDesired {
			continue
		}
		if err := h.programmer.DeleteModel1Tunnel(ctx, h.active[id]); err != nil {
			return fmt.Errorf("delete OVSDB-Programmed tunnel %s: %w", h.active[id].Name, err)
		}
		delete(h.active, id)
	}
	for _, id := range sortedTunnelKeys(desired) {
		tunnel := desired[id]
		if err := h.programmer.EnsureModel1Tunnel(ctx, tunnel); err != nil {
			return fmt.Errorf("ensure OVSDB-Programmed tunnel %s: %w", tunnel.Name, err)
		}
		h.active[id] = tunnel
	}
	if len(desired) == 0 {
		if err := h.programmer.DeleteModel1Network(ctx, network); err != nil {
			return fmt.Errorf("delete OVSDB-Programmed provider network %s: %w", network.NetworkID, err)
		}
		if err := h.programmer.DeleteModel1ProviderBridge(ctx, network); err != nil {
			return fmt.Errorf("delete OVSDB-Programmed provider bridge %s: %w", network.ProviderBridge, err)
		}
	}
	h.generation, h.digest = snapshot.Generation, digest
	return nil
}

func model1TunnelID(key Model1TunnelKey) string {
	sum := sha256.Sum256([]byte(key.NetworkID + "\x00" + key.LogicalSwitch + "\x00" +
		strconv.FormatUint(uint64(key.VNI), 10) + "\x00" + key.RemoteVTEP))
	return hex.EncodeToString(sum[:])
}

func model1TunnelName(key Model1TunnelKey) string {
	return "m1-" + model1TunnelID(key)[:12]
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func sortedTunnelKeys(tunnels map[string]Model1Tunnel) []string {
	keys := make([]string, 0, len(tunnels))
	for key := range tunnels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type LibovsdbModel1Programmer struct {
	nb  client.Client
	ovs client.Client
}

func NewLibovsdbModel1Programmer(nb, ovs client.Client) (*LibovsdbModel1Programmer, error) {
	if nb == nil || ovs == nil {
		return nil, fmt.Errorf("OVSDB-Programmed integration requires connected NBDB and OVSDB clients")
	}
	return &LibovsdbModel1Programmer{nb: nb, ovs: ovs}, nil
}

func (p *LibovsdbModel1Programmer) EnsureModel1Tunnel(ctx context.Context, tunnel Model1Tunnel) error {
	if err := validateModel1Tunnel(tunnel); err != nil {
		return err
	}
	return p.ensureModel1OVSTunnel(ctx, tunnel)
}

func (p *LibovsdbModel1Programmer) DeleteModel1Tunnel(ctx context.Context, tunnel Model1Tunnel) error {
	if err := validateModel1Tunnel(tunnel); err != nil {
		return err
	}
	return p.deleteModel1OVSTunnel(ctx, tunnel)
}

func runModel1DatabaseSteps(operation, firstDatabase, secondDatabase, recovery string, first, second func() error) error {
	if err := first(); err != nil {
		return fmt.Errorf("OVSDB-Programmed %s %s step: %w", operation, firstDatabase, err)
	}
	if err := second(); err != nil {
		return &Model1PartialOperationError{
			Operation: operation, Completed: firstDatabase, Failed: secondDatabase,
			Recovery: recovery, Err: err,
		}
	}
	return nil
}

func validateModel1Tunnel(t Model1Tunnel) error {
	if strings.TrimSpace(t.Key.NetworkID) == "" || strings.TrimSpace(t.Key.LogicalSwitch) == "" ||
		t.Key.VNI == 0 || t.Key.VNI > 16777215 || !unicastIPv4(t.Key.RemoteVTEP) ||
		t.Name != model1TunnelName(t.Key) || t.UDPPort == 0 || strings.TrimSpace(t.Bridge) == "" {
		return fmt.Errorf("invalid OVSDB-Programmed tunnel identity")
	}
	if len(t.Addresses) == 0 {
		return fmt.Errorf("OVSDB-Programmed tunnel requires at least one remote endpoint address")
	}
	for _, address := range t.Addresses {
		fields := strings.Fields(address)
		if len(fields) < 1 || len(fields) > 2 {
			return fmt.Errorf("invalid OVN remote endpoint address %q", address)
		}
		mac, err := net.ParseMAC(fields[0])
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 || fields[0] == "00:00:00:00:00:00" {
			return fmt.Errorf("invalid unicast remote MAC in %q", address)
		}
		if len(fields) == 2 {
			ip, err := netip.ParseAddr(fields[1])
			if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
				return fmt.Errorf("invalid remote IPv4 address in %q", address)
			}
		}
	}
	return nil
}

func model1ExternalIDs(key Model1TunnelKey) map[string]string {
	return map[string]string{
		model1ManagedByKey:        model1Owner,
		model1RoleKey:             model1TunnelRole,
		"evpn4ovn-network-id":     key.NetworkID,
		"evpn4ovn-vni":            strconv.FormatUint(uint64(key.VNI), 10),
		"evpn4ovn-remote-vtep":    key.RemoteVTEP,
		"evpn4ovn-logical-switch": key.LogicalSwitch,
	}
}

func model1TunnelExternalIDs(tunnel Model1Tunnel) map[string]string {
	ids := model1ExternalIDs(tunnel.Key)
	addresses := append([]string(nil), tunnel.Addresses...)
	sort.Strings(addresses)
	ids["evpn4ovn-endpoints-sha256"] = remoteHash(addresses)
	return ids
}

func model1ProviderBridgeMatchesTunnel(bridge vswitch.Bridge, tunnel Model1Tunnel) bool {
	ids := bridge.ExternalIDs
	return ids[model1ManagedByKey] == model1Owner &&
		ids[model1RoleKey] == model1BridgeRole &&
		ids[model1NetworkIDKey] == tunnel.Key.NetworkID &&
		ids[model1LogicalSwitchKey] == tunnel.Key.LogicalSwitch &&
		ids["evpn4ovn-vni"] == strconv.FormatUint(uint64(tunnel.Key.VNI), 10) &&
		ids[model1ProviderBridgeKey] == tunnel.Bridge
}

func model1IDsMatch(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func model1MapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func mergeModel1IDs(actual, expected map[string]string) map[string]string {
	result := make(map[string]string, len(actual)+len(expected))
	for key, value := range actual {
		result[key] = value
	}
	for key, value := range expected {
		result[key] = value
	}
	return result
}

func (p *LibovsdbModel1Programmer) ensureModel1OVSTunnel(ctx context.Context, tunnel Model1Tunnel) error {
	var bridges []vswitch.Bridge
	var interfaces []vswitch.Interface
	var ports []vswitch.Port
	if err := p.ovs.List(ctx, &bridges); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &interfaces); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &ports); err != nil {
		return err
	}
	var bridge *vswitch.Bridge
	for i := range bridges {
		if bridges[i].Name == tunnel.Bridge {
			if bridge != nil {
				return fmt.Errorf("OVS bridge name is not unique")
			}
			bridge = &bridges[i]
		}
	}
	if bridge == nil {
		return fmt.Errorf("OVS bridge %q not found", tunnel.Bridge)
	}
	if !model1ProviderBridgeMatchesTunnel(*bridge, tunnel) {
		return fmt.Errorf("OVS bridge %q is not the owned OVSDB-Programmed provider bridge for this network", bridge.Name)
	}
	expectedIDs := model1TunnelExternalIDs(tunnel)
	expectedIdentityIDs := model1ExternalIDs(tunnel.Key)
	var iface *vswitch.Interface
	for i := range interfaces {
		if interfaces[i].Name == tunnel.Name {
			if iface != nil {
				return fmt.Errorf("OVS interface name is not unique")
			}
			iface = &interfaces[i]
		}
	}
	options := model1VXLANOptions(tunnel)
	if iface == nil {
		iface = model1VXLANInterface(tunnel)
	} else {
		if !model1IDsMatch(iface.ExternalIDs, expectedIdentityIDs) {
			return fmt.Errorf("OVS interface %q is not owned by the OVSDB-Programmed integration", tunnel.Name)
		}
		iface.Type, iface.Options = "vxlan", options
		iface.ExternalIDs = mergeModel1IDs(iface.ExternalIDs, expectedIDs)
	}
	var port *vswitch.Port
	for i := range ports {
		if ports[i].Name == tunnel.Name {
			if port != nil {
				return fmt.Errorf("OVS port name is not unique")
			}
			port = &ports[i]
		}
	}
	for i := range ports {
		if !model1Contains(ports[i].Interfaces, iface.UUID) {
			continue
		}
		if port == nil || ports[i].UUID != port.UUID {
			return fmt.Errorf("OVS interface %q is referenced by another port %q", tunnel.Name, ports[i].Name)
		}
	}
	if port == nil {
		port = model1VXLANPort(tunnel, iface.UUID)
	} else {
		if !model1IDsMatch(port.ExternalIDs, expectedIdentityIDs) ||
			len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
			return fmt.Errorf("OVS port %q is not the owned OVSDB-Programmed tunnel port", tunnel.Name)
		}
		port.ExternalIDs = mergeModel1IDs(port.ExternalIDs, expectedIDs)
	}
	bridgeOwners := 0
	for i := range bridges {
		if !model1Contains(bridges[i].Ports, port.UUID) {
			continue
		}
		bridgeOwners++
		if bridges[i].Name != tunnel.Bridge {
			return fmt.Errorf("OVS port is attached to unexpected bridge %q", bridges[i].Name)
		}
	}
	if bridgeOwners > 1 {
		return fmt.Errorf("OVS port is attached to multiple bridges")
	}
	if bridgeOwners == 1 {
		var ops []ovsdb.Operation
		if exists := interfaceUUIDExists(interfaces, iface.UUID); exists {
			update, err := p.ovs.Where(&vswitch.Interface{UUID: iface.UUID}).Update(
				iface, &iface.Type, &iface.Options, &iface.ExternalIDs,
			)
			if err != nil {
				return err
			}
			ops = append(ops, update...)
		} else {
			create, err := p.ovs.Create(iface)
			if err != nil {
				return err
			}
			ops = append(ops, create...)
		}
		updatePort, err := p.ovs.Where(&vswitch.Port{UUID: port.UUID}).Update(
			port, &port.Interfaces, &port.ExternalIDs,
		)
		if err != nil {
			return err
		}
		ops = append(ops, updatePort...)
		return transactModel1(ctx, p.ovs, ops...)
	}
	var ops []ovsdb.Operation
	if interfaceUUIDExists(interfaces, iface.UUID) {
		update, err := p.ovs.Where(&vswitch.Interface{UUID: iface.UUID}).Update(
			iface, &iface.Type, &iface.Options, &iface.ExternalIDs,
		)
		if err != nil {
			return err
		}
		ops = append(ops, update...)
	} else {
		create, err := p.ovs.Create(iface)
		if err != nil {
			return err
		}
		ops = append(ops, create...)
	}
	if portExistsByUUID(ports, port.UUID) {
		update, err := p.ovs.Where(&vswitch.Port{UUID: port.UUID}).Update(
			port, &port.Interfaces, &port.ExternalIDs,
		)
		if err != nil {
			return err
		}
		ops = append(ops, update...)
	} else {
		create, err := p.ovs.Create(port)
		if err != nil {
			return err
		}
		ops = append(ops, create...)
	}
	mutate, err := p.ovs.Where(&vswitch.Bridge{UUID: bridge.UUID}).Mutate(bridge, model.Mutation{
		Field: &bridge.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{port.UUID},
	})
	if err != nil {
		return err
	}
	ops = append(ops, mutate...)
	return transactModel1(ctx, p.ovs, ops...)
}

func model1VXLANOptions(tunnel Model1Tunnel) map[string]string {
	return map[string]string{
		"remote_ip": tunnel.Key.RemoteVTEP,
		"key":       strconv.FormatUint(uint64(tunnel.Key.VNI), 10),
		"dst_port":  strconv.FormatUint(uint64(tunnel.UDPPort), 10),
	}
}

func model1VXLANInterface(tunnel Model1Tunnel) *vswitch.Interface {
	return &vswitch.Interface{
		UUID: uuid.NewString(), Name: tunnel.Name, Type: "vxlan",
		Options:     model1VXLANOptions(tunnel),
		ExternalIDs: model1TunnelExternalIDs(tunnel),
	}
}

func model1VXLANPort(tunnel Model1Tunnel, interfaceUUID string) *vswitch.Port {
	return &vswitch.Port{
		UUID: uuid.NewString(), Name: tunnel.Name,
		Interfaces: []string{interfaceUUID}, ExternalIDs: model1TunnelExternalIDs(tunnel),
	}
}

func (p *LibovsdbModel1Programmer) deleteModel1OVSTunnel(ctx context.Context, tunnel Model1Tunnel) error {
	var bridges []vswitch.Bridge
	var interfaces []vswitch.Interface
	var ports []vswitch.Port
	if err := p.ovs.List(ctx, &bridges); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &interfaces); err != nil {
		return err
	}
	if err := p.ovs.List(ctx, &ports); err != nil {
		return err
	}
	expectedIDs := model1TunnelExternalIDs(tunnel)
	var iface *vswitch.Interface
	for i := range interfaces {
		if interfaces[i].Name == tunnel.Name {
			if iface != nil {
				return fmt.Errorf("OVS interface name is not unique")
			}
			iface = &interfaces[i]
		}
	}
	var port *vswitch.Port
	for i := range ports {
		if ports[i].Name == tunnel.Name {
			if port != nil {
				return fmt.Errorf("OVS port name is not unique")
			}
			port = &ports[i]
		}
	}
	if iface == nil && port == nil {
		return nil
	}
	if iface == nil || !model1IDsMatch(iface.ExternalIDs, expectedIDs) {
		return fmt.Errorf("refusing to delete unowned or incomplete OVS tunnel interface")
	}
	if iface.Type != "vxlan" || !model1MapsEqual(iface.Options, model1VXLANOptions(tunnel)) {
		return fmt.Errorf("refusing to delete OVSDB-Programmed interface with changed VXLAN configuration")
	}
	if port != nil && (!model1IDsMatch(port.ExternalIDs, expectedIDs) ||
		len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID) {
		return fmt.Errorf("refusing to delete unowned or mismatched OVS tunnel port")
	}
	if port == nil {
		for i := range ports {
			if model1Contains(ports[i].Interfaces, iface.UUID) {
				return fmt.Errorf("refusing to delete OVS interface still referenced by port %q", ports[i].Name)
			}
		}
	}
	var ops []ovsdb.Operation
	if port != nil {
		bridgeOwners := 0
		for i := range bridges {
			if !model1Contains(bridges[i].Ports, port.UUID) {
				continue
			}
			if bridges[i].Name != tunnel.Bridge {
				return fmt.Errorf("refusing to detach OVS port from unexpected bridge %q", bridges[i].Name)
			}
			if !model1ProviderBridgeMatchesTunnel(bridges[i], tunnel) {
				return fmt.Errorf("refusing to detach OVSDB-Programmed tunnel from an unowned provider bridge %q", bridges[i].Name)
			}
			bridgeOwners++
			mutate, err := p.ovs.Where(&vswitch.Bridge{UUID: bridges[i].UUID}).Mutate(&bridges[i], model.Mutation{
				Field: &bridges[i].Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{port.UUID},
			})
			if err != nil {
				return err
			}
			ops = append(ops, mutate...)
		}
		if bridgeOwners > 1 {
			return fmt.Errorf("refusing to delete OVS port attached to multiple bridges")
		}
		deletePort, err := p.ovs.Where(&vswitch.Port{UUID: port.UUID}).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, deletePort...)
	} else {
		deleteInterface, err := p.ovs.Where(&vswitch.Interface{
			UUID: iface.UUID, ExternalIDs: expectedIDs,
		}).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, deleteInterface...)
	}
	return transactModel1(ctx, p.ovs, ops...)
}

func interfaceUUIDExists(interfaces []vswitch.Interface, uuid string) bool {
	for i := range interfaces {
		if interfaces[i].UUID == uuid {
			return true
		}
	}
	return false
}

func portExistsByUUID(ports []vswitch.Port, uuid string) bool {
	for i := range ports {
		if ports[i].UUID == uuid {
			return true
		}
	}
	return false
}

func model1Contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func sameModel1Addresses(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func transactModel1(ctx context.Context, c client.Client, ops ...ovsdb.Operation) error {
	if len(ops) == 0 {
		return nil
	}
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	for index, result := range results {
		if result.Error != "" {
			return fmt.Errorf("OVSDB operation %d failed: %s %s", index, result.Error, result.Details)
		}
		if ops[index].Op == ovsdb.OperationUpdate && result.Count == 0 {
			return fmt.Errorf("OVSDB update operation %d matched no rows", index)
		}
	}
	return nil
}
