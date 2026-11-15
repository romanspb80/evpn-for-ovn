package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const inventoryConfigMap = "evpn-worker-inventory"
const inventoryOwnerKey = "evpn.romanspb80.github.io/installation-uid"

// InventoryGateOptions pins the API object and trust scope independently of
// the ConfigMap data. The caller must obtain these values from a trusted source.
type InventoryGateOptions struct {
	Namespace, ConfigMapUID, ClusterUID, InstallationUID string
	JournalDirectory, ExportConfigPath, ImportConfigPath string
	ContinuityConfigPath, OVSDBEndpoint, SBEndpoint      string
}

type inventorySource interface {
	ConfigMaps(string) coreclient.ConfigMapInterface
	Nodes() coreclient.NodeInterface
}

type InventoryGate struct {
	mu         sync.Mutex
	source     inventorySource
	options    InventoryGateOptions
	nodeName   string
	nodeUID    string
	continuity CNIContinuityConfig
	importer   RemoteRuntimeConfig
	fence      *workerinventory.Fence
	chassis    func(context.Context, string) error
	speaker    func(context.Context, workerinventory.Worker) error
	loadExport func(string) (evpn.ExporterConfig, error)
	loadImport func(string) (RemoteRuntimeConfig, error)
	loadCNI    func(string) (CNIContinuityConfig, error)
	checkVTEP  func(evpn.ExporterConfig) error
}

// OpenInventoryGate uses only in-cluster credentials and requires both Type-2
// runtimes and CNI continuity to share the independently configured node.
func OpenInventoryGate(opts InventoryGateOptions) (*InventoryGate, error) {
	if opts.Namespace == "" || opts.ConfigMapUID == "" || opts.ClusterUID == "" || opts.InstallationUID == "" ||
		opts.JournalDirectory == "" || opts.ExportConfigPath == "" || opts.ImportConfigPath == "" ||
		opts.ContinuityConfigPath == "" || opts.OVSDBEndpoint != "unix:/var/run/openvswitch/db.sock" || opts.SBEndpoint == "" {
		return nil, fmt.Errorf("inventory gate requires pinned scope, object UID, journal, both runtime configs, CNI config and local OVS/SB endpoints")
	}
	if !filepath.IsAbs(opts.JournalDirectory) || filepath.Clean(opts.JournalDirectory) != opts.JournalDirectory || opts.JournalDirectory == "/" {
		return nil, fmt.Errorf("inventory fence requires a clean absolute journal directory")
	}
	node := os.Getenv("NODE_NAME")
	if node == "" {
		return nil, fmt.Errorf("inventory gate requires NODE_NAME from the Kubernetes pod")
	}
	continuity, err := LoadCNIContinuityConfig(opts.ContinuityConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load inventory CNI continuity configuration: %w", err)
	}
	importer, err := LoadRemoteRuntimeConfig(opts.ImportConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load inventory importer configuration: %w", err)
	}
	if continuity.NodeName != node || continuity.Domain.InstallationUID != opts.InstallationUID || continuity.Domain.NodeUID == "" {
		return nil, fmt.Errorf("inventory gate CNI node or installation identity differs")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("inventory in-cluster credentials: %w", err)
	}
	config.Timeout = 5 * time.Second
	client, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	fence, err := workerinventory.OpenFence(opts.JournalDirectory, opts.ClusterUID, opts.InstallationUID, node, continuity.Domain.NodeUID)
	if err != nil {
		return nil, fmt.Errorf("open inventory acceptance fence: %w", err)
	}
	g := &InventoryGate{source: client, options: opts, nodeName: node, nodeUID: continuity.Domain.NodeUID,
		continuity: continuity, importer: importer, fence: fence, loadExport: LoadExporterConfig,
		loadImport: LoadRemoteRuntimeConfig, loadCNI: LoadCNIContinuityConfig, checkVTEP: validateLocalExportConfig}
	g.chassis = func(ctx context.Context, expected string) error {
		ovs, err := newOVSClient(ctx, opts.OVSDBEndpoint)
		if err != nil {
			return err
		}
		defer ovs.Close()
		sb, err := newExportSBClient(ctx, opts.SBEndpoint)
		if err != nil {
			return err
		}
		defer sb.Close()
		return verifyLocalOVNChassis(ctx, ovs, sb, expected)
	}
	g.speaker = func(ctx context.Context, w workerinventory.Worker) error {
		conn, continuity, client, err := newProviderTransport(w.AttestationPath, w.SpeakerEndpoint, w.SpeakerASN,
			w.SpeakerRouterID, opts.InstallationUID, w.NodeUID)
		if err != nil {
			return err
		}
		gate := attestedReadiness{evpn.SpeakerReadiness{Client: client, ASN: w.SpeakerASN, RouterID: w.SpeakerRouterID}, continuity}
		err = gate.Check(ctx)
		return errors.Join(err, conn.Close(), continuity.Close())
	}
	return g, nil
}

func (g *InventoryGate) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fence.Close()
}

func (g *InventoryGate) CheckExporter(ctx context.Context, cfg evpn.ExporterConfig) error {
	return g.check(ctx, &cfg, nil)
}

func (g *InventoryGate) CheckImporter(ctx context.Context, cfg RemoteRuntimeConfig) error {
	return g.check(ctx, nil, &cfg)
}

func (g *InventoryGate) ValidateContinuity(cfg CNIContinuityConfig) error {
	if !reflect.DeepEqual(g.continuity, cfg) {
		return fmt.Errorf("inventory CNI configuration changed during startup")
	}
	return nil
}

func (g *InventoryGate) check(ctx context.Context, expectedExport *evpn.ExporterConfig, expectedImport *RemoteRuntimeConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cm, err := g.source.ConfigMaps(g.options.Namespace).Get(ctx, inventoryConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("inventory publication unavailable: %w", err)
	}
	in, err := verifyInventoryPublication(cm, g.options)
	if err != nil {
		return err
	}
	node, err := g.source.Nodes().Get(ctx, g.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("inventory Node lookup: %w", err)
	}
	if err := verifyInventoryNode(node, g.nodeName, g.nodeUID); err != nil {
		return err
	}
	worker, err := in.Select(g.nodeName, g.nodeUID)
	if err != nil {
		return err
	}
	exporter, err := g.loadExport(g.options.ExportConfigPath)
	if err != nil {
		return fmt.Errorf("load inventory exporter configuration: %w", err)
	}
	importer, err := g.loadImport(g.options.ImportConfigPath)
	if err != nil {
		return fmt.Errorf("load inventory importer configuration: %w", err)
	}
	continuity, err := g.loadCNI(g.options.ContinuityConfigPath)
	if err != nil {
		return fmt.Errorf("load inventory continuity configuration: %w", err)
	}
	if !reflect.DeepEqual(importer, g.importer) || !reflect.DeepEqual(continuity, g.continuity) {
		return fmt.Errorf("inventory launch-bound importer or CNI configuration changed")
	}
	if expectedExport != nil && !reflect.DeepEqual(exporter, *expectedExport) ||
		expectedImport != nil && !reflect.DeepEqual(importer, *expectedImport) {
		return fmt.Errorf("inventory runtime configuration changed during validation")
	}
	if err := validateInventoryConfiguration(worker, exporter, importer, continuity, g.options); err != nil {
		return err
	}
	if err := g.checkVTEP(exporter); err != nil {
		return err
	}
	if err := g.chassis(ctx, worker.Chassis); err != nil {
		return fmt.Errorf("inventory local chassis verification: %w", err)
	}
	if err := g.speaker(ctx, worker); err != nil {
		return fmt.Errorf("inventory speaker attestation: %w", err)
	}
	if _, err := g.fence.Accept(in); err != nil {
		return fmt.Errorf("inventory durable acceptance: %w", err)
	}
	return nil
}

func verifyInventoryNode(node *corev1.Node, name, uid string) error {
	if node == nil || name == "" || uid == "" || node.Name != name || string(node.UID) != uid || !node.DeletionTimestamp.IsZero() {
		return fmt.Errorf("inventory Node name or UID changed")
	}
	return nil
}

func verifyInventoryPublication(cm *corev1.ConfigMap, opts InventoryGateOptions) (workerinventory.Inventory, error) {
	var in workerinventory.Inventory
	if cm == nil || cm.Name != inventoryConfigMap || cm.Namespace != opts.Namespace || string(cm.UID) != opts.ConfigMapUID ||
		!cm.DeletionTimestamp.IsZero() || cm.Labels["app.kubernetes.io/managed-by"] != "evpn-core-operator" ||
		cm.Labels[inventoryOwnerKey] != opts.InstallationUID || cm.Annotations[inventoryOwnerKey] != opts.InstallationUID {
		return in, fmt.Errorf("inventory publication identity or ownership mismatch")
	}
	var applied corev1.ConfigMap
	if err := json.Unmarshal([]byte(cm.Annotations["evpn.romanspb80.github.io/last-applied"]), &applied); err != nil ||
		applied.Name != cm.Name || applied.Namespace != cm.Namespace || applied.APIVersion != "v1" || applied.Kind != "ConfigMap" ||
		applied.Annotations[inventoryOwnerKey] != opts.InstallationUID || !reflect.DeepEqual(applied.Data, cm.Data) {
		return in, fmt.Errorf("inventory publication differs from its Operator apply journal")
	}
	d := json.NewDecoder(bytes.NewBufferString(cm.Data["inventory.json"]))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid inventory publication: %w", err)
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return in, fmt.Errorf("inventory publication contains trailing data")
	}
	canonical, digest, err := in.Canonical()
	if err != nil {
		return in, err
	}
	if string(canonical) != cm.Data["inventory.json"] || cm.Data["sha256"] != digest ||
		cm.Data["revision"] != strconv.FormatUint(in.Revision, 10) ||
		in.ClusterUID != opts.ClusterUID || in.InstallationUID != opts.InstallationUID ||
		cm.Data["installationUID"] != opts.InstallationUID {
		return in, fmt.Errorf("inventory canonical content, digest, revision or scope differs")
	}
	return in, nil
}

func validateInventoryConfiguration(w workerinventory.Worker, export evpn.ExporterConfig, remote RemoteRuntimeConfig, cni CNIContinuityConfig, opts InventoryGateOptions) error {
	if !export.Enabled || export.DryRun || !remote.Enabled || export.Recovery == nil || remote.Netlink.Recovery == nil ||
		cni.Admission == nil || export.NodeName != w.NodeName || cni.NodeName != w.NodeName ||
		export.LocalChassis != w.Chassis || export.GRPCEndpoint != w.SpeakerEndpoint ||
		export.RouterID != w.SpeakerRouterID || export.ASN != w.SpeakerASN ||
		export.AttestationConfigPath != w.AttestationPath || opts.ContinuityConfigPath != w.AuthorizationPath ||
		export.Recovery.StateDirectory != w.ExportJournal || remote.Netlink.StateDirectory != w.ImportJournal ||
		cni.JournalDirectory != w.ContinuityJournal {
		return fmt.Errorf("inventory worker differs from enabled runtime, trust or journal configuration")
	}
	if opts.JournalDirectory != "" && (opts.JournalDirectory == w.ExportJournal || opts.JournalDirectory == w.ImportJournal ||
		opts.JournalDirectory == w.ContinuityJournal) {
		return fmt.Errorf("inventory fence requires a separate journal directory")
	}
	if err := cni.Admission.validate(); err != nil {
		return fmt.Errorf("inventory CNI admission: %w", err)
	}
	for _, domain := range [][2]string{{export.Recovery.Domain.InstallationUID, export.Recovery.Domain.NodeUID},
		{remote.Netlink.Recovery.Domain.InstallationUID, remote.Netlink.Recovery.Domain.NodeUID},
		{cni.Domain.InstallationUID, cni.Domain.NodeUID}} {
		if domain[0] != opts.InstallationUID || domain[1] != w.NodeUID {
			return fmt.Errorf("inventory worker recovery domain mismatch")
		}
	}
	if err := ValidateCohostedType2Config(export, remote); err != nil {
		return err
	}
	if remote.Import.LocalVTEP != w.LocalVTEP || len(export.Networks) != len(w.Networks) ||
		len(remote.Netlink.Networks) != len(w.Networks) || len(remote.Import.Networks) != len(w.Networks) || len(cni.Networks) != len(w.Networks) {
		return fmt.Errorf("inventory worker VTEP or network count mismatch")
	}
	for _, n := range w.Networks {
		exportMatch, netlinkMatch, importMatch := false, false, false
		for _, x := range export.Networks {
			exportMatch = exportMatch || (x.Enabled && x.NetworkID == n.ID && x.LogicalSwitch == n.LogicalSwitch && x.VNI == n.VNI && x.LocalVTEP == w.LocalVTEP)
		}
		for _, x := range remote.Netlink.Networks {
			netlinkMatch = netlinkMatch || (x.NetworkID == n.ID && x.LogicalSwitch == n.LogicalSwitch && x.VNI == n.VNI)
		}
		for _, x := range remote.Import.Networks {
			importMatch = importMatch || (x.NetworkID == n.ID && x.VNI == n.VNI)
		}
		if !exportMatch || !netlinkMatch || !importMatch || cni.Networks[n.ID] != n.LogicalSwitch {
			return fmt.Errorf("inventory network %s differs from actual runtime configuration", n.ID)
		}
	}
	return nil
}
