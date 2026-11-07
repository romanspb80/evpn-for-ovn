package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := evpnv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func coreFixture(name, uid string, age int) *evpnv1.EvpnCore {
	return &evpnv1.EvpnCore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "installation", UID: types.UID(uid), Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Unix(int64(age), 0))},
		Spec: evpnv1.EvpnCoreSpec{Enabled: true, DataplaneNode: "worker"},
	}
}

func newTestReconciler(t *testing.T, objects ...client.Object) *EvpnCoreReconciler {
	t.Helper()
	data, err := os.ReadFile("../core-resources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := testScheme(t)
	objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"kubernetes.io/hostname": "worker"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&evpnv1.EvpnCore{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).WithObjects(objects...).Build()
	return &EvpnCoreReconciler{Client: c, Scheme: s, ManifestData: data, Recorder: record.NewFakeRecorder(100)}
}

func reconcileCore(t *testing.T, r *EvpnCoreReconciler, core *evpnv1.EvpnCore) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fetchCore(t *testing.T, r *EvpnCoreReconciler, core *evpnv1.EvpnCore) *evpnv1.EvpnCore {
	t.Helper()
	var current evpnv1.EvpnCore
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(core), &current); err != nil {
		t.Fatal(err)
	}
	return &current
}

func assertPhase(t *testing.T, core *evpnv1.EvpnCore, phase string) {
	t.Helper()
	if core.Status.Phase != phase {
		t.Fatalf("phase=%s want=%s reason=%s", core.Status.Phase, phase, core.Status.Reason)
	}
	for _, typ := range []string{"Reconciling", "Ready", "Degraded", "Deleting", "ConfigurationInvalid"} {
		c := meta.FindStatusCondition(core.Status.Conditions, typ)
		if c == nil || c.ObservedGeneration != core.Generation {
			t.Fatalf("missing/current-generation condition %s: %+v", typ, c)
		}
		if (c.Status == metav1.ConditionTrue) != (phase == typ) {
			t.Fatalf("incorrect condition %s: %+v", typ, c)
		}
	}
	if core.Status.Ready != (phase == "Ready") {
		t.Fatal("legacy ready disagrees with conditions")
	}
}

func TestSingletonAndEvents(t *testing.T) {
	old, next := coreFixture("old", "old-uid", 1), coreFixture("new", "new-uid", 2)
	next.Namespace = "another-installation"
	r := newTestReconciler(t, next, old)
	reconcileCore(t, r, next) // Arrival order does not determine the first owner.
	conflicting := fetchCore(t, r, next)
	assertPhase(t, conflicting, "Degraded")
	if !strings.Contains(conflicting.Status.Reason, "singleton") || len(conflicting.Finalizers) != 0 {
		t.Fatalf("conflicting core: %+v", conflicting)
	}
	select {
	case event := <-r.Recorder.(*record.FakeRecorder).Events:
		if !strings.Contains(event, "OwnershipConflict") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing conflict event")
	}
	reconcileCore(t, r, old)
	reconcileCore(t, r, next)
	var lock coordv1.Lease
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: LockNamespace, Name: LockName}, &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Spec.HolderIdentity == nil || *lock.Spec.HolderIdentity != string(old.UID) {
		t.Fatal("wrong singleton owner")
	}
	if len(fetchCore(t, r, next).Status.ManagedResources) != 0 {
		t.Fatal("loser managed resources")
	}
}

func TestSingletonTieBreakAndStickyOwner(t *testing.T) {
	a, b := coreFixture("a", "a-uid", 1), coreFixture("b", "b-uid", 1)
	r := newTestReconciler(t, b, a)
	reconcileCore(t, r, b)
	assertPhase(t, fetchCore(t, r, b), "Degraded")
	reconcileCore(t, r, a)
	// A newly observed older object must not displace an established installation.
	c := coreFixture("earlier", "earlier-uid", 0)
	if err := r.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, c)
	assertPhase(t, fetchCore(t, r, c), "Degraded")
	reconcileCore(t, r, a)
	assertPhase(t, fetchCore(t, r, a), "Reconciling")
}

func TestInvalidCandidateDoesNotBlockValidInstallation(t *testing.T) {
	invalid, valid := coreFixture("invalid", "invalid-uid", 1), coreFixture("valid", "valid-uid", 2)
	invalid.Spec.AppNamespace = "bad namespace"
	r := newTestReconciler(t, invalid, valid)
	reconcileCore(t, r, invalid)
	assertPhase(t, fetchCore(t, r, invalid), "ConfigurationInvalid")
	reconcileCore(t, r, valid)
	assertPhase(t, fetchCore(t, r, valid), "Reconciling")
}

func TestNodeAgentCreateUpdateDisableAndNamespaces(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.AppNamespace, core.Spec.OvnNamespace = "apps-custom", "ovn-custom"
	core.Spec.AppImage, core.Spec.OvnImage = "app:v1", "ovn:v1"
	core.Spec.NodeAgent = evpnv1.NodeAgentSpec{Enabled: true, Image: "agent:v1", NodeSelector: map[string]string{"kubernetes.io/hostname": "worker"},
		ExportConfigPath: "/run/evpn-node-agent/export.json", RemoteConfigPath: "/run/evpn-node-agent/import.json",
		GoBGPHostPath: "/run/gobgp-export-test"}
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Namespace: "apps-custom", Name: "evpn-node-agent"}
	if err := r.Get(context.Background(), key, &ds); err != nil {
		t.Fatal(err)
	}
	pod := ds.Spec.Template.Spec
	if !pod.HostNetwork || !pod.HostPID || !*pod.Containers[0].SecurityContext.Privileged {
		t.Fatal("missing host runtime access")
	}
	if pod.Containers[0].Image != "agent:v1" || pod.InitContainers[0].Image != "agent:v1" {
		t.Fatal("image not applied")
	}
	args := strings.Join(pod.Containers[0].Args, " ")
	for _, required := range []string{"--mode=real-ovn-ovs", "--network-backend=linux-veth", "--evpn-export-config=/run/evpn-node-agent/export.json", "--evpn-remote-config=/run/evpn-node-agent/import.json"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing %s in %s", required, args)
		}
	}
	if ds.Annotations[OwnerKey] != string(core.UID) || len(ds.OwnerReferences) != 0 {
		t.Fatal("wrong cross-namespace ownership")
	}
	var d appsv1.Deployment
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "apps-custom", Name: "ovsdb-mgmt"}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[0].Image != "app:v1" ||
		d.Spec.Template.Spec.Containers[0].Env[0].Value != "tcp:ovn-ovs.ovn-custom.svc.cluster.local:6641" {
		t.Fatal("app image/dependencies did not relocate")
	}
	current := fetchCore(t, r, core)
	current.Spec.NodeAgent.Image = "agent:v2"
	current.Spec.NodeAgent.NodeSelector = nil
	current.Spec.NodeAgent.ExportConfigPath = ""
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), key, &ds); err != nil {
		t.Fatal(err)
	}
	if ds.Spec.Template.Spec.Containers[0].Image != "agent:v2" || len(ds.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatal("node-agent update did not converge")
	}
	if strings.Contains(strings.Join(ds.Spec.Template.Spec.Containers[0].Args, " "), "/export.json") {
		t.Fatal("removed config flag persisted")
	}
	current = fetchCore(t, r, core)
	current.Spec.NodeAgent.Enabled = false
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), key, &ds); !apierrors.IsNotFound(err) {
		t.Fatalf("disabled agent remains: %v", err)
	}
	for _, ref := range fetchCore(t, r, core).Status.ManagedResources {
		if ref.Kind == "DaemonSet" {
			t.Fatal("stale agent inventory")
		}
	}
}

func TestDisabledAndProviders(t *testing.T) {
	for _, provider := range []string{"", "k8gobgp", "standalone", "frr", "K8GoBGP"} {
		t.Run(defaultString(provider, "default"), func(t *testing.T) {
			core := coreFixture("core", "core-uid", 1)
			core.Spec.BGP.Provider = provider
			r := newTestReconciler(t, core)
			reconcileCore(t, r, core)
			got := fetchCore(t, r, core)
			if provider == "" || provider == "k8gobgp" {
				assertPhase(t, got, "Reconciling")
				var ds appsv1.DaemonSetList
				if err := r.List(context.Background(), &ds); err != nil {
					t.Fatal(err)
				}
				if len(ds.Items) != 0 {
					t.Fatal("Node-Agent enabled by default")
				}
			} else {
				assertPhase(t, got, "ConfigurationInvalid")
				if len(got.Status.ManagedResources) != 0 || len(got.Finalizers) != 0 {
					t.Fatal("invalid configuration created resources")
				}
			}
		})
	}
	core := coreFixture("disabled", "disabled-uid", 1)
	core.Spec.Enabled = false
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	if len(fetchCore(t, r, core).Status.ManagedResources) != 0 {
		t.Fatal("disabled installation applied resources")
	}
}

func TestIdempotencyDriftAndServiceAllocations(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	var svc corev1.Service
	key := types.NamespacedName{Namespace: "evpn-system", Name: "evpn-mgmt"}
	if err := r.Get(context.Background(), key, &svc); err != nil {
		t.Fatal(err)
	}
	svc.Spec.ClusterIP, svc.Spec.ClusterIPs = "10.96.0.10", []string{"10.96.0.10"}
	svc.Annotations["external.example/note"] = "preserve"
	if err := r.Update(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	dkey := types.NamespacedName{Namespace: "evpn-system", Name: "evpn-mgmt"}
	if err := r.Get(context.Background(), dkey, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.Containers[0].Env[1].Value = "unix:///custom/gobgp.sock"
	d.Spec.Template.Spec.Containers[0].Image = "drift:bad"
	if err := r.Update(context.Background(), &d); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), key, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.ClusterIP != "10.96.0.10" || svc.Annotations["external.example/note"] != "preserve" {
		t.Fatal("server allocation/external metadata overwritten")
	}
	if err := r.Get(context.Background(), dkey, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[0].Image != "evpn4ovn/app:kind" || d.Spec.Template.Spec.Containers[0].Env[1].Value != "unix:///custom/gobgp.sock" {
		t.Fatal("image drift/env compatibility failure")
	}
	before := d.ResourceVersion
	coreBefore := fetchCore(t, r, core).ResourceVersion
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), dkey, &d); err != nil {
		t.Fatal(err)
	}
	if before != d.ResourceVersion || coreBefore != fetchCore(t, r, core).ResourceVersion {
		t.Fatalf("idempotent reconcile wrote unchanged resource/status: deployment %s -> %s, core %s -> %s", before, d.ResourceVersion, coreBefore, fetchCore(t, r, core).ResourceVersion)
	}
}

func TestReadinessAndFailureConditions(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Reconciling")
	setWorkloadsReady(t, r)
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Ready")
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Namespace: "evpn-system", Name: "evpn-node-agent"}
	if err := r.Get(context.Background(), key, &ds); err != nil {
		t.Fatal(err)
	}
	ds.Status.DesiredNumberScheduled = 0
	if err := r.Status().Update(context.Background(), &ds); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Reconciling")
	setWorkloadsReady(t, r)
	var d appsv1.Deployment
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "ovn-system", Name: "ovn-ovs"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded", Message: "rollout failed"}}
	if err := r.Status().Update(context.Background(), &d); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Degraded")
}

func setWorkloadsReady(t *testing.T, r *EvpnCoreReconciler) {
	t.Helper()
	var deployments appsv1.DeploymentList
	var agents appsv1.DaemonSetList
	if err := r.List(context.Background(), &deployments); err != nil {
		t.Fatal(err)
	}
	if err := r.List(context.Background(), &agents); err != nil {
		t.Fatal(err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		if err := r.Status().Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	for i := range agents.Items {
		d := &agents.Items[i]
		d.Status = appsv1.DaemonSetStatus{ObservedGeneration: d.Generation, DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}
		if err := r.Status().Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForeignResourcesAndDuplicateAgent(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ovn-ovs", Namespace: "ovn-system", UID: "foreign-uid"}}
	r := newTestReconciler(t, core, foreign)
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Degraded")
	var got corev1.Service
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[OwnerKey] != "" {
		t.Fatal("foreign service adopted")
	}
	core = coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "cni-test"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Command: []string{"/usr/local/bin/evpn-node-agent"}}}}}}}
	r = newTestReconciler(t, core, ds)
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Degraded")
	var agents appsv1.DaemonSetList
	if err := r.List(context.Background(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents.Items) != 1 {
		t.Fatal("duplicate Node-Agent created")
	}
}

func TestFinalizerCrossNamespaceCleanupAndProviderPreservation(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	provider := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "k8gobgp", Namespace: "k8gobgp-system"}}
	config := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "bgp.purelb.io/v1", "kind": "BGPConfiguration", "metadata": map[string]interface{}{"name": "provider", "namespace": "k8gobgp-system"}, "spec": map[string]interface{}{"global": map[string]interface{}{"asn": int64(64512)}}}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "k8gobgp", Namespace: "k8gobgp-system"}}
	r := newTestReconciler(t, core, provider, config, service)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if !contains(current.Finalizers, Finalizer) {
		t.Fatal("no finalizer")
	}
	// Unrelated, unjournaled resources survive cleanup.
	var survivor corev1.Service
	survivor.ObjectMeta = metav1.ObjectMeta{Name: "independent", Namespace: "evpn-system"}
	if err := r.Create(context.Background(), &survivor); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	assertPhase(t, fetchCore(t, r, core), "Deleting")
	// Simulate operator restart: inventory, not in-memory desired spec, drives cleanup.
	restarted := &EvpnCoreReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder}
	reconcileCore(t, restarted, core)
	var deleted evpnv1.EvpnCore
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(core), &deleted); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer remained: %v", err)
	}
	for _, obj := range []client.Object{provider, config, service, &survivor} {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("survivor removed: %T %v", obj, err)
		}
	}
	var deployments appsv1.DeploymentList
	if err := r.List(context.Background(), &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 0 {
		t.Fatal("cross-namespace workloads remain")
	}
	var ns corev1.Namespace
	if err := r.Get(context.Background(), types.NamespacedName{Name: "ovn-system"}, &ns); err != nil {
		t.Fatal("namespace deleted")
	}
	var lock coordv1.Lease
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: LockNamespace, Name: LockName}, &lock); err != nil {
		t.Fatal(err)
	}
	if *lock.Spec.HolderIdentity != "" {
		t.Fatal("installation lock not released")
	}
}

func TestCleanupFailureKeepsFinalizerAndRetries(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	failure := fmt.Errorf("injected forbidden cleanup")
	watchClient, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("test client must support watches")
	}
	c := interceptor.NewClient(watchClient, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return failure },
	})
	r.Client = c
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil || !strings.Contains(err.Error(), failure.Error()) {
		t.Fatalf("cleanup did not surface error: %v", err)
	}
	if !contains(fetchCore(t, r, core).Finalizers, Finalizer) {
		t.Fatal("removed finalizer before mandatory cleanup")
	}
	version := fetchCore(t, r, core).ResourceVersion
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil {
		t.Fatal("retry hid cleanup error")
	}
	if version != fetchCore(t, r, core).ResourceVersion {
		t.Fatal("repeated cleanup failure churned status and bypassed retry backoff")
	}
}

func TestNamespaceRelocationPrunesBeforeReplacement(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	current.Spec.AppNamespace, current.Spec.OvnNamespace = "relocated-app", "relocated-ovn"
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "relocated-app", Name: "evpn-node-agent"}, &ds); !apierrors.IsNotFound(err) {
		t.Fatalf("replacement launched before old cleanup completed: %v", err)
	}
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "relocated-app", Name: "evpn-node-agent"}, &ds); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "evpn-system", Name: "evpn-node-agent"}, &ds); !apierrors.IsNotFound(err) {
		t.Fatalf("old agent remains: %v", err)
	}
	for _, ref := range fetchCore(t, r, core).Status.ManagedResources {
		if ref.Namespace == "evpn-system" || ref.Namespace == "ovn-system" {
			t.Fatalf("old namespace inventory remains: %+v", ref)
		}
	}
}

func TestCleanupUIDReplacementIsRetained(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Finalizers = []string{Finalizer}
	core.Status.ManagedResources = []evpnv1.ManagedResource{{APIVersion: "v1", Kind: "Service", Namespace: "evpn-system", Name: "replacement", UID: "original-uid"}}
	survivor := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "evpn-system", UID: "replacement-uid",
		Annotations: map[string]string{OwnerKey: string(core.UID)}}}
	r := newTestReconciler(t, core, survivor)
	if err := r.Delete(context.Background(), core); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil {
		t.Fatal("replacement cleanup conflict was hidden")
	}
	current := fetchCore(t, r, core)
	if len(current.Status.ManagedResources) != 1 || current.Status.ManagedResources[0].UID != "original-uid" ||
		current.Status.ManagedResources[0].Conflict == "" || !contains(current.Finalizers, Finalizer) {
		t.Fatalf("replacement conflict not retained: %+v", current.Status)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(survivor), survivor); err != nil {
		t.Fatalf("UID replacement deleted: %v", err)
	}
	select {
	case event := <-r.Recorder.(*record.FakeRecorder).Events:
		if !strings.Contains(event, "CleanupOwnershipChanged") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing retained replacement event")
	}
}

func TestSpecValidation(t *testing.T) {
	for _, n := range []evpnv1.NodeAgentSpec{
		{Enabled: true, Socket: "/etc/agent.sock"},
		{Enabled: true, ExportConfigPath: "/run/evpn-node-agent/../bad.json"},
		{Enabled: true, ExportInterval: "0s"},
		{Enabled: true, RemoteTimeout: "invalid"},
		{Enabled: true, GoBGPHostPath: "/"},
		{Enabled: true, Mode: "in-memory", ExportConfigPath: "/run/evpn-node-agent/export.json"},
		{Enabled: true, Mode: "unknown"},
		{Enabled: true, NetworkBackend: "unknown"},
		{Enabled: true, NodeSelector: map[string]string{"bad key": "value"}},
	} {
		if err := validateSpec(evpnv1.EvpnCoreSpec{NodeAgent: n}); err == nil {
			t.Fatalf("invalid node configuration accepted: %+v", n)
		}
	}
	if err := validateSpec(evpnv1.EvpnCoreSpec{AppNamespace: "k8gobgp-system"}); err == nil {
		t.Fatal("provider namespace accepted for app resources")
	}
}

func TestManifestValidationAndDeepCopy(t *testing.T) {
	r := newTestReconciler(t)
	for _, data := range [][]byte{[]byte("kind: Service\n"), []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: bad}"), []byte("not: [valid")} {
		if _, err := renderManifest(data, evpnv1.EvpnCoreSpec{}); err == nil {
			t.Fatal("malformed/unsupported manifest silently accepted")
		}
	}
	objects, err := renderManifest(r.ManifestData, evpnv1.EvpnCoreSpec{NodeAgent: evpnv1.NodeAgentSpec{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if obj.GetName() == "k8gobgp" {
			t.Fatal("managed provider resource in manifest")
		}
		if _, err := json.Marshal(obj.Object); err != nil {
			t.Fatal(err)
		}
	}
	core := coreFixture("core", "core-uid", 1)
	seconds := int64(10)
	core.Spec.NodeAgent.NodeSelector = map[string]string{"node": "one"}
	core.Spec.NodeAgent.Tolerations = []corev1.Toleration{{TolerationSeconds: &seconds}}
	core.Status.Conditions = []metav1.Condition{{Type: "Ready"}}
	core.Status.ManagedResources = []evpnv1.ManagedResource{{Name: "owned"}}
	copy := core.DeepCopyObject().(*evpnv1.EvpnCore)
	copy.Spec.NodeAgent.NodeSelector["node"] = "two"
	*copy.Spec.NodeAgent.Tolerations[0].TolerationSeconds = 20
	copy.Status.Conditions[0].Type = "Degraded"
	copy.Status.ManagedResources[0].Name = "changed"
	if core.Spec.NodeAgent.NodeSelector["node"] != "one" || seconds != 10 ||
		core.Status.Conditions[0].Type != "Ready" || core.Status.ManagedResources[0].Name != "owned" {
		t.Fatal("deep copy aliases original")
	}
	if reflect.DeepEqual(copy, core) {
		t.Fatal("copy mutation failed")
	}
}
