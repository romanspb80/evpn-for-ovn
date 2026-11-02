package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	ovsdbpb "github.com/romanspb80/evpn-for-ovn/gen/ovsdb_mgmt"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	frameworkNetlinkE2ENetworkID = "framework-netlink-e2e"
	frameworkOVSDBE2ENetworkID   = "framework-ovsdb-e2e"
)

func TestFrameworkNetlinkEndpointLifecycle_Live(t *testing.T) {
	if os.Getenv("FRAMEWORK_NETLINK_E2E") != "1" {
		t.Skip("requires the dedicated Framework Netlink E2E runtime")
	}
	runFrameworkEndpointLifecycleLive(t, frameworkNetlinkE2ENetworkID)
}

func TestFrameworkOVSDBEndpointLifecycle_Live(t *testing.T) {
	if os.Getenv("FRAMEWORK_OVSDB_E2E") != "1" {
		t.Skip("requires the dedicated Framework OVSDB E2E runtime")
	}
	runFrameworkEndpointLifecycleLive(t, frameworkOVSDBE2ENetworkID)
}

func runFrameworkEndpointLifecycleLive(t *testing.T, networkID string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("Framework E2E provisioner requires root")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	action := strings.TrimSpace(os.Getenv("FRAMEWORK_E2E_ACTION"))
	runID := strings.TrimSpace(os.Getenv("FRAMEWORK_E2E_RUN_ID"))
	logicalSwitch := strings.TrimSpace(os.Getenv("FRAMEWORK_E2E_LOGICAL_SWITCH"))
	serviceAddress := strings.TrimSpace(os.Getenv("FRAMEWORK_E2E_OVSDB_MGMT"))
	if runID == "" || logicalSwitch == "" || serviceAddress == "" {
		t.Fatal("FRAMEWORK_E2E_RUN_ID, FRAMEWORK_E2E_LOGICAL_SWITCH and FRAMEWORK_E2E_OVSDB_MGMT are required")
	}

	networkClient, networkConn, err := frameworkE2EOvsdbClient(ctx, serviceAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := networkConn.Close(); err != nil {
			t.Errorf("close OVSDB management connection: %v", err)
		}
	}()

	if action == "provision" || action == "network" || action == "endpoints" {
		status, err := networkClient.AddNetwork(ctx, &ovsdbpb.AddNetworkRequest{Name: logicalSwitch})
		if err != nil || status == nil || !status.Success {
			t.Fatalf("Framework AddNetwork %q: status=%+v err=%v", logicalSwitch, status, err)
		}
		t.Logf("Framework network provisioned: %s (%s)", logicalSwitch, status.Message)
	}
	if action == "network" {
		return
	}
	if action == "delete-network" {
		status, err := networkClient.DelNetwork(ctx, &ovsdbpb.DelNetworkRequest{Name: logicalSwitch})
		if err != nil || status == nil || !status.Success {
			t.Fatalf("Framework DelNetwork %q: status=%+v err=%v", logicalSwitch, status, err)
		}
		t.Logf("Framework network deleted: %s", status.Message)
		return
	}

	server, closeServer, err := BuildServerForOptions(ctx, NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth",
		OVNNBEndpoint:  "unix:/var/run/ovn/ovnnb_db.sock",
		OVSDBEndpoint:  "unix:/var/run/openvswitch/db.sock",
	}, nil)
	if err != nil {
		t.Fatalf("build Framework endpoint managers: %v", err)
	}
	defer closeServer()

	endpoints := []frameworkE2EEndpoint{
		frameworkE2EEndpointFor(runID, networkID, logicalSwitch, "a", "0a:58:f0:01:00:0a", "10.244.240.10"),
		frameworkE2EEndpointFor(runID, networkID, logicalSwitch, "b", "0a:58:f0:01:00:0b", "10.244.240.11"),
	}

	switch action {
	case "provision", "endpoints":
		created := make([]frameworkE2EEndpoint, 0, len(endpoints))
		for _, item := range endpoints {
			if err := frameworkE2EEnsureEndpoint(ctx, server, item); err != nil {
				for i := len(created) - 1; i >= 0; i-- {
					if cleanupErr := frameworkE2EDeleteEndpoint(ctx, server, created[i]); cleanupErr != nil {
						t.Errorf("rollback endpoint %s: %v", created[i].identity.EndpointId, cleanupErr)
					}
				}
				t.Fatalf("provision Framework endpoint %s: %v", item.identity.EndpointId, err)
			}
			created = append(created, item)
			t.Logf("Framework endpoint provisioned id=%s ip=%s mac=%s lsp=%s host_if=%s", item.spec.ID, item.spec.IPAddresses[0], item.spec.MACAddress, item.spec.LogicalSwitchPort, item.spec.OVSInterfaceName)
		}
		frameworkE2EWriteReceipt(t, runID, logicalSwitch, endpoints)
	case "delete-a":
		if err := frameworkE2EDeleteEndpoint(ctx, server, endpoints[0]); err != nil {
			t.Fatalf("delete Framework endpoint A: %v", err)
		}
		t.Logf("Framework endpoint deleted id=%s ip=%s", endpoints[0].spec.ID, endpoints[0].spec.IPAddresses[0])
	case "cleanup":
		for i := len(endpoints) - 1; i >= 0; i-- {
			if err := frameworkE2EDeleteEndpoint(ctx, server, endpoints[i]); err != nil {
				t.Fatalf("delete Framework endpoint %s: %v", endpoints[i].spec.ID, err)
			}
		}
		frameworkE2EWriteReceipt(t, runID, logicalSwitch, nil)
	default:
		t.Fatalf("unsupported FRAMEWORK_E2E_ACTION %q", action)
	}
}

type frameworkE2EEndpoint struct {
	spec       endpoint.EndpointSpec
	identity   *nodeagentpb.EndpointIdentity
	runtime    *nodeagentpb.CNIRuntimeContext
	attachment *nodeagentpb.AttachmentRequest
}

func frameworkE2EEndpointFor(runID, networkID, logicalSwitch, suffix, mac, ip string) frameworkE2EEndpoint {
	id := "framework-" + runID + "-" + suffix
	uid := "framework-e2e-" + runID + "-" + suffix
	incarnation := "framework-e2e-" + runID
	netnsName := "fw-" + runID + "-" + suffix
	spec := endpoint.EndpointSpec{
		ID:                endpoint.EndpointID(id),
		NetworkID:         networkID,
		AttachmentID:      "attachment-" + id,
		Incarnation:       incarnation,
		Workload:          endpoint.WorkloadRef{Kind: "framework-e2e", UID: uid, Name: id, Namespace: "framework-e2e"},
		LogicalSwitch:     logicalSwitch,
		LogicalSwitchPort: "lsp-" + id,
		MACAddress:        mac,
		IPAddresses:       []string{ip},
	}
	identity := &nodeagentpb.EndpointIdentity{
		EndpointId: id, NetworkId: networkID, AttachmentId: spec.AttachmentID,
		WorkloadUid: uid, Incarnation: incarnation,
	}
	return frameworkE2EEndpoint{
		spec:     spec,
		identity: identity,
		runtime: &nodeagentpb.CNIRuntimeContext{
			NetnsPath: filepath.Join("/run/netns", netnsName),
			IfName:    "eth1",
		},
		attachment: &nodeagentpb.AttachmentRequest{StaticMac: mac, StaticIpCidr: ip + "/24"},
	}
}

func frameworkE2EEnsureEndpoint(ctx context.Context, server *CNIServer, item frameworkE2EEndpoint) error {
	if err := item.spec.Validate(); err != nil {
		return err
	}
	if err := frameworkE2EEnsureNamespace(item.runtime.GetNetnsPath()); err != nil {
		return err
	}
	key := endpointKey(item.identity)
	rec := &endpointRecord{
		identity: item.identity, runtime: item.runtime, attachment: item.attachment, endpointSpec: item.spec,
	}
	networkState, _, err := server.network.Ensure(ctx, key, rec)
	if err != nil {
		_ = frameworkE2EDeleteNamespace(item.runtime.GetNetnsPath())
		return fmt.Errorf("ensure identity-owned Linux veth: %w", err)
	}
	item.spec.OVSInterfaceName = networkState.HostIfName
	rec.endpointSpec = item.spec
	if _, err := server.endpoint.EnsureEndpoint(ctx, item.spec); err != nil {
		if deleteErr := server.network.Delete(ctx, key, rec); deleteErr != nil {
			return fmt.Errorf("ensure endpoint: %v; rollback Linux veth: %w", err, deleteErr)
		}
		_ = frameworkE2EDeleteNamespace(item.runtime.GetNetnsPath())
		return fmt.Errorf("ensure endpoint: %w", err)
	}
	return nil
}

func frameworkE2EDeleteEndpoint(ctx context.Context, server *CNIServer, item frameworkE2EEndpoint) error {
	key := endpointKey(item.identity)
	item.spec.OVSInterfaceName = hostVethNameForKey(key)
	rec := &endpointRecord{
		identity: item.identity, runtime: item.runtime, attachment: item.attachment, endpointSpec: item.spec,
	}
	if err := server.endpoint.DeleteEndpoint(ctx, item.spec); err != nil {
		return fmt.Errorf("delete identity-owned OVN endpoint: %w", err)
	}
	if err := server.network.Delete(ctx, key, rec); err != nil {
		return fmt.Errorf("delete identity-owned Linux veth: %w", err)
	}
	if err := frameworkE2EDeleteNamespace(item.runtime.GetNetnsPath()); err != nil {
		return fmt.Errorf("delete test workload network namespace: %w", err)
	}
	return nil
}

func frameworkE2EEnsureNamespace(path string) (retErr error) {
	name := filepath.Base(path)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing pre-existing network namespace %q", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	runtime.LockOSThread()
	origNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("get original network namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore original network namespace: %w", err))
		}
		if err := origNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close original network namespace handle: %w", err))
		}
		runtime.UnlockOSThread()
	}()
	ns, err := netns.NewNamed(name)
	if err != nil {
		return fmt.Errorf("create test network namespace %q: %w", name, err)
	}
	if err := ns.Close(); err != nil {
		return err
	}
	if err := withNetNSPath(path, func() error {
		link, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	}); err != nil {
		_ = netns.DeleteNamed(name)
		return fmt.Errorf("enable workload loopback: %w", err)
	}
	return nil
}

func frameworkE2EDeleteNamespace(path string) error {
	name := filepath.Base(path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return netns.DeleteNamed(name)
}

func frameworkE2EWriteReceipt(t *testing.T, runID, logicalSwitch string, endpoints []frameworkE2EEndpoint) {
	t.Helper()
	type receiptEndpoint struct {
		ID                string `json:"id"`
		NetworkID         string `json:"networkId"`
		AttachmentID      string `json:"attachmentId"`
		WorkloadUID       string `json:"workloadUid"`
		Incarnation       string `json:"incarnation"`
		LogicalSwitch     string `json:"logicalSwitch"`
		LogicalSwitchPort string `json:"logicalSwitchPort"`
		InterfaceName     string `json:"interfaceName"`
		NetnsPath         string `json:"netnsPath"`
		MAC               string `json:"mac"`
		IP                string `json:"ip"`
	}
	result := struct {
		RunID         string            `json:"runId"`
		LogicalSwitch string            `json:"logicalSwitch"`
		Endpoints     []receiptEndpoint `json:"endpoints"`
	}{RunID: runID, LogicalSwitch: logicalSwitch}
	for _, item := range endpoints {
		key := endpointKey(item.identity)
		result.Endpoints = append(result.Endpoints, receiptEndpoint{
			ID: string(item.spec.ID), NetworkID: item.spec.NetworkID, AttachmentID: item.spec.AttachmentID,
			WorkloadUID: item.spec.Workload.UID, Incarnation: item.spec.Incarnation,
			LogicalSwitch: item.spec.LogicalSwitch, LogicalSwitchPort: item.spec.LogicalSwitchPort,
			InterfaceName: hostVethNameForKey(key), NetnsPath: item.runtime.GetNetnsPath(),
			MAC: item.spec.MACAddress, IP: item.spec.IPAddresses[0],
		})
	}
	compactData, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FRAMEWORK_E2E_RECEIPT=%s", compactData)
}

func frameworkE2EOvsdbClient(ctx context.Context, address string) (ovsdbpb.OvsdbServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	if _, err := ovsdbpb.NewOvsdbServiceClient(conn).GetNetworks(ctx, &ovsdbpb.Empty{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("check OVSDB management API %q: %w", address, err)
	}
	return ovsdbpb.NewOvsdbServiceClient(conn), conn, nil
}
