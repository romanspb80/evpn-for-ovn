package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	evpncorev1alpha1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
)

// EvpnCoreReconciler reconciles an EvpnCore object.
type EvpnCoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *EvpnCoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&evpncorev1alpha1.EvpnCore{}).
		Complete(r)
}

func (r *EvpnCoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var evpnCore evpncorev1alpha1.EvpnCore
	if err := r.Get(ctx, req.NamespacedName, &evpnCore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !evpnCore.Spec.Enabled {
		logger.Info("evpn core is disabled; skipping reconciliation", "name", evpnCore.Name)
		return ctrl.Result{}, nil
	}

	appNamespace := evpnCore.Spec.AppNamespace
	if appNamespace == "" {
		appNamespace = "evpn-system"
	}
	ovnNamespace := evpnCore.Spec.OvnNamespace
	if ovnNamespace == "" {
		ovnNamespace = "ovn-system"
	}

	if err := r.ensureNamespace(ctx, appNamespace); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureNamespace(ctx, ovnNamespace); err != nil {
		return ctrl.Result{}, err
	}

	manifestData, err := loadCoreManifest()
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyManifestSet(ctx, manifestData, appNamespace, ovnNamespace); err != nil {
		return ctrl.Result{}, err
	}

	evpnCore.Status.Phase = "Ready"
	evpnCore.Status.Ready = true
	evpnCore.Status.Reason = "core resources reconciled"
	if err := r.Status().Update(ctx, &evpnCore); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *EvpnCoreReconciler) ensureNamespace(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	obj := &corev1.Namespace{}
	obj.SetName(name)
	if err := r.Get(ctx, types.NamespacedName{Name: name}, obj); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func (r *EvpnCoreReconciler) applyManifestSet(ctx context.Context, manifestData []byte, appNamespace, ovnNamespace string) error {
	for _, block := range strings.Split(string(manifestData), "\n---\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var raw map[string]interface{}
		if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
			return fmt.Errorf("decode manifest block: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{Object: raw}
		if obj.GetKind() == "" || obj.GetName() == "" {
			continue
		}
		if obj.GetKind() == "Namespace" {
			if err := r.ensureNamespace(ctx, obj.GetName()); err != nil {
				return err
			}
			continue
		}
		ns := obj.GetNamespace()
		if ns == "" {
			switch obj.GetKind() {
			case "Deployment", "StatefulSet", "Service", "ServiceAccount", "ConfigMap":
				ns = appNamespace
			default:
				ns = ovnNamespace
			}
		}
		obj.SetNamespace(ns)

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		if err := r.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) {
				if err := r.Create(ctx, obj); err != nil {
					return err
				}
				continue
			}
			return err
		}
		preserveRuntimeEnvOverrides(obj, existing)
		obj.SetResourceVersion(existing.GetResourceVersion())
		if err := r.Update(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

func preserveRuntimeEnvOverrides(desired, existing *unstructured.Unstructured) {
	if desired == nil || existing == nil {
		return
	}
	kind := desired.GetKind()
	if kind != "Deployment" && kind != "StatefulSet" {
		return
	}
	desiredSpec, found, err := unstructured.NestedMap(desired.Object, "spec", "template", "spec")
	if err != nil || !found {
		return
	}
	existingSpec, found, err := unstructured.NestedMap(existing.Object, "spec", "template", "spec")
	if err != nil || !found {
		return
	}
	desiredContainers, _, _ := unstructured.NestedSlice(desiredSpec, "containers")
	existingContainers, _, _ := unstructured.NestedSlice(existingSpec, "containers")
	if len(desiredContainers) == 0 {
		return
	}
	byName := map[string]map[string]interface{}{}
	for _, container := range existingContainers {
		containerMap, ok := container.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := containerMap["name"].(string)
		if name == "" {
			continue
		}
		byName[name] = containerMap
	}
	for i, container := range desiredContainers {
		containerMap, ok := container.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := containerMap["name"].(string)
		existingContainer, ok := byName[name]
		if !ok {
			continue
		}
		desiredEnv, _, _ := unstructured.NestedSlice(containerMap, "env")
		existingEnv, _, _ := unstructured.NestedSlice(existingContainer, "env")
		if len(desiredEnv) == 0 {
			continue
		}
		existingEnvByName := map[string]map[string]interface{}{}
		for _, envVar := range existingEnv {
			envMap, ok := envVar.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := envMap["name"].(string)
			if key == "" {
				continue
			}
			existingEnvByName[key] = envMap
		}
		for j, envVar := range desiredEnv {
			envMap, ok := envVar.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := envMap["name"].(string)
			if key == "" {
				continue
			}
			if override, exists := existingEnvByName[key]; exists {
				desiredEnv[j] = override
			}
		}
		containerMap["env"] = desiredEnv
		desiredContainers[i] = containerMap
	}
	if err := unstructured.SetNestedSlice(desiredSpec, desiredContainers, "containers"); err != nil {
		return
	}
	if err := unstructured.SetNestedMap(desired.Object, desiredSpec, "spec", "template", "spec"); err != nil {
		return
	}
}

func loadCoreManifest() ([]byte, error) {
	paths := []string{
		"/app/core-resources.yaml",
		"/core-resources.yaml",
		"deploy/operator/core-resources.yaml",
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("core-resource manifest not found")
}
