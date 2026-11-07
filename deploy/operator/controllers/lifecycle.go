package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *EvpnCoreReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// The non-expiring lease is an installation lock, not a liveness lease.
// Only completed cleanup releases it; operator downtime never transfers ownership.
func (r *EvpnCoreReconciler) claimLock(ctx context.Context, core *evpnv1.EvpnCore) error {
	var cores evpnv1.EvpnCoreList
	if err := r.reader().List(ctx, &cores); err != nil {
		return err
	}
	var lock coordv1.Lease
	key := types.NamespacedName{Namespace: LockNamespace, Name: LockName}
	err := r.reader().Get(ctx, key, &lock)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if lock.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return &ownershipConflict{"installation lock is not managed by this operator"}
		}
		if lock.Spec.HolderIdentity != nil && *lock.Spec.HolderIdentity != "" {
			if *lock.Spec.HolderIdentity == string(core.UID) {
				return nil
			}
			return &ownershipConflict{fmt.Sprintf("singleton installation held by EvpnCore UID %s; delete that installation and complete cleanup before installing another", *lock.Spec.HolderIdentity)}
		}
	}
	sort.Slice(cores.Items, func(i, j int) bool {
		a, b := cores.Items[i], cores.Items[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	for _, item := range cores.Items {
		if !item.Spec.Enabled && !contains(item.Finalizers, Finalizer) {
			continue
		}
		if !contains(item.Finalizers, Finalizer) && validateSpec(item.Spec) != nil {
			continue
		}
		if item.UID != core.UID {
			return &ownershipConflict{fmt.Sprintf("singleton candidate is %s/%s; only the oldest enabled installation may acquire the lock", item.Namespace, item.Name)}
		}
		break
	}
	if core.UID == "" {
		return fmt.Errorf("EvpnCore UID is required for ownership")
	}
	// Persist the finalizer before obtaining the lock, closing the delete/claim race.
	if !contains(core.Finalizers, Finalizer) {
		if err := r.editFinalizer(ctx, core, true); err != nil {
			return err
		}
	}
	if _, checkErr := r.currentSnapshot(ctx, core); checkErr != nil {
		return checkErr
	}
	uid := string(core.UID)
	if apierrors.IsNotFound(err) {
		lock = coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LockName, Namespace: LockNamespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": fieldManager}},
			Spec: coordv1.LeaseSpec{HolderIdentity: &uid}}
		return r.Create(ctx, &lock)
	}
	lock.Spec.HolderIdentity = &uid
	return r.Update(ctx, &lock)
}

func (r *EvpnCoreReconciler) releaseLock(ctx context.Context, core *evpnv1.EvpnCore) error {
	if _, err := r.currentSnapshot(ctx, core); err != nil {
		return err
	}
	var lock coordv1.Lease
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: LockNamespace, Name: LockName}, &lock); err != nil {
		return client.IgnoreNotFound(err)
	}
	if lock.Labels["app.kubernetes.io/managed-by"] != fieldManager ||
		lock.Spec.HolderIdentity == nil || *lock.Spec.HolderIdentity != string(core.UID) {
		return nil
	}
	empty := ""
	lock.Spec.HolderIdentity = &empty
	return r.Update(ctx, &lock)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (r *EvpnCoreReconciler) ensureNamespace(ctx context.Context, name string) error {
	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: name}, &ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	err = r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func resourceRef(obj *unstructured.Unstructured) evpnv1.ManagedResource {
	return evpnv1.ManagedResource{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()), State: "Observed"}
}

func sameResource(a, b evpnv1.ManagedResource) bool {
	return a.APIVersion == b.APIVersion && a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

func (r *EvpnCoreReconciler) remember(ctx context.Context, core *evpnv1.EvpnCore, ref evpnv1.ManagedResource) error {
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		for i, old := range status.ManagedResources {
			if sameResource(old, ref) {
				if old.UID != "" && ref.UID != "" && old.UID != ref.UID {
					return &ownershipConflict{fmt.Sprintf("%s %s/%s journal UID %s differs from live UID %s", ref.Kind, ref.Namespace, ref.Name, old.UID, ref.UID)}
				}
				if ref.UID != "" {
					status.ManagedResources[i] = ref
				}
				return nil
			}
		}
		status.ManagedResources = append(status.ManagedResources, ref)
		return nil
	})
}

func (r *EvpnCoreReconciler) applyResource(ctx context.Context, core *evpnv1.EvpnCore, obj *unstructured.Unstructured) error {
	// Do not mutate the rendered desired-resource snapshot.
	obj = obj.DeepCopy()
	// Journal intent before creation so a crash cannot strand an untracked child.
	intent := resourceRef(obj)
	intent.State = "CreationPending"
	if err := r.remember(ctx, core, intent); err != nil {
		return err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(obj), current)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if message := ownershipProblem(core, current); message != "" {
			return r.recordConflict(ctx, core, resourceRef(obj), message)
		}
		if !current.GetDeletionTimestamp().IsZero() {
			return fmt.Errorf("%s %s is still deleting", obj.GetKind(), obj.GetName())
		}
		if err := preserveRuntimeEnvOverrides(obj, current); err != nil {
			return err
		}
		if err := applySpecOverrides(obj, core.Spec); err != nil {
			return err
		}
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[OwnerKey] = string(core.UID)
	obj.SetAnnotations(annotations)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[OwnerKey] = string(core.UID)
	labels["app.kubernetes.io/managed-by"] = fieldManager
	obj.SetLabels(labels)
	desired, marshalErr := json.Marshal(obj.Object)
	if marshalErr != nil {
		return marshalErr
	}
	if err == nil && current.GetAnnotations()[lastAppliedKey] == string(desired) && controlledFieldsEqual(obj.Object, current.Object) {
		return r.remember(ctx, core, resourceRef(current))
	}
	annotations[lastAppliedKey] = string(desired)
	obj.SetAnnotations(annotations)
	if apierrors.IsNotFound(err) {
		// Confirm absence and persist a new creation intent before recreating.
		if err := r.creationIntent(ctx, core, resourceRef(obj)); err != nil {
			return err
		}
		if _, err := r.currentSnapshot(ctx, core); err != nil {
			return err
		}
		if err := r.Create(ctx, obj, client.FieldOwner(fieldManager)); err != nil {
			return err
		}
		return r.remember(ctx, core, resourceRef(obj))
	}
	previous := current.GetAnnotations()[lastAppliedKey]
	if previous == "" {
		return fmt.Errorf("owned %s %s has no last-applied journal; refusing unsafe update", obj.GetKind(), obj.GetName())
	}
	next, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	live, err := json.Marshal(current.Object)
	if err != nil {
		return err
	}
	patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch([]byte(previous), next, live)
	if err != nil {
		return fmt.Errorf("compute patch for %s: %w", obj.GetName(), err)
	}
	if string(patch) != "{}" {
		// Include resourceVersion to prevent an ownership change between read and patch.
		var fields map[string]interface{}
		if err := json.Unmarshal(patch, &fields); err != nil {
			return err
		}
		metadata, _ := fields["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
		metadata["resourceVersion"] = current.GetResourceVersion()
		fields["metadata"] = metadata
		patch, err = json.Marshal(fields)
		if err != nil {
			return err
		}
		if _, err := r.currentSnapshot(ctx, core); err != nil {
			return err
		}
		if err := r.Patch(ctx, current, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return err
		}
	}
	return r.remember(ctx, core, resourceRef(current))
}

// Ignore API-server defaults in fields absent from the YAML, while detecting
// drift in every field the installation actually declares.
func controlledFieldsEqual(desired, live interface{}) bool {
	switch d := desired.(type) {
	case map[string]interface{}:
		l, ok := live.(map[string]interface{})
		if !ok {
			return false
		}
		for key, value := range d {
			if !controlledFieldsEqual(value, l[key]) {
				return false
			}
		}
		return true
	case []interface{}:
		l, ok := live.([]interface{})
		if !ok || len(d) != len(l) {
			return false
		}
		for i := range d {
			if !controlledFieldsEqual(d[i], l[i]) {
				return false
			}
		}
		return true
	default:
		a, err := json.Marshal(desired)
		if err != nil {
			return false
		}
		b, err := json.Marshal(live)
		return err == nil && bytes.Equal(a, b)
	}
}

func (r *EvpnCoreReconciler) cleanup(ctx context.Context, core *evpnv1.EvpnCore, desired []*unstructured.Unstructured) (bool, error) {
	if err := r.discover(ctx, core); err != nil {
		return false, err
	}
	expectedInventory := core.DeepCopyObject().(*evpnv1.EvpnCore).Status.ManagedResources
	remaining := make([]evpnv1.ManagedResource, 0, len(core.Status.ManagedResources))
	var deletes []*unstructured.Unstructured
	var conflicts []error
	for _, ref := range core.Status.ManagedResources {
		keep := false
		for _, obj := range desired {
			if sameResource(ref, resourceRef(obj)) {
				keep = true
				break
			}
		}
		// Never include namespaces, upstream speaker objects or arbitrary GVKs.
		if !cleanupAllowed(ref) {
			return false, fmt.Errorf("unsupported cleanup inventory entry: %s %s/%s", ref.Kind, ref.Namespace, ref.Name)
		}
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if message := ownershipProblem(core, obj); message != "" {
			ref.Conflict = message
			remaining = append(remaining, ref)
			conflicts = append(conflicts, &ownershipConflict{message})
			r.event(core, "CleanupOwnershipChanged", message)
			continue
		}
		ref.Conflict = ""
		remaining = append(remaining, ref)
		if !keep {
			deletes = append(deletes, obj)
		}
	}
	if err := r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		if !reflect.DeepEqual(status.ManagedResources, expectedInventory) {
			return apierrors.NewConflict(evpnv1.SchemeGroupVersion.WithResource("evpncores").GroupResource(), core.Name, fmt.Errorf("inventory changed during cleanup planning"))
		}
		status.ManagedResources = remaining
		return nil
	}); err != nil {
		return false, err
	}
	if err := errors.Join(conflicts...); err != nil {
		return false, err
	}
	for _, obj := range deletes {
		if _, err := r.currentSnapshot(ctx, core); err != nil {
			return false, err
		}
		if !obj.GetDeletionTimestamp().IsZero() {
			continue
		}
		uid, rv := obj.GetUID(), obj.GetResourceVersion()
		policy := metav1.DeletePropagationForeground
		if err := r.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &policy,
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return len(deletes) == 0, nil
}

func cleanupAllowed(ref evpnv1.ManagedResource) bool {
	switch ref.APIVersion + "/" + ref.Kind {
	case "v1/Service", "v1/ConfigMap", "v1/ServiceAccount", "apps/v1/Deployment", "apps/v1/DaemonSet":
		return ref.Namespace != "k8gobgp-system" && ref.Name != "k8gobgp"
	default:
		return false
	}
}

func (r *EvpnCoreReconciler) checkNodeAgentConflict(ctx context.Context, core *evpnv1.EvpnCore) error {
	if !core.Spec.NodeAgent.Enabled {
		return nil
	}
	var agents appsv1.DaemonSetList
	if err := r.reader().List(ctx, &agents); err != nil {
		return err
	}
	for _, ds := range agents.Items {
		if ds.Annotations[OwnerKey] == string(core.UID) {
			continue
		}
		if nodeAgentPod(ds.Name, ds.Spec.Template) {
			return &ownershipConflict{fmt.Sprintf("independent Node-Agent DaemonSet %s/%s exists; remove it before enabling operator management", ds.Namespace, ds.Name)}
		}
	}
	var deployments appsv1.DeploymentList
	if err := r.reader().List(ctx, &deployments); err != nil {
		return err
	}
	for _, d := range deployments.Items {
		if d.Annotations[OwnerKey] != string(core.UID) && nodeAgentPod(d.Name, d.Spec.Template) {
			return &ownershipConflict{fmt.Sprintf("independent Node-Agent Deployment %s/%s exists; remove it before enabling operator management", d.Namespace, d.Name)}
		}
	}
	return nil
}

func nodeAgentPod(name string, pod corev1.PodTemplateSpec) bool {
	for _, value := range []string{name, pod.Labels["app"], pod.Labels["app.kubernetes.io/name"]} {
		if value == "node-agent" || value == "framework-node-agent" || value == "evpn-node-agent" {
			return true
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, command := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(command, "evpn-node-agent") {
				return true
			}
		}
	}
	return false
}
