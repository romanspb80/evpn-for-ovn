package evpn

import (
	"context"
	"errors"
	"testing"
)

func TestEndpointExporterEvidencedRecovery(t *testing.T) {
	ctx := context.Background()
	for _, change := range []string{"unchanged", "delete", "new-incarnation", "observer-error", "rib-error", "process-exited", "missing-path"} {
		t.Run(change, func(t *testing.T) {
			first := sampleEndpoint()
			second := cloneLocalEndpoint(first)
			second.EndpointID, second.MACAddress = "second", "02:00:00:00:00:99"
			foreign := cloneLocalEndpoint(first)
			foreign.EndpointID, foreign.RouteDistinguisher = "foreign", "64512:32999"
			a := &memoryType2API{}
			route, err := BuildType2Route(foreign)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Add(ctx, route); err != nil {
				t.Fatal(err)
			}
			o := &exportTestObserver{desired: []LocalEndpoint{first, second}}
			g, continuity := &exportTestGate{}, &exportTestGate{}
			e := &EndpointExporter{Observer: o, Gate: g, Continuity: continuity, Source: a, Applier: a}
			before, err := e.Reconcile(ctx)
			if err != nil || len(before.Applied) != 2 {
				t.Fatalf("initial: %+v %v", before, err)
			}
			adds := a.adds
			for i := 0; i < 2; i++ {
				g.err = errors.New("temporary API error")
				if res, err := e.Reconcile(ctx); err == nil || !res.OwnershipUnverified || len(res.Applied) != 2 || a.deletes != 0 || a.adds != adds {
					t.Fatalf("outage mutated confirmed state: %+v %v", res, err)
				}
			}
			g.err = nil
			switch change {
			case "delete":
				o.desired = []LocalEndpoint{second}
			case "new-incarnation":
				first.Incarnation = "new-incarnation"
				o.desired = []LocalEndpoint{first, second}
			case "observer-error":
				o.err = errors.New("SB unavailable")
			case "rib-error":
				a.snapshotErr = errors.New("ListPath failed")
			case "process-exited":
				continuity.err = errors.New("pidfd exited")
			case "missing-path":
				a.paths = a.paths[:2]
			}
			res, err := e.Reconcile(ctx)
			if change == "observer-error" || change == "rib-error" || change == "process-exited" || change == "missing-path" {
				if err == nil || len(res.Applied) != 2 || a.deletes != 0 {
					t.Fatalf("unsafe recovery: %+v %v", res, err)
				}
				o.err, a.snapshotErr, continuity.err = nil, nil, nil
				if change == "process-exited" || change == "missing-path" {
					if _, err := e.Reconcile(ctx); err == nil {
						t.Fatal("permanent evidence loss must stay fenced")
					}
					return
				}
				res, err = e.Reconcile(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if change == "delete" {
				want = 1
			}
			if res.OwnershipUnverified || len(res.Applied) != want || len(a.paths) != want+1 {
				t.Fatalf("bad recovery: %+v paths=%d", res, len(a.paths))
			}
			if a.paths[0].PathUUID != "path-1" {
				t.Fatal("foreign path changed")
			}
			if change == "unchanged" && (a.adds != adds || a.deletes != 0 || res.Applied[0].PathUUID != before.Applied[0].PathUUID) {
				t.Fatal("duplicate on recovery")
			}
			if _, err := e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
