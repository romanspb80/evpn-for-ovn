package controllers

import (
	"context"
	"fmt"
	"strings"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inventoryJournal = "/var/lib/evpn-node-agent/inventory"

func (r *EvpnCoreReconciler) invalidateInventoryConditions(ctx context.Context, core *evpnv1.EvpnCore) error {
	if core.Spec.WorkerInventory == nil {
		return nil
	}
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		for _, name := range []string{"InventoryPublished", "InventoryRolloutReady", "InventoryAccepted", "RuntimeReady", "MigrationRequired"} {
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: name, Status: metav1.ConditionUnknown, ObservedGeneration: core.Generation,
				Reason: "ReconciliationIncomplete", Message: "current inventory reconciliation did not complete",
			})
		}
		return nil
	})
}

func (r *EvpnCoreReconciler) setInventoryConditions(ctx context.Context, core *evpnv1.EvpnCore, published, migration, rollout bool) error {
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		values := []metav1.Condition{
			{Type: "InventoryPublished", Status: metav1.ConditionFalse, Reason: "PublicationPending", Message: "inventory has not been published"},
			{Type: "InventoryAccepted", Status: metav1.ConditionUnknown, Reason: "AcceptanceUnconfirmed", Message: "no authenticated per-worker acceptance acknowledgement is available"},
			{Type: "RuntimeReady", Status: metav1.ConditionUnknown, Reason: "RuntimeHealthUnconfirmed", Message: "DaemonSet readiness does not establish Type-2 runtime health"},
			{Type: "MigrationRequired", Status: metav1.ConditionFalse, Reason: "NoLegacyWorkload", Message: "no ungated workload detected"},
			{Type: "InventoryRolloutReady", Status: metav1.ConditionFalse, Reason: "WorkloadsPending", Message: "inventory-gated workload rollout is incomplete"},
		}
		if published {
			values[0].Status, values[0].Reason, values[0].Message = metav1.ConditionTrue, "InventoryPublished", "inventory publication is owned by this installation"
		}
		if migration {
			values[3].Status, values[3].Reason, values[3].Message = metav1.ConditionTrue, "MigrationRequired", "existing workload requires controlled administrative transition"
		}
		if rollout {
			values[4].Status, values[4].Reason, values[4].Message = metav1.ConditionTrue, "WorkloadsAvailable", "DaemonSet reports its rollout available; acceptance remains unconfirmed"
		}
		for _, condition := range values {
			condition.ObservedGeneration = core.Generation
			meta.SetStatusCondition(&status.Conditions, condition)
		}
		return nil
	})
}

func inventoryArguments(core *evpnv1.EvpnCore, uid string) []string {
	ns, _ := namespaces(core.Spec)
	return []string{"--worker-inventory-namespace=" + ns, "--worker-inventory-uid=" + uid,
		"--worker-inventory-cluster-uid=" + core.Spec.WorkerInventory.ClusterUID,
		"--worker-inventory-installation-uid=" + string(core.UID),
		"--worker-inventory-journal=" + inventoryJournal}
}

func wireInventoryDaemonSet(objects []*unstructured.Unstructured, core *evpnv1.EvpnCore, uid string) error {
	if uid == "" {
		return fmt.Errorf("inventory publication has no observed Kubernetes UID")
	}
	found := false
	for _, obj := range objects {
		if obj.GetKind() != "DaemonSet" || obj.GetName() != "evpn-node-agent" {
			continue
		}
		var ds appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &ds); err != nil {
			return err
		}
		if found || !inventoryPodIdentityMatches(ds.Spec.Template.Spec) {
			return fmt.Errorf("inventory requires exactly one Node-Agent workload and container")
		}
		found = true
		ds.Spec.Template.Spec.Containers[0].Args = append(ds.Spec.Template.Spec.Containers[0].Args, inventoryArguments(core, uid)...)
		pod, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ds.Spec.Template.Spec)
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(obj.Object, pod, "spec", "template", "spec"); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("inventory requires a rendered Node-Agent DaemonSet")
	}
	return nil
}

func inventoryPodIdentityMatches(pod corev1.PodSpec) bool {
	if len(pod.Containers) != 1 || len(pod.Containers[0].Command) != 1 ||
		pod.Containers[0].Command[0] != "/usr/local/bin/evpn-node-agent" ||
		pod.ServiceAccountName != "evpn-provider-observer" ||
		pod.AutomountServiceAccountToken == nil || !*pod.AutomountServiceAccountToken {
		return false
	}
	foundNode := false
	for _, env := range pod.Containers[0].Env {
		if env.Name == "NODE_NAME" {
			if foundNode || env.Value != "" || env.ValueFrom == nil || env.ValueFrom.FieldRef == nil ||
				env.ValueFrom.FieldRef.FieldPath != "spec.nodeName" {
				return false
			}
			foundNode = true
		}
	}
	return foundNode
}

// Exact argument matching refuses an ungated workload or a changed UID pin;
// updating either would restart agents without proof that old ownership drained.
func inventoryArgsMatch(containers []corev1.Container, expected []string) bool {
	if len(containers) != 1 {
		return false
	}
	seen := map[string]bool{}
	for _, arg := range containers[0].Args {
		if strings.HasPrefix(arg, "--worker-inventory-") {
			match := false
			for _, wanted := range expected {
				if arg == wanted {
					match = true
				}
			}
			if !match || seen[arg] {
				return false
			}
			seen[arg] = true
		}
	}
	return len(seen) == len(expected)
}

func (r *EvpnCoreReconciler) inventoryMigrationRequired(ctx context.Context, core *evpnv1.EvpnCore, uid string) (string, error) {
	expected := inventoryArguments(core, uid)
	ns, _ := namespaces(core.Spec)
	var deployments appsv1.DeploymentList
	if err := r.reader().List(ctx, &deployments); err != nil {
		return "", err
	}
	for _, deployment := range deployments.Items {
		if nodeAgentPod(deployment.Name, deployment.Spec.Template) {
			return fmt.Sprintf("existing Node-Agent Deployment %s/%s requires controlled administrative transition", deployment.Namespace, deployment.Name), nil
		}
	}
	var sets appsv1.DaemonSetList
	if err := r.reader().List(ctx, &sets); err != nil {
		return "", err
	}
	var selectedUID string
	for _, ds := range sets.Items {
		if !nodeAgentPod(ds.Name, ds.Spec.Template) {
			continue
		}
		if ds.Namespace != ns || ds.Name != "evpn-node-agent" || ds.Annotations[OwnerKey] != string(core.UID) ||
			ds.UID == "" || !ds.DeletionTimestamp.IsZero() || !inventoryPodIdentityMatches(ds.Spec.Template.Spec) ||
			!inventoryArgsMatch(ds.Spec.Template.Spec.Containers, expected) {
			return fmt.Sprintf("existing Node-Agent DaemonSet %s/%s is not bound to this inventory; controlled administrative transition required", ds.Namespace, ds.Name), nil
		}
		selectedUID = string(ds.UID)
	}
	if selectedUID == "" {
		for _, ref := range core.Status.ManagedResources {
			if ref.Kind == "DaemonSet" && ref.Name == "evpn-node-agent" && ref.UID != "" {
				return "a previous Node-Agent workload is recorded; verify and complete controlled ownership cleanup before a new installation", nil
			}
		}
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods); err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if !nodeAgentPod(pod.Name, corev1.PodTemplateSpec{ObjectMeta: pod.ObjectMeta, Spec: pod.Spec}) {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		owned := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" && string(owner.UID) == selectedUID && selectedUID != "" {
				owned = true
			}
		}
		if !owned || !inventoryPodIdentityMatches(pod.Spec) || !inventoryArgsMatch(pod.Spec.Containers, expected) {
			return fmt.Sprintf("existing Node-Agent Pod %s/%s is not bound to this inventory; controlled administrative transition required", pod.Namespace, pod.Name), nil
		}
	}
	return "", nil
}

func (r *EvpnCoreReconciler) publishedInventoryUID(ctx context.Context, core *evpnv1.EvpnCore) (string, error) {
	ns, _ := namespaces(core.Spec)
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: workerInventoryName}, obj); err != nil {
		return "", err
	}
	if message := ownershipProblem(core, obj); message != "" {
		return "", &ownershipConflict{message}
	}
	if obj.GetUID() == "" || !obj.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("inventory ConfigMap UID is unavailable or resource is deleting")
	}
	return string(obj.GetUID()), nil
}

func (r *EvpnCoreReconciler) rejectInventoryDisable(ctx context.Context, core *evpnv1.EvpnCore) (bool, error) {
	var sets appsv1.DaemonSetList
	if err := r.reader().List(ctx, &sets); err != nil {
		return false, err
	}
	hasGate := func(containers []corev1.Container) bool {
		for _, c := range containers {
			for _, arg := range c.Args {
				if strings.HasPrefix(arg, "--worker-inventory-") {
					return true
				}
			}
		}
		return false
	}
	for _, ds := range sets.Items {
		if nodeAgentPod(ds.Name, ds.Spec.Template) && hasGate(ds.Spec.Template.Spec.Containers) {
			return true, nil
		}
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods); err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed &&
			nodeAgentPod(pod.Name, corev1.PodTemplateSpec{ObjectMeta: pod.ObjectMeta, Spec: pod.Spec}) && hasGate(pod.Spec.Containers) {
			return true, nil
		}
	}
	return false, nil
}
