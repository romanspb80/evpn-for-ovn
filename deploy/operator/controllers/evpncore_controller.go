package controllers

import (
	"context"
	"errors"
	"time"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	Finalizer      = "evpn.romanspb80.github.io/core-cleanup"
	OwnerKey       = "evpn.romanspb80.github.io/installation-uid"
	LockName       = "evpn-core-installation"
	LockNamespace  = "evpn-operator-system"
	fieldManager   = "evpn-core-operator"
	lastAppliedKey = "evpn.romanspb80.github.io/last-applied"
	envDefaultsKey = "evpn.romanspb80.github.io/template-env-defaults"
)

type ownershipConflict struct{ message string }

func (e *ownershipConflict) Error() string { return e.message }

type EvpnCoreReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	// ManifestData is an optional test fixture. Production loads the packaged YAML.
	ManifestData []byte
}

func (r *EvpnCoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorderFor(fieldManager)
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var cores evpnv1.EvpnCoreList
		if err := r.List(ctx, &cores); err != nil {
			mgr.GetLogger().Error(err, "enqueue EvpnCore installations")
			return nil
		}
		result := make([]reconcile.Request, 0, len(cores.Items))
		for i := range cores.Items {
			result = append(result, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&cores.Items[i])})
		}
		return result
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&evpnv1.EvpnCore{}).
		Watches(&evpnv1.EvpnCore{}, all).
		Watches(&appsv1.Deployment{}, all).
		Watches(&appsv1.DaemonSet{}, all).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *EvpnCoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var core evpnv1.EvpnCore
	if err := r.Get(ctx, req.NamespacedName, &core); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !core.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&core, Finalizer) {
			return ctrl.Result{}, nil
		}
		if core.Status.Phase != "Deleting" {
			if err := r.setStatus(ctx, &core, "Deleting", "CleanupInProgress", "removing installation-owned Kubernetes resources"); err != nil {
				return ctrl.Result{}, err
			}
		}
		done, err := r.cleanup(ctx, &core, nil)
		if err != nil {
			r.event(&core, "CleanupFailed", err.Error())
			return ctrl.Result{}, errors.Join(err, r.setStatus(ctx, &core, "Deleting", "CleanupFailed", err.Error()))
		}
		if !done {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, &core, "Deleting", "CleanupInProgress", "removing installation-owned Kubernetes resources")
		}
		if err := r.releaseLock(ctx, &core); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.editFinalizer(ctx, &core, false)
	}
	if !core.Spec.Enabled {
		return ctrl.Result{}, r.setStatus(ctx, &core, "Reconciling", "Disabled", "reconciliation paused; existing resources and installation lock are retained")
	}
	if err := validateSpec(core.Spec); err != nil {
		r.event(&core, "ConfigurationInvalid", err.Error())
		return ctrl.Result{}, r.setStatus(ctx, &core, "ConfigurationInvalid", "InvalidSpec", err.Error())
	}
	if core.Status.ObservedGeneration != core.Generation {
		if err := r.setStatus(ctx, &core, "Reconciling", "NewGeneration", "evaluating current installation intent"); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.claimLock(ctx, &core); err != nil {
		return r.failed(ctx, &core, err)
	}
	available, message, err := r.placementAvailable(ctx, core.Spec)
	if err != nil {
		return r.failed(ctx, &core, err)
	}
	if !available {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.setStatus(ctx, &core, "Reconciling", "PlacementPending", message)
	}
	appNS, ovnNS := namespaces(core.Spec)
	for _, ns := range []string{appNS, ovnNS} {
		if err := r.ensureNamespace(ctx, ns); err != nil {
			return r.failed(ctx, &core, err)
		}
	}
	data := r.ManifestData
	if data == nil {
		var err error
		data, err = loadCoreManifest()
		if err != nil {
			return r.failed(ctx, &core, err)
		}
	}
	objects, err := renderManifest(data, core.Spec)
	if err != nil {
		return r.failed(ctx, &core, err)
	}
	if err := r.checkNodeAgentConflict(ctx, &core); err != nil {
		return r.failed(ctx, &core, err)
	}
	// Remove old namespace placements before launching replacements that share
	// host sockets/ports. Applying first could briefly run two node agents.
	done, err := r.cleanup(ctx, &core, objects)
	if err != nil {
		return r.failed(ctx, &core, err)
	}
	if !done {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, &core, "Reconciling", "PruningResources", "waiting for obsolete managed resources to be deleted")
	}
	for _, obj := range objects {
		if err := r.applyResource(ctx, &core, obj); err != nil {
			return r.failed(ctx, &core, err)
		}
	}
	phase, reason, message, err := r.workloadStatus(ctx, objects, &core)
	if err != nil {
		return r.failed(ctx, &core, err)
	}
	if err := r.setStatus(ctx, &core, phase, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *EvpnCoreReconciler) failed(ctx context.Context, core *evpnv1.EvpnCore, err error) (ctrl.Result, error) {
	if errors.Is(err, errSnapshotChanged) {
		return ctrl.Result{Requeue: true}, nil
	}
	var conflict *ownershipConflict
	reason := "ReconcileFailed"
	if errors.As(err, &conflict) {
		reason = "OwnershipConflict"
	}
	r.event(core, reason, err.Error())
	if statusErr := r.setStatus(ctx, core, "Degraded", reason, err.Error()); statusErr != nil {
		return ctrl.Result{}, errors.Join(err, statusErr)
	}
	if conflict != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, err
}

func (r *EvpnCoreReconciler) event(core *evpnv1.EvpnCore, reason, message string) {
	if r.Recorder != nil && (core.Status.Reason != message || core.Status.Phase == "Ready") {
		r.Recorder.Event(core, corev1.EventTypeWarning, reason, message)
	}
}

func (r *EvpnCoreReconciler) setStatus(ctx context.Context, core *evpnv1.EvpnCore, phase, reason, message string) error {
	return r.editStatus(ctx, core, func(status *evpnv1.EvpnCoreStatus) error {
		status.Phase, status.Ready, status.Reason = phase, phase == "Ready", message
		status.ObservedGeneration = core.Generation
		for _, typ := range []string{"Reconciling", "Ready", "Degraded", "Deleting", "ConfigurationInvalid"} {
			value := metav1.ConditionFalse
			if phase == typ {
				value = metav1.ConditionTrue
			}
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: typ, Status: value, ObservedGeneration: core.Generation,
				Reason: reason, Message: message,
			})
		}
		return nil
	})
}
