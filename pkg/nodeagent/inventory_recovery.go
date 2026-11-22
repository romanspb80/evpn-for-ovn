package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type inventoryFailure struct {
	Cause    string
	Security bool
	Err      error
}

func (e *inventoryFailure) Error() string { return e.Cause + ": " + e.Err.Error() }
func (e *inventoryFailure) Unwrap() error { return e.Err }

func inventoryFailureClass(err error) (string, bool) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		cause := ""
		for _, child := range joined.Unwrap() {
			next, denied := inventoryFailureClass(child)
			if denied {
				return next, true
			}
			if cause == "" {
				cause = next
			}
		}
		if cause != "" {
			return cause, false
		}
	}
	if _, classified := err.(*inventoryFailure); !classified && errors.Unwrap(err) != nil {
		return inventoryFailureClass(errors.Unwrap(err))
	}
	var failure *inventoryFailure
	if errors.As(err, &failure) {
		return failure.Cause, failure.Security
	}
	return "unclassified-trust-failure", true
}

func inventoryObservationCanceled(err error) bool {
	cause, security := inventoryFailureClass(err)
	return !security && cause == "observation-canceled"
}

func loadInventoryRecoveryRequest(path string) (*workerinventory.RecoveryRequest, error) {
	f, err := openAdministratorFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&07777 != 0600 || st.Nlink != 1 || st.Size > 65536 {
		return nil, fmt.Errorf("recovery request requires a bounded root-owned single-link mode-0600 file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(data) > 65536 {
		return nil, fmt.Errorf("recovery request exceeds size limit")
	}
	if err := validateRecoveryRequestKeys(data); err != nil {
		return nil, err
	}
	var request *workerinventory.RecoveryRequest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, fmt.Errorf("recovery request must be an object")
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return nil, fmt.Errorf("recovery request contains trailing data")
	}
	return request, nil
}

// Recovery grants have only two object levels. Reject duplicate keys and case
// aliases before encoding/json can silently choose one privileged instruction.
func validateRecoveryRequestKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var object func(bool) error
	object = func(binding bool) error {
		token, err := d.Token()
		if err != nil || token != json.Delim('{') {
			return fmt.Errorf("recovery request requires an object")
		}
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate or invalid recovery request key %q", key)
			}
			seen[key] = true
			valid := false
			if binding {
				switch key {
				case "Transaction", "Publication", "ClusterUID", "InstallationUID", "NodeName", "NodeUID",
					"Revision", "Digest", "WorkerDigest", "Incident", "Denial", "Generation", "Domain", "Incarnation", "Ownership":
					valid = true
				}
			} else {
				switch key {
				case "Version", "Action", "ID", "Reason", "Binding":
					valid = true
				}
			}
			if !valid {
				return fmt.Errorf("unknown recovery request key %q", key)
			}
			if !binding && key == "Binding" {
				if err := object(true); err != nil {
					return err
				}
				continue
			}
			token, err = d.Token()
			if err != nil {
				return err
			}
			if _, nested := token.(json.Delim); nested {
				return fmt.Errorf("recovery request field %q must be scalar", key)
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('}') {
			return fmt.Errorf("unterminated recovery request")
		}
		return nil
	}
	if err := object(false); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("recovery request contains trailing data")
	}
	return nil
}

type inventoryRecoveryApplier struct {
	gate     *InventoryGate
	recovery *evpn.ExportRecovery
	observer evpn.EndpointObserver
}

func (a inventoryRecoveryApplier) Add(ctx context.Context, route evpn.Type2Route) (string, error) {
	g := a.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkLocked(ctx, nil, nil); err != nil {
		return "", err
	}
	if a.observer == nil {
		return "", fmt.Errorf("current endpoint observer required")
	}
	endpoints, err := a.observer.DesiredLocalEndpoints(ctx)
	if err != nil {
		return "", err
	}
	authorized := false
	for _, endpoint := range endpoints {
		if endpoint.Equivalent(route.Endpoint) {
			authorized = true
			break
		}
	}
	if !authorized {
		return "", fmt.Errorf("%w: endpoint no longer authorized", ownership.ErrBlocked)
	}
	s, err := g.fence.Trust()
	if err != nil || s.Phase != workerinventory.TrustFinal || s.Binding == nil {
		return "", fmt.Errorf("%w: durable inventory FINAL required: %v", ownership.ErrBlocked, err)
	}
	e, err := a.recovery.InventoryEvidence()
	if err != nil {
		return "", err
	}
	target, err := g.fence.RecoveryTarget(e.Domain, e.Incarnation, e.Ownership)
	if err != nil {
		return "", err
	}
	b := *s.Binding
	if b.Publication != target.Publication || b.Revision != target.Revision || b.Digest != target.Digest ||
		b.WorkerDigest != target.WorkerDigest || b.Denial != target.Denial || b.Domain != e.Domain ||
		b.Incarnation != e.Incarnation || workerinventory.TrustDigest(b) != e.Ready {
		return "", fmt.Errorf("%w: inventory/export grant binding changed", ownership.ErrBlocked)
	}
	return a.recovery.AddInventory(ctx, route, e.Ready)
}

func (a inventoryRecoveryApplier) Delete(ctx context.Context, route evpn.OwnedType2Route) error {
	return a.recovery.Delete(ctx, route)
}

func (g *InventoryGate) RecordFailure(failure error, recovery *evpn.ExportRecovery) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked != nil {
		return g.blocked
	}
	if inventoryObservationCanceled(failure) {
		return nil
	}
	cause, security := inventoryFailureClass(failure)
	if err := g.fence.Deny(cause, security, failure.Error()); err != nil {
		g.blocked = err
		return err
	}
	if recovery != nil {
		e, err := recovery.InventoryEvidence()
		if err != nil {
			return err
		}
		if e.LegacyFence {
			if err := g.fence.Deny("LEGACY_UNCLASSIFIED", true); err != nil {
				return err
			}
		}
		return recovery.EnableInventory()
	}
	return nil
}

func (g *InventoryGate) RecoveryStatus(recovery *evpn.ExportRecovery) (*workerinventory.TrustState, *workerinventory.TrustBinding) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, err := g.fence.Trust()
	if err != nil {
		return nil, nil
	}
	if recovery == nil {
		return &s, nil
	}
	e, err := recovery.InventoryEvidence()
	if err != nil {
		return &s, nil
	}
	b, err := g.fence.RecoveryTarget(e.Domain, e.Incarnation, e.Ownership)
	if err != nil {
		return &s, nil
	}
	return &s, &b
}

// CoordinateRecovery holds the same gate lock used by importer observations
// and each AddPath decision. Lock order is gate -> exporter -> export journal;
// export recovery never calls back into the gate.
func (g *InventoryGate) CoordinateRecovery(ctx context.Context, exporter *evpn.EndpointExporter, recovery *evpn.ExportRecovery) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkLocked(ctx, nil, nil); err != nil {
		return err
	}
	if _, err := recovery.Recover(ctx); err != nil {
		cause, security := "ownership-reconciliation-blocked", true
		var classified *inventoryFailure
		if errors.As(err, &classified) {
			cause, security = classified.Cause, classified.Security
		} else if errors.Is(ctx.Err(), context.Canceled) &&
			(errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled) {
			return &inventoryFailure{Cause: "observation-canceled", Err: err}
		}
		if cause == "observation-canceled" && !security {
			return err
		}
		return errors.Join(err, g.fence.Deny(cause, security, err.Error()))
	}
	e, err := recovery.InventoryEvidence()
	if err != nil {
		return err
	}
	if e.LegacyFence {
		if err := g.fence.Deny("LEGACY_UNCLASSIFIED", true); err != nil {
			return err
		}
	}
	if err := recovery.EnableInventory(); err != nil {
		return err
	}
	s, err := g.fence.Trust()
	if err != nil {
		return err
	}
	if s.Phase != workerinventory.TrustFinal && s.Phase != workerinventory.TrustAuthorized {
		if err := recovery.RequireWithdrawal(); err != nil {
			return err
		}
		if _, err := exporter.WithdrawAll(ctx); err != nil {
			return err
		}
		e, err = recovery.InventoryEvidence()
		if err != nil {
			return err
		}
	}
	target, err := g.fence.RecoveryTarget(e.Domain, e.Incarnation, e.Ownership)
	if err != nil {
		return err
	}
	if s.Phase == workerinventory.TrustFinal && s.Binding != nil &&
		s.Binding.Revision == target.Revision && s.Binding.Digest == target.Digest &&
		s.Binding.Incarnation == e.Incarnation && s.Binding.Domain == e.Domain {
		if e.Ready != workerinventory.TrustDigest(*s.Binding) {
			return errors.Join(fmt.Errorf("%w: inventory FINAL and export READY disagree", ownership.ErrBlocked),
				g.fence.Deny("inventory-export-grant-mismatch", true))
		}
		return nil
	}
	if s.Phase == workerinventory.TrustFinal && s.Binding != nil && s.Binding.Incarnation != e.Incarnation {
		if err := g.fence.Deny("verified-speaker-replacement", false); err != nil {
			return err
		}
		s, err = g.fence.Trust()
		if err != nil {
			return err
		}
	}
	var request *workerinventory.RecoveryRequest
	if s.Security && s.Phase != workerinventory.TrustFinal {
		request, err = loadInventoryRecoveryRequest(g.options.RecoveryFile)
		if err != nil {
			return fmt.Errorf("administrative recovery request rejected: %w", err)
		}
	}
	binding, err := g.fence.Prepare(e.Domain, e.Incarnation, e.Ownership, request)
	if err != nil {
		return err
	}
	if s.Denial != 0 && s.Phase != workerinventory.TrustFinal {
		if err := recovery.RequireWithdrawal(); err != nil {
			return err
		}
		if _, err := exporter.WithdrawAll(ctx); err != nil {
			return err
		}
	}
	if err := g.checkLocked(ctx, nil, nil); err != nil {
		return err
	}
	if err := recovery.InventoryReady(ctx, workerinventory.TrustDigest(binding), s.Denial != 0 && s.Phase != workerinventory.TrustFinal); err != nil {
		return err
	}
	return g.fence.Finalize(binding)
}
