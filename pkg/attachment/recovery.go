package attachment

import (
	"errors"
	"fmt"
	"strings"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

var (
	ErrAttachmentOwnershipConflict = errors.New("attachment ownership conflict")
	ErrStaleOperation              = errors.New("stale operation")
	ErrAttachmentNotFound          = errors.New("attachment not found")
)

type AttachmentRecord struct {
	InterfaceName     string
	LogicalSwitchPort string
	Identity          Identity
	ManagedBy         string
}

func identityFromExternalIDs(externalIDs map[string]string) Identity {
	if externalIDs == nil {
		return Identity{}
	}
	return Identity{
		EndpointID:   strings.TrimSpace(externalIDs[ExternalIDEndpointID]),
		NetworkID:    strings.TrimSpace(externalIDs[ExternalIDNetworkID]),
		AttachmentID: strings.TrimSpace(externalIDs[ExternalIDAttachID]),
		WorkloadUID:  strings.TrimSpace(externalIDs[ExternalIDWorkloadUID]),
		Incarnation:  strings.TrimSpace(externalIDs[ExternalIDIncarnation]),
	}
}

func isManagedByFramework(iface *vswitch.Interface) bool {
	return iface.ExternalIDs != nil && strings.TrimSpace(iface.ExternalIDs[ExternalIDManagedBy]) == OwnerValue
}

func validateIdentityMatch(existing Identity, wanted Identity) error {
	if existing.EndpointID != wanted.EndpointID {
		return fmt.Errorf("%w: endpoint mismatch existing=%q wanted=%q", ErrAttachmentOwnershipConflict, existing.EndpointID, wanted.EndpointID)
	}
	if existing.NetworkID != wanted.NetworkID {
		return fmt.Errorf("%w: network mismatch existing=%q wanted=%q", ErrAttachmentOwnershipConflict, existing.NetworkID, wanted.NetworkID)
	}
	if existing.AttachmentID != wanted.AttachmentID {
		return fmt.Errorf("%w: attachment mismatch existing=%q wanted=%q", ErrAttachmentOwnershipConflict, existing.AttachmentID, wanted.AttachmentID)
	}
	if existing.WorkloadUID != wanted.WorkloadUID {
		return fmt.Errorf("%w: workload uid mismatch existing=%q wanted=%q", ErrAttachmentOwnershipConflict, existing.WorkloadUID, wanted.WorkloadUID)
	}
	return nil
}
