package endpoint

import (
	"fmt"
	"net"
	"strings"
)

type EndpointID string

type WorkloadRef struct {
	Kind      string
	UID       string
	Name      string
	Namespace string
	NodeName  string
}

// EndpointSpec describes desired endpoint state independent of local interface names.
type EndpointSpec struct {
	ID           EndpointID
	NetworkID    string
	AttachmentID string
	Incarnation  string
	Workload     WorkloadRef

	LogicalSwitch     string
	LogicalSwitchPort string
	OVSInterfaceName  string

	MACAddress  string
	IPAddresses []string
}

type ObservationState string

const (
	StateUnobserved ObservationState = "unobserved"
	StateEnsured    ObservationState = "ensured"
	StateSkipped    ObservationState = "skipped"
	StateConflict   ObservationState = "conflict"
	StateError      ObservationState = "error"
)

type EndpointStatus struct {
	ID EndpointID

	LogicalSwitch     string
	LogicalSwitchPort string
	OVSInterfaceName  string

	OVNPortState       ObservationState
	LocalAttachState   ObservationState
	DataplaneState     ObservationState
	EVPNAdvertiseState ObservationState

	Message string
}

func (s EndpointSpec) Validate() error {
	if strings.TrimSpace(string(s.ID)) == "" {
		return fmt.Errorf("endpoint id is required")
	}
	if strings.TrimSpace(s.NetworkID) == "" {
		return fmt.Errorf("network id is required")
	}
	if strings.TrimSpace(s.AttachmentID) == "" {
		return fmt.Errorf("attachment id is required")
	}
	if strings.TrimSpace(s.LogicalSwitch) == "" {
		return fmt.Errorf("logical switch is required")
	}
	if strings.TrimSpace(s.LogicalSwitchPort) == "" {
		return fmt.Errorf("logical switch port is required")
	}
	if strings.TrimSpace(s.MACAddress) == "" {
		return fmt.Errorf("mac address is required")
	}
	if _, err := net.ParseMAC(s.MACAddress); err != nil {
		return fmt.Errorf("invalid mac address %q: %w", s.MACAddress, err)
	}
	for _, ip := range s.IPAddresses {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if strings.Contains(ip, "/") {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				return fmt.Errorf("invalid ip prefix %q: %w", ip, err)
			}
			continue
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("invalid ip address %q", ip)
		}
	}
	return nil
}
