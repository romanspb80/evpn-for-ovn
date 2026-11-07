package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type creatingSandboxVerifier struct{ calls int }

func (v *creatingSandboxVerifier) VerifySandbox(context.Context, endpoint.WorkloadRef, string, string) error {
	v.calls++
	return nil
}

func TestKubernetesCNICreatingSandboxFallbackOnlyForADD(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "node-uid"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "ns-a", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "node-a"}},
	)
	fallback := &creatingSandboxVerifier{}
	sandbox := &cniSandboxClient{err: status.Error(codes.NotFound, "not published yet")}
	attestor := &KubernetesCNIWorkloadAttestor{Client: client.CoreV1(), NodeName: "node-a", NodeUID: "node-uid", Sandbox: sandbox, CreatingSandbox: fallback}
	ref := endpoint.WorkloadRef{Kind: "pod", UID: "pod-uid", Name: "pod-a", Namespace: "ns-a", NodeName: "node-a"}
	if _, err := attestor.VerifyCreating(context.Background(), ref, "sandbox-a", "/proc/self/ns/net"); err != nil || fallback.calls != 1 {
		t.Fatal("creating sandbox fallback unavailable")
	}
	if _, err := attestor.Verify(context.Background(), ref, "sandbox-a", "/proc/self/ns/net"); err == nil || fallback.calls != 1 {
		t.Fatal("recovery used creation-only fallback")
	}
	sandbox.err = status.Error(codes.Unavailable, "CRI outage")
	if _, err := attestor.VerifyCreating(context.Background(), ref, "sandbox-a", "/proc/self/ns/net"); err == nil || fallback.calls != 1 {
		t.Fatal("runtime outage treated as pre-publication")
	}
}

func TestCNICreatingSandboxIndependentEvidence(t *testing.T) {
	id := strings.Repeat("a", 64)
	ref := endpoint.WorkloadRef{Kind: "pod", UID: "pod-uid", Name: "pod-a", Namespace: "ns-a"}
	info := map[string]interface{}{"ID": id, "Labels": map[string]string{
		"io.cri-containerd.kind": "sandbox", "io.kubernetes.pod.uid": ref.UID,
		"io.kubernetes.pod.namespace": ref.Namespace, "io.kubernetes.pod.name": ref.Name,
	}}
	raw, _ := json.Marshal(info)
	tasks := []byte(fmt.Sprintf("TASK PID STATUS\n%s %d RUNNING\n", id, os.Getpid()))
	if err := verifyCreatingSandboxEvidence(raw, tasks, ref, id, "/proc/self/ns/net"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pod-replaced", "task-missing", "task-stopped", "task-duplicate", "namespace-wrong", "container-not-sandbox"} {
		t.Run(mode, func(t *testing.T) {
			r, listing, path, data := ref, tasks, "/proc/self/ns/net", raw
			switch mode {
			case "pod-replaced":
				r.UID = "foreign-pod"
			case "task-missing":
				listing = []byte("TASK PID STATUS\n")
			case "task-stopped":
				listing = []byte(fmt.Sprintf("TASK PID STATUS\n%s %d STOPPED\n", id, os.Getpid()))
			case "task-duplicate":
				listing = append(append([]byte(nil), tasks...), []byte(fmt.Sprintf("%s %d RUNNING\n", id, os.Getpid()))...)
			case "namespace-wrong":
				path = "/proc/self/ns/mnt"
			case "container-not-sandbox":
				info["Labels"].(map[string]string)["io.cri-containerd.kind"] = "container"
				data, _ = json.Marshal(info)
			}
			if err := verifyCreatingSandboxEvidence(data, listing, r, id, path); err == nil {
				t.Fatal("unsafe creating sandbox evidence accepted")
			}
		})
	}
}
