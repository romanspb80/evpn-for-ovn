package nodeagent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	"sigs.k8s.io/yaml"
)

func TestTwoWorkerExampleConfigurations(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "operator", "examples", "two-worker")
	replacements := strings.NewReplacer(
		"<NODE_A>", "worker-a", "<NODE_B>", "worker-b",
		"<NODE_A_UID>", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"<NODE_B_UID>", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"<CHASSIS_A>", "chassis-a", "<CHASSIS_B>", "chassis-b",
		"<CLUSTER_UID>", "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"<NODE_AGENT_IMAGE>", "example.invalid/node-agent:example",
		"<OVN_NB_HOST>", "ovn-nb.example", "<OVN_SB_HOST>", "ovn-sb.example")
	raw, err := os.ReadFile(filepath.Join(dir, "evpncore.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var core evpnv1.EvpnCore
	if err := yaml.UnmarshalStrict([]byte(replacements.Replace(string(raw))), &core); err != nil {
		t.Fatal(err)
	}
	installation := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	in := workerinventory.Inventory{Version: 1, Revision: core.Spec.WorkerInventory.Revision,
		ClusterUID: core.Spec.WorkerInventory.ClusterUID, InstallationUID: installation,
		Workers: core.Spec.WorkerInventory.Workers}
	if _, _, err := in.Canonical(); err != nil {
		t.Fatal(err)
	}
	for _, worker := range in.Workers {
		t.Run(worker.NodeName, func(t *testing.T) {
			replace := strings.NewReplacer(
				"<NODE>", worker.NodeName, "<CHASSIS>", worker.Chassis, "<NODE_UID>", worker.NodeUID,
				"<INSTALLATION_UID>", installation, "<VTEP>", worker.LocalVTEP,
				"<BOOT_ID>", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "<HOST_NETNS>", "net:[4026531993]",
				"<SERVICE_UID>", "ffffffff-ffff-4fff-8fff-ffffffffffff", "<SPEAKER_CGROUP>", "/system.slice/gobgpd.service",
				"<CTR_SHA256>", strings.Repeat("a", 64), "<CNI_SHA256>", strings.Repeat("b", 64),
				"<MULTUS_SHA256>", strings.Repeat("c", 64), "<RUNTIME_SHA256>", strings.Repeat("d", 64),
				"<GOBGPD_SHA256>", strings.Repeat("e", 64), "<GOBGP_CONFIG_SHA256>", strings.Repeat("f", 64))
			load := func(name string, out interface{}) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				d := json.NewDecoder(bytes.NewBufferString(replace.Replace(string(data))))
				d.DisallowUnknownFields()
				if err := d.Decode(out); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			var export evpn.ExporterConfig
			var remote RemoteRuntimeConfig
			var cni CNIContinuityConfig
			var provider ProviderAttestationConfig
			load("export.json", &export)
			load("import.json", &remote)
			load("cni.json", &cni)
			load("provider.json", &provider)
			if err := export.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := remote.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := provider.validate(); err != nil {
				t.Fatal(err)
			}
			opts := InventoryGateOptions{InstallationUID: installation, ContinuityConfigPath: core.Spec.NodeAgent.CNIContinuityConfigPath,
				JournalDirectory: "/var/lib/evpn-node-agent/inventory"}
			if err := validateInventoryConfiguration(worker, export, remote, cni, opts); err != nil {
				t.Fatal(err)
			}
			if provider.Standalone.NodeUID != worker.NodeUID || provider.Standalone.InstallationUID != installation ||
				provider.Standalone.RouterID != worker.SpeakerRouterID || provider.Standalone.ASN != worker.SpeakerASN {
				t.Fatal("attestation differs from worker identity")
			}
		})
	}
}
