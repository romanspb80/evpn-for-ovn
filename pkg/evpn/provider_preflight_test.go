package evpn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// This opt-in test writes only its reserved RD range on a disposable speaker.
// Never point it at a shared speaker.
func TestDisposableProviderType2(t *testing.T) {
	target := os.Getenv("EVPN_PROVIDER_TARGET")
	if target == "" {
		t.Skip("set EVPN_PROVIDER_TARGET only for an isolated disposable speaker")
	}
	if os.Getenv("EVPN_PROVIDER_DISPOSABLE") != "yes" {
		t.Fatal("EVPN_PROVIDER_DISPOSABLE=yes is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ctx, state) {
			t.Fatal("disposable speaker gRPC unavailable")
		}
	}
	client := api.NewGoBgpServiceClient(conn)
	global, err := client.GetBgp(ctx, &api.GetBgpRequest{})
	if err != nil {
		t.Fatalf("GetBgp using repository bindings: %v", err)
	}
	t.Logf("actual global: %v", global)
	adapter := &GoBGPType2Adapter{Client: client}
	initial, err := adapter.Snapshot(ctx)
	if err != nil {
		t.Fatalf("ListPath using repository bindings: %v", err)
	}
	ep := sampleEndpoint()
	ep.NetworkID, ep.EndpointID, ep.Incarnation = "provider-preflight", "endpoint-a", "inc-1"
	ep.RouteDistinguisher, ep.RouteTargets = "64512:32001", []string{"64512:32000"}
	ep.MACAddress, ep.IPAddresses = "02:00:00:32:00:01", []string{"10.253.32.1"}
	ep.LocalVTEP, ep.VNI = "192.0.2.32", 32000
	foreign := ep
	foreign.EndpointID, foreign.RouteDistinguisher = "foreign-component", "64512:32999"
	var receipts []OwnedType2Route
	if stage := os.Getenv("EVPN_PROVIDER_STAGE"); stage != "" && stage != "lifecycle" {
		providerRestartStage(t, ctx, adapter, ep, foreign, stage)
		return
	}
	if len(initial) != 0 {
		t.Fatal("lifecycle test requires an empty disposable Type-2 RIB")
	}
	foreignID, err := adapter.Add(ctx, mustProviderRoute(t, foreign))
	if err != nil {
		t.Fatalf("AddPath foreign control route: %v", err)
	}
	receipts = append(receipts, OwnedType2Route{Endpoint: foreign, PathUUID: foreignID})
	defer func() {
		for _, receipt := range receipts {
			if err := adapter.Delete(ctx, receipt); err != nil {
				t.Errorf("exact UUID cleanup %s: %v", receipt.PathUUID, err)
			}
		}
	}()
	r := NewRouteReconciler()
	if err := r.Resync(ctx, adapter, nil); err != nil {
		t.Fatal(err)
	}
	faults := &failingType2Client{GoBgpServiceClient: client, addErr: errors.New("pre-send AddPath fault")}
	adapter.Client = faults
	add := planType2(t, r, ep)
	if err := r.Apply(ctx, add[0], adapter); !errors.Is(err, faults.addErr) || len(r.AppliedRoutes()) != 0 {
		t.Fatalf("failed AddPath advanced confirmed state: %v", err)
	}
	checkProviderRIB(t, ctx, adapter, foreign)
	faults.addErr = nil
	applyProviderPlan(t, ctx, r, adapter, add)
	receipts = append(receipts, r.AppliedRoutes()[0])
	t.Logf("AddPath UUID=%s", r.AppliedRoutes()[0].PathUUID)
	checkProviderRIB(t, ctx, adapter, ep, foreign)
	if len(planType2(t, r, ep, ep)) != 0 {
		t.Fatal("duplicate reconciliation generated writes")
	}
	if _, err := adapter.Add(ctx, mustProviderRoute(t, foreign)); !errors.Is(err, ErrRouteOwnership) {
		t.Fatalf("foreign route overwrite not refused: %v", err)
	}
	for _, attribute := range []string{"rd", "next-hop", "vni", "rt", "incarnation"} {
		next := cloneLocalEndpoint(ep)
		switch attribute {
		case "rd":
			next.RouteDistinguisher = "64512:32002"
		case "next-hop":
			next.LocalVTEP = "192.0.2.33"
		case "vni":
			next.VNI++
		case "rt":
			next.RouteTargets = []string{"64512:32001"}
		case "incarnation":
			next.Incarnation = "inc-2"
		}
		deltas := planType2(t, r, next)
		if len(deltas) != 2 {
			t.Fatalf("%s: expected replacement, got %v", attribute, deltas)
		}
		applyProviderPlan(t, ctx, r, adapter, deltas)
		receipts = append(receipts[:1], r.AppliedRoutes()[0])
		if err := r.Apply(ctx, deltas[0], adapter); !errors.Is(err, ErrStaleRouteOperation) {
			t.Fatalf("%s: stale withdrawal reached API: %v", attribute, err)
		}
		checkProviderRIB(t, ctx, adapter, next, foreign)
		t.Logf("%s replacement verified; current UUID=%s", attribute, r.AppliedRoutes()[0].PathUUID)
		ep = next
	}
	restarted := NewRouteReconciler()
	err = restarted.Resync(ctx, adapter, r.AppliedRoutes())
	if err != nil {
		if !errors.Is(err, ErrRouteOwnership) {
			t.Fatalf("client restart: %v", err)
		}
		t.Logf("client restart adoption FAIL-CLOSED: %v", err)
	} else {
		t.Log("client restart adoption confirmed from real UUID-bearing RIB")
	}
	badCtx, badCancel := context.WithCancel(ctx)
	badCancel()
	deletion := planType2(t, r)
	if err := r.Apply(badCtx, deletion[0], adapter); err == nil || len(r.AppliedRoutes()) != 1 {
		t.Fatal("canceled delete changed confirmed state")
	}
	faults.deleteErr = errors.New("pre-send DeletePath fault")
	if err := r.Apply(ctx, deletion[0], adapter); !errors.Is(err, faults.deleteErr) || len(r.AppliedRoutes()) != 1 {
		t.Fatalf("failed DeletePath advanced confirmed state: %v", err)
	}
	checkProviderRIB(t, ctx, adapter, ep, foreign)
	faults.deleteErr = nil
	applyProviderPlan(t, ctx, r, adapter, planType2(t, r))
	paths, err := adapter.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(initial)+1 || !providerHasRoute(paths, foreign) {
		t.Fatalf("foreign route damaged during withdrawal: %v", paths)
	}
	receipts = receipts[:1]
	t.Log("exact UUID withdrawal preserved foreign route")
	t.Log("injected pre-send AddPath/DeletePath failures retried against actual speaker")
	providerRawReplacement(t, ctx, adapter, ep)
}

func applyProviderPlan(t *testing.T, ctx context.Context, r *RouteReconciler, adapter *GoBGPType2Adapter, deltas []Type2Route) {
	t.Helper()
	for _, delta := range deltas {
		if err := r.Apply(ctx, delta, adapter); err != nil {
			t.Fatal(err)
		}
	}
}

func providerRawReplacement(t *testing.T, ctx context.Context, adapter *GoBGPType2Adapter, ep LocalEndpoint) {
	t.Helper()
	ep.RouteDistinguisher = "64512:32003"
	add := func(endpoint LocalEndpoint) []byte {
		route := mustProviderRoute(t, endpoint)
		path, err := apiutil.NewPath(bgp.RF_EVPN, route.NLRI, false, route.PathAttributes, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		response, err := adapter.Client.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		id, err := uuid.FromBytes(response.GetUuid())
		if err != nil || id == uuid.Nil {
			t.Fatalf("raw AddPath missing UUID: %v", err)
		}
		return response.Uuid
	}
	oldID := add(ep)
	duplicateID := add(ep)
	t.Logf("raw duplicate AddPath UUID old=%x duplicate=%x", oldID, duplicateID)
	paths, err := adapter.Snapshot(ctx)
	if err != nil || !providerHasRoute(paths, ep) || len(paths) != 2 {
		t.Fatalf("raw duplicate created unexpected paths: %v %v", paths, err)
	}
	ep.LocalVTEP = "192.0.2.34"
	currentID := add(ep)
	oldDelete := &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: oldID}
	_, err = adapter.Client.DeletePath(ctx, oldDelete)
	t.Logf("raw old UUID DeletePath after replacement: %v", err)
	paths, err = adapter.Snapshot(ctx)
	if err != nil || !providerHasRoute(paths, ep) || len(paths) != 2 {
		t.Fatalf("raw duplicate/replacement or old UUID fencing incorrect: %v %v", paths, err)
	}
	if _, err := adapter.Client.DeletePath(ctx, &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: currentID}); err != nil {
		t.Fatal(err)
	}
	paths, err = adapter.Snapshot(ctx)
	if err != nil || providerHasRoute(paths, ep) || len(paths) != 1 {
		t.Fatalf("raw exact UUID delete failed: %v %v", paths, err)
	}
}

func mustProviderRoute(t *testing.T, ep LocalEndpoint) Type2Route {
	t.Helper()
	route, err := BuildType2Route(ep)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func providerHasRoute(paths []Type2RIBPath, ep LocalEndpoint) bool {
	for _, path := range paths {
		if path.Local && ribPathMatchesEndpoint(path, ep) {
			return true
		}
	}
	return false
}

func checkProviderRIB(t *testing.T, ctx context.Context, adapter *GoBGPType2Adapter, endpoints ...LocalEndpoint) {
	t.Helper()
	paths, err := adapter.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(endpoints) {
		t.Fatalf("unexpected RIB path count: want %d, got %d", len(endpoints), len(paths))
	}
	for _, ep := range endpoints {
		count := 0
		for _, path := range paths {
			if ribPathMatchesEndpoint(path, ep) {
				count++
				mac := path.NLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
				t.Logf("RIB RD=%s etag=%d MAC=%s IP=%s labels=%v NH=%s attrs=%v UUID=%q",
					mac.RD, mac.ETag, mac.MacAddress, mac.IPAddress, mac.Labels, pathNextHop(path.PathAttributes), path.PathAttributes, path.PathUUID)
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly one correct path for %s, got %d", ep.EndpointID, count)
		}
	}
}

func providerRestartStage(t *testing.T, ctx context.Context, adapter *GoBGPType2Adapter, ep, foreign LocalEndpoint, stage string) {
	t.Helper()
	dir := os.Getenv("EVPN_PROVIDER_EVIDENCE")
	if dir == "" {
		t.Fatal("restart observations require EVPN_PROVIDER_EVIDENCE")
	}
	file := filepath.Join(dir, "owned-receipts.json")
	if stage == "seed" || stage == "watch-speaker" {
		r := NewRouteReconciler()
		if err := r.Resync(ctx, adapter, nil); err != nil {
			t.Fatal(err)
		}
		applyProviderPlan(t, ctx, r, adapter, planType2(t, r, ep, foreign))
		value, err := json.MarshalIndent(r.AppliedRoutes(), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, value, 0600); err != nil {
			t.Fatal(err)
		}
		checkProviderRIB(t, ctx, adapter, ep, foreign)
		if stage == "watch-speaker" {
			withdrawals := planType2(t, r)
			if err := os.WriteFile(filepath.Join(dir, "client-watching"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			for {
				if _, err := os.Stat(filepath.Join(dir, "check-restart")); err == nil {
					break
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
			paths, err := adapter.Snapshot(ctx)
			if err != nil || len(paths) != 0 || len(r.AppliedRoutes()) != 2 {
				t.Fatalf("live client restart observation: rib=%v applied=%v err=%v", paths, r.AppliedRoutes(), err)
			}
			if err := r.Resync(ctx, adapter, r.AppliedRoutes()); !errors.Is(err, ErrRouteOwnership) {
				t.Fatalf("speaker restart did not fail closed: %v", err)
			}
			if err := r.Apply(ctx, withdrawals[0], adapter); !errors.Is(err, ErrRouteResyncRequired) {
				t.Fatalf("stale writes not disabled after failed resync: %v", err)
			}
			t.Log("client remained running across speaker process restart; actual RIB empty; resync rejected old receipts and disabled writes")
		}
		return
	}
	value, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []OwnedType2Route
	if err := json.Unmarshal(value, &receipts); err != nil {
		t.Fatal(err)
	}
	switch stage {
	case "preserved":
		checkProviderRIB(t, ctx, adapter, ep, foreign)
		r := NewRouteReconciler()
		if err := r.Resync(ctx, adapter, receipts); err != nil {
			if !errors.Is(err, ErrRouteOwnership) {
				t.Fatal(err)
			}
			t.Logf("new client process: safe adoption refused: %v", err)
		} else {
			t.Log("new client process: actual RIB UUID adoption verified")
		}
	case "speaker-restarted":
		paths, err := adapter.Snapshot(ctx)
		if err != nil || len(paths) != 0 {
			t.Fatalf("speaker restart did not clear dynamic paths: %v %v", paths, err)
		}
		r := NewRouteReconciler()
		if err := r.Resync(ctx, adapter, receipts); !errors.Is(err, ErrRouteOwnership) {
			t.Fatalf("old receipts after speaker restart not rejected: %v", err)
		}
		t.Log("speaker restart: actual RIB empty; stale receipts rejected")
	case "cleanup":
		for _, receipt := range receipts {
			if err := adapter.Delete(ctx, receipt); err != nil {
				t.Fatal(err)
			}
		}
		paths, err := adapter.Snapshot(ctx)
		if err != nil || len(paths) != 0 {
			t.Fatalf("test RIB not empty after cleanup: %v %v", paths, err)
		}
	default:
		t.Fatalf("unknown stage %q", stage)
	}
}
