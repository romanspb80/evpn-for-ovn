package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type admissionTestWorkload struct{ *memoryCNIWorkload }

type delayedCNIObserver struct {
	*memoryCNIObserver
	unready int
}

func (o *delayedCNIObserver) Observe(ctx context.Context, rec *endpointRecord) (cniReceipt, bool, error) {
	if o.unready > 0 {
		o.unready--
		return cniReceipt{}, false, errCNIResourceUnready
	}
	return o.memoryCNIObserver.Observe(ctx, rec)
}

func TestCNICreatedOfportReadback(t *testing.T) {
	o := &delayedCNIObserver{memoryCNIObserver: &memoryCNIObserver{present: true}, unready: 1}
	c := &cniContinuity{observer: o}
	if _, absent, err := c.observeCreated(context.Background(), &endpointRecord{}); err != nil || absent {
		t.Fatalf("delayed assigned ofport: absent=%v err=%v", absent, err)
	}
	o.unready = 100
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, absent, err := c.observeCreated(ctx, &endpointRecord{}); err == nil || absent {
		t.Fatal("unavailable ofport accepted as successful or absent")
	}
	o.unready, o.observeErr = 0, errors.New("foreign attachment")
	if _, _, err := c.observeCreated(context.Background(), &endpointRecord{}); err == nil {
		t.Fatal("conflicting attachment accepted")
	}
}

func (w admissionTestWorkload) VerifyPod(context.Context, endpoint.WorkloadRef) (bool, error) {
	return w.deleted, nil
}

type admissionTestAuthority struct {
	err      error
	evidence CNIAdmissionEvidence
	calls    int
}

func (a *admissionTestAuthority) Admit(_ context.Context, req *pb.CNIAddRequest) (CNIAdmissionEvidence, error) {
	a.calls++
	a.evidence.ContainerID, a.evidence.PodUID = req.Runtime.ContainerId, req.Identity.WorkloadUid
	return a.evidence, a.err
}

func enableTestAdmission(t *testing.T, s *CNIServer, c *cniContinuity, w *memoryCNIWorkload, o *memoryCNIObserver) {
	t.Helper()
	o.receipt.NetNSDevice, o.receipt.NetNSInode = 10, 20
	processes := []CNIProcessEvidence{
		{PID: 10, StartTicks: "100", ExecutableSHA256: strings.Repeat("a", 64), BootID: "12345678-1234-1234-1234-123456789abc"},
		{PID: 9, StartTicks: "90", ExecutableSHA256: strings.Repeat("b", 64), BootID: "12345678-1234-1234-1234-123456789abc"},
		{PID: 8, StartTicks: "80", ExecutableSHA256: strings.Repeat("c", 64), BootID: "12345678-1234-1234-1234-123456789abc"},
	}
	c.authority = &admissionTestAuthority{evidence: CNIAdmissionEvidence{Caller: processes[0], Processes: processes, NetNSDevice: 10, NetNSInode: 20}}
	c.workload = admissionTestWorkload{w}
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestCNITwoStageActivation(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, false)
	enableTestAdmission(t, s, c, w, o)
	w.err = errCNIRuntimePending
	if _, err := s.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	key := endpointKey(req.Identity)
	if c.state.Endpoints[key].State != "pending_creation" || j.pending != nil {
		t.Fatal("ADD did not acknowledge pending resources")
	}
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{}); err == nil {
		t.Fatal("pending creation was allowed to export")
	}
	if err := s.verifyPendingCNI(context.Background()); !errors.Is(err, errCNIRuntimePending) {
		t.Fatalf("unpublished runtime: %v", err)
	}
	w.err = nil
	if err := s.verifyPendingCNI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.state.Endpoints[key].State != "active" {
		t.Fatal("independently verified sandbox did not activate")
	}
	if err := s.ValidateContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCNICreationAuthorityFailureBeforeMutation(t *testing.T) {
	s, c, j, w, _, manager, req := continuityFixture(t, false)
	c.authority = &admissionTestAuthority{err: errors.New("unrelated caller")}
	c.workload = admissionTestWorkload{w}
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(context.Background(), req); err == nil {
		t.Fatal("unauthenticated creation accepted")
	}
	if j.pending != nil || manager.ensureCount != 0 {
		t.Fatal("authority failure mutated resources")
	}
}

func TestCNIPendingRestartAndQuarantine(t *testing.T) {
	for _, scenario := range []string{"expired", "conflict", "deleted", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, j, w, o, _, req := continuityFixture(t, false)
			enableTestAdmission(t, s, c, w, o)
			if _, err := s.Add(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			key := endpointKey(req.Identity)
			ack := c.state.Endpoints[key]
			if scenario == "expired" {
				ack.CreatedAt = time.Now().Add(-3 * time.Minute)
				ack.VerifyBy = ack.CreatedAt.Add(2 * time.Minute)
				c.state.Endpoints[key] = ack
			}
			j.raw, _ = json.Marshal(c.state)
			restarted := NewCNIServerWithBackends(nil, s.endpoint, s.network)
			rc := &cniContinuity{nodeName: c.nodeName, journal: j, workload: c.workload, observer: o, authority: c.authority}
			if err := restarted.recoverCNI(context.Background(), rc); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "conflict":
				w.err = errCNIIdentityConflict
			case "deleted":
				w.deleted = true
			default:
				w.err = errors.New("temporary runtime outage")
			}
			err := restarted.verifyPendingCNI(context.Background())
			if scenario == "unavailable" {
				if err == nil || rc.state.Endpoints[key].State != "pending_creation" {
					t.Fatal("runtime outage treated as success or absence")
				}
			} else if err != nil || rc.state.Endpoints[key].State != "quarantined" {
				t.Fatalf("unsafe pending record not quarantined: %v", err)
			}
			if o.deletes != 0 {
				t.Fatal("verification deleted external resources")
			}
		})
	}
}

func TestCNIPendingDuplicateAndDEL(t *testing.T) {
	s, c, _, w, o, _, req := continuityFixture(t, false)
	enableTestAdmission(t, s, c, w, o)
	if _, err := s.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Add(context.Background(), req)
	if err != nil || !resp.GetOutcome().GetIdempotentReplay() {
		t.Fatalf("pending duplicate: %v", err)
	}
	del := &pb.CNIDelRequest{Identity: req.Identity, Runtime: req.Runtime, K8S: req.K8S, Attachment: req.Attachment}
	if _, err := s.Del(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Del(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	if c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatal("pending deletion not acknowledged")
	}
}
