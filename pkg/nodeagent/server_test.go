package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type fakeEndpointManager struct {
	mu          sync.Mutex
	ensureCount int
	getCount    int
	deleteCount int
	ensureErr   error
	getErr      error
	deleteErr   error
	specs       map[endpoint.EndpointID]endpoint.EndpointSpec
}

type fakeNetworkBackend struct {
	mu         sync.Mutex
	ensureErr  error
	deleteErr  error
	checkErr   error
	state      networkState
	created    bool
	ensureCnt  int
	deleteCnt  int
	checkCnt   int
	ensureKeys []string
}

type fakeAttachmentChecker struct {
	err               error
	checkCnt          int
	expectedInterface string
}

func (f *fakeAttachmentChecker) CheckAttachment(_ context.Context, spec endpoint.EndpointSpec) error {
	f.checkCnt++
	if f.expectedInterface != "" && spec.OVSInterfaceName != f.expectedInterface {
		return fmt.Errorf("interface name mismatch existing=%q expected=%q", f.expectedInterface, spec.OVSInterfaceName)
	}
	return f.err
}

func (f *fakeNetworkBackend) Ensure(_ context.Context, key string, _ *endpointRecord) (networkState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCnt++
	f.ensureKeys = append(f.ensureKeys, key)
	if f.ensureErr != nil {
		return networkState{}, false, f.ensureErr
	}
	if f.state == (networkState{}) {
		f.state = networkState{
			HostIfName:      "evp-test0",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.1.11/24",
		}
	}
	return f.state, f.created, nil
}

func (f *fakeNetworkBackend) Delete(_ context.Context, _ string, _ *endpointRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCnt++
	return f.deleteErr
}

func (f *fakeNetworkBackend) Check(_ context.Context, _ string, _ *endpointRecord) (networkState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCnt++
	if f.checkErr != nil {
		return networkState{}, f.checkErr
	}
	return f.state, nil
}

func newFakeEndpointManager() *fakeEndpointManager {
	return &fakeEndpointManager{
		specs: make(map[endpoint.EndpointID]endpoint.EndpointSpec),
	}
}

func (f *fakeEndpointManager) EnsureEndpoint(_ context.Context, spec endpoint.EndpointSpec) (endpoint.EndpointStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return endpoint.EndpointStatus{}, f.ensureErr
	}
	f.ensureCount++
	f.specs[spec.ID] = spec
	return endpoint.EndpointStatus{
		ID:                spec.ID,
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
		OVSInterfaceName:  spec.OVSInterfaceName,
		OVNPortState:      endpoint.StateEnsured,
		LocalAttachState:  endpoint.StateEnsured,
	}, nil
}

func (f *fakeEndpointManager) GetEndpoint(_ context.Context, id endpoint.EndpointID, logicalSwitchPort string) (endpoint.EndpointStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return endpoint.EndpointStatus{}, f.getErr
	}
	f.getCount++
	spec, ok := f.specs[id]
	if !ok || spec.LogicalSwitchPort != logicalSwitchPort {
		return endpoint.EndpointStatus{}, fmt.Errorf("not found")
	}
	return endpoint.EndpointStatus{
		ID:                id,
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
		OVNPortState:      endpoint.StateEnsured,
	}, nil
}

func (f *fakeEndpointManager) DeleteEndpoint(_ context.Context, spec endpoint.EndpointSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleteCount++
	delete(f.specs, spec.ID)
	return nil
}

func TestAddCheckDelLifecycleOverUnixSocket(t *testing.T) {
	socketPath := uniqueSocketPath(t, "lifecycle")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()

	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	addResp, err := client.Add(ctx, sampleAddRequest("ep-lifecycle", "inc-a", "10.251.1.11/24"))
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if !addResp.GetSuccess() || addResp.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("unexpected add response: %+v", addResp)
	}

	checkResp, err := client.Check(ctx, sampleCheckRequest("ep-lifecycle", "inc-a"))
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !checkResp.GetSuccess() || !checkResp.GetDetails().GetEndpointIdentityValid() {
		t.Fatalf("unexpected check response: %+v", checkResp)
	}

	delResp, err := client.Del(ctx, sampleDelRequest("ep-lifecycle", "inc-a"))
	if err != nil {
		t.Fatalf("Del failed: %v", err)
	}
	if delResp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED {
		t.Fatalf("unexpected DEL outcome: %s", delResp.GetOutcome().String())
	}

	_, err = client.Check(ctx, sampleCheckRequest("ep-lifecycle", "inc-a"))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound after delete, got err=%v", err)
	}
}

func TestMapCNIAddRequestToEndpointSpec(t *testing.T) {
	req := sampleAddRequest("ep-map", "inc-map", "10.251.10.11/24")
	req.K8S = &nodeagentpb.K8SIdentity{
		PodNamespace: "ns-a",
		PodName:      "pod-a",
		PodUid:       "uid-a",
		NodeName:     "node-a",
	}
	spec, err := mapCNIAddRequestToEndpointSpec(req)
	if err != nil {
		t.Fatalf("mapping failed: %v", err)
	}
	if string(spec.ID) != "ep-map" {
		t.Fatalf("unexpected endpoint id: %q", spec.ID)
	}
	if spec.NetworkID != "net-h22" || spec.AttachmentID != "net1" {
		t.Fatalf("unexpected network mapping: %+v", spec)
	}
	if spec.Workload.UID != "uid-a" || spec.Workload.Name != "pod-a" || spec.Workload.Namespace != "ns-a" || spec.Workload.NodeName != "node-a" {
		t.Fatalf("unexpected workload mapping: %+v", spec.Workload)
	}
	if spec.LogicalSwitch != "ls-h22" {
		t.Fatalf("unexpected logical switch: %q", spec.LogicalSwitch)
	}
	if spec.MACAddress != "0a:58:fb:03:00:11" {
		t.Fatalf("unexpected MAC mapping: %q", spec.MACAddress)
	}
	if len(spec.IPAddresses) != 1 || spec.IPAddresses[0] != "10.251.10.11/24" {
		t.Fatalf("unexpected IP mapping: %+v", spec.IPAddresses)
	}
	if !strings.Contains(spec.LogicalSwitchPort, "net-h22") || !strings.Contains(spec.LogicalSwitchPort, "net1") {
		t.Fatalf("logical switch port should include network and attachment context, got %q", spec.LogicalSwitchPort)
	}
}

func TestMapCNIAddRequestToEndpointSpecRequiresStaticMAC(t *testing.T) {
	req := sampleAddRequest("ep-map-mac", "inc-map", "10.251.10.12/24")
	req.Attachment.StaticMac = ""
	_, err := mapCNIAddRequestToEndpointSpec(req)
	if err == nil || !strings.Contains(err.Error(), "attachment.static_mac is required") {
		t.Fatalf("expected static MAC requirement error, got: %v", err)
	}
}

func TestServerReusesEndpointManagerLifecycle(t *testing.T) {
	socketPath := uniqueSocketPath(t, "manager-lifecycle")
	fakeMgr := newFakeEndpointManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, err := StartUnixServer(ctx, socketPath, NewCNIServerWithManager(NewStore(), fakeMgr))
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop(context.Background()) }()

	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeoutCancel()

	if _, err := client.Add(timeoutCtx, sampleAddRequest("ep-mgr", "inc-a", "10.251.1.200/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if _, err := client.Check(timeoutCtx, sampleCheckRequest("ep-mgr", "inc-a")); err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if _, err := client.Del(timeoutCtx, sampleDelRequest("ep-mgr", "inc-a")); err != nil {
		t.Fatalf("Del failed: %v", err)
	}

	fakeMgr.mu.Lock()
	defer fakeMgr.mu.Unlock()
	if fakeMgr.ensureCount == 0 || fakeMgr.getCount == 0 || fakeMgr.deleteCount == 0 {
		t.Fatalf("expected endpoint manager lifecycle methods to be used, got ensure=%d get=%d delete=%d", fakeMgr.ensureCount, fakeMgr.getCount, fakeMgr.deleteCount)
	}
}

func TestRepeatedAddIsIdempotent(t *testing.T) {
	socketPath := uniqueSocketPath(t, "idempotent-add")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := sampleAddRequest("ep-replay", "inc-a", "10.251.1.12/24")

	first, err := client.Add(ctx, req)
	if err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	second, err := client.Add(ctx, req)
	if err != nil {
		t.Fatalf("second Add failed: %v", err)
	}
	if first.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("first add should not be replay")
	}
	if !second.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("second add should be replay")
	}
	if normalizeResult(first.GetResult()) != normalizeResult(second.GetResult()) {
		t.Fatalf("replayed ADD returned different result")
	}
}

func TestRepeatedDelIsIdempotent(t *testing.T) {
	socketPath := uniqueSocketPath(t, "idempotent-del")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Add(ctx, sampleAddRequest("ep-del", "inc-a", "10.251.1.13/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	first, err := client.Del(ctx, sampleDelRequest("ep-del", "inc-a"))
	if err != nil {
		t.Fatalf("first Del failed: %v", err)
	}
	second, err := client.Del(ctx, sampleDelRequest("ep-del", "inc-a"))
	if err != nil {
		t.Fatalf("second Del failed: %v", err)
	}
	if first.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED {
		t.Fatalf("unexpected first DEL outcome: %s", first.GetOutcome().String())
	}
	if second.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT {
		t.Fatalf("unexpected second DEL outcome: %s", second.GetOutcome().String())
	}
}

func TestConflictingAddRequestsFail(t *testing.T) {
	socketPath := uniqueSocketPath(t, "conflict-add")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Add(ctx, sampleAddRequest("ep-conflict", "inc-a", "10.251.1.14/24")); err != nil {
		t.Fatalf("initial Add failed: %v", err)
	}

	_, err := client.Add(ctx, sampleAddRequest("ep-conflict", "inc-a", "10.251.1.99/24"))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists for conflicting ADD, got err=%v", err)
	}
}

func TestFailedAddDoesNotPersistCreatedState(t *testing.T) {
	socketPath := uniqueSocketPath(t, "failed-add")
	fakeMgr := newFakeEndpointManager()
	fakeMgr.ensureErr = fmt.Errorf("ensure failed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := StartUnixServer(ctx, socketPath, NewCNIServerWithManager(NewStore(), fakeMgr))
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeoutCancel()

	_, err = client.Add(timeoutCtx, sampleAddRequest("ep-failed-add", "inc-a", "10.251.4.11/24"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on Add failure, got err=%v", err)
	}
	_, err = client.Check(timeoutCtx, sampleCheckRequest("ep-failed-add", "inc-a"))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("failed Add should not leave created state; expected NotFound, got err=%v", err)
	}

	fakeMgr.mu.Lock()
	fakeMgr.ensureErr = nil
	fakeMgr.mu.Unlock()
	if _, err := client.Add(timeoutCtx, sampleAddRequest("ep-failed-add", "inc-a", "10.251.4.11/24")); err != nil {
		t.Fatalf("retry Add should succeed after ensure recovers, got err=%v", err)
	}
}

func TestFailedDelPreservesStateForRetry(t *testing.T) {
	socketPath := uniqueSocketPath(t, "failed-del")
	fakeMgr := newFakeEndpointManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := StartUnixServer(ctx, socketPath, NewCNIServerWithManager(NewStore(), fakeMgr))
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeoutCancel()
	if _, err := client.Add(timeoutCtx, sampleAddRequest("ep-failed-del", "inc-a", "10.251.4.21/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	fakeMgr.mu.Lock()
	fakeMgr.deleteErr = fmt.Errorf("delete failed")
	fakeMgr.mu.Unlock()

	_, err = client.Del(timeoutCtx, sampleDelRequest("ep-failed-del", "inc-a"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on Del failure, got err=%v", err)
	}
	if _, err := client.Check(timeoutCtx, sampleCheckRequest("ep-failed-del", "inc-a")); err != nil {
		t.Fatalf("failed Del must preserve state for retry, check err=%v", err)
	}

	fakeMgr.mu.Lock()
	fakeMgr.deleteErr = nil
	fakeMgr.mu.Unlock()
	delResp, err := client.Del(timeoutCtx, sampleDelRequest("ep-failed-del", "inc-a"))
	if err != nil {
		t.Fatalf("retry Del should succeed, got err=%v", err)
	}
	if delResp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED {
		t.Fatalf("unexpected DEL outcome after retry: %s", delResp.GetOutcome().String())
	}
}

func TestIdentityContextPreventsCrossAttachmentCollision(t *testing.T) {
	socketPath := uniqueSocketPath(t, "identity-context")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	reqA := sampleAddRequest("ep-shared", "inc-a", "10.251.5.11/24")
	reqB := sampleAddRequest("ep-shared", "inc-b", "10.251.6.11/24")
	reqB.Identity.NetworkId = "net-h23"
	reqB.Identity.AttachmentId = "net2"
	reqB.Attachment.LogicalSwitch = "ls-h23"
	reqB.Attachment.OvsInterfaceName = "net2"
	reqB.Runtime.IfName = "net2"
	reqB.Attachment.StaticMac = "0a:58:fb:03:00:22"

	if _, err := client.Add(ctx, reqA); err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	if _, err := client.Add(ctx, reqB); err != nil {
		t.Fatalf("second Add with same endpoint_id but different identity context should succeed, got err=%v", err)
	}
	if _, err := client.Check(ctx, sampleCheckRequest("ep-shared", "inc-a")); err != nil {
		t.Fatalf("check for first attachment failed: %v", err)
	}
	checkB := sampleCheckRequest("ep-shared", "inc-b")
	checkB.Identity.NetworkId = "net-h23"
	checkB.Identity.AttachmentId = "net2"
	if _, err := client.Check(ctx, checkB); err != nil {
		t.Fatalf("check for second attachment failed: %v", err)
	}
}

func TestStaleDeleteProtection(t *testing.T) {
	socketPath := uniqueSocketPath(t, "stale-del")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Add(ctx, sampleAddRequest("ep-stale", "inc-a", "10.251.1.15/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	delResp, err := client.Del(ctx, sampleDelRequest("ep-stale", "inc-b"))
	if err != nil {
		t.Fatalf("stale Del RPC failed: %v", err)
	}
	if delResp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION {
		t.Fatalf("expected stale operation outcome, got: %s", delResp.GetOutcome().String())
	}

	if _, err := client.Check(ctx, sampleCheckRequest("ep-stale", "inc-a")); err != nil {
		t.Fatalf("endpoint should remain after stale DEL, check err=%v", err)
	}
}

func TestStaleDeleteSkipsDownstreamDeletion(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{}
	server := NewCNIServerWithBackends(store, fakeMgr, fakeNet)

	if _, err := server.Add(context.Background(), sampleAddRequest("ep-stale-downstream", "inc-a", "10.251.7.10/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	resp, err := server.Del(context.Background(), sampleDelRequest("ep-stale-downstream", "inc-b"))
	if err != nil {
		t.Fatalf("Del failed: %v", err)
	}
	if resp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION {
		t.Fatalf("expected stale outcome, got %s", resp.GetOutcome().String())
	}

	fakeMgr.mu.Lock()
	deleteCount := fakeMgr.deleteCount
	fakeMgr.mu.Unlock()
	if deleteCount != 0 {
		t.Fatalf("expected stale delete to skip endpoint deletion, got %d", deleteCount)
	}
	fakeNet.mu.Lock()
	netDeletes := fakeNet.deleteCnt
	fakeNet.mu.Unlock()
	if netDeletes != 0 {
		t.Fatalf("expected stale delete to skip network deletion, got %d", netDeletes)
	}
}

func TestCheckRecoversWithoutInMemoryStoreRecord(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-recover1",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.11.11/24",
		},
	}
	server := NewCNIServerWithBackendsAndChecker(store, fakeMgr, fakeNet, &fakeAttachmentChecker{})
	addReq := sampleAddRequest("ep-recover-check", "inc-a", "10.251.11.11/24")
	if _, err := server.Add(context.Background(), addReq); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	store.delete(endpointKey(addReq.GetIdentity()))

	resp, err := server.Check(context.Background(), sampleCheckRequest("ep-recover-check", "inc-a"))
	if err != nil {
		t.Fatalf("Check after store loss failed: %v", err)
	}
	if !resp.GetSuccess() || !resp.GetDetails().GetEndpointIdentityValid() {
		t.Fatalf("unexpected check response: %+v", resp)
	}
}

func TestDelRecoversWithoutInMemoryStoreRecord(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-recover2",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.11.12/24",
		},
	}

	server := NewCNIServerWithBackendsAndChecker(store, fakeMgr, fakeNet, &fakeAttachmentChecker{})
	addReq := sampleAddRequest("ep-recover-del", "inc-a", "10.251.11.12/24")
	if _, err := server.Add(context.Background(), addReq); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	store.delete(endpointKey(addReq.GetIdentity()))

	resp, err := server.Del(context.Background(), sampleDelRequest("ep-recover-del", "inc-a"))
	if err != nil {
		t.Fatalf("Del after store loss failed: %v", err)
	}
	if resp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED {
		t.Fatalf("unexpected DEL outcome: %s", resp.GetOutcome().String())
	}

	fakeNet.mu.Lock()
	deleteCount := fakeNet.deleteCnt
	fakeNet.mu.Unlock()
	if deleteCount == 0 {
		t.Fatalf("expected network deletion to be invoked during recovered DEL")
	}
}

func TestRestartCheckUsesHostInterfaceWithoutCachingRecoveredRecord(t *testing.T) {
	ctx := context.Background()
	req := sampleAddRequest("ep-restart-host", "inc-a", "10.251.11.14/24")
	mgr := newFakeEndpointManager()
	network := &fakeNetworkBackend{}
	initial := NewCNIServerWithBackends(NewStore(), mgr, network)
	if _, err := initial.Add(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, failCheck := range []bool{false, true} {
		t.Run(fmt.Sprintf("attachment-failure=%t", failCheck), func(t *testing.T) {
			store := NewStore()
			checker := &fakeAttachmentChecker{expectedInterface: network.state.HostIfName}
			if failCheck {
				checker.err = fmt.Errorf("controlled OVS check failure")
			}
			restarted := NewCNIServerWithBackendsAndChecker(store, mgr, network, checker)
			check := sampleCheckRequest("ep-restart-host", "inc-a")
			check.Attachment = cloneAttachment(req.Attachment)
			_, err := restarted.Check(ctx, check)
			if failCheck {
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("expected failed OVS check, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("CHECK after restart: %v", err)
			}
			if _, exists := store.get(endpointKey(req.Identity)); exists {
				t.Fatal("read-only CHECK cached an incomplete recovered record")
			}
			if _, err := restarted.Add(ctx, req); err != nil {
				t.Fatalf("ADD after recovered CHECK: %v", err)
			}
		})
	}
}

func TestRecoveredDELWithoutRuntimeFailsSafely(t *testing.T) {
	mgr := newFakeEndpointManager()
	network := &fakeNetworkBackend{}
	server := NewCNIServerWithBackendsAndChecker(NewStore(), mgr, network, &fakeAttachmentChecker{})
	req := sampleDelRequest("ep-missing-context", "inc-a")
	req.Runtime = nil
	if _, err := server.Del(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing runtime cannot prove absence: %v", err)
	}
	if mgr.deleteCount != 0 || network.deleteCnt != 0 {
		t.Fatal("ambiguous DEL called downstream deletion")
	}
}

func TestRecoveredStaleDeleteSkipsNetworkDeletion(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-recover3",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.11.13/24",
		},
	}
	server := NewCNIServerWithBackendsAndChecker(store, fakeMgr, fakeNet, &fakeAttachmentChecker{})
	addReq := sampleAddRequest("ep-recover-stale", "inc-a", "10.251.11.13/24")
	if _, err := server.Add(context.Background(), addReq); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	store.delete(endpointKey(addReq.GetIdentity()))

	fakeMgr.mu.Lock()
	fakeMgr.deleteErr = fmt.Errorf("stale operation: expected incarnation=%q got=%q", "inc-b", "inc-a")
	fakeMgr.mu.Unlock()
	resp, err := server.Del(context.Background(), sampleDelRequest("ep-recover-stale", "inc-b"))
	if err != nil {
		t.Fatalf("stale Del failed: %v", err)
	}
	if resp.GetOutcome() != nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION {
		t.Fatalf("unexpected stale DEL outcome: %s", resp.GetOutcome().String())
	}
	fakeNet.mu.Lock()
	deleteCount := fakeNet.deleteCnt
	fakeNet.mu.Unlock()
	if deleteCount != 0 {
		t.Fatalf("expected stale recovered DEL to skip network deletion, got %d", deleteCount)
	}
}

func TestCheckIdentityMismatchReturnsFailedPrecondition(t *testing.T) {
	socketPath := uniqueSocketPath(t, "check-mismatch")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Add(ctx, sampleAddRequest("ep-mismatch", "inc-a", "10.251.1.16/24")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	badReq := sampleCheckRequest("ep-mismatch", "inc-a")
	badReq.Identity.NetworkId = "net-other"
	_, err := client.Check(ctx, badReq)
	code := status.Code(err)
	if code != codes.FailedPrecondition && code != codes.NotFound {
		t.Fatalf("expected FailedPrecondition or NotFound for mismatched identity context, got err=%v", err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	socketPath := uniqueSocketPath(t, "concurrent")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()
	client, closeFn := dialClient(t, socketPath)
	defer closeFn()

	const workers = 25
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep := fmt.Sprintf("ep-concurrent-%d", i)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := client.Add(ctx, sampleAddRequest(ep, "inc-a", fmt.Sprintf("10.251.2.%d/24", i+10))); err != nil {
				errCh <- fmt.Errorf("add %s: %w", ep, err)
				return
			}
			if _, err := client.Check(ctx, sampleCheckRequest(ep, "inc-a")); err != nil {
				errCh <- fmt.Errorf("check %s: %w", ep, err)
				return
			}
			if _, err := client.Del(ctx, sampleDelRequest(ep, "inc-a")); err != nil {
				errCh <- fmt.Errorf("del %s: %w", ep, err)
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
}

func TestSocketCleanupAndGracefulShutdown(t *testing.T) {
	socketPath := uniqueSocketPath(t, "cleanup")
	ctx, cancel := context.WithCancel(context.Background())
	server, err := StartUnixServer(ctx, socketPath, NewCNIServer(NewStore()))
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("socket does not exist after start: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path is not a unix socket: %s", socketPath)
	}

	cancel()
	if stopErr := server.Stop(context.Background()); stopErr != nil {
		t.Fatalf("stop failed: %v", stopErr)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket should be removed after stop, stat err=%v", err)
	}

	server2 := startTestServer(t, socketPath)
	if stopErr := server2.Stop(context.Background()); stopErr != nil {
		t.Fatalf("second stop failed: %v", stopErr)
	}
}

func TestInteropWithEVPNCNIClient(t *testing.T) {
	netns := isolatedInteropNetNS(t)
	socketPath := uniqueSocketPath(t, "interop")
	server := startTestServer(t, socketPath)
	defer func() { _ = server.Stop(context.Background()) }()

	conf := fmt.Sprintf(`{"cniVersion":"1.0.0","name":"evpn-net","type":"evpn-cni","nodeAgentSocket":"%s","networkID":"net-h22","logicalSwitch":"ls-h22","attachmentID":"net1","ipAddress":"10.251.3.11/24","macAddress":"0a:58:fb:03:00:11","timeoutSeconds":2}`, socketPath)
	args := "K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-interop;K8S_POD_UID=uid-interop"

	stdout, stderr, err := runCNIBinary(t, "ADD", conf, args, netns)
	if err != nil {
		t.Fatalf("evpn-cni ADD failed: %v, stdout=%s stderr=%s", err, stdout, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("unexpected ADD stderr: %q", stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("ADD stdout is not valid CNI JSON: %v, out=%q", err, stdout)
	}
	if out["cniVersion"] != "1.0.0" {
		t.Fatalf("unexpected cniVersion in ADD output: %#v", out["cniVersion"])
	}

	stdout, stderr, err = runCNIBinary(t, "CHECK", conf, args, netns)
	if err != nil {
		t.Fatalf("evpn-cni CHECK failed: %v, stderr=%s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) != "" {
		t.Fatalf("unexpected CHECK output: stdout=%q stderr=%q", stdout, stderr)
	}

	stdout, stderr, err = runCNIBinary(t, "DEL", conf, args, netns)
	if err != nil {
		t.Fatalf("evpn-cni DEL failed: %v, stderr=%s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) != "" {
		t.Fatalf("unexpected DEL output: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestInteropNetNSHelper(t *testing.T) {
	if os.Getenv("EVPN_INTEROP_NETNS_HELPER") != "1" {
		return
	}
	fmt.Println("netns-ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func isolatedInteropNetNS(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInteropNetNSHelper$")
	cmd.Env = append(os.Environ(), "EVPN_INTEROP_NETNS_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if os.Geteuid() != 0 {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		cancel()
		t.Fatalf("isolated CNI fixture requires CAP_SYS_ADMIN (root) or permitted unprivileged user/network namespaces: %v", err)
	}
	t.Cleanup(func() {
		input.Close()
		err := cmd.Wait()
		cancel()
		if err != nil {
			t.Errorf("network namespace helper cleanup: %v", err)
		}
	})
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "netns-ready\n" {
		t.Fatalf("network namespace helper readiness: line=%q error=%v", line, err)
	}
	path := fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid)
	held, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(held, current) {
		t.Fatal("CNI fixture must not share the invoking process's network namespace")
	}
	return path
}

func TestAddRollbackInvokesNetworkCleanupOnEndpointFailure(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeMgr.ensureErr = fmt.Errorf("endpoint ensure failed")
	fakeNet := &fakeNetworkBackend{created: true}
	server := NewCNIServerWithBackends(store, fakeMgr, fakeNet)

	_, err := server.Add(context.Background(), sampleAddRequest("ep-rollback", "inc-a", "10.251.9.11/24"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition from Add, got err=%v", err)
	}

	fakeNet.mu.Lock()
	defer fakeNet.mu.Unlock()
	if fakeNet.deleteCnt != 1 {
		t.Fatalf("expected network cleanup delete once on rollback, got %d", fakeNet.deleteCnt)
	}
}

func TestCheckInvokesNetworkBackend(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-test1",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.9.12/24",
		},
	}
	server := NewCNIServerWithBackends(store, fakeMgr, fakeNet)

	req := sampleAddRequest("ep-check-net", "inc-a", "10.251.9.12/24")
	if _, err := server.Add(context.Background(), req); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if _, err := server.Check(context.Background(), sampleCheckRequest("ep-check-net", "inc-a")); err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	fakeNet.mu.Lock()
	defer fakeNet.mu.Unlock()
	if fakeNet.checkCnt == 0 {
		t.Fatalf("expected network backend check to be invoked")
	}
}

func TestCheckInvokesAttachmentCheckerWhenConfigured(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-test1",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.9.12/24",
		},
	}
	checker := &fakeAttachmentChecker{}
	server := NewCNIServerWithBackendsAndChecker(store, fakeMgr, fakeNet, checker)

	req := sampleAddRequest("ep-check-attach", "inc-a", "10.251.9.12/24")
	if _, err := server.Add(context.Background(), req); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if _, err := server.Check(context.Background(), sampleCheckRequest("ep-check-attach", "inc-a")); err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if checker.checkCnt == 0 {
		t.Fatalf("expected attachment checker to be invoked")
	}
}

func TestCheckAttachmentFailureReturnsFailedPrecondition(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-test1",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.9.12/24",
		},
	}
	checker := &fakeAttachmentChecker{err: fmt.Errorf("ovs bridge membership mismatch")}
	server := NewCNIServerWithBackendsAndChecker(store, fakeMgr, fakeNet, checker)

	req := sampleAddRequest("ep-check-attach-fail", "inc-a", "10.251.9.12/24")
	if _, err := server.Add(context.Background(), req); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	_, err := server.Check(context.Background(), sampleCheckRequest("ep-check-attach-fail", "inc-a"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition from attachment check failure, got err=%v", err)
	}
}

func TestAddUsesHostInterfaceNameForEndpointAttachment(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-host42",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.9.13/24",
		},
	}
	server := NewCNIServerWithBackends(store, fakeMgr, fakeNet)

	req := sampleAddRequest("ep-host-if", "inc-a", "10.251.9.13/24")
	if _, err := server.Add(context.Background(), req); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	fakeMgr.mu.Lock()
	defer fakeMgr.mu.Unlock()
	spec, ok := fakeMgr.specs[endpoint.EndpointID("ep-host-if")]
	if !ok {
		t.Fatalf("expected endpoint spec to be persisted")
	}
	if spec.OVSInterfaceName != "evp-host42" {
		t.Fatalf("expected host interface propagation, got %q", spec.OVSInterfaceName)
	}
}

func TestRepeatedAddPreservesHostInterfaceIdentity(t *testing.T) {
	store := NewStore()
	fakeMgr := newFakeEndpointManager()
	fakeNet := &fakeNetworkBackend{
		state: networkState{
			HostIfName:      "evp-host77",
			ContainerIfName: "net1",
			MACAddress:      "0a:58:fb:03:00:11",
			StaticIPCIDR:    "10.251.9.14/24",
		},
	}
	server := NewCNIServerWithBackends(store, fakeMgr, fakeNet)

	req := sampleAddRequest("ep-host-replay", "inc-a", "10.251.9.14/24")
	first, err := server.Add(context.Background(), req)
	if err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	second, err := server.Add(context.Background(), req)
	if err != nil {
		t.Fatalf("second Add failed: %v", err)
	}
	if first.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("first add should not be replay")
	}
	if !second.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("second add should be replay")
	}

	fakeMgr.mu.Lock()
	defer fakeMgr.mu.Unlock()
	spec, ok := fakeMgr.specs[endpoint.EndpointID("ep-host-replay")]
	if !ok {
		t.Fatalf("expected endpoint spec to exist")
	}
	if spec.OVSInterfaceName != "evp-host77" {
		t.Fatalf("expected replay to keep host interface identity, got %q", spec.OVSInterfaceName)
	}
}

func TestInterfaceNameGenerationDeterministicAndScoped(t *testing.T) {
	keyA := "net-a|att-1|ep-1"
	keyB := "net-a|att-2|ep-1"
	hostA1 := hostVethNameForKey(keyA)
	hostA2 := hostVethNameForKey(keyA)
	hostB := hostVethNameForKey(keyB)
	if hostA1 != hostA2 {
		t.Fatalf("host name generation must be deterministic: %q vs %q", hostA1, hostA2)
	}
	if hostA1 == hostB {
		t.Fatalf("host name generation must be scoped by attachment/network context")
	}
	if len(hostA1) > ifNameMaxLen || len(peerVethNameForKey(keyA)) > ifNameMaxLen {
		t.Fatalf("generated interface names must be IFNAMSIZ-safe")
	}
}

func startTestServer(t *testing.T, socketPath string) *UnixServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server, err := StartUnixServer(ctx, socketPath, NewCNIServer(NewStore()))
	if err != nil {
		t.Fatalf("failed to start test server: %v", err)
	}
	return server
}

func dialClient(t *testing.T, socketPath string) (nodeagentpb.NodeAgentCNIClient, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(
		ctx,
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}
	return nodeagentpb.NewNodeAgentCNIClient(conn), func() { _ = conn.Close() }
}

func sampleAddRequest(endpointID, incarnation, ipCIDR string) *nodeagentpb.CNIAddRequest {
	return &nodeagentpb.CNIAddRequest{
		Runtime: &nodeagentpb.CNIRuntimeContext{
			CniVersion:  "1.0.0",
			ContainerId: "container-a",
			NetnsPath:   "/proc/100/ns/net",
			IfName:      "net1",
		},
		Identity: &nodeagentpb.EndpointIdentity{
			EndpointId:   endpointID,
			NetworkId:    "net-h22",
			AttachmentId: "net1",
			WorkloadUid:  "uid-a",
			Incarnation:  incarnation,
		},
		Attachment: &nodeagentpb.AttachmentRequest{
			LogicalSwitch:    "ls-h22",
			OvsInterfaceName: "net1",
			StaticIpCidr:     ipCIDR,
			StaticMac:        "0a:58:fb:03:00:11",
		},
		Meta: &nodeagentpb.RequestMeta{
			RequestId:      "req-a",
			TimeoutSeconds: 2,
		},
	}
}

func sampleCheckRequest(endpointID, incarnation string) *nodeagentpb.CNICheckRequest {
	return &nodeagentpb.CNICheckRequest{
		Runtime: &nodeagentpb.CNIRuntimeContext{
			CniVersion:  "1.0.0",
			ContainerId: "container-a",
			NetnsPath:   "/proc/100/ns/net",
			IfName:      "net1",
		},
		Identity: &nodeagentpb.EndpointIdentity{
			EndpointId:   endpointID,
			NetworkId:    "net-h22",
			AttachmentId: "net1",
			WorkloadUid:  "uid-a",
			Incarnation:  incarnation,
		},
	}
}

func sampleDelRequest(endpointID, incarnation string) *nodeagentpb.CNIDelRequest {
	return &nodeagentpb.CNIDelRequest{
		Runtime: &nodeagentpb.CNIRuntimeContext{
			CniVersion:  "1.0.0",
			ContainerId: "container-a",
			NetnsPath:   "/proc/100/ns/net",
			IfName:      "net1",
		},
		Identity: &nodeagentpb.EndpointIdentity{
			EndpointId:   endpointID,
			NetworkId:    "net-h22",
			AttachmentId: "net1",
			WorkloadUid:  "uid-a",
			Incarnation:  incarnation,
		},
	}
}

func uniqueSocketPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("/tmp", fmt.Sprintf("evpn-nodeagent-%s-%d.sock", name, time.Now().UnixNano()))
}

func runCNIBinary(t *testing.T, command, stdinConfig, cniArgs, netns string) (stdout string, stderr string, err error) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", "", fmt.Errorf("cannot locate CNI test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	cmd := exec.Command("go", "run", "./cmd/evpn-cni")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID=container-interop",
		"CNI_NETNS="+netns,
		"CNI_IFNAME=net1",
		"CNI_PATH=/opt/cni/bin",
		"CNI_ARGS="+cniArgs,
	)
	cmd.Stdin = strings.NewReader(stdinConfig)
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}
