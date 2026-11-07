package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func namespaces(spec evpnv1.EvpnCoreSpec) (string, string) {
	return defaultString(spec.AppNamespace, "evpn-system"), defaultString(spec.OvnNamespace, "ovn-system")
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validateSpec(spec evpnv1.EvpnCoreSpec) error {
	if spec.Enabled && len(validation.IsDNS1123Subdomain(spec.DataplaneNode)) != 0 || spec.Enabled && spec.DataplaneNode == "" {
		return fmt.Errorf("spec.dataplaneNode must name the single designated Kubernetes Node")
	}
	provider := defaultString(spec.BGP.Provider, "k8gobgp")
	if provider != "k8gobgp" {
		return fmt.Errorf("BGP provider %q is not implemented; only k8gobgp is supported", provider)
	}
	app, ovn := namespaces(spec)
	for _, ns := range []string{app, ovn} {
		if problems := validation.IsDNS1123Label(ns); len(problems) != 0 {
			return fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(problems, ", "))
		}
		if ns == "k8gobgp-system" {
			return fmt.Errorf("k8gobgp-system is reserved for the independent BGP provider")
		}
	}
	n := spec.NodeAgent
	if !n.Enabled {
		return nil
	}
	mode := defaultString(n.Mode, "real-ovn-ovs")
	backend := defaultString(n.NetworkBackend, "linux-veth")
	if mode != "real-ovn-ovs" && mode != "in-memory" {
		return fmt.Errorf("unsupported Node-Agent mode %q", mode)
	}
	if backend != "linux-veth" && backend != "in-memory" {
		return fmt.Errorf("unsupported Node-Agent network backend %q", backend)
	}
	if (n.ExportConfigPath != "" || n.RemoteConfigPath != "" || n.CNIContinuityConfigPath != "") && (mode != "real-ovn-ovs" || backend != "linux-veth") {
		return fmt.Errorf("Type-2 export/import requires real-ovn-ovs/linux-veth")
	}
	for name, value := range map[string]string{"exportInterval": n.ExportInterval, "exportTimeout": n.ExportTimeout, "remoteInterval": n.RemoteInterval, "remoteTimeout": n.RemoteTimeout} {
		if value == "" {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("nodeAgent.%s must be a positive Go duration", name)
		}
	}
	for name, path := range map[string]string{"socket": n.Socket, "exportConfigPath": n.ExportConfigPath,
		"exportStatusPath": n.ExportStatusPath, "remoteConfigPath": n.RemoteConfigPath, "remoteStatusPath": n.RemoteStatusPath,
		"cniContinuityConfigPath": n.CNIContinuityConfigPath} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, "/run/evpn-node-agent/")) {
			return fmt.Errorf("nodeAgent.%s must be a clean absolute path under /run/evpn-node-agent/", name)
		}
	}
	if n.GoBGPHostPath != "" && (!filepath.IsAbs(n.GoBGPHostPath) || filepath.Clean(n.GoBGPHostPath) != n.GoBGPHostPath || n.GoBGPHostPath == "/") {
		return fmt.Errorf("nodeAgent.gobgpHostPath must be a clean absolute directory path other than /")
	}
	if n.ProviderCTRHostPath != "" && (!filepath.IsAbs(n.ProviderCTRHostPath) || filepath.Clean(n.ProviderCTRHostPath) != n.ProviderCTRHostPath || n.ProviderCTRHostPath == "/") {
		return fmt.Errorf("nodeAgent.providerCTRHostPath must be a clean absolute file path")
	}
	if n.CNIContinuityConfigPath != "" && !n.ProviderAttestation {
		return fmt.Errorf("CNI continuity requires administrator-enabled provider attestation runtime access")
	}
	for key, value := range n.NodeSelector {
		if key != "kubernetes.io/hostname" || value != spec.DataplaneNode {
			return fmt.Errorf("multi-node/arbitrary nodeAgent.nodeSelector is unsupported; only kubernetes.io/hostname equal to dataplaneNode is allowed")
		}
		if errs := validation.IsQualifiedName(key); len(errs) != 0 {
			return fmt.Errorf("invalid nodeSelector key %q", key)
		}
		if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
			return fmt.Errorf("invalid nodeSelector value %q", value)
		}
	}
	return nil
}

func renderManifest(data []byte, spec evpnv1.EvpnCoreSpec) ([]*unstructured.Unstructured, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	appNS, ovnNS := namespaces(spec)
	var result []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(&obj.Object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode core manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "Namespace" {
			continue
		}
		if obj.GetName() == "" || obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			return nil, fmt.Errorf("core manifest object requires apiVersion, kind and metadata.name")
		}
		switch obj.GetNamespace() {
		case "", "evpn-system":
			obj.SetNamespace(appNS)
		case "ovn-system":
			obj.SetNamespace(ovnNS)
		}
		if !cleanupAllowed(resourceRef(obj)) {
			return nil, fmt.Errorf("unsupported managed resource %s %s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
		}
		if obj.GetKind() == "DaemonSet" && obj.GetName() == "evpn-node-agent" {
			if !spec.NodeAgent.Enabled {
				continue
			}
			if err := configureNodeAgent(obj, spec.NodeAgent); err != nil {
				return nil, err
			}
		}
		if err := applySpecOverrides(obj, spec); err != nil {
			return nil, err
		}
		if obj.GetKind() == "Deployment" {
			defaults, err := templateEnvDefaults(obj)
			if err != nil {
				return nil, err
			}
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[envDefaultsKey] = defaults
			obj.SetAnnotations(annotations)
		}
		if obj.GetName() == "ovn-ovs" && obj.GetKind() == "Deployment" || obj.GetName() == "evpn-node-agent" && obj.GetKind() == "DaemonSet" {
			if err := configurePlacement(obj, spec); err != nil {
				return nil, err
			}
		}
		result = append(result, obj)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("core manifest contains no managed resources")
	}
	return result, nil
}

func applySpecOverrides(obj *unstructured.Unstructured, spec evpnv1.EvpnCoreSpec) error {
	appNS, ovnNS := namespaces(spec)
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	for i, value := range containers {
		c, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid container in %s", obj.GetName())
		}
		name, _ := c["name"].(string)
		switch {
		case obj.GetName() == "evpn-node-agent":
			c["image"] = defaultString(spec.NodeAgent.Image, "evpn4ovn/node-agent:kind")
		case name == "ovn-ovs":
			c["image"] = defaultString(spec.OvnImage, "evpn4ovn/ovn-ovs:kind")
		default:
			c["image"] = defaultString(spec.AppImage, "evpn4ovn/app:kind")
		}
		envs, _, err := unstructured.NestedSlice(c, "env")
		if err != nil {
			return err
		}
		for j, value := range envs {
			env, ok := value.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid environment entry in %s", obj.GetName())
			}
			key, _ := env["name"].(string)
			switch key {
			case "OVN_NB_ENDPOINT":
				env["value"] = "tcp:ovn-ovs." + ovnNS + ".svc.cluster.local:6641"
				delete(env, "valueFrom")
			case "OVS_DB_ENDPOINT":
				env["value"] = "tcp:ovn-ovs." + ovnNS + ".svc.cluster.local:6640"
				delete(env, "valueFrom")
			case "OVSDB_ADDR":
				env["value"] = "ovsdb-mgmt." + appNS + ".svc.cluster.local:50053"
				delete(env, "valueFrom")
			}
			envs[j] = env
		}
		if len(envs) != 0 {
			c["env"] = envs
		}
		containers[i] = c
	}
	return unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers")
}

func configureNodeAgent(obj *unstructured.Unstructured, n evpnv1.NodeAgentSpec) error {
	podSpec := corev1.PodSpec{}
	raw, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if err != nil || !found {
		return fmt.Errorf("Node-Agent template has no pod spec: %v", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &podSpec); err != nil {
		return err
	}
	if len(podSpec.Containers) != 1 || len(podSpec.InitContainers) != 1 {
		return fmt.Errorf("Node-Agent template requires one agent and one dependency init container")
	}
	if len(n.NodeSelector) != 0 {
		podSpec.NodeSelector = n.NodeSelector
	}
	podSpec.Tolerations = n.Tolerations
	if n.ProviderAttestation {
		// RBAC is deliberately an administrator-installed manifest, not part of
		// the operator's owned resource lifecycle or its write authority.
		podSpec.ServiceAccountName = "evpn-provider-observer"
		automount := true
		podSpec.AutomountServiceAccountToken = &automount
		podSpec.Volumes = append(podSpec.Volumes,
			corev1.Volume{Name: "provider-attestation", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/evpn/provider-attestation", Type: hostPathType(corev1.HostPathDirectory)}}},
			corev1.Volume{Name: "provider-containerd", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd", Type: hostPathType(corev1.HostPathDirectory)}}},
			corev1.Volume{Name: "provider-ctr", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: defaultString(n.ProviderCTRHostPath, "/usr/bin/ctr"), Type: hostPathType(corev1.HostPathFile)}}},
		)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "provider-attestation", MountPath: "/etc/evpn/provider-attestation", ReadOnly: true},
			corev1.VolumeMount{Name: "provider-containerd", MountPath: "/run/containerd", ReadOnly: true},
			corev1.VolumeMount{Name: "provider-ctr", MountPath: "/usr/local/libexec/evpn-ctr", ReadOnly: true},
		)
	}
	mode := defaultString(n.Mode, "real-ovn-ovs")
	nb := defaultString(n.OVNNBEndpoint, "unix:/var/run/ovn/ovnnb_db.sock")
	ovs := defaultString(n.OVSDBEndpoint, "unix:/var/run/openvswitch/db.sock")
	args := []string{
		"--socket=" + defaultString(n.Socket, "/run/evpn-node-agent/agent.sock"),
		"--mode=" + mode, "--network-backend=" + defaultString(n.NetworkBackend, "linux-veth"),
		"--ovn-nb-endpoint=" + nb, "--ovsdb-endpoint=" + ovs,
		"--ovn-sb-endpoint=" + defaultString(n.OVNSBEndpoint, "unix:/var/run/ovn/ovnsb_db.sock"),
		"--evpn-export-config=" + n.ExportConfigPath, "--evpn-export-status=" + n.ExportStatusPath,
		"--evpn-export-interval=" + defaultString(n.ExportInterval, "2s"),
		"--evpn-export-timeout=" + defaultString(n.ExportTimeout, "5s"),
		"--evpn-remote-config=" + n.RemoteConfigPath, "--evpn-remote-status=" + n.RemoteStatusPath,
		"--evpn-remote-interval=" + defaultString(n.RemoteInterval, "2s"),
		"--evpn-remote-timeout=" + defaultString(n.RemoteTimeout, "5s"),
		"--cni-continuity-config=" + n.CNIContinuityConfigPath,
	}

	podSpec.Containers[0].Args = args
	podSpec.Containers[0].Image = defaultString(n.Image, "evpn4ovn/node-agent:kind")
	podSpec.InitContainers[0].Image = podSpec.Containers[0].Image
	var dependencies []string
	if mode == "real-ovn-ovs" {
		for _, endpoint := range []string{nb, ovs} {
			if strings.HasPrefix(endpoint, "unix:") {
				dependencies = append(dependencies, "test -S "+shellQuote(strings.TrimPrefix(endpoint, "unix:")))
			}
		}
	}
	for _, path := range []string{n.ExportConfigPath, n.RemoteConfigPath, n.CNIContinuityConfigPath} {
		if path != "" {
			dependencies = append(dependencies, "test -r "+shellQuote(path))
		}
	}
	check := ":"
	if len(dependencies) != 0 {
		check = strings.Join(dependencies, " && ")
	}
	podSpec.InitContainers[0].Args = []string{"for i in $(seq 1 180); do if " + check + "; then exit 0; fi; sleep 1; done; echo 'Node-Agent dependencies unavailable' >&2; exit 1"}
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "gobgp" {
			podSpec.Volumes[i].HostPath.Path = defaultString(n.GoBGPHostPath, "/run/gobgp")
		}
	}
	socket := shellQuote(defaultString(n.Socket, "/run/evpn-node-agent/agent.sock"))
	podSpec.Containers[0].ReadinessProbe.Exec.Command = []string{"/bin/sh", "-ec", "nc -z -U " + socket}
	data, err = json.Marshal(podSpec)
	if err != nil {
		return err
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(data, &updated); err != nil {
		return err
	}
	return unstructured.SetNestedMap(obj.Object, updated, "spec", "template", "spec")
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

// Only an explicitly changed GoBGP endpoint is a supported legacy override.
// Unchanged old defaults must not mask newer template defaults.
func preserveRuntimeEnvOverrides(desired, existing *unstructured.Unstructured) error {
	if desired.GetKind() != "Deployment" {
		return nil
	}
	next, _, err := unstructured.NestedSlice(desired.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	old, _, err := unstructured.NestedSlice(existing.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	var applied unstructured.Unstructured
	if err := json.Unmarshal([]byte(existing.GetAnnotations()[lastAppliedKey]), &applied.Object); err != nil {
		return fmt.Errorf("decode previous environment defaults: %w", err)
	}
	defaults, _, err := unstructured.NestedSlice(applied.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	if encoded := existing.GetAnnotations()[envDefaultsKey]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &defaults); err != nil {
			return fmt.Errorf("decode template environment defaults: %w", err)
		}
	}
	for _, value := range next {
		c, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid desired container")
		}
		for _, previous := range old {
			p, ok := previous.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid existing container")
			}
			if c["name"] != p["name"] {
				continue
			}
			env, _, err := unstructured.NestedSlice(c, "env")
			if err != nil {
				return err
			}
			oldEnv, _, err := unstructured.NestedSlice(p, "env")
			if err != nil {
				return err
			}
			for i, value := range env {
				e, ok := value.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid desired environment entry")
				}
				if e["name"] != "GOBGP_ENDPOINT" {
					continue
				}
				for _, previous := range oldEnv {
					o, ok := previous.(map[string]interface{})
					if !ok {
						return fmt.Errorf("invalid existing environment entry")
					}
					if e["name"] == o["name"] && !previousEnvEqual(defaults, c["name"], o) {
						env[i] = o
						break
					}

				}
			}
			if len(env) != 0 {
				c["env"] = env
			}
		}
	}
	return unstructured.SetNestedSlice(desired.Object, next, "spec", "template", "spec", "containers")
}

func templateEnvDefaults(obj *unstructured.Unstructured) (string, error) {
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return "", err
	}
	result := []interface{}{}
	for _, value := range containers {
		c, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("invalid template container")
		}
		envs, _, err := unstructured.NestedSlice(c, "env")
		if err != nil {
			return "", err
		}
		for _, value := range envs {
			e, ok := value.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("invalid template environment entry")
			}
			if e["name"] == "GOBGP_ENDPOINT" {
				result = append(result, map[string]interface{}{"name": c["name"], "env": []interface{}{e}})
			}
		}
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func previousEnvEqual(containers []interface{}, containerName interface{}, env map[string]interface{}) bool {
	for _, value := range containers {
		c, ok := value.(map[string]interface{})
		if !ok || c["name"] != containerName {
			continue
		}
		envs, _, err := unstructured.NestedSlice(c, "env")
		if err != nil {
			return false
		}
		for _, previous := range envs {
			e, ok := previous.(map[string]interface{})
			if ok && e["name"] == env["name"] {
				return controlledFieldsEqual(e, env) && controlledFieldsEqual(env, e)
			}
		}
	}
	return false
}

func loadCoreManifest() ([]byte, error) {
	for _, path := range []string{"/app/core-resources.yaml", "/core-resources.yaml", "deploy/operator/core-resources.yaml"} {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read core manifest %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("core-resource manifest not found")
}
