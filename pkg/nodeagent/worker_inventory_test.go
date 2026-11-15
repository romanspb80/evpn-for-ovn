package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func inventoryTestFixture(t *testing.T) (workerinventory.Worker, evpn.ExporterConfig, RemoteRuntimeConfig, CNIContinuityConfig, InventoryGateOptions, *corev1.ConfigMap) {
	t.Helper()
	worker := workerinventory.Worker{NodeName: "worker-a", NodeUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Chassis: "chassis-a", SpeakerRouterID: "192.0.2.32", SpeakerASN: 64512, SpeakerEndpoint: "unix:///var/run/gobgp/gobgp.sock",
		LocalVTEP: "192.0.2.32", AttestationPath: "/etc/evpn/provider.json", AuthorizationPath: "/run/evpn-node-agent/cni.json",
		ExportJournal: "/var/lib/evpn/export", ImportJournal: "/var/lib/evpn/import", ContinuityJournal: "/var/lib/evpn/cni",
		Networks: []workerinventory.Network{{ID: "n24", LogicalSwitch: "ls24b", VNI: 100}}}
	opts := InventoryGateOptions{Namespace: "evpn-system", ConfigMapUID: "resource-uid",
		ClusterUID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InstallationUID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		ContinuityConfigPath: worker.AuthorizationPath}
	domain := ownership.Domain{InstallationUID: opts.InstallationUID, NodeUID: worker.NodeUID}
	export := evpn.ExporterConfig{Enabled: true, ProcessContinuity: true, NodeName: worker.NodeName, LocalChassis: worker.Chassis,
		GRPCEndpoint: worker.SpeakerEndpoint, ASN: worker.SpeakerASN, RouterID: worker.SpeakerRouterID,
		AttestationConfigPath: worker.AttestationPath, Recovery: &evpn.ExportRecoveryConfig{StateDirectory: worker.ExportJournal, Domain: domain},
		Networks: []evpn.EVPNNetworkConfig{{Enabled: true, NetworkID: "n24", LogicalSwitch: "ls24b", VNI: 100, LocalVTEP: worker.LocalVTEP}}}
	remote := validRemoteRuntimeConfig()
	remote.AttestationConfigPath = worker.AttestationPath
	remote.Netlink.StateDirectory = worker.ImportJournal
	remote.Netlink.Recovery = &evpn.RemoteNetlinkRecovery{Domain: domain}
	cni := CNIContinuityConfig{NodeName: worker.NodeName, JournalDirectory: worker.ContinuityJournal,
		Domain: domain, Networks: map[string]string{"n24": "ls24b"}, Admission: &CNIAdmissionConfig{
			CNIExecutableSHA256: strings.Repeat("a", 64), MultusExecutableSHA256: strings.Repeat("b", 64),
			RuntimeExecutableSHA256: strings.Repeat("c", 64)}}
	in := workerinventory.Inventory{Version: 1, Revision: 1, ClusterUID: opts.ClusterUID, InstallationUID: opts.InstallationUID, Workers: []workerinventory.Worker{worker}}
	raw, digest, err := in.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: inventoryConfigMap, Namespace: opts.Namespace, UID: types.UID(opts.ConfigMapUID),
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "evpn-core-operator", inventoryOwnerKey: opts.InstallationUID},
		Annotations: map[string]string{inventoryOwnerKey: opts.InstallationUID}},
		Data: map[string]string{"inventory.json": string(raw), "sha256": digest, "revision": "1", "installationUID": opts.InstallationUID}}
	applied := cm.DeepCopy()
	applied.APIVersion, applied.Kind = "v1", "ConfigMap"
	journal, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	cm.Annotations["evpn.romanspb80.github.io/last-applied"] = string(journal)
	return worker, export, remote, cni, opts, cm
}

func TestInventoryPublicationIdentityAndIntegrity(t *testing.T) {
	_, _, _, _, opts, cm := inventoryTestFixture(t)
	if _, err := verifyInventoryPublication(cm, opts); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*corev1.ConfigMap){
		func(c *corev1.ConfigMap) { c.UID = "replacement" },
		func(c *corev1.ConfigMap) { c.Namespace = "other" },
		func(c *corev1.ConfigMap) { c.Labels["app.kubernetes.io/managed-by"] = "other" },
		func(c *corev1.ConfigMap) { c.Annotations[inventoryOwnerKey] = "other" },
		func(c *corev1.ConfigMap) { c.Data["sha256"] = strings.Repeat("0", 64) },
		func(c *corev1.ConfigMap) { c.Data["revision"] = "2" },
		func(c *corev1.ConfigMap) { c.Data["inventory.json"] += " " },
		func(c *corev1.ConfigMap) { c.Data["installationUID"] = "other" },
	} {
		other := cm.DeepCopy()
		mutate(other)
		if _, err := verifyInventoryPublication(other, opts); err == nil {
			t.Fatal("invalid inventory publication accepted")
		}
	}
	opts.InstallationUID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if _, err := verifyInventoryPublication(cm, opts); err == nil {
		t.Fatal("wrong installation accepted")
	}
}

func TestInventoryRuntimeAssignment(t *testing.T) {
	w, export, remote, cni, opts, _ := inventoryTestFixture(t)
	if err := validateInventoryConfiguration(w, export, remote, cni, opts); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*workerinventory.Worker){
		func(w *workerinventory.Worker) { w.NodeUID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" },
		func(w *workerinventory.Worker) { w.Chassis = "other" },
		func(w *workerinventory.Worker) { w.LocalVTEP = "192.0.2.33" },
		func(w *workerinventory.Worker) { w.SpeakerRouterID = "192.0.2.33" },
		func(w *workerinventory.Worker) { w.AuthorizationPath = "/different" },
		func(w *workerinventory.Worker) { w.ExportJournal = "/different" },
		func(w *workerinventory.Worker) { w.Networks[0].VNI++ },
	} {
		bad := w
		bad.Networks = append([]workerinventory.Network(nil), w.Networks...)
		mutate(&bad)
		if err := validateInventoryConfiguration(bad, export, remote, cni, opts); err == nil {
			t.Fatal("mismatched worker assignment accepted")
		}
	}
}

func TestInventoryNodeIdentity(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "node-uid"}}
	if err := verifyInventoryNode(node, "worker-a", "node-uid"); err != nil {
		t.Fatal(err)
	}
	if err := verifyInventoryNode(node, "worker-b", "node-uid"); err == nil {
		t.Fatal("wrong node accepted")
	}
	if err := verifyInventoryNode(node, "worker-a", "replacement-uid"); err == nil {
		t.Fatal("replacement node accepted")
	}
}

func TestInventoryGateBlocksImporterBeforeTransport(t *testing.T) {
	_, _, remote, _, _, _ := inventoryTestFixture(t)
	path := filepath.Join(t.TempDir(), "remote.json")
	raw, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	// No recovery or attestation is needed to exercise pre-transport gating.
	remote.Netlink.Recovery = nil
	remote.AttestationConfigPath = ""
	raw, err = json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	blocked := errors.New("inventory rejected")
	err = RunRemoteType2Importer(context.Background(), RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth", OVNNBEndpoint: "unused"},
		RemoteRuntimeOptions{ConfigPath: path, Interval: time.Second, Timeout: time.Second,
			InventoryGate: func(context.Context, RemoteRuntimeConfig) error { return blocked }}, func(RemoteRuntimeStatus) {})
	if !errors.Is(err, blocked) {
		t.Fatalf("importer did not stop at inventory gate: %v", err)
	}
}

func TestInventoryGateBlocksExporterCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(path, []byte(`{"Enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var observed ExportStatus
	err := NewCNIServer(nil).RunEndpointExporter(ctx, RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"},
		ExportRuntimeOptions{ConfigPath: path, SBEndpoint: "unused", Interval: time.Second, Timeout: time.Second,
			InventoryGate: func(context.Context, evpn.ExporterConfig) error { return errors.New("inventory rejected") }},
		func(s ExportStatus) { observed = s; cancel() })
	if err != nil || !observed.Result.OwnershipUnverified || !strings.Contains(observed.Error, "inventory rejected") {
		t.Fatalf("exporter did not block: %+v %v", observed, err)
	}
}

func TestInventoryGateDurableAcceptanceAndRestart(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("inventory ownership journal requires root")
	}
	w, export, remote, cni, opts, cm := inventoryTestFixture(t)
	opts.JournalDirectory = filepath.Join(t.TempDir(), "inventory")
	source := fake.NewSimpleClientset(cm, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.NodeName, UID: types.UID(w.NodeUID)}})
	open := func() *InventoryGate {
		t.Helper()
		fence, err := workerinventory.OpenFence(opts.JournalDirectory, opts.ClusterUID, opts.InstallationUID, w.NodeName, w.NodeUID)
		if err != nil {
			t.Fatal(err)
		}
		return &InventoryGate{source: source.CoreV1(), options: opts, nodeName: w.NodeName, nodeUID: w.NodeUID,
			continuity: cni, importer: remote, fence: fence,
			loadExport: func(string) (evpn.ExporterConfig, error) { return export, nil },
			loadImport: func(string) (RemoteRuntimeConfig, error) { return remote, nil },
			loadCNI:    func(string) (CNIContinuityConfig, error) { return cni, nil },
			checkVTEP:  func(evpn.ExporterConfig) error { return nil },
			chassis:    func(context.Context, string) error { return nil },
			speaker:    func(context.Context, workerinventory.Worker) error { return nil }}
	}
	ctx := context.Background()
	gate := open()
	if err := gate.CheckExporter(ctx, export); err != nil {
		t.Fatal(err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	gate = open()
	defer gate.Close()
	if err := gate.CheckImporter(ctx, remote); err != nil {
		t.Fatalf("accepted revision not recovered: %v", err)
	}
	in, err := verifyInventoryPublication(cm, opts)
	if err != nil {
		t.Fatal(err)
	}
	in.Revision = 2
	raw, digest, err := in.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	next := cm.DeepCopy()
	next.Data["inventory.json"], next.Data["sha256"], next.Data["revision"] = string(raw), digest, "2"
	applied := next.DeepCopy()
	delete(applied.Annotations, "evpn.romanspb80.github.io/last-applied")
	applied.APIVersion, applied.Kind = "v1", "ConfigMap"
	journal, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	next.Annotations["evpn.romanspb80.github.io/last-applied"] = string(journal)
	if _, err := source.CoreV1().ConfigMaps(opts.Namespace).Update(ctx, next, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := gate.CheckImporter(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := source.CoreV1().ConfigMaps(opts.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := gate.CheckExporter(ctx, export); err == nil || !strings.Contains(err.Error(), "below accepted revision") {
		t.Fatalf("stale revision accepted: %v", err)
	}
	node, err := source.CoreV1().Nodes().Get(ctx, w.NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.UID = "replacement"
	if _, err := source.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := gate.CheckImporter(ctx, remote); err == nil || !strings.Contains(err.Error(), "Node name or UID") {
		t.Fatalf("wrong node accepted: %v", err)
	}
}
