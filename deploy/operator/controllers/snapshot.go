package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errSnapshotChanged = errors.New("EvpnCore reconciliation snapshot changed")

// The active core's UID, generation, spec and deletion state never change.
// API responses may refresh its journal/status, but never its desired intent.
func (r *EvpnCoreReconciler) currentSnapshot(ctx context.Context, snapshot *evpnv1.EvpnCore) (*evpnv1.EvpnCore, error) {
	var current evpnv1.EvpnCore
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(snapshot), &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errSnapshotChanged
		}
		return nil, err
	}
	if current.UID != snapshot.UID || current.Generation != snapshot.Generation ||
		!reflect.DeepEqual(current.Spec, snapshot.Spec) ||
		current.DeletionTimestamp.IsZero() != snapshot.DeletionTimestamp.IsZero() {
		return nil, errSnapshotChanged
	}
	return &current, nil
}

func (r *EvpnCoreReconciler) editStatus(ctx context.Context, snapshot *evpnv1.EvpnCore, edit func(*evpnv1.EvpnCoreStatus) error) error {
	current, err := r.currentSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	base := current.DeepCopyObject().(*evpnv1.EvpnCore)
	if err := edit(&current.Status); err != nil {
		return err
	}
	if !reflect.DeepEqual(base.Status, current.Status) {
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, current, patch); err != nil {
			return fmt.Errorf("persist EvpnCore status: %w", err)
		}
	}
	// Deep-copy only status: a Patch response can contain a newer spec.
	snapshot.Status = current.DeepCopyObject().(*evpnv1.EvpnCore).Status
	return nil
}

func (r *EvpnCoreReconciler) editFinalizer(ctx context.Context, snapshot *evpnv1.EvpnCore, add bool) error {
	current, err := r.currentSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	base := current.DeepCopyObject().(*evpnv1.EvpnCore)
	if add {
		if contains(current.Finalizers, Finalizer) {
			return nil
		}
		current.Finalizers = append(current.Finalizers, Finalizer)
	} else {
		next := make([]string, 0, len(current.Finalizers))
		for _, f := range current.Finalizers {
			if f != Finalizer {
				next = append(next, f)
			}
		}
		current.Finalizers = next
	}
	return r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
