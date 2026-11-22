package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	return s.reconcileCNI(ctx, true)
}

func (s *CNIServer) reconcileCNI(ctx context.Context, rescan bool) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.stopping || s.continuity == nil || ctx.Err() != nil {
		return nil
	}
	c := s.continuity
	if err := s.retryPendingOrphanLocked(ctx, c); err != nil {
		return fmt.Errorf("orphan intent pending retry: %w", err)
	}
	if !c.recovering {
		if err := c.ready(); err != nil {
			return err
		}
	}
	var scanErr error
	if c.recovering || rescan {
		scanErr = s.rescanCNIReceiptsLocked(ctx, c)
	}
	if c.recovering || ctx.Err() != nil {
		return scanErr
	}
	if err := c.ready(); err != nil {
		return errors.Join(scanErr, err)
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
	return scanErr
}

func (s *CNIServer) retryPendingOrphanLocked(ctx context.Context, c *cniContinuity) error {
	_, pending, err := c.journal.Snapshot()
	if err != nil || pending == nil || pending.Operation != "cni-orphan-del" {
		return err
	}
	var ack cniEndpointACK
	if err := decodeCNIState(pending.Payload, &ack); err != nil {
		return fmt.Errorf("%w: invalid orphan cleanup intent", ownership.ErrBlocked)
	}
	key := endpointKey(ack.Identity)
	current, ok := c.state.Endpoints[key]
	if !ok || current.State != "active" || !reflect.DeepEqual(current, ack) {
		return fmt.Errorf("%w: orphan cleanup intent does not match active ACK", ownership.ErrBlocked)
	}
	return s.cleanupOrphanLocked(ctx, c, key, ack, pending.Sequence)
}

func (s *CNIServer) runCNIVerification(ctx context.Context) {
	s.runCNIReconciliation(ctx, time.Second, 10*time.Second)
}

func (s *CNIServer) runCNIReconciliation(ctx context.Context, tick, scanInterval time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var nextScan, nextLog time.Time
	for {
		now := time.Now()
		rescan := !now.Before(nextScan)
		if rescan {
			nextScan = now.Add(scanInterval)
		}
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.reconcileCNI(step, rescan)
		cancel()
		if err == nil {
			nextLog = time.Time{}
		} else if ctx.Err() == nil && !time.Now().Before(nextLog) {
			fmt.Fprintf(os.Stderr, "CNI asynchronous reconciliation blocked; will retry: %v\n", err)
			nextLog = time.Now().Add(30 * time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
