package endpointauth

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type memoryFenceDisk struct {
	data []byte
}

type memoryFenceStore struct {
	disk                 *memoryFenceDisk
	loadErr              error
	emptyWithoutNotFound bool
	failBefore           bool
	failAfterSave        bool
}

func (s *memoryFenceStore) Load() ([]byte, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.disk.data == nil {
		if s.emptyWithoutNotFound {
			return nil, nil
		}
		return nil, ErrFenceStateNotFound
	}
	return append([]byte(nil), s.disk.data...), nil
}

func (s *memoryFenceStore) Persist(data []byte) error {
	if s.failBefore {
		return errors.New("injected failure before durable write")
	}
	s.disk.data = append([]byte(nil), data...)
	if s.failAfterSave {
		return errors.New("injected failure after durable write")
	}
	return nil
}

func (*memoryFenceStore) Close() error { return nil }

func openTestFencer(t *testing.T, disk *memoryFenceDisk) *Fencer {
	t.Helper()
	fencer, err := OpenFencer(&memoryFenceStore{disk: disk}, "installation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fencer.Close(); err != nil {
			t.Error(err)
		}
	})
	return fencer
}

func testPolicy(t *testing.T, revision, generation uint64, endpoints ...EndpointRecord) *Snapshot {
	t.Helper()
	for _, endpoint := range endpoints {
		if endpoint.AttachmentGeneration != generation {
			t.Fatalf("endpoint generation=%d, want fixture generation %d",
				endpoint.AttachmentGeneration, generation)
		}
	}
	document := Document{
		Version: CurrentVersion, Revision: revision,
		InstallationUID: "installation-1", NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: append([]EndpointRecord{}, endpoints...),
	}
	snapshot, err := NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testLifecyclePolicy(
	t *testing.T,
	revision uint64,
	endpoints []EndpointRecord,
	tombstones []EndpointTombstone,
) *Snapshot {
	t.Helper()
	if endpoints == nil {
		endpoints = []EndpointRecord{}
	}
	if tombstones == nil {
		tombstones = []EndpointTombstone{}
	}
	policy, err := NewSnapshot(Document{
		Version: LifecycleVersion, Revision: revision,
		InstallationUID: "installation-1", NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: endpoints, Tombstones: tombstones,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func testFenceRecord(generation uint64) EndpointRecord {
	return EndpointRecord{
		NetworkID: "net-a", EndpointID: "endpoint-1", AttachmentID: "attachment-1",
		AttachmentGeneration: generation, LogicalSwitchPort: "lsp-1",
		LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000123",
		MAC:                   "02:00:00:00:00:01", IP: "192.0.2.10",
	}
}

func TestFencerAcceptsFirstHigherAndIdempotentRevisions(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	first := testPolicy(t, 12, 4, testFenceRecord(4))
	accepted, err := fencer.Accept(first)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes := append([]byte(nil), disk.data...)
	if accepted.PolicyVersion() != first.PolicyVersion() {
		t.Fatalf("accepted version=%#v, want %#v", accepted.PolicyVersion(), first.PolicyVersion())
	}
	again, err := fencer.Accept(first)
	if err != nil || again.PolicyVersion() != accepted.PolicyVersion() {
		t.Fatalf("same revision/digest was not idempotent: %v", err)
	}
	if string(firstBytes) != string(disk.data) {
		t.Fatal("idempotent acceptance rewrote durable state")
	}

	higher := testPolicy(t, 13, 4, testFenceRecord(4))
	if _, err := fencer.Accept(higher); err != nil {
		t.Fatalf("higher revision rejected: %v", err)
	}
}

func TestFencerRejectsRevisionReplayAndDigestConflict(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	current := testPolicy(t, 12, 4, testFenceRecord(4))
	if _, err := fencer.Accept(current); err != nil {
		t.Fatal(err)
	}
	conflictDoc := validDocument()
	conflictDoc.Revision = 12
	conflictDoc.Endpoints[0].IP = "192.0.2.11"
	conflict, err := NewSnapshot(conflictDoc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fencer.Accept(conflict); err == nil {
		t.Fatal("same revision with a different digest was accepted")
	}
	if _, err := fencer.Accept(testPolicy(t, 11, 4, testFenceRecord(4))); err == nil {
		t.Fatal("lower revision was accepted")
	}
}

func TestFencerRejectsInvalidPolicyWithoutAdvancingHighWater(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	if _, err := fencer.Accept(nil); err == nil {
		t.Fatal("missing policy was accepted")
	}
	if _, err := fencer.Accept(&Snapshot{}); err == nil {
		t.Fatal("invalid zero-value policy was accepted")
	}
	if disk.data != nil {
		t.Fatal("invalid policy advanced durable state")
	}
	if _, err := fencer.Accept(testPolicy(t, 1, 4, testFenceRecord(4))); err != nil {
		t.Fatalf("valid policy after invalid input was rejected: %v", err)
	}
}

func TestFencerEmptyPolicyRevokesWithoutResettingTombstones(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	current := testPolicy(t, 12, 4, testFenceRecord(4))
	if _, err := fencer.Accept(current); err != nil {
		t.Fatal(err)
	}
	empty := testPolicy(t, 13, 0)
	if _, err := fencer.Accept(empty); err != nil {
		t.Fatal(err)
	}
	state, err := decodeFenceState(disk.data)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 13 || state.Digest != empty.Digest() || len(state.Endpoints) != 1 ||
		state.Endpoints[0].Active || state.Endpoints[0].HighestGeneration != 4 {
		t.Fatalf("empty policy reset lifecycle history: %#v", state)
	}
	if _, err := fencer.Accept(testPolicy(t, 14, 4, testFenceRecord(4))); err == nil {
		t.Fatal("revoked generation was reintroduced")
	}
	if _, err := fencer.Accept(testPolicy(t, 14, 5, testFenceRecord(5))); err != nil {
		t.Fatalf("new generation after revocation rejected: %v", err)
	}
}

func TestFencerRequiresIncreasingGenerationForReplacement(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	original := testFenceRecord(4)
	if _, err := fencer.Accept(testPolicy(t, 1, 4, original)); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.AttachmentID = "attachment-2"
	replacement.AttachmentGeneration = 4
	if _, err := fencer.Accept(testPolicy(t, 2, 4, replacement)); err == nil {
		t.Fatal("attachment replacement reused an existing generation")
	}
	replacement.AttachmentGeneration = 5
	replacement.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000456"
	if _, err := fencer.Accept(testPolicy(t, 2, 5, replacement)); err != nil {
		t.Fatalf("higher-generation attachment replacement rejected: %v", err)
	}
}

func TestFencerDurablyAcceptsDeletionTombstoneAndRejectsReplayAfterRestart(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	original := testFenceRecord(4)
	if _, err := fencer.Accept(testLifecyclePolicy(t, 1, []EndpointRecord{original}, []EndpointTombstone{})); err != nil {
		t.Fatal(err)
	}
	deleted := testLifecyclePolicy(t, 2, []EndpointRecord{}, []EndpointTombstone{{
		Reason: TombstoneDeleted, Record: original,
	}})
	accepted, err := fencer.Accept(deleted)
	if err != nil || accepted == nil || !accepted.IsCurrent() {
		t.Fatalf("valid deletion tombstone was not accepted: accepted=%v err=%v", accepted != nil, err)
	}
	if _, err := fencer.Accept(deleted); err != nil {
		t.Fatalf("same revision/tombstone was not idempotent: %v", err)
	}
	state, err := decodeFenceState(disk.data)
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Endpoints[0]
	if entry.Active || entry.HighestGeneration != 4 || entry.RetiredGeneration != 4 ||
		entry.RetirementReason != string(TombstoneDeleted) || entry.RetiredRecordDigest != endpointRecordDigest(original) {
		t.Fatalf("durable lifecycle state lost exact deletion evidence: %#v", entry)
	}
	if err := fencer.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openTestFencer(t, disk)
	if _, err := restarted.Accept(deleted); err != nil {
		t.Fatalf("restart did not recover accepted tombstone: %v", err)
	}
	if _, err := restarted.Accept(testLifecyclePolicy(t, 3, []EndpointRecord{original}, []EndpointTombstone{})); err == nil {
		t.Fatal("retired attachment generation replayed after restart")
	}
	if _, err := restarted.Accept(testLifecyclePolicy(t, 3, []EndpointRecord{testFenceRecord(5)}, []EndpointTombstone{})); err != nil {
		t.Fatalf("new generation after deletion was rejected: %v", err)
	}
}

func TestFencerReplacementRequiresExactTombstoneAndRecoversBothGenerations(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	old := testFenceRecord(4)
	if _, err := fencer.Accept(testLifecyclePolicy(t, 1, []EndpointRecord{old}, []EndpointTombstone{})); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.AttachmentID, newer.AttachmentGeneration = "attachment-2", 5
	newer.LogicalSwitchPort, newer.LogicalSwitchPortUUID = "lsp-2", "00000000-0000-4000-8000-000000000456"
	newer.MAC, newer.IP = "02:00:00:00:00:02", "192.0.2.11"
	withoutTombstone := testLifecyclePolicy(t, 2, []EndpointRecord{newer}, []EndpointTombstone{})
	if _, err := fencer.Accept(withoutTombstone); err == nil {
		t.Fatal("version-2 attachment replacement without explicit tombstone was accepted")
	}
	replacement := testLifecyclePolicy(t, 2, []EndpointRecord{newer}, []EndpointTombstone{{
		Reason: TombstoneReplaced, Record: old,
	}})
	if _, err := fencer.Accept(replacement); err != nil {
		t.Fatalf("exact replacement tombstone was rejected: %v", err)
	}
	if _, err := fencer.Accept(replacement); err != nil {
		t.Fatalf("replacement snapshot was not idempotent: %v", err)
	}
	state, err := decodeFenceState(disk.data)
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Endpoints[0]
	if !entry.Active || entry.HighestGeneration != 5 || entry.RetiredGeneration != 4 ||
		entry.RetirementReason != string(TombstoneReplaced) || entry.RetiredRecordDigest != endpointRecordDigest(old) {
		t.Fatalf("replacement did not retain the old incarnation proof: %#v", entry)
	}
}

func TestFencerTombstonePersistenceFailuresNeverPublishAcceptance(t *testing.T) {
	for _, failAfterSave := range []bool{false, true} {
		name := "before durable write"
		if failAfterSave {
			name = "after durable write"
		}
		t.Run(name, func(t *testing.T) {
			disk := &memoryFenceDisk{}
			initial := testFenceRecord(4)
			fencer := openTestFencer(t, disk)
			if _, err := fencer.Accept(testLifecyclePolicy(t, 1, []EndpointRecord{initial}, []EndpointTombstone{})); err != nil {
				t.Fatal(err)
			}
			if err := fencer.Close(); err != nil {
				t.Fatal(err)
			}
			store := &memoryFenceStore{disk: disk, failBefore: !failAfterSave, failAfterSave: failAfterSave}
			fencer, err := OpenFencer(store, "installation-1", "node-1")
			if err != nil {
				t.Fatal(err)
			}
			deleted := testLifecyclePolicy(t, 2, []EndpointRecord{}, []EndpointTombstone{{
				Reason: TombstoneDeleted, Record: initial,
			}})
			if accepted, err := fencer.Accept(deleted); err == nil || accepted != nil {
				t.Fatal("failed persistence returned an accepted deletion snapshot")
			}
			_ = fencer.Close()

			restarted := openTestFencer(t, disk)
			accepted, err := restarted.Accept(deleted)
			if err != nil || accepted == nil || !accepted.IsCurrent() {
				t.Fatalf("restart did not safely resolve interrupted tombstone persistence: accepted=%v err=%v",
					accepted != nil, err)
			}
		})
	}
}

func TestFencerMigratesVersionOneDurableState(t *testing.T) {
	legacy := []byte(`{"version":1,"installationUID":"installation-1","nodeUID":"node-1","revision":7,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoints":[{"networkID":"net-a","endpointID":"endpoint-1","highestGeneration":4,"active":false}]}`)
	state, err := decodeFenceState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != fenceStateVersion || state.Endpoints[0].HighestGeneration != 4 || state.Endpoints[0].Active {
		t.Fatalf("legacy fence was not migrated in memory: %#v", state)
	}
}

func TestFencerRejectsLSPUUIDReplacementWithoutGenerationBump(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	original := testFenceRecord(4)
	if _, err := fencer.Accept(testPolicy(t, 1, 4, original)); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000456"
	if _, err := fencer.Accept(testPolicy(t, 2, 4, replacement)); err == nil {
		t.Fatal("LSP UUID replacement reused the active attachment generation")
	}
}

func TestFencerRevocationAndRestartPreventRetiredGenerationReplay(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	first := testPolicy(t, 1, 4, testFenceRecord(4))
	if _, err := fencer.Accept(first); err != nil {
		t.Fatal(err)
	}
	revoked := testPolicy(t, 2, 0)
	if _, err := fencer.Accept(revoked); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openTestFencer(t, disk)
	if _, err := restarted.Accept(first); err == nil {
		t.Fatal("retired generation replayed after restart")
	}
	reintroduced := testPolicy(t, 3, 5, testFenceRecord(5))
	if _, err := restarted.Accept(reintroduced); err != nil {
		t.Fatalf("new generation after restart rejected: %v", err)
	}
}

func TestFencerCrashBeforePersistenceLeavesPriorState(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	store := &memoryFenceStore{disk: disk, failBefore: true}
	if err := fencer.Close(); err != nil {
		t.Fatal(err)
	}
	fencer, err := OpenFencer(store, "installation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fencer.Accept(testPolicy(t, 1, 4, testFenceRecord(4))); err == nil {
		t.Fatal("injected pre-persistence failure returned acceptance")
	}
	if disk.data != nil {
		t.Fatal("pre-persistence failure changed durable state")
	}
	if _, err := fencer.Accept(testPolicy(t, 2, 5, testFenceRecord(5))); err == nil {
		t.Fatal("fencer was not quarantined after persistence failure")
	}
	_ = fencer.Close()

	restarted := openTestFencer(t, disk)
	if _, err := restarted.Accept(testPolicy(t, 1, 4, testFenceRecord(4))); err != nil {
		t.Fatalf("restart could not accept policy after prior state remained unchanged: %v", err)
	}
}

func TestFencerCrashAfterPersistenceBeforePublicationRecoversIdempotently(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer, err := OpenFencer(&memoryFenceStore{disk: disk, failAfterSave: true}, "installation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	policy := testPolicy(t, 7, 4, testFenceRecord(4))
	accepted, err := fencer.Accept(policy)
	if err == nil || accepted != nil {
		t.Fatal("post-persistence failure published an accepted snapshot")
	}
	if _, ok := (&AcceptedSnapshot{}).Snapshot(); ok {
		t.Fatal("unbound accepted snapshot was usable")
	}
	_ = fencer.Close()

	restarted := openTestFencer(t, disk)
	accepted, err = restarted.Accept(policy)
	if err != nil || accepted == nil || !accepted.IsCurrent() {
		t.Fatalf("durably accepted policy was not recoverable: accepted=%v err=%v",
			accepted != nil, err)
	}
}

func TestFencerRejectsCorruptAndWrongScopeState(t *testing.T) {
	for name, disk := range map[string]*memoryFenceDisk{
		"corrupt":     {data: []byte(`{"version":`)},
		"wrong scope": {},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "wrong scope" {
				store := &memoryFenceStore{disk: &memoryFenceDisk{}}
				fencer, err := OpenFencer(store, "installation-1", "node-1")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fencer.Accept(testPolicy(t, 1, 4, testFenceRecord(4))); err != nil {
					t.Fatal(err)
				}
				if err := fencer.Close(); err != nil {
					t.Fatal(err)
				}
				disk = store.disk
			}
			_, err := OpenFencer(&memoryFenceStore{disk: disk}, "installation-2", "node-1")
			if err == nil {
				t.Fatal("corrupt or wrong-scope state was opened")
			}
		})
	}
}

func TestFencerRejectsEmptyStoreResultWithoutNotFoundSignal(t *testing.T) {
	_, err := OpenFencer(
		&memoryFenceStore{disk: &memoryFenceDisk{}, emptyWithoutNotFound: true},
		"installation-1", "node-1",
	)
	if err == nil {
		t.Fatal("empty state without explicit not-found result was treated as fresh")
	}
}

func TestFencerRejectsInaccessibleStore(t *testing.T) {
	_, err := OpenFencer(
		&memoryFenceStore{disk: &memoryFenceDisk{}, loadErr: errors.New("injected read failure")},
		"installation-1", "node-1",
	)
	if err == nil {
		t.Fatal("inaccessible durable state was treated as fresh")
	}
}

func TestFencerRejectsMalformedLifecycleState(t *testing.T) {
	state := fenceState{
		Version: fenceStateVersion, InstallationUID: "installation-1", NodeUID: "node-1",
		Revision: 3, Digest: strings.Repeat("a", 64),
		Endpoints: []fenceEndpoint{{
			NetworkID: "net-a", EndpointID: "endpoint-1", HighestGeneration: 0,
		}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenFencer(&memoryFenceStore{disk: &memoryFenceDisk{data: data}}, "installation-1", "node-1")
	if err == nil {
		t.Fatal("malformed lifecycle state was opened")
	}
}

func TestFencerSerializesConcurrentPolicyUpdates(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	if _, err := fencer.Accept(testPolicy(t, 1, 4, testFenceRecord(4))); err != nil {
		t.Fatal(err)
	}
	type result struct {
		revision uint64
		err      error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	policies := []*Snapshot{
		testPolicy(t, 2, 4, testFenceRecord(4)),
		testPolicy(t, 3, 4, testFenceRecord(4)),
	}
	for i, revision := range []uint64{2, 3} {
		policy := policies[i]
		wait.Add(1)
		go func(revision uint64) {
			defer wait.Done()
			_, err := fencer.Accept(policy)
			results <- result{revision: revision, err: err}
		}(revision)
	}
	wait.Wait()
	close(results)
	for result := range results {
		if result.revision == 3 && result.err != nil {
			t.Fatalf("highest concurrent revision failed: %v", result.err)
		}
		if result.revision == 2 && result.err != nil && !strings.Contains(result.err.Error(), "below accepted high-water") {
			t.Fatalf("unexpected lower-revision error: %v", result.err)
		}
	}
	state, err := decodeFenceState(disk.data)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 3 {
		t.Fatalf("durable revision=%d, want 3", state.Revision)
	}
}

func TestAcceptedSnapshotBecomesUnusableAfterNewerPolicy(t *testing.T) {
	disk := &memoryFenceDisk{}
	fencer := openTestFencer(t, disk)
	old, err := fencer.Accept(testPolicy(t, 1, 4, testFenceRecord(4)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fencer.Accept(testPolicy(t, 2, 5, testFenceRecord(5))); err != nil {
		t.Fatal(err)
	}
	if old.IsCurrent() {
		t.Fatal("superseded accepted snapshot remained eligible")
	}
}
