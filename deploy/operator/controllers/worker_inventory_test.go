package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inventoryCluster = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

func inventoryFixture() (*evpnv1.EvpnCore, *corev1.ConfigMap, []client.Object) {
	core := externalCore()
	core.UID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	core.Spec.NodeAgent.ExportConfigPath = "/run/evpn-node-agent/export.json"
	core.Spec.NodeAgent.RemoteConfigPath = "/run/evpn-node-agent/import.json"
	core.Spec.NodeAgent.CNIContinuityConfigPath = "/run/evpn-node-agent/cni.json"
	core.Spec.NodeAgent.Type2JournalHostPath = "/var/lib/evpn-worker"
	core.Spec.NodeAgent.ProviderAttestation = true
	a := workerinventory.Worker{NodeName: "worker-a", NodeUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Chassis: "chassis-a", SpeakerRouterID: "192.0.2.11", SpeakerASN: 64512,
		SpeakerEndpoint: "unix:///var/run/gobgp/api.sock", LocalVTEP: "192.0.2.21",
		AttestationPath: "/etc/evpn/provider.json", AuthorizationPath: "/run/evpn-node-agent/cni.json",
		ExportJournal: "/var/lib/evpn/export", ImportJournal: "/var/lib/evpn/import", ContinuityJournal: "/var/lib/evpn/cni",
		Networks: []workerinventory.Network{{ID: "tenant-a", LogicalSwitch: "tenant-a", VNI: 100}}}
	b := a
	b.NodeName, b.NodeUID, b.Chassis = "worker-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "chassis-b"
	b.SpeakerRouterID, b.LocalVTEP = "192.0.2.12", "192.0.2.22"
	core.Spec.WorkerInventory = &evpnv1.WorkerInventorySpec{ClusterUID: inventoryCluster, Revision: 1, Workers: []workerinventory.Worker{a, b}}
	reservations := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: reservationsName, Namespace: LockNamespace,
		Labels: map[string]string{"app.kubernetes.io/managed-by": fieldManager}},
		Data: map[string]string{"reservations.json": `{"version":1,"clusterUID":"` + inventoryCluster + `","reservations":[],"heads":{}}`}}
	nodes := []client.Object{}
	for _, w := range []workerinventory.Worker{a, b} {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.NodeName, UID: types.UID(w.NodeUID),
			Labels: map[string]string{"evpn.example.com/eligible": "true"}}})
	}
	return core, reservations, nodes
}

func TestWorkerInventoryPublicationAndRetention(t *testing.T) {
	ctx := context.Background()
	core, reservations, nodes := inventoryFixture()
	objects := append([]client.Object{core, reservations}, nodes...)
	r := newTestReconciler(t, objects...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if current.Status.Phase != "Reconciling" || !strings.Contains(current.Status.Reason, "unconfirmed") {
		t.Fatalf("inventory must not claim runtime acceptance: %+v", current.Status)
	}
	var published corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: "evpn-system", Name: workerInventoryName}, &published); err != nil {
		t.Fatal(err)
	}
	var in workerinventory.Inventory
	if err := json.Unmarshal([]byte(published.Data["inventory.json"]), &in); err != nil {
		t.Fatal(err)
	}
	data, digest, err := in.Canonical()
	if err != nil || string(data) != published.Data["inventory.json"] || digest != published.Data["sha256"] ||
		in.InstallationUID != string(core.UID) || published.Data["revision"] != "1" {
		t.Fatalf("invalid publication: %+v, %v", published.Data, err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: "evpn-system", Name: "evpn-node-agent"}, &ds); err != nil {
		t.Fatal(err)
	}
	if !inventoryArgsMatch(ds.Spec.Template.Spec.Containers, inventoryArguments(core, string(published.UID))) ||
		ds.Spec.Template.Spec.ServiceAccountName != "evpn-provider-observer" {
		t.Fatalf("Node-Agent is not inventory-gated: %+v", ds.Spec.Template.Spec)
	}
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil || !*ds.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("inventory-enabled Node-Agent lacks service account credentials")
	}
	var nodeEnv bool
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		nodeEnv = nodeEnv || (e.Name == "NODE_NAME" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "spec.nodeName")
	}
	if !nodeEnv {
		t.Fatal("Node-Agent lacks downward API node name")
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, "InventoryAccepted"); condition == nil || condition.Status != metav1.ConditionUnknown {
		t.Fatalf("acceptance must be unconfirmed: %+v", condition)
	}
	var reserved corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKeyFromObject(reservations), &reserved); err != nil {
		t.Fatal(err)
	}

	var state reservationState
	if err := json.Unmarshal([]byte(reserved.Data["reservations.json"]), &state); err != nil || len(state.Reservations) != 2 {
		t.Fatalf("reservations missing: %+v %v", state, err)
	}
	// Reconciliation is idempotent and a removed worker remains reserved.
	reconcileCore(t, r, core)
	current = fetchCore(t, r, core)
	current.Spec.WorkerInventory.Revision = 2
	current.Spec.WorkerInventory.Workers = current.Spec.WorkerInventory.Workers[:1]
	current.Generation++
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "worker-b"}, &node); err != nil {
		t.Fatal(err)
	}
	node.Labels["evpn.example.com/eligible"] = "false"
	if err := r.Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, current)
	if err := r.Get(ctx, client.ObjectKeyFromObject(reservations), &reserved); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(reserved.Data["reservations.json"]), &state); err != nil || len(state.Reservations) != 2 {
		t.Fatalf("removed worker lost reservation: %+v %v", state, err)
	}
}

func TestWorkerInventoryRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*evpnv1.EvpnCore, *corev1.ConfigMap, []client.Object)
	}{
		{"wrong node UID", func(c *evpnv1.EvpnCore, _ *corev1.ConfigMap, _ []client.Object) {
			c.Spec.WorkerInventory.Workers[0].NodeUID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		}},
		{"duplicate chassis", func(c *evpnv1.EvpnCore, _ *corev1.ConfigMap, _ []client.Object) {
			c.Spec.WorkerInventory.Workers[1].Chassis = c.Spec.WorkerInventory.Workers[0].Chassis
		}},
		{"duplicate speaker", func(c *evpnv1.EvpnCore, _ *corev1.ConfigMap, _ []client.Object) {
			c.Spec.WorkerInventory.Workers[1].SpeakerRouterID = c.Spec.WorkerInventory.Workers[0].SpeakerRouterID
		}},
		{"duplicate VTEP", func(c *evpnv1.EvpnCore, _ *corev1.ConfigMap, _ []client.Object) {
			c.Spec.WorkerInventory.Workers[1].LocalVTEP = c.Spec.WorkerInventory.Workers[0].LocalVTEP
		}},
		{"untrusted reservation resource", func(_ *evpnv1.EvpnCore, cm *corev1.ConfigMap, _ []client.Object) {
			cm.Labels = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, reservations, nodes := inventoryFixture()
			tc.change(core, reservations, nodes)
			r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
			if _, err := r.prepareWorkerInventory(context.Background(), core); err == nil {
				t.Fatal("invalid inventory accepted")
			}
		})
	}
}

func TestWorkerReservationsRejectRollbackAndReassignment(t *testing.T) {
	ctx := context.Background()
	core, reservations, nodes := inventoryFixture()
	r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
	if _, err := r.prepareWorkerInventory(ctx, core); err != nil {
		t.Fatal(err)
	}
	core.Spec.WorkerInventory.Revision = 2
	if _, err := r.prepareWorkerInventory(ctx, core); err == nil {
		// currentSnapshot intentionally rejects an unpersisted CR update.
		t.Fatal("unpersisted inventory generation accepted")
	}
	var current evpnv1.EvpnCore
	if err := r.Get(ctx, client.ObjectKeyFromObject(core), &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.WorkerInventory.Revision = 2
	current.Generation++
	if err := r.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareWorkerInventory(ctx, &current); err != nil {
		t.Fatal(err)
	}
	stale := current.DeepCopyObject().(*evpnv1.EvpnCore)
	stale.Spec.WorkerInventory.Revision = 1
	if _, err := r.prepareWorkerInventory(ctx, stale); err == nil {
		t.Fatal("stale revision accepted")
	}
	changed := current.DeepCopyObject().(*evpnv1.EvpnCore)
	changed.Spec.WorkerInventory.Revision = 3
	changed.Spec.WorkerInventory.Workers[0].Chassis = "new-chassis"
	if _, err := r.prepareWorkerInventory(ctx, changed); err == nil {
		t.Fatal("worker reassignment accepted")
	}
	// A replacement installation cannot claim a prior worker, even after the
	// original installation is gone; release is an administrative operation.
	replacement := current.DeepCopyObject().(*evpnv1.EvpnCore)
	replacement.UID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	replacement.Spec.WorkerInventory.Revision = 1
	if _, err := r.prepareWorkerInventory(ctx, replacement); err == nil {
		t.Fatal("replacement installation reused a reserved worker")
	}
}

func TestWorkerInventoryRejectsSubstitutedPublication(t *testing.T) {
	core, reservations, nodes := inventoryFixture()
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: workerInventoryName, Namespace: "evpn-system", UID: "foreign"}}
	objects := append([]client.Object{core, reservations, foreign}, nodes...)
	r := newTestReconciler(t, objects...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if current.Status.Phase != "Degraded" || !strings.Contains(current.Status.Reason, "ownership") {
		t.Fatalf("substituted publication was not rejected: %+v", current.Status)
	}

	var actual corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.UID != foreign.UID || len(actual.Data) != 0 {
		t.Fatal("foreign publication was modified")
	}
}

func TestWorkerReservationsSurviveInstallationDeletion(t *testing.T) {
	ctx := context.Background()
	core, reservations, nodes := inventoryFixture()
	if cleanupAllowed(evpnv1.ManagedResource{APIVersion: "v1", Kind: "ConfigMap", Namespace: LockNamespace, Name: reservationsName}) {
		t.Fatal("persistent reservations must never be installation cleanup targets")
	}
	r := newTestReconciler(t, append([]client.Object{core, reservations}, nodes...)...)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if err := r.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		reconcileCore(t, r, core)
	}
	var published corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: "evpn-system", Name: workerInventoryName}, &published); !apierrors.IsNotFound(err) {
		t.Fatalf("installation publication must be removed: %v", err)
	}
	var retained corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKeyFromObject(reservations), &retained); err != nil {
		t.Fatalf("reservation was deleted with installation: %v", err)
	}
	var state reservationState
	if err := json.Unmarshal([]byte(retained.Data["reservations.json"]), &state); err != nil || len(state.Reservations) != 2 {
		t.Fatalf("reservation history not retained: %+v %v", state, err)
	}
}
