package evpn

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

type model1TestProgrammer struct {
	ensured         []Model1Tunnel
	deleted         []Model1Tunnel
	networksEnsured []Model1Network
	networksDeleted []Model1Network
	bridgesDeleted  []Model1Network
	events          []string
	fail            bool
}

type model1TestRIBSource struct{}

func (*model1TestRIBSource) ReadRemoteRIB(context.Context) (RemoteRIBSnapshot, error) {
	return RemoteRIBSnapshot{Complete: true}, nil
}

func (p *model1TestProgrammer) EnsureModel1Network(_ context.Context, network Model1Network) error {
	p.networksEnsured = append(p.networksEnsured, network)
	return nil
}

func (p *model1TestProgrammer) DeleteModel1Network(_ context.Context, network Model1Network) error {
	p.networksDeleted = append(p.networksDeleted, network)
	return nil
}

func (p *model1TestProgrammer) DeleteModel1ProviderBridge(_ context.Context, network Model1Network) error {
	p.bridgesDeleted = append(p.bridgesDeleted, network)
	return nil
}

func (p *model1TestProgrammer) EnsureModel1Tunnel(_ context.Context, tunnel Model1Tunnel) error {
	if p.fail {
		return errors.New("injected ensure failure")
	}
	p.ensured = append(p.ensured, tunnel)
	p.events = append(p.events, "ensure:"+tunnel.Key.RemoteVTEP)
	return nil
}

func (p *model1TestProgrammer) DeleteModel1Tunnel(_ context.Context, tunnel Model1Tunnel) error {
	p.deleted = append(p.deleted, tunnel)
	p.events = append(p.events, "delete:"+tunnel.Key.RemoteVTEP)
	return nil
}

func model1TestConfig() Model1Config {
	return Model1Config{
		Networks: []Model1Network{{
			NetworkID: "remote-test", LogicalSwitch: "ls-remote", VNI: 100,
			PhysicalNetwork: "m1-physnet", ProviderBridge: "br-model1",
		}},
		UDPPort: 4789, ExclusiveWriter: true,
	}
}

func model1Snapshot(t *testing.T, macOnly bool, generation uint64) RemoteImportSnapshot {
	t.Helper()
	path := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		if macOnly {
			route.IPAddressLength = 0
			route.IPAddress = netip.Addr{}
		}
	})
	endpoint := model1Endpoint(t, path)
	return RemoteImportSnapshot{
		Authoritative: true, Generation: generation,
		Endpoints: []DesiredRemoteEndpoint{endpoint},
	}
}

func model1Endpoint(t *testing.T, path *api.Path) DesiredRemoteEndpoint {
	t.Helper()
	decoded, err := DecodeRemoteType2(path)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := importConfig().ValidateRoute(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return DesiredRemoteEndpoint{
		Key:       remoteHash([]string{validated.NetworkID, decoded.MAC, decoded.IP}),
		NetworkID: validated.NetworkID, VNI: validated.VNI,
		MAC: decoded.MAC, IP: decoded.IP, RemoteVTEP: decoded.NextHop,
		ValidationState: "validated", Routes: []ValidatedRemoteRoute{validated},
	}
}

func TestOVSDBProgrammedConfigRequiresExplicitUniqueOwnershipMapping(t *testing.T) {
	if err := model1TestConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*Model1Config)
	}{
		{"writer-not-exclusive", func(c *Model1Config) { c.ExclusiveWriter = false }},
		{"duplicate-vni", func(c *Model1Config) {
			c.Networks = append(c.Networks, Model1Network{NetworkID: "other", LogicalSwitch: "ls-other", VNI: 100})
		}},
		{"duplicate-switch", func(c *Model1Config) {
			c.Networks = append(c.Networks, Model1Network{NetworkID: "other", LogicalSwitch: "ls-remote", VNI: 101})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := model1TestConfig()
			test.edit(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("invalid OVSDB-Programmed config accepted")
			}
		})
	}
}

func TestOVSDBProgrammedSnapshotCreatesStaticTunnelForMACIPAndMACOnly(t *testing.T) {
	for _, test := range []struct {
		name    string
		macOnly bool
	}{
		{"mac-ip", false},
		{"mac-only", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			programmer := &model1TestProgrammer{}
			reader, err := NewReadOnlyType2Importer(&model1TestRIBSource{}, importConfig())
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewModel1Type2Handler(reader, programmer, model1TestConfig())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := model1Snapshot(t, test.macOnly, 1)
			if err := handler.ReconcileSnapshot(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			if len(programmer.ensured) != 1 {
				t.Fatalf("expected one static tunnel, got %d", len(programmer.ensured))
			}
			tunnel := programmer.ensured[0]
			if tunnel.Key.RemoteVTEP != "192.0.2.80" || tunnel.Key.VNI != 100 ||
				tunnel.Key.NetworkID != "remote-test" || tunnel.Key.LogicalSwitch != "ls-remote" {
				t.Fatalf("unexpected route-to-tunnel identity: %+v", tunnel.Key)
			}
			address := snapshot.Endpoints[0].MAC
			if snapshot.Endpoints[0].IP != "" {
				address += " " + snapshot.Endpoints[0].IP
			}
			if !reflect.DeepEqual(tunnel.Addresses, []string{address}) {
				t.Fatalf("unexpected LSP addresses: %v", tunnel.Addresses)
			}
			if len(tunnel.Name) > 15 || tunnel.UDPPort != 4789 || tunnel.Bridge != "br-model1" {
				t.Fatalf("invalid OVS tunnel properties: %+v", tunnel)
			}
		})
	}
}

func TestOVSDBProgrammedRejectsUncertainAndInvalidSnapshotsBeforeProgramming(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&model1TestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &model1TestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, model1TestConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := model1Snapshot(t, false, 1)
	snapshot.Authoritative = false
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("non-authoritative snapshot accepted")
	}
	snapshot = model1Snapshot(t, false, 1)
	snapshot.Endpoints[0].RemoteVTEP = "192.0.2.81"
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("endpoint whose route VTEP disagrees was accepted")
	}
	snapshot = model1Snapshot(t, false, 1)
	snapshot.Endpoints[0].VNI = 101
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("endpoint mapped to an unexpected VNI was accepted")
	}
	if len(programmer.ensured) != 0 || len(programmer.deleted) != 0 {
		t.Fatalf("invalid snapshots mutated the programmer: %+v", programmer)
	}
}

func TestOVSDBProgrammedAggregatesRoutesByActualRemoteVTEPAndWithdraws(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&model1TestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &model1TestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, model1TestConfig())
	if err != nil {
		t.Fatal(err)
	}
	initial := model1Snapshot(t, false, 1)
	secondPath := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.MacAddress = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
		route.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	second := model1Endpoint(t, secondPath)
	initial.Endpoints = append(initial.Endpoints, second)
	if err := handler.ReconcileSnapshot(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	if len(programmer.ensured) != 1 || len(programmer.ensured[0].Addresses) != 2 {
		t.Fatalf("routes sharing the actual VTEP did not share one tunnel: %+v", programmer.ensured)
	}
	withdrawn := RemoteImportSnapshot{Authoritative: true, Generation: 2}
	if err := handler.ReconcileSnapshot(context.Background(), withdrawn); err != nil {
		t.Fatal(err)
	}
	if len(programmer.deleted) != 1 || programmer.deleted[0].Key.RemoteVTEP != "192.0.2.80" {
		t.Fatalf("withdrawal did not delete the exact tunnel: %+v", programmer.deleted)
	}
	if len(programmer.networksDeleted) != 1 || programmer.networksDeleted[0] != model1TestConfig().Networks[0] {
		t.Fatalf("empty authoritative snapshot did not remove the provider network: %+v", programmer.networksDeleted)
	}
	if len(programmer.bridgesDeleted) != 1 || programmer.bridgesDeleted[0] != model1TestConfig().Networks[0] {
		t.Fatalf("empty authoritative snapshot did not remove the provider bridge: %+v", programmer.bridgesDeleted)
	}
}

func TestOVSDBProgrammedVTEPMobilityWithdrawsOldTunnelBeforeAddingNew(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&model1TestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &model1TestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, model1TestConfig())
	if err != nil {
		t.Fatal(err)
	}
	initial := model1Snapshot(t, false, 1)
	if err := handler.ReconcileSnapshot(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	moved := model1Snapshot(t, false, 2)
	moved.Endpoints[0].RemoteVTEP = "192.0.2.81"
	moved.Endpoints[0].Routes[0].Route.NextHop = "192.0.2.81"
	if err := handler.ReconcileSnapshot(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	want := []string{"ensure:192.0.2.80", "delete:192.0.2.80", "ensure:192.0.2.81"}
	if !reflect.DeepEqual(programmer.events, want) {
		t.Fatalf("route mobility was not withdrawal-first: got %v want %v", programmer.events, want)
	}
}

func TestOVSDBProgrammedOwnershipMetadataIsExactForIdentity(t *testing.T) {
	key := Model1TunnelKey{NetworkID: "remote-test", LogicalSwitch: "ls-remote", VNI: 100, RemoteVTEP: "192.0.2.80"}
	expected := model1ExternalIDs(key)
	if !model1IDsMatch(expected, expected) {
		t.Fatal("identical OVSDB-Programmed ownership metadata did not match")
	}
	conflict := model1ExternalIDs(key)
	conflict["evpn4ovn-network-id"] = "another-network"
	if model1IDsMatch(conflict, expected) {
		t.Fatal("conflicting network ownership metadata matched")
	}
}

func TestOVSDBProgrammedStaticOVSAndNBDBObjectSpecs(t *testing.T) {
	key := Model1TunnelKey{
		NetworkID: "remote-test", LogicalSwitch: "ls-remote", VNI: 100,
		RemoteVTEP: "192.0.2.80",
	}
	tunnel := Model1Tunnel{
		Key: key, Name: model1TunnelName(key), UDPPort: 4789, Bridge: "br-int",
		Addresses: []string{"aa:bb:cc:dd:ee:ff 10.251.1.11"},
	}
	options := model1VXLANOptions(tunnel)
	if options["remote_ip"] != key.RemoteVTEP || options["key"] != "100" || options["dst_port"] != "4789" {
		t.Fatalf("static VXLAN options do not use route VTEP/VNI/UDP port: %v", options)
	}
	iface := model1VXLANInterface(tunnel)
	if iface.Type != "vxlan" || iface.ExternalIDs["iface-id"] != "" {
		t.Fatalf("static VXLAN interface must not masquerade as an OVN VIF: %+v", iface)
	}
	port := model1VXLANPort(tunnel, iface.UUID)
	if port.Name != tunnel.Name || len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
		t.Fatalf("OVS port does not reference its dedicated VXLAN interface: %+v", port)
	}
	network := model1TestConfig().Networks[0]
	lsp := model1LocalnetLSP(network)
	if lsp.Type != "localnet" || lsp.Name != model1NetworkPortName(network) ||
		!reflect.DeepEqual(lsp.Addresses, []string{"unknown"}) ||
		!model1MapsEqual(lsp.Options, map[string]string{"network_name": network.PhysicalNetwork}) || lsp.Tag != nil ||
		lsp.ExternalIDs[model1ManagedByKey] != model1Owner {
		t.Fatalf("unexpected owned OVN localnet LSP specification: %+v", lsp)
	}
}

func TestOVSDBProgrammedRejectsMultipleRemoteVTEPsBeforeProgramming(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&model1TestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &model1TestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, model1TestConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := model1Snapshot(t, false, 1)
	secondPath := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.MacAddress = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
		route.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	second := model1Endpoint(t, secondPath)
	second.RemoteVTEP = "192.0.2.81"
	second.Routes[0].Route.NextHop = second.RemoteVTEP
	snapshot.Endpoints = append(snapshot.Endpoints, second)
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("snapshot with multiple VTEPs accepted by the single-VTEP OVSDB-Programmed integration")
	}
	if len(programmer.networksEnsured) != 0 || len(programmer.ensured) != 0 || len(programmer.deleted) != 0 {
		t.Fatalf("rejected multi-VTEP snapshot mutated OVSDB-Programmed state: %+v", programmer)
	}
}
