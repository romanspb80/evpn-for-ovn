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

type ovsdbTestProgrammer struct {
	ensured         []Model1Tunnel
	deleted         []Model1Tunnel
	networksEnsured []Model1Network
	networksDeleted []Model1Network
	bridgesDeleted  []Model1Network
	events          []string
	fail            bool
}

type ovsdbTestRIBSource struct{}

func (*ovsdbTestRIBSource) ReadRemoteRIB(context.Context) (RemoteRIBSnapshot, error) {
	return RemoteRIBSnapshot{Complete: true}, nil
}

func (p *ovsdbTestProgrammer) EnsureModel1Network(_ context.Context, network Model1Network) error {
	p.networksEnsured = append(p.networksEnsured, network)
	return nil
}

func (p *ovsdbTestProgrammer) DeleteModel1Network(_ context.Context, network Model1Network) error {
	p.networksDeleted = append(p.networksDeleted, network)
	return nil
}

func (p *ovsdbTestProgrammer) DeleteModel1ProviderBridge(_ context.Context, network Model1Network) error {
	p.bridgesDeleted = append(p.bridgesDeleted, network)
	return nil
}

func (p *ovsdbTestProgrammer) EnsureModel1Tunnel(_ context.Context, tunnel Model1Tunnel) error {
	if p.fail {
		return errors.New("injected ensure failure")
	}
	p.ensured = append(p.ensured, tunnel)
	p.events = append(p.events, "ensure:"+tunnel.Key.RemoteVTEP)
	return nil
}

func (p *ovsdbTestProgrammer) DeleteModel1Tunnel(_ context.Context, tunnel Model1Tunnel) error {
	p.deleted = append(p.deleted, tunnel)
	p.events = append(p.events, "delete:"+tunnel.Key.RemoteVTEP)
	return nil
}

func ovsdbTestConfig() Model1Config {
	return Model1Config{
		Networks: []Model1Network{{
			NetworkID: "remote-test", LogicalSwitch: "ls-remote", VNI: 100,
			PhysicalNetwork: "ovsdb-physnet", ProviderBridge: "br-ovsdb",
		}},
		UDPPort: 4789, ExclusiveWriter: true,
	}
}

func ovsdbSnapshot(t *testing.T, macOnly bool, generation uint64) RemoteImportSnapshot {
	t.Helper()
	path := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		if macOnly {
			route.IPAddressLength = 0
			route.IPAddress = netip.Addr{}
		}
	})
	endpoint := ovsdbEndpoint(t, path)
	return RemoteImportSnapshot{
		Authoritative: true, Generation: generation,
		Endpoints: []DesiredRemoteEndpoint{endpoint},
	}
}

func ovsdbEndpoint(t *testing.T, path *api.Path) DesiredRemoteEndpoint {
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
	if err := ovsdbTestConfig().Validate(); err != nil {
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
			config := ovsdbTestConfig()
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
			programmer := &ovsdbTestProgrammer{}
			reader, err := NewReadOnlyType2Importer(&ovsdbTestRIBSource{}, importConfig())
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewModel1Type2Handler(reader, programmer, ovsdbTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := ovsdbSnapshot(t, test.macOnly, 1)
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
			if len(tunnel.Name) > 15 || tunnel.UDPPort != 4789 || tunnel.Bridge != "br-ovsdb" {
				t.Fatalf("invalid OVS tunnel properties: %+v", tunnel)
			}
		})
	}
}

func TestOVSDBProgrammedRejectsUncertainAndInvalidSnapshotsBeforeProgramming(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&ovsdbTestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &ovsdbTestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, ovsdbTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ovsdbSnapshot(t, false, 1)
	snapshot.Authoritative = false
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("non-authoritative snapshot accepted")
	}
	snapshot = ovsdbSnapshot(t, false, 1)
	snapshot.Endpoints[0].RemoteVTEP = "192.0.2.81"
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("endpoint whose route VTEP disagrees was accepted")
	}
	snapshot = ovsdbSnapshot(t, false, 1)
	snapshot.Endpoints[0].VNI = 101
	if err := handler.ReconcileSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("endpoint mapped to an unexpected VNI was accepted")
	}
	if len(programmer.ensured) != 0 || len(programmer.deleted) != 0 {
		t.Fatalf("invalid snapshots mutated the programmer: %+v", programmer)
	}
}

func TestOVSDBProgrammedAggregatesRoutesByActualRemoteVTEPAndWithdraws(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&ovsdbTestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &ovsdbTestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, ovsdbTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	initial := ovsdbSnapshot(t, false, 1)
	secondPath := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.MacAddress = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
		route.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	second := ovsdbEndpoint(t, secondPath)
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
	if len(programmer.networksDeleted) != 1 || programmer.networksDeleted[0] != ovsdbTestConfig().Networks[0] {
		t.Fatalf("empty authoritative snapshot did not remove the provider network: %+v", programmer.networksDeleted)
	}
	if len(programmer.bridgesDeleted) != 1 || programmer.bridgesDeleted[0] != ovsdbTestConfig().Networks[0] {
		t.Fatalf("empty authoritative snapshot did not remove the provider bridge: %+v", programmer.bridgesDeleted)
	}
}

func TestOVSDBProgrammedVTEPMobilityWithdrawsOldTunnelBeforeAddingNew(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&ovsdbTestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &ovsdbTestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, ovsdbTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	initial := ovsdbSnapshot(t, false, 1)
	if err := handler.ReconcileSnapshot(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	moved := ovsdbSnapshot(t, false, 2)
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
	expected := ovsdbExternalIDs(key)
	if !ovsdbIDsMatch(expected, expected) {
		t.Fatal("identical OVSDB-Programmed ownership metadata did not match")
	}
	conflict := ovsdbExternalIDs(key)
	conflict["evpn4ovn-network-id"] = "another-network"
	if ovsdbIDsMatch(conflict, expected) {
		t.Fatal("conflicting network ownership metadata matched")
	}
}

func TestOVSDBProgrammedStaticOVSAndNBDBObjectSpecs(t *testing.T) {
	key := Model1TunnelKey{
		NetworkID: "remote-test", LogicalSwitch: "ls-remote", VNI: 100,
		RemoteVTEP: "192.0.2.80",
	}
	tunnel := Model1Tunnel{
		Key: key, Name: ovsdbTunnelName(key), UDPPort: 4789, Bridge: "br-int",
		Addresses: []string{"aa:bb:cc:dd:ee:ff 10.251.1.11"},
	}
	options := ovsdbVXLANOptions(tunnel)
	if options["remote_ip"] != key.RemoteVTEP || options["key"] != "100" || options["dst_port"] != "4789" {
		t.Fatalf("static VXLAN options do not use route VTEP/VNI/UDP port: %v", options)
	}
	iface := ovsdbVXLANInterface(tunnel)
	if iface.Type != "vxlan" || iface.ExternalIDs["iface-id"] != "" {
		t.Fatalf("static VXLAN interface must not masquerade as an OVN VIF: %+v", iface)
	}
	port := ovsdbVXLANPort(tunnel, iface.UUID)
	if port.Name != tunnel.Name || len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
		t.Fatalf("OVS port does not reference its dedicated VXLAN interface: %+v", port)
	}
	network := ovsdbTestConfig().Networks[0]
	lsp := ovsdbLocalnetLSP(network)
	if lsp.Type != "localnet" || lsp.Name != ovsdbNetworkPortName(network) ||
		!reflect.DeepEqual(lsp.Addresses, []string{"unknown"}) ||
		!ovsdbMapsEqual(lsp.Options, map[string]string{"network_name": network.PhysicalNetwork}) || lsp.Tag != nil ||
		lsp.ExternalIDs[ovsdbManagedByKey] != ovsdbOwner {
		t.Fatalf("unexpected owned OVN localnet LSP specification: %+v", lsp)
	}
}

func TestOVSDBProgrammedRejectsMultipleRemoteVTEPsBeforeProgramming(t *testing.T) {
	reader, err := NewReadOnlyType2Importer(&ovsdbTestRIBSource{}, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	programmer := &ovsdbTestProgrammer{}
	handler, err := NewModel1Type2Handler(reader, programmer, ovsdbTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ovsdbSnapshot(t, false, 1)
	secondPath := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.MacAddress = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
		route.IPAddress = netip.MustParseAddr("10.251.1.12")
	})
	second := ovsdbEndpoint(t, secondPath)
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
