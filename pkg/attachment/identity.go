package attachment

import (
	"fmt"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
)

const (
	ExternalIDManagedBy   = "evpn.framework/managed-by"
	ExternalIDEndpointID  = "evpn.framework/endpoint-id"
	ExternalIDNetworkID   = "evpn.framework/network-id"
	ExternalIDAttachID    = "evpn.framework/attachment-id"
	ExternalIDWorkloadUID = "evpn.framework/workload-uid"
	ExternalIDIncarnation = "evpn.framework/incarnation"
	ExternalIDIfaceID     = "iface-id"
	OwnerValue            = "evpn-node-agent"
)

type Identity = endpoint.AttachmentIdentity

func ValidateIdentity(i Identity) error {
	if strings.TrimSpace(i.EndpointID) == "" {
		return fmt.Errorf("endpoint_id is required")
	}
	if strings.TrimSpace(i.NetworkID) == "" {
		return fmt.Errorf("network_id is required")
	}
	if strings.TrimSpace(i.AttachmentID) == "" {
		return fmt.Errorf("attachment_id is required")
	}
	if strings.TrimSpace(i.WorkloadUID) == "" {
		return fmt.Errorf("workload_uid is required")
	}
	if strings.TrimSpace(i.Incarnation) == "" {
		return fmt.Errorf("incarnation is required")
	}
	return nil
}

func ExternalIDsForIdentity(i Identity, logicalSwitchPort, mac string) map[string]string {
	ids := map[string]string{
		ExternalIDManagedBy:   OwnerValue,
		ExternalIDEndpointID:  i.EndpointID,
		ExternalIDNetworkID:   i.NetworkID,
		ExternalIDAttachID:    i.AttachmentID,
		ExternalIDWorkloadUID: i.WorkloadUID,
		ExternalIDIncarnation: i.Incarnation,
		ExternalIDIfaceID:     logicalSwitchPort,
	}
	if strings.TrimSpace(mac) != "" {
		ids["attached-mac"] = mac
	}
	return ids
}
