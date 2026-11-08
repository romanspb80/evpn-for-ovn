package nodeagent

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent/ovnexport"
)

type contractOVNSource struct {
	ports    []ovnexport.OVNLogicalSwitchPort
	bindings []ovnexport.OVNPortBinding
	err      error
}

func (s *contractOVNSource) ListLogicalSwitchPorts(context.Context) ([]ovnexport.OVNLogicalSwitchPort, error) {
	return s.ports, s.err
}

func (s *contractOVNSource) ListPortBindings(context.Context) ([]ovnexport.OVNPortBinding, error) {
	return s.bindings, s.err
}

type contractRouteAPI struct {
	paths      []evpn.Type2RIBPath
	operations []string
}

func (a *contractRouteAPI) Snapshot(context.Context) ([]evpn.Type2RIBPath, error) {
	return append([]evpn.Type2RIBPath(nil), a.paths...), nil
}

func (a *contractRouteAPI) Add(_ context.Context, route evpn.Type2Route) (string, error) {
	uuid := fmt.Sprintf("owned-%d", len(a.operations)+1)
	nlri := route.NLRI
	nlri.Labels = append([]uint32(nil), route.NLRI.Labels...)
	a.paths = append(a.paths, evpn.Type2RIBPath{
		PathUUID: uuid, Local: true, NLRI: &nlri, PathAttributes: route.PathAttributes,
	})
	a.operations = append(a.operations, "add:"+uuid)
	return uuid, nil
}

func (a *contractRouteAPI) Delete(_ context.Context, route evpn.OwnedType2Route) error {
	for i, path := range a.paths {
		if path.PathUUID == route.PathUUID {
			a.paths = append(a.paths[:i], a.paths[i+1:]...)
			a.operations = append(a.operations, "delete:"+route.PathUUID)
			return nil
		}
	}
	return fmt.Errorf("owned route UUID %q is absent", route.PathUUID)
}

type contractReadiness struct{}

func (contractReadiness) Check(context.Context) error { return nil }

type contractObserverFunc func(context.Context) ([]evpn.LocalEndpoint, error)

func (f contractObserverFunc) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	return f(ctx)
}

type frameworkObserverContract struct {
	observer *ovnexport.FrameworkEndpointObserver
	source   *contractOVNSource
}

func newFrameworkObserverContract() frameworkObserverContract {
	up := true
	attachmentID, workloadUID := "attachment-a", "workload-a"
	port := ovnexport.OVNLogicalSwitchPort{
		UUID: "lsp-uuid-a", Name: "lsp-a", LogicalSwitch: "ls-a",
		Addresses: []string{"02:00:00:00:00:0a 10.20.0.10"},
		ExternalIDs: map[string]string{
			attachment.ExternalIDManagedBy:   attachment.OwnerValue,
			attachment.ExternalIDNetworkID:   "network-a",
			attachment.ExternalIDEndpointID:  string(endpoint.NewEndpointID("network-a", attachmentID, workloadUID)),
			attachment.ExternalIDAttachID:    attachmentID,
			attachment.ExternalIDWorkloadUID: workloadUID,
			attachment.ExternalIDIncarnation: "incarnation-a",
			attachment.ExternalIDIfaceID:     "lsp-a",
		},
	}
	source := &contractOVNSource{
		ports:    []ovnexport.OVNLogicalSwitchPort{port},
		bindings: []ovnexport.OVNPortBinding{{LogicalPort: "lsp-a", Chassis: "chassis-a", Up: &up}},
	}
	observer := &ovnexport.FrameworkEndpointObserver{
		Source: source, LocalNode: "node-a", LocalChassis: "chassis-a",
		Networks: map[string]evpn.EVPNNetworkConfig{
			"network-a": {
				NetworkID: "network-a", LogicalSwitch: "ls-a", Enabled: true,
				ExporterSource: "framework", VNI: 100, RouteDistinguisher: "64512:100",
				RouteTargets: []string{"64512:100"}, LocalVTEP: "192.0.2.10",
			},
		},
	}
	return frameworkObserverContract{observer: observer, source: source}
}

func contractExporter(observer evpn.EndpointObserver, api *contractRouteAPI) *evpn.EndpointExporter {
	return &evpn.EndpointExporter{
		Observer: observer, Gate: contractReadiness{}, Source: api, Applier: api,
	}
}

func TestEndpointObserverContractAuthorizedAndStableObservation(t *testing.T) {
	fixture := newFrameworkObserverContract()
	api := &contractRouteAPI{}
	exporter := contractExporter(fixture.observer, api)

	first, err := exporter.Reconcile(context.Background())
	if err != nil || len(first.Desired) != 1 || !first.Desired[0].Advertise || len(api.operations) != 1 {
		t.Fatalf("verified endpoint was not authorized and advertised: result=%+v operations=%v err=%v",
			first, api.operations, err)
	}
	second, err := exporter.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(first.Desired, second.Desired) || len(api.operations) != 1 {
		t.Fatalf("repeated stable observation changed candidates or repeated AddPath: first=%+v second=%+v operations=%v err=%v",
			first.Desired, second.Desired, api.operations, err)
	}
}

func TestEndpointObserverContractDisabledAdvertisementDoesNotAddPath(t *testing.T) {
	fixture := newFrameworkObserverContract()
	api := &contractRouteAPI{}
	observer := contractObserverFunc(func(ctx context.Context) ([]evpn.LocalEndpoint, error) {
		endpoints, err := fixture.observer.DesiredLocalEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		for i := range endpoints {
			endpoints[i].Advertise = false
		}
		return endpoints, nil
	})
	result, err := contractExporter(observer, api).Reconcile(context.Background())
	if err != nil || len(result.Desired) != 1 || result.Desired[0].Advertise || len(api.operations) != 0 {
		t.Fatalf("disabled advertisement mutated the RIB: result=%+v operations=%v err=%v", result, api.operations, err)
	}
}

func TestEndpointObserverContractUnboundEndpointCannotBeAdvertised(t *testing.T) {
	fixture := newFrameworkObserverContract()
	fixture.source.bindings = nil
	api := &contractRouteAPI{}
	result, err := contractExporter(fixture.observer, api).Reconcile(context.Background())
	if err != nil || len(result.Desired) != 0 || len(api.operations) != 0 {
		t.Fatalf("unbound endpoint produced an advertisement: result=%+v operations=%v err=%v", result, api.operations, err)
	}
}

func TestEndpointObserverContractUncertainObservationRetainsRoute(t *testing.T) {
	fixture := newFrameworkObserverContract()
	api := &contractRouteAPI{}
	exporter := contractExporter(fixture.observer, api)
	if _, err := exporter.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial authorized advertisement: %v", err)
	}

	fixture.source.bindings = []ovnexport.OVNPortBinding{{LogicalPort: "lsp-a", Chassis: "chassis-a"}}
	result, err := exporter.Reconcile(context.Background())
	if err == nil {
		t.Fatalf("uncertain readiness was accepted: result=%+v err=%v", result, err)
	}
	if len(api.operations) != 1 || len(api.paths) != 1 {
		t.Fatalf("uncertain observation withdrew an owned route: operations=%v paths=%d", api.operations, len(api.paths))
	}

	fixture.source.bindings = nil
	if _, err := exporter.Reconcile(context.Background()); err != nil {
		t.Fatalf("authoritative local-binding loss: %v", err)
	}
	if len(api.operations) != 2 || len(api.paths) != 0 || api.operations[1] != "delete:owned-1" {
		t.Fatalf("confirmed binding loss did not withdraw only the owned route: operations=%v paths=%d", api.operations, len(api.paths))
	}
}

func TestEndpointObserverContractAuthoritativeAbsenceAndReadinessLoss(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*frameworkObserverContract)
	}{
		{name: "complete snapshot absent", apply: func(f *frameworkObserverContract) { f.source.ports = nil }},
		{name: "confirmed readiness loss", apply: func(f *frameworkObserverContract) {
			down := false
			f.source.bindings = []ovnexport.OVNPortBinding{{LogicalPort: "lsp-a", Chassis: "chassis-a", Up: &down}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFrameworkObserverContract()
			api := &contractRouteAPI{}
			exporter := contractExporter(fixture.observer, api)
			if _, err := exporter.Reconcile(context.Background()); err != nil {
				t.Fatalf("initial authorized advertisement: %v", err)
			}
			tc.apply(&fixture)
			result, err := exporter.Reconcile(context.Background())
			if err != nil || len(result.Desired) != 0 {
				t.Fatalf("authoritative loss did not produce an empty desired set: result=%+v err=%v", result, err)
			}
			if len(api.paths) != 0 || len(api.operations) != 2 || api.operations[1] != "delete:owned-1" {
				t.Fatalf("authoritative loss did not withdraw only the owned route: operations=%v paths=%d", api.operations, len(api.paths))
			}
		})
	}
}

func TestEndpointObserverContractIdentityReplacementWithdrawsBeforeAdd(t *testing.T) {
	fixture := newFrameworkObserverContract()
	api := &contractRouteAPI{}
	exporter := contractExporter(fixture.observer, api)
	if _, err := exporter.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial authorized advertisement: %v", err)
	}

	port := fixture.source.ports[0]
	port.Addresses = []string{"02:00:00:00:00:0b 10.20.0.11"}
	port.ExternalIDs = map[string]string{
		attachment.ExternalIDManagedBy:   attachment.OwnerValue,
		attachment.ExternalIDNetworkID:   "network-a",
		attachment.ExternalIDEndpointID:  string(endpoint.NewEndpointID("network-a", "attachment-b", "workload-b")),
		attachment.ExternalIDAttachID:    "attachment-b",
		attachment.ExternalIDWorkloadUID: "workload-b",
		attachment.ExternalIDIncarnation: "incarnation-b",
		attachment.ExternalIDIfaceID:     "lsp-a",
	}
	fixture.source.ports = []ovnexport.OVNLogicalSwitchPort{port}
	result, err := exporter.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile endpoint replacement: %v", err)
	}
	if len(result.Desired) != 1 || result.Desired[0].EndpointID != string(endpoint.NewEndpointID("network-a", "attachment-b", "workload-b")) ||
		!reflect.DeepEqual(api.operations, []string{"add:owned-1", "delete:owned-1", "add:owned-3"}) {
		t.Fatalf("identity replacement did not withdraw old ownership before AddPath: desired=%+v operations=%v",
			result.Desired, api.operations)
	}
}
