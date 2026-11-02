package endpoint

import (
	"context"
	"fmt"
	"strings"

	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

const (
	ovnExternalIDManagedBy   = "evpn.framework/managed-by"
	ovnExternalIDEndpointID  = "evpn.framework/endpoint-id"
	ovnExternalIDNetworkID   = "evpn.framework/network-id"
	ovnExternalIDAttachID    = "evpn.framework/attachment-id"
	ovnExternalIDWorkloadUID = "evpn.framework/workload-uid"
	ovnExternalIDIncarnation = "evpn.framework/incarnation"
	ovnExternalIDIfaceID     = "iface-id"
	ovnOwnerValue            = "evpn-node-agent"
)

type LibovsdbOVNBackend struct {
	client client.Client
}

func NewLibovsdbOVNBackend(c client.Client) *LibovsdbOVNBackend {
	return &LibovsdbOVNBackend{client: c}
}

func (b *LibovsdbOVNBackend) EnsureLogicalSwitchPort(ctx context.Context, spec OVNPortSpec) (OVNPortStatus, error) {
	if b == nil || b.client == nil {
		return OVNPortStatus{}, fmt.Errorf("ovn backend client is not configured")
	}
	lsName := strings.TrimSpace(spec.LogicalSwitch)
	if lsName == "" {
		return OVNPortStatus{}, fmt.Errorf("logical switch is required")
	}
	lspName := strings.TrimSpace(spec.LogicalSwitchPort)
	if lspName == "" {
		return OVNPortStatus{}, fmt.Errorf("logical switch port is required")
	}

	ls, err := b.getLogicalSwitchByName(ctx, lsName)
	if err != nil {
		return OVNPortStatus{}, err
	}
	desiredExternalIDs := ovnExternalIDsForSpec(spec)
	existing, err := b.getLogicalSwitchPortByName(ctx, lspName)
	if err != nil {
		return OVNPortStatus{}, err
	}
	if existing != nil {
		if err := validateOVNOwnership(existing.ExternalIDs, spec); err != nil {
			return OVNPortStatus{}, err
		}
		update := &nbdb.LogicalSwitchPort{
			UUID:        existing.UUID,
			Addresses:   append([]string(nil), spec.Addresses...),
			ExternalIDs: desiredExternalIDs,
		}
		updateOps, err := b.client.Where(&nbdb.LogicalSwitchPort{UUID: existing.UUID}).Update(update)
		if err != nil {
			return OVNPortStatus{}, err
		}
		if err := transactNoErr(ctx, b.client, updateOps...); err != nil {
			return OVNPortStatus{}, err
		}
		if !containsString(ls.Ports, existing.UUID) {
			mutateOps, err := b.client.Where(ls).Mutate(ls, model.Mutation{
				Field:   &ls.Ports,
				Mutator: ovsdb.MutateOperationInsert,
				Value:   []string{existing.UUID},
			})
			if err != nil {
				return OVNPortStatus{}, err
			}
			if err := transactNoErr(ctx, b.client, mutateOps...); err != nil {
				return OVNPortStatus{}, err
			}
		}
		return OVNPortStatus{LogicalSwitch: lsName, LogicalSwitchPort: lspName}, nil
	}

	lspUUID := uuid.New().String()
	lsp := &nbdb.LogicalSwitchPort{
		UUID:        lspUUID,
		Name:        lspName,
		Addresses:   append([]string(nil), spec.Addresses...),
		ExternalIDs: desiredExternalIDs,
	}
	createOps, err := b.client.Create(lsp)
	if err != nil {
		return OVNPortStatus{}, err
	}
	mutateOps, err := b.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{lspUUID},
	})
	if err != nil {
		return OVNPortStatus{}, err
	}
	ops := append(createOps, mutateOps...)
	if err := transactNoErr(ctx, b.client, ops...); err != nil {
		return OVNPortStatus{}, err
	}
	return OVNPortStatus{LogicalSwitch: lsName, LogicalSwitchPort: lspName}, nil
}

func (b *LibovsdbOVNBackend) GetLogicalSwitchPort(ctx context.Context, logicalSwitchPort string) (OVNPortStatus, error) {
	if b == nil || b.client == nil {
		return OVNPortStatus{}, fmt.Errorf("ovn backend client is not configured")
	}
	lspName := strings.TrimSpace(logicalSwitchPort)
	if lspName == "" {
		return OVNPortStatus{}, fmt.Errorf("logical switch port is required")
	}
	lsp, err := b.getLogicalSwitchPortByName(ctx, lspName)
	if err != nil {
		return OVNPortStatus{}, err
	}
	if lsp == nil {
		return OVNPortStatus{}, fmt.Errorf("logical switch port %q not found", lspName)
	}
	lsName, err := b.findLogicalSwitchForPort(ctx, lsp.UUID)
	if err != nil {
		return OVNPortStatus{}, err
	}
	return OVNPortStatus{
		LogicalSwitch:     lsName,
		LogicalSwitchPort: lsp.Name,
	}, nil
}

func (b *LibovsdbOVNBackend) DeleteLogicalSwitchPort(ctx context.Context, spec OVNPortSpec) error {
	if b == nil || b.client == nil {
		return fmt.Errorf("ovn backend client is not configured")
	}
	lspName := strings.TrimSpace(spec.LogicalSwitchPort)
	if lspName == "" {
		return fmt.Errorf("logical switch port is required")
	}
	lsp, err := b.getLogicalSwitchPortByName(ctx, lspName)
	if err != nil {
		return err
	}
	if lsp == nil {
		return nil
	}
	if err := validateOVNOwnership(lsp.ExternalIDs, spec); err != nil {
		return err
	}
	if err := validateOVNIncarnation(lsp.ExternalIDs, spec.Incarnation); err != nil {
		return err
	}

	switches, err := b.listLogicalSwitches(ctx)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for i := range switches {
		if !containsString(switches[i].Ports, lsp.UUID) {
			continue
		}
		ls := switches[i]
		mutateOps, err := b.client.Where(&ls).Mutate(&ls, model.Mutation{
			Field:   &ls.Ports,
			Mutator: ovsdb.MutateOperationDelete,
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return err
		}
		ops = append(ops, mutateOps...)
	}
	deleteOps, err := b.client.Where(&nbdb.LogicalSwitchPort{UUID: lsp.UUID}).Delete()
	if err != nil {
		return err
	}
	ops = append(ops, deleteOps...)
	return transactNoErr(ctx, b.client, ops...)
}

func (b *LibovsdbOVNBackend) CheckLogicalSwitchPortIdentity(ctx context.Context, spec OVNPortSpec) error {
	if b == nil || b.client == nil {
		return fmt.Errorf("ovn backend client is not configured")
	}
	lsp, err := b.getLogicalSwitchPortByName(ctx, spec.LogicalSwitchPort)
	if err != nil {
		return err
	}
	if lsp == nil {
		return fmt.Errorf("logical switch port %q not found", spec.LogicalSwitchPort)
	}
	if err := validateOVNOwnership(lsp.ExternalIDs, spec); err != nil {
		return err
	}
	if err := validateOVNIncarnation(lsp.ExternalIDs, spec.Incarnation); err != nil {
		return err
	}
	if expectedIfaceID := strings.TrimSpace(spec.LogicalSwitchPort); expectedIfaceID != "" {
		if got := strings.TrimSpace(lsp.ExternalIDs[ovnExternalIDIfaceID]); got != expectedIfaceID {
			return fmt.Errorf("ovn lsp %q iface-id mismatch: expected=%q got=%q", spec.LogicalSwitchPort, expectedIfaceID, got)
		}
	}
	if strings.TrimSpace(spec.LogicalSwitch) != "" {
		lsName, err := b.findLogicalSwitchForPort(ctx, lsp.UUID)
		if err != nil {
			return err
		}
		if lsName != strings.TrimSpace(spec.LogicalSwitch) {
			return fmt.Errorf("ovn lsp %q is attached to logical switch %q, expected %q", spec.LogicalSwitchPort, lsName, spec.LogicalSwitch)
		}
	}
	return nil
}

func (b *LibovsdbOVNBackend) getLogicalSwitchByName(ctx context.Context, name string) (*nbdb.LogicalSwitch, error) {
	switches, err := b.listLogicalSwitches(ctx)
	if err != nil {
		return nil, err
	}
	for i := range switches {
		if switches[i].Name == name {
			ls := switches[i]
			return &ls, nil
		}
	}
	return nil, fmt.Errorf("logical switch %q not found", name)
}

func (b *LibovsdbOVNBackend) getLogicalSwitchPortByName(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error) {
	var all []nbdb.LogicalSwitchPort
	if err := b.client.List(ctx, &all); err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			lsp := all[i]
			return &lsp, nil
		}
	}
	return nil, nil
}

func (b *LibovsdbOVNBackend) listLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error) {
	var switches []nbdb.LogicalSwitch
	if err := b.client.List(ctx, &switches); err != nil {
		return nil, err
	}
	return switches, nil
}

func (b *LibovsdbOVNBackend) findLogicalSwitchForPort(ctx context.Context, lspUUID string) (string, error) {
	switches, err := b.listLogicalSwitches(ctx)
	if err != nil {
		return "", err
	}
	for i := range switches {
		if containsString(switches[i].Ports, lspUUID) {
			return switches[i].Name, nil
		}
	}
	return "", fmt.Errorf("logical switch for lsp UUID %q not found", lspUUID)
}

func ovnExternalIDsForSpec(spec OVNPortSpec) map[string]string {
	return map[string]string{
		ovnExternalIDManagedBy:   ovnOwnerValue,
		ovnExternalIDEndpointID:  strings.TrimSpace(string(spec.EndpointID)),
		ovnExternalIDNetworkID:   strings.TrimSpace(spec.NetworkID),
		ovnExternalIDAttachID:    strings.TrimSpace(spec.AttachmentID),
		ovnExternalIDWorkloadUID: strings.TrimSpace(spec.WorkloadUID),
		ovnExternalIDIncarnation: strings.TrimSpace(spec.Incarnation),
		ovnExternalIDIfaceID:     strings.TrimSpace(spec.LogicalSwitchPort),
	}
}

func validateOVNOwnership(ids map[string]string, spec OVNPortSpec) error {
	if len(ids) == 0 {
		return fmt.Errorf("ovn lsp %q has no ownership metadata", spec.LogicalSwitchPort)
	}
	managedBy := strings.TrimSpace(ids[ovnExternalIDManagedBy])
	if managedBy != "" && managedBy != ovnOwnerValue {
		return fmt.Errorf("ovn lsp %q managed by %q", spec.LogicalSwitchPort, managedBy)
	}
	if endpointID := strings.TrimSpace(ids[ovnExternalIDEndpointID]); endpointID != "" && endpointID != strings.TrimSpace(string(spec.EndpointID)) {
		return fmt.Errorf("ovn lsp %q endpoint ownership mismatch", spec.LogicalSwitchPort)
	}
	if networkID := strings.TrimSpace(ids[ovnExternalIDNetworkID]); networkID != "" && networkID != strings.TrimSpace(spec.NetworkID) {
		return fmt.Errorf("ovn lsp %q network ownership mismatch", spec.LogicalSwitchPort)
	}
	if attachmentID := strings.TrimSpace(ids[ovnExternalIDAttachID]); attachmentID != "" && attachmentID != strings.TrimSpace(spec.AttachmentID) {
		return fmt.Errorf("ovn lsp %q attachment ownership mismatch", spec.LogicalSwitchPort)
	}
	if workloadUID := strings.TrimSpace(ids[ovnExternalIDWorkloadUID]); workloadUID != "" && workloadUID != strings.TrimSpace(spec.WorkloadUID) {
		return fmt.Errorf("ovn lsp %q workload ownership mismatch", spec.LogicalSwitchPort)
	}
	return nil
}

func validateOVNIncarnation(ids map[string]string, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	got := strings.TrimSpace(ids[ovnExternalIDIncarnation])
	if got == "" {
		return fmt.Errorf("ovn lsp missing incarnation metadata")
	}
	if got != expected {
		return fmt.Errorf("stale operation: expected incarnation=%q got=%q", expected, got)
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func transactNoErr(ctx context.Context, c client.Client, ops ...ovsdb.Operation) error {
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	for i, r := range results {
		if r.Error != "" {
			return fmt.Errorf("ovn op %d failed: %s %s", i, r.Error, r.Details)
		}
	}
	return nil
}
