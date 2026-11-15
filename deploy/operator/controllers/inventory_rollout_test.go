package controllers

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestInventoryMigrationBlocksLegacyDaemonSet(t *testing.T) {
	ctx := context.Background()
	core, reservations, nodes := inventoryFixture()
	inventory := core.Spec.WorkerInventory
	core.Spec.WorkerInventory = nil
	r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	var old appsv1.DaemonSet
	key := client.ObjectKey{Namespace: "evpn-system", Name: "evpn-node-agent"}
	if err := r.Get(ctx, key, &old); err != nil {
		t.Fatal(err)
	}
	current := fetchCore(t, r, core)
	current.Spec.WorkerInventory = inventory
	current.Generation++
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, current)
	current = fetchCore(t, r, core)
	cond := meta.FindStatusCondition(current.Status.Conditions, "MigrationRequired")
	if current.Status.Phase != "Degraded" || cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("legacy migration not blocked: %+v", current.Status)
	}
	var after appsv1.DaemonSet
	if err := r.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.Spec.Template, after.Spec.Template) || old.UID != after.UID {
		t.Fatal("legacy workload was replaced or mutated")
	}
}

func TestInventoryPinAndArgumentsAreExact(t *testing.T) {
	core, _, _ := inventoryFixture()
	expected := inventoryArguments(core, "resource-uid")
	container := []corev1.Container{{Args: append([]string(nil), expected...)}}
	if !inventoryArgsMatch(container, expected) {
		t.Fatal("expected inventory arguments rejected")
	}

	container[0].Args[1] = "--worker-inventory-uid=other"
	if inventoryArgsMatch(container, expected) {
		t.Fatal("different ConfigMap UID accepted")
	}
	container[0].Args[1] = expected[0]
	if inventoryArgsMatch(container, expected) {
		t.Fatal("duplicate flag accepted")
	}
}

func TestInventoryRolloutReadyDoesNotClaimAcceptance(t *testing.T) {
	ctx := context.Background()
	core, reservations, nodes := inventoryFixture()
	r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	var ds appsv1.DaemonSet
	key := client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}
	if err := r.Get(ctx, key, &ds); err != nil {
		t.Fatal(err)
	}

	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	ds.Status.UpdatedNumberScheduled = 1
	ds.Status.NumberReady = 1
	ds.Status.NumberAvailable = 1
	if err := r.Status().Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if condition := meta.FindStatusCondition(current.Status.Conditions, "InventoryRolloutReady"); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("partial selected-worker rollout reported ready: %+v", condition)
	}
	if err := r.Get(ctx, key, &ds); err != nil {
		t.Fatal(err)
	}
	ds.Status.DesiredNumberScheduled = 2
	ds.Status.UpdatedNumberScheduled = 2
	ds.Status.NumberReady = 2
	ds.Status.NumberAvailable = 2
	if err := r.Status().Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	current = fetchCore(t, r, core)
	if current.Status.Ready || current.Status.Phase != "Reconciling" {
		t.Fatalf("readiness must not claim acceptance: %+v", current.Status)
	}
	for _, check := range []struct {
		name   string
		status metav1.ConditionStatus
	}{{"InventoryPublished", metav1.ConditionTrue}, {"InventoryRolloutReady", metav1.ConditionTrue},
		{"InventoryAccepted", metav1.ConditionUnknown}, {"RuntimeReady", metav1.ConditionUnknown}} {
		condition := meta.FindStatusCondition(current.Status.Conditions, check.name)
		if condition == nil || condition.Status != check.status {
			t.Fatalf("%s condition: %+v", check.name, condition)
		}
	}
}

func TestInventoryMigrationBlocksDeploymentWithoutPods(t *testing.T) {
	core, reservations, nodes := inventoryFixture()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "evpn-node-agent", Namespace: "old"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "evpn-node-agent"}},
		}}},
	}
	r := newTestReconciler(t, append([]client.Object{core, reservations, deployment}, nodes...)...)
	message, err := r.inventoryMigrationRequired(context.Background(), core, "inventory-uid")
	if err != nil || message == "" {
		t.Fatalf("legacy Deployment not blocked: %q, %v", message, err)
	}
}

func TestInventoryDisableBlockedAcrossNamespacesAndOrphanPods(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		core, reservations, nodes := inventoryFixture()
		podSpec := corev1.PodSpec{Containers: []corev1.Container{{
			Name: "evpn-node-agent", Args: inventoryArguments(core, "inventory-uid"),
		}}}
		var old client.Object = &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "evpn-node-agent", Namespace: "old"},
			Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: podSpec}},
		}
		if orphan {
			old = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "evpn-node-agent-old", Namespace: "old"}, Spec: podSpec}
		}
		r := newTestReconciler(t, append([]client.Object{core, reservations, old}, nodes...)...)
		blocked, err := r.rejectInventoryDisable(context.Background(), core)
		if err != nil || !blocked {
			t.Fatalf("orphan=%v: inventory disable not blocked: %v", orphan, err)
		}
	}
}

func TestInventoryReplacementDoesNotRollExistingDaemonSet(t *testing.T) {
	core, reservations, nodes := inventoryFixture()
	r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	var before appsv1.DaemonSet
	key := client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}
	if err := r.Get(context.Background(), key, &before); err != nil {
		t.Fatal(err)
	}
	message, err := r.inventoryMigrationRequired(context.Background(), fetchCore(t, r, core), "replacement-configmap-uid")
	if err != nil || message == "" {
		t.Fatalf("replacement UID not blocked: %q, %v", message, err)
	}
	var after appsv1.DaemonSet
	if err := r.Get(context.Background(), key, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Spec, after.Spec) {
		t.Fatal("existing DaemonSet changed")
	}
}
