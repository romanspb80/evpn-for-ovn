package controllers

import (
	"context"
	"fmt"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *EvpnCoreReconciler) workloadStatus(ctx context.Context, objects []*unstructured.Unstructured, core *evpnv1.EvpnCore) (string, string, string, error) {
	waiting := ""
	count := 0
	for _, obj := range objects {
		switch obj.GetKind() {
		case "Deployment":
			count++
			var d appsv1.Deployment
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(obj), &d); err != nil {
				return "", "", "", err
			}
			if err := verifyWorkloadOwnership(core, obj, &d); err != nil {
				return "", "", "", err
			}
			if d.Status.ObservedGeneration < d.Generation {
				waiting = fmt.Sprintf("Deployment %s/%s controller has not observed generation %d", d.Namespace, d.Name, d.Generation)
				continue
			}
			for _, condition := range d.Status.Conditions {
				if (condition.Type == appsv1.DeploymentProgressing && condition.Status == "False" && condition.Reason == "ProgressDeadlineExceeded") ||
					(condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == "True") {
					return "Degraded", "WorkloadFailed", fmt.Sprintf("Deployment %s/%s: %s", d.Namespace, d.Name, condition.Message), nil
				}
			}
			replicas := int32(1)
			if d.Spec.Replicas != nil {
				replicas = *d.Spec.Replicas
			}
			if d.Status.ObservedGeneration < d.Generation || replicas == 0 || d.Status.UpdatedReplicas != replicas ||
				d.Status.AvailableReplicas != replicas || d.Status.ReadyReplicas != replicas || d.Status.Replicas != replicas {
				waiting = fmt.Sprintf("Deployment %s/%s has not completed its rollout", d.Namespace, d.Name)
			}
		case "DaemonSet":
			count++
			var d appsv1.DaemonSet
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(obj), &d); err != nil {
				return "", "", "", err
			}
			if err := verifyWorkloadOwnership(core, obj, &d); err != nil {
				return "", "", "", err
			}
			if d.Status.ObservedGeneration < d.Generation || d.Status.DesiredNumberScheduled == 0 ||
				obj.GetName() == "evpn-node-agent" && d.Status.DesiredNumberScheduled != 1 ||
				d.Status.UpdatedNumberScheduled != d.Status.DesiredNumberScheduled ||
				d.Status.NumberReady != d.Status.DesiredNumberScheduled ||
				d.Status.NumberAvailable != d.Status.DesiredNumberScheduled || d.Status.NumberUnavailable != 0 ||
				d.Status.NumberMisscheduled != 0 {
				waiting = fmt.Sprintf("DaemonSet %s/%s is not available on every selected node (desired=%d ready=%d)", d.Namespace, d.Name, d.Status.DesiredNumberScheduled, d.Status.NumberReady)
			}
		}
	}
	if count == 0 {
		return "Reconciling", "NoWorkloads", "no managed workloads to evaluate", nil
	}
	if waiting != "" {
		return "Reconciling", "WorkloadsPending", waiting, nil
	}
	return "Ready", "WorkloadsAvailable", "all managed Deployments and DaemonSets have completed their rollouts; BGP sessions and dataplane health are not evaluated", nil
}

func verifyWorkloadOwnership(core *evpnv1.EvpnCore, desired *unstructured.Unstructured, live client.Object) error {
	if core == nil {
		return nil
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(live)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: raw}
	obj.SetGroupVersionKind(desired.GroupVersionKind())
	if message := ownershipProblem(core, obj); message != "" {
		return &ownershipConflict{message}
	}
	if !live.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%s %s/%s is deleting", desired.GetKind(), live.GetNamespace(), live.GetName())
	}
	return nil
}
