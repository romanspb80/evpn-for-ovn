package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

type testGuard struct {
	id     string
	err    error
	closed int
}

func (g *testGuard) Check(context.Context) error  { return g.err }
func (g *testGuard) Incarnation() (string, error) { return g.id, g.err }
func (g *testGuard) Evidence() nodeagent.SpeakerProcessEvidence {
	return nodeagent.SpeakerProcessEvidence{}
}
func (g *testGuard) Close() error { g.closed++; return nil }

type testConnection struct{ closed int }

func (c *testConnection) Close() error { c.closed++; return nil }

type testRoutes struct {
	paths   []evpn.Type2RIBPath
	deleted []string
	addErr  error
}

func (r *testRoutes) Snapshot(context.Context) ([]evpn.Type2RIBPath, error) {
	return append([]evpn.Type2RIBPath(nil), r.paths...), nil
}

func (r *testRoutes) Add(_ context.Context, route evpn.Type2Route) (string, error) {
	id := uuid.NewString()
	nlri := route.NLRI
	r.paths = append(r.paths, evpn.Type2RIBPath{PathUUID: id, Local: true, NLRI: &nlri, PathAttributes: route.PathAttributes})
	if r.addErr != nil {
		return "", r.addErr
	}
	return id, nil
}

func (r *testRoutes) Delete(_ context.Context, owned evpn.OwnedType2Route) error {
	for i, path := range r.paths {
		if path.PathUUID == owned.PathUUID {
			r.deleted = append(r.deleted, path.PathUUID)
			r.paths = append(r.paths[:i], r.paths[i+1:]...)
			return nil
		}
	}
	return errors.New("exact UUID absent")
}

type testRemoteSource struct{ complete bool }

func (s *testRemoteSource) ReadRemoteRIB(context.Context) (evpn.RemoteRIBSnapshot, error) {
	return evpn.RemoteRIBSnapshot{Complete: s.complete}, nil
}

type testObserver struct{ endpoints []evpn.LocalEndpoint }

func (s *testObserver) DesiredLocalEndpoints(context.Context) ([]evpn.LocalEndpoint, error) {
	return s.endpoints, nil
}

func testTransport(id string, routes *testRoutes) *transport {
	guard := &testGuard{id: id}
	return &transport{conn: &testConnection{}, guard: guard, gate: guard,
		adapter: routes, applier: routes, source: &testRemoteSource{complete: true}}
}

func bindingFixture(t *testing.T) (config, ownership.Domain, *speakerBinding, *testRoutes, *testObserver, *lifecycle) {
	t.Helper()
	c, domain, ledger := lifecycleFixture(t)
	routes := &testRoutes{}
	current := testTransport("speaker-one", routes)
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{
		StateDirectory: filepath.Join(c.State, "export"), Domain: domain}, routes, routes, current.guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovery.Close() })
	reader, err := evpn.NewReadOnlyType2Importer(current.source, c.Import)
	if err != nil {
		t.Fatal(err)
	}
	observer := &testObserver{[]evpn.LocalEndpoint{c.Endpoint}}
	b := &speakerBinding{current: current, recovery: recovery, reader: reader, observer: observer,
		control: filepath.Join(c.State, "observation")}
	b.exporter = b.exporterFor(current)
	t.Cleanup(func() { _ = b.Close() })
	return c, domain, b, routes, observer, ledger
}

func TestSpeakerRebindClosesOldPeerAndRequiresFreshCompleteRIB(t *testing.T) {
	c, _, b, routes, _, _ := bindingFixture(t)
	ctx := context.Background()
	result, err := b.exporter.Reconcile(ctx)
	if err != nil || len(result.Applied) != 1 {
		t.Fatalf("initial owned export: %+v %v", result, err)
	}
	oldUUID := result.Applied[0].PathUUID
	for range 2 {
		if _, err := b.reader.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	old := b.current
	old.guard.(*testGuard).err = errors.New("old pidfd exited")
	if _, err := b.exporter.Reconcile(ctx); err == nil {
		t.Fatal("old speaker incarnation remained writable")
	}
	dialFailed := func(context.Context, config) (*transport, error) {
		if old.guard.(*testGuard).closed != 1 || old.conn.(*testConnection).closed != 1 {
			t.Fatal("new attestation attempted before closing old transport")
		}
		return nil, errors.New("new executable/config pin mismatch")
	}
	requireError(t, b.ensure(ctx, c, dialFailed), "attestation")
	if b.current != nil || len(routes.deleted) != 0 {
		t.Fatal("failed attestation authorized mutation")
	}
	// A replacement with retained matching old paths must block, never adopt.
	next := testTransport("speaker-two", routes)
	if err := b.ensure(ctx, c, func(context.Context, config) (*transport, error) { return next, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.exporter.Reconcile(ctx); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("retained old-incarnation path adopted: %v", err)
	}
	if len(routes.deleted) != 0 {
		t.Fatal("old UUID withdrawn through new speaker incarnation")
	}
	// Actual new-speaker absence allows receipt recovery, not UUID transfer.
	routes.paths = nil
	result, err = b.exporter.Reconcile(ctx)
	if err != nil || len(result.Applied) != 1 || result.Applied[0].PathUUID == oldUUID {
		t.Fatalf("fresh export receipt not established: %+v %v", result, err)
	}
	next.source.(*testRemoteSource).complete = false
	for range 2 {
		snapshot, err := b.reader.Poll(ctx)
		if err == nil || snapshot.Authoritative {
			t.Fatal("incomplete post-rebind RIB accepted")
		}
	}
	next.source.(*testRemoteSource).complete = true
	snapshot, err := b.reader.Poll(ctx)
	if err != nil || snapshot.Authoritative {
		t.Fatalf("first fresh complete observation became authoritative: %+v %v", snapshot, err)
	}
	snapshot, err = b.reader.Poll(ctx)
	if err != nil || !snapshot.Authoritative {
		t.Fatalf("second fresh complete observation not authoritative: %+v %v", snapshot, err)
	}
}

func TestProjectorOnlyRestartRetainsExactAcknowledgedUUID(t *testing.T) {
	c, domain, b, routes, _, _ := bindingFixture(t)
	ctx := context.Background()
	before, err := b.exporter.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.recovery.Close(); err != nil {
		t.Fatal(err)
	}
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{StateDirectory: filepath.Join(c.State, "export"),
		Domain: domain}, routes, routes, b.current.guard)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	b.recovery = recovery
	b.exporter = b.exporterFor(b.current)
	after, err := b.exporter.Reconcile(ctx)
	if err != nil || !reflect.DeepEqual(before.Applied, after.Applied) || len(routes.deleted) != 0 {
		t.Fatalf("projector restart replaced UUID: %+v %+v %v", before, after, err)
	}
}

func TestCombinedRestartUsesOriginalJournalAndNewSpeaker(t *testing.T) {
	c, domain, b, routes, observer, _ := bindingFixture(t)
	ctx := context.Background()
	before, err := b.exporter.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	b.recovery.Close()
	// Model the independent combined startup, not the live-projector rebind path.
	replacementRoutes := &testRoutes{}
	next := testTransport("combined-new-speaker", replacementRoutes)
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{StateDirectory: filepath.Join(c.State, "export"),
		Domain: domain}, replacementRoutes, replacementRoutes, next.guard)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	restarted := &speakerBinding{current: next, recovery: recovery, observer: observer}
	defer restarted.Close()
	restarted.exporter = restarted.exporterFor(next)
	result, err := restarted.exporter.Reconcile(ctx)
	if err != nil || len(result.Applied) != 1 || result.Applied[0].PathUUID == before.Applied[0].PathUUID {
		t.Fatalf("combined startup transferred old UUID authority: %+v %v", result, err)
	}
	if len(routes.deleted) != 0 {
		t.Fatal("combined startup deleted old-incarnation path")
	}
	observer.endpoints = nil
	drained, err := restarted.exporter.Reconcile(ctx)
	if err != nil || len(drained.Applied) != 0 || !reflect.DeepEqual(replacementRoutes.deleted, []string{result.Applied[0].PathUUID}) {
		t.Fatalf("combined recovery exact withdrawal failed: %+v %v", drained, err)
	}
}

func TestUnacknowledgedAddPathSurvivesRebindAsBlocker(t *testing.T) {
	c, _, b, routes, _, _ := bindingFixture(t)
	ctx := context.Background()
	routes.addErr = errors.New("AddPath acknowledgement lost")
	if _, err := b.exporter.Reconcile(ctx); err == nil || len(routes.paths) != 1 {
		t.Fatal("test did not retain unacknowledged path")
	}
	b.current.guard.(*testGuard).err = errors.New("speaker unavailable")
	next := testTransport("different-incarnation", routes)
	if err := b.ensure(ctx, c, func(context.Context, config) (*transport, error) { return next, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.exporter.Reconcile(ctx); !errors.Is(err, ownership.ErrBlocked) {
		t.Fatalf("ambiguous AddPath adopted: %v", err)
	}
	if len(routes.deleted) != 0 || len(routes.paths) != 1 {
		t.Fatal("ambiguous path damaged")
	}
}

func TestEndpointLifecycleWithdrawsOnlyOldUUIDBeforeNewGeneration(t *testing.T) {
	c, _, b, routes, _, ledger := bindingFixture(t)
	b.observer = observerFunc(func(ctx context.Context) ([]evpn.LocalEndpoint, error) {
		return ledger.desired(ctx, c, func(_ context.Context, next config) ([]evpn.LocalEndpoint, error) {
			return []evpn.LocalEndpoint{next.Endpoint}, nil
		})
	})
	b.exporter = b.exporterFor(b.current)
	ctx := context.Background()
	initial, err := b.exporter.Reconcile(ctx)
	if err != nil || len(initial.Applied) != 1 {
		t.Fatalf("initial generation: %+v %v", initial, err)
	}
	foreign := c.Endpoint
	foreign.EndpointID, foreign.Incarnation = uuid.NewString(), uuid.NewString()
	foreign.MACAddress, foreign.IPAddresses = "02:00:00:02:ff:01", []string{"10.100.2.199"}
	wire, err := evpn.BuildType2Route(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignUUID, err := routes.Add(ctx, wire)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, filepath.Join(c.State, "endpoint.json"), endpointRequest{1, c.Endpoint.Incarnation, true})
	drained, err := b.exporter.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.acknowledge(drained, err); err != nil {
		t.Fatal(err)
	}
	if ledger.state.Phase != "retired" || !reflect.DeepEqual(routes.deleted, []string{initial.Applied[0].PathUUID}) ||
		len(routes.paths) != 1 || routes.paths[0].PathUUID != foreignUUID {
		t.Fatal("retirement did not preserve foreign route and withdraw only acknowledged UUID")
	}
	publish(t, filepath.Join(c.State, "endpoint.json"), endpointRequest{2, uuid.NewString(), false})
	recreated, err := b.exporter.Reconcile(ctx)
	if err != nil || len(recreated.Applied) != 1 || recreated.Applied[0].PathUUID == initial.Applied[0].PathUUID ||
		recreated.Applied[0].Endpoint.Incarnation == initial.Applied[0].Endpoint.Incarnation ||
		len(routes.paths) != 2 {
		t.Fatalf("new endpoint generation not independently owned: %+v %v", recreated, err)
	}
}

type observerFunc func(context.Context) ([]evpn.LocalEndpoint, error)

func (f observerFunc) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	return f(ctx)
}

func TestAttestedGateRechecksAfterReadiness(t *testing.T) {
	g := &testGuard{id: "peer"}
	gate := attestedGate{guard: g, readiness: readinessFunc(func(context.Context) error {
		g.err = errors.New("peer replaced during readiness")
		return nil
	})}
	requireError(t, gate.Check(context.Background()), "peer replaced")
}

type readinessFunc func(context.Context) error

func (f readinessFunc) Check(ctx context.Context) error { return f(ctx) }
