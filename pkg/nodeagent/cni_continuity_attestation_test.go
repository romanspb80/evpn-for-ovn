package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type cniSandboxClient struct {
	cri.RuntimeServiceClient
	response *cri.PodSandboxStatusResponse
	err      error
}

func (c *cniSandboxClient) PodSandboxStatus(_ context.Context, req *cri.PodSandboxStatusRequest, _ ...grpc.CallOption) (*cri.PodSandboxStatusResponse, error) {
	if req.PodSandboxId != "sandbox-a" || !req.Verbose {
		return nil, errors.New("wrong read-only sandbox query")
	}
	return c.response, c.err
}

func TestKubernetesCNIWorkloadAttestation(t *testing.T) {
	for _, mode := range []string{"verified", "pod-notfound", "pod-replaced", "pod-deleting", "node-replaced", "api-unavailable", "wrong-sandbox-pod", "sandbox-notready", "missing-task-pid", "sandbox-unavailable", "wrong-netns"} {
		t.Run(mode, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID("node-uid")}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "ns-a", UID: types.UID("pod-uid")}, Spec: corev1.PodSpec{NodeName: "node-a"}}
			client := fake.NewSimpleClientset(node, pod)
			pid, _ := json.Marshal(struct {
				PID int `json:"pid"`
			}{os.Getpid()})
			sandbox := &cniSandboxClient{response: &cri.PodSandboxStatusResponse{
				Status: &cri.PodSandboxStatus{Id: "sandbox-a", State: cri.PodSandboxState_SANDBOX_READY,
					Metadata: &cri.PodSandboxMetadata{Uid: "pod-uid", Name: "pod-a", Namespace: "ns-a"}},
				Info: map[string]string{"info": string(pid)},
			}}
			ref := endpoint.WorkloadRef{Kind: "pod", UID: "pod-uid", Name: "pod-a", Namespace: "ns-a", NodeName: "node-a"}
			path := "/proc/self/ns/net"
			switch mode {
			case "pod-notfound":
				client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pod-a")
				})
			case "pod-replaced":
				ref.UID = "stale-pod"
			case "pod-deleting":
				client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					now := metav1.Now()
					pod.DeletionTimestamp = &now
					return true, pod, nil
				})
			case "node-replaced":
				client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
					node.UID = "replacement"
					return true, node, nil
				})
			case "api-unavailable":
				client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API outage") })
			case "wrong-sandbox-pod":
				sandbox.response.Status.Metadata.Uid = "foreign-pod"
			case "sandbox-notready":
				sandbox.response.Status.State = cri.PodSandboxState_SANDBOX_NOTREADY
			case "missing-task-pid":
				sandbox.response.Info = nil
			case "sandbox-unavailable":
				sandbox.err = errors.New("CRI offline")
			case "wrong-netns":
				path = "/proc/self/ns/mnt"
			}

			attestor := &KubernetesCNIWorkloadAttestor{Client: client.CoreV1(), NodeName: "node-a", NodeUID: "node-uid", Sandbox: sandbox}
			deleted, err := attestor.Verify(context.Background(), ref, "sandbox-a", path)
			if mode == "verified" && (err != nil || deleted) {
				t.Fatalf("independent verification failed: %v", err)
			}
			if mode == "pod-notfound" && (err != nil || !deleted) {
				t.Fatal("known deleted Pod not recognized")
			}
			if mode != "verified" && mode != "pod-notfound" && (err == nil || deleted) {
				t.Fatalf("unsafe attestation: deleted=%v error=%v", deleted, err)
			}
			if mode == "sandbox-notready" {
				if deleted, err := attestor.VerifyCreating(context.Background(), ref, "sandbox-a", path); err != nil || deleted {
					t.Fatalf("independently identified configuring sandbox denied initial ADD: %v", err)
				}
			}
		})
	}
}
