package evpn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/client"
	dbase "github.com/ovn-kubernetes/libovsdb/database"
	"github.com/ovn-kubernetes/libovsdb/database/inmemory"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/ovn-kubernetes/libovsdb/server"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

type model1TestDB struct {
	client   client.Client
	database dbase.Database
	name     string
}

func startModel1TestDB(t *testing.T, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema) *model1TestDB {
	t.Helper()
	logger := logr.Discard()
	databaseModel, problems := model.NewDatabaseModel(schema, dbModel)
	if len(problems) != 0 {
		t.Fatalf("build %s test database model: %v", dbModel.Name(), problems)
	}
	database := inmemory.NewDatabase(map[string]model.ClientDBModel{dbModel.Name(): dbModel}, &logger)
	ovsServer, err := server.NewOvsdbServer(database, &logger, databaseModel)
	if err != nil {
		t.Fatalf("create in-memory %s server: %v", dbModel.Name(), err)
	}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("m1-%s.sock", uuid.NewString()[:8]))
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove stale test socket %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = os.Remove(socket) })
	go func() {
		if err := ovsServer.Serve("unix", socket); err != nil {
			t.Errorf("serve in-memory %s database: %v", dbModel.Name(), err)
		}
	}()
	t.Cleanup(ovsServer.Close)
	deadline := time.Now().Add(2 * time.Second)
	for !ovsServer.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ovsServer.Ready() {
		t.Fatalf("in-memory %s database did not become ready", dbModel.Name())
	}
	ovsClient, err := client.NewOVSDBClient(dbModel, client.WithEndpoint("unix:"+socket))
	if err != nil {
		t.Fatalf("create %s test client: %v", dbModel.Name(), err)
	}
	if err := ovsClient.Connect(context.Background()); err != nil {
		t.Fatalf("connect %s test client: %v", dbModel.Name(), err)
	}
	if _, err := ovsClient.MonitorAll(context.Background()); err != nil {
		ovsClient.Close()
		t.Fatalf("monitor %s test database: %v", dbModel.Name(), err)
	}
	t.Cleanup(ovsClient.Close)
	return &model1TestDB{client: ovsClient, database: database, name: dbModel.Name()}
}

func model1TestDatabases(t *testing.T) (*model1TestDB, *model1TestDB) {
	t.Helper()
	nbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		t.Fatal(err)
	}
	ovsModel, err := vswitch.FullDatabaseModel()
	if err != nil {
		t.Fatal(err)
	}
	nb := startModel1TestDB(t, nbModel, nbdb.Schema())
	ovs := startModel1TestDB(t, ovsModel, vswitch.Schema())
	return nb, ovs
}

func seedModel1Row(t *testing.T, ctx context.Context, db client.Client, row model.Model) {
	t.Helper()
	ops, err := db.Create(row)
	if err != nil {
		t.Fatalf("build seed transaction: %v", err)
	}
	results, err := db.Transact(ctx, ops...)
	if err != nil {
		t.Fatalf("seed test database row: %v", err)
	}
	for index, result := range results {
		if result.Error != "" {
			t.Fatalf("seed operation %d failed: %+v", index, result)
		}
	}
}

func seedModel1Topology(t *testing.T, ctx context.Context, nb, ovs client.Client) {
	t.Helper()
	seedModel1Row(t, ctx, nb, &nbdb.LogicalSwitch{
		UUID: uuid.NewString(), Name: "ls-model1",
		ExternalIDs: map[string]string{"unrelated": "preserve-nb"},
	})
	seedModel1Root(t, ctx, ovs)
}

func seedModel1Root(t *testing.T, ctx context.Context, ovs client.Client) {
	t.Helper()
	root := &vswitch.OpenvSwitch{
		UUID: uuid.NewString(),
		ExternalIDs: map[string]string{
			"unrelated":             "preserve-openvswitch",
			model1BridgeMappingsKey: "other-physnet:br-other",
		},
	}
	seedModel1Row(t, ctx, ovs, root)
}

func waitForModel1Row(t *testing.T, name string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := check()
		if err == nil && ok {
			return
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not become visible: %v", name, lastErr)
}

func model1DatabaseTunnel() Model1Tunnel {
	key := Model1TunnelKey{
		NetworkID: "test-network", LogicalSwitch: "ls-model1",
		VNI: 1000, RemoteVTEP: "192.0.2.90",
	}
	return Model1Tunnel{
		Key: key, Name: model1TunnelName(key),
		Addresses: []string{"02:00:00:00:00:01 198.51.100.10"},
		UDPPort:   4789, Bridge: "br-model1",
	}
}

func model1DatabaseNetwork() Model1Network {
	return Model1Network{
		NetworkID: "test-network", LogicalSwitch: "ls-model1",
		PhysicalNetwork: "m1-physnet", ProviderBridge: "br-model1", VNI: 1000,
	}
}

func updateModel1BridgeMappings(t *testing.T, ctx context.Context, ovs client.Client, mappings string) {
	t.Helper()
	var roots []vswitch.OpenvSwitch
	if err := ovs.List(ctx, &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("expected one Open_vSwitch root, got %d", len(roots))
	}
	root := roots[0]
	root.ExternalIDs = copyStringMap(root.ExternalIDs)
	root.ExternalIDs[model1BridgeMappingsKey] = mappings
	ops, err := ovs.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Update(&root, &root.ExternalIDs)
	if err != nil {
		t.Fatal(err)
	}
	if err := transactModel1(ctx, ovs, ops...); err != nil {
		t.Fatal(err)
	}
}

func TestOVSDBProgrammedLibovsdbTransactionsAreOwnedIdempotentAndWithdrawalSafe(t *testing.T) {
	ctx := context.Background()
	nbDB, ovsDB := model1TestDatabases(t)
	nb, ovs := nbDB.client, ovsDB.client
	seedModel1Topology(t, ctx, nb, ovs)
	programmer, err := NewLibovsdbModel1Programmer(nb, ovs)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := model1DatabaseTunnel()
	tunnel.Addresses = append(tunnel.Addresses, "02:00:00:00:00:02 198.51.100.11")
	network := model1DatabaseNetwork()
	if err := programmer.EnsureModel1Network(ctx, network); err != nil {
		t.Fatal(err)
	}
	if err := programmer.EnsureModel1Network(ctx, network); err != nil {
		t.Fatalf("repeat network ensure: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := programmer.EnsureModel1Tunnel(ctx, tunnel); err != nil {
			t.Fatalf("ensure pass %d: %v", i+1, err)
		}
	}
	var logicalSwitches []nbdb.LogicalSwitch
	var logicalPorts []nbdb.LogicalSwitchPort
	if err := nb.List(ctx, &logicalSwitches); err != nil {
		t.Fatal(err)
	}
	if err := nb.List(ctx, &logicalPorts); err != nil {
		t.Fatal(err)
	}
	if len(logicalSwitches) != 1 || logicalSwitches[0].ExternalIDs["unrelated"] != "preserve-nb" {
		t.Fatalf("unrelated NBDB state changed: %+v", logicalSwitches)
	}
	if len(logicalPorts) != 1 || logicalPorts[0].Name != model1NetworkPortName(network) ||
		logicalPorts[0].Type != "localnet" || !sameModel1Addresses(logicalPorts[0].Addresses, []string{"unknown"}) ||
		!model1MapsEqual(logicalPorts[0].Options, model1LocalnetOptions(network)) ||
		logicalPorts[0].Tag != nil ||
		!model1IDsMatch(logicalPorts[0].ExternalIDs, model1NetworkExternalIDs(network)) ||
		!model1Contains(logicalSwitches[0].Ports, logicalPorts[0].UUID) {
		t.Fatalf("unexpected owned NBDB localnet LSP state: switches=%+v ports=%+v", logicalSwitches, logicalPorts)
	}

	var bridges []vswitch.Bridge
	var interfaces []vswitch.Interface
	var ports []vswitch.Port
	if err := ovs.List(ctx, &bridges); err != nil {
		t.Fatal(err)
	}
	if err := ovs.List(ctx, &interfaces); err != nil {
		t.Fatal(err)
	}
	if err := ovs.List(ctx, &ports); err != nil {
		t.Fatal(err)
	}
	if len(bridges) != 1 || !model1IDsMatch(bridges[0].ExternalIDs, model1ProviderBridgeExternalIDs(network)) {
		t.Fatalf("provider bridge was not created with owned identity: %+v", bridges)
	}
	var roots []vswitch.OpenvSwitch
	if err := ovs.List(ctx, &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || !model1Contains(roots[0].Bridges, bridges[0].UUID) {
		t.Fatalf("provider bridge is not referenced by Open_vSwitch: roots=%+v bridges=%+v", roots, bridges)
	}
	if len(interfaces) != 2 {
		t.Fatalf("expected provider internal and static VXLAN interfaces, got %+v", interfaces)
	}
	var vxlanInterface *vswitch.Interface
	for i := range interfaces {
		if interfaces[i].Name == tunnel.Name {
			vxlanInterface = &interfaces[i]
		}
	}
	if vxlanInterface == nil || vxlanInterface.Type != "vxlan" ||
		!model1MapsEqual(vxlanInterface.Options, model1VXLANOptions(tunnel)) ||
		vxlanInterface.ExternalIDs["iface-id"] != "" {
		t.Fatalf("unexpected static VXLAN Interface: %+v", interfaces)
	}
	if len(ports) != 2 {
		t.Fatalf("expected provider internal and static VXLAN ports, got %+v", ports)
	}
	var vxlanPort *vswitch.Port
	for i := range ports {
		if ports[i].Name == tunnel.Name {
			vxlanPort = &ports[i]
		}
	}
	if vxlanPort == nil || len(vxlanPort.Interfaces) != 1 ||
		vxlanPort.Interfaces[0] != vxlanInterface.UUID || !model1Contains(bridges[0].Ports, vxlanPort.UUID) {
		t.Fatalf("unexpected OVS VXLAN Port/bridge membership: ports=%+v bridges=%+v", ports, bridges)
	}
	if err := programmer.DeleteModel1Network(ctx, network); err == nil {
		t.Fatal("provider network was deleted while its static VTEP tunnel remained")
	}

	changed := tunnel
	changed.Addresses = []string{"02:00:00:00:00:01 198.51.100.10"}
	if err := programmer.DeleteModel1Tunnel(ctx, changed); err == nil {
		t.Fatal("withdrawal deleted a tunnel while the stored aggregate still had another route")
	}
	if err := programmer.EnsureModel1Tunnel(ctx, changed); err != nil {
		t.Fatalf("update shared tunnel address set: %v", err)
	}
	if err := programmer.DeleteModel1Tunnel(ctx, changed); err != nil {
		t.Fatalf("withdraw owned tunnel: %v", err)
	}
	if err := programmer.DeleteModel1Tunnel(ctx, changed); err != nil {
		t.Fatalf("repeat withdrawal should be idempotent: %v", err)
	}
	if err := programmer.DeleteModel1Network(ctx, network); err != nil {
		t.Fatalf("withdraw provider network: %v", err)
	}
	if err := programmer.DeleteModel1Network(ctx, network); err != nil {
		t.Fatalf("repeat provider-network withdrawal should be idempotent: %v", err)
	}
	if err := programmer.DeleteModel1ProviderBridge(ctx, network); err != nil {
		t.Fatalf("withdraw owned provider bridge: %v", err)
	}
	if err := programmer.DeleteModel1ProviderBridge(ctx, network); err != nil {
		t.Fatalf("repeat provider-bridge withdrawal should be idempotent: %v", err)
	}
	waitForModel1Row(t, "withdrawn NBDB and OVSDB rows", func() (bool, error) {
		nbRows, err := nbDB.database.List(nbDB.name, "Logical_Switch_Port")
		if err != nil {
			return false, err
		}
		bridgeRows, err := ovsDB.database.List(ovsDB.name, "Bridge")
		if err != nil {
			return false, err
		}
		interfaceRows, err := ovsDB.database.List(ovsDB.name, "Interface")
		if err != nil {
			return false, err
		}
		portRows, err := ovsDB.database.List(ovsDB.name, "Port")
		if err != nil {
			return false, err
		}
		if len(nbRows) == 0 && len(bridgeRows) == 0 && len(interfaceRows) == 0 && len(portRows) == 0 {
			return true, nil
		}
		return false, fmt.Errorf("NB LSPs=%+v OVS bridges=%+v interfaces=%+v ports=%+v",
			nbRows, bridgeRows, interfaceRows, portRows)
	})
	interfaceRows, err := ovsDB.database.List(ovsDB.name, "Interface")
	if err != nil {
		t.Fatal(err)
	}
	portRows, err := ovsDB.database.List(ovsDB.name, "Port")
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaceRows) != 0 || len(portRows) != 0 {
		t.Fatalf("OVSDB-Programmed OVS state remains after withdrawal: interfaces=%+v ports=%+v", interfaceRows, portRows)
	}
	bridgeRows, err := ovsDB.database.List(ovsDB.name, "Bridge")
	if err != nil {
		t.Fatal(err)
	}
	if len(bridgeRows) != 0 {
		t.Fatalf("owned provider bridge was not removed: %+v", bridgeRows)
	}
	rootRows, err := ovsDB.database.List(ovsDB.name, "Open_vSwitch")
	if err != nil {
		t.Fatal(err)
	}
	if len(rootRows) != 1 {
		t.Fatalf("expected one Open_vSwitch root after cleanup, got %+v", rootRows)
	}
	var cleanRoot *vswitch.OpenvSwitch
	for _, row := range rootRows {
		cleanRoot = row.(*vswitch.OpenvSwitch)
	}
	if cleanRoot.ExternalIDs["unrelated"] != "preserve-openvswitch" ||
		cleanRoot.ExternalIDs[model1BridgeMappingsKey] != "other-physnet:br-other" ||
		len(cleanRoot.Bridges) != 0 {
		t.Fatalf("provider-network cleanup changed unrelated bridge mappings: %+v", cleanRoot)
	}
}

func TestOVSDBProgrammedProviderMappingRejectsConflictsAndBorrowsMatchingMappings(t *testing.T) {
	ctx := context.Background()
	t.Run("conflicting-bridge", func(t *testing.T) {
		nbDB, ovsDB := model1TestDatabases(t)
		seedModel1Topology(t, ctx, nbDB.client, ovsDB.client)
		updateModel1BridgeMappings(t, ctx, ovsDB.client, "m1-physnet:br-other,other-physnet:br-other")
		programmer, err := NewLibovsdbModel1Programmer(nbDB.client, ovsDB.client)
		if err != nil {
			t.Fatal(err)
		}
		if err := programmer.EnsureModel1Network(ctx, model1DatabaseNetwork()); err == nil {
			t.Fatal("conflicting physical-network bridge mapping was accepted")
		}
		var ports []nbdb.LogicalSwitchPort
		if err := nbDB.client.List(ctx, &ports); err != nil {
			t.Fatal(err)
		}
		if len(ports) != 0 {
			t.Fatalf("conflicting mapping created an NBDB localnet port: %+v", ports)
		}
	})

	t.Run("provider-bridge-already-mapped", func(t *testing.T) {
		nbDB, ovsDB := model1TestDatabases(t)
		seedModel1Topology(t, ctx, nbDB.client, ovsDB.client)
		updateModel1BridgeMappings(t, ctx, ovsDB.client, "other-physnet:br-model1")
		programmer, err := NewLibovsdbModel1Programmer(nbDB.client, ovsDB.client)
		if err != nil {
			t.Fatal(err)
		}
		if err := programmer.EnsureModel1Network(ctx, model1DatabaseNetwork()); err == nil {
			t.Fatal("provider bridge shared by another physical network was accepted")
		}
	})

	t.Run("matching-mapping-is-borrowed", func(t *testing.T) {
		nbDB, ovsDB := model1TestDatabases(t)
		seedModel1Topology(t, ctx, nbDB.client, ovsDB.client)
		updateModel1BridgeMappings(t, ctx, ovsDB.client, "m1-physnet:br-model1,other-physnet:br-other")
		programmer, err := NewLibovsdbModel1Programmer(nbDB.client, ovsDB.client)
		if err != nil {
			t.Fatal(err)
		}
		network := model1DatabaseNetwork()
		if err := programmer.EnsureModel1Network(ctx, network); err != nil {
			t.Fatal(err)
		}
		if err := programmer.DeleteModel1Network(ctx, network); err != nil {
			t.Fatal(err)
		}
		var roots []vswitch.OpenvSwitch
		if err := ovsDB.client.List(ctx, &roots); err != nil {
			t.Fatal(err)
		}
		if len(roots) != 1 || roots[0].ExternalIDs[model1BridgeMappingsKey] !=
			"m1-physnet:br-model1,other-physnet:br-other" {
			t.Fatalf("cleanup removed or changed a borrowed mapping: %+v", roots)
		}
		if _, owned := roots[0].ExternalIDs[model1BridgeMappingOwnerKey(network.PhysicalNetwork)]; owned {
			t.Fatalf("cleanup left OVSDB-Programmed ownership metadata for a borrowed mapping: %+v", roots[0].ExternalIDs)
		}
		if err := programmer.DeleteModel1ProviderBridge(ctx, network); err == nil {
			t.Fatal("provider bridge was deleted while a borrowed physnet mapping still referenced it")
		}
	})
}

func TestOVSDBProgrammedProviderNetworkRequiresStandaloneBridgeWithoutController(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		bridge *vswitch.Bridge
	}{
		{
			name: "unowned-existing-bridge",
			bridge: func() *vswitch.Bridge {
				failMode := vswitch.BridgeFailModeStandalone
				return &vswitch.Bridge{
					UUID: uuid.NewString(), Name: "br-model1", FailMode: &failMode,
					ExternalIDs: map[string]string{"owner": "someone-else"},
				}
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbDB, ovsDB := model1TestDatabases(t)
			seedModel1Row(t, ctx, nbDB.client, &nbdb.LogicalSwitch{
				UUID: uuid.NewString(), Name: "ls-model1",
			})
			root := &vswitch.OpenvSwitch{
				UUID: uuid.NewString(),
				ExternalIDs: map[string]string{
					model1BridgeMappingsKey: "other-physnet:br-other",
				},
			}
			seedModel1Row(t, ctx, ovsDB.client, root)
			if tt.bridge != nil {
				ops, err := ovsDB.client.Create(tt.bridge)
				if err != nil {
					t.Fatal(err)
				}
				mutate, err := ovsDB.client.Where(&vswitch.OpenvSwitch{UUID: root.UUID}).Mutate(root, model.Mutation{
					Field: &root.Bridges, Mutator: ovsdb.MutateOperationInsert, Value: []string{tt.bridge.UUID},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := transactModel1(ctx, ovsDB.client, append(ops, mutate...)...); err != nil {
					t.Fatal(err)
				}
			}
			programmer, err := NewLibovsdbModel1Programmer(nbDB.client, ovsDB.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := programmer.EnsureModel1Network(ctx, model1DatabaseNetwork()); err == nil {
				t.Fatal("invalid provider bridge configuration was accepted")
			}
			var ports []nbdb.LogicalSwitchPort
			if err := nbDB.client.List(ctx, &ports); err != nil {
				t.Fatal(err)
			}
			if len(ports) != 0 {
				t.Fatalf("invalid bridge configuration created a localnet LSP: %+v", ports)
			}
			var roots []vswitch.OpenvSwitch
			if err := ovsDB.client.List(ctx, &roots); err != nil {
				t.Fatal(err)
			}
			if len(roots) != 1 || roots[0].ExternalIDs[model1BridgeMappingsKey] != "other-physnet:br-other" {
				t.Fatalf("invalid bridge configuration changed bridge mappings: %+v", roots)
			}
		})
	}
}

func TestOVSDBProgrammedProviderBridgeCleanupProtectsForeignPorts(t *testing.T) {
	ctx := context.Background()
	nbDB, ovsDB := model1TestDatabases(t)
	seedModel1Topology(t, ctx, nbDB.client, ovsDB.client)
	programmer, err := NewLibovsdbModel1Programmer(nbDB.client, ovsDB.client)
	if err != nil {
		t.Fatal(err)
	}
	network := model1DatabaseNetwork()
	if err := programmer.EnsureModel1Network(ctx, network); err != nil {
		t.Fatal(err)
	}
	var bridges []vswitch.Bridge
	if err := ovsDB.client.List(ctx, &bridges); err != nil {
		t.Fatal(err)
	}
	var providerBridge *vswitch.Bridge
	for i := range bridges {
		if bridges[i].Name == network.ProviderBridge {
			providerBridge = &bridges[i]
		}
	}
	if providerBridge == nil {
		t.Fatal("provider bridge was not provisioned")
	}
	foreignInterface := &vswitch.Interface{UUID: uuid.NewString(), Name: "foreign0", Type: "internal"}
	foreignPort := &vswitch.Port{
		UUID: uuid.NewString(), Name: "foreign0", Interfaces: []string{foreignInterface.UUID},
	}
	createInterface, err := ovsDB.client.Create(foreignInterface)
	if err != nil {
		t.Fatal(err)
	}
	createPort, err := ovsDB.client.Create(foreignPort)
	if err != nil {
		t.Fatal(err)
	}
	attach, err := ovsDB.client.Where(&vswitch.Bridge{UUID: providerBridge.UUID}).Mutate(providerBridge, model.Mutation{
		Field: &providerBridge.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{foreignPort.UUID},
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := append(createInterface, createPort...)
	ops = append(ops, attach...)
	if err := transactModel1(ctx, ovsDB.client, ops...); err != nil {
		t.Fatal(err)
	}
	if err := programmer.DeleteModel1Network(ctx, network); err != nil {
		t.Fatal(err)
	}
	if err := programmer.DeleteModel1ProviderBridge(ctx, network); err == nil {
		t.Fatal("provider bridge was deleted with an unowned port still attached")
	}
	var remainingPorts []vswitch.Port
	if err := ovsDB.client.List(ctx, &remainingPorts); err != nil {
		t.Fatal(err)
	}
	foundForeignPort := false
	for i := range remainingPorts {
		foundForeignPort = foundForeignPort || remainingPorts[i].UUID == foreignPort.UUID
	}
	if !foundForeignPort {
		t.Fatal("failed bridge cleanup removed a foreign port")
	}
}

func TestOVSDBProgrammedPartialApplyReportsRetryRecovery(t *testing.T) {
	ctx := context.Background()
	nbDB, ovsDB := model1TestDatabases(t)
	nb, ovs := nbDB.client, ovsDB.client
	seedModel1Root(t, ctx, ovs)
	programmer, err := NewLibovsdbModel1Programmer(nb, ovs)
	if err != nil {
		t.Fatal(err)
	}
	network := model1DatabaseNetwork()
	err = programmer.EnsureModel1Network(ctx, network)
	var partial *Model1PartialOperationError
	if !errors.As(err, &partial) || partial.Operation != "ensure provider network" ||
		partial.Completed != "OVSDB" || partial.Failed != "NBDB" ||
		partial.Recovery != "retry EnsureModel1Network with the same network specification" {
		t.Fatalf("expected explicit recoverable cross-database failure, got %T %v", err, err)
	}
	seedModel1Row(t, ctx, nb, &nbdb.LogicalSwitch{UUID: uuid.NewString(), Name: "ls-model1"})
	if err := programmer.EnsureModel1Network(ctx, network); err != nil {
		t.Fatalf("retry did not complete prior partial network ensure: %v", err)
	}
	var logicalPorts []nbdb.LogicalSwitchPort
	if err := nb.List(ctx, &logicalPorts); err != nil {
		t.Fatal(err)
	}
	if len(logicalPorts) != 1 || logicalPorts[0].Name != model1NetworkPortName(network) ||
		logicalPorts[0].Type != "localnet" {
		t.Fatalf("retry did not preserve a single localnet LSP: %+v", logicalPorts)
	}
	tunnel := model1DatabaseTunnel()
	if err := programmer.EnsureModel1Tunnel(ctx, tunnel); err != nil {
		t.Fatalf("ensure static tunnel: %v", err)
	}
	var interfaces []vswitch.Interface
	if err := ovs.List(ctx, &interfaces); err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 2 {
		t.Fatalf("retry did not retain exactly one provider and one VXLAN Interface: %+v", interfaces)
	}
	foundTunnel := false
	for i := range interfaces {
		if interfaces[i].Name == tunnel.Name && interfaces[i].Type == "vxlan" {
			foundTunnel = true
		}
	}
	if !foundTunnel {
		t.Fatalf("retry did not create the static VXLAN Interface: %+v", interfaces)
	}
}

func TestOVSDBProgrammedDatabaseStepFailureIsExplicitAndPreservesCause(t *testing.T) {
	want := errors.New("injected second database failure")
	var completed []string
	err := runModel1DatabaseSteps("withdraw", "NBDB", "OVSDB", "retry delete",
		func() error { completed = append(completed, "NBDB"); return nil },
		func() error { completed = append(completed, "OVSDB"); return want })
	var partial *Model1PartialOperationError
	if !errors.As(err, &partial) || !errors.Is(err, want) ||
		partial.Operation != "withdraw" || partial.Completed != "NBDB" || partial.Failed != "OVSDB" {
		t.Fatalf("unexpected partial-operation classification: %v", err)
	}
	if fmt.Sprint(completed) != "[NBDB OVSDB]" {
		t.Fatalf("database steps ran in unexpected order: %v", completed)
	}
}
