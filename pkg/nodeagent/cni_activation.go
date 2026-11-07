package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

var errCNIRuntimePending = errors.New("runtime sandbox not READY")
var errCNIIdentityConflict = errors.New("runtime workload identity conflict")
var errCNIResourceUnready = errors.New("CNI OVS ofport not assigned")

func (c *cniContinuity) observeCreated(ctx context.Context, rec *endpointRecord) (cniReceipt, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		receipt, absent, err := c.observer.Observe(ctx, rec)
		if !errors.Is(err, errCNIResourceUnready) {
			return receipt, absent, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cniReceipt{}, false, fmt.Errorf("CNI owned attachment readback: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// Only acknowledged resources may be promoted. No Ensure operation occurs here.
func (s *CNIServer) verifyPendingCNI(ctx context.Context) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.stopping || s.continuity == nil {
		return nil
	}
	c := s.continuity
	if err := c.ready(); err != nil {
		return err
	}
	for key, ack := range c.state.Endpoints {
		if ack.State != "pending_creation" {
			continue
		}
		deleted, err := c.workload.Verify(ctx, ack.Spec.Workload, ack.Runtime.GetContainerId(), ack.Runtime.GetNetnsPath())
		reason := ""
		if deleted {
			reason = "Pod deleted before activation"
		} else if errors.Is(err, errCNIIdentityConflict) {
			reason = err.Error()
		} else if !time.Now().Before(ack.VerifyBy) {
			reason = "runtime publication deadline expired"
		} else if err != nil {
			return fmt.Errorf("pending CNI runtime verification: %w", err)
		} else if resourceErr := c.verifyResources(ctx, s, ack); resourceErr != nil {
			reason = "acknowledged resource identity unavailable or changed"
		}
		if reason == "" && !time.Now().Before(ack.VerifyBy) {
			reason = "runtime publication deadline expired during verification"
		}
		if reason != "" {
			ack.State, ack.Reason = "quarantined", reason
		} else {
			ack.State = "active"
			ack.VerifiedAt = time.Now().UTC()
		}
		seq, err := c.journal.Begin("cni-identity", ack)
		if err != nil {
			return err
		}
		next := cniACKState{Version: c.state.Version, Endpoints: make(map[string]cniEndpointACK, len(c.state.Endpoints))}
		for k, v := range c.state.Endpoints {
			next.Endpoints[k] = v
		}
		next.Endpoints[key] = ack
		if err := c.journal.Commit(seq, next); err != nil {
			return fmt.Errorf("%w: activation acknowledgement: %v", ownership.ErrBlocked, err)
		}
		c.state = next
		if ack.State == "quarantined" {
			s.store.delete(key)
		}
		fmt.Fprintf(os.Stderr, "CNI identity state=%s endpoint=%q reason=%q\n", ack.State, key, ack.Reason)
	}
	return nil
}

func (s *CNIServer) runCNIVerification(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.verifyPendingCNI(step)
		cancel()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "CNI asynchronous verification blocked: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
