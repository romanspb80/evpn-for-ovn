package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
)

func cniWaitUUID(table, id string, conditions ...ovsdb.Condition) ovsdb.Operation {
	timeout := 0
	where := []ovsdb.Condition{{Column: "_uuid", Function: ovsdb.ConditionEqual, Value: ovsdb.UUID{GoUUID: id}}}
	where = append(where, conditions...)
	return ovsdb.Operation{Op: ovsdb.OperationWait, Table: table, Where: where,
		Columns: []string{"_uuid"}, Rows: []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: id}}},
		Until: "==", Timeout: &timeout}
}

func cniExactTransaction(ctx context.Context, c client.Client, ops []ovsdb.Operation) error {
	if c == nil || !c.Connected() {
		return errors.New("CNI exact cleanup backend disconnected")
	}
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	if len(results) != len(ops) {
		return errors.New("incomplete CNI exact cleanup transaction")
	}
	for i, result := range results {
		if result.Error != "" {
			return fmt.Errorf("CNI exact cleanup failed: %s %s", result.Error, result.Details)
		}
		if ops[i].Op == ovsdb.OperationDelete && result.Count != 1 {
			return errors.New("CNI exact cleanup did not delete one acknowledged UUID")
		}
	}
	return nil
}

// No name-based DELETE is permitted in durable cleanup. Transactional waits
// fence the acknowledged UUID and ownership metadata before removing references.
func (o *nativeCNIObserver) DeleteExact(ctx context.Context, ack cniEndpointACK) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	live, ok := o.source.(cniLiveState)
	if !ok {
		return errors.New("CNI exact cleanup requires native live backend")
	}
	receipt, absent, err := o.Observe(ctx, ack.record())
	if err != nil {
		return err
	}
	if absent || receipt != ack.Receipt {
		return errors.New("CNI exact cleanup resource identity changed")
	}
	want := attachment.ExternalIDsForIdentity(attachment.Identity{
		EndpointID: string(ack.Spec.ID), NetworkID: ack.Spec.NetworkID, AttachmentID: ack.Spec.AttachmentID,
		WorkloadUID: ack.Spec.Workload.UID, Incarnation: ack.Spec.Incarnation,
	}, ack.Spec.LogicalSwitchPort, "")
	ids, err := ovsdb.NewOvsMap(want)
	if err != nil {
		return err
	}
	owner := ovsdb.Condition{Column: "external_ids", Function: ovsdb.ConditionIncludes, Value: ids}
	data, err := o.source.Snapshot(ctx)
	if err != nil {
		return err
	}
	var bridgeUUID string
	for _, bridge := range data.Bridges {
		if bridge.Name == "br-int" {
			for _, id := range bridge.Ports {
				if id == receipt.PortUUID {
					bridgeUUID = bridge.UUID
				}
			}
		}
	}
	if bridgeUUID == "" {
		return errors.New("CNI exact cleanup bridge UUID missing")
	}
	one := func(id string) ovsdb.OvsSet { return ovsdb.OvsSet{GoSet: []interface{}{ovsdb.UUID{GoUUID: id}}} }
	where := func(id string) []ovsdb.Condition {
		return []ovsdb.Condition{{Column: "_uuid", Function: ovsdb.ConditionEqual, Value: ovsdb.UUID{GoUUID: id}}}
	}
	ovsOps := []ovsdb.Operation{
		cniWaitUUID("Interface", receipt.InterfaceUUID, owner),
		cniWaitUUID("Port", receipt.PortUUID, ovsdb.Condition{Column: "interfaces", Function: ovsdb.ConditionEqual, Value: one(receipt.InterfaceUUID)}),
		cniWaitUUID("Bridge", bridgeUUID, ovsdb.Condition{Column: "ports", Function: ovsdb.ConditionIncludes, Value: one(receipt.PortUUID)}),
		{Op: ovsdb.OperationMutate, Table: "Bridge", Where: where(bridgeUUID), Mutations: []ovsdb.Mutation{{Column: "ports", Mutator: ovsdb.MutateOperationDelete, Value: one(receipt.PortUUID)}}},
		{Op: ovsdb.OperationDelete, Table: "Port", Where: where(receipt.PortUUID)},
		{Op: ovsdb.OperationDelete, Table: "Interface", Where: where(receipt.InterfaceUUID)},
	}
	if err := cniExactTransaction(ctx, live.ovs, ovsOps); err != nil {
		return err
	}
	nbOps := []ovsdb.Operation{
		cniWaitUUID("Logical_Switch_Port", receipt.LSPUUID, owner),
		cniWaitUUID("Logical_Switch", receipt.SwitchUUID, ovsdb.Condition{Column: "ports", Function: ovsdb.ConditionIncludes, Value: one(receipt.LSPUUID)}),
		{Op: ovsdb.OperationMutate, Table: "Logical_Switch", Where: where(receipt.SwitchUUID), Mutations: []ovsdb.Mutation{{Column: "ports", Mutator: ovsdb.MutateOperationDelete, Value: one(receipt.LSPUUID)}}},
		{Op: ovsdb.OperationDelete, Table: "Logical_Switch_Port", Where: where(receipt.LSPUUID)},
	}
	return cniExactTransaction(ctx, live.nb, nbOps)
}
