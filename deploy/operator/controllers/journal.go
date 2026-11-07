package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const maxManagedResources = 256

var managedKinds = []struct{ version, kind string }{
	{"v1", "Service"}, {"v1", "ConfigMap"}, {"v1", "ServiceAccount"},
	{"apps/v1", "Deployment"}, {"apps/v1", "DaemonSet"},
}

func ownedJournalValid(core *evpnv1.EvpnCore, obj *unstructured.Unstructured) bool {
	if obj.GetAnnotations()[OwnerKey] != string(core.UID) {
		return false
	}
	var previous unstructured.Unstructured
	if err := json.Unmarshal([]byte(obj.GetAnnotations()[lastAppliedKey]), &previous.Object); err != nil {
		return false
	}
	return previous.GetAnnotations()[OwnerKey] == string(core.UID) &&
		sameResource(resourceRef(obj), resourceRef(&previous))
}

func ownershipProblem(core *evpnv1.EvpnCore, obj *unstructured.Unstructured) string {
	ref := resourceRef(obj)
	for _, old := range core.Status.ManagedResources {
		if sameResource(old, ref) && old.UID != "" && old.UID != ref.UID {
			return fmt.Sprintf("%s %s/%s has replacement UID %s; recorded UID is %s", ref.Kind, ref.Namespace, ref.Name, ref.UID, old.UID)
		}
	}
	if !ownedJournalValid(core, obj) {
		return fmt.Sprintf("%s %s/%s lacks verified installation ownership and last-applied journal; adoption/deletion is refused", ref.Kind, ref.Namespace, ref.Name)
	}
	return ""
}

func (r *EvpnCoreReconciler) recordConflict(ctx context.Context, core *evpnv1.EvpnCore, ref evpnv1.ManagedResource, message string) error {
	err := r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		for i, old := range status.ManagedResources {
			if sameResource(old, ref) {
				status.ManagedResources[i].Conflict = message
				return nil
			}
		}
		ref.Conflict = message
		status.ManagedResources = append(status.ManagedResources, ref)
		return nil
	})
	if err != nil {
		return err
	}
	return &ownershipConflict{message}
}

func (r *EvpnCoreReconciler) creationIntent(ctx context.Context, core *evpnv1.EvpnCore, ref evpnv1.ManagedResource) error {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetKind(ref.Kind)
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return &ownershipConflict{fmt.Sprintf("%s %s/%s appeared before recreation; retry ownership verification", ref.Kind, ref.Namespace, ref.Name)}
	}
	ref.UID, ref.State, ref.Conflict = "", "CreationPending", ""
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		for i, old := range status.ManagedResources {
			if sameResource(old, ref) {
				status.ManagedResources[i] = ref
				return nil
			}
		}
		status.ManagedResources = append(status.ManagedResources, ref)
		return nil
	})
}

// Cluster-wide, label-filtered discovery covers historical namespace placements,
// but only the five supported kinds. Oversized/truncated inventories fail closed.
func (r *EvpnCoreReconciler) discover(ctx context.Context, core *evpnv1.EvpnCore) error {
	if _, err := r.currentSnapshot(ctx, core); err != nil {
		return err
	}
	var discovered []evpnv1.ManagedResource
	for _, gvk := range managedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(gvk.version)
		list.SetKind(gvk.kind + "List")
		if err := r.reader().List(ctx, list, client.MatchingLabels{OwnerKey: string(core.UID)}, client.Limit(maxManagedResources+1)); err != nil {
			return err
		}
		if list.GetContinue() != "" || len(discovered)+len(list.Items) > maxManagedResources {
			return fmt.Errorf("installation discovery exceeds %d resources; administrator inventory required", maxManagedResources)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			ref := resourceRef(obj)
			if !cleanupAllowed(ref) {
				return fmt.Errorf("installation label on excluded resource %s %s/%s; administrator disposition required", ref.Kind, ref.Namespace, ref.Name)
			}
			ref.Conflict = ownershipProblem(core, obj)
			discovered = append(discovered, ref)
		}
	}
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		for _, ref := range discovered {
			found := false
			for i, old := range status.ManagedResources {
				if !sameResource(old, ref) {
					continue
				}
				found = true
				if old.UID != "" && old.UID != ref.UID {
					status.ManagedResources[i].Conflict = fmt.Sprintf("%s %s/%s UID changed from %s to %s", ref.Kind, ref.Namespace, ref.Name, old.UID, ref.UID)
				} else if ref.Conflict != "" {
					status.ManagedResources[i].Conflict = ref.Conflict
				} else {
					status.ManagedResources[i] = ref
				}
				break
			}
			if !found {
				status.ManagedResources = append(status.ManagedResources, ref)
			}
		}
		if len(status.ManagedResources) > maxManagedResources {
			return fmt.Errorf("installation journal exceeds %d entries", maxManagedResources)
		}
		return nil
	})
}
