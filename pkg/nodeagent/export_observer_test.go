package nodeagent

import (
	"context"
	"errors"
	"testing"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

type exportMemorySource struct {
	data exportSnapshot
	err  error
}

func (s *exportMemorySource) Snapshot(context.Context) (exportSnapshot, error) { return s.data, s.err }

func readyExportObserver(t *testing.T) (*CNIEndpointObserver, *exportMemorySource) {
	t.Helper()
	req := sampleAddRequest("net-h22|net1|uid-a", "container-a", "10.1.2.3/24")
	server := NewCNIServer(nil)
	if _, err := server.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rec, _ := server.store.get(endpointKey(req.Identity))
	spec := rec.endpointSpec
	ids := attachment.ExternalIDsForIdentity(attachment.Identity{EndpointID: string(spec.ID), NetworkID: spec.NetworkID, AttachmentID: spec.AttachmentID, WorkloadUID: spec.Workload.UID, Incarnation: spec.Incarnation}, spec.LogicalSwitchPort, spec.MACAddress)
	ofport, up, chassis := 1, true, "c-uuid"
	source := &exportMemorySource{data: exportSnapshot{
		Switches:   []*nbdb.LogicalSwitch{{UUID: "ls-uuid", Name: spec.LogicalSwitch, Ports: []string{"lsp-uuid"}}},
		LSPs:       []*nbdb.LogicalSwitchPort{{UUID: "lsp-uuid", Name: spec.LogicalSwitchPort, ExternalIDs: ids, Addresses: []string{spec.MACAddress + " " + spec.IPAddresses[0]}}},
		Interfaces: []*vswitch.Interface{{UUID: "i-uuid", Name: spec.OVSInterfaceName, ExternalIDs: ids, Ofport: &ofport}},
		Ports:      []*vswitch.Port{{UUID: "p-uuid", Name: spec.OVSInterfaceName, Interfaces: []string{"i-uuid"}}},
		Bridges:    []*vswitch.Bridge{{Name: "br-int", Ports: []string{"p-uuid"}}},
		Bindings:   []*exportPortBinding{{LogicalPort: spec.LogicalSwitchPort, Chassis: &chassis, Up: &up}},
		Chassis:    []*exportChassis{{UUID: chassis, Name: "local-chassis"}},
	}}
	cfg := evpn.ExporterConfig{NodeName: "node-a", LocalChassis: "local-chassis", Networks: []evpn.EVPNNetworkConfig{{NetworkID: spec.NetworkID, LogicalSwitch: spec.LogicalSwitch, Enabled: true, VNI: 100, RouteDistinguisher: "64512:100", RouteTargets: []string{"64512:100"}, LocalVTEP: "192.0.2.1"}}}
	return &CNIEndpointObserver{server: server, source: source, config: cfg}, source
}

func TestCNIExportReadiness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CNIEndpointObserver, *exportMemorySource)
	}{
		{"ready", func(*CNIEndpointObserver, *exportMemorySource) {}},
		{"wrong-owner", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.LSPs[0].ExternalIDs = map[string]string{} }},
		{"wrong-incarnation", func(_ *CNIEndpointObserver, s *exportMemorySource) {
			ids := map[string]string{}
			for k, v := range s.data.Interfaces[0].ExternalIDs {
				ids[k] = v
			}
			ids[attachment.ExternalIDIncarnation] = "stale"
			s.data.Interfaces[0].ExternalIDs = ids
		}},
		{"wrong-switch", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Switches[0].Name = "other" }},
		{"no-switch-membership", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Switches[0].Ports = nil }},
		{"wrong-mac", func(_ *CNIEndpointObserver, s *exportMemorySource) {
			s.data.LSPs[0].Addresses = []string{"02:00:00:00:00:99 10.1.2.3"}
		}},
		{"no-ovs", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Interfaces = nil }},
		{"invalid-ofport", func(_ *CNIEndpointObserver, s *exportMemorySource) { v := -1; s.data.Interfaces[0].Ofport = &v }},
		{"ovs-error", func(_ *CNIEndpointObserver, s *exportMemorySource) { v := "failed"; s.data.Interfaces[0].Error = &v }},
		{"wrong-iface-id", func(_ *CNIEndpointObserver, s *exportMemorySource) {
			s.data.Interfaces[0].ExternalIDs = map[string]string{}
		}},
		{"wrong-port", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Ports[0].Interfaces = []string{"other"} }},
		{"wrong-bridge", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Bridges[0].Name = "other" }},
		{"pb-absent", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Bindings = nil }},
		{"pb-down", func(_ *CNIEndpointObserver, s *exportMemorySource) { v := false; s.data.Bindings[0].Up = &v }},
		{"wrong-chassis", func(_ *CNIEndpointObserver, s *exportMemorySource) { s.data.Chassis[0].Name = "remote" }},
		{"not-allowlisted", func(o *CNIEndpointObserver, _ *exportMemorySource) { o.config.Networks = nil }},
		{"disabled-network", func(o *CNIEndpointObserver, _ *exportMemorySource) { o.config.Networks[0].Enabled = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, s := readyExportObserver(t)
			tc.mutate(o, s)
			desired, err := o.DesiredLocalEndpoints(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.name == "ready" {
				want = 1
			}
			if len(desired) != want {
				t.Fatalf("gate allowed %s: %+v", tc.name, desired)
			}
			if want == 1 && (desired[0].IPAddresses[0] != "10.1.2.3" || desired[0].Incarnation != "container-a") {
				t.Fatal("wrong endpoint mapping")
			}
		})
	}
}

func TestCNIExportErrorAndAbsent(t *testing.T) {
	o, s := readyExportObserver(t)
	s.err = errors.New("backend disconnected")
	if desired, err := o.DesiredLocalEndpoints(context.Background()); err == nil || desired != nil || o.Observations[0].State != "observation-error" {
		t.Fatal("backend error converted to empty authoritative state")
	}
	s.err = nil
	o.server.store.records = map[string]*endpointRecord{}
	desired, err := o.DesiredLocalEndpoints(context.Background())
	if err != nil || len(desired) != 0 || o.Observations[0].State != "absent" {
		t.Fatal("absence not distinguished")
	}
}
