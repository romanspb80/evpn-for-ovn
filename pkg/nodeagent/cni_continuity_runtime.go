package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CNIContinuityConfig is trusted root-owned configuration, not CNI input.
// Networks is the complete immutable network-ID to logical-switch mapping.
type CNIContinuityConfig struct {
	JournalDirectory string
	Create           bool
	Domain           ownership.Domain
	NodeName         string
	Kubeconfig       string
	CRIEndpoint      string
	CTRPath          string
	CTRSHA256        string
	Networks         map[string]string
	Admission        *CNIAdmissionConfig
}

func LoadCNIContinuityConfig(path string) (CNIContinuityConfig, error) {
	var cfg CNIContinuityConfig
	f, err := openAdministratorFile(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return cfg, err
	}
	if st.Uid != 0 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Nlink != 1 {
		return cfg, errors.New("CNI config must be root-owned non-writable regular single-link file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return cfg, err
	}
	if len(data) > 65536 {
		return cfg, errors.New("CNI config too large")
	}
	if err := decodeCNIState(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// EnableCNIContinuity restores only fully independently verified ACKs before
// socket preparation. Close must run after RPCs have drained.
func (s *CNIServer) EnableCNIContinuity(ctx context.Context, cfg CNIContinuityConfig, opts RuntimeOptions) (func() error, error) {
	lifetime := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if opts.Mode != RuntimeModeRealOVNOVS || opts.NetworkBackend != "linux-veth" ||
		cfg.NodeName == "" || len(cfg.Networks) == 0 || !filepath.IsAbs(cfg.JournalDirectory) {
		return nil, fmt.Errorf("%w: real OVN/OVS/Linux mode and complete continuity domain required", ownership.ErrBlocked)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) != cfg.Domain.BootID {
		return nil, fmt.Errorf("%w: boot identity mismatch", ownership.ErrBlocked)
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil || ns != cfg.Domain.NetworkNamespace {
		return nil, fmt.Errorf("%w: network namespace identity mismatch", ownership.ErrBlocked)
	}
	for network, sw := range cfg.Networks {
		if strings.TrimSpace(network) == "" || strings.TrimSpace(sw) == "" {
			return nil, fmt.Errorf("%w: empty network mapping", ownership.ErrBlocked)
		}
	}
	// Binding backend endpoints prevents an otherwise matching tuple being
	// recovered into a different OVN/OVS ownership domain.
	mapping, _ := json.Marshal(struct {
		Node, Kubeconfig, NB, OVS, CRI, CTR, CTRHash string
		Networks                                     map[string]string
		Admission                                    *CNIAdmissionConfig
	}{cfg.NodeName, cfg.Kubeconfig, opts.OVNNBEndpoint, opts.OVSDBEndpoint, cfg.CRIEndpoint, cfg.CTRPath, cfg.CTRSHA256, cfg.Networks, cfg.Admission})
	digest := sha256.Sum256(mapping)
	wanted := hex.EncodeToString(digest[:])
	if cfg.Domain.ConfigDigest != "" && cfg.Domain.ConfigDigest != wanted {
		return nil, fmt.Errorf("%w: CNI configuration digest mismatch", ownership.ErrBlocked)
	}
	cfg.Domain.ConfigDigest = wanted
	j, err := ownership.Open(cfg.JournalDirectory, cfg.Domain, cfg.Create)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func() error, error) { return nil, errors.Join(err, j.Close()) }
	var kube *rest.Config
	if cfg.Kubeconfig == "" {
		kube, err = rest.InClusterConfig()
	} else {
		kube, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}
	if err != nil {
		return fail(err)
	}
	kube.Timeout = 5 * time.Second
	kc, err := coreclient.NewForConfig(kube)
	if err != nil {
		return fail(err)
	}
	conn, err := dialTrustedCRI(ctx, cfg.CRIEndpoint)
	if err != nil {
		return fail(err)
	}
	nb, err := newOVNNBClient(ctx, opts.OVNNBEndpoint)
	if err != nil {
		conn.Close()
		return fail(err)
	}
	ovs, err := newOVSClient(ctx, opts.OVSDBEndpoint)
	if err != nil {
		nb.Close()
		conn.Close()
		return fail(err)
	}
	observer := &nativeCNIObserver{source: cniLiveState{liveExportState{nb: nb, ovs: ovs}}, networks: cfg.Networks}
	workload := &KubernetesCNIWorkloadAttestor{Client: kc, NodeName: cfg.NodeName, NodeUID: cfg.Domain.NodeUID, Sandbox: cri.NewRuntimeServiceClient(conn)}
	if cfg.CTRPath != "" || cfg.CTRSHA256 != "" {
		if !validSHA256(cfg.CTRSHA256) || !filepath.IsAbs(cfg.CTRPath) || filepath.Clean(cfg.CTRPath) != cfg.CTRPath {
			nb.Close()
			ovs.Close()
			conn.Close()
			return fail(errors.New("creating-sandbox ctr requires explicit root path and binary SHA256"))
		}
		workload.CreatingSandbox = &cniCreatingSandboxAttestor{source: &kubernetesContainerdSource{config: ProviderAttestationConfig{
			CTRPath: cfg.CTRPath, CTRSHA256: cfg.CTRSHA256, RuntimeSocket: strings.TrimPrefix(cfg.CRIEndpoint, "unix://"),
		}}}
	}
	c := &cniContinuity{nodeName: cfg.NodeName, journal: j, workload: workload, observer: observer}
	if cfg.Admission != nil {
		if err := cfg.Admission.validate(); err != nil {
			nb.Close()
			ovs.Close()
			conn.Close()
			return fail(fmt.Errorf("invalid creation admission configuration: %w", err))
		}
		c.authority = &cniCallerAuthority{config: *cfg.Admission, runtimeSocket: strings.TrimPrefix(cfg.CRIEndpoint, "unix://")}
	}
	if err := s.recoverCNI(ctx, c); err != nil {
		nb.Close()
		ovs.Close()
		conn.Close()
		return fail(err)
	}
	verification, stopVerification := context.WithCancel(lifetime)
	verificationDone := make(chan struct{})
	go func() {
		defer close(verificationDone)
		s.runCNIVerification(verification)
	}()
	return func() error {
		stopVerification()
		<-verificationDone
		s.operations.Lock()
		defer s.operations.Unlock()
		nb.Close()
		ovs.Close()
		conn.Close()
		return j.Close()
	}, nil
}

// KubernetesCNIWorkloadAttestor requires current Node and Pod UIDs plus a
// running container status. Only direct GETs are used; caches are not evidence.
type KubernetesCNIWorkloadAttestor struct {
	Client          coreclient.CoreV1Interface
	NodeName        string
	NodeUID         string
	Sandbox         cri.RuntimeServiceClient
	CreatingSandbox interface {
		VerifySandbox(context.Context, endpoint.WorkloadRef, string, string) error
	}
}

func (a *KubernetesCNIWorkloadAttestor) Verify(ctx context.Context, ref endpoint.WorkloadRef, container, netns string) (bool, error) {
	return a.verify(ctx, ref, container, netns, false)
}

// A CNI ADD executes while the independently identified CRI sandbox is still
// being configured; READY is required for restart recovery and CHECK instead.
func (a *KubernetesCNIWorkloadAttestor) VerifyCreating(ctx context.Context, ref endpoint.WorkloadRef, container, netns string) (bool, error) {
	return a.verify(ctx, ref, container, netns, true)
}

func (a *KubernetesCNIWorkloadAttestor) verify(ctx context.Context, ref endpoint.WorkloadRef, container, netns string, creating bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.Client == nil || ref.Kind != "pod" || ref.UID == "" || ref.Name == "" || ref.Namespace == "" ||
		ref.NodeName != a.NodeName || a.NodeUID == "" || container == "" || netns == "" || a.Sandbox == nil {
		return false, errors.New("incomplete authoritative CNI workload identity")
	}
	deleted, err := a.VerifyPod(ctx, ref)
	if err != nil || deleted {
		return deleted, err
	}
	resp, err := a.Sandbox.PodSandboxStatus(ctx, &cri.PodSandboxStatusRequest{PodSandboxId: container, Verbose: true})
	if err != nil {
		if creating && status.Code(err) == codes.NotFound && a.CreatingSandbox != nil {
			return false, a.CreatingSandbox.VerifySandbox(ctx, ref, container, netns)
		}
		if status.Code(err) == codes.NotFound {
			return false, fmt.Errorf("%w: sandbox unpublished", errCNIRuntimePending)
		}
		return false, fmt.Errorf("CRI sandbox observation: %w", err)
	}
	st := resp.GetStatus()
	if st == nil || st.Id != container || st.Metadata == nil || st.Metadata.Uid != ref.UID || st.Metadata.Name != ref.Name || st.Metadata.Namespace != ref.Namespace {
		return false, fmt.Errorf("%w: CRI sandbox/Pod mismatch", errCNIIdentityConflict)
	}
	if !creating && st.State != cri.PodSandboxState_SANDBOX_READY {
		return false, errCNIRuntimePending
	}
	var info struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(resp.Info["info"]), &info); err != nil || info.PID <= 0 {
		if creating && a.CreatingSandbox != nil {
			return false, a.CreatingSandbox.VerifySandbox(ctx, ref, container, netns)
		}
		return false, errors.New("CRI sandbox task PID unavailable")
	}
	taskNS, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", info.PID))
	if err != nil {
		return false, err
	}
	attachmentNS, err := os.Stat(netns)
	if err != nil {
		return false, err
	}
	if !os.SameFile(taskNS, attachmentNS) {
		return false, fmt.Errorf("%w: sandbox namespace differs from attachment", errCNIIdentityConflict)
	}
	return false, nil
}

func (a *KubernetesCNIWorkloadAttestor) VerifyPod(ctx context.Context, ref endpoint.WorkloadRef) (bool, error) {
	if a.Client == nil || ref.Kind != "pod" || ref.UID == "" || ref.Name == "" || ref.Namespace == "" ||
		ref.NodeName != a.NodeName || a.NodeUID == "" {
		return false, errors.New("incomplete authoritative Pod identity")
	}
	node, err := a.Client.Nodes().Get(ctx, a.NodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if string(node.UID) != a.NodeUID || node.DeletionTimestamp != nil {
		return false, fmt.Errorf("%w: Node changed or deleting", errCNIIdentityConflict)
	}
	pod, err := a.Client.Pods(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if string(pod.UID) != ref.UID || pod.Spec.NodeName != a.NodeName || pod.DeletionTimestamp != nil {
		return false, fmt.Errorf("%w: Pod UID/node changed or deleting", errCNIIdentityConflict)
	}
	return false, nil
}

type cniLiveState struct{ liveExportState }

func (s cniLiveState) Snapshot(ctx context.Context) (exportSnapshot, error) {
	var out exportSnapshot
	if err := selectExportRows(ctx, s.nb, "Logical_Switch", []string{"_uuid", "name", "ports"}, &out.Switches); err != nil {
		return out, err
	}
	if err := selectExportRows(ctx, s.nb, "Logical_Switch_Port", []string{"_uuid", "name", "addresses", "external_ids"}, &out.LSPs); err != nil {
		return out, err
	}
	if err := selectExportRows(ctx, s.ovs, "Bridge", []string{"_uuid", "name", "ports"}, &out.Bridges); err != nil {
		return out, err
	}
	if err := selectExportRows(ctx, s.ovs, "Port", []string{"_uuid", "name", "interfaces"}, &out.Ports); err != nil {
		return out, err
	}
	err := selectExportRows(ctx, s.ovs, "Interface", []string{"_uuid", "name", "external_ids", "ofport", "error"}, &out.Interfaces)
	return out, err
}

type nativeCNIObserver struct {
	source   exportStateSource
	networks map[string]string
}

func (o *nativeCNIObserver) Observe(ctx context.Context, rec *endpointRecord) (cniReceipt, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var receipt cniReceipt
	spec := rec.endpointSpec
	if o.networks[spec.NetworkID] == "" || o.networks[spec.NetworkID] != spec.LogicalSwitch {
		return receipt, false, errors.New("CNI logical switch not in immutable mapping")
	}
	data, err := o.source.Snapshot(ctx)
	if err != nil {
		return receipt, false, err
	}
	switches := 0
	for _, sw := range data.Switches {
		if sw == nil {
			return receipt, false, errors.New("nil switch observation")
		}
		if sw.Name == spec.LogicalSwitch {
			switches++
			receipt.SwitchUUID = sw.UUID
		}
	}
	if switches != 1 || receipt.SwitchUUID == "" {
		return receipt, false, errors.New("configured CNI switch absent or ambiguous")
	}
	var lsp *nbdb.LogicalSwitchPort
	var iface *vswitch.Interface
	var port *vswitch.Port
	for _, row := range data.LSPs {
		if row == nil {
			return receipt, false, errors.New("nil LSP observation")
		}
		if row.Name == spec.LogicalSwitchPort {
			if lsp != nil {
				return receipt, false, errors.New("duplicate CNI LSP")
			}
			lsp = row
		} else if row.ExternalIDs[attachment.ExternalIDEndpointID] == string(spec.ID) {
			return receipt, false, errors.New("endpoint identity exists under another LSP name")
		}
	}
	for _, row := range data.Interfaces {
		if row == nil {
			return receipt, false, errors.New("nil Interface observation")
		}
		if row.Name == spec.OVSInterfaceName {
			if iface != nil {
				return receipt, false, errors.New("duplicate CNI Interface")
			}
			iface = row
		} else if row.ExternalIDs[attachment.ExternalIDEndpointID] == string(spec.ID) {
			return receipt, false, errors.New("endpoint identity exists under another Interface name")
		}
	}
	for _, row := range data.Ports {
		if row == nil {
			return receipt, false, errors.New("nil Port observation")
		}
		if row.Name == spec.OVSInterfaceName {
			if port != nil {
				return receipt, false, errors.New("duplicate CNI Port")
			}
			port = row
		}
	}
	if lsp == nil && iface == nil && port == nil {
		return receipt, true, nil
	}
	if lsp == nil || iface == nil || port == nil || lsp.UUID == "" || iface.UUID == "" || port.UUID == "" {
		return receipt, false, errors.New("partial CNI resources or missing UUID")
	}
	want := attachment.ExternalIDsForIdentity(attachment.Identity{
		EndpointID: string(spec.ID), NetworkID: spec.NetworkID, AttachmentID: spec.AttachmentID,
		WorkloadUID: spec.Workload.UID, Incarnation: spec.Incarnation,
	}, spec.LogicalSwitchPort, "")
	for key, value := range want {
		if lsp.ExternalIDs[key] != value || iface.ExternalIDs[key] != value {
			return receipt, false, errors.New("CNI resource ownership mismatch")
		}
	}
	if iface.Error != nil && *iface.Error != "" {
		return receipt, false, fmt.Errorf("CNI OVS attachment error: %s", *iface.Error)
	}
	if len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
		return receipt, false, errors.New("CNI OVS interface membership mismatch")
	}
	if iface.Ofport == nil || *iface.Ofport <= 0 {
		return receipt, false, errCNIResourceUnready
	}
	members, bridges := 0, 0
	for _, sw := range data.Switches {
		if sw == nil {
			return receipt, false, errors.New("nil switch observation")
		}
		for _, id := range sw.Ports {
			if id == lsp.UUID {
				if sw.Name != spec.LogicalSwitch || sw.UUID == "" {
					return receipt, false, errors.New("CNI switch mismatch")
				}
				members++
				receipt.SwitchUUID = sw.UUID
			}
		}
	}
	for _, bridge := range data.Bridges {
		if bridge == nil {
			return receipt, false, errors.New("nil bridge observation")
		}
		for _, id := range bridge.Ports {
			if id == port.UUID {
				if bridge.Name != "br-int" || bridge.UUID == "" {
					return receipt, false, errors.New("CNI bridge mismatch")
				}
				bridges++
				receipt.BridgeUUID = bridge.UUID
			}
		}
	}
	if members != 1 || bridges != 1 {
		return receipt, false, errors.New("CNI membership not unique")
	}
	if len(spec.IPAddresses) != 1 || len(lsp.Addresses) != 1 {
		return receipt, false, errors.New("incomplete CNI static address observation")
	}
	fields := strings.Fields(lsp.Addresses[0])
	ip, _, err := net.ParseCIDR(spec.IPAddresses[0])
	if err != nil || len(fields) != 2 || !strings.EqualFold(fields[0], spec.MACAddress) ||
		(fields[1] != spec.IPAddresses[0] && fields[1] != ip.String()) {
		return receipt, false, errors.New("CNI static address mismatch")
	}
	receipt.LSPUUID, receipt.InterfaceUUID, receipt.PortUUID = lsp.UUID, iface.UUID, port.UUID
	if _, live := o.source.(cniLiveState); live {
		host, err := netlink.LinkByName(spec.OVSInterfaceName)
		if err != nil {
			return receipt, false, err
		}
		if host.Attrs() == nil || host.Attrs().Alias != ownerAliasForKey(endpointKey(rec.identity)) || host.Type() != "veth" {
			return receipt, false, errors.New("CNI host veth identity mismatch")
		}
		var ns unix.Stat_t
		if err := unix.Stat(rec.runtime.GetNetnsPath(), &ns); err != nil {
			return receipt, false, err
		}
		receipt.HostIfIndex = host.Attrs().Index
		receipt.NetNSDevice, receipt.NetNSInode = uint64(ns.Dev), ns.Ino
	}
	return receipt, false, nil
}

func (o *nativeCNIObserver) NetworkAbsent(_ context.Context, key string) error {
	_, err := netlink.LinkByName(hostVethNameForKey(key))
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("CNI host veth still present")
}
