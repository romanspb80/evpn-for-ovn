package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"

	"github.com/containernetworking/cni/pkg/skel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type mockNodeAgentServer struct {
	nodeagentpb.UnimplementedNodeAgentCNIServer
	addFn   func(context.Context, *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error)
	delFn   func(context.Context, *nodeagentpb.CNIDelRequest) (*nodeagentpb.CNIDelResponse, error)
	checkFn func(context.Context, *nodeagentpb.CNICheckRequest) (*nodeagentpb.CNICheckResponse, error)
}

func (m *mockNodeAgentServer) Add(ctx context.Context, req *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error) {
	if m.addFn == nil {
		return &nodeagentpb.CNIAddResponse{Success: true}, nil
	}
	return m.addFn(ctx, req)
}

func (m *mockNodeAgentServer) Del(ctx context.Context, req *nodeagentpb.CNIDelRequest) (*nodeagentpb.CNIDelResponse, error) {
	if m.delFn == nil {
		return &nodeagentpb.CNIDelResponse{
			Success: true,
			Outcome: nodeagentpb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT,
		}, nil
	}
	return m.delFn(ctx, req)
}

func (m *mockNodeAgentServer) Check(ctx context.Context, req *nodeagentpb.CNICheckRequest) (*nodeagentpb.CNICheckResponse, error) {
	if m.checkFn == nil {
		return &nodeagentpb.CNICheckResponse{Success: true}, nil
	}
	return m.checkFn(ctx, req)
}

func TestCmdAddMapsRequestAndPrintsResult(t *testing.T) {
	t.Helper()

	var (
		mu        sync.Mutex
		captured  *nodeagentpb.CNIAddRequest
		socket, s = startMockNodeAgentServer(t, &mockNodeAgentServer{
			addFn: func(_ context.Context, req *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error) {
				mu.Lock()
				captured = req
				mu.Unlock()
				return &nodeagentpb.CNIAddResponse{
					Success: true,
					Result: &nodeagentpb.CNIResult{
						CniVersion: "1.0.0",
						Interfaces: []*nodeagentpb.CNIInterface{{
							Name:    "net1",
							Mac:     "0a:58:aa:bb:cc:dd",
							Sandbox: "/proc/1/ns/net",
						}},
						Ips: []*nodeagentpb.CNIIPConfig{{
							Address:   "10.251.1.11/24",
							Interface: 0,
							Gateway:   "10.251.1.1",
						}},
						Routes: []*nodeagentpb.CNIRoute{{
							Dst: "0.0.0.0/0",
							Gw:  "10.251.1.1",
						}},
						Dns: &nodeagentpb.CNIDNS{
							Nameservers: []string{"8.8.8.8"},
							Searches:    []string{"svc.cluster.local"},
						},
					},
				}, nil
			},
		})
	)
	defer s()

	conf := fmt.Sprintf(`{
		"cniVersion":"1.0.0",
		"name":"evpn-net",
		"type":"evpn-cni",
		"nodeAgentSocket":"%s",
		"networkID":"net-h22",
		"logicalSwitch":"ls-h22",
		"attachmentID":"net1",
		"ipAddress":"10.251.1.11/24",
		"macAddress":"0a:58:aa:bb:cc:dd",
		"timeoutSeconds":2,
		"requestID":"req-123"
	}`, socket)
	args := &skel.CmdArgs{
		ContainerID: "container-123",
		Netns:       "/proc/100/ns/net",
		IfName:      "eth1",
		Args:        "K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-a;K8S_POD_UID=uid-a;K8S_NODE_NAME=node-a",
		StdinData:   []byte(conf),
	}

	stdout, stderr, err := captureOutput(func() error { return cmdAdd(args) })
	if err != nil {
		t.Fatalf("cmdAdd returned error: %v", err)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("unexpected stderr output: %q", stderr)
	}

	mu.Lock()
	gotReq := captured
	mu.Unlock()
	if gotReq == nil {
		t.Fatalf("server did not capture Add request")
	}
	if gotReq.GetRuntime().GetContainerId() != "container-123" {
		t.Fatalf("unexpected container_id: %q", gotReq.GetRuntime().GetContainerId())
	}
	if gotReq.GetRuntime().GetNetnsPath() != "/proc/100/ns/net" {
		t.Fatalf("unexpected netns_path: %q", gotReq.GetRuntime().GetNetnsPath())
	}
	if gotReq.GetK8S().GetPodNamespace() != "default" || gotReq.GetK8S().GetPodName() != "pod-a" || gotReq.GetK8S().GetPodUid() != "uid-a" {
		t.Fatalf("unexpected k8s identity: %+v", gotReq.GetK8S())
	}
	if gotReq.GetIdentity().GetNetworkId() != "net-h22" || gotReq.GetIdentity().GetAttachmentId() != "net1" {
		t.Fatalf("unexpected endpoint identity: %+v", gotReq.GetIdentity())
	}
	if gotReq.GetAttachment().GetLogicalSwitch() != "ls-h22" {
		t.Fatalf("unexpected logical switch: %q", gotReq.GetAttachment().GetLogicalSwitch())
	}
	if gotReq.GetAttachment().GetStaticIpCidr() != "10.251.1.11/24" || gotReq.GetAttachment().GetStaticMac() != "0a:58:aa:bb:cc:dd" {
		t.Fatalf("unexpected static config: %+v", gotReq.GetAttachment())
	}
	if gotReq.GetMeta().GetRequestId() != "req-123" {
		t.Fatalf("unexpected request_id: %q", gotReq.GetMeta().GetRequestId())
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to decode ADD stdout as CNI result JSON: %v", err)
	}
	if result["cniVersion"] != "1.0.0" {
		t.Fatalf("unexpected cniVersion in output: %#v", result["cniVersion"])
	}
	if _, ok := result["interfaces"]; !ok {
		t.Fatalf("missing interfaces in output JSON: %s", stdout)
	}
}

func TestCmdDelAcceptsIdempotentOutcomes(t *testing.T) {
	outcomes := []nodeagentpb.DelOutcome{
		nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED,
		nodeagentpb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT,
		nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION,
	}

	for _, outcome := range outcomes {
		outcome := outcome
		t.Run(outcome.String(), func(t *testing.T) {
			socket, cleanup := startMockNodeAgentServer(t, &mockNodeAgentServer{
				delFn: func(_ context.Context, _ *nodeagentpb.CNIDelRequest) (*nodeagentpb.CNIDelResponse, error) {
					return &nodeagentpb.CNIDelResponse{
						Success: true,
						Outcome: outcome,
					}, nil
				},
			})
			defer cleanup()

			conf := fmt.Sprintf(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","nodeAgentSocket":"%s","networkID":"net-h22","attachmentID":"net1","timeoutSeconds":2}`, socket)
			args := &skel.CmdArgs{
				ContainerID: "container-123",
				Netns:       "/proc/100/ns/net",
				IfName:      "eth1",
				StdinData:   []byte(conf),
			}

			stdout, stderr, err := captureOutput(func() error { return cmdDel(args) })
			if err != nil {
				t.Fatalf("cmdDel returned error for %s: %v", outcome.String(), err)
			}
			if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) != "" {
				t.Fatalf("unexpected output for DEL: stdout=%q stderr=%q", stdout, stderr)
			}
		})
	}
}

func TestCmdCheckMapsRequestAndPropagatesFailure(t *testing.T) {
	var gotReq *nodeagentpb.CNICheckRequest
	socket, cleanup := startMockNodeAgentServer(t, &mockNodeAgentServer{
		checkFn: func(_ context.Context, req *nodeagentpb.CNICheckRequest) (*nodeagentpb.CNICheckResponse, error) {
			gotReq = req
			return &nodeagentpb.CNICheckResponse{
				Success: false,
				Message: "ovs attachment missing",
			}, nil
		},
	})
	defer cleanup()

	conf := fmt.Sprintf(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","nodeAgentSocket":"%s","networkID":"net-h22","attachmentID":"net1","timeoutSeconds":2}`, socket)
	args := &skel.CmdArgs{
		ContainerID: "container-123",
		Netns:       "/proc/100/ns/net",
		IfName:      "eth1",
		Args:        "K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-check;K8S_POD_UID=uid-check",
		StdinData:   []byte(conf),
	}

	stdout, _, err := captureOutput(func() error { return cmdCheck(args) })
	if err == nil || !strings.Contains(err.Error(), "ovs attachment missing") {
		t.Fatalf("expected check failure to propagate, got err=%v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("CHECK should not write protocol output, got: %q", stdout)
	}
	if gotReq == nil {
		t.Fatalf("server did not receive CHECK request")
	}
	if gotReq.GetRuntime().GetIfName() != "eth1" {
		t.Fatalf("unexpected ifName mapping: %q", gotReq.GetRuntime().GetIfName())
	}
}

func TestInvalidConfigAndUnavailableSocketAndDeadline(t *testing.T) {
	t.Run("invalid-config-no-socket", func(t *testing.T) {
		args := &skel.CmdArgs{
			ContainerID: "container-123",
			Netns:       "/proc/100/ns/net",
			IfName:      "eth1",
			StdinData:   []byte(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","networkID":"net-h22","attachmentID":"net1"}`),
		}
		stdout, _, err := captureOutput(func() error { return cmdAdd(args) })
		if err == nil || !strings.Contains(err.Error(), "nodeAgentSocket is required") {
			t.Fatalf("expected invalid config error, got: %v", err)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("expected no stdout for invalid config, got: %q", stdout)
		}
	})

	t.Run("unavailable-socket", func(t *testing.T) {
		args := &skel.CmdArgs{
			ContainerID: "container-123",
			Netns:       "/proc/100/ns/net",
			IfName:      "eth1",
			StdinData:   []byte(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","nodeAgentSocket":"/tmp/does-not-exist.sock","networkID":"net-h22","attachmentID":"net1","timeoutSeconds":1}`),
		}
		stdout, _, err := captureOutput(func() error { return cmdAdd(args) })
		if err == nil || !strings.Contains(err.Error(), "dial node-agent socket") {
			t.Fatalf("expected socket dial error, got: %v", err)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("expected no stdout for dial error, got: %q", stdout)
		}
	})

	t.Run("rpc-deadline-exceeded", func(t *testing.T) {
		socket, cleanup := startMockNodeAgentServer(t, &mockNodeAgentServer{
			addFn: func(ctx context.Context, _ *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(3 * time.Second):
					return &nodeagentpb.CNIAddResponse{Success: true}, nil
				}
			},
		})
		defer cleanup()

		args := &skel.CmdArgs{
			ContainerID: "container-123",
			Netns:       "/proc/100/ns/net",
			IfName:      "eth1",
			StdinData:   []byte(fmt.Sprintf(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","nodeAgentSocket":"%s","networkID":"net-h22","attachmentID":"net1","timeoutSeconds":1}`, socket)),
		}
		stdout, _, err := captureOutput(func() error { return cmdAdd(args) })
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("expected deadline exceeded, got: %v", err)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("expected no stdout for RPC timeout, got: %q", stdout)
		}
	})
}

func TestMultusIdentityAcrossLifecycle(t *testing.T) {
	for _, netns := range []string{"/run/netns/cni-sandbox", ""} {
		t.Run("DEL-netns="+netns, func(t *testing.T) {
			conf, parsed, err := parseConfig([]byte(`{"cniVersion":"1.0.0","name":"secondary","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"n24","attachmentID":"s"}`),
				"IgnoreUnknown=1;K8S_POD_NAMESPACE=test;K8S_POD_NAME=pod-a;K8S_POD_UID=pod-uid-a;K8S_POD_INFRA_CONTAINER_ID=sandbox-a;FUTURE_ARG=value")
			if err != nil {
				t.Fatal(err)
			}
			args := &skel.CmdArgs{ContainerID: "sandbox-a", Netns: "/run/netns/cni-sandbox", IfName: "net1"}
			add := buildAddRequest(conf, parsed, args)
			check := buildCheckRequest(conf, parsed, args)
			args.Netns = netns
			del := buildDelRequest(conf, parsed, args)
			if !proto.Equal(add.Identity, check.Identity) || !proto.Equal(add.Identity, del.Identity) {
				t.Fatalf("identity drift: ADD=%v CHECK=%v DEL=%v", add.Identity, check.Identity, del.Identity)
			}
			if add.Identity.WorkloadUid != "pod-uid-a" || add.Identity.Incarnation != "sandbox-a" || del.Runtime.NetnsPath != netns {
				t.Fatalf("incorrect mapping: %v", del)
			}
			// No Kubernetes API client is used: disappearance of the Pod cannot
			// change identity supplied by the runtime on cached DEL.
			parsed.K8S_POD_NAME = ""
			parsed.K8S_POD_NAMESPACE = ""
			if !proto.Equal(add.Identity, buildDelRequest(conf, parsed, args).Identity) {
				t.Fatal("missing display names changed DEL identity")
			}
			parsed.K8S_POD_UID = "new-pod-uid"
			args.ContainerID = "new-sandbox"
			recreated := buildAddRequest(conf, parsed, args)
			if recreated.Identity.EndpointId == add.Identity.EndpointId || recreated.Identity.Incarnation == add.Identity.Incarnation {
				t.Fatal("Pod recreation reused endpoint identity")
			}
		})
	}
}

func TestMultusMissingUIDFailsBeforeRPC(t *testing.T) {
	for _, command := range []struct {
		name string
		call func(*skel.CmdArgs) error
	}{{"ADD", cmdAdd}, {"CHECK", cmdCheck}, {"DEL", cmdDel}} {
		t.Run(command.name, func(t *testing.T) {
			_, _, err := captureOutput(func() error {
				return command.call(&skel.CmdArgs{
					ContainerID: "sandbox-a", IfName: "net1",
					Args:      "IgnoreUnknown=1;K8S_POD_NAMESPACE=test;K8S_POD_NAME=gone;K8S_POD_INFRA_CONTAINER_ID=sandbox-a",
					StdinData: []byte(`{"cniVersion":"1.0.0","name":"secondary","type":"evpn-cni","nodeAgentSocket":"/nonexistent.sock","networkID":"n24","attachmentID":"s"}`),
				})
			})
			if err == nil || !strings.Contains(err.Error(), "requires K8S_POD_UID") {
				t.Fatalf("ambiguous identity must fail before connecting: %v", err)
			}
		})
	}
}

func TestVersionAdvertisesOnlyVerifiedContract(t *testing.T) {
	cmd := exec.Command("go", "run", ".")
	cmd.Env = append(os.Environ(), "CNI_COMMAND=VERSION")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatal(err)
	}
	if len(info.SupportedVersions) != 1 || info.SupportedVersions[0] != "1.0.0" {
		t.Fatalf("unverified advertised versions: %s", out)
	}
}

func startMockNodeAgentServer(t *testing.T, srv nodeagentpb.NodeAgentCNIServer) (string, func()) {
	t.Helper()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("evpn-cni-%d.sock", time.Now().UnixNano()))
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
	}

	grpcServer := grpc.NewServer()
	nodeagentpb.RegisterNodeAgentCNIServer(grpcServer, srv)
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	cleanup := func() {
		grpcServer.Stop()
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
	return socketPath, cleanup
}

func captureOutput(fn func() error) (stdout, stderr string, err error) {
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	stdoutReader, stdoutWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return "", "", pipeErr
	}
	stderrReader, stderrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return "", "", pipeErr
	}

	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter

	runErr := fn()

	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	stdoutBytes, readErr := io.ReadAll(stdoutReader)
	if readErr != nil {
		return "", "", readErr
	}
	stderrBytes, readErr := io.ReadAll(stderrReader)
	if readErr != nil {
		return "", "", readErr
	}
	_ = stdoutReader.Close()
	_ = stderrReader.Close()

	return string(stdoutBytes), string(stderrBytes), runErr
}
