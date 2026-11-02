package evpn

import (
	"context"
	"errors"
	"testing"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"google.golang.org/grpc"
)

type exportTestObserver struct {
	desired []LocalEndpoint
	err     error
}

func (o *exportTestObserver) DesiredLocalEndpoints(context.Context) ([]LocalEndpoint, error) {
	return o.desired, o.err
}

type exportTestGate struct {
	err   error
	calls int
}

func (g *exportTestGate) Check(context.Context) error { g.calls++; return g.err }

func TestEndpointExporterSafety(t *testing.T) {
	ctx := context.Background()
	ep := sampleEndpoint()
	o := &exportTestObserver{desired: []LocalEndpoint{ep}}
	g := &exportTestGate{}
	a := &memoryType2API{}
	e := &EndpointExporter{Observer: o, Gate: g, Source: a, Applier: a, DryRun: true}
	if res, err := e.Reconcile(ctx); err != nil || len(res.Desired) != 1 || a.adds != 0 || g.calls != 0 {
		t.Fatalf("dry run called speaker: %+v %v", res, err)
	}
	e.DryRun = false
	g.err = errors.New("ASN zero")
	if _, err := e.Reconcile(ctx); err == nil || a.adds != 0 {
		t.Fatal("not-ready speaker permitted mutation")
	}
	g.err = nil
	a.addErr = errors.New("AddPath failed")
	if res, err := e.Reconcile(ctx); err == nil || len(res.Applied) != 0 {
		t.Fatalf("failed add acknowledged: %v %v", res, err)
	}
	a.addErr = nil
	if res, err := e.Reconcile(ctx); err != nil || len(res.Applied) != 1 {
		t.Fatalf("retry failed: %v %v", res, err)
	}
	adds := a.adds
	if _, err := e.Reconcile(ctx); err != nil || a.adds != adds {
		t.Fatal("periodic reconcile duplicated route")
	}
	o.err = errors.New("SB disconnected")
	o.desired = nil
	if res, err := e.Reconcile(ctx); err == nil || len(res.Applied) != 1 || a.deletes != 0 {
		t.Fatal("observer error withdrew routes")
	}
	o.err = nil
	a.snapshotErr = errors.New("temporary RIB read failure")
	if _, err := e.Reconcile(ctx); err == nil || a.deletes != 0 {
		t.Fatal("RIB error allowed mutation")
	}
	a.snapshotErr = nil
	a.deleteErr = errors.New("DeletePath failed")
	if res, err := e.Reconcile(ctx); err == nil || len(res.Applied) != 1 {
		t.Fatal("failed delete acknowledged")
	}
	a.deleteErr = nil
	if res, err := e.Reconcile(ctx); err != nil || len(res.Applied) != 0 || len(a.paths) != 0 {
		t.Fatalf("DEL retry: %v %v", res, err)
	}
}

func TestEndpointExporterSpeakerContinuity(t *testing.T) {
	for _, mode := range []string{"unready", "lost-rib"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			o := &exportTestObserver{desired: []LocalEndpoint{sampleEndpoint()}}
			g := &exportTestGate{}
			a := &memoryType2API{}
			e := &EndpointExporter{Observer: o, Gate: g, Source: a, Applier: a}
			if _, err := e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "unready" {
				g.err = errors.New("API unavailable")
			} else {
				a.paths = nil
			}
			if _, err := e.Reconcile(ctx); err == nil {
				t.Fatal("lost speaker continuity accepted")
			}
			g.err = nil
			if _, err := e.Reconcile(ctx); err == nil || a.adds != 1 || a.deletes != 0 {
				t.Fatal("unsafe automatic recovery")
			}
		})
	}
}

func TestEndpointExporterPermanentFenceContract(t *testing.T) {
	for _, cause := range []string{"API timeout", "ASN zero", "router ID changed", "EVPN missing"} {
		t.Run(cause, func(t *testing.T) {
			ctx := context.Background()
			ep := sampleEndpoint()
			second := cloneLocalEndpoint(ep)
			second.EndpointID, second.MACAddress = "second", "02:00:00:00:00:99"
			o := &exportTestObserver{desired: []LocalEndpoint{ep, second}}
			g := &exportTestGate{}
			a := &memoryType2API{}
			e := &EndpointExporter{Observer: o, Gate: g, Source: a, Applier: a}
			if res, err := e.Reconcile(ctx); err != nil || len(res.Applied) != 2 {
				t.Fatalf("initial apply: %+v %v", res, err)
			}
			g.err = errors.New(cause)
			if res, err := e.Reconcile(ctx); err == nil || len(res.Applied) != 2 {
				t.Fatalf("uncertainty must freeze writes: %+v %v", res, err)
			}
			g.err, o.desired = nil, nil
			if res, err := e.Reconcile(ctx); err == nil || len(res.Applied) != 2 || a.deletes != 0 {
				t.Fatalf("baseline must not guess continuity after reconnect: %+v %v", res, err)
			}
		})
	}
}

type exportGlobalClient struct {
	api.GoBgpServiceClient
	global *api.Global
	err    error
}

func (c exportGlobalClient) GetBgp(context.Context, *api.GetBgpRequest, ...grpc.CallOption) (*api.GetBgpResponse, error) {
	return &api.GetBgpResponse{Global: c.global}, c.err
}
func TestSpeakerReadiness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global *api.Global
		err    error
		ready  bool
	}{
		{"ready", &api.Global{Asn: 64512, RouterId: "192.0.2.32", Families: []uint32{uint32(oc.AFI_SAFI_TYPE_L2VPN_EVPN.ToInt())}}, nil, true},
		{"unconfigured", &api.Global{}, nil, false},
		{"wrong-asn", &api.Global{Asn: 64513, RouterId: "192.0.2.32", Families: []uint32{9}}, nil, false},
		{"wrong-router", &api.Global{Asn: 64512, RouterId: "192.0.2.33", Families: []uint32{9}}, nil, false},
		{"no-evpn", &api.Global{Asn: 64512, RouterId: "192.0.2.32", Families: []uint32{0}}, nil, false},
		{"api-error", nil, errors.New("unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (SpeakerReadiness{Client: exportGlobalClient{global: tc.global, err: tc.err}, ASN: 64512, RouterID: "192.0.2.32"}).Check(context.Background())
			if (err == nil) != tc.ready {
				t.Fatalf("readiness: %v", err)
			}
		})
	}
}

func TestExporterConfigValidation(t *testing.T) {
	valid := func() ExporterConfig {
		return ExporterConfig{Enabled: true, NodeName: "worker", LocalChassis: "chassis", GRPCEndpoint: "unix:///run/gobgp/gobgp.sock", ASN: 64512, RouterID: "192.0.2.32", Networks: []EVPNNetworkConfig{{NetworkID: "n", LogicalSwitch: "ls", Enabled: true, VNI: 100, RouteDistinguisher: "64512:100", RouteTargets: []string{"64512:100"}, LocalVTEP: "192.0.2.32"}}}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ExporterConfig)
	}{
		{"vni-zero", func(c *ExporterConfig) { c.Networks[0].VNI = 0 }},
		{"vni-overflow", func(c *ExporterConfig) { c.Networks[0].VNI = 16777216 }},
		{"rd", func(c *ExporterConfig) { c.Networks[0].RouteDistinguisher = "bad" }},
		{"rt", func(c *ExporterConfig) { c.Networks[0].RouteTargets = []string{"bad"} }},
		{"ipv6-vtep", func(c *ExporterConfig) { c.Networks[0].LocalVTEP = "2001:db8::1" }},
		{"unspecified-vtep", func(c *ExporterConfig) { c.Networks[0].LocalVTEP = "0.0.0.0" }},
		{"service-target", func(c *ExporterConfig) { c.GRPCEndpoint = "k8gobgp:50051" }},
		{"invalid-port", func(c *ExporterConfig) { c.GRPCEndpoint = "127.0.0.1:bgp" }},
		{"zero-port", func(c *ExporterConfig) { c.GRPCEndpoint = "127.0.0.1:0" }},
		{"overflow-port", func(c *ExporterConfig) { c.GRPCEndpoint = "127.0.0.1:65536" }},
		{"asn", func(c *ExporterConfig) { c.ASN = 0 }},
		{"switch", func(c *ExporterConfig) { c.Networks[0].LogicalSwitch = "" }},
		{"duplicate", func(c *ExporterConfig) { c.Networks = append(c.Networks, c.Networks[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
