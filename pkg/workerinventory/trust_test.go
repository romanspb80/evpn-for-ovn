package workerinventory

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/google/uuid"
)

type trustCrashInput struct {
	Directory string
	Cut       string
	Request   RecoveryRequest
}

func TestTrustRecoveryCrashHelper(t *testing.T) {
	raw := os.Getenv("EVPN_TRUST_CRASH_INPUT")
	if raw == "" {
		t.Skip("subprocess-only recovery crash helper")
	}
	var input trustCrashInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	in := sampleInventory()
	f, err := OpenFence(input.Directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
	if err != nil {
		t.Fatal(err)
	}
	if input.Cut == "denial" {
		err = f.Deny("publication-integrity", true)
	} else {
		d, i, o := trustEvidence()
		var binding TrustBinding
		binding, err = f.Prepare(d, i, o, &input.Request)
		if err == nil && input.Cut == "final" {
			err = f.Finalize(binding)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {}
}

// I4/I8/I9: process death cannot forget denial, unconsume an epoch or promote
// PREPARED. The separate coordinator tests check the export READY intersection.
func TestTrustRecoverySIGKILL(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected recovery journal requires root")
	}
	for _, cut := range []string{"denial", "prepared", "final"} {
		t.Run(cut, func(t *testing.T) {
			in := sampleInventory()
			directory := filepath.Join(t.TempDir(), "inventory")
			open := func() *Fence {
				t.Helper()
				f, err := OpenFence(directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
				if err != nil {
					t.Fatal(err)
				}
				return f
			}
			f := open()
			if _, err := f.Accept(in); err != nil {
				t.Fatal(err)
			}
			if err := f.EnableTrust("namespace/inventory/publication-uid"); err != nil {
				t.Fatal(err)
			}
			if cut != "denial" {
				if err := f.Deny("publication-integrity", true); err != nil {
					t.Fatal(err)
				}
			}
			input := trustCrashInput{Directory: directory, Cut: cut, Request: requestFor(t, f)}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestTrustRecoveryCrashHelper$")
			cmd.Env = append(os.Environ(), "EVPN_TRUST_CRASH_INPUT="+string(raw))
			output, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("recovery child missed SIGKILL: %v %s", err, output)
			}
			f = open()
			defer f.Close()
			state, err := f.Trust()
			want := map[string]string{"denial": TrustFenced, "prepared": TrustPrepared, "final": TrustFinal}[cut]
			if err != nil || state.Phase != want || !state.Security || state.Denial != 1 {
				t.Fatalf("lost durable trust state after SIGKILL: %+v %v", state, err)
			}
			if cut != "denial" && (state.Generation != 1 || state.RequestID != input.Request.ID) {
				t.Fatalf("lost consumed administrative authorization: %+v", state)
			}
		})
	}
}

func trustFixture(t *testing.T) (*Fence, func() *Fence) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("protected durable trust journal requires root")
	}
	in := sampleInventory()
	directory := filepath.Join(t.TempDir(), "inventory")
	open := func() *Fence {
		f, err := OpenFence(directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	f := open()
	if _, err := f.Accept(in); err != nil {
		t.Fatal(err)
	}
	if err := f.EnableTrust("namespace/inventory/publication-uid"); err != nil {
		t.Fatal(err)
	}
	return f, open
}

func trustEvidence() (string, string, string) {
	return TrustDigest("domain"), "independently-attested-incarnation", TrustDigest("owned-routes")
}

func requestFor(t *testing.T, f *Fence) RecoveryRequest {
	t.Helper()
	b, err := f.RecoveryTarget(trustEvidence())
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryRequest{Version: 1, Action: "authorize-verified-recovery", ID: uuid.NewString(), Reason: "independent administrator investigation complete", Binding: b}
}

func TestTrustFailureEvidenceSurvivesLaterFailuresAndRestart(t *testing.T) {
	f, open := trustFixture(t)
	if err := f.Deny("speaker-identity", true, "original integrity failure"); err != nil {
		t.Fatal(err)
	}
	if err := f.Deny("speaker-identity", true, "later observation"); err != nil {
		t.Fatal(err)
	}
	if err := f.Deny("speaker-transport-unavailable", false, "socket unavailable"); err != nil {
		t.Fatal(err)
	}
	state, err := f.Trust()
	if err != nil || len(state.FailureEvidence) != 2 {
		t.Fatalf("missing first-cause evidence: %+v %v", state, err)
	}
	state.FailureEvidence[0] = "mutated diagnostic copy"
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	defer f.Close()
	state, err = f.Trust()
	if err != nil || !state.Security || state.FailureEvidence[0] != "speaker-identity: original integrity failure" {
		t.Fatalf("first security cause lost across restart: %+v %v", state, err)
	}
}

func TestTrustRejectsMalformedAdministrativeGrant(t *testing.T) {
	f, _ := trustFixture(t)
	defer f.Close()
	if err := f.Deny("publication-integrity", true); err != nil {
		t.Fatal(err)
	}
	request := requestFor(t, f)
	d, i, o := trustEvidence()
	if _, err := f.Prepare(d, i, o, &request); err != nil {
		t.Fatal(err)
	}
	valid, err := f.Trust()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"request", "generation", "digest", "reason"} {
		state := *cloneTrust(&valid)
		switch field {
		case "request":
			state.RequestID = ""
		case "generation":
			state.Generation, state.Binding.Generation = 0, 0
		case "digest":
			state.RequestDigest = ""
		case "reason":
			state.Reason = ""
		}
		if err := state.Validate(); err == nil {
			t.Fatalf("malformed administrative grant accepted: %s", field)
		}
	}
}

// I1/I4: health or inventory freshness alone never releases a security fence.
func TestTrustSecurityFenceAndAdministrativeScope(t *testing.T) {
	for _, scenario := range []string{"missing", "worker", "authority", "publication", "revision", "digest", "epoch", "incident", "ownership", "domain", "action", "request-id"} {
		t.Run(scenario, func(t *testing.T) {
			f, _ := trustFixture(t)
			defer f.Close()
			if err := f.Deny("publication-integrity", true); err != nil {
				t.Fatal(err)
			}
			r := requestFor(t, f)
			switch scenario {
			case "worker":
				r.Binding.NodeUID = uuid.NewString()
			case "authority":
				r.Binding.InstallationUID = uuid.NewString()
			case "publication":
				r.Binding.Publication = "another"
			case "revision":
				r.Binding.Revision++
			case "digest":
				r.Binding.Digest = TrustDigest("equivocation")
			case "epoch":
				r.Binding.Generation--
			case "incident":
				r.Binding.Incident = uuid.NewString()
			case "ownership":
				r.Binding.Ownership = TrustDigest("other receipts")
			case "domain":
				r.Binding.Domain = TrustDigest("other domain")
			case "action":
				r.Action = "force-clear"
			case "request-id":
				r.ID = ""
			}
			var request *RecoveryRequest
			if scenario != "missing" {
				request = &r
			}
			d, i, o := trustEvidence()
			if _, err := f.Prepare(d, i, o, request); err == nil {
				t.Fatal("untrusted request granted")
			}
			s, err := f.Trust()
			if err != nil || s.Phase != TrustFenced || s.Generation != 0 {
				t.Fatalf("rejection mutated authority: %+v %v", s, err)
			}
		})
	}
}

// I4/I5/I9: restart preserves fences and consumed requests are idempotent.
func TestTrustRecoveryReplayRestartAndDenialPrecedence(t *testing.T) {
	f, open := trustFixture(t)
	if err := f.Deny("publication-integrity", true); err != nil {
		t.Fatal(err)
	}
	r := requestFor(t, f)
	d, i, o := trustEvidence()
	b, err := f.Prepare(d, i, o, &r)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	defer f.Close()
	s, _ := f.Trust()
	if s.Phase != TrustPrepared || s.Generation != 1 {
		t.Fatalf("lost prepared grant: %+v", s)
	}
	again, err := f.Prepare(d, i, o, &r)
	if err != nil || again != b {
		t.Fatalf("non-idempotent resume: %+v %v", again, err)
	}
	conflict := r
	conflict.Reason = "different instruction"
	if _, err := f.Prepare(d, i, o, &conflict); err == nil {
		t.Fatal("equivocated request accepted")
	}
	if err := f.Finalize(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Deny("publication-integrity", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Finalize(b); err == nil {
		t.Fatal("old grant erased new denial")
	}
	if _, err := f.Prepare(d, i, o, &r); err == nil {
		t.Fatal("consumed epoch restored new incident")
	}
}

// I1/I2/I10: availability can recover, but never downgrade a security cause.
func TestTrustTransientAndSecurityDominance(t *testing.T) {
	f, _ := trustFixture(t)
	defer f.Close()
	if err := f.Deny("speaker-unavailable", false); err != nil {
		t.Fatal(err)
	}
	b, err := f.Prepare(trustEvidenceWithNilRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Finalize(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Deny("identity-mismatch", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Deny("speaker-unavailable", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Prepare(trustEvidenceWithNilRequest()); err == nil {
		t.Fatal("health cleared identity fence")
	}
}

func trustEvidenceWithNilRequest() (string, string, string, *RecoveryRequest) {
	d, i, o := trustEvidence()
	return d, i, o, nil
}

// I9: concurrent identical grants prepare exactly one transaction/generation.
func TestTrustConcurrentRequests(t *testing.T) {
	f, _ := trustFixture(t)
	defer f.Close()
	if err := f.Deny("publication-integrity", true); err != nil {
		t.Fatal(err)
	}
	r := requestFor(t, f)
	d, i, o := trustEvidence()
	var wg sync.WaitGroup
	bindings := make(chan TrustBinding, 8)
	failures := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); b, err := f.Prepare(d, i, o, &r); bindings <- b; failures <- err }()
	}
	wg.Wait()
	close(bindings)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := <-bindings
	for b := range bindings {
		if b != first {
			t.Fatal("duplicate transactions")
		}
	}
	s, _ := f.Trust()
	if s.Generation != 1 {
		t.Fatal("generation consumed repeatedly")
	}
}

func TestTrustConcurrentConflictingRequests(t *testing.T) {
	f, _ := trustFixture(t)
	defer f.Close()
	if err := f.Deny("publication-integrity", true); err != nil {
		t.Fatal(err)
	}
	request := requestFor(t, f)
	d, i, o := trustEvidence()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for n := 0; n < 8; n++ {
		candidate := request
		candidate.ID = uuid.NewString()
		wg.Add(1)
		go func(r RecoveryRequest) {
			defer wg.Done()
			_, err := f.Prepare(d, i, o, &r)
			results <- err
		}(candidate)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	state, err := f.Trust()
	if success != 1 || err != nil || state.Generation != 1 {
		t.Fatalf("concurrent grants were not serialized: successes=%d state=%+v err=%v", success, state, err)
	}
}

func TestTrustConversionPreservesAuthorityHighWater(t *testing.T) {
	f, open := trustFixture(t)
	before := f.state
	if before.Version != 2 || before.Revision == 0 || before.Digest == "" || before.WorkerDigest == "" {
		t.Fatal("conversion lost accepted v1 authority")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	defer f.Close()
	if f.state.Revision != before.Revision || f.state.Digest != before.Digest || f.state.WorkerDigest != before.WorkerDigest {
		t.Fatal("restart changed converted inventory high-water marks")
	}
	stale := sampleInventory()
	stale.Revision = 0
	if _, err := f.Accept(stale); err == nil {
		t.Fatal("converted journal accepted stale authority")
	}
}

// I8: an interrupted FINAL is replayed as PREPARED, never as permission.
func TestTrustPendingFinalRequiresReverification(t *testing.T) {
	f, open := trustFixture(t)
	if err := f.Deny("speaker-unavailable", false); err != nil {
		t.Fatal(err)
	}
	b, err := f.Prepare(trustEvidenceWithNilRequest())
	if err != nil {
		t.Fatal(err)
	}
	next := f.state
	next.Trust = cloneTrust(next.Trust)
	next.Trust.Phase = TrustFinal
	if _, err := f.journal.Begin("worker-inventory-trust", next); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	defer f.Close()
	s, err := f.Trust()
	if err != nil || s.Phase != TrustPrepared || *s.Binding != b {
		t.Fatalf("pending FINAL became authority: %+v %v", s, err)
	}
}
