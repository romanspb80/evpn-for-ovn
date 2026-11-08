package evpn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type memoryType2API struct {
	paths       []Type2RIBPath
	addErr      error
	deleteErr   error
	snapshotErr error
	adds        int
	deletes     int
}

func (a *memoryType2API) Snapshot(context.Context) ([]Type2RIBPath, error) {
	return append([]Type2RIBPath(nil), a.paths...), a.snapshotErr
}

func (a *memoryType2API) Add(_ context.Context, route Type2Route) (string, error) {
	a.adds++
	if a.addErr != nil {
		return "", a.addErr
	}
	id := fmt.Sprintf("path-%d", a.adds)
	nlri := route.NLRI
	a.paths = append(a.paths, Type2RIBPath{
		PathUUID: id, Local: true, NLRI: &nlri, PathAttributes: route.PathAttributes,
	})
	return id, nil
}

func (a *memoryType2API) Delete(_ context.Context, owned OwnedType2Route) error {
	a.deletes++
	if a.deleteErr != nil {
		return a.deleteErr
	}
	for i, path := range a.paths {
		if path.PathUUID == owned.PathUUID {
			a.paths = append(a.paths[:i], a.paths[i+1:]...)
			return nil
		}
	}
	return errors.New("owned path not found")
}

func syncedReconciler(t *testing.T, api *memoryType2API) *RouteReconciler {
	t.Helper()
	r := NewRouteReconciler()
	if err := r.Resync(context.Background(), api, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

func planType2(t *testing.T, r *RouteReconciler, desired ...LocalEndpoint) []Type2Route {
	t.Helper()
	changes, err := r.Reconcile(desired)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func applyType2(t *testing.T, r *RouteReconciler, api Type2RouteApplier, changes []Type2Route) {
	t.Helper()
	for _, change := range changes {
		if err := r.Apply(context.Background(), change, api); err != nil {
			t.Fatal(err)
		}
	}
}

func TestType2ConfirmedApplicationAndRetry(t *testing.T) {
	ctx := context.Background()
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	ep := sampleEndpoint()
	api.addErr = errors.New("AddPath unavailable")
	add := planType2(t, r, ep)
	if err := r.Apply(ctx, add[0], api); !errors.Is(err, api.addErr) {
		t.Fatalf("expected AddPath error, got %v", err)
	}
	if len(r.AppliedRoutes()) != 0 {
		t.Fatal("failed AddPath was acknowledged")
	}
	api.addErr = nil
	applyType2(t, r, api, planType2(t, r, ep))
	if len(r.AppliedRoutes()) != 1 || len(planType2(t, r, ep, ep)) != 0 {
		t.Fatal("successful add/duplicate convergence failed")
	}
	api.deleteErr = errors.New("DeletePath unavailable")
	del := planType2(t, r)
	if err := r.Apply(ctx, del[0], api); !errors.Is(err, api.deleteErr) {
		t.Fatalf("expected DeletePath error, got %v", err)
	}
	if len(r.AppliedRoutes()) != 1 {
		t.Fatal("failed DeletePath was acknowledged")
	}
	api.deleteErr = nil
	applyType2(t, r, api, planType2(t, r))
	if len(r.AppliedRoutes()) != 0 || len(api.paths) != 0 {
		t.Fatal("retry left an owned route")
	}
}

func TestType2ReplacementFencing(t *testing.T) {
	for _, change := range []string{"rd", "incarnation", "attributes", "vni", "vtep"} {
		t.Run(change, func(t *testing.T) {
			api := &memoryType2API{}
			r := syncedReconciler(t, api)
			old := sampleEndpoint()
			applyType2(t, r, api, planType2(t, r, old))
			next := old
			switch change {
			case "rd":
				next.RouteDistinguisher = "65000:2000"
			case "incarnation":
				next.Incarnation = "inc-2"
			case "attributes":
				next.RouteTargets = []string{"65000:200"}
			case "vni":
				next.VNI++
			case "vtep":
				next.LocalVTEP = "192.0.2.20"
			}
			deltas := planType2(t, r, next)
			if len(deltas) != 2 || deltas[0].Action != RouteWithdraw || deltas[1].Action != RouteAdvertise {
				t.Fatalf("expected ordered replacement, got %v", deltas)
			}
			if change != "rd" {
				if err := r.Apply(context.Background(), deltas[1], api); !errors.Is(err, ErrRouteOrder) {
					t.Fatalf("out-of-order replacement not rejected: %v", err)
				}
			}
			applyType2(t, r, api, deltas)
			calls := api.deletes
			if err := r.Apply(context.Background(), deltas[0], api); !errors.Is(err, ErrStaleRouteOperation) {
				t.Fatalf("delayed withdrawal not rejected: %v", err)
			}
			if api.deletes != calls || len(api.paths) != 1 || !r.AppliedRoutes()[0].Endpoint.Equivalent(next) {
				t.Fatal("delayed operation damaged replacement")
			}
		})
	}
}

func TestType2PlansAreFencedAndImmutable(t *testing.T) {
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	ep := sampleEndpoint()
	oldPlan := planType2(t, r, ep)
	newPlan := planType2(t, r, ep)
	if err := r.Apply(context.Background(), oldPlan[0], api); !errors.Is(err, ErrStaleRouteOperation) {
		t.Fatalf("old plan accepted: %v", err)
	}
	ep.IPAddresses[0] = "10.0.0.99"
	newPlan[0].Endpoint.RouteTargets[0] = "65000:999"
	newPlan[0].Action = RouteWithdraw
	newPlan[0].NLRI.MAC = "02:00:00:00:00:ff"
	applyType2(t, r, api, newPlan)
	applied := r.AppliedRoutes()
	if applied[0].Endpoint.RouteTargets[0] != "65000:100" || primaryIP(applied[0].Endpoint.IPAddresses) != "10.251.1.11" {
		t.Fatal("caller mutated planned/applied state")
	}
	applied[0].Endpoint.RouteTargets[0] = "65000:998"
	if r.AppliedRoutes()[0].Endpoint.RouteTargets[0] != "65000:100" {
		t.Fatal("applied snapshot leaked mutable state")
	}
}

func TestType2ResyncOwnership(t *testing.T) {
	ctx := context.Background()
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	ep := sampleEndpoint()
	applyType2(t, r, api, planType2(t, r, ep))
	claims := r.AppliedRoutes()
	restarted := NewRouteReconciler()
	if err := restarted.Resync(ctx, api, claims); err != nil {
		t.Fatal(err)
	}
	if len(planType2(t, restarted, ep)) != 0 {
		t.Fatal("existing owned RIB route was re-advertised")
	}
	applyType2(t, restarted, api, planType2(t, restarted))
	if len(api.paths) != 0 {
		t.Fatal("adopted route was not withdrawn")
	}

	r = restarted
	applyType2(t, r, api, planType2(t, r, ep))
	unknown := NewRouteReconciler()
	if err := unknown.Resync(ctx, api, nil); err != nil {
		t.Fatal(err)
	}
	if len(planType2(t, unknown)) != 0 {
		t.Fatal("unknown RIB route scheduled for deletion")
	}
	if _, err := unknown.Reconcile([]LocalEndpoint{ep}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("foreign local wire route not protected: %v", err)
	}
	claims = r.AppliedRoutes()
	claims[0].PathUUID = "foreign-claim"
	if err := unknown.Resync(ctx, api, claims); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("unverified claim accepted: %v", err)
	}
}

func TestType2ResyncUnavailableFailsClosed(t *testing.T) {
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	ep := sampleEndpoint()
	applyType2(t, r, api, planType2(t, r, ep))
	claims := r.AppliedRoutes()
	api.snapshotErr = errors.New("RIB unavailable")
	if err := r.Resync(context.Background(), api, claims); !errors.Is(err, api.snapshotErr) {
		t.Fatalf("expected RIB error, got %v", err)
	}
	if len(r.AppliedRoutes()) != 1 {
		t.Fatal("failed resync invented an empty applied state")
	}
	del := planType2(t, r)
	if err := r.Apply(context.Background(), del[0], api); !errors.Is(err, ErrRouteResyncRequired) {
		t.Fatalf("write permitted after failed resync: %v", err)
	}
	if api.deletes != 0 {
		t.Fatal("RIB failure caused delete")
	}
}

func TestType2ConflictingDesiredOwners(t *testing.T) {
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	ep := sampleEndpoint()
	next := ep
	next.Incarnation = "inc-2"
	if _, err := r.Reconcile([]LocalEndpoint{ep, next}); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("conflicting desired owners not rejected: %v", err)
	}
	if api.adds != 0 {
		t.Fatal("conflicting desired state reached adapter")
	}
}

func TestType2ConcurrentDuplicateApplication(t *testing.T) {
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	add := planType2(t, r, sampleEndpoint())[0]
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- r.Apply(context.Background(), add, api)
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrStaleRouteOperation) {
			t.Fatal(err)
		}
	}
	if success != 1 || api.adds != 1 {
		t.Fatalf("duplicate application: successes=%d adds=%d", success, api.adds)
	}
}

func TestType2WireIdentityIsNotEndpointIdentity(t *testing.T) {
	ep := sampleEndpoint()
	first, err := BuildType2Route(ep)
	if err != nil {
		t.Fatal(err)
	}
	next := ep
	next.Incarnation = "inc-2"
	next.VNI++
	next.LocalVTEP = "192.0.2.20"
	second, err := BuildType2Route(next)
	if err != nil {
		t.Fatal(err)
	}
	if !first.NLRI.Equal(second.NLRI) || first.NLRI.Key() != second.NLRI.Key() ||
		sameType2NLRIValue(first.NLRI, second.NLRI) || ep.RouteKey() == next.RouteKey() {
		t.Fatal("endpoint generation/attributes confused with RFC7432 wire prefix")
	}
	next.RouteDistinguisher = "65000:2000"
	second, err = BuildType2Route(next)
	if err != nil || first.NLRI.Equal(second.NLRI) {
		t.Fatalf("RD not part of wire identity: %v", err)
	}
}

func TestType2SupersededWithdrawalNeverReachesAdapter(t *testing.T) {
	api := &memoryType2API{}
	r := syncedReconciler(t, api)
	old := sampleEndpoint()
	applyType2(t, r, api, planType2(t, r, old))
	stale := planType2(t, r)[0]
	next := old
	next.Incarnation = "inc-2"
	applyType2(t, r, api, planType2(t, r, next))
	deletes := api.deletes
	if err := r.Apply(context.Background(), stale, api); !errors.Is(err, ErrStaleRouteOperation) {
		t.Fatalf("superseded DEL executed: %v", err)
	}
	if api.deletes != deletes || !r.AppliedRoutes()[0].Endpoint.Equivalent(next) {
		t.Fatal("superseded DEL reached adapter or removed current owner")
	}
}

func TestType2InitialWritesAndInvalidDesiredFailClosed(t *testing.T) {
	api := &memoryType2API{}
	r := NewRouteReconciler()
	delta := planType2(t, r, sampleEndpoint())[0]
	if err := r.Apply(context.Background(), delta, api); !errors.Is(err, ErrRouteResyncRequired) {
		t.Fatalf("initial write without RIB read: %v", err)
	}
	if api.adds != 0 {
		t.Fatal("unsynchronized write reached API")
	}
	r = syncedReconciler(t, api)
	delta = planType2(t, r, sampleEndpoint())[0]
	invalid := sampleEndpoint()
	invalid.RouteDistinguisher = "invalid"
	if _, err := r.Reconcile([]LocalEndpoint{invalid}); err == nil {
		t.Fatal("invalid desired endpoint accepted")
	}
	if err := r.Apply(context.Background(), delta, api); !errors.Is(err, ErrStaleRouteOperation) {
		t.Fatalf("invalid new snapshot left an old plan executable: %v", err)
	}
}

func TestType2ResyncRejectsPayloadMismatchAndRemoteClaims(t *testing.T) {
	for _, field := range []string{"rd", "vni", "rt", "next-hop", "remote"} {
		t.Run(field, func(t *testing.T) {
			api := &memoryType2API{}
			r := syncedReconciler(t, api)
			ep := sampleEndpoint()
			applyType2(t, r, api, planType2(t, r, ep))
			receipts := r.AppliedRoutes()
			switch field {
			case "rd":
				receipts[0].Endpoint.RouteDistinguisher = "65000:2000"
			case "vni":
				receipts[0].Endpoint.VNI++
			case "rt":
				receipts[0].Endpoint.RouteTargets = []string{"65000:200"}
			case "next-hop":
				receipts[0].Endpoint.LocalVTEP = "192.0.2.20"
			case "remote":
				api.paths[0].Local = false
			}
			restarted := NewRouteReconciler()
			if err := restarted.Resync(context.Background(), api, receipts); !errors.Is(err, ErrRouteOwnership) {
				t.Fatalf("unverified ownership accepted: %v", err)
			}
			if len(restarted.AppliedRoutes()) != 0 || api.deletes != 0 {
				t.Fatal("failed adoption invented applied state or deleted a path")
			}
		})
	}
}
