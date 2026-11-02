package attachment

import (
	"context"
	"fmt"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type DeleteOutcome = endpoint.AttachmentDeleteOutcome

const (
	DeleteOutcomeDeleted DeleteOutcome = endpoint.AttachmentDeleteOutcomeDeleted
	DeleteOutcomeMissing DeleteOutcome = endpoint.AttachmentDeleteOutcomeMissing
	DeleteOutcomeStale   DeleteOutcome = endpoint.AttachmentDeleteOutcomeStale
)

type OVSStore interface {
	ListBridges(ctx context.Context) ([]vswitch.Bridge, error)
	ListInterfaces(ctx context.Context) ([]vswitch.Interface, error)
	ListPorts(ctx context.Context) ([]vswitch.Port, error)
	CreateInterface(ctx context.Context, iface *vswitch.Interface) error
	CreateInterfacePortAndAttachBridge(ctx context.Context, bridgeName string, iface *vswitch.Interface, port *vswitch.Port) error
	UpdateInterface(ctx context.Context, iface *vswitch.Interface) error
	DeleteInterface(ctx context.Context, iface *vswitch.Interface) error
	DeleteInterfaceIfExternalIDsMatch(ctx context.Context, uuid string, expectedExternalIDs map[string]string) (bool, error)
	CreatePort(ctx context.Context, port *vswitch.Port) error
	CreatePortAndAttachBridge(ctx context.Context, bridgeName string, port *vswitch.Port) error
	UpdatePort(ctx context.Context, port *vswitch.Port) error
	DeletePort(ctx context.Context, port *vswitch.Port) error
	EnsureBridgePort(ctx context.Context, bridgeName, portUUID string) error
	DetachPortFromBridgeIfInterfaceOwned(ctx context.Context, bridgeName, portUUID, ifaceUUID string, expectedExternalIDs map[string]string) (bool, error)
}

type OVSAttachmentManager struct {
	store OVSStore
}

func NewOVSAttachmentManager(store OVSStore) *OVSAttachmentManager {
	return &OVSAttachmentManager{store: store}
}

func (m *OVSAttachmentManager) EnsureAttachment(ctx context.Context, spec endpoint.AttachmentSpec) (endpoint.AttachmentStatus, error) {
	identity := Identity{
		EndpointID:   string(spec.EndpointID),
		NetworkID:    "legacy",
		AttachmentID: spec.LogicalSwitchPort,
		WorkloadUID:  "legacy",
		Incarnation:  "legacy",
	}
	return m.EnsureAttachmentWithIdentity(ctx, identity, spec)
}

func (m *OVSAttachmentManager) EnsureAttachmentWithIdentity(ctx context.Context, identity Identity, spec endpoint.AttachmentSpec) (endpoint.AttachmentStatus, error) {
	if err := ValidateIdentity(identity); err != nil {
		return endpoint.AttachmentStatus{}, err
	}
	if strings.TrimSpace(spec.OVSInterfaceName) == "" {
		return endpoint.AttachmentStatus{}, fmt.Errorf("ovs interface name is required")
	}
	if strings.TrimSpace(spec.LogicalSwitchPort) == "" {
		return endpoint.AttachmentStatus{}, fmt.Errorf("logical switch port is required")
	}

	ifaces, err := m.store.ListInterfaces(ctx)
	if err != nil {
		return endpoint.AttachmentStatus{}, err
	}

	var existing *vswitch.Interface
	for i := range ifaces {
		if ifaces[i].Name == spec.OVSInterfaceName {
			existing = &ifaces[i]
			break
		}
	}

	desiredExternalIDs := ExternalIDsForIdentity(identity, spec.LogicalSwitchPort, spec.MACAddress)
	ports, err := m.store.ListPorts(ctx)
	if err != nil {
		return endpoint.AttachmentStatus{}, err
	}
	var existingPort *vswitch.Port
	for i := range ports {
		if ports[i].Name == spec.OVSInterfaceName {
			existingPort = &ports[i]
			break
		}
	}

	if existing != nil {
		if existing.ExternalIDs != nil {
			managedBy := strings.TrimSpace(existing.ExternalIDs[ExternalIDManagedBy])
			if managedBy != "" && managedBy != OwnerValue {
				return endpoint.AttachmentStatus{}, fmt.Errorf("%w: interface %s managed by %s", ErrAttachmentOwnershipConflict, existing.Name, managedBy)
			}
			existingIdentity := identityFromExternalIDs(existing.ExternalIDs)
			if existingIdentity.EndpointID != "" {
				if err := validateIdentityMatch(existingIdentity, identity); err != nil {
					return endpoint.AttachmentStatus{}, err
				}
				if existingIdentity.Incarnation != identity.Incarnation {
					// Ensure from newer incarnation is allowed and updates metadata.
				}
			}
		}
		if !isManagedByFramework(existing) {
			existingIfaceID := ""
			if existing.ExternalIDs != nil {
				existingIfaceID = strings.TrimSpace(existing.ExternalIDs[ExternalIDIfaceID])
			}
			if existingIfaceID != spec.LogicalSwitchPort {
				return endpoint.AttachmentStatus{}, fmt.Errorf("%w: unmanaged legacy interface %s is bound to %q and cannot be modified", ErrAttachmentOwnershipConflict, existing.Name, existingIfaceID)
			}
			return endpoint.AttachmentStatus{OVSInterfaceName: existing.Name}, nil
		}
		upd := &vswitch.Interface{
			UUID:        existing.UUID,
			Name:        existing.Name,
			Type:        "system",
			ExternalIDs: desiredExternalIDs,
		}
		if err := m.store.UpdateInterface(ctx, upd); err != nil {
			return endpoint.AttachmentStatus{}, err
		}
		if existingPort == nil {
			port := &vswitch.Port{
				Name:       spec.OVSInterfaceName,
				Interfaces: []string{existing.UUID},
			}
			if err := m.store.CreatePortAndAttachBridge(ctx, "br-int", port); err != nil {
				return endpoint.AttachmentStatus{}, err
			}
			return endpoint.AttachmentStatus{OVSInterfaceName: existing.Name}, nil
		}
		if !contains(existingPort.Interfaces, existing.UUID) {
			if len(existingPort.Interfaces) == 0 {
				updPort := &vswitch.Port{
					UUID:       existingPort.UUID,
					Name:       existingPort.Name,
					Interfaces: []string{existing.UUID},
				}
				if err := m.store.UpdatePort(ctx, updPort); err != nil {
					return endpoint.AttachmentStatus{}, err
				}
			} else {
				return endpoint.AttachmentStatus{}, fmt.Errorf("%w: port %s references different interfaces %v", ErrAttachmentOwnershipConflict, existingPort.Name, existingPort.Interfaces)
			}
		}
		if err := m.store.EnsureBridgePort(ctx, "br-int", existingPort.UUID); err != nil {
			return endpoint.AttachmentStatus{}, err
		}
		return endpoint.AttachmentStatus{OVSInterfaceName: existing.Name}, nil
	}
	if existingPort != nil {
		return endpoint.AttachmentStatus{}, fmt.Errorf("%w: port %s exists without matching interface", ErrAttachmentOwnershipConflict, existingPort.Name)
	}

	iface := &vswitch.Interface{
		Name:        spec.OVSInterfaceName,
		Type:        "system",
		ExternalIDs: desiredExternalIDs,
	}
	port := &vswitch.Port{
		Name: spec.OVSInterfaceName,
	}
	if err := m.store.CreateInterfacePortAndAttachBridge(ctx, "br-int", iface, port); err != nil {
		return endpoint.AttachmentStatus{}, err
	}

	return endpoint.AttachmentStatus{OVSInterfaceName: spec.OVSInterfaceName}, nil
}

func (m *OVSAttachmentManager) GetAttachmentByIdentity(ctx context.Context, identity Identity, logicalSwitchPort string) (AttachmentRecord, error) {
	if err := ValidateIdentity(identity); err != nil {
		return AttachmentRecord{}, err
	}
	ifaces, err := m.store.ListInterfaces(ctx)
	if err != nil {
		return AttachmentRecord{}, err
	}
	for i := range ifaces {
		iface := ifaces[i]
		if iface.ExternalIDs == nil {
			continue
		}
		if strings.TrimSpace(iface.ExternalIDs[ExternalIDIfaceID]) != logicalSwitchPort {
			continue
		}
		existingIdentity := identityFromExternalIDs(iface.ExternalIDs)
		if existingIdentity.EndpointID == "" {
			return AttachmentRecord{}, fmt.Errorf("%w: unmanaged legacy interface %s", ErrAttachmentOwnershipConflict, iface.Name)
		}
		if err := validateIdentityMatch(existingIdentity, identity); err != nil {
			return AttachmentRecord{}, err
		}
		if existingIdentity.Incarnation != identity.Incarnation {
			return AttachmentRecord{}, fmt.Errorf("%w: expected incarnation=%s got=%s", ErrStaleOperation, identity.Incarnation, existingIdentity.Incarnation)
		}
		return AttachmentRecord{
			InterfaceName:     iface.Name,
			LogicalSwitchPort: logicalSwitchPort,
			Identity:          existingIdentity,
			ManagedBy:         strings.TrimSpace(iface.ExternalIDs[ExternalIDManagedBy]),
		}, nil
	}
	return AttachmentRecord{}, ErrAttachmentNotFound
}

func (m *OVSAttachmentManager) CheckAttachmentWithIdentity(ctx context.Context, identity Identity, logicalSwitchPort, expectedInterfaceName string) (AttachmentRecord, error) {
	record, err := m.GetAttachmentByIdentity(ctx, identity, logicalSwitchPort)
	if err != nil {
		return AttachmentRecord{}, err
	}
	if expected := strings.TrimSpace(expectedInterfaceName); expected != "" && record.InterfaceName != expected {
		return AttachmentRecord{}, fmt.Errorf("%w: interface name mismatch existing=%q expected=%q", ErrAttachmentOwnershipConflict, record.InterfaceName, expected)
	}

	ifaces, err := m.store.ListInterfaces(ctx)
	if err != nil {
		return AttachmentRecord{}, err
	}
	var iface *vswitch.Interface
	for i := range ifaces {
		if ifaces[i].Name == record.InterfaceName {
			iface = &ifaces[i]
			break
		}
	}
	if iface == nil {
		return AttachmentRecord{}, ErrAttachmentNotFound
	}
	if iface.ExternalIDs == nil || strings.TrimSpace(iface.ExternalIDs[ExternalIDIfaceID]) != logicalSwitchPort {
		return AttachmentRecord{}, fmt.Errorf("%w: iface-id mismatch for %s", ErrAttachmentOwnershipConflict, record.InterfaceName)
	}

	ports, err := m.store.ListPorts(ctx)
	if err != nil {
		return AttachmentRecord{}, err
	}
	var port *vswitch.Port
	for i := range ports {
		if !contains(ports[i].Interfaces, iface.UUID) {
			continue
		}
		if port != nil {
			return AttachmentRecord{}, fmt.Errorf("%w: interface %s is referenced by multiple ports", ErrAttachmentOwnershipConflict, record.InterfaceName)
		}
		port = &ports[i]
	}
	if port == nil {
		return AttachmentRecord{}, fmt.Errorf("%w: interface %s has no port binding", ErrAttachmentOwnershipConflict, record.InterfaceName)
	}

	bridges, err := m.store.ListBridges(ctx)
	if err != nil {
		return AttachmentRecord{}, err
	}
	bridgeName := ""
	for i := range bridges {
		if !contains(bridges[i].Ports, port.UUID) {
			continue
		}
		if bridgeName != "" {
			return AttachmentRecord{}, fmt.Errorf("%w: port %s is attached to multiple bridges", ErrAttachmentOwnershipConflict, port.Name)
		}
		bridgeName = bridges[i].Name
	}
	if bridgeName != "br-int" {
		if bridgeName == "" {
			return AttachmentRecord{}, fmt.Errorf("%w: port %s is not attached to any bridge", ErrAttachmentOwnershipConflict, port.Name)
		}
		return AttachmentRecord{}, fmt.Errorf("%w: port %s is attached to unexpected bridge %s", ErrAttachmentOwnershipConflict, port.Name, bridgeName)
	}

	return record, nil
}

func (m *OVSAttachmentManager) DeleteAttachment(ctx context.Context, endpointID endpoint.EndpointID, logicalSwitchPort string) error {
	identity := Identity{
		EndpointID:   string(endpointID),
		NetworkID:    "legacy",
		AttachmentID: logicalSwitchPort,
		WorkloadUID:  "legacy",
		Incarnation:  "legacy",
	}
	_, err := m.DeleteAttachmentWithIdentity(ctx, identity, logicalSwitchPort)
	return err
}

func (m *OVSAttachmentManager) DeleteAttachmentWithIdentity(ctx context.Context, identity Identity, logicalSwitchPort string) (DeleteOutcome, error) {
	if err := ValidateIdentity(identity); err != nil {
		return DeleteOutcomeMissing, err
	}
	ifaces, err := m.store.ListInterfaces(ctx)
	if err != nil {
		return DeleteOutcomeMissing, err
	}
	for i := range ifaces {
		iface := ifaces[i]
		if iface.ExternalIDs == nil {
			continue
		}
		if strings.TrimSpace(iface.ExternalIDs[ExternalIDIfaceID]) != logicalSwitchPort {
			continue
		}
		managedBy := strings.TrimSpace(iface.ExternalIDs[ExternalIDManagedBy])
		if managedBy != "" && managedBy != OwnerValue {
			return DeleteOutcomeMissing, fmt.Errorf("%w: interface %s managed by %s", ErrAttachmentOwnershipConflict, iface.Name, managedBy)
		}
		if !isManagedByFramework(&iface) {
			return DeleteOutcomeMissing, fmt.Errorf("%w: unmanaged legacy interface %s is preserved and cannot be deleted by this workflow", ErrAttachmentOwnershipConflict, iface.Name)
		}

		existingIdentity := identityFromExternalIDs(iface.ExternalIDs)
		if err := validateIdentityMatch(existingIdentity, identity); err != nil {
			return DeleteOutcomeMissing, err
		}
		if existingIdentity.Incarnation != identity.Incarnation {
			return DeleteOutcomeStale, fmt.Errorf("%w: expected incarnation=%s got=%s", ErrStaleOperation, identity.Incarnation, existingIdentity.Incarnation)
		}

		ports, perr := m.store.ListPorts(ctx)
		if perr != nil {
			return DeleteOutcomeMissing, perr
		}
		var boundPort *vswitch.Port
		for j := range ports {
			if !contains(ports[j].Interfaces, iface.UUID) {
				continue
			}
			if boundPort != nil {
				return DeleteOutcomeMissing, fmt.Errorf("%w: interface %s is referenced by multiple ports", ErrAttachmentOwnershipConflict, iface.Name)
			}
			boundPort = &ports[j]
		}
		expectedExternalIDs := cloneExternalIDs(iface.ExternalIDs)
		if boundPort == nil {
			deleted, derr := m.store.DeleteInterfaceIfExternalIDsMatch(ctx, iface.UUID, expectedExternalIDs)
			if derr != nil {
				return DeleteOutcomeMissing, derr
			}
			if !deleted {
				return DeleteOutcomeStale, fmt.Errorf("%w: attachment changed before delete", ErrStaleOperation)
			}
			return DeleteOutcomeDeleted, nil
		}

		bridges, berr := m.store.ListBridges(ctx)
		if berr != nil {
			return DeleteOutcomeMissing, berr
		}
		bridgeName := ""
		for b := range bridges {
			if contains(bridges[b].Ports, boundPort.UUID) {
				if bridgeName != "" {
					return DeleteOutcomeMissing, fmt.Errorf("%w: port %s is attached to multiple bridges", ErrAttachmentOwnershipConflict, boundPort.Name)
				}
				bridgeName = bridges[b].Name
			}
		}
		if bridgeName == "" {
			return DeleteOutcomeMissing, fmt.Errorf("%w: port %s is not attached to any bridge", ErrAttachmentOwnershipConflict, boundPort.Name)
		}
		if bridgeName != "br-int" {
			return DeleteOutcomeMissing, fmt.Errorf("%w: port %s is attached to unexpected bridge %s", ErrAttachmentOwnershipConflict, boundPort.Name, bridgeName)
		}

		detached, derr := m.store.DetachPortFromBridgeIfInterfaceOwned(ctx, "br-int", boundPort.UUID, iface.UUID, expectedExternalIDs)
		if derr != nil {
			return DeleteOutcomeMissing, derr
		}
		if !detached {
			return DeleteOutcomeStale, fmt.Errorf("%w: attachment changed before delete", ErrStaleOperation)
		}
		return DeleteOutcomeDeleted, nil
	}
	return DeleteOutcomeMissing, nil
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func cloneExternalIDs(ids map[string]string) map[string]string {
	if len(ids) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(ids))
	for k, v := range ids {
		out[k] = v
	}
	return out
}
