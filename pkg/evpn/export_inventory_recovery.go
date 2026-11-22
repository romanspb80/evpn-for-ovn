package evpn

import (
	"context"
	"fmt"
	"sort"

	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
)

type ExportInventoryEvidence struct {
	Domain      string
	Incarnation string
	Ownership   string
	Ready       string
	LegacyFence bool
}

// InventoryEvidence is read only. The caller must still independently Recover
// against the current attested speaker before preparing any grant.
func (r *ExportRecovery) InventoryEvidence() (ExportInventoryEvidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, pending, err := r.journal.Snapshot(); err != nil || pending != nil {
		return ExportInventoryEvidence{}, fmt.Errorf("%w: unresolved export operation: %v", ownership.ErrBlocked, err)
	}
	return ExportInventoryEvidence{Domain: workerinventory.TrustDigest(r.domain), Incarnation: r.state.Incarnation,
		Ownership: inventoryOwnershipDigest(r.state.Receipts), Ready: r.state.InventoryReady,
		LegacyFence: r.state.Version == 1 && r.state.WithdrawalRequired}, nil
}

func inventoryOwnershipDigest(receipts []exportReceipt) string {
	canonical := append(make([]exportReceipt, 0, len(receipts)), receipts...)
	sort.Slice(canonical, func(i, j int) bool {
		return canonical[i].Owned.PathUUID < canonical[j].Owned.PathUUID
	})
	return workerinventory.TrustDigest(canonical)
}

// EnableInventory is an explicit schema conversion, performed only by an
// opted-in coordinator after it has recorded any legacy security fence.
func (r *ExportRecovery) EnableInventory() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.InventoryManaged {
		return nil
	}
	next := r.state
	next.Version, next.InventoryManaged, next.InventoryReady = 2, true, ""
	return r.inventoryCheckpoint(next)
}

func (r *ExportRecovery) inventoryCheckpoint(next exportRecoveryState) error {
	if _, pending, err := r.journal.Snapshot(); err != nil || pending != nil {
		return fmt.Errorf("%w: unresolved export operation: %v", ownership.ErrBlocked, err)
	}
	seq, err := r.journal.Begin("export-checkpoint", next)
	if err != nil {
		return err
	}
	if err := r.journal.Commit(seq, next); err != nil {
		return err
	}
	r.state = next
	return nil
}

// InventoryReady cannot introduce receipts or waive old-slot ambiguity.
// The coordinator must still commit the matching inventory FINAL record.
func (r *ExportRecovery) InventoryReady(ctx context.Context, binding string, requireEmpty bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.state.InventoryManaged || binding == "" {
		return fmt.Errorf("%w: inventory-managed journal and explicit binding required", ownership.ErrBlocked)
	}
	current, paths, err := r.observation(ctx)
	if err != nil {
		return err
	}
	if current != r.state.Incarnation || (requireEmpty && len(r.state.Receipts) != 0) {
		return fmt.Errorf("%w: old ownership must be reconciled before recovery grant", ownership.ErrBlocked)
	}
	for _, receipt := range r.state.Receipts {
		found := matchingExportPaths(paths, receipt)
		if len(found) != 1 || !ribPathMatchesEndpoint(found[0], receipt.Owned.Endpoint) {
			return fmt.Errorf("%w: ownership readback changed", ownership.ErrBlocked)
		}
	}
	next := r.state
	next.InventoryReady, next.WithdrawalRequired = binding, false
	return r.inventoryCheckpoint(next)
}
