package evpn

import (
	"context"
	"fmt"
	"testing"
)

type interruptedExportGate struct{ calls, fail int }

func (g *interruptedExportGate) Check(context.Context) error {
	g.calls++
	if g.calls == g.fail {
		return fmt.Errorf("transient failure before second mutation")
	}
	return nil
}

func TestEndpointExporterPartialRecoveryReceipts(t *testing.T) {
	first := sampleEndpoint()
	second := cloneLocalEndpoint(first)
	second.EndpointID, second.MACAddress = "second", "02:00:00:00:00:99"
	o := &exportTestObserver{desired: []LocalEndpoint{first, second}}
	g := &interruptedExportGate{fail: 3}
	a := &memoryType2API{}
	e := &EndpointExporter{Observer: o, Gate: g, Continuity: &exportTestGate{}, Source: a, Applier: a}
	res, err := e.Reconcile(context.Background())
	if err == nil || !res.OwnershipUnverified || len(res.Applied) != 1 || a.adds != 1 {
		t.Fatalf("partial success lost receipt: %+v %v", res, err)
	}
	g.fail = 0
	res, err = e.Reconcile(context.Background())
	if err != nil || res.OwnershipUnverified || len(res.Applied) != 2 || a.adds != 2 {
		t.Fatalf("partial retry duplicated or lost state: %+v %v", res, err)
	}
}

func TestExporterContinuityRequiresUnix(t *testing.T) {
	c := ExporterConfig{Enabled: true, ProcessContinuity: true, NodeName: "node", LocalChassis: "chassis", ASN: 64512, RouterID: "192.0.2.32", GRPCEndpoint: "127.0.0.1:50051"}
	if err := c.Validate(); err == nil {
		t.Fatal("TCP cannot provide Unix peer process evidence")
	}
	c.GRPCEndpoint = "unix:///var/run/gobgp/gobgp.sock"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
