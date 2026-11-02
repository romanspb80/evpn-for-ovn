package evpn

import (
	"context"
	"testing"
)

type fakeOVNSource struct {
	ports    []OVNLogicalSwitchPort
	bindings []OVNPortBinding
}

func (f *fakeOVNSource) ListLogicalSwitchPorts(context.Context) ([]OVNLogicalSwitchPort, error) {
	return f.ports, nil
}

func (f *fakeOVNSource) ListPortBindings(context.Context) ([]OVNPortBinding, error) {
	return f.bindings, nil
}

func TestOVNEndpointObserverFiltersUnboundRemoteAndDisabled(t *testing.T) {
	src := &fakeOVNSource{
		ports: []OVNLogicalSwitchPort{
			{
				Name:      "lsp-local",
				Addresses: []string{"00:11:22:33:44:55 10.20.0.10"},
				ExternalIDs: map[string]string{
					"evpn.framework/endpoint-id":   "ep-local",
					"evpn.framework/network-id":    "net-a",
					"evpn.framework/attachment-id": "net1",
					"evpn.framework/workload-uid":  "uid-local",
					"evpn.framework/incarnation":   "inc-1",
				},
			},
			{
				Name:      "lsp-remote",
				Addresses: []string{"00:11:22:33:44:66 10.20.0.11"},
				ExternalIDs: map[string]string{
					"evpn.framework/endpoint-id":   "ep-remote",
					"evpn.framework/network-id":    "net-a",
					"evpn.framework/attachment-id": "net1",
					"evpn.framework/workload-uid":  "uid-remote",
					"evpn.framework/incarnation":   "inc-1",
				},
			},
			{
				Name:      "lsp-disabled-net",
				Addresses: []string{"00:11:22:33:44:77 10.20.0.12"},
				ExternalIDs: map[string]string{
					"evpn.framework/endpoint-id":   "ep-disabled",
					"evpn.framework/network-id":    "net-disabled",
					"evpn.framework/attachment-id": "net1",
					"evpn.framework/workload-uid":  "uid-disabled",
					"evpn.framework/incarnation":   "inc-1",
				},
			},
			{
				Name:      "lsp-unbound",
				Addresses: []string{"00:11:22:33:44:88 10.20.0.13"},
				ExternalIDs: map[string]string{
					"evpn.framework/endpoint-id":   "ep-unbound",
					"evpn.framework/network-id":    "net-a",
					"evpn.framework/attachment-id": "net1",
					"evpn.framework/workload-uid":  "uid-unbound",
					"evpn.framework/incarnation":   "inc-1",
				},
			},
		},
		bindings: []OVNPortBinding{
			{LogicalPort: "lsp-local", Chassis: "ch-local"},
			{LogicalPort: "lsp-remote", Chassis: "ch-remote"},
			{LogicalPort: "lsp-disabled-net", Chassis: "ch-local"},
		},
	}

	obs := &OVNEndpointObserver{
		Source:       src,
		LocalNode:    "node-a",
		LocalChassis: "ch-local",
		Networks: map[string]EVPNNetworkConfig{
			"net-a": {
				NetworkID:          "net-a",
				Enabled:            true,
				VNI:                100,
				RouteDistinguisher: "65000:100",
				RouteTargets:       []string{"65000:100"},
				LocalVTEP:          "10.0.0.1",
			},
		},
	}

	got, err := obs.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe local endpoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 local endpoint, got %d: %#v", len(got), got)
	}
	if got[0].EndpointID != "ep-local" {
		t.Fatalf("unexpected endpoint selected: %#v", got[0])
	}
}

func TestOVNEndpointObserverIdempotentWithReconciler(t *testing.T) {
	src := &fakeOVNSource{
		ports: []OVNLogicalSwitchPort{
			{
				Name:      "lsp-local",
				Addresses: []string{"00:11:22:33:44:55 10.20.0.10"},
				ExternalIDs: map[string]string{
					"evpn.framework/endpoint-id":   "ep-local",
					"evpn.framework/network-id":    "net-a",
					"evpn.framework/attachment-id": "net1",
					"evpn.framework/workload-uid":  "uid-local",
					"evpn.framework/incarnation":   "inc-1",
				},
			},
		},
		bindings: []OVNPortBinding{{LogicalPort: "lsp-local", Chassis: "ch-local"}},
	}
	obs := &OVNEndpointObserver{
		Source:       src,
		LocalNode:    "node-a",
		LocalChassis: "ch-local",
		Networks: map[string]EVPNNetworkConfig{
			"net-a": {
				NetworkID:          "net-a",
				Enabled:            true,
				VNI:                100,
				RouteDistinguisher: "65000:100",
				RouteTargets:       []string{"65000:100"},
				LocalVTEP:          "10.0.0.1",
			},
		},
	}

	r := NewRouteReconciler()
	desired, err := obs.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe desired endpoints: %v", err)
	}
	first, err := r.Reconcile(desired)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(first) != 1 || first[0].Action != RouteAdvertise {
		t.Fatalf("expected single advertise change, got %#v", first)
	}
	api := &memoryType2API{}
	if err := r.Resync(context.Background(), api, nil); err != nil {
		t.Fatal(err)
	}
	applyType2(t, r, api, planType2(t, r, desired...))

	desiredAgain, err := obs.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe desired endpoints second time: %v", err)
	}
	second, err := r.Reconcile(desiredAgain)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("expected idempotent no-op on second reconcile, got %#v", second)
	}
}
