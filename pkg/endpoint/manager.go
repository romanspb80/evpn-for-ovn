package endpoint

import (
	"context"
	"fmt"
	"strings"
)

type OVNPortSpec struct {
	EndpointID        EndpointID
	NetworkID         string
	AttachmentID      string
	WorkloadUID       string
	Incarnation       string
	LogicalSwitch     string
	LogicalSwitchPort string
	Addresses         []string
}

type OVNPortStatus struct {
	LogicalSwitch     string
	LogicalSwitchPort string
}

type AttachmentSpec struct {
	EndpointID        EndpointID
	LogicalSwitchPort string
	OVSInterfaceName  string
	MACAddress        string
}

type AttachmentStatus struct {
	OVSInterfaceName string
}

type AttachmentIdentity struct {
	EndpointID   string
	NetworkID    string
	AttachmentID string
	WorkloadUID  string
	Incarnation  string
}

type AttachmentDeleteOutcome string

const (
	AttachmentDeleteOutcomeDeleted AttachmentDeleteOutcome = "deleted"
	AttachmentDeleteOutcomeMissing AttachmentDeleteOutcome = "missing"
	AttachmentDeleteOutcomeStale   AttachmentDeleteOutcome = "stale"
)

type OVNBackend interface {
	EnsureLogicalSwitchPort(ctx context.Context, spec OVNPortSpec) (OVNPortStatus, error)
	GetLogicalSwitchPort(ctx context.Context, logicalSwitchPort string) (OVNPortStatus, error)
	DeleteLogicalSwitchPort(ctx context.Context, spec OVNPortSpec) error
}

type AttachmentManager interface {
	EnsureAttachment(ctx context.Context, spec AttachmentSpec) (AttachmentStatus, error)
	DeleteAttachment(ctx context.Context, endpointID EndpointID, logicalSwitchPort string) error
}

type IdentityAwareAttachmentManager interface {
	EnsureAttachmentWithIdentity(ctx context.Context, identity AttachmentIdentity, spec AttachmentSpec) (AttachmentStatus, error)
	DeleteAttachmentWithIdentity(ctx context.Context, identity AttachmentIdentity, logicalSwitchPort string) (AttachmentDeleteOutcome, error)
}

type EndpointManager interface {
	EnsureEndpoint(ctx context.Context, spec EndpointSpec) (EndpointStatus, error)
	GetEndpoint(ctx context.Context, id EndpointID, logicalSwitchPort string) (EndpointStatus, error)
	DeleteEndpoint(ctx context.Context, spec EndpointSpec) error
}

type Manager struct {
	ovn         OVNBackend
	attach      AttachmentManager
	allowAttach bool
}

func NewManager(ovn OVNBackend, attach AttachmentManager, allowAttach bool) *Manager {
	return &Manager{
		ovn:         ovn,
		attach:      attach,
		allowAttach: allowAttach,
	}
}

func (m *Manager) EnsureEndpoint(ctx context.Context, spec EndpointSpec) (EndpointStatus, error) {
	if err := spec.Validate(); err != nil {
		return EndpointStatus{}, err
	}
	select {
	case <-ctx.Done():
		return EndpointStatus{}, ctx.Err()
	default:
	}

	addresses := make([]string, 0, len(spec.IPAddresses))
	for _, ip := range spec.IPAddresses {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		addresses = append(addresses, fmt.Sprintf("%s %s", spec.MACAddress, ip))
	}
	if len(addresses) == 0 {
		addresses = append(addresses, spec.MACAddress)
	}

	ovnStatus, err := m.ovn.EnsureLogicalSwitchPort(ctx, OVNPortSpec{
		EndpointID:        spec.ID,
		NetworkID:         spec.NetworkID,
		AttachmentID:      spec.AttachmentID,
		WorkloadUID:       spec.Workload.UID,
		Incarnation:       spec.Incarnation,
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
		Addresses:         addresses,
	})
	if err != nil {
		return EndpointStatus{
			ID:                 spec.ID,
			LogicalSwitch:      spec.LogicalSwitch,
			LogicalSwitchPort:  spec.LogicalSwitchPort,
			OVSInterfaceName:   spec.OVSInterfaceName,
			OVNPortState:       StateError,
			LocalAttachState:   StateSkipped,
			DataplaneState:     StateUnobserved,
			EVPNAdvertiseState: StateUnobserved,
			Message:            err.Error(),
		}, err
	}

	status := EndpointStatus{
		ID:                 spec.ID,
		LogicalSwitch:      ovnStatus.LogicalSwitch,
		LogicalSwitchPort:  ovnStatus.LogicalSwitchPort,
		OVSInterfaceName:   spec.OVSInterfaceName,
		OVNPortState:       StateEnsured,
		LocalAttachState:   StateSkipped,
		DataplaneState:     StateUnobserved,
		EVPNAdvertiseState: StateUnobserved,
	}

	if !m.allowAttach || m.attach == nil {
		status.LocalAttachState = StateSkipped
		status.Message = "local attachment skipped by configuration"
		return status, nil
	}

	identityManager, ok := m.attach.(IdentityAwareAttachmentManager)
	if !ok {
		return status, fmt.Errorf("attachment manager must support identity-aware lifecycle operations")
	}
	attachStatus, err := identityManager.EnsureAttachmentWithIdentity(ctx, attachmentIdentityFromSpec(spec), AttachmentSpec{
		EndpointID:        spec.ID,
		LogicalSwitchPort: spec.LogicalSwitchPort,
		OVSInterfaceName:  spec.OVSInterfaceName,
		MACAddress:        spec.MACAddress,
	})
	if err != nil {
		status.LocalAttachState = StateError
		status.Message = err.Error()
		return status, err
	}
	status.LocalAttachState = StateEnsured
	status.OVSInterfaceName = attachStatus.OVSInterfaceName
	return status, nil
}

func (m *Manager) GetEndpoint(ctx context.Context, id EndpointID, logicalSwitchPort string) (EndpointStatus, error) {
	ovnStatus, err := m.ovn.GetLogicalSwitchPort(ctx, logicalSwitchPort)
	if err != nil {
		return EndpointStatus{
			ID:                 id,
			LogicalSwitchPort:  logicalSwitchPort,
			OVNPortState:       StateError,
			LocalAttachState:   StateUnobserved,
			DataplaneState:     StateUnobserved,
			EVPNAdvertiseState: StateUnobserved,
			Message:            err.Error(),
		}, err
	}
	return EndpointStatus{
		ID:                 id,
		LogicalSwitch:      ovnStatus.LogicalSwitch,
		LogicalSwitchPort:  ovnStatus.LogicalSwitchPort,
		OVNPortState:       StateEnsured,
		LocalAttachState:   StateUnobserved,
		DataplaneState:     StateUnobserved,
		EVPNAdvertiseState: StateUnobserved,
	}, nil
}

func (m *Manager) DeleteEndpoint(ctx context.Context, spec EndpointSpec) error {
	if strings.TrimSpace(string(spec.ID)) == "" {
		return fmt.Errorf("endpoint id is required")
	}
	if m.allowAttach && m.attach != nil {
		identityManager, ok := m.attach.(IdentityAwareAttachmentManager)
		if !ok {
			return fmt.Errorf("attachment manager must support identity-aware lifecycle operations")
		}
		if _, err := identityManager.DeleteAttachmentWithIdentity(ctx, attachmentIdentityFromSpec(spec), spec.LogicalSwitchPort); err != nil {
			return err
		}
	}
	return m.ovn.DeleteLogicalSwitchPort(ctx, OVNPortSpec{
		EndpointID:        spec.ID,
		NetworkID:         spec.NetworkID,
		AttachmentID:      spec.AttachmentID,
		WorkloadUID:       spec.Workload.UID,
		Incarnation:       spec.Incarnation,
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
	})
}

func (m *Manager) CheckEndpointIdentity(ctx context.Context, spec EndpointSpec) error {
	if strings.TrimSpace(string(spec.ID)) == "" {
		return fmt.Errorf("endpoint id is required")
	}
	if checker, ok := m.ovn.(interface {
		CheckLogicalSwitchPortIdentity(context.Context, OVNPortSpec) error
	}); ok {
		return checker.CheckLogicalSwitchPortIdentity(ctx, OVNPortSpec{
			EndpointID:        spec.ID,
			NetworkID:         spec.NetworkID,
			AttachmentID:      spec.AttachmentID,
			WorkloadUID:       spec.Workload.UID,
			Incarnation:       spec.Incarnation,
			LogicalSwitch:     spec.LogicalSwitch,
			LogicalSwitchPort: spec.LogicalSwitchPort,
		})
	}
	_, err := m.GetEndpoint(ctx, spec.ID, spec.LogicalSwitchPort)
	return err
}

func attachmentIdentityFromSpec(spec EndpointSpec) AttachmentIdentity {
	return AttachmentIdentity{
		EndpointID:   string(spec.ID),
		NetworkID:    strings.TrimSpace(spec.NetworkID),
		AttachmentID: strings.TrimSpace(spec.AttachmentID),
		WorkloadUID:  strings.TrimSpace(spec.Workload.UID),
		Incarnation:  strings.TrimSpace(spec.Incarnation),
	}
}
