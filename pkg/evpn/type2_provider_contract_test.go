package evpn

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type neutralRemoteType2Source struct {
	snapshot RemoteType2Snapshot
	err      error
}

func (s *neutralRemoteType2Source) ReadRemoteType2RIB(context.Context) (RemoteType2Snapshot, error) {
	return s.snapshot, s.err
}

type neutralRouteSnapshotSource struct {
	snapshot Type2RouteSnapshot
	err      error
}

func (s *neutralRouteSnapshotSource) SnapshotType2(context.Context) (Type2RouteSnapshot, error) {
	return s.snapshot, s.err
}

type recordingDataplaneBackend struct {
	applied   []ImportedType2Route
	withdrawn []ImportedType2Route
	events    []string
}

func (b *recordingDataplaneBackend) Apply(_ context.Context, route ImportedType2Route) error {
	b.applied = append(b.applied, route)
	b.events = append(b.events, "apply:"+route.Key)
	return nil
}

func (b *recordingDataplaneBackend) Withdraw(_ context.Context, route ImportedType2Route) error {
	b.withdrawn = append(b.withdrawn, route)
	b.events = append(b.events, "withdraw:"+route.Key)
	return nil
}

func TestProviderNeutralRemoteRIBContractAndCompleteness(t *testing.T) {
	route := DecodedRemoteType2{
		Source:         RemotePathIdentity{Key: "path-1"},
		Classification: "remote",
		RouteType:      2,
		RD:             "64512:1",
		ESI:            "00000000000000000000",
		MAC:            "02:00:00:00:00:10",
		IP:             "10.10.0.10",
		Labels:         []uint32{100},
		RouteTargets:   []string{"64512:100"},
		Encapsulation:  []string{"vxlan"},
		NextHop:        "192.0.2.80",
	}
	source := &neutralRemoteType2Source{snapshot: RemoteType2Snapshot{
		Complete: true, Paths: []RemoteType2PathObservation{{Route: route}},
	}}
	importer, err := NewReadOnlyType2ImporterFromSnapshot(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmed := confirmedImport(t, importer)
	if !confirmed.Authoritative || len(confirmed.Endpoints) != 1 ||
		confirmed.Endpoints[0].MAC != route.MAC || confirmed.Endpoints[0].RemoteVTEP != route.NextHop {
		t.Fatalf("provider-neutral route was not imported: %+v", confirmed)
	}

	source.snapshot = RemoteType2Snapshot{Complete: false}
	got, err := importer.Poll(context.Background())
	if err == nil || got.Authoritative || len(got.Endpoints) != 1 {
		t.Fatalf("incomplete provider snapshot replaced confirmed state: snapshot=%+v err=%v", got, err)
	}

	source.err = errors.New("provider transport unavailable")
	got, err = importer.Poll(context.Background())
	if err == nil || got.Authoritative || len(got.Endpoints) != 1 {
		t.Fatalf("provider error replaced confirmed state: snapshot=%+v err=%v", got, err)
	}
}

func TestProviderNeutralRouteSnapshotDoesNotWithdrawOnIncompleteObservation(t *testing.T) {
	source := &neutralRouteSnapshotSource{snapshot: Type2RouteSnapshot{
		Complete: true,
		Routes: []Type2RouteObservation{{
			MAC: "02:00:00:00:00:10", IP: "10.10.0.10", RD: "64512:1",
			Labels: []uint32{100}, NextHop: "192.0.2.80", RouteTargets: []string{"64512:100"},
		}},
	}}
	backend := &recordingDataplaneBackend{}
	importer := NewRemoteType2ImporterFromSnapshot(source, &Type2RouteValidator{
		LocalVTEP: "192.0.2.10",
		Networks:  []EVPNImportNetwork{{Name: "blue", VNI: 100, RouteTarget: "64512:100"}},
	}, backend)
	if err := importer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.applied) != 1 || len(backend.withdrawn) != 0 {
		t.Fatalf("complete provider snapshot did not apply exactly one route: %+v", backend)
	}

	source.snapshot = Type2RouteSnapshot{Complete: false}
	if err := importer.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("incomplete provider snapshot accepted")
	}
	if len(backend.withdrawn) != 0 {
		t.Fatalf("incomplete provider snapshot authorized withdrawal: %+v", backend.withdrawn)
	}
}

func TestDataplaneBackendReconciliationIsDeterministicAndWithdrawalFirst(t *testing.T) {
	makeRoutes := func(nextHop string) []Type2RouteObservation {
		return []Type2RouteObservation{
			{MAC: "02:00:00:00:00:11", IP: "10.10.0.11", RD: "64512:1",
				Labels: []uint32{100}, NextHop: nextHop, RouteTargets: []string{"64512:100"}},
			{MAC: "02:00:00:00:00:10", IP: "10.10.0.10", RD: "64512:1",
				Labels: []uint32{100}, NextHop: nextHop, RouteTargets: []string{"64512:100"}},
		}
	}
	source := &neutralRouteSnapshotSource{snapshot: Type2RouteSnapshot{
		Complete: true, Routes: makeRoutes("192.0.2.80"),
	}}
	backend := &recordingDataplaneBackend{}
	importer := NewRemoteType2ImporterFromSnapshot(source, &Type2RouteValidator{
		LocalVTEP: "192.0.2.10",
		Networks:  []EVPNImportNetwork{{Name: "blue", VNI: 100, RouteTarget: "64512:100"}},
	}, backend)

	if err := importer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"blue/100/02:00:00:00:00:10/10.10.0.10",
		"blue/100/02:00:00:00:00:11/10.10.0.11",
	}
	wantApply := []string{"apply:" + wantKeys[0], "apply:" + wantKeys[1]}
	if !reflect.DeepEqual(backend.events, wantApply) {
		t.Fatalf("initial apply order is nondeterministic: got %v want %v", backend.events, wantApply)
	}

	backend.events = nil
	if err := importer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.events) != 0 {
		t.Fatalf("identical complete observation was not idempotent: %v", backend.events)
	}

	source.snapshot = Type2RouteSnapshot{Complete: false, Routes: makeRoutes("192.0.2.81")}
	if err := importer.ReconcileOnce(context.Background()); err == nil || len(backend.events) != 0 {
		t.Fatalf("incomplete observation mutated dataplane: events=%v err=%v", backend.events, err)
	}

	source.snapshot = Type2RouteSnapshot{Complete: true, Routes: makeRoutes("192.0.2.81")}
	if err := importer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantMobility := []string{
		"withdraw:" + wantKeys[0], "apply:" + wantKeys[0],
		"withdraw:" + wantKeys[1], "apply:" + wantKeys[1],
	}
	if !reflect.DeepEqual(backend.events, wantMobility) {
		t.Fatalf("mobility reconciliation was not deterministic with per-route withdrawal before apply: got %v want %v",
			backend.events, wantMobility)
	}

	backend.events = nil
	source.snapshot = Type2RouteSnapshot{Complete: true}
	if err := importer.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantDelete := []string{"withdraw:" + wantKeys[0], "withdraw:" + wantKeys[1]}
	if !reflect.DeepEqual(backend.events, wantDelete) {
		t.Fatalf("delete order is nondeterministic: got %v want %v", backend.events, wantDelete)
	}
}
