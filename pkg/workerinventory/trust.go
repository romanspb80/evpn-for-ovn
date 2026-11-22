package workerinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

const (
	TrustAuthorized = "AUTHORIZED"
	TrustTemporary  = "TEMPORARILY_UNAVAILABLE"
	TrustFenced     = "FENCED"
	TrustPrepared   = "RECOVERY_PENDING"
	TrustFinal      = "RECOVERY_AUTHORIZED"
)

// TrustState is persisted separately from export ownership. A prepared grant
// is never permission to advertise, even if the export journal is ready.
type TrustState struct {
	Publication     string
	Phase           string
	Incident        string
	Denial          uint64
	Security        bool
	Causes          []string
	FailureEvidence []string `json:",omitempty"`
	Generation      uint64
	RequestID       string
	RequestDigest   string
	Reason          string
	Binding         *TrustBinding
}

type TrustBinding struct {
	Transaction     string
	Publication     string
	ClusterUID      string
	InstallationUID string
	NodeName        string
	NodeUID         string
	Revision        uint64
	Digest          string
	WorkerDigest    string
	Incident        string
	Denial          uint64
	Generation      uint64
	Domain          string
	Incarnation     string
	Ownership       string
}

// RecoveryRequest is an instruction from the independently authenticated host
// administrator, not from an inventory publication or a speaker health check.
type RecoveryRequest struct {
	Version int
	Action  string
	ID      string
	Reason  string
	Binding TrustBinding
}

func TrustDigest(value interface{}) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneTrust(s *TrustState) *TrustState {
	next := *s
	next.Causes = append([]string(nil), s.Causes...)
	next.FailureEvidence = append([]string(nil), s.FailureEvidence...)
	if s.Binding != nil {
		b := *s.Binding
		next.Binding = &b
	}
	return &next
}

func (s TrustState) Validate() error {
	if s.Publication == "" {
		return fmt.Errorf("publication pin required")
	}
	switch s.Phase {
	case TrustAuthorized, TrustTemporary, TrustFenced, TrustPrepared, TrustFinal:
	default:
		return fmt.Errorf("invalid trust phase")
	}
	if s.Denial != 0 && (!canonicalUUID(s.Incident) || len(s.Causes) == 0) {
		return fmt.Errorf("incident evidence required")
	}
	if s.Security && s.Denial == 0 {
		return fmt.Errorf("security incident required")
	}
	if (s.Phase == TrustTemporary && s.Security) || (s.Phase == TrustFenced && !s.Security) ||
		(s.Phase == TrustAuthorized && (s.Denial != 0 || s.Binding != nil)) {
		return fmt.Errorf("inconsistent trust phase and incident")
	}
	if s.Phase == TrustPrepared || s.Phase == TrustFinal {
		if s.Security && (s.RequestID == "" || s.Generation == 0) {
			return fmt.Errorf("administrative recovery identity and generation required")
		}
		if s.Binding == nil || !canonicalUUID(s.Binding.Transaction) ||
			s.Binding.Publication != s.Publication || s.Binding.Denial != s.Denial ||
			s.Binding.Incident != s.Incident || s.Binding.Generation != s.Generation ||
			!digestValid(s.Binding.Digest) || !digestValid(s.Binding.WorkerDigest) ||
			!digestValid(s.Binding.Domain) || !digestValid(s.Binding.Ownership) || s.Binding.Incarnation == "" {
			return fmt.Errorf("invalid recovery binding")
		}
	}
	if s.RequestID != "" && (!canonicalUUID(s.RequestID) || !digestValid(s.RequestDigest) || s.Reason == "") {
		return fmt.Errorf("invalid consumed administrative request")
	}
	return nil
}

// EnableTrust is an explicitly opted-in schema conversion. Ownership, accepted
// high-water marks and any unresolved legacy acceptance are never discarded.
func (f *Fence) EnableTrust(publication string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if publication == "" {
		return fmt.Errorf("pinned publication identity required")
	}
	if f.state.Trust != nil {
		if f.state.Trust.Publication != publication {
			return fmt.Errorf("%w: publication pin changed", ownership.ErrBlocked)
		}
		return nil
	}
	next := f.state
	if next.Version == 0 {
		next = fenceState{ClusterUID: f.cluster, Installation: f.install, NodeName: f.nodeName, NodeUID: f.nodeUID}
	}
	next.Version = 2
	next.Trust = &TrustState{Publication: publication, Phase: TrustAuthorized}
	return f.commitTrust(next)
}

func (f *Fence) commitTrust(next fenceState) error {
	if err := f.validateState(next); err != nil {
		return err
	}

	seq, err := f.journal.Begin("worker-inventory-trust", next)
	if err != nil {
		return err
	}
	if err := f.journal.Commit(seq, next); err != nil {
		return err
	}
	f.state = next
	return nil
}

func (f *Fence) recoverTrustIntent(pending *ownership.Intent) error {
	var next fenceState
	if err := decodeFence(pending.Payload, &next); err != nil {
		return err
	}
	if err := f.validateState(next); err != nil {
		return err
	}
	if next.Version != 2 || next.Revision != f.state.Revision || next.Digest != f.state.Digest ||
		next.WorkerDigest != f.state.WorkerDigest {
		return fmt.Errorf("%w: trust checkpoint cannot advance inventory authority", ownership.ErrBlocked)
	}
	if f.state.Trust != nil && (next.Trust.Denial < f.state.Trust.Denial || next.Trust.Generation < f.state.Trust.Generation) {
		return fmt.Errorf("%w: trust checkpoint rolls back a high-water mark", ownership.ErrBlocked)
	}
	// A pending FINAL is only a proposal. Recreate PREPARED so the owning
	// runtime must check the other journal and current evidence again.
	if next.Trust.Phase == TrustFinal {
		next.Trust.Phase = TrustPrepared
	}
	if err := f.journal.Commit(pending.Sequence, next); err != nil {
		return err
	}
	f.state = next
	return nil
}

func (f *Fence) Trust() (TrustState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Trust == nil {
		return TrustState{}, fmt.Errorf("trust recovery is not enabled")
	}
	// A poisoned journal cannot supply a grant.
	if _, pending, err := f.journal.Snapshot(); err != nil || pending != nil {
		return TrustState{}, fmt.Errorf("%w: unresolved trust journal: %v", ownership.ErrBlocked, err)
	}
	return *cloneTrust(f.state.Trust), nil
}

// Deny never downgrades a security incident to availability uncertainty.
func (f *Fence) Deny(cause string, security bool, evidence ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Trust == nil || cause == "" {
		return fmt.Errorf("trust recovery and explicit denial cause required")
	}
	next := f.state
	next.Trust = cloneTrust(next.Trust)
	s := next.Trust
	active := s.Phase != TrustAuthorized && s.Phase != TrustFinal
	if active {
		for _, prior := range s.Causes {
			if prior == cause && (!security || s.Security) {
				return nil
			}
		}
	}
	if s.Denial == ^uint64(0) {
		return fmt.Errorf("%w: denial sequence exhausted", ownership.ErrBlocked)
	}
	if !active {
		s.Causes, s.Security = nil, false
		s.FailureEvidence = nil
	}
	s.Incident, s.Denial = uuid.NewString(), s.Denial+1
	s.Causes = append(s.Causes, cause)
	if len(evidence) != 0 {
		s.FailureEvidence = append(s.FailureEvidence, cause+": "+evidence[0])
	}
	s.Security = s.Security || security
	s.Phase, s.Binding = TrustTemporary, nil
	if s.Security {
		s.Phase = TrustFenced
	}
	return f.commitTrust(next)
}

func (f *Fence) binding(domain, incarnation, owned string) TrustBinding {
	s := f.state.Trust
	return TrustBinding{Publication: s.Publication, ClusterUID: f.cluster, InstallationUID: f.install,
		NodeName: f.nodeName, NodeUID: f.nodeUID, Revision: f.state.Revision, Digest: f.state.Digest,
		WorkerDigest: f.state.WorkerDigest, Incident: s.Incident, Denial: s.Denial,
		Generation: s.Generation, Domain: domain, Incarnation: incarnation, Ownership: owned}
}

// RecoveryTarget is diagnostic challenge material. Its origin confers no
// authority: requests must arrive through the protected administrative channel.
func (f *Fence) RecoveryTarget(domain, incarnation, owned string) (TrustBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Trust == nil || f.state.Revision == 0 {
		return TrustBinding{}, fmt.Errorf("accepted authority required")
	}
	b := f.binding(domain, incarnation, owned)
	if f.state.Trust.Security && f.state.Trust.Phase != TrustFinal && f.state.Trust.Phase != TrustPrepared {
		if b.Generation == ^uint64(0) {
			return TrustBinding{}, fmt.Errorf("recovery generation exhausted")
		}
		b.Generation++
	}
	return b, nil
}

func (f *Fence) Prepare(domain, incarnation, owned string, request *RecoveryRequest) (TrustBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Trust == nil || f.state.Revision == 0 {
		return TrustBinding{}, fmt.Errorf("accepted authority required")
	}
	next := f.state
	next.Trust = cloneTrust(next.Trust)
	s := next.Trust
	want := f.binding(domain, incarnation, owned)
	if s.Phase == TrustFinal && s.Binding != nil && s.Binding.Revision == want.Revision &&
		s.Binding.Digest == want.Digest && s.Binding.Domain == domain && s.Binding.Incarnation == incarnation {
		if request != nil && TrustDigest(request) != s.RequestDigest {
			return TrustBinding{}, fmt.Errorf("%w: consumed request differs", ownership.ErrBlocked)
		}
		return *s.Binding, nil
	}
	if s.Phase == TrustPrepared {
		prior := *s.Binding
		want.Transaction, want.Generation, want.Ownership = prior.Transaction, prior.Generation, prior.Ownership
		if !reflect.DeepEqual(want, prior) {
			return TrustBinding{}, fmt.Errorf("%w: prepared recovery evidence changed", ownership.ErrBlocked)
		}
		if s.Security && (request == nil || TrustDigest(request) != s.RequestDigest) {
			return TrustBinding{}, fmt.Errorf("%w: prepared administrative request differs", ownership.ErrBlocked)
		}
		return prior, nil
	}
	if s.Security && s.Phase != TrustFinal {
		if s.Generation == ^uint64(0) {
			return TrustBinding{}, fmt.Errorf("recovery generation exhausted")
		}
		want.Generation++
		if request == nil || request.Version != 1 || request.Action != "authorize-verified-recovery" ||
			!canonicalUUID(request.ID) || request.ID == s.RequestID || request.Reason == "" ||
			!reflect.DeepEqual(request.Binding, want) {
			return TrustBinding{}, fmt.Errorf("%w: scoped fresh administrative recovery authorization required", ownership.ErrBlocked)
		}
		s.Generation, s.RequestID, s.RequestDigest, s.Reason = want.Generation, request.ID, TrustDigest(request), request.Reason
	}
	want.Transaction = uuid.NewString()
	s.Binding, s.Phase = &want, TrustPrepared
	if err := f.commitTrust(next); err != nil {
		return TrustBinding{}, err
	}
	return want, nil
}

func (f *Fence) Finalize(binding TrustBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Trust == nil || f.state.Trust.Binding == nil || !reflect.DeepEqual(*f.state.Trust.Binding, binding) {
		return fmt.Errorf("%w: prepared binding required", ownership.ErrBlocked)
	}
	if f.state.Trust.Phase == TrustFinal {
		return nil
	}
	if f.state.Trust.Phase != TrustPrepared {
		return fmt.Errorf("%w: recovery is not prepared", ownership.ErrBlocked)
	}
	next := f.state
	next.Trust = cloneTrust(next.Trust)
	next.Trust.Phase = TrustFinal
	return f.commitTrust(next)
}
