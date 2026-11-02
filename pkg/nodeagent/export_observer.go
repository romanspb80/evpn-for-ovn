package nodeagent

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type exportPortBinding struct {
	UUID        string            `ovsdb:"_uuid"`
	LogicalPort string            `ovsdb:"logical_port"`
	Chassis     *string           `ovsdb:"chassis"`
	Up          *bool             `ovsdb:"up"`
	Datapath    string            `ovsdb:"datapath"`
	Type        string            `ovsdb:"type"`
	MAC         []string          `ovsdb:"mac"`
	Options     map[string]string `ovsdb:"options"`
}

type exportDatapath struct {
	UUID        string            `ovsdb:"_uuid"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type exportChassis struct {
	UUID string `ovsdb:"_uuid"`
	Name string `ovsdb:"name"`
}

type exportSnapshot struct {
	Switches   []*nbdb.LogicalSwitch
	LSPs       []*nbdb.LogicalSwitchPort
	Bridges    []*vswitch.Bridge
	Ports      []*vswitch.Port
	Interfaces []*vswitch.Interface
	Bindings   []*exportPortBinding
	Chassis    []*exportChassis
	Datapaths  []*exportDatapath
}

type exportStateSource interface {
	Snapshot(context.Context) (exportSnapshot, error)
}

type liveExportState struct{ nb, ovs, sb client.Client }

func selectExportRows(ctx context.Context, c client.Client, table string, columns []string, target interface{}) error {
	if c == nil || !c.Connected() {
		return fmt.Errorf("%s backend disconnected", table)
	}
	ops := []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: table, Where: []ovsdb.Condition{}, Columns: columns}}
	ovsdb.SetCorrelationID(&ops[0], "endpoint-export-read")
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	if len(results) != 1 || results[0].Error != "" {
		return fmt.Errorf("incomplete/failed %s read: %v", table, results)
	}
	if err := c.GetSelectResults(ops, results, target); err != nil {
		return fmt.Errorf("decode %s: %w", table, err)
	}
	return nil
}

func (s liveExportState) Snapshot(ctx context.Context) (exportSnapshot, error) {
	var out exportSnapshot
	reads := []struct {
		c       client.Client
		table   string
		columns []string
		target  interface{}
	}{
		{s.nb, "Logical_Switch", []string{"_uuid", "name", "ports", "other_config"}, &out.Switches},
		{s.nb, "Logical_Switch_Port", []string{"_uuid", "name", "addresses", "external_ids"}, &out.LSPs},
		{s.ovs, "Bridge", []string{"_uuid", "name", "ports"}, &out.Bridges},
		{s.ovs, "Port", []string{"_uuid", "name", "interfaces"}, &out.Ports},
		{s.ovs, "Interface", []string{"_uuid", "name", "external_ids", "ofport", "error"}, &out.Interfaces},
		{s.sb, "Port_Binding", []string{"_uuid", "logical_port", "chassis", "up", "datapath", "type", "mac", "options"}, &out.Bindings},
		{s.sb, "Chassis", []string{"_uuid", "name"}, &out.Chassis},
		{s.sb, "Datapath_Binding", []string{"_uuid", "external_ids"}, &out.Datapaths},
	}
	for _, read := range reads {
		if err := selectExportRows(ctx, read.c, read.table, read.columns, read.target); err != nil {
			return exportSnapshot{}, err
		}
	}
	return out, nil
}

type liveOVNExportState struct{ nb, sb client.Client }

func (s liveOVNExportState) ListLogicalSwitchPorts(ctx context.Context) ([]evpn.OVNLogicalSwitchPort, error) {
	var ports []*nbdb.LogicalSwitchPort
	if err := selectExportRows(ctx, s.nb, "Logical_Switch_Port",
		[]string{"_uuid", "name", "addresses", "external_ids"}, &ports); err != nil {
		return nil, err
	}
	out := make([]evpn.OVNLogicalSwitchPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, evpn.OVNLogicalSwitchPort{
			Name: port.Name, Addresses: append([]string(nil), port.Addresses...),
			ExternalIDs: copyExportIDs(port.ExternalIDs),
		})
	}
	return out, nil
}

func (s liveOVNExportState) ListPortBindings(ctx context.Context) ([]evpn.OVNPortBinding, error) {
	var bindings []*exportPortBinding
	if err := selectExportRows(ctx, s.sb, "Port_Binding",
		[]string{"_uuid", "logical_port", "chassis"}, &bindings); err != nil {
		return nil, err
	}
	var chassis []*exportChassis
	if err := selectExportRows(ctx, s.sb, "Chassis", []string{"_uuid", "name"}, &chassis); err != nil {
		return nil, err
	}
	chassisNames := make(map[string]string, len(chassis))
	for _, row := range chassis {
		chassisNames[row.UUID] = row.Name
	}
	out := make([]evpn.OVNPortBinding, 0, len(bindings))
	for _, binding := range bindings {
		name := ""
		if binding.Chassis != nil {
			var ok bool
			name, ok = chassisNames[*binding.Chassis]
			if !ok {
				return nil, fmt.Errorf("Port_Binding %q references unknown chassis %q", binding.LogicalPort, *binding.Chassis)
			}
		}
		out = append(out, evpn.OVNPortBinding{LogicalPort: binding.LogicalPort, Chassis: name})
	}
	return out, nil
}

func copyExportIDs(ids map[string]string) map[string]string {
	copied := make(map[string]string, len(ids))
	for key, value := range ids {
		copied[key] = value
	}
	return copied
}

func newExportSBClient(ctx context.Context, address string) (client.Client, error) {
	m, err := model.NewClientDBModel("OVN_Southbound", map[string]model.Model{
		"Port_Binding": &exportPortBinding{}, "Chassis": &exportChassis{},
		"Datapath_Binding": &exportDatapath{},
	})
	if err != nil {
		return nil, err
	}
	c, err := client.NewOVSDBClient(m, client.WithEndpoint(address),
		client.WithReconnect(2*time.Second, backoff.NewConstantBackOff(100*time.Millisecond)))
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type ExportObservation struct {
	EndpointID  string
	Incarnation string
	State       string
	Reason      string
}

// CNIEndpointObserver starts from managed attachment records, not arbitrary
// NB rows. Backend read failures abort the whole desired snapshot.
type CNIEndpointObserver struct {
	server       *CNIServer
	source       exportStateSource
	config       evpn.ExporterConfig
	Observations []ExportObservation
}

func (o *CNIEndpointObserver) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	o.Observations = nil
	data, err := o.source.Snapshot(ctx)
	if err != nil {
		o.Observations = []ExportObservation{{State: "observation-error", Reason: err.Error()}}
		return nil, err
	}
	o.server.store.mu.RLock()
	records := make([]*endpointRecord, 0, len(o.server.store.records))
	for _, rec := range o.server.store.records {
		records = append(records, cloneRecord(rec))
	}
	o.server.store.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return endpointKey(records[i].identity) < endpointKey(records[j].identity) })
	configs := map[string]evpn.EVPNNetworkConfig{}
	for _, cfg := range o.config.Networks {
		configs[cfg.NetworkID] = cfg
	}
	var desired []evpn.LocalEndpoint
	if len(records) == 0 {
		o.Observations = append(o.Observations, ExportObservation{State: "absent", Reason: "no managed CNI attachment records"})
	}
	for _, rec := range records {
		spec := rec.endpointSpec
		obs := ExportObservation{EndpointID: string(spec.ID), Incarnation: spec.Incarnation, State: "unready"}
		cfg, ok := configs[spec.NetworkID]
		reason := "network not allowlisted"
		if ok && cfg.Enabled && cfg.ExportSource() == "cni" {
			reason = exportDataplaneReason(spec, rec, cfg, o.config.LocalChassis, data)
		} else if ok && cfg.ExportSource() != "cni" {
			reason = "network authority is " + cfg.ExportSource()
		}
		if reason == "" {
			if _, err := o.server.network.Check(ctx, endpointKey(rec.identity), rec); err != nil {
				obs.State, obs.Reason = "observation-error", "Linux attachment verification: "+err.Error()
				o.Observations = append(o.Observations, obs)
				return nil, fmt.Errorf("%s: %s", spec.ID, obs.Reason)
			}
			ip, err := netip.ParsePrefix(spec.IPAddresses[0])
			if err != nil {
				return nil, fmt.Errorf("invalid managed endpoint address: %w", err)
			}
			ep := evpn.LocalEndpoint{
				NetworkID: spec.NetworkID, EndpointID: string(spec.ID), Incarnation: spec.Incarnation,
				MACAddress: spec.MACAddress, IPAddresses: []string{ip.Addr().String()},
				VNI: cfg.VNI, RouteDistinguisher: cfg.RouteDistinguisher, RouteTargets: append([]string(nil), cfg.RouteTargets...),
				LocalVTEP: cfg.LocalVTEP, NodeName: o.config.NodeName, Chassis: o.config.LocalChassis, Advertise: true,
			}
			desired = append(desired, ep)
			obs.State, obs.Reason = "ready", "NB/OVS/Linux/SB ownership and readiness verified"
		} else {
			obs.Reason = reason
		}
		o.Observations = append(o.Observations, obs)
	}
	return desired, nil
}

func exportDataplaneReason(spec endpoint.EndpointSpec, rec *endpointRecord, cfg evpn.EVPNNetworkConfig, chassis string, data exportSnapshot) string {
	id := rec.identity
	if spec.Workload.Kind != "pod" || spec.Workload.UID == "" || string(spec.ID) != spec.NetworkID+"|"+spec.AttachmentID+"|"+spec.Workload.UID ||
		id.GetEndpointId() != string(spec.ID) || id.GetNetworkId() != spec.NetworkID || id.GetAttachmentId() != spec.AttachmentID ||
		id.GetWorkloadUid() != spec.Workload.UID || id.GetIncarnation() != spec.Incarnation || spec.Incarnation == "" ||
		rec.runtime.GetContainerId() != spec.Incarnation {
		return "CNI identity/incarnation mismatch"
	}
	if spec.LogicalSwitch != cfg.LogicalSwitch {
		return "logical switch differs from allowlist"
	}
	if len(spec.IPAddresses) != 1 {
		return "exactly one managed IPv4 prefix required"
	}
	ip, err := netip.ParsePrefix(spec.IPAddresses[0])
	if err != nil || !ip.Addr().Is4() {
		return "managed endpoint is not a single IPv4 prefix"
	}
	want := attachment.ExternalIDsForIdentity(attachment.Identity{
		EndpointID: string(spec.ID), NetworkID: spec.NetworkID, AttachmentID: spec.AttachmentID, WorkloadUID: spec.Workload.UID, Incarnation: spec.Incarnation,
	}, spec.LogicalSwitchPort, "")
	owned := func(ids map[string]string) bool {
		for key, value := range want {
			if ids[key] != value {
				return false
			}
		}
		return true
	}
	var lsp *nbdb.LogicalSwitchPort
	for i := range data.LSPs {
		if data.LSPs[i].Name == spec.LogicalSwitchPort {
			if lsp != nil {
				return "duplicate LSP"
			}
			lsp = data.LSPs[i]
		}
	}
	if lsp == nil {
		return "NB LSP absent"
	}
	if !owned(lsp.ExternalIDs) {
		return "NB LSP ownership mismatch"
	}
	memberships := 0
	for _, sw := range data.Switches {
		for _, port := range sw.Ports {
			if port == lsp.UUID {
				if sw.Name != cfg.LogicalSwitch {
					return "LSP in unexpected switch"
				}
				memberships++
			}
		}
	}
	if memberships != 1 {
		return "LSP switch membership not unique"
	}
	if len(lsp.Addresses) != 1 {
		return "LSP address set differs from CNI attachment"
	}
	fields := strings.Fields(lsp.Addresses[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], spec.MACAddress) || (fields[1] != spec.IPAddresses[0] && fields[1] != ip.Addr().String()) {
		return "LSP MAC/IP differs from CNI attachment"
	}
	var iface *vswitch.Interface
	for i := range data.Interfaces {
		if data.Interfaces[i].Name == spec.OVSInterfaceName {
			if iface != nil {
				return "duplicate OVS Interface"
			}
			iface = data.Interfaces[i]
		}
	}
	if iface == nil || !owned(iface.ExternalIDs) {
		return "OVS Interface absent or wrong owner/iface-id"
	}
	if iface.Ofport == nil || *iface.Ofport <= 0 || (iface.Error != nil && *iface.Error != "") {
		return "OVS Interface ofport/error unready"
	}
	var port *vswitch.Port
	for i := range data.Ports {
		if data.Ports[i].Name == spec.OVSInterfaceName {
			if port != nil {
				return "duplicate OVS Port"
			}
			port = data.Ports[i]
		}
	}
	if port == nil || len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
		return "OVS Port/Interface relationship unready"
	}
	bridges := 0
	for _, bridge := range data.Bridges {
		for _, id := range bridge.Ports {
			if id == port.UUID {
				if bridge.Name != "br-int" {
					return "OVS Port attached outside br-int"
				}
				bridges++
			}
		}
	}
	if bridges != 1 {
		return "OVS Port not uniquely attached to br-int"
	}
	var binding *exportPortBinding
	for i := range data.Bindings {
		if data.Bindings[i].LogicalPort == lsp.Name {
			if binding != nil {
				return "duplicate Port_Binding"
			}
			binding = data.Bindings[i]
		}
	}
	if binding == nil || binding.Chassis == nil || binding.Up == nil || !*binding.Up {
		return "SB Port_Binding absent/down/unbound"
	}
	for _, c := range data.Chassis {
		if c.UUID == *binding.Chassis && c.Name == chassis {
			return ""
		}
	}
	return "SB Port_Binding is not on configured local chassis"
}
