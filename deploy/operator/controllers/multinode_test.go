package controllers

import (
	"context"
	"reflect"
	"strings"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func externalCore() *evpnv1.EvpnCore {
	core := coreFixture("external", "external-uid", 1)
	core.Spec.DataplaneNode = ""
	core.Spec.DataplaneMode = "external-ovn"
	core.Spec.NodeAgent.Enabled = true
	core.Spec.NodeAgent.NodeSelector = map[string]string{"evpn.example.com/eligible": "true"}
	core.Spec.NodeAgent.OVNNBEndpoint = "tcp:ovn.example:6641"
	core.Spec.NodeAgent.OVNSBEndpoint = "tcp:ovn.example:6642"
	return core
}

func TestExternalOVNValidation(t *testing.T) {
	spec := externalCore().Spec
	if err := validateSpec(spec); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*evpnv1.EvpnCoreSpec){
		func(s *evpnv1.EvpnCoreSpec) { s.DataplaneNode = "worker" },
		func(s *evpnv1.EvpnCoreSpec) { s.NodeAgent.Enabled = false },
		func(s *evpnv1.EvpnCoreSpec) { s.NodeAgent.NodeSelector = nil },
		func(s *evpnv1.EvpnCoreSpec) { s.NodeAgent.NodeSelector = map[string]string{"invalid key": "x"} },
		func(s *evpnv1.EvpnCoreSpec) { s.NodeAgent.Mode = "in-memory" },
		func(s *evpnv1.EvpnCoreSpec) { s.NodeAgent.OVSDBEndpoint = "tcp:other-node:6640" },
	} {
		bad := externalCore().Spec
		mutate(&bad)
		if err := validateSpec(bad); err == nil {
			t.Fatalf("accepted invalid external spec: %+v", bad)
		}
	}
}

func TestExternalOVNType2JournalMount(t *testing.T) {
	core := externalCore()
	core.Spec.NodeAgent.ExportConfigPath = "/run/evpn-node-agent/export.json"
	core.Spec.NodeAgent.RemoteConfigPath = "/run/evpn-node-agent/import.json"
	core.Spec.NodeAgent.Type2JournalHostPath = "/var/lib/evpn-worker"
	if err := validateSpec(core.Spec); err != nil {
		t.Fatal(err)
	}
	r := newTestReconciler(t, core)
	objects, err := renderManifest(r.ManifestData, core.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	found := false
	for _, obj := range objects {
		if obj.GetKind() == "DaemonSet" && obj.GetName() == "evpn-node-agent" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &ds); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("Node-Agent DaemonSet missing")
	}
	pod := ds.Spec.Template.Spec
	if !strings.Contains(strings.Join(pod.Containers[0].Args, " "), "--evpn-export-verify-local-chassis") {
		t.Fatal("external Type-2 exporter must verify local chassis")
	}
	mountFound, volumeFound := false, false
	for _, mount := range pod.Containers[0].VolumeMounts {
		mountFound = mountFound || (mount.Name == "type2-journal" && mount.MountPath == "/var/lib/evpn-node-agent" && !mount.ReadOnly)
	}
	for _, volume := range pod.Volumes {
		volumeFound = volumeFound || (volume.Name == "type2-journal" && volume.HostPath != nil &&
			volume.HostPath.Path == "/var/lib/evpn-worker" && volume.HostPath.Type != nil && *volume.HostPath.Type == corev1.HostPathDirectory)
	}
	if !mountFound || !volumeFound {
		t.Fatalf("missing node-local journal mount: %+v", pod)
	}
	for _, bad := range []string{"/", "relative", "/var/lib/../tmp"} {
		spec := core.Spec
		spec.NodeAgent.Type2JournalHostPath = bad
		if err := validateSpec(spec); err == nil {
			t.Fatalf("accepted unsafe journal path %q", bad)
		}
	}
	spec := core.Spec
	spec.NodeAgent.ExportConfigPath, spec.NodeAgent.RemoteConfigPath = "", ""
	if err := validateSpec(spec); err == nil {
		t.Fatal("accepted journal mount without Type-2 config")
	}
}

func TestExternalOVNReconciliationAndRollout(t *testing.T) {
	ctx := context.Background()
	core := externalCore()
	provider := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "k8gobgp-system"}}
	r := newTestReconciler(t, core, provider)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if current.Status.DataplaneMode != "external-ovn" || len(current.Status.ManagedResources) != 1 ||
		current.Status.ManagedResources[0].UID == "" {
		t.Fatalf("unexpected installation journal: %+v", current.Status)
	}
	key := client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, key, &ds); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds.Spec.Template.Spec.NodeSelector, core.Spec.NodeAgent.NodeSelector) ||
		ds.Spec.Template.Spec.Affinity != nil || ds.Annotations[OwnerKey] != string(core.UID) {
		t.Fatalf("incorrect managed DaemonSet: %+v", ds)
	}
	var ovn appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Name: "ovn-ovs", Namespace: "ovn-system"}, &ovn); !apierrors.IsNotFound(err) {
		t.Fatalf("external OVN must not be managed: %v", err)
	}
	for _, count := range []int32{0, 1, 2} {
		ds.Status.ObservedGeneration = ds.Generation
		ds.Status.DesiredNumberScheduled = count
		ds.Status.UpdatedNumberScheduled = count
		ds.Status.NumberReady = count
		ds.Status.NumberAvailable = count
		if err := r.Status().Update(ctx, &ds); err != nil {
			t.Fatal(err)
		}
		objects, err := renderManifest(r.ManifestData, core.Spec)
		if err != nil {
			t.Fatal(err)
		}
		phase, _, _, err := r.workloadStatus(ctx, objects, fetchCore(t, r, core))
		if err != nil || (count == 0 && phase != "Reconciling") || (count > 0 && phase != "Ready") {
			t.Fatalf("count=%d phase=%s err=%v", count, phase, err)
		}
	}
	ds.Status.NumberReady = 1
	if err := r.Status().Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	objects, err := renderManifest(r.ManifestData, core.Spec)
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _, err := r.workloadStatus(ctx, objects, fetchCore(t, r, core))
	if err != nil || phase != "Reconciling" {
		t.Fatalf("incomplete rollout is ready: %s %v", phase, err)
	}
	current = fetchCore(t, r, core)
	current.Spec.NodeAgent.NodeSelector = map[string]string{"evpn.example.com/next": "true"}
	current.Generation++
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	// A fresh reconciler models operator restart while retaining API state.
	restarted := &EvpnCoreReconciler{Client: r.Client, Scheme: r.Scheme, ManifestData: r.ManifestData}
	reconcileCore(t, restarted, core)
	if err := restarted.Get(ctx, key, &ds); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds.Spec.Template.Spec.NodeSelector, current.Spec.NodeAgent.NodeSelector) {
		t.Fatalf("selector not updated: %v", ds.Spec.Template.Spec.NodeSelector)
	}
	var unchanged corev1.ConfigMap
	if err := restarted.Get(ctx, client.ObjectKeyFromObject(provider), &unchanged); err != nil {
		t.Fatalf("independent provider resource changed: %v", err)
	}
}

func TestExistingUnmarkedInstallationDefaultsToLegacy(t *testing.T) {
	core := coreFixture("legacy", "legacy-uid", 1)
	core.Finalizers = []string{Finalizer}
	core.Spec = externalCore().Spec
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	if current.Status.Phase != "Degraded" || len(current.Status.ManagedResources) != 0 {
		t.Fatalf("unmarked installation accepted mode change: %+v", current.Status)
	}
}

func TestExternalOVNFinalizationOnlyDeletesOwnedResources(t *testing.T) {
	ctx := context.Background()
	core := externalCore()
	provider := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "k8gobgp", Namespace: "k8gobgp-system"}}
	r := newTestReconciler(t, core, provider)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	if err := r.Delete(ctx, fetchCore(t, r, core)); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	restarted := &EvpnCoreReconciler{Client: r.Client, Scheme: r.Scheme, ManifestData: r.ManifestData}
	reconcileCore(t, restarted, core)
	var deleted evpnv1.EvpnCore
	if err := restarted.Get(ctx, client.ObjectKeyFromObject(core), &deleted); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not completed: %v", err)
	}
	var agent appsv1.DaemonSet
	if err := restarted.Get(ctx, client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}, &agent); !apierrors.IsNotFound(err) {
		t.Fatalf("managed agent not removed: %v", err)
	}
	if err := restarted.Get(ctx, client.ObjectKeyFromObject(provider), provider); err != nil {
		t.Fatalf("independent provider removed: %v", err)
	}
	var lease coordv1.Lease
	if err := restarted.Get(ctx, client.ObjectKey{Name: LockName, Namespace: LockNamespace}, &lease); err != nil ||
		lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "" {
		t.Fatalf("installation lock not released: %v", err)
	}
}

func TestDataplaneModeTransitionRejected(t *testing.T) {
	ctx := context.Background()
	for _, initial := range []*evpnv1.EvpnCore{coreFixture("legacy", "legacy-uid", 1), externalCore()} {
		t.Run(dataplaneMode(initial.Spec), func(t *testing.T) {
			r := newTestReconciler(t, initial)
			withResourceUIDs(t, r)
			reconcileCore(t, r, initial)
			current := fetchCore(t, r, initial)
			before := append([]evpnv1.ManagedResource(nil), current.Status.ManagedResources...)
			if dataplaneMode(current.Spec) == "external-ovn" {
				current.Spec.DataplaneMode = ""
				current.Spec.DataplaneNode = "worker"
				current.Spec.NodeAgent.NodeSelector = nil
			} else {
				current.Spec = externalCore().Spec
			}
			current.Generation++
			if err := r.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			reconcileCore(t, r, initial)
			after := fetchCore(t, r, initial)
			if after.Status.Phase != "Degraded" || !reflect.DeepEqual(before, after.Status.ManagedResources) {
				t.Fatalf("transition changed inventory: %+v", after.Status)
			}
		})
	}
}
