package nodeagent

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestCNIContinuityMultusMissingNodeClaim(t *testing.T) {
	s, c, _, w, _, mgr, req := continuityFixture(t, false)
	w.nodeName = "node-a"
	req.K8S.NodeName = ""
	before := proto.Clone(req)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	created, err := s.Add(context.Background(), req)
	if err != nil || !created.Success {
		t.Fatalf("Multus-shaped ADD without K8S_NODE_NAME failed: %v", err)
	}
	if !proto.Equal(req, before) {
		t.Fatal("ADD mutated the caller's Kubernetes claims")
	}
	rec, present := s.store.get(endpointKey(req.Identity))
	if !present || rec.endpointSpec.Workload.NodeName != "node-a" {
		t.Fatal("verified trusted node scope not persisted")
	}
	replayed, err := s.Add(context.Background(), req)
	if err != nil || !replayed.Outcome.IdempotentReplay || mgr.ensureCount != 1 {
		t.Fatalf("missing-node replay mutated durable endpoint: %v", err)
	}
	if !proto.Equal(req, before) {
		t.Fatal("replay mutated caller")
	}
}

func TestCNIContinuityMultusMissingNodeFailsClosed(t *testing.T) {
	for _, mode := range []string{"unknown-pod", "deleted-pod", "wrong-explicit-node", "wrong-observed-node", "missing-pod-name", "missing-trusted-node"} {
		t.Run(mode, func(t *testing.T) {
			s, c, j, w, _, mgr, req := continuityFixture(t, false)
			req.K8S.NodeName = ""
			w.nodeName = "node-a"
			switch mode {
			case "unknown-pod":
				w.err = errors.New("Pod identity unavailable")
			case "deleted-pod":
				w.deleted = true
			case "wrong-explicit-node":
				req.K8S.NodeName = "foreign-node"
			case "wrong-observed-node":
				w.nodeName = "foreign-node"
			case "missing-pod-name":
				req.K8S.PodName = ""
			case "missing-trusted-node":
				c.nodeName = ""
			}
			before := proto.Clone(req)
			if err := s.recoverCNI(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Add(context.Background(), req); err == nil {
				t.Fatal("unverified missing-node ADD accepted")
			}
			if mgr.ensureCount != 0 || j.pending != nil || len(s.store.records) != 0 {
				t.Fatal("unverified workload gained durable intent or resources")
			}
			if !proto.Equal(req, before) {
				t.Fatal("failed ADD mutated caller's claims")
			}
		})
	}
}
