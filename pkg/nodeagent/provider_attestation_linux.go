package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ProviderAttestationConfig is administrator-authored, not a claim from the
// speaker, an agent RPC caller, or the workload being attested.
type ProviderAttestationConfig struct {
	NodeName         string  `json:"nodeName"`
	NodeUID          string  `json:"nodeUID"`
	Namespace        string  `json:"namespace"`
	DaemonSetName    string  `json:"daemonSetName"`
	DaemonSetUID     string  `json:"daemonSetUID"`
	ContainerName    string  `json:"containerName"`
	Image            string  `json:"image"`
	ImageDigest      string  `json:"imageDigest"`
	ExpectedUID      *uint32 `json:"expectedUID"`
	ExecutableSHA256 string  `json:"executableSHA256"`
	Runtime          string  `json:"runtime"`
	RuntimeSocket    string  `json:"runtimeSocket"`
	CTRPath          string  `json:"ctrPath"`
	CTRSHA256        string  `json:"ctrSHA256"`
	ConfigNamespace  string  `json:"configNamespace"`
	ConfigName       string  `json:"configName"`
	ConfigUID        string  `json:"configUID"`
	ConfigDigest     string  `json:"configDigest"`
}

type SpeakerWorkloadEvidence struct {
	NodeUID, PodUID, DaemonSetUID, ContainerID, ImageDigest, ConfigUID, ConfigDigest string
}

type providerRuntimeEvidence struct {
	ID, Image, PodUID, Namespace, PodName, ContainerName string
	PIDs                                                 []int32
	Running                                              bool
	PIDNamespace                                         string
}

type providerIdentitySource interface {
	Node(context.Context, string) (*corev1.Node, error)
	DaemonSet(context.Context, string, string) (*appsv1.DaemonSet, error)
	Pods(context.Context, string, string) ([]corev1.Pod, error)
	Container(context.Context, string) (providerRuntimeEvidence, error)
	Configuration(context.Context, string, string) (*unstructured.Unstructured, error)
}

type providerAttestor struct {
	config ProviderAttestationConfig
	source providerIdentitySource
}

func validSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}

func (c ProviderAttestationConfig) validate() error {
	if c.NodeName == "" || c.NodeUID == "" || c.Namespace == "" || c.DaemonSetName == "" ||
		c.DaemonSetUID == "" || c.ContainerName == "" || c.ExpectedUID == nil ||
		c.ConfigNamespace == "" || c.ConfigName == "" || c.ConfigUID == "" || !validSHA256(c.ConfigDigest) {
		return fmt.Errorf("provider attestation requires pinned node, workload, container and UID")
	}
	if !validSHA256(c.ExecutableSHA256) || !validSHA256(c.CTRSHA256) ||
		!strings.HasPrefix(c.ImageDigest, "sha256:") || !validSHA256(strings.TrimPrefix(c.ImageDigest, "sha256:")) ||
		!strings.HasSuffix(c.Image, "@"+c.ImageDigest) {
		return fmt.Errorf("provider executable, ctr and immutable image SHA256 pins required")
	}
	if c.Runtime != "containerd-ctr-v1" {
		return fmt.Errorf("only administrator-pinned containerd-ctr-v1 runtime inspection is supported")
	}
	for _, p := range []string{c.RuntimeSocket, c.CTRPath} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("runtime socket and ctr require clean absolute paths")
		}
	}
	return nil
}

// openAdministratorFile walks without following symlinks, including parents.
// Holding the returned fd also prevents path replacement during a read/exec.
func openAdministratorFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("administrator file requires clean absolute path")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		}
		next, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			return nil, fmt.Errorf("open administrator path %s: %w", path, err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil {
			unix.Close(next)
			return nil, err
		}
		if st.Uid != 0 || st.Mode&0022 != 0 || (i == len(parts)-1 && st.Mode&unix.S_IFMT != unix.S_IFREG) {
			unix.Close(next)
			return nil, fmt.Errorf("administrator path %s must be root-owned, non-writable by group/other, and regular", path)
		}
		unix.Close(fd)
		fd = next
	}
	result := os.NewFile(uintptr(fd), path)
	fd = -1
	return result, nil
}

func fileSHA256(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func LoadProviderAttestationConfig(path string) (ProviderAttestationConfig, error) {
	var c ProviderAttestationConfig
	f, err := openAdministratorFile(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return c, err
	}
	if st.Size == 0 || st.Size > 64*1024 || st.Nlink != 1 {
		return c, fmt.Errorf("provider config must be a bounded single-link regular file")
	}
	return decodeProviderAttestationConfig(f)
}

func decodeProviderAttestationConfig(r io.Reader) (ProviderAttestationConfig, error) {
	var c ProviderAttestationConfig
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("provider configuration must contain exactly one JSON object")
	}
	return c, c.validate()
}

// NewAttestedSpeakerContinuity must be created anew for each replacement
// transport, after closing the previous gRPC connection and guard. There is no
// reset operation that could accidentally bless a stale transport.
func NewAttestedSpeakerContinuity(path string) (*unixSpeakerContinuity, error) {
	c, err := LoadProviderAttestationConfig(path)
	if err != nil {
		return nil, err
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("provider attestation Kubernetes identity: %w", err)
	}
	rc.Timeout = 10 * time.Second
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &unixSpeakerContinuity{attestor: &providerAttestor{config: c, source: &kubernetesContainerdSource{client: client, dynamic: dynamicClient, config: c}}}, nil
}

func (a *providerAttestor) attest(ctx context.Context, process SpeakerProcessEvidence) (SpeakerWorkloadEvidence, error) {
	var none SpeakerWorkloadEvidence
	c := a.config
	if err := c.validate(); err != nil {
		return none, err
	}
	if process.UID != *c.ExpectedUID || process.ExecutableSHA256 != c.ExecutableSHA256 {
		return none, fmt.Errorf("Unix peer UID or executable SHA256 does not match administrator pin")
	}
	configuration, err := a.source.Configuration(ctx, c.ConfigNamespace, c.ConfigName)
	if err != nil {
		return none, fmt.Errorf("independent provider configuration: %w", err)
	}
	if err := verifyProviderConfiguration(configuration, c); err != nil {
		return none, err
	}
	node, err := a.source.Node(ctx, c.NodeName)
	if err != nil {
		return none, err
	}
	if node == nil || node.Name != c.NodeName || string(node.UID) != c.NodeUID || node.DeletionTimestamp != nil {
		return none, fmt.Errorf("provider node identity changed")
	}
	ds, err := a.source.DaemonSet(ctx, c.Namespace, c.DaemonSetName)
	if err != nil {
		return none, err
	}
	if ds == nil || ds.Name != c.DaemonSetName || ds.Namespace != c.Namespace || string(ds.UID) != c.DaemonSetUID || ds.DeletionTimestamp != nil {
		return none, fmt.Errorf("provider DaemonSet identity changed")
	}
	pods, err := a.source.Pods(ctx, c.Namespace, c.NodeName)
	if err != nil {
		return none, err
	}
	var matched *SpeakerWorkloadEvidence
	for _, pod := range pods {
		if pod.Spec.NodeName != c.NodeName || pod.Namespace != c.Namespace || pod.UID == "" ||
			pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !pod.Spec.HostNetwork {
			continue
		}
		owned := false
		for _, ref := range pod.OwnerReferences {
			if ref.Controller != nil && *ref.Controller && ref.APIVersion == "apps/v1" && ref.Kind == "DaemonSet" &&
				ref.Name == c.DaemonSetName && string(ref.UID) == c.DaemonSetUID {
				owned = true
			}
		}
		if !owned {
			continue
		}
		image := ""
		for _, container := range pod.Spec.Containers {
			if container.Name == c.ContainerName {
				image = container.Image
			}
		}
		if image != c.Image {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != c.ContainerName || status.State.Running == nil ||
				!strings.HasPrefix(status.ContainerID, "containerd://") || imageDigest(status.ImageID) != c.ImageDigest {
				continue
			}
			id := strings.TrimPrefix(status.ContainerID, "containerd://")
			if !validSHA256(id) {
				return none, fmt.Errorf("invalid provider runtime container ID")
			}
			r, err := a.source.Container(ctx, id)
			if err != nil {
				return none, fmt.Errorf("independent runtime identity: %w", err)
			}
			contains := false
			for _, pid := range r.PIDs {
				if pid == process.PID {
					contains = true
				}
			}
			if !contains {
				continue
			}
			if !r.Running || r.PIDNamespace == "" || r.PIDNamespace != process.PIDNamespace || r.ID != id || r.Image != c.Image || r.PodUID != string(pod.UID) ||
				r.Namespace != pod.Namespace || r.PodName != pod.Name || r.ContainerName != c.ContainerName {
				return none, fmt.Errorf("runtime process association disagrees with Kubernetes provider identity")
			}
			if matched != nil {
				return none, fmt.Errorf("ambiguous runtime provider process association")
			}
			matched = &SpeakerWorkloadEvidence{NodeUID: c.NodeUID, PodUID: string(pod.UID), DaemonSetUID: c.DaemonSetUID, ContainerID: id, ImageDigest: c.ImageDigest, ConfigUID: c.ConfigUID, ConfigDigest: c.ConfigDigest}
		}
	}
	if matched == nil {
		return none, fmt.Errorf("actual Unix peer is not a verified provider container process")
	}
	return *matched, nil
}

func verifyProviderConfiguration(obj *unstructured.Unstructured, c ProviderAttestationConfig) error {
	if obj == nil || obj.GetUID() == "" || string(obj.GetUID()) != c.ConfigUID || obj.GetName() != c.ConfigName ||
		obj.GetNamespace() != c.ConfigNamespace || obj.GetDeletionTimestamp() != nil || obj.GetAPIVersion() != "bgp.purelb.io/v1" || obj.GetKind() != "BGPConfiguration" {
		return fmt.Errorf("provider BGPConfiguration identity changed")
	}
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("provider BGPConfiguration spec unavailable")
	}
	for _, writer := range []string{"netlinkImport", "netlinkExport"} {
		enabled, _, err := unstructured.NestedBool(spec, writer, "enabled")
		if err != nil || enabled {
			return fmt.Errorf("provider %s must not enable a competing Netlink writer", writer)
		}
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != c.ConfigDigest {
		return fmt.Errorf("provider BGPConfiguration spec digest changed")
	}
	return nil
}

func imageDigest(s string) string {
	if i := strings.LastIndex(s, "sha256:"); i >= 0 {
		d := s[i:]
		if validSHA256(strings.TrimPrefix(d, "sha256:")) {
			return d
		}
	}
	return ""
}

type kubernetesContainerdSource struct {
	client  kubernetes.Interface
	dynamic dynamic.Interface
	config  ProviderAttestationConfig
}

func (s *kubernetesContainerdSource) Configuration(ctx context.Context, ns, name string) (*unstructured.Unstructured, error) {
	return s.dynamic.Resource(schema.GroupVersionResource{Group: "bgp.purelb.io", Version: "v1", Resource: "configs"}).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
}

func (s *kubernetesContainerdSource) Node(ctx context.Context, name string) (*corev1.Node, error) {
	return s.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}
func (s *kubernetesContainerdSource) DaemonSet(ctx context.Context, ns, name string) (*appsv1.DaemonSet, error) {
	return s.client.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
}
func (s *kubernetesContainerdSource) Pods(ctx context.Context, ns, node string) ([]corev1.Pod, error) {
	p, err := s.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, err
	}
	return p.Items, nil
}
