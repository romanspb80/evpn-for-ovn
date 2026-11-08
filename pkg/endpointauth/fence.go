package endpointauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

var ErrFenceStateNotFound = errors.New("endpoint authorization fence state not found")

const fenceStateVersion = 2

const MaxFenceStateSize = 2 << 20

// FenceStore provides atomic, durable storage for one authorization fence.
// Persist must leave either the prior complete value or the new complete value
// after a crash. After Persist returns an error, the caller must reopen it.
type FenceStore interface {
	Load() ([]byte, error)
	Persist([]byte) error
	Close() error
}

type fenceState struct {
	Version         int             `json:"version"`
	InstallationUID string          `json:"installationUID"`
	NodeUID         string          `json:"nodeUID"`
	Revision        uint64          `json:"revision"`
	Digest          string          `json:"digest"`
	Endpoints       []fenceEndpoint `json:"endpoints"`
}

// One high-water mark per network/endpoint is sufficient because generations
// are monotonic across attachment-ID changes for that endpoint.
type fenceEndpoint struct {
	NetworkID           string `json:"networkID"`
	EndpointID          string `json:"endpointID"`
	HighestGeneration   uint64 `json:"highestGeneration"`
	Active              bool   `json:"active"`
	AttachmentID        string `json:"attachmentID,omitempty"`
	RecordDigest        string `json:"recordDigest,omitempty"`
	RetiredGeneration   uint64 `json:"retiredGeneration,omitempty"`
	RetiredRecordDigest string `json:"retiredRecordDigest,omitempty"`
	RetirementReason    string `json:"retirementReason,omitempty"`
}

// Fencer durably accepts policy revisions and prevents retired endpoint
// generations from being reused.
type Fencer struct {
	mu              sync.Mutex
	store           FenceStore
	state           fenceState
	installationUID string
	nodeUID         string
	blocked         error
	closed          bool
}

// AcceptedSnapshot is issued only after its policy revision and lifecycle
// state have been durably persisted by a Fencer.
type AcceptedSnapshot struct {
	fencer   *Fencer
	snapshot *Snapshot
	version  PolicyVersion
}

// OpenFencer loads durable state for one installation and node. A missing
// state is accepted only when the backing store explicitly reports
// ErrFenceStateNotFound.
func OpenFencer(store FenceStore, installationUID, nodeUID string) (*Fencer, error) {
	fail := func(err error) (*Fencer, error) {
		if store != nil {
			err = errors.Join(err, store.Close())
		}
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("durable endpoint authorization fence store is required")
	}
	if err := validateIdentity("installationUID", installationUID); err != nil {
		return fail(err)
	}
	if err := validateIdentity("nodeUID", nodeUID); err != nil {
		return fail(err)
	}

	data, err := store.Load()
	if errors.Is(err, ErrFenceStateNotFound) {
		data = nil
	} else if err != nil {
		return fail(fmt.Errorf("load endpoint authorization fence: %w", err))
	} else if len(data) == 0 {
		return fail(fmt.Errorf("empty endpoint authorization fence state"))
	}
	state := fenceState{
		Version: fenceStateVersion, InstallationUID: installationUID,
		NodeUID: nodeUID, Endpoints: []fenceEndpoint{},
	}
	if data != nil {
		if len(data) > MaxFenceStateSize {
			return fail(fmt.Errorf("endpoint authorization fence state exceeds %d bytes", MaxFenceStateSize))
		}
		state, err = decodeFenceState(data)
		if err != nil {
			return fail(fmt.Errorf("invalid endpoint authorization fence state: %w", err))
		}
	}
	if err := validateFenceState(state, installationUID, nodeUID); err != nil {
		return fail(fmt.Errorf("invalid endpoint authorization fence state: %w", err))
	}
	return &Fencer{
		store: store, state: state,
		installationUID: installationUID, nodeUID: nodeUID,
	}, nil
}

// Accept validates and durably records a policy before returning an accepted
// snapshot. Persistence failures quarantine this Fencer until it is reopened.
func (f *Fencer) Accept(policy *Snapshot) (*AcceptedSnapshot, error) {
	validated, err := validateFencePolicy(policy)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return nil, err
	}
	if validated.InstallationUID() != f.installationUID || validated.NodeUID() != f.nodeUID {
		return nil, fmt.Errorf("authorization policy installation/node scope differs from durable fence")
	}

	version := validated.PolicyVersion()
	switch {
	case version.Revision < f.state.Revision:
		return nil, fmt.Errorf("authorization policy revision %d is below accepted high-water mark %d",
			version.Revision, f.state.Revision)
	case version.Revision == f.state.Revision && f.state.Revision != 0:
		if version.Digest != f.state.Digest {
			return nil, fmt.Errorf("authorization policy revision %d conflicts with accepted digest", version.Revision)
		}
		if err := validatePolicyMatchesFence(validated, f.state); err != nil {
			return nil, fmt.Errorf("accepted policy does not match durable lifecycle state: %w", err)
		}
		return &AcceptedSnapshot{fencer: f, snapshot: validated, version: version}, nil
	}

	next, err := advanceFenceState(f.state, validated)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode endpoint authorization fence state: %w", err)
	}
	if len(data) > MaxFenceStateSize {
		return nil, fmt.Errorf("endpoint authorization fence state exceeds %d bytes", MaxFenceStateSize)
	}
	if err := f.store.Persist(data); err != nil {
		f.blocked = fmt.Errorf("durable endpoint authorization fence write failed; reopen required: %w", err)
		return nil, f.blocked
	}
	f.state = next
	return &AcceptedSnapshot{fencer: f, snapshot: validated, version: version}, nil
}

// PolicyVersion returns the revision and digest that were durably accepted.
func (a *AcceptedSnapshot) PolicyVersion() PolicyVersion {
	if a == nil {
		return PolicyVersion{}
	}
	return a.version
}

// Snapshot returns the immutable policy only while this acceptance remains the
// current, healthy durable fence state.
func (a *AcceptedSnapshot) Snapshot() (*Snapshot, bool) {
	if a == nil || a.fencer == nil || a.snapshot == nil {
		return nil, false
	}
	a.fencer.mu.Lock()
	defer a.fencer.mu.Unlock()
	if a.fencer.closed || a.fencer.blocked != nil ||
		a.fencer.state.Revision != a.version.Revision || a.fencer.state.Digest != a.version.Digest {
		return nil, false
	}
	return a.snapshot, true
}

// IsCurrent reports whether this accepted revision is still the healthy
// high-water policy of its open fencer.
func (a *AcceptedSnapshot) IsCurrent() bool {
	_, current := a.Snapshot()
	return current
}

// Close invalidates accepted snapshots and releases the durable store.
func (f *Fencer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.store.Close()
}

func (f *Fencer) check() error {
	switch {
	case f.closed:
		return fmt.Errorf("endpoint authorization fencer is closed")
	case f.blocked != nil:
		return f.blocked
	default:
		return nil
	}
}

func validateFencePolicy(policy *Snapshot) (*Snapshot, error) {
	if policy == nil {
		return nil, fmt.Errorf("authorization policy is unavailable")
	}
	canonical := policy.CanonicalJSON()
	if len(canonical) == 0 || len(canonical) > MaxDocumentSize {
		return nil, fmt.Errorf("canonical authorization policy size must be between 1 and %d bytes", MaxDocumentSize)
	}
	validated, err := Decode(canonical)
	if err != nil {
		return nil, fmt.Errorf("authorization policy is invalid: %w", err)
	}
	if !bytes.Equal(canonical, validated.CanonicalJSON()) ||
		policy.PolicyVersion() != validated.PolicyVersion() ||
		policy.InstallationUID() != validated.InstallationUID() ||
		policy.NodeUID() != validated.NodeUID() {
		return nil, fmt.Errorf("authorization policy snapshot is inconsistent")
	}
	return validated, nil
}

func advanceFenceState(current fenceState, policy *Snapshot) (fenceState, error) {
	byIdentity := make(map[fenceIdentity]fenceEndpoint, len(current.Endpoints)+len(policy.endpoints))
	priorByIdentity := make(map[fenceIdentity]fenceEndpoint, len(current.Endpoints))
	for _, entry := range current.Endpoints {
		key := fenceIdentity{networkID: entry.NetworkID, endpointID: entry.EndpointID}
		byIdentity[key] = entry
		priorByIdentity[key] = entry
	}
	tombstonesByIdentity := make(map[fenceIdentity]EndpointTombstone, len(policy.tombstones))
	for _, tombstone := range policy.tombstones {
		record := tombstone.Record
		key := fenceIdentity{networkID: record.NetworkID, endpointID: record.EndpointID}
		tombstonesByIdentity[key] = tombstone
		entry, exists := byIdentity[key]
		if !exists {
			return fenceState{}, fmt.Errorf("endpoint tombstone %s/%s generation %d does not match durable lifecycle state",
				record.NetworkID, record.EndpointID, record.AttachmentGeneration)
		}
		digest := endpointRecordDigest(record)
		if entry.Active && entry.HighestGeneration == record.AttachmentGeneration {
			if entry.AttachmentID != record.AttachmentID || entry.RecordDigest != digest {
				return fenceState{}, fmt.Errorf("endpoint tombstone %s/%s does not match the active attachment record",
					record.NetworkID, record.EndpointID)
			}
			entry.Active, entry.AttachmentID, entry.RecordDigest = false, "", ""
			entry.RetiredGeneration = record.AttachmentGeneration
			entry.RetiredRecordDigest = digest
			entry.RetirementReason = string(tombstone.Reason)
		} else if entry.RetiredGeneration != record.AttachmentGeneration ||
			entry.RetiredRecordDigest != digest || entry.RetirementReason != string(tombstone.Reason) {
			return fenceState{}, fmt.Errorf("endpoint tombstone %s/%s conflicts with durable retirement evidence",
				record.NetworkID, record.EndpointID)
		}
		byIdentity[key] = entry
	}
	policyByIdentity := make(map[fenceIdentity]EndpointRecord, len(policy.endpoints))
	for _, record := range policy.endpoints {
		key := fenceIdentity{networkID: record.NetworkID, endpointID: record.EndpointID}
		policyByIdentity[key] = record
		entry, exists := byIdentity[key]
		if !exists {
			byIdentity[key] = newFenceEndpoint(record)
			continue
		}
		if record.AttachmentGeneration < entry.HighestGeneration {
			return fenceState{}, fmt.Errorf("endpoint %s/%s generation %d is below retired generation high-water mark %d",
				record.NetworkID, record.EndpointID, record.AttachmentGeneration, entry.HighestGeneration)
		}
		switch {
		case record.AttachmentGeneration == entry.HighestGeneration:
			if !entry.Active || entry.AttachmentID != record.AttachmentID ||
				entry.RecordDigest != endpointRecordDigest(record) {
				return fenceState{}, fmt.Errorf("endpoint %s/%s reuses retired or changed generation %d",
					record.NetworkID, record.EndpointID, record.AttachmentGeneration)
			}
		case record.AttachmentGeneration > entry.HighestGeneration:
			prior := priorByIdentity[key]
			if policy.Version() >= LifecycleVersion && prior.Active {
				tombstone, hasTombstone := tombstonesByIdentity[key]
				if !hasTombstone || tombstone.Reason != TombstoneReplaced ||
					tombstone.Record.AttachmentGeneration != prior.HighestGeneration {
					return fenceState{}, fmt.Errorf("endpoint replacement %s/%s requires an exact replaced tombstone",
						record.NetworkID, record.EndpointID)
				}
			}
			next := newFenceEndpoint(record)
			next.RetiredGeneration = entry.RetiredGeneration
			next.RetiredRecordDigest = entry.RetiredRecordDigest
			next.RetirementReason = entry.RetirementReason
			byIdentity[key] = next
		}
	}
	for key, entry := range byIdentity {
		if entry.Active {
			if _, present := policyByIdentity[key]; !present {
				entry.Active, entry.AttachmentID, entry.RecordDigest = false, "", ""
				entry.RetiredGeneration, entry.RetiredRecordDigest, entry.RetirementReason = 0, "", ""
				byIdentity[key] = entry
			}
		}
	}

	entries := make([]fenceEndpoint, 0, len(byIdentity))
	for _, entry := range byIdentity {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].NetworkID != entries[j].NetworkID {
			return entries[i].NetworkID < entries[j].NetworkID
		}
		return entries[i].EndpointID < entries[j].EndpointID
	})
	return fenceState{
		Version: fenceStateVersion, InstallationUID: policy.InstallationUID(),
		NodeUID: policy.NodeUID(), Revision: policy.Revision(), Digest: policy.Digest(),
		Endpoints: entries,
	}, nil
}

type fenceIdentity struct {
	networkID  string
	endpointID string
}

func newFenceEndpoint(record EndpointRecord) fenceEndpoint {
	return fenceEndpoint{
		NetworkID: record.NetworkID, EndpointID: record.EndpointID,
		HighestGeneration: record.AttachmentGeneration, Active: true,
		AttachmentID: record.AttachmentID, RecordDigest: endpointRecordDigest(record),
	}
}

func endpointRecordDigest(record EndpointRecord) string {
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validatePolicyMatchesFence(policy *Snapshot, state fenceState) error {
	active := make(map[fenceIdentity]EndpointRecord, len(policy.endpoints))
	for _, record := range policy.endpoints {
		active[fenceIdentity{networkID: record.NetworkID, endpointID: record.EndpointID}] = record
	}
	for _, entry := range state.Endpoints {
		key := fenceIdentity{networkID: entry.NetworkID, endpointID: entry.EndpointID}
		record, present := active[key]
		if entry.Active != present {
			return fmt.Errorf("endpoint %s/%s active state disagrees with policy", entry.NetworkID, entry.EndpointID)
		}
		if present && (record.AttachmentGeneration != entry.HighestGeneration ||
			record.AttachmentID != entry.AttachmentID || endpointRecordDigest(record) != entry.RecordDigest) {
			return fmt.Errorf("endpoint %s/%s lifecycle disagrees with policy", entry.NetworkID, entry.EndpointID)
		}
	}
	for _, tombstone := range policy.tombstones {
		record := tombstone.Record
		var entry *fenceEndpoint
		for i := range state.Endpoints {
			candidate := &state.Endpoints[i]
			if candidate.NetworkID == record.NetworkID && candidate.EndpointID == record.EndpointID {
				entry = candidate
				break
			}
		}
		if entry == nil || entry.RetiredGeneration != record.AttachmentGeneration ||
			entry.RetiredRecordDigest != endpointRecordDigest(record) ||
			entry.RetirementReason != string(tombstone.Reason) {
			return fmt.Errorf("endpoint tombstone %s/%s disagrees with durable retirement state", record.NetworkID, record.EndpointID)
		}
	}
	if len(active) > len(state.Endpoints) {
		return fmt.Errorf("policy contains an endpoint absent from durable lifecycle state")
	}
	return nil
}

func decodeFenceState(data []byte) (fenceState, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return fenceState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state fenceState
	if err := decoder.Decode(&state); err != nil {
		return fenceState{}, err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fenceState{}, fmt.Errorf("durable fence state must contain exactly one JSON value")
	}
	if state.Version == 1 {
		var legacy map[string]json.RawMessage
		if err := json.Unmarshal(data, &legacy); err != nil {
			return fenceState{}, err
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(legacy["endpoints"], &entries); err != nil {
			return fenceState{}, fmt.Errorf("legacy durable fence endpoint list is invalid: %w", err)
		}
		for _, entry := range entries {
			if _, present := entry["retiredGeneration"]; present {
				return fenceState{}, fmt.Errorf("legacy durable fence endpoint contains version-2 lifecycle data")
			}
			if _, present := entry["retiredRecordDigest"]; present {
				return fenceState{}, fmt.Errorf("legacy durable fence endpoint contains version-2 lifecycle data")
			}
			if _, present := entry["retirementReason"]; present {
				return fenceState{}, fmt.Errorf("legacy durable fence endpoint contains version-2 lifecycle data")
			}
		}
		state.Version = fenceStateVersion
	}
	return state, nil
}

func validateFenceState(state fenceState, installationUID, nodeUID string) error {
	if state.Version != fenceStateVersion || state.InstallationUID != installationUID ||
		state.NodeUID != nodeUID || state.Endpoints == nil {
		return fmt.Errorf("unsupported schema, scope mismatch, or missing endpoint lifecycle list")
	}
	if state.Revision == 0 {
		if state.Digest != "" || len(state.Endpoints) != 0 {
			return fmt.Errorf("empty high-water state contains revision, digest or lifecycle entries")
		}
		return nil
	}
	if !validFenceDigest(state.Digest) {
		return fmt.Errorf("accepted policy digest is invalid")
	}
	last := fenceIdentity{}
	for i, entry := range state.Endpoints {
		if err := validateIdentity("networkID", entry.NetworkID); err != nil {
			return err
		}
		if err := validateIdentity("endpointID", entry.EndpointID); err != nil {
			return err
		}
		key := fenceIdentity{networkID: entry.NetworkID, endpointID: entry.EndpointID}
		if i > 0 && (key.networkID < last.networkID ||
			(key.networkID == last.networkID && key.endpointID <= last.endpointID)) {
			return fmt.Errorf("endpoint lifecycle entries are duplicate or unsorted")
		}
		if entry.HighestGeneration == 0 {
			return fmt.Errorf("endpoint lifecycle generation must be positive")
		}
		if entry.Active {
			if err := validateIdentity("attachmentID", entry.AttachmentID); err != nil {
				return err
			}
			if !validFenceDigest(entry.RecordDigest) {
				return fmt.Errorf("active endpoint record digest is invalid")
			}
		} else {
			if entry.AttachmentID != "" || entry.RecordDigest != "" {
				return fmt.Errorf("retired endpoint lifecycle retains active attachment data")
			}
		}
		proofFieldsPresent := entry.RetiredGeneration != 0 || entry.RetiredRecordDigest != "" || entry.RetirementReason != ""
		if proofFieldsPresent {
			if entry.RetiredGeneration == 0 || entry.RetiredGeneration > entry.HighestGeneration ||
				(entry.Active && entry.RetiredGeneration >= entry.HighestGeneration) ||
				(!entry.Active && entry.RetiredGeneration != entry.HighestGeneration) {
				return fmt.Errorf("endpoint lifecycle retired generation is inconsistent with high-water state")
			}
			if !validFenceDigest(entry.RetiredRecordDigest) {
				return fmt.Errorf("retired endpoint lifecycle digest is invalid")
			}
			if entry.RetirementReason != string(TombstoneDeleted) &&
				entry.RetirementReason != string(TombstoneReplaced) &&
				entry.RetirementReason != string(TombstoneMigrated) {
				return fmt.Errorf("retired endpoint lifecycle has an unsupported reason")
			}
		}
		last = key
	}
	return nil
}

func validFenceDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
