package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"syscall"
	"testing"
	"time"

	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/vishvananda/netlink"
)

func TestCNIOrphanRecoveryHostAbsenceEvidence(t *testing.T) {
	s, c, _, _, _, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	key := endpointKey(req.Identity)
	ack := c.state.Endpoints[key]
	for _, scenario := range []struct {
		name   string
		attrs  netlink.LinkAttrs
		absent bool
	}{
		{"stable", netlink.LinkAttrs{Name: ack.Spec.OVSInterfaceName, Index: ack.Receipt.HostIfIndex, Alias: ownerAliasForKey(key)}, false},
		{"name reused", netlink.LinkAttrs{Name: ack.Spec.OVSInterfaceName, Index: 200}, false},
		{"index reused or original renamed", netlink.LinkAttrs{Name: "unrelated", Index: ack.Receipt.HostIfIndex}, false},
		{"alias still visible", netlink.LinkAttrs{Name: "unrelated", Index: 200, Alias: ownerAliasForKey(key)}, false},
		{"old host absent", netlink.LinkAttrs{Name: "unrelated", Index: 200}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := cniOrphanHostAbsentIn([]netlink.Link{&netlink.Veth{LinkAttrs: scenario.attrs}}, ack)
			if (err == nil) != scenario.absent {
				t.Fatalf("host absence=%v, want %v", err == nil, scenario.absent)
			}
		})
	}
}

func TestCNIOrphanRecoveryNeverDeletesHostLink(t *testing.T) {
	for _, scenario := range []struct{ name, hostObservation string }{
		{"absent with workload netns unavailable", ""},
		{"original matching veth remains", "host veth present"},
		{"replacement veth by name", "host veth present"},
		{"replacement reuses ifindex", "host veth present"},
		{"replacement after lookup", "host lookup inconclusive"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, c, j, w, o, _, req := continuityFixture(t, true)
			ctx := context.Background()
			if err := s.recoverCNI(ctx, c); err != nil {
				t.Fatal(err)
			}
			w.deleted = true
			want := "deleted"
			if scenario.hostObservation != "" {
				o.hostAbsentErr = errors.New(scenario.hostObservation)
				want = cniOrphanHostUnresolved
			}
			key := endpointKey(req.Identity)
			for i := 0; i < 2; i++ {
				if err := s.verifyPendingCNI(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if o.deletes != 1 || o.present || j.pending != nil || s.network.(*fakeNetworkBackend).deleteCnt != 0 {
				t.Fatal("database cleanup must finish once, with no host delete")
			}
			if _, present := s.store.get(key); present {
				t.Fatal("retired endpoint still published")
			}
			restarted, recovered, _, _, _, _, _ := continuityFixture(t, false)
			recovered.journal = j
			if err := restarted.recoverCNI(ctx, recovered); err != nil {
				t.Fatal(err)
			}
			if ack := recovered.state.Endpoints[key]; ack.State != want || (want == cniOrphanHostUnresolved && ack.Reason == "") {
				t.Fatalf("restart ACK state=%q reason=%q, want %q", ack.State, ack.Reason, want)
			}
			if want == cniOrphanHostUnresolved {
				if _, err := restarted.Del(ctx, &pb.CNIDelRequest{Identity: req.Identity}); err == nil {
					t.Fatal("DEL claimed host absence for unresolved ACK")
				}
			}
		})
	}
}

func TestCNIOrphanRecoveryRetriesStartupDependencies(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, true)
	ctx := context.Background()
	w.err = errors.New("Kubernetes API unavailable")
	if err := s.initializeCNI(ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyPendingCNI(ctx); err == nil || o.deletes != 0 || j.pending != nil || c.ready() == nil {
		t.Fatal("early Kubernetes error authorized cleanup or readiness")
	}
	w.err = nil
	w.deleted = true
	o.deleteErr = errors.New("OVN NB unavailable")
	if err := s.verifyPendingCNI(ctx); err == nil || j.pending == nil || c.ready() == nil {
		t.Fatal("OVN failure did not leave an intent and fence")
	}
	o.deleteErr = nil
	if err := s.verifyPendingCNI(ctx); err != nil || j.pending != nil || c.ready() != nil || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatalf("startup retry failed: %v", err)
	}
}

type selectiveOrphanWorkload struct{ retiredUID string }

func (w selectiveOrphanWorkload) Verify(_ context.Context, ref endpoint.WorkloadRef, _, _ string) (bool, error) {
	if ref.UID == w.retiredUID {
		return true, nil
	}
	return false, errors.New("unrelated Pod observation unavailable")
}

func TestCNIOrphanRecoveryReplaysTargetBeforeOtherACKs(t *testing.T) {
	s, c, j, _, o, _, req := continuityFixture(t, true)
	ctx := context.Background()
	var state cniACKState
	if err := json.Unmarshal(j.raw, &state); err != nil {
		t.Fatal(err)
	}
	key := endpointKey(req.Identity)
	target := state.Endpoints[key]
	otherReq := sampleAddRequest("net-h22|net1|uid-0", "container-0", "10.1.2.4/24")
	otherReq.Identity.WorkloadUid = "uid-0"
	otherReq.Runtime.ContainerId = "container-0"
	otherReq.K8S = &pb.K8SIdentity{PodUid: "uid-0", PodName: "pod-0", PodNamespace: "ns-a", NodeName: "node-a"}
	otherKey := endpointKey(otherReq.Identity)
	other := target
	other.Identity, other.Runtime, other.Attachment = otherReq.Identity, otherReq.Runtime, otherReq.Attachment
	other.Result = buildResultFromStaticConfig(other.Runtime, other.Attachment)
	var err error
	other.Spec, err = mapCNIAddRequestToEndpointSpec(otherReq)
	if err != nil {
		t.Fatal(err)
	}
	other.Spec.OVSInterfaceName = hostVethNameForKey(otherKey)
	state.Endpoints[otherKey] = other
	j.raw, _ = json.Marshal(state)
	if _, err := j.Begin("cni-orphan-del", target); err != nil {
		t.Fatal(err)
	}
	c.workload = selectiveOrphanWorkload{retiredUID: target.Spec.Workload.UID}
	o.present = false // Simulate already-removed database rows from a prior attempt.
	if err := s.initializeCNI(ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyPendingCNI(ctx); err == nil || j.pending != nil || o.deletes != 1 || c.state.Endpoints[key].State != "deleted" || c.state.Endpoints[otherKey].State != "active" {
		t.Fatalf("pending target was not replayed independently: %v", err)
	}
}

type cancellationOrphanWorkload struct{ entered chan struct{} }

func (w cancellationOrphanWorkload) Verify(ctx context.Context, _ endpoint.WorkloadRef, _, _ string) (bool, error) {
	close(w.entered)
	<-ctx.Done()
	return false, ctx.Err()
}

func TestCNIOrphanRecoveryWorkerStopsOnCancellation(t *testing.T) {
	s, c, _, _, o, _, _ := continuityFixture(t, true)
	w := cancellationOrphanWorkload{entered: make(chan struct{})}
	c.workload = w
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.initializeCNI(ctx, c, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.runCNIVerification(ctx) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered startup recovery")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery worker did not cancel")
	}
	if o.deletes != 0 {
		t.Fatal("cancellation authorized deletion")
	}
}

func TestCNIOrphanRecoveryWorkerRescansWithoutExporter(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.recoverCNI(ctx, c); err != nil {
		cancel()
		t.Fatal(err)
	}
	c.scanAfter = ""
	done := make(chan struct{})
	go func() { defer close(done); s.runCNIReconciliation(ctx, 10*time.Millisecond, 20*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	key := endpointKey(req.Identity)
	// Wait for one scan of the live Pod before retiring it without an RPC.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	retired := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("worker did not discover missed DEL")
		case <-tick.C:
			s.operations.Lock()
			if !retired && c.scanAfter == key {
				w.deleted, retired = true, true
			}
			clean := c.state.Endpoints[key].State == "deleted" && o.deletes == 1 && j.pending == nil
			s.operations.Unlock()
			if clean {
				return
			}
		}
	}
}

func TestCNIOrphanRecoveryWaitsForStartupTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := 0
	if err := WaitForCNIBackends(ctx, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return cniBackendStartupError(syscall.ECONNREFUSED)
		}
		return nil
	}); err != nil || attempts != 2 {
		t.Fatalf("transport recovery attempts=%d err=%v", attempts, err)
	}
	invalid := errors.New("invalid backend configuration")
	if err := WaitForCNIBackends(ctx, func(context.Context) error { return cniBackendStartupError(invalid) }); !errors.Is(err, invalid) {
		t.Fatalf("permanent configuration error was not returned: %v", err)
	}
}
