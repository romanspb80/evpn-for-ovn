package evpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

type ExportRecoveryConfig struct {
	StateDirectory string
	Domain         ownership.Domain
}

// VerifiedSpeaker binds observations and mutations to independently attested
// workload/runtime identity and the actual Unix peer process incarnation.
type VerifiedSpeaker interface {
	Check(context.Context) error
	Incarnation() (string, error)
}

type exportReceipt struct {
	Owned       OwnedType2Route
	NLRI        []byte
	Attributes  [][]byte
	Incarnation string
}

type exportRecoveryState struct {
	Version     int
	Incarnation string
	Receipts    []exportReceipt
}

type exportIntent struct {
	Incarnation string
	Route       exportReceipt
}

// ExportRecovery never adopts RIB paths. Its only ownership authority is a
// durable acknowledged UUID in the same domain and verified incarnation.
type ExportRecovery struct {
	mu      sync.Mutex
	journal *ownership.Journal
	state   exportRecoveryState
	source  Type2RIBSource
	applier Type2RouteApplier
	speaker VerifiedSpeaker
	fault   func(string) error
}

func OpenExportRecovery(cfg ExportRecoveryConfig, source Type2RIBSource, applier Type2RouteApplier, speaker VerifiedSpeaker) (*ExportRecovery, error) {
	if source == nil || applier == nil || speaker == nil {
		return nil, fmt.Errorf("%w: exporter recovery requires attested speaker, RIB and applier", ownership.ErrBlocked)
	}
	j, err := ownership.Open(cfg.StateDirectory, cfg.Domain, true)
	if err != nil {
		return nil, err
	}
	r := &ExportRecovery{journal: j, source: source, applier: applier, speaker: speaker}
	data, pending, err := j.Snapshot()
	if err == nil && string(data) != "null" {
		err = decodeExportRecord(data, &r.state)
		if err == nil {
			err = r.validate()
		}
	} else if err == nil && (pending == nil || pending.Operation == "export-checkpoint") {
		r.state.Version = 1
	}
	if err != nil || (r.state.Version == 0 && pending != nil) {
		return nil, errors.Join(fmt.Errorf("%w: invalid exporter state: %v", ownership.ErrBlocked, err), j.Close())
	}
	return r, nil
}

func decodeExportRecord(data []byte, target interface{}) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("expected one exporter record")
	}
	return nil
}

func durableExportRoute(endpoint LocalEndpoint, incarnation, id string) (exportReceipt, error) {
	nlri, attributes, err := type2WirePayload(endpoint)
	if err != nil {
		return exportReceipt{}, err
	}
	receipt := exportReceipt{Owned: OwnedType2Route{Endpoint: cloneLocalEndpoint(endpoint), PathUUID: id}, NLRI: nlri, Incarnation: incarnation}
	receipt.Attributes = attributes
	return receipt, nil
}

func (r *ExportRecovery) validate() error {
	if r.state.Version != 1 || (r.state.Incarnation == "" && len(r.state.Receipts) != 0) {
		return fmt.Errorf("incompatible exporter schema/incarnation")
	}
	keys, ids := map[string]bool{}, map[string]bool{}
	for _, receipt := range r.state.Receipts {
		id, err := uuid.Parse(receipt.Owned.PathUUID)
		if err != nil || id == uuid.Nil || ids[id.String()] || receipt.Incarnation != r.state.Incarnation {
			return fmt.Errorf("invalid or duplicate UUID/incarnation receipt")
		}
		want, err := durableExportRoute(receipt.Owned.Endpoint, receipt.Incarnation, receipt.Owned.PathUUID)
		if err != nil || !sameExportPayload(want, receipt) {
			return fmt.Errorf("receipt payload differs from recorded endpoint")
		}
		key := string(receipt.NLRI)
		if keys[key] {
			return fmt.Errorf("duplicate wire-route receipt")
		}
		keys[key], ids[id.String()] = true, true
	}
	return nil
}

func sameExportPayload(a, b exportReceipt) bool {
	if !bytes.Equal(a.NLRI, b.NLRI) || len(a.Attributes) != len(b.Attributes) {
		return false
	}
	for i := range a.Attributes {
		if !bytes.Equal(a.Attributes[i], b.Attributes[i]) {
			return false
		}
	}
	return true
}

func (r *ExportRecovery) observation(ctx context.Context) (string, []Type2RIBPath, error) {
	if err := r.speaker.Check(ctx); err != nil {
		return "", nil, fmt.Errorf("%w: provider attestation: %v", ownership.ErrBlocked, err)
	}
	before, err := r.speaker.Incarnation()
	if err != nil || before == "" {
		return "", nil, fmt.Errorf("%w: missing speaker incarnation: %v", ownership.ErrBlocked, err)
	}
	paths, err := r.source.Snapshot(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("%w: incomplete/unavailable RIB: %v", ownership.ErrBlocked, err)
	}
	if err := r.speaker.Check(ctx); err != nil {
		return "", nil, fmt.Errorf("%w: speaker changed during RIB read: %v", ownership.ErrBlocked, err)
	}
	after, err := r.speaker.Incarnation()
	if err != nil || before != after {
		return "", nil, fmt.Errorf("%w: RIB incarnation changed", ownership.ErrBlocked)
	}
	for _, path := range paths {
		if path.NLRI == nil {
			return "", nil, fmt.Errorf("%w: nil RIB NLRI", ownership.ErrBlocked)
		}
		if err := path.NLRI.Validate(); err != nil {
			return "", nil, fmt.Errorf("%w: invalid RIB NLRI: %v", ownership.ErrBlocked, err)
		}
	}
	return before, paths, nil
}

func matchingExportPaths(paths []Type2RIBPath, receipt exportReceipt) []Type2RIBPath {
	var found []Type2RIBPath
	want, err := BuildType2Route(receipt.Owned.Endpoint)
	if err != nil {
		return found
	}
	for _, path := range paths {
		if path.Local && path.NLRI != nil && sameType2NLRIValue(*path.NLRI, want.NLRI) {
			found = append(found, path)
		}
	}
	return found
}

// Recover resolves only confirmed absence or a confirmed UUID deletion.
// A retained unacknowledged AddPath can never become an ownership receipt.
func (r *ExportRecovery) Recover(ctx context.Context) ([]OwnedType2Route, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, paths, err := r.observation(ctx)
	if err != nil {
		return nil, err
	}
	_, pending, err := r.journal.Snapshot()
	if err != nil {
		return nil, err
	}
	next := exportRecoveryState{Version: 1, Incarnation: current}
	for _, receipt := range r.state.Receipts {
		found := matchingExportPaths(paths, receipt)
		if len(found) == 0 {
			continue
		}
		if current != receipt.Incarnation || len(found) != 1 || !ribPathMatchesEndpoint(found[0], receipt.Owned.Endpoint) ||
			(found[0].PathUUID != "" && found[0].PathUUID != receipt.Owned.PathUUID) {
			return nil, fmt.Errorf("%w: retained old or conflicting path; no adoption", ownership.ErrBlocked)
		}
		next.Receipts = append(next.Receipts, receipt)
	}
	if pending != nil {
		if pending.Operation == "export-checkpoint" {
			var checkpoint exportRecoveryState
			if err := decodeExportRecord(pending.Payload, &checkpoint); err != nil || checkpoint.Version != 1 {
				return nil, fmt.Errorf("%w: invalid exporter checkpoint", ownership.ErrBlocked)
			}
			for _, candidate := range checkpoint.Receipts {
				known := false
				for _, previous := range r.state.Receipts {
					if reflect.DeepEqual(candidate, previous) {
						known = true
					}
				}
				if !known {
					return nil, fmt.Errorf("%w: checkpoint cannot introduce ownership", ownership.ErrBlocked)
				}
			}
			if err := r.journal.Commit(pending.Sequence, next); err != nil {
				return nil, err
			}
			r.state = next
			return r.receipts(), nil
		}
		var intent exportIntent
		if err := decodeExportRecord(pending.Payload, &intent); err != nil {
			return nil, fmt.Errorf("%w: invalid exporter intent: %v", ownership.ErrBlocked, err)
		}
		want, err := durableExportRoute(intent.Route.Owned.Endpoint, intent.Incarnation, intent.Route.Owned.PathUUID)
		if err != nil || !sameExportPayload(want, intent.Route) || intent.Incarnation == "" {
			return nil, fmt.Errorf("%w: invalid intent payload", ownership.ErrBlocked)
		}
		found := matchingExportPaths(paths, intent.Route)
		switch pending.Operation {
		case "export-add":
			if intent.Route.Owned.PathUUID != "" || len(found) != 0 {
				return nil, fmt.Errorf("%w: unresolved AddPath with retained route", ownership.ErrBlocked)
			}
		case "export-delete":
			owned := false
			for _, receipt := range r.state.Receipts {
				if receipt.Owned.PathUUID == intent.Route.Owned.PathUUID && sameExportPayload(receipt, intent.Route) {
					owned = true
				}
			}
			if !owned {
				return nil, fmt.Errorf("%w: deletion intent has no durable UUID receipt", ownership.ErrBlocked)
			}
			if len(found) != 0 {
				if current != intent.Incarnation {
					return nil, fmt.Errorf("%w: withdrawal cannot cross incarnations", ownership.ErrBlocked)
				}
				if err := r.applier.Delete(ctx, intent.Route.Owned); err != nil {
					return nil, fmt.Errorf("%w: exact pending UUID withdrawal: %v", ownership.ErrBlocked, err)
				}
				verified, observed, err := r.observation(ctx)
				if err != nil || verified != current || len(matchingExportPaths(observed, intent.Route)) != 0 {
					return nil, fmt.Errorf("%w: pending withdrawal absence unverified: %v", ownership.ErrBlocked, err)
				}
			}
			next.Receipts = withoutExportReceipt(next.Receipts, intent.Route.Owned.PathUUID)
		default:
			return nil, fmt.Errorf("%w: unknown exporter operation %q", ownership.ErrBlocked, pending.Operation)
		}
		if err := r.journal.Commit(pending.Sequence, next); err != nil {
			return nil, err
		}
	} else if !reflect.DeepEqual(r.state, next) {
		seq, err := r.journal.Begin("export-checkpoint", next)
		if err != nil {
			return nil, err
		}
		if err := r.journal.Commit(seq, next); err != nil {
			return nil, err
		}
	}
	r.state = next
	return r.receipts(), nil
}

func withoutExportReceipt(receipts []exportReceipt, id string) []exportReceipt {
	out := make([]exportReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.Owned.PathUUID != id {
			out = append(out, receipt)
		}
	}
	return out
}

func (r *ExportRecovery) receipts() []OwnedType2Route {
	out := make([]OwnedType2Route, 0, len(r.state.Receipts))
	for _, receipt := range r.state.Receipts {
		owned := receipt.Owned
		owned.Endpoint = cloneLocalEndpoint(owned.Endpoint)
		out = append(out, owned)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PathUUID < out[j].PathUUID })
	return out
}

func (r *ExportRecovery) boundary(name string) error {
	if r.fault != nil {
		return r.fault(name)
	}
	return nil
}

func (r *ExportRecovery) Add(ctx context.Context, route Type2Route) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, paths, err := r.observation(ctx)
	if err != nil {
		return "", err
	}
	if current != r.state.Incarnation {
		return "", fmt.Errorf("%w: speaker requires recovery", ownership.ErrBlocked)
	}
	receipt, err := durableExportRoute(route.Endpoint, current, "")
	if err != nil {
		return "", err
	}
	if len(matchingExportPaths(paths, receipt)) != 0 {
		return "", fmt.Errorf("%w: foreign/unclaimed local route", ownership.ErrBlocked)
	}
	seq, err := r.journal.Begin("export-add", exportIntent{Incarnation: current, Route: receipt})
	if err != nil {
		return "", err
	}
	if err := r.boundary("add-intent"); err != nil {
		return "", err
	}
	id, err := r.applier.Add(ctx, route)
	if err != nil {
		return "", err
	}
	if err := r.boundary("add-executed"); err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return "", fmt.Errorf("%w: no acknowledged UUID", ownership.ErrBlocked)
	}
	verified, observed, err := r.observation(ctx)
	found := matchingExportPaths(observed, receipt)
	if err != nil || verified != current || len(found) != 1 || !ribPathMatchesEndpoint(found[0], route.Endpoint) ||
		(found[0].PathUUID != "" && found[0].PathUUID != id) {
		return "", fmt.Errorf("%w: AddPath readback unverified: %v", ownership.ErrBlocked, err)
	}
	receipt.Owned.PathUUID = id
	next := r.state
	next.Receipts = append(append([]exportReceipt(nil), next.Receipts...), receipt)
	if err := r.journal.Commit(seq, next); err != nil {
		return "", err
	}
	r.state = next
	if err := r.boundary("add-ack"); err != nil {
		return "", err
	}
	return id, nil
}

func (r *ExportRecovery) Delete(ctx context.Context, owned OwnedType2Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, _, err := r.observation(ctx)
	if err != nil {
		return err
	}
	var receipt *exportReceipt
	for i := range r.state.Receipts {
		if r.state.Receipts[i].Owned.PathUUID == owned.PathUUID && r.state.Receipts[i].Owned.Endpoint.Equivalent(owned.Endpoint) {
			receipt = &r.state.Receipts[i]
		}
	}
	if receipt == nil || receipt.Incarnation != current {
		return fmt.Errorf("%w: exact durable receipt in current incarnation required", ownership.ErrBlocked)
	}
	seq, err := r.journal.Begin("export-delete", exportIntent{Incarnation: current, Route: *receipt})
	if err != nil {
		return err
	}
	if err := r.boundary("delete-intent"); err != nil {
		return err
	}
	if err := r.applier.Delete(ctx, owned); err != nil {
		return err
	}
	if err := r.boundary("delete-executed"); err != nil {
		return err
	}
	verified, paths, err := r.observation(ctx)
	if err != nil || verified != current || len(matchingExportPaths(paths, *receipt)) != 0 {
		return fmt.Errorf("%w: withdrawal readback unverified: %v", ownership.ErrBlocked, err)
	}
	next := r.state
	next.Receipts = withoutExportReceipt(next.Receipts, owned.PathUUID)
	if err := r.journal.Commit(seq, next); err != nil {
		return err
	}
	r.state = next
	return r.boundary("delete-ack")
}

func (r *ExportRecovery) Close() error { return r.journal.Close() }

// Rebind is used only after closing the old transport. It does not transfer
// receipts: Recover still requires the new actual peer and authoritative RIB.
func (r *ExportRecovery) Rebind(source Type2RIBSource, applier Type2RouteApplier, speaker VerifiedSpeaker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if source == nil || applier == nil || speaker == nil {
		return fmt.Errorf("%w: missing replacement transport evidence", ownership.ErrBlocked)
	}
	r.source, r.applier, r.speaker = source, applier, speaker
	return nil
}
