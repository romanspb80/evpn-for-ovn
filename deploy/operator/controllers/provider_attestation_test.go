package controllers

import (
	"encoding/json"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProviderAttestationManifest(t *testing.T) {
	r := newTestReconciler(t)
	for _, enabled := range []bool{false, true} {
		spec := evpnv1.EvpnCoreSpec{Enabled: true, DataplaneNode: "worker",
			NodeAgent: evpnv1.NodeAgentSpec{Enabled: true, ProviderAttestation: enabled, ProviderCTRHostPath: "/opt/ctr"}}
		if err := validateSpec(spec); err != nil {
			t.Fatal(err)
		}
		objects, err := renderManifest(r.ManifestData, spec)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, obj := range objects {
			if obj.GetName() != "evpn-node-agent" {
				continue
			}
			found = true
			value, _, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var pod corev1.PodSpec
			if err := json.Unmarshal(data, &pod); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, mount := range pod.Containers[0].VolumeMounts {
				if mount.Name != "provider-ctr" && mount.Name != "provider-containerd" && mount.Name != "provider-attestation" {
					continue
				}
				count++
				if !mount.ReadOnly {
					t.Fatal("runtime inspection mount is not read-only")
				}
			}
			if enabled {
				if count != 3 || pod.ServiceAccountName != "evpn-provider-observer" || pod.AutomountServiceAccountToken == nil || !*pod.AutomountServiceAccountToken {
					t.Fatal("provider observer identity/mounts unavailable")
				}
				for _, volume := range pod.Volumes {
					if volume.Name == "provider-ctr" && (volume.HostPath == nil || volume.HostPath.Path != "/opt/ctr") {
						t.Fatal("ctr host pin path ignored")
					}
				}
			} else if count != 0 || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Fatal("disabled observer unexpectedly exposes runtime/token")
			}
		}
		if !found {
			t.Fatal("node agent not rendered")
		}
	}
	if err := validateSpec(evpnv1.EvpnCoreSpec{NodeAgent: evpnv1.NodeAgentSpec{Enabled: true, ProviderCTRHostPath: "../ctr"}}); err == nil {
		t.Fatal("unsafe ctr path accepted")
	}
}
