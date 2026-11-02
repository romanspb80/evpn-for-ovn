//go:build linux

package attachment

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("required binary %s not available", name)
	}
}

func runCmd(t *testing.T, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %s %s: %v\n%s", command, strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("condition not met: %v", last)
	}
	t.Fatal("condition not met before timeout")
}

func newTestOVSClient(ctx context.Context, endpoint string) (client.Client, error) {
	ovsDBModel, err := model.NewClientDBModel("Open_vSwitch",
		map[string]model.Model{
			"Bridge":    &vswitch.Bridge{},
			"Port":      &vswitch.Port{},
			"Interface": &vswitch.Interface{},
		})
	if err != nil {
		return nil, err
	}
	c, err := client.NewOVSDBClient(
		ovsDBModel,
		client.WithEndpoint(endpoint),
		client.WithReconnect(2*time.Second, backoff.NewConstantBackOff(100*time.Millisecond)),
	)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	if _, err := c.MonitorAll(ctx); err != nil {
		c.Close()
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var ifaces []vswitch.Interface
		if err := c.List(waitCtx, &ifaces); err == nil {
			break
		} else if waitCtx.Err() != nil {
			c.Close()
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return c, nil
}

func startTestOVSDB(t *testing.T) string {
	t.Helper()
	requireBinary(t, "ovsdb-server")
	requireBinary(t, "ovsdb-tool")
	requireBinary(t, "ovs-vsctl")

	schema := "/usr/share/openvswitch/vswitch.ovsschema"
	if _, err := os.Stat(schema); err != nil {
		t.Skipf("required schema %s not available", schema)
	}

	dir, err := os.MkdirTemp("/tmp", "ovst-")
	if err != nil {
		t.Fatalf("create temp dir failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dbPath := filepath.Join(dir, "conf.db")
	pidPath := filepath.Join(dir, "ovsdb-server.pid")
	logPath := filepath.Join(dir, "ovsdb-server.log")
	sockPath := filepath.Join(dir, "db.sock")
	ctlPath := filepath.Join(dir, "ovsdb-server.ctl")

	runCmd(t, "ovsdb-tool", "create", dbPath, schema)
	runCmd(t, "ovsdb-server",
		"--detach",
		"--no-chdir",
		"--pidfile="+pidPath,
		"--log-file="+logPath,
		"--unixctl="+ctlPath,
		"--remote=punix:"+sockPath,
		dbPath,
	)
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidPath); err == nil {
			pid := strings.TrimSpace(string(data))
			if pid != "" {
				_ = exec.Command("kill", pid).Run()
			}
		}
	})
	waitForCondition(t, 5*time.Second, func() error {
		if _, err := os.Stat(sockPath); err != nil {
			return err
		}
		return nil
	})
	runCmd(t, "ovs-vsctl", "--db=unix:"+sockPath, "--no-wait", "init")
	runCmd(t, "ovs-vsctl", "--db=unix:"+sockPath, "--no-wait", "--may-exist", "add-br", "br-int")
	return "unix:" + sockPath
}

func TestLibovsdbOVSStoreAtomicCreateRealOVSDB(t *testing.T) {
	endpointPath := startTestOVSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := newTestOVSClient(ctx, endpointPath)
	if err != nil {
		t.Fatalf("new test ovs client failed: %v", err)
	}
	defer client.Close()

	store := NewLibovsdbOVSStore(client)
	mgr := NewOVSAttachmentManager(store)
	identity := Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}
	spec := endpoint.AttachmentSpec{
		EndpointID:        endpoint.EndpointID(identity.EndpointID),
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
		MACAddress:        "aa:bb:cc:dd:ee:ff",
	}

	if _, err := mgr.EnsureAttachmentWithIdentity(ctx, identity, spec); err != nil {
		t.Fatalf("ensure attachment failed: %v", err)
	}
	if _, err := mgr.EnsureAttachmentWithIdentity(ctx, identity, spec); err != nil {
		t.Fatalf("second ensure attachment failed: %v", err)
	}

	ifaceName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Interface", "name=evp-abc")
	if strings.TrimSpace(ifaceName) != "evp-abc" {
		t.Fatalf("expected interface evp-abc, got %q", ifaceName)
	}
	portName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Port", "name=evp-abc")
	if strings.TrimSpace(portName) != "evp-abc" {
		t.Fatalf("expected port evp-abc, got %q", portName)
	}
	ports := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "list-ports", "br-int")
	if !strings.Contains(ports, "evp-abc") {
		t.Fatalf("expected br-int to contain evp-abc, got %q", ports)
	}
}

func TestLibovsdbOVSStoreAtomicCreateRollbackRealOVSDB(t *testing.T) {
	endpointPath := startTestOVSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := newTestOVSClient(ctx, endpointPath)
	if err != nil {
		t.Fatalf("new test ovs client failed: %v", err)
	}
	defer client.Close()

	store := NewLibovsdbOVSStore(client)
	iface := &vswitch.Interface{
		Name:        "evp-rollback",
		Type:        "system",
		ExternalIDs: ExternalIDsForIdentity(Identity{EndpointID: "ep-1", NetworkID: "net-a", AttachmentID: "att-1", WorkloadUID: "wl-1", Incarnation: "1"}, "lsp-a", "aa:bb:cc:dd:ee:ff"),
	}
	port := &vswitch.Port{
		Name:       "evp-rollback",
		Interfaces: []string{iface.UUID},
	}
	err = store.CreateInterfacePortAndAttachBridge(ctx, "br-missing", iface, port)
	if err == nil {
		t.Fatalf("expected missing bridge transaction to fail")
	}
	if !strings.Contains(err.Error(), "bridge br-missing not found") {
		t.Fatalf("expected missing bridge error, got %v", err)
	}
	ifaceName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Interface", "name=evp-rollback")
	if strings.TrimSpace(ifaceName) != "" {
		t.Fatalf("expected no interface after failed atomic transaction, got %q", ifaceName)
	}
	portName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Port", "name=evp-rollback")
	if strings.TrimSpace(portName) != "" {
		t.Fatalf("expected no port after failed atomic transaction, got %q", portName)
	}
}

func TestLibovsdbOVSStoreCreateOpsUseNamedUUIDs(t *testing.T) {
	endpointPath := startTestOVSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := newTestOVSClient(ctx, endpointPath)
	if err != nil {
		t.Fatalf("new test ovs client failed: %v", err)
	}
	defer client.Close()

	store := NewLibovsdbOVSStore(client)
	iface := &vswitch.Interface{
		Name:        "evp-payload",
		Type:        "system",
		ExternalIDs: map[string]string{ExternalIDIfaceID: "lsp-payload"},
	}
	port := &vswitch.Port{Name: "evp-payload"}
	bridgeUUID, err := store.bridgeUUIDByName(ctx, "br-int")
	if err != nil {
		t.Fatalf("bridge lookup failed: %v", err)
	}
	ops, err := store.buildCreateInterfacePortAndAttachBridgeOps(iface, port, bridgeUUID)
	if err != nil {
		t.Fatalf("build ops failed: %v", err)
	}
	opsJSON, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops failed: %v", err)
	}
	got := string(opsJSON)
	if !strings.Contains(got, "\"uuid-name\":\"iface_") || !strings.Contains(got, "\"uuid-name\":\"port_") {
		t.Fatalf("expected named uuid payload in ops, got %s", got)
	}
	if !strings.Contains(got, "\"interfaces\":[\"named-uuid\",\"iface_") {
		t.Fatalf("expected port.interfaces to reference named uuid, got %s", got)
	}
}

func TestLibovsdbOVSStoreDetachPortFromBridgeOwnedAndStale(t *testing.T) {
	endpointPath := startTestOVSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := newTestOVSClient(ctx, endpointPath)
	if err != nil {
		t.Fatalf("new test ovs client failed: %v", err)
	}
	defer client.Close()

	store := NewLibovsdbOVSStore(client)
	identity := Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "2",
	}
	iface := &vswitch.Interface{
		Name:        "evp-detach",
		Type:        "system",
		ExternalIDs: ExternalIDsForIdentity(identity, "lsp-detach", "aa:bb:cc:dd:ee:ff"),
	}
	port := &vswitch.Port{Name: "evp-detach"}
	if err := store.CreateInterfacePortAndAttachBridge(ctx, "br-int", iface, port); err != nil {
		t.Fatalf("create attachment rows failed: %v", err)
	}
	portUUID := strings.TrimSpace(runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=_uuid", "find", "Port", "name=evp-detach"))
	ifaceUUID := strings.TrimSpace(runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=_uuid", "find", "Interface", "name=evp-detach"))
	if portUUID == "" || ifaceUUID == "" {
		t.Fatalf("failed to discover created port/interface UUIDs")
	}

	staleIDs := identity
	staleIDs.Incarnation = "1"
	detached, err := store.DetachPortFromBridgeIfInterfaceOwned(ctx, "br-int", portUUID, ifaceUUID, ExternalIDsForIdentity(staleIDs, "lsp-detach", "aa:bb:cc:dd:ee:ff"))
	if err != nil {
		t.Fatalf("stale detach returned unexpected error: %v", err)
	}
	if detached {
		t.Fatalf("expected stale detach to be rejected")
	}
	portsBefore := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "list-ports", "br-int")
	if !strings.Contains(portsBefore, "evp-detach") {
		t.Fatalf("expected stale detach to leave bridge membership untouched, got %q", portsBefore)
	}

	detached, err = store.DetachPortFromBridgeIfInterfaceOwned(ctx, "br-int", portUUID, ifaceUUID, ExternalIDsForIdentity(identity, "lsp-detach", "aa:bb:cc:dd:ee:ff"))
	if err != nil {
		t.Fatalf("owned detach failed: %v", err)
	}
	if !detached {
		t.Fatalf("expected owned detach to succeed")
	}
	portsAfter := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "list-ports", "br-int")
	if strings.Contains(portsAfter, "evp-detach") {
		t.Fatalf("expected port removed from bridge, got %q", portsAfter)
	}
	portName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Port", "name=evp-detach")
	if strings.TrimSpace(portName) != "" {
		t.Fatalf("expected port row to be garbage-collected, got %q", portName)
	}
	ifaceName := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "--data=bare", "--no-heading", "--columns=name", "find", "Interface", "name=evp-detach")
	if strings.TrimSpace(ifaceName) != "" {
		t.Fatalf("expected interface row to be garbage-collected, got %q", ifaceName)
	}
}

func TestOVSAttachmentManagerDeleteRealOVSDBIdempotentAndStaleSafe(t *testing.T) {
	endpointPath := startTestOVSDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := newTestOVSClient(ctx, endpointPath)
	if err != nil {
		t.Fatalf("new test ovs client failed: %v", err)
	}
	defer client.Close()

	store := NewLibovsdbOVSStore(client)
	mgr := NewOVSAttachmentManager(store)
	identityOld := Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}
	spec := endpoint.AttachmentSpec{
		EndpointID:        endpoint.EndpointID(identityOld.EndpointID),
		LogicalSwitchPort: "lsp-del",
		OVSInterfaceName:  "evp-del",
		MACAddress:        "aa:bb:cc:dd:ee:11",
	}
	if _, err := mgr.EnsureAttachmentWithIdentity(ctx, identityOld, spec); err != nil {
		t.Fatalf("initial ensure failed: %v", err)
	}
	identityNew := identityOld
	identityNew.Incarnation = "2"
	if _, err := mgr.EnsureAttachmentWithIdentity(ctx, identityNew, spec); err != nil {
		t.Fatalf("replacement ensure failed: %v", err)
	}

	outcome, err := mgr.DeleteAttachmentWithIdentity(ctx, identityOld, spec.LogicalSwitchPort)
	if err == nil || outcome != DeleteOutcomeStale {
		t.Fatalf("expected stale delete rejection, outcome=%s err=%v", outcome, err)
	}
	portsStillPresent := runCmd(t, "ovs-vsctl", "--db="+endpointPath, "list-ports", "br-int")
	if !strings.Contains(portsStillPresent, "evp-del") {
		t.Fatalf("expected stale delete to preserve current attachment, got %q", portsStillPresent)
	}

	outcome, err = mgr.DeleteAttachmentWithIdentity(ctx, identityNew, spec.LogicalSwitchPort)
	if err != nil {
		t.Fatalf("delete current attachment failed: %v", err)
	}
	if outcome != DeleteOutcomeDeleted {
		t.Fatalf("unexpected delete outcome: %s", outcome)
	}
	outcome, err = mgr.DeleteAttachmentWithIdentity(ctx, identityNew, spec.LogicalSwitchPort)
	if err != nil {
		t.Fatalf("repeated delete failed: %v", err)
	}
	if outcome != DeleteOutcomeMissing {
		t.Fatalf("expected repeated delete to be idempotent missing, got %s", outcome)
	}
}
