package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func withResourceUIDs(t *testing.T, r *EvpnCoreReconciler) client.WithWatch {
	t.Helper()
	raw := r.Client.(client.WithWatch)
	sequence := 0
	r.Client = interceptor.NewClient(raw, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				sequence++
				obj.SetUID(types.UID(fmt.Sprintf("child-%d", sequence)))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	return r.Client.(client.WithWatch)
}

func updateIntent(t *testing.T, c client.Client, core *evpnv1.EvpnCore, deleting bool) {
	t.Helper()
	var current evpnv1.EvpnCore
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(core), &current); err != nil {
		t.Fatal(err)
	}
	if deleting {
		if err := c.Delete(ctx, &current); err != nil {
			t.Fatal(err)
		}
		return
	}
	current.Spec.AppImage = "new-generation:image"
	current.Generation++
	if err := c.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotChangesDuringReconciliation(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, point := range []string{"before-journal", "after-journal", "during-create", "before-ready"} {
			t.Run(fmt.Sprintf("%s/delete=%t", point, deleting), func(t *testing.T) {
				core := coreFixture("core", "core-uid", 1)
				r := newTestReconciler(t, core)
				raw := withResourceUIDs(t, r)
				reconcileCore(t, r, core)
				setWorkloadsReady(t, r)
				if point == "during-create" {
					var svc corev1.Service
					if err := raw.Get(context.Background(), client.ObjectKey{Namespace: "evpn-system", Name: "evpn-mgmt"}, &svc); err != nil {
						t.Fatal(err)
					}
					if err := raw.Delete(context.Background(), &svc); err != nil {
						t.Fatal(err)
					}
				}
				triggered := false
				r.Client = interceptor.NewClient(raw, interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						coreObj, ok := obj.(*evpnv1.EvpnCore)
						if ok && !triggered && ((point == "before-journal" || point == "after-journal") && coreObj.Status.Phase != "Ready" || point == "before-ready" && coreObj.Status.Phase == "Ready") {
							triggered = true
							if point != "after-journal" {
								updateIntent(t, c, core, deleting)
							}
							err := c.SubResource(sub).Patch(ctx, obj, patch, opts...)
							if point == "after-journal" && err == nil {
								updateIntent(t, c, core, deleting)
								// Model a successful status response containing a newer spec.
								return c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
							}
							return err
						}
						return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if point == "during-create" && !triggered && obj.GetName() == "evpn-mgmt" {
							triggered = true
							updateIntent(t, c, core, deleting)
						}
						return c.Create(ctx, obj, opts...)
					},
				})
				// Force a journal write for the before/after-journal cases.
				if strings.Contains(point, "journal") {
					current := fetchCore(t, r, core)
					current.Status.ManagedResources = nil
					if err := raw.Status().Update(context.Background(), current); err != nil {
						t.Fatal(err)
					}
				}
				_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)})
				if !triggered {
					t.Fatal("fault hook was not executed")
				}
				got := fetchCore(t, r, core)
				if got.Status.Ready || got.Status.ObservedGeneration > 1 {
					t.Fatalf("stale snapshot reported Ready/current generation: %+v", got.Status)
				}
				if !deleting && (got.Generation != 2 || got.Spec.AppImage != "new-generation:image") {
					t.Fatal("snapshot write overwrote new intent")
				}
				r.Client = raw
				if deleting {
					reconcileCore(t, r, core)
					reconcileCore(t, r, core)
					var gone evpnv1.EvpnCore
					if err := raw.Get(context.Background(), client.ObjectKeyFromObject(core), &gone); !apierrors.IsNotFound(err) {
						t.Fatalf("restart cleanup failed: %v", err)
					}
				} else {
					reconcileCore(t, r, core)
					if fetchCore(t, r, core).Status.ObservedGeneration != 2 {
						t.Fatal("new generation did not reconcile")
					}
				}
			})
		}
	}
}

func TestPruningAbortsWhenIntentChanges(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint(deleting), func(t *testing.T) {
			core := coreFixture("core", "core-uid", 1)
			r := newTestReconciler(t, core)
			reconcileCore(t, r, core)
			current := fetchCore(t, r, core)
			current.Spec.AppNamespace = "new-apps"
			current.Generation++
			if err := r.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			raw := r.Client.(client.WithWatch)
			triggered, deletes := false, 0
			r.Client = interceptor.NewClient(raw, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					if _, ok := obj.(*unstructured.Unstructured); ok && !triggered {
						triggered = true
						updateIntent(t, c, core, deleting)
					}
					return err
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					return c.Delete(ctx, obj, opts...)
				},
			})
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)})
			if !triggered || deletes != 0 {
				t.Fatalf("pruned changed intent: hook=%t deletes=%d", triggered, deletes)
			}
		})
	}
}

func TestJournalFaultsRecoverAcrossRestart(t *testing.T) {
	for _, point := range []string{"intent-persistence", "resource-create", "uid-persistence"} {
		t.Run(point, func(t *testing.T) {
			core := coreFixture("core", "core-uid", 1)
			r := newTestReconciler(t, core)
			raw := withResourceUIDs(t, r)
			failure := errors.New("injected journal/create outage")
			failed := false
			r.Client = interceptor.NewClient(raw, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if coreObj, ok := obj.(*evpnv1.EvpnCore); ok && !failed {
						for _, ref := range coreObj.Status.ManagedResources {
							if ref.Name == "ovn-ovs" && (point == "intent-persistence" && ref.State == "CreationPending" || point == "uid-persistence" && ref.UID != "") {
								failed = true
								return failure
							}
						}
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if point == "resource-create" && !failed && obj.GetName() == "ovn-ovs" {
						failed = true
						return failure
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil || !failed {
				t.Fatalf("fault did not fail: %v", err)
			}
			var svc corev1.Service
			err := raw.Get(context.Background(), client.ObjectKey{Name: "ovn-ovs", Namespace: "ovn-system"}, &svc)
			if point == "uid-persistence" {
				if err != nil {
					t.Fatal("created child lost")
				}
				refs := fetchCore(t, r, core).Status.ManagedResources
				if len(refs) != 1 || refs[0].State != "CreationPending" || refs[0].UID != "" {
					t.Fatalf("creation intent lost: %+v", refs)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("created without successful journal/create: %v", err)
			}
			restarted := &EvpnCoreReconciler{Client: raw, Scheme: r.Scheme, ManifestData: r.ManifestData}
			reconcileCore(t, restarted, core)
			for _, ref := range fetchCore(t, restarted, core).Status.ManagedResources {
				if ref.UID == "" || ref.State != "Observed" {
					t.Fatalf("restart did not confirm UID: %+v", ref)
				}
			}
		})
	}
}

func TestUIDReplacementCannotBeReboundByNormalReconciliation(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	r := newTestReconciler(t, core)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	var agent appsv1.DaemonSet
	key := client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}
	if err := r.Get(context.Background(), key, &agent); err != nil {
		t.Fatal(err)
	}
	oldUID := agent.UID
	if err := r.Delete(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	agent.ResourceVersion = ""
	agent.UID = "replacement-uid"
	if err := r.Create(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reconcileCore(t, r, core)
		got := fetchCore(t, r, core)
		assertPhase(t, got, "Degraded")
		for _, ref := range got.Status.ManagedResources {
			if ref.Name == "evpn-node-agent" && (ref.UID != string(oldUID) || ref.Conflict == "") {
				t.Fatalf("journal rebound replacement: %+v", ref)
			}
		}
	}
	current := fetchCore(t, r, core)
	current.Spec.AppNamespace = "relocated"
	current.Generation++
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	var replacement appsv1.DaemonSet
	if err := r.Get(context.Background(), client.ObjectKey{Name: key.Name, Namespace: "relocated"}, &replacement); !apierrors.IsNotFound(err) {
		t.Fatalf("conflicting relocation launched agent: %v", err)
	}
	if err := r.Delete(context.Background(), fetchCore(t, r, core)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil {
			t.Fatal("cleanup conflict hidden")
		}
	}
	if !contains(fetchCore(t, r, core).Finalizers, Finalizer) {
		t.Fatal("finalizer removed with conflicting workload")
	}
	if err := r.Get(context.Background(), key, &agent); err != nil {
		t.Fatal("replacement workload deleted")
	}
}

func TestConfirmedAbsenceAllowsOperatorRecreation(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	var svc corev1.Service
	key := client.ObjectKey{Name: "evpn-mgmt", Namespace: "evpn-system"}
	if err := r.Get(context.Background(), key, &svc); err != nil {
		t.Fatal(err)
	}
	oldUID := svc.UID
	if err := r.Delete(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	if err := r.Get(context.Background(), key, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.UID == oldUID {
		t.Fatal("no operator recreation")
	}
	for _, ref := range fetchCore(t, r, core).Status.ManagedResources {
		if ref.Name == key.Name && ref.Kind == "Service" && ref.UID != string(svc.UID) {
			t.Fatal("recreated UID not confirmed")
		}
	}
}

func TestDiscoveryRecoversMissingAndPartialJournalBeforeRelocation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			core := coreFixture("core", "core-uid", 1)
			core.Spec.NodeAgent.Enabled = true
			r := newTestReconciler(t, core)
			withResourceUIDs(t, r)
			reconcileCore(t, r, core)
			current := fetchCore(t, r, core)
			if partial {
				current.Status.ManagedResources = current.Status.ManagedResources[:1]
			} else {
				current.Status.ManagedResources = nil
			}
			if err := r.Status().Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			current = fetchCore(t, r, core)
			current.Spec.AppNamespace, current.Spec.OvnNamespace = "new-apps", "new-ovn"
			current.Generation++
			if err := r.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			reconcileCore(t, r, core)
			var ds appsv1.DaemonSet
			if err := r.Get(context.Background(), client.ObjectKey{Name: "evpn-node-agent", Namespace: "new-apps"}, &ds); !apierrors.IsNotFound(err) {
				t.Fatal("missing journal allowed overlapping replacement")
			}
			if len(fetchCore(t, r, core).Status.ManagedResources) < 8 {
				t.Fatal("historical namespace inventory not recovered")
			}
			reconcileCore(t, r, core)
			if err := r.Get(context.Background(), client.ObjectKey{Name: "evpn-node-agent", Namespace: "new-apps"}, &ds); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscoveryConflictAndBounds(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	orphan := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "historical", UID: "orphan-uid",
		Labels: map[string]string{OwnerKey: string(core.UID)}, Annotations: map[string]string{OwnerKey: string(core.UID)}}}
	r := newTestReconciler(t, core, orphan)
	reconcileCore(t, r, core)
	got := fetchCore(t, r, core)
	assertPhase(t, got, "Degraded")
	if len(got.Status.ManagedResources) != 1 || got.Status.ManagedResources[0].Conflict == "" {
		t.Fatal("unverifiable orphan was dropped")
	}
	if err := r.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil {
		t.Fatal("unverifiable orphan cleanup completed")
	}
	core = coreFixture("bounded", "bounded-uid", 1)
	objects := []client.Object{core}
	for i := 0; i <= maxManagedResources; i++ {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("orphan-%d", i), Namespace: "historical", Labels: map[string]string{OwnerKey: string(core.UID)}}})
	}
	r = newTestReconciler(t, objects...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize discovery did not fail closed: %v", err)
	}
}

func TestPartialCleanupFailureRetainsInventoryAndLease(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	current := fetchCore(t, r, core)
	count := len(current.Status.ManagedResources)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	raw := r.Client.(client.WithWatch)
	deletes := 0
	r.Client = interceptor.NewClient(raw, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			if deletes == 2 {
				return errors.New("injected second deletion failure")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil {
		t.Fatal("partial failure hidden")
	}
	if len(fetchCore(t, r, core).Status.ManagedResources) != count {
		t.Fatal("failed cleanup lost inventory")
	}
	var lease coordv1.Lease
	if err := r.Get(context.Background(), client.ObjectKey{Name: LockName, Namespace: LockNamespace}, &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(core.UID) {
		t.Fatal("partial cleanup released lock")
	}
	r.Client = raw
	reconcileCore(t, r, core)
	reconcileCore(t, r, core)
	var gone evpnv1.EvpnCore
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(core), &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("restart cleanup did not finish: %v", err)
	}
}

func TestSingleNodePlacementAndFailures(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	core.Spec.NodeAgent.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	var ds appsv1.DaemonSet
	var ovn appsv1.Deployment
	if err := r.Get(context.Background(), client.ObjectKey{Name: "evpn-node-agent", Namespace: "evpn-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "ovn-ovs", Namespace: "ovn-system"}, &ovn); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []corev1.PodSpec{ds.Spec.Template.Spec, ovn.Spec.Template.Spec} {
		if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil {
			t.Fatal("no exact-node affinity")
		}
		terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 1 || len(terms[0].MatchFields) != 1 || terms[0].MatchFields[0].Key != "metadata.name" ||
			!reflect.DeepEqual(terms[0].MatchFields[0].Values, []string{"worker"}) || !reflect.DeepEqual(pod.Tolerations, core.Spec.NodeAgent.Tolerations) {
			t.Fatalf("dataplane scheduling mismatch: %+v", pod)
		}
	}
	for _, selector := range []map[string]string{{"zone": "all"}, {"kubernetes.io/hostname": "another"}} {
		spec := core.Spec
		spec.NodeAgent.NodeSelector = selector
		if validateSpec(spec) == nil {
			t.Fatal("unsupported selector accepted")
		}
	}
	current := fetchCore(t, r, core)
	current.Spec.DataplaneNode = "missing"
	current.Generation++
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCore(t, r, core)
	got := fetchCore(t, r, core)
	assertPhase(t, got, "Reconciling")
	if !strings.Contains(got.Status.Reason, "does not exist") {
		t.Fatal(got.Status.Reason)
	}
	for _, mode := range []string{"unready", "cordoned", "tainted"} {
		t.Run(mode, func(t *testing.T) {
			core := coreFixture("core", "core-uid", 1)
			r := newTestReconciler(t, core)
			var node corev1.Node
			if err := r.Get(context.Background(), client.ObjectKey{Name: "worker"}, &node); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unready":
				node.Status.Conditions = nil
				if err := r.Status().Update(context.Background(), &node); err != nil {
					t.Fatal(err)
				}
			case "cordoned":
				node.Spec.Unschedulable = true
			case "tainted":
				node.Spec.Taints = []corev1.Taint{{Key: "isolated", Effect: corev1.TaintEffectNoSchedule}}
			}
			if mode != "unready" {
				if err := r.Update(context.Background(), &node); err != nil {
					t.Fatal(err)
				}
			}
			reconcileCore(t, r, core)
			got := fetchCore(t, r, core)
			assertPhase(t, got, "Reconciling")
			if len(got.Status.ManagedResources) != 0 {
				t.Fatal("workloads launched without usable designated Node")
			}
		})
	}
}

func TestNamespaceEndpointsRoundTripAndChangedDefaults(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	for _, names := range [][2]string{{"custom-apps", "custom-ovn"}, {"evpn-system", "ovn-system"}, {"custom-apps", "custom-ovn"}} {
		current := fetchCore(t, r, core)
		current.Spec.AppNamespace, current.Spec.OvnNamespace = names[0], names[1]
		current.Generation++
		if err := r.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		reconcileCore(t, r, core)
		reconcileCore(t, r, core)
		var ovs, bgp appsv1.Deployment
		if err := r.Get(context.Background(), client.ObjectKey{Name: "ovsdb-mgmt", Namespace: names[0]}, &ovs); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(context.Background(), client.ObjectKey{Name: "bgp-ad-handler", Namespace: names[0]}, &bgp); err != nil {
			t.Fatal(err)
		}
		env := ovs.Spec.Template.Spec.Containers[0].Env
		if env[0].Value != "tcp:ovn-ovs."+names[1]+".svc.cluster.local:6641" || env[1].Value != "tcp:ovn-ovs."+names[1]+".svc.cluster.local:6640" {
			t.Fatal("OVN namespace round-trip endpoints stale")
		}
		if bgp.Spec.Template.Spec.Containers[0].Env[2].Value != "ovsdb-mgmt."+names[0]+".svc.cluster.local:50053" {
			t.Fatal("app namespace round-trip endpoint stale")
		}
	}
	r.ManifestData = []byte(strings.ReplaceAll(string(r.ManifestData), "k8gobgp.k8gobgp-system.svc.cluster.local:50051", "changed-provider:50051"))
	reconcileCore(t, r, core)
	var mgmt appsv1.Deployment
	key := client.ObjectKey{Name: "evpn-mgmt", Namespace: "custom-apps"}
	if err := r.Get(context.Background(), key, &mgmt); err != nil {
		t.Fatal(err)
	}
	if mgmt.Spec.Template.Spec.Containers[0].Env[1].Value != "changed-provider:50051" {
		t.Fatal("old YAML default masked changed default")
	}
	mgmt.Spec.Template.Spec.Containers[0].Env[1].Value = "unix:///explicit-provider.sock"
	mgmt.Spec.Template.Spec.Containers[0].Env[0].Value = "unsupported-drift"
	if err := r.Update(context.Background(), &mgmt); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reconcileCore(t, r, core)
	}
	if err := r.Get(context.Background(), key, &mgmt); err != nil {
		t.Fatal(err)
	}
	if mgmt.Spec.Template.Spec.Containers[0].Env[1].Value != "unix:///explicit-provider.sock" || mgmt.Spec.Template.Spec.Containers[0].Env[0].Value != "k8gobgp" {
		t.Fatal("explicit override allowlist not respected")
	}
}

func TestDeploymentFailureConditionsRequireObservedGeneration(t *testing.T) {
	for _, condition := range []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
		{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue},
	} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "rollout", Namespace: "apps", Generation: 3},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{condition}, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1}}
		r := newTestReconciler(t, d)
		obj := &unstructured.Unstructured{}
		obj.SetKind("Deployment")
		obj.SetAPIVersion("apps/v1")
		obj.SetName(d.Name)
		obj.SetNamespace(d.Namespace)
		phase, _, _, err := r.workloadStatus(context.Background(), []*unstructured.Unstructured{obj}, nil)
		if err != nil || phase != "Reconciling" {
			t.Fatalf("stale failure interpreted as current: %s %v", phase, err)
		}
		d.Status.ObservedGeneration = 3
		if err := r.Status().Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		phase, _, _, err = r.workloadStatus(context.Background(), []*unstructured.Unstructured{obj}, nil)
		if err != nil || phase != "Degraded" {
			t.Fatalf("current failure ignored: %s %v", phase, err)
		}
	}
}

func TestStaleInstallationLeaseIsNeverAutomaticallyTakenOver(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	holder := "force-removed-core-uid"
	lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LockName, Namespace: LockNamespace, Labels: map[string]string{"app.kubernetes.io/managed-by": fieldManager}},
		Spec: coordv1.LeaseSpec{HolderIdentity: &holder}}
	r := newTestReconciler(t, core, lease)
	for range 2 {
		reconcileCore(t, r, core)
	}
	got := fetchCore(t, r, core)
	assertPhase(t, got, "Degraded")
	if len(got.Status.ManagedResources) != 0 {
		t.Fatal("stale holder was taken over")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lease), lease); err != nil {
		t.Fatal(err)
	}
	if *lease.Spec.HolderIdentity != holder || lease.Spec.LeaseDurationSeconds != nil {
		t.Fatal("installation Lease unexpectedly expired/rebound")
	}
}

func TestSnapshotRejectsRecreatedCRUID(t *testing.T) {
	core := coreFixture("core", "original-core-uid", 1)
	r := newTestReconciler(t, core)
	snapshot := fetchCore(t, r, core)
	if err := r.Delete(context.Background(), core); err != nil {
		t.Fatal(err)
	}
	replacement := coreFixture("core", "replacement-core-uid", 1)
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := r.setStatus(context.Background(), snapshot, "Ready", "WorkloadsAvailable", "stale"); !errors.Is(err, errSnapshotChanged) {
		t.Fatalf("recreated CR accepted stale status: %v", err)
	}
	if fetchCore(t, r, replacement).Status.Ready {
		t.Fatal("new CR received old installation Ready")
	}
}

func TestInventoryConcurrencyAbortsCleanupBeforeDeletion(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	r := newTestReconciler(t, core)
	reconcileCore(t, r, core)
	if err := r.Delete(context.Background(), fetchCore(t, r, core)); err != nil {
		t.Fatal(err)
	}
	raw := r.Client.(client.WithWatch)
	changed, deletes := false, 0
	r.Client = interceptor.NewClient(raw, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if _, ok := obj.(*unstructured.Unstructured); ok && !changed {
				changed = true
				current := fetchCore(t, r, core)
				current.Status.ManagedResources = append(current.Status.ManagedResources, evpnv1.ManagedResource{
					APIVersion: "v1", Kind: "ConfigMap", Namespace: "historical", Name: "concurrently-restored", UID: "retained-uid",
				})
				if err := c.Status().Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			return err
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(core)}); err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("concurrent inventory update was overwritten: %v", err)
	}
	if !changed || deletes != 0 {
		t.Fatalf("stale cleanup plan executed: changed=%t deletes=%d", changed, deletes)
	}
	found := false
	for _, ref := range fetchCore(t, r, core).Status.ManagedResources {
		if ref.Name == "concurrently-restored" {
			found = true
		}
	}
	if !found {
		t.Fatal("restored inventory entry lost")
	}
}

func TestReadinessRejectsUnexpectedWorkloadUIDAndMultiNodeAgent(t *testing.T) {
	core := coreFixture("core", "core-uid", 1)
	core.Spec.NodeAgent.Enabled = true
	r := newTestReconciler(t, core)
	withResourceUIDs(t, r)
	reconcileCore(t, r, core)
	setWorkloadsReady(t, r)
	objects, err := renderManifest(r.ManifestData, core.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var agent appsv1.DaemonSet
	key := client.ObjectKey{Namespace: "evpn-system", Name: "evpn-node-agent"}
	if err := r.Get(context.Background(), key, &agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.DesiredNumberScheduled, agent.Status.UpdatedNumberScheduled, agent.Status.NumberReady, agent.Status.NumberAvailable = 2, 2, 2, 2
	if err := r.Status().Update(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	phase, _, _, err := r.workloadStatus(context.Background(), objects, fetchCore(t, r, core))
	if err != nil || phase != "Reconciling" {
		t.Fatalf("multi-node agent was Ready: %s %v", phase, err)
	}
	if err := r.Delete(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	agent.ResourceVersion = ""
	agent.UID = "unexpected-ready-workload"
	if err := r.Create(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = r.workloadStatus(context.Background(), objects, fetchCore(t, r, core))
	var conflict *ownershipConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("readiness accepted replacement UID: %v", err)
	}
}
