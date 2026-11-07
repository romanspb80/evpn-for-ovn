package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type fakeProviderSource struct {
	node          *corev1.Node
	ds            *appsv1.DaemonSet
	pods          []corev1.Pod
	runtime       providerRuntimeEvidence
	err           error
	configuration *unstructured.Unstructured
}

func (f *fakeProviderSource) Node(context.Context, string) (*corev1.Node, error) {
	return f.node, f.err
}
func (f *fakeProviderSource) DaemonSet(context.Context, string, string) (*appsv1.DaemonSet, error) {
	return f.ds, f.err
}
func (f *fakeProviderSource) Pods(context.Context, string, string) ([]corev1.Pod, error) {
	return f.pods, f.err
}
func (f *fakeProviderSource) Container(context.Context, string) (providerRuntimeEvidence, error) {
	return f.runtime, f.err
}

func (f *fakeProviderSource) Configuration(context.Context, string, string) (*unstructured.Unstructured, error) {
	return f.configuration, f.err
}

func providerFixture(pid int32, uid uint32, executableHash string) (*providerAttestor, *fakeProviderSource, SpeakerProcessEvidence) {
	digest := "sha256:" + strings.Repeat("a", 64)
	id := strings.Repeat("b", 64)
	image := "example.invalid/k8gobgp@" + digest
	spec := map[string]interface{}{"asn": int64(64512), "routerId": "192.0.2.1", "netlinkImport": map[string]interface{}{"enabled": false}, "netlinkExport": map[string]interface{}{"enabled": false}}
	data, _ := json.Marshal(spec)
	hash := sha256.Sum256(data)
	config := ProviderAttestationConfig{NodeName: "node", NodeUID: "node-uid", Namespace: "provider", DaemonSetName: "speaker",
		DaemonSetUID: "ds-uid", ContainerName: "k8gobgp", Image: image, ImageDigest: digest,
		ExpectedUID: &uid, ExecutableSHA256: executableHash, Runtime: "containerd-ctr-v1",
		RuntimeSocket: "/run/containerd/containerd.sock", CTRPath: "/usr/bin/ctr", CTRSHA256: strings.Repeat("c", 64),
		ConfigNamespace: "provider", ConfigName: "cni-export", ConfigUID: "config-uid", ConfigDigest: hex.EncodeToString(hash[:])}
	yes := true
	f := &fakeProviderSource{
		configuration: &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "bgp.purelb.io/v1", "kind": "BGPConfiguration", "metadata": map[string]interface{}{"name": "cni-export", "namespace": "provider", "uid": "config-uid"}, "spec": spec}},
		node:          &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}},
		ds:            &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "speaker", Namespace: "provider", UID: "ds-uid"}},
		pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "speaker-pod", Namespace: "provider", UID: "pod-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "speaker", UID: "ds-uid", Controller: &yes}}},
			Spec: corev1.PodSpec{NodeName: "node", HostNetwork: true, Containers: []corev1.Container{{Name: "k8gobgp", Image: image}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "k8gobgp",
				ContainerID: "containerd://" + id, ImageID: "docker-pullable://" + image, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}},
		runtime: providerRuntimeEvidence{ID: id, Image: image, PodUID: "pod-uid", Namespace: "provider", PodName: "speaker-pod", ContainerName: "k8gobgp", PIDs: []int32{42, pid}, Running: true, PIDNamespace: "pid:[123]"},
	}
	return &providerAttestor{config: config, source: f}, f, SpeakerProcessEvidence{PID: pid, UID: uid, ExecutableSHA256: executableHash, PIDNamespace: "pid:[123]"}
}

func TestProviderAttestationIndependentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*providerAttestor, *fakeProviderSource, *SpeakerProcessEvidence)
	}{
		{"UID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) { p.UID++ }},
		{"executable", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			p.ExecutableSHA256 = strings.Repeat("f", 64)
		}},
		{"node UID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.node.UID = "replacement"
		}},
		{"DS UID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) { f.ds.UID = "replacement" }},
		{"pod node", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods[0].Spec.NodeName = "another"
		}},
		{"pod owner UID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods[0].OwnerReferences[0].UID = "replacement"
		}},
		{"runtime PID namespace", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.runtime.PIDNamespace = "pid:[456]"
		}},
		{"hostNetwork", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods[0].Spec.HostNetwork = false
		}},
		{"image spec", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods[0].Spec.Containers[0].Image = "mutable:latest"
		}},
		{"image ID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods[0].Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("f", 64)
		}},
		{"runtime pod UID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.runtime.PodUID = "replacement"
		}},
		{"runtime ID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.runtime.ID = strings.Repeat("f", 64)
		}},
		{"runtime image", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.runtime.Image = "forged:latest"
		}},
		{"runtime PID", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.runtime.PIDs = []int32{42}
		}},
		{"runtime stopped", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) { f.runtime.Running = false }},
		{"API unavailable", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.err = errors.New("offline")
		}},
		{"UID omitted", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			a.config.ExpectedUID = nil
		}},
		{"runtime unsupported", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			a.config.Runtime = "cgroup-only"
		}},
		{"terminating Pod", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			now := metav1.Now()
			f.pods[0].DeletionTimestamp = &now
		}},
		{"ambiguous Pod", func(a *providerAttestor, f *fakeProviderSource, p *SpeakerProcessEvidence) {
			f.pods = append(f.pods, f.pods[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, p := providerFixture(99, 0, strings.Repeat("d", 64))
			if _, err := a.attest(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			tc.change(a, f, &p)
			if _, err := a.attest(context.Background(), p); err == nil {
				t.Fatal("unverified provider accepted")
			}
		})
	}
}

func TestProviderAttestationPodReplacement(t *testing.T) {
	a, f, p := providerFixture(99, 0, strings.Repeat("d", 64))
	before, err := a.attest(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.pods[0].UID = types.UID("new-pod")
	f.runtime.PodUID = "new-pod"
	id := strings.Repeat("e", 64)
	f.runtime.ID = id
	f.pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://" + id
	after, err := a.attest(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if before == after || before.DaemonSetUID != after.DaemonSetUID {
		t.Fatal("replacement workload evidence not distinguished")
	}
}

func TestProviderAdministratorPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 {
		if _, err := openAdministratorFile(path); err == nil {
			t.Fatal("non-root config accepted")
		}
		return
	}
	f, err := openAdministratorFile(path)
	if err == nil {
		f.Close()
	} else {
		// A project under a non-root home directory is not a trusted config
		// ancestor, even when the file itself was created by root.
		t.Logf("root config correctly rejected untrusted project ancestor: %v", err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := openAdministratorFile(path); err == nil {
		t.Fatal("group writable config accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openAdministratorFile(link); err == nil {
		t.Fatal("symlink config accepted")
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := openAdministratorFile(path); err == nil {
		t.Fatal("writable parent accepted")
	}
}

func TestProviderConfigDecode(t *testing.T) {
	a, _, _ := providerFixture(99, 0, strings.Repeat("d", 64))
	data, err := json.Marshal(a.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeProviderAttestationConfig(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{string(data) + " {}", `{"nodeName":"node","unknown":true}`, `{}`, `null`} {
		if _, err := decodeProviderAttestationConfig(strings.NewReader(data)); err == nil {
			t.Fatal("missing, unknown or trailing config accepted")
		}
	}
}

func TestProviderConfigurationAttestation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*providerAttestor, *fakeProviderSource)
	}{
		{"UID", func(a *providerAttestor, f *fakeProviderSource) { f.configuration.SetUID("new-config") }},
		{"namespace", func(a *providerAttestor, f *fakeProviderSource) { f.configuration.SetNamespace("other") }},
		{"missing", func(a *providerAttestor, f *fakeProviderSource) { f.configuration = nil }},
		{"terminating", func(a *providerAttestor, f *fakeProviderSource) {
			now := metav1.Now()
			f.configuration.SetDeletionTimestamp(&now)
		}},
		{"spec changed", func(a *providerAttestor, f *fakeProviderSource) {
			_ = unstructured.SetNestedField(f.configuration.Object, int64(64513), "spec", "asn")
		}},
		{"spec absent", func(a *providerAttestor, f *fakeProviderSource) { delete(f.configuration.Object, "spec") }},
		{"pin absent", func(a *providerAttestor, f *fakeProviderSource) { a.config.ConfigUID = "" }},
		{"netlink import", func(a *providerAttestor, f *fakeProviderSource) {
			_ = unstructured.SetNestedField(f.configuration.Object, true, "spec", "netlinkImport", "enabled")
			b, _ := json.Marshal(f.configuration.Object["spec"])
			h := sha256.Sum256(b)
			a.config.ConfigDigest = hex.EncodeToString(h[:])
		}},
		{"netlink export", func(a *providerAttestor, f *fakeProviderSource) {
			_ = unstructured.SetNestedField(f.configuration.Object, true, "spec", "netlinkExport", "enabled")
			b, _ := json.Marshal(f.configuration.Object["spec"])
			h := sha256.Sum256(b)
			a.config.ConfigDigest = hex.EncodeToString(h[:])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, p := providerFixture(99, 0, strings.Repeat("d", 64))
			tc.change(a, f)
			if _, err := a.attest(context.Background(), p); err == nil {
				t.Fatal("unverified or competing provider configuration accepted")
			}
		})
	}
}

func TestProviderUnconfiguredContinuity(t *testing.T) {
	guard := &unixSpeakerContinuity{}
	if conn, err := guard.DialContext(context.Background(), "unix:///missing-provider.sock"); err == nil {
		conn.Close()
		t.Fatal("missing provider attestation config accepted")
	}
	if _, err := guard.Incarnation(); err == nil {
		t.Fatal("unbound incarnation accepted")
	}
}

func TestProviderContainerdFailureDiagnostic(t *testing.T) {
	cause := &exec.ExitError{Stderr: []byte("container task not found\n" + strings.Repeat("x", 2048))}
	err := containerdInspectionError([]string{"tasks", "ps", "container-id"}, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), `["tasks" "ps" "container-id"]`) ||
		!strings.Contains(err.Error(), "container task not found") || len(err.Error()) > 1300 {
		t.Fatalf("bounded runtime diagnostic missing: %v", err)
	}
	if !strings.Contains(containerdInspectionError([]string{"tasks", "list"}, context.DeadlineExceeded).Error(), "deadline exceeded") {
		t.Fatal("non-exit runtime error lost")
	}
}

func TestProviderContainerdOutput(t *testing.T) {
	id := strings.Repeat("b", 64)
	if ok, err := containerdTaskRunning([]byte("TASK PID STATUS\n"+id+" 42 RUNNING\n"), id); err != nil || !ok {
		t.Fatalf("task: %v %v", ok, err)
	}
	if pids, err := containerdTaskPIDs([]byte("PID INFO\n42 -\n99 -\n")); err != nil || len(pids) != 2 || pids[1] != 99 {
		t.Fatalf("child PID not independently associated: %v %v", pids, err)
	}
	for _, output := range []string{"", "PID\n42", "PID INFO\n-1 -", "PID INFO\n42 -\n42 -"} {
		if _, err := containerdTaskPIDs([]byte(output)); err == nil {
			t.Fatal("malformed runtime process evidence accepted")
		}
	}
	for _, output := range []string{"TASKS PID STATUS", "TASK PID STATUS\n" + id + " x RUNNING", "TASK PID STATUS\n" + id + " 42 RUNNING\n" + id + " 42 RUNNING"} {
		if _, err := containerdTaskRunning([]byte(output), id); err == nil {
			t.Fatal("malformed runtime task evidence accepted")
		}
	}
}
