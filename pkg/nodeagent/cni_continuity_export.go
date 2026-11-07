package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

// ValidateContinuity is fail-closed on unavailable identity/resource evidence.
// Disabled continuity retains existing behavior. Known deleted Pods are omitted
// from the store without recreating or deleting their residual resources.
func (s *CNIServer) ValidateContinuity(ctx context.Context) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	return s.validateContinuityLocked(ctx)
}

func (s *CNIServer) validateContinuityLocked(ctx context.Context) error {
	c := s.continuity
	if c == nil {
		return nil
	}
	if err := c.ready(); err != nil {
		return err
	}
	for key, ack := range c.state.Endpoints {
		if ack.State == "pending_creation" || ack.State == "quarantined" {
			return fmt.Errorf("RECOVERY_BLOCKED: endpoint %q is %s", key, ack.State)
		}
		if ack.State != "active" {
			continue
		}
		deleted, err := c.workload.Verify(ctx, ack.Spec.Workload, ack.Runtime.GetContainerId(), ack.Runtime.GetNetnsPath())
		if err != nil {
			return err
		}
		if deleted {
			s.store.delete(key)
			continue
		}
		if err := c.verifyResources(ctx, s, ack); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRecoveredExportEndpoints gates both CNI and ovn-netlink export
// authorities using durable ACKs. Native IDs map to the pinned LSP UUID; native
// incarnation is the independently observed SB Port_Binding UUID, not CNI ID.
// Unknown/deleted endpoints are rejected, never implicitly adopted.
func (s *CNIServer) ValidateRecoveredExportEndpoints(ctx context.Context, endpoints []evpn.LocalEndpoint) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	return s.validateRecoveredExportEndpointsLocked(ctx, endpoints)
}

// Caller holds operations through both validation and the export mutation.
func (s *CNIServer) validateRecoveredExportEndpointsLocked(ctx context.Context, endpoints []evpn.LocalEndpoint) error {
	if s.stopping {
		return errors.New("CNI operations drained")
	}
	if s.continuity == nil {
		return nil
	}
	if err := s.validateContinuityLocked(ctx); err != nil {
		return err
	}
	for _, ep := range endpoints {
		matched := false
		for key, ack := range s.continuity.state.Endpoints {
			if ack.State != "active" || ep.NetworkID != ack.Spec.NetworkID {
				continue
			}
			if ep.EndpointID != string(ack.Spec.ID) && ep.EndpointID != "ovn-netlink/"+ack.Receipt.LSPUUID {
				continue
			}
			if _, present := s.store.get(key); !present {
				return errors.New("deleted Pod cannot be exported from residual native resources")
			}
			if ep.EndpointID == string(ack.Spec.ID) && ep.Incarnation != ack.Spec.Incarnation {
				return errors.New("CNI export incarnation mismatch")
			}
			if !strings.EqualFold(ep.MACAddress, ack.Spec.MACAddress) {
				return errors.New("export address differs from durable endpoint")
			}
			if ep.MACOnly {
				if len(ep.IPAddresses) != 0 {
					return errors.New("MAC-only export contains IP addresses")
				}
			} else {
				if len(ep.IPAddresses) != len(ack.Spec.IPAddresses) {
					return errors.New("export IP set differs from durable endpoint")
				}
				for i, cidr := range ack.Spec.IPAddresses {
					ip, _, err := net.ParseCIDR(cidr)
					if err != nil || ep.IPAddresses[i] != ip.String() {
						return errors.New("export IP differs from durable endpoint")
					}
				}
			}
			matched = true
			break
		}
		if !matched {
			return fmt.Errorf("native/CNI export endpoint %q has no verified durable ACK", ep.EndpointID)
		}
	}
	return nil
}
