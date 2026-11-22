package nodeagent

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

type cniOrphanResources struct {
	lsp          *nbdb.LogicalSwitchPort
	switchRow    *nbdb.LogicalSwitch
	switchMember bool
	iface        *vswitch.Interface
	port         *vswitch.Port
	bridge       *vswitch.Bridge
	bridgeMember bool
}

func inspectCNIOrphanResources(ctx context.Context, source exportStateSource, networks map[string]string, ack cniEndpointACK) (cniOrphanResources, error) {
	var resources cniOrphanResources
	key := endpointKey(ack.Identity)
	if ack.State != "active" || key == "" || validateCNIACK(key, ack) != nil || ack.Receipt.HostIfIndex <= 0 {
		return resources, errors.New("orphan cleanup requires a complete active receipt")
	}
	if networks[ack.Spec.NetworkID] != ack.Spec.LogicalSwitch {
		return resources, errors.New("orphan cleanup network mapping mismatch")
	}
	data, err := source.Snapshot(ctx)
	if err != nil {
		return resources, err
	}
	findLSP := func() (*nbdb.LogicalSwitchPort, error) {
		var found *nbdb.LogicalSwitchPort
		for _, row := range data.LSPs {
			if row == nil {
				return nil, errors.New("nil LSP in orphan inventory")
			}
			if row.UUID == ack.Receipt.LSPUUID || row.Name == ack.Spec.LogicalSwitchPort {
				if found != nil {
					return nil, errors.New("duplicate LSP identity in orphan inventory")
				}
				if row.UUID != ack.Receipt.LSPUUID || row.Name != ack.Spec.LogicalSwitchPort {
					return nil, errors.New("LSP UUID or name changed since CNI receipt")
				}
				found = row
			}
			if row.ExternalIDs[attachment.ExternalIDEndpointID] == string(ack.Spec.ID) && row.Name != ack.Spec.LogicalSwitchPort {
				return nil, errors.New("CNI endpoint identity moved to another LSP name")
			}
		}
		return found, nil
	}
	resources.lsp, err = findLSP()
	if err != nil {
		return resources, err
	}
	wantIDs := attachment.ExternalIDsForIdentity(attachment.Identity{
		EndpointID: string(ack.Spec.ID), NetworkID: ack.Spec.NetworkID, AttachmentID: ack.Spec.AttachmentID,
		WorkloadUID: ack.Spec.Workload.UID, Incarnation: ack.Spec.Incarnation,
	}, ack.Spec.LogicalSwitchPort, "")
	if resources.lsp != nil {
		if !hasExactIDs(resources.lsp.ExternalIDs, wantIDs) {
			return resources, errors.New("LSP ownership metadata differs from durable CNI receipt")
		}
		if err := validateOrphanLSPAddress(resources.lsp, ack.Spec); err != nil {
			return resources, err
		}
	}
	for _, row := range data.Switches {
		if row == nil {
			return resources, errors.New("nil Logical_Switch in orphan inventory")
		}
		if row.UUID == ack.Receipt.SwitchUUID || row.Name == ack.Spec.LogicalSwitch {
			if row.UUID != ack.Receipt.SwitchUUID || row.Name != ack.Spec.LogicalSwitch || resources.switchRow != nil {
				return resources, errors.New("Logical_Switch identity changed since CNI receipt")
			}
			resources.switchRow = row
		}
		if containsCNIUUID(row.Ports, ack.Receipt.LSPUUID) {
			if row.UUID != ack.Receipt.SwitchUUID {
				return resources, errors.New("LSP is now attached to a different Logical_Switch")
			}
			resources.switchMember = true
		}
	}
	for _, row := range data.Interfaces {
		if row == nil {
			return resources, errors.New("nil OVS Interface in orphan inventory")
		}
		if row.UUID == ack.Receipt.InterfaceUUID || row.Name == ack.Spec.OVSInterfaceName {
			if row.UUID != ack.Receipt.InterfaceUUID || row.Name != ack.Spec.OVSInterfaceName || resources.iface != nil {
				return resources, errors.New("OVS Interface UUID or name changed since CNI receipt")
			}
			if !hasExactIDs(row.ExternalIDs, wantIDs) {
				return resources, errors.New("OVS Interface ownership differs from durable CNI receipt")
			}
			resources.iface = row
		}
		if row.ExternalIDs[attachment.ExternalIDEndpointID] == string(ack.Spec.ID) && row.Name != ack.Spec.OVSInterfaceName {
			return resources, errors.New("CNI endpoint identity moved to another OVS Interface")
		}
	}
	for _, row := range data.Ports {
		if row == nil {
			return resources, errors.New("nil OVS Port in orphan inventory")
		}
		if row.UUID == ack.Receipt.PortUUID || row.Name == ack.Spec.OVSInterfaceName {
			if row.UUID != ack.Receipt.PortUUID || row.Name != ack.Spec.OVSInterfaceName || resources.port != nil {
				return resources, errors.New("OVS Port UUID or name changed since CNI receipt")
			}
			for _, ifaceID := range row.Interfaces {
				if ifaceID != ack.Receipt.InterfaceUUID {
					return resources, errors.New("OVS Port now references a different Interface")
				}
			}
			resources.port = row
		}
	}
	for _, row := range data.Bridges {
		if row == nil {
			return resources, errors.New("nil OVS Bridge in orphan inventory")
		}
		if row.UUID == ack.Receipt.BridgeUUID {
			if row.Name != "br-int" || resources.bridge != nil {
				return resources, errors.New("OVS Bridge identity changed since CNI receipt")
			}
			resources.bridge = row
		}
		if containsCNIUUID(row.Ports, ack.Receipt.PortUUID) {
			if row.UUID != ack.Receipt.BridgeUUID {
				return resources, errors.New("OVS Port is now attached to a different Bridge")
			}
			resources.bridgeMember = true
		}
	}
	if resources.port != nil && resources.iface != nil &&
		(len(resources.port.Interfaces) != 1 || resources.port.Interfaces[0] != resources.iface.UUID) {
		return resources, errors.New("OVS Port/Interface membership changed")
	}
	if resources.port != nil && resources.iface == nil && len(resources.port.Interfaces) > 1 {
		return resources, errors.New("orphan OVS Port has ambiguous Interface membership")
	}
	return resources, nil
}

func containsCNIUUID(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasExactIDs(got, want map[string]string) bool {
	if len(got) == 0 || len(want) == 0 {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func validateOrphanLSPAddress(lsp *nbdb.LogicalSwitchPort, spec endpoint.EndpointSpec) error {
	if lsp == nil || len(spec.IPAddresses) != 1 || len(lsp.Addresses) != 1 {
		return errors.New("orphan LSP static address evidence is incomplete")
	}
	ip, _, err := net.ParseCIDR(spec.IPAddresses[0])
	fields := strings.Fields(lsp.Addresses[0])
	if err != nil || len(fields) != 2 || !strings.EqualFold(fields[0], spec.MACAddress) ||
		(fields[1] != spec.IPAddresses[0] && fields[1] != ip.String()) {
		return errors.New("orphan LSP static address differs from durable CNI receipt")
	}
	return nil
}

func deleteCNIOrphanOVS(ctx context.Context, c client.Client, ack cniEndpointACK, resources cniOrphanResources) error {
	var ops []ovsdb.Operation
	one := func(id string) ovsdb.OvsSet { return ovsdb.OvsSet{GoSet: []interface{}{ovsdb.UUID{GoUUID: id}}} }
	where := func(id string) []ovsdb.Condition {
		return []ovsdb.Condition{{Column: "_uuid", Function: ovsdb.ConditionEqual, Value: ovsdb.UUID{GoUUID: id}}}
	}
	if resources.bridgeMember {
		ops = append(ops, cniWaitUUID("Bridge", ack.Receipt.BridgeUUID,
			ovsdb.Condition{Column: "ports", Function: ovsdb.ConditionIncludes, Value: one(ack.Receipt.PortUUID)}))
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationMutate, Table: "Bridge", Where: where(ack.Receipt.BridgeUUID),
			Mutations: []ovsdb.Mutation{{Column: "ports", Mutator: ovsdb.MutateOperationDelete, Value: one(ack.Receipt.PortUUID)}}})
	}
	if resources.port != nil {
		interfaces := make([]interface{}, 0, len(resources.port.Interfaces))
		for _, id := range resources.port.Interfaces {
			interfaces = append(interfaces, ovsdb.UUID{GoUUID: id})
		}
		ops = append(ops, cniWaitUUID("Port", ack.Receipt.PortUUID,
			ovsdb.Condition{Column: "interfaces", Function: ovsdb.ConditionEqual, Value: ovsdb.OvsSet{GoSet: interfaces}}))
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: "Port", Where: where(ack.Receipt.PortUUID)})
	}
	if resources.iface != nil {
		owner, err := ovsdb.NewOvsMap(attachment.ExternalIDsForIdentity(attachment.Identity{
			EndpointID: string(ack.Spec.ID), NetworkID: ack.Spec.NetworkID, AttachmentID: ack.Spec.AttachmentID,
			WorkloadUID: ack.Spec.Workload.UID, Incarnation: ack.Spec.Incarnation,
		}, ack.Spec.LogicalSwitchPort, ""))
		if err != nil {
			return err
		}
		ops = append(ops, cniWaitUUID("Interface", ack.Receipt.InterfaceUUID,
			ovsdb.Condition{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: owner}))
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: "Interface", Where: where(ack.Receipt.InterfaceUUID)})
	}
	if len(ops) == 0 {
		return nil
	}
	return cniExactTransaction(ctx, c, ops)
}

func deleteCNIOrphanOVN(ctx context.Context, c client.Client, ack cniEndpointACK, resources cniOrphanResources) error {
	var ops []ovsdb.Operation
	one := func(id string) ovsdb.OvsSet { return ovsdb.OvsSet{GoSet: []interface{}{ovsdb.UUID{GoUUID: id}}} }
	where := func(id string) []ovsdb.Condition {
		return []ovsdb.Condition{{Column: "_uuid", Function: ovsdb.ConditionEqual, Value: ovsdb.UUID{GoUUID: id}}}
	}
	if resources.switchMember {
		ops = append(ops, cniWaitUUID("Logical_Switch", ack.Receipt.SwitchUUID,
			ovsdb.Condition{Column: "ports", Function: ovsdb.ConditionIncludes, Value: one(ack.Receipt.LSPUUID)}))
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationMutate, Table: "Logical_Switch", Where: where(ack.Receipt.SwitchUUID),
			Mutations: []ovsdb.Mutation{{Column: "ports", Mutator: ovsdb.MutateOperationDelete, Value: one(ack.Receipt.LSPUUID)}}})
	}
	if resources.lsp != nil {
		owner, err := ovsdb.NewOvsMap(attachment.ExternalIDsForIdentity(attachment.Identity{
			EndpointID: string(ack.Spec.ID), NetworkID: ack.Spec.NetworkID, AttachmentID: ack.Spec.AttachmentID,
			WorkloadUID: ack.Spec.Workload.UID, Incarnation: ack.Spec.Incarnation,
		}, ack.Spec.LogicalSwitchPort, ""))
		if err != nil {
			return err
		}
		ops = append(ops, cniWaitUUID("Logical_Switch_Port", ack.Receipt.LSPUUID,
			ovsdb.Condition{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: owner}))
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationDelete, Table: "Logical_Switch_Port", Where: where(ack.Receipt.LSPUUID)})
	}
	if len(ops) == 0 {
		return nil
	}
	return cniExactTransaction(ctx, c, ops)
}
