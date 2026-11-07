package controllers

import (
	"context"
	"fmt"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func configurePlacement(obj *unstructured.Unstructured, spec evpnv1.EvpnCoreSpec) error {
	unstructured.RemoveNestedField(obj.Object, "spec", "template", "spec", "nodeSelector")
	affinity := corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{spec.DataplaneNode},
			}}}},
		},
	}}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&affinity)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedMap(obj.Object, raw, "spec", "template", "spec", "affinity"); err != nil {
		return err
	}
	pod := corev1.PodSpec{Tolerations: spec.NodeAgent.Tolerations}
	raw, err = runtime.DefaultUnstructuredConverter.ToUnstructured(&pod)
	if err != nil {
		return err
	}
	tolerations, _, err := unstructured.NestedSlice(raw, "tolerations")
	if err != nil {
		return err
	}
	if len(tolerations) == 0 {
		unstructured.RemoveNestedField(obj.Object, "spec", "template", "spec", "tolerations")
		return nil
	}
	return unstructured.SetNestedSlice(obj.Object, tolerations, "spec", "template", "spec", "tolerations")
}

func (r *EvpnCoreReconciler) placementAvailable(ctx context.Context, spec evpnv1.EvpnCoreSpec) (bool, string, error) {
	var node corev1.Node
	if err := r.reader().Get(ctx, client.ObjectKey{Name: spec.DataplaneNode}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "designated dataplane Node does not exist", nil
		}
		return false, "", err
	}
	if selector := spec.NodeAgent.NodeSelector["kubernetes.io/hostname"]; selector != "" && node.Labels["kubernetes.io/hostname"] != selector {
		return false, "designated dataplane Node does not match the supplied hostname selector", nil
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || node.Spec.Unschedulable {
		return false, "designated dataplane Node is not Ready and schedulable", nil
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, toleration := range spec.NodeAgent.Tolerations {
			if toleration.ToleratesTaint(ctrl.LoggerFrom(ctx), &taint, false) {
				tolerated = true
			}
		}
		if !tolerated {
			return false, fmt.Sprintf("designated dataplane Node has untolerated taint %s", taint.Key), nil
		}
	}
	return true, "", nil
}
