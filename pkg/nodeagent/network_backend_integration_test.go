package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestLinuxVethBackendLifecycle_Privileged(t *testing.T) {
	if os.Getenv("EVPN_NODEAGENT_RUN_PRIV_TESTS") != "1" {
		t.Skip("set EVPN_NODEAGENT_RUN_PRIV_TESTS=1 to run privileged linux netns/veth tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root privileges")
	}

	netnsPath := privilegedNetNS(t)
	backend := NewLinuxVethBackend()
	key := "net-it|att-it|ep-it"
	rec := privilegedEndpoint(netnsPath)

	state, _, err := backend.Ensure(context.Background(), key, rec)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Delete(context.Background(), key, rec); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if state.HostIfName == "" || state.ContainerIfName == "" {
		t.Fatalf("Ensure returned empty interface state: %+v", state)
	}
	if _, err := backend.Check(context.Background(), key, rec); err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if err := backend.Delete(context.Background(), key, rec); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := netlink.LinkByName(state.HostIfName); err == nil {
		t.Fatalf("host interface %q still exists after delete", state.HostIfName)
	}
}

func privilegedNetNS(t *testing.T) string {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	nsName := fmt.Sprintf("evpn-it-%d-%s", os.Getpid(), shortHash(t.Name(), 8))
	nsHandle, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("failed to create netns %q: %v", nsName, err)
	}
	_ = nsHandle.Close()
	if err := netns.Set(original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := netns.DeleteNamed(nsName); err != nil {
			t.Errorf("delete test namespace: %v", err)
		}
	})
	return filepath.Join("/var/run/netns", nsName)
}

func privilegedEndpoint(netnsPath string) *endpointRecord {
	return &endpointRecord{
		identity: &nodeagentpb.EndpointIdentity{
			EndpointId:   "ep-it",
			NetworkId:    "net-it",
			AttachmentId: "att-it",
			WorkloadUid:  "uid-it",
			Incarnation:  "inc-it",
		},
		runtime: &nodeagentpb.CNIRuntimeContext{
			CniVersion:  "1.0.0",
			ContainerId: "container-it",
			NetnsPath:   netnsPath,
			IfName:      "eth1",
		},
		attachment: &nodeagentpb.AttachmentRequest{
			StaticMac:    "0a:58:fb:99:00:11",
			StaticIpCidr: "10.251.99.11/24",
		},
	}

}

func TestLinuxVethBackendRestart_Privileged(t *testing.T) {
	if os.Getenv("EVPN_NODEAGENT_RUN_PRIV_TESTS") != "1" || os.Geteuid() != 0 {
		t.Skip("requires root and EVPN_NODEAGENT_RUN_PRIV_TESTS=1")
	}
	path := privilegedNetNS(t)
	rec := privilegedEndpoint(path)
	ctx := context.Background()
	key := "net-it|att-it|ep-restart"
	initial := NewLinuxVethBackend()
	state, _, err := initial.Ensure(ctx, key, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := initial.Delete(ctx, key, rec); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	host, err := netlink.LinkByName(state.HostIfName)
	if err != nil {
		t.Fatal(err)
	}
	ifindex := host.Attrs().Index
	restarted := NewLinuxVethBackend()
	if _, err := restarted.Check(ctx, key, rec); err != nil {
		t.Fatalf("CHECK after state loss: %v", err)
	}
	if _, created, err := restarted.Ensure(ctx, key, rec); err != nil || created {
		t.Fatalf("adoption: created=%t err=%v", created, err)
	}
	host, err = netlink.LinkByName(state.HostIfName)
	if err != nil || host.Attrs().Index != ifindex {
		t.Fatalf("adoption replaced original veth: %v", err)
	}
	if err := withNetNSPath(path, func() error {
		peer, err := netlink.LinkByName(state.ContainerIfName)
		if err != nil {
			return err
		}
		return netlink.LinkSetAlias(peer, "foreign-owner")
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewLinuxVethBackend().Ensure(ctx, key, rec); err == nil {
		t.Fatal("adopted peer without verified ownership")
	}
	if _, err := netlink.LinkByName(state.HostIfName); err != nil {
		t.Fatalf("rejected adoption removed existing veth: %v", err)
	}
}
