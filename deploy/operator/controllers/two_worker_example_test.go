package controllers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestTwoWorkerExampleRendersInventoryGate(t *testing.T) {
	dir := filepath.Join("..", "examples", "two-worker")
	data, err := os.ReadFile(filepath.Join(dir, "evpncore.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	render := strings.NewReplacer(
		"<NODE_A>", "worker-a", "<NODE_B>", "worker-b",
		"<NODE_A_UID>", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"<NODE_B_UID>", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"<CHASSIS_A>", "chassis-a", "<CHASSIS_B>", "chassis-b",
		"<CLUSTER_UID>", inventoryCluster, "<NODE_AGENT_IMAGE>", "example.invalid/agent:example",
		"<OVN_NB_HOST>", "ovn-nb.example", "<OVN_SB_HOST>", "ovn-sb.example")
	var core evpnv1.EvpnCore
	if err := yaml.UnmarshalStrict([]byte(render.Replace(string(data))), &core); err != nil {
		t.Fatal(err)
	}
	core.UID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if err := validateSpec(core.Spec); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "core-resources.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := renderManifest(manifest, core.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := wireInventoryDaemonSet(objects, &core, "inventory-uid"); err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if obj.GetKind() != "DaemonSet" || obj.GetName() != "evpn-node-agent" {
			continue
		}
		var ds appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &ds); err != nil {
			t.Fatal(err)
		}
		if !inventoryPodIdentityMatches(ds.Spec.Template.Spec) ||
			!inventoryArgsMatch(ds.Spec.Template.Spec.Containers, inventoryArguments(&core, "inventory-uid")) {
			t.Fatal("example did not render the expected gated DaemonSet")
		}
		return
	}
	t.Fatal("example did not render a Node-Agent DaemonSet")
}
