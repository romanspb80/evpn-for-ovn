package ovnexport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type frameworkObserverSource struct {
	ports    []OVNLogicalSwitchPort
	bindings []OVNPortBinding
	err      error
}

type frameworkRouteApplier struct{}

type emptyRouteSource struct{}

type mutableFrameworkRoutes struct {
	paths   []evpn.Type2RIBPath
	adds    int
	deletes int
}

type readyFrameworkGate struct{}

func (emptyRouteSource) Snapshot(context.Context) ([]evpn.Type2RIBPath, error) { return nil, nil }

func (frameworkRouteApplier) Add(context.Context, evpn.Type2Route) (string, error) {
	return "acknowledged-uuid", nil
}

func (frameworkRouteApplier) Delete(context.Context, evpn.OwnedType2Route) error { return nil }

func (g readyFrameworkGate) Check(context.Context) error { return nil }

func (r *mutableFrameworkRoutes) Snapshot(context.Context) ([]evpn.Type2RIBPath, error) {
	return append([]evpn.Type2RIBPath(nil), r.paths...), nil
}

func (r *mutableFrameworkRoutes) Add(_ context.Context, route evpn.Type2Route) (string, error) {
	r.adds++
	uuid := fmt.Sprintf("uuid-%d", r.adds)
	nlri := route.NLRI
	nlri.Labels = append([]uint32(nil), route.NLRI.Labels...)
	r.paths = append(r.paths, evpn.Type2RIBPath{
		PathUUID: uuid, Local: true, NLRI: &nlri, PathAttributes: route.PathAttributes,
	})
	return uuid, nil
}

func (r *mutableFrameworkRoutes) Delete(_ context.Context, owned evpn.OwnedType2Route) error {
	for i, path := range r.paths {
		if path.PathUUID == owned.PathUUID {
			r.paths = append(r.paths[:i], r.paths[i+1:]...)
			r.deletes++
			return nil
		}
	}
	return fmt.Errorf("owned UUID %q not found", owned.PathUUID)
}

func (s frameworkObserverSource) ListLogicalSwitchPorts(context.Context) ([]OVNLogicalSwitchPort, error) {
	return s.ports, s.err
}

func (s frameworkObserverSource) ListPortBindings(context.Context) ([]OVNPortBinding, error) {
	return s.bindings, s.err
}

func frameworkPort(name, networkID, endpointID, macIP string) OVNLogicalSwitchPort {
	attachmentID, workloadUID := "net1", "uid-"+endpointID
	return OVNLogicalSwitchPort{
		UUID:          "uuid-" + name,
		Name:          name,
		LogicalSwitch: "ls-" + networkID,
		Addresses:     []string{macIP},
		ExternalIDs: map[string]string{
			attachment.ExternalIDManagedBy:   attachment.OwnerValue,
			attachment.ExternalIDEndpointID:  string(endpoint.NewEndpointID(networkID, attachmentID, workloadUID)),
			attachment.ExternalIDNetworkID:   networkID,
			attachment.ExternalIDAttachID:    attachmentID,
			attachment.ExternalIDWorkloadUID: workloadUID,
			attachment.ExternalIDIncarnation: "inc-" + endpointID,
			attachment.ExternalIDIfaceID:     name,
		},
	}
}

func frameworkReadyBinding(name string) OVNPortBinding {
	up := true
	return OVNPortBinding{LogicalPort: name, Chassis: "ch-local", Up: &up}
}

func frameworkObserver(source OVNStateSource) *FrameworkEndpointObserver {
	return &FrameworkEndpointObserver{
		Source:       source,
		LocalNode:    "node-a",
		LocalChassis: "ch-local",
		Networks: map[string]evpn.EVPNNetworkConfig{
			"net-a": {
				NetworkID: "net-a", LogicalSwitch: "ls-net-a", Enabled: true,
				ExporterSource: "framework", VNI: 100, RouteDistinguisher: "65000:100",
				RouteTargets: []string{"65000:100"}, LocalVTEP: "10.0.0.1",
			},
		},
	}
}

func TestFrameworkEndpointObserverSelectsOnlyEnabledLocalValidatedCandidates(t *testing.T) {
	source := frameworkObserverSource{
		ports: []OVNLogicalSwitchPort{
			frameworkPort("lsp-local", "net-a", "ep-local", "00:11:22:33:44:55 10.20.0.10"),
			frameworkPort("lsp-remote", "net-a", "ep-remote", "00:11:22:33:44:66 10.20.0.11"),
			frameworkPort("lsp-disabled", "net-disabled", "ep-disabled", "00:11:22:33:44:77 10.20.0.12"),
			frameworkPort("lsp-unbound", "net-a", "ep-unbound", "00:11:22:33:44:88 10.20.0.13"),
			frameworkPort("lsp-no-identity", "net-a", "", "00:11:22:33:44:99 10.20.0.14"),
			frameworkPort("lsp-invalid-address", "net-a", "ep-invalid", "invalid-address"),
		},
		bindings: []OVNPortBinding{
			frameworkReadyBinding("lsp-local"),
			{LogicalPort: "lsp-remote", Chassis: "ch-remote", Up: boolPointer(true)},
			frameworkReadyBinding("lsp-disabled"),
		},
	}
	observer := frameworkObserver(source)
	observer.Networks["net-disabled"] = evpn.EVPNNetworkConfig{NetworkID: "net-disabled", Enabled: false, ExporterSource: "framework"}

	got, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe local endpoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one validated candidate, got %d: %#v", len(got), got)
	}
	wantID := string(endpoint.NewEndpointID("net-a", "net1", "uid-ep-local"))
	if got[0].EndpointID != wantID || got[0].Incarnation != "inc-ep-local" || !got[0].Advertise ||
		got[0].NetworkID != "net-a" || got[0].MACAddress != "00:11:22:33:44:55" || got[0].IPAddresses[0] != "10.20.0.10" {
		t.Fatalf("unexpected candidate: %#v", got[0])
	}
}

func TestFrameworkObserverRequiresLocalReadyOwnedEndpoint(t *testing.T) {
	port := frameworkPort("lsp-local", "net-a", "ep-local", "00:11:22:33:44:55 10.20.0.10")
	for _, tc := range []struct {
		name     string
		bindings []OVNPortBinding
		mutate   func(*OVNLogicalSwitchPort)
	}{
		{name: "exists-but-unbound"},
		{name: "bound-to-remote", bindings: []OVNPortBinding{{LogicalPort: "lsp-local", Chassis: "ch-remote", Up: boolPointer(true)}}},
		{name: "local-but-down", bindings: []OVNPortBinding{{LogicalPort: "lsp-local", Chassis: "ch-local", Up: boolPointer(false)}}},
		{name: "wrong-managed-owner", bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")}, mutate: func(p *OVNLogicalSwitchPort) {
			p.ExternalIDs[attachment.ExternalIDManagedBy] = "foreign"
		}},
		{name: "wrong-interface-identity", bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")}, mutate: func(p *OVNLogicalSwitchPort) {
			p.ExternalIDs[attachment.ExternalIDIfaceID] = "other"
		}},
		{name: "wrong-logical-switch", bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")}, mutate: func(p *OVNLogicalSwitchPort) {
			p.LogicalSwitch = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := port
			candidate.ExternalIDs = copyIDs(port.ExternalIDs)
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			observer := frameworkObserver(frameworkObserverSource{
				ports: []OVNLogicalSwitchPort{candidate}, bindings: tc.bindings,
			})
			got, err := observer.DesiredLocalEndpoints(context.Background())
			if err != nil {
				t.Fatalf("observe endpoint: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("unauthorized endpoint produced candidate: %#v", got)
			}
		})
	}
}

func TestFrameworkEndpointObserverErrorsAreNotEmptySnapshots(t *testing.T) {
	wantErr := errors.New("OVN read failed")
	observer := frameworkObserver(frameworkObserverSource{err: wantErr})
	if got, err := observer.DesiredLocalEndpoints(context.Background()); !errors.Is(err, wantErr) || got != nil {
		t.Fatalf("OVN failure became an empty snapshot: endpoints=%v err=%v", got, err)
	}
}

func TestFrameworkEndpointObserverReturnsStableCandidates(t *testing.T) {
	source := frameworkObserverSource{
		ports:    []OVNLogicalSwitchPort{frameworkPort("lsp-local", "net-a", "ep-local", "00:11:22:33:44:55 10.20.0.10")},
		bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")},
	}
	observer := frameworkObserver(source)
	first, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe desired endpoints: %v", err)
	}
	second, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe desired endpoints again: %v", err)
	}
	if !reflect.DeepEqual(first, second) || len(first) != 1 {
		t.Fatalf("repeated observations changed candidate values: first=%#v second=%#v", first, second)
	}
}

func TestFrameworkEndpointObserverRequiresCompleteReadinessAndWithdrawsSafely(t *testing.T) {
	source := frameworkObserverSource{
		ports:    []OVNLogicalSwitchPort{frameworkPort("lsp-local", "net-a", "ep-local", "00:11:22:33:44:55 10.20.0.10")},
		bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")},
	}
	observer := frameworkObserver(source)
	routes := &mutableFrameworkRoutes{}
	exporter := &evpn.EndpointExporter{
		Observer: observer, Gate: readyFrameworkGate{}, Source: routes, Applier: routes,
	}
	if _, err := exporter.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile authorized endpoint: %v", err)
	}
	if routes.adds != 1 || len(routes.paths) != 1 {
		t.Fatalf("authorized endpoint was not advertised exactly once: adds=%d paths=%d", routes.adds, len(routes.paths))
	}

	// A local binding with unknown readiness is not an authoritative empty set.
	observer.Source = frameworkObserverSource{
		ports: source.ports, bindings: []OVNPortBinding{{LogicalPort: "lsp-local", Chassis: "ch-local"}},
	}
	if _, err := exporter.Reconcile(context.Background()); err == nil {
		t.Fatal("uncertain readiness was accepted as an authoritative observation")
	}
	if routes.adds != 1 || routes.deletes != 0 || len(routes.paths) != 1 {
		t.Fatalf("uncertain observation mutated an existing route: adds=%d deletes=%d paths=%d", routes.adds, routes.deletes, len(routes.paths))
	}

	// Explicitly observed loss of readiness removes the candidate and withdraws
	// only the already-owned path; it never creates another AddPath.
	observer.Source = frameworkObserverSource{
		ports: source.ports, bindings: []OVNPortBinding{{LogicalPort: "lsp-local", Chassis: "ch-local", Up: boolPointer(false)}},
	}
	if _, err := exporter.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile explicit readiness loss: %v", err)
	}
	if routes.adds != 1 || routes.deletes != 1 || len(routes.paths) != 0 {
		t.Fatalf("readiness loss did not safely withdraw the owned route: adds=%d deletes=%d paths=%d", routes.adds, routes.deletes, len(routes.paths))
	}
}

func TestFrameworkEndpointObserverTracksIdentityReplacement(t *testing.T) {
	oldPort := frameworkPort("lsp-local", "net-a", "ep-local", "00:11:22:33:44:55 10.20.0.10")
	newPort := frameworkPort("lsp-local", "net-a", "ep-replacement", "00:11:22:33:44:66 10.20.0.11")
	newPort.ExternalIDs[attachment.ExternalIDAttachID] = "net2"
	newPort.ExternalIDs[attachment.ExternalIDWorkloadUID] = "uid-replacement"
	newPort.ExternalIDs[attachment.ExternalIDIncarnation] = "inc-replacement"
	newPort.ExternalIDs[attachment.ExternalIDEndpointID] = string(endpoint.NewEndpointID("net-a", "net2", "uid-replacement"))

	observer := frameworkObserver(frameworkObserverSource{
		ports: []OVNLogicalSwitchPort{oldPort}, bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")},
	})
	oldCandidate, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe initial endpoint: %v", err)
	}
	routes := &mutableFrameworkRoutes{}
	reconciler := evpn.NewRouteReconciler()
	if err := reconciler.Resync(context.Background(), emptyRouteSource{}, nil); err != nil {
		t.Fatalf("initial route resynchronization: %v", err)
	}
	deltas, err := reconciler.Reconcile(oldCandidate)
	if err != nil || len(deltas) != 1 || deltas[0].Action != evpn.RouteAdvertise {
		t.Fatalf("initial candidate did not plan an advertisement: deltas=%#v err=%v", deltas, err)
	}
	if err := reconciler.Apply(context.Background(), deltas[0], routes); err != nil {
		t.Fatalf("apply initial advertisement: %v", err)
	}

	observer.Source = frameworkObserverSource{
		ports: []OVNLogicalSwitchPort{newPort}, bindings: []OVNPortBinding{frameworkReadyBinding("lsp-local")},
	}
	newCandidate, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatalf("observe replacement endpoint: %v", err)
	}
	repeated, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil || !reflect.DeepEqual(newCandidate, repeated) {
		t.Fatalf("replacement candidate was not stable: candidate=%#v repeated=%#v err=%v", newCandidate, repeated, err)
	}
	if len(newCandidate) != 1 || newCandidate[0].EndpointID == oldCandidate[0].EndpointID ||
		newCandidate[0].Incarnation == oldCandidate[0].Incarnation ||
		newCandidate[0].RouteKey() == oldCandidate[0].RouteKey() {
		t.Fatalf("attachment/identity replacement did not create a new route candidate: old=%#v new=%#v", oldCandidate, newCandidate)
	}
	deltas, err = reconciler.Reconcile(newCandidate)
	if err != nil || len(deltas) != 2 || deltas[0].Action != evpn.RouteWithdraw || deltas[1].Action != evpn.RouteAdvertise {
		t.Fatalf("replacement did not plan owned withdrawal before advertisement: deltas=%#v err=%v", deltas, err)
	}
	for _, delta := range deltas {
		if err := reconciler.Apply(context.Background(), delta, routes); err != nil {
			t.Fatalf("apply replacement action %s: %v", delta.Action, err)
		}
	}
	if routes.adds != 2 || routes.deletes != 1 {
		t.Fatalf("replacement route operations mismatch: adds=%d deletes=%d", routes.adds, routes.deletes)
	}
}

func boolPointer(value bool) *bool { return &value }

func copyIDs(source map[string]string) map[string]string {
	copied := make(map[string]string, len(source))
	for key, value := range source {
		copied[key] = value
	}
	return copied
}
