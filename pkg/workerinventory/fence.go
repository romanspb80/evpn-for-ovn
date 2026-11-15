package workerinventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

const fenceOperation = "worker-inventory-accept"

type fenceState struct {
	Version      int    `json:"version"`
	ClusterUID   string `json:"clusterUID"`
	Installation string `json:"installationUID"`
	NodeName     string `json:"nodeName"`
	NodeUID      string `json:"nodeUID"`
	Revision     uint64 `json:"revision"`
	Digest       string `json:"digest"`
	WorkerDigest string `json:"workerDigest"`
}

// Fence serializes acceptance of publisher-authenticated declarations for one
// node. It is not a publisher authentication mechanism.
type Fence struct {
	mu       sync.Mutex
	journal  *ownership.Journal
	cluster  string
	install  string
	nodeName string
	nodeUID  string
	state    fenceState
}

func fenceDomain(cluster, install, node string) (ownership.Domain, error) {
	if !canonicalUUID(cluster) || !canonicalUUID(install) || !canonicalUUID(node) {
		return ownership.Domain{}, fmt.Errorf("inventory fence requires canonical cluster, installation and node UUIDs")
	}
	scope := sha256.Sum256([]byte("worker-inventory-v1:" + cluster))
	return ownership.Domain{
		InstallationUID: install, NodeUID: node,
		BootID:           uuid.NewSHA1(uuid.NameSpaceOID, []byte("worker-inventory-fence-v1")).String(),
		NetworkNamespace: "worker-inventory:" + cluster,
		ConfigDigest:     hex.EncodeToString(scope[:]),
	}, nil
}

func OpenFence(directory, cluster, installation, nodeName, nodeUID string) (*Fence, error) {
	if !validName(nodeName) {
		return nil, fmt.Errorf("inventory fence requires node name")
	}
	domain, err := fenceDomain(cluster, installation, nodeUID)
	if err != nil {
		return nil, err
	}
	journal, err := ownership.Open(directory, domain, true)
	if err != nil {
		return nil, err
	}
	f := &Fence{journal: journal, cluster: cluster, install: installation, nodeName: nodeName, nodeUID: nodeUID}
	raw, pending, err := journal.Snapshot()
	if err == nil && pending != nil {
		err = fmt.Errorf("%w: unresolved inventory acceptance requires operator intervention", ownership.ErrBlocked)
	}
	if err == nil && string(raw) != "null" {
		err = decodeFence(raw, &f.state)
		if err == nil {
			err = f.validateState(f.state)
		}
	}
	if err != nil {
		return nil, errors.Join(err, journal.Close())
	}
	return f, nil
}

func decodeFence(data []byte, out *fenceState) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("inventory fence contains trailing data")
	}
	return nil
}

func digestValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func (f *Fence) validateState(s fenceState) error {
	if s.Version != 1 || s.ClusterUID != f.cluster || s.Installation != f.install || s.NodeName != f.nodeName ||
		s.NodeUID != f.nodeUID || s.Revision == 0 || !digestValid(s.Digest) || !digestValid(s.WorkerDigest) {
		return fmt.Errorf("%w: invalid inventory fence scope or state", ownership.ErrBlocked)
	}
	return nil
}

// Accept must be called only after independent publisher authentication and
// verification of the Kubernetes Node name and UID. Existing worker entries
// are immutable; higher revisions may add or remove other workers.
func (f *Fence) Accept(in Inventory) (Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, digest, err := in.Canonical()
	if err != nil {
		return Worker{}, err
	}
	if in.ClusterUID != f.cluster || in.InstallationUID != f.install {
		return Worker{}, fmt.Errorf("inventory scope differs from durable fence")
	}
	var canonical Inventory
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return Worker{}, err
	}
	worker, err := canonical.Select(f.nodeName, f.nodeUID)
	if err != nil {
		return Worker{}, err
	}
	workerBytes, err := json.Marshal(worker)
	if err != nil {
		return Worker{}, err
	}
	hash := sha256.Sum256(workerBytes)
	workerDigest := hex.EncodeToString(hash[:])
	if f.state.Revision > in.Revision {
		return Worker{}, fmt.Errorf("inventory revision %d is below accepted revision %d", in.Revision, f.state.Revision)
	}
	if f.state.Revision != 0 && f.state.WorkerDigest != workerDigest {
		return Worker{}, fmt.Errorf("worker assignment differs from durable ownership binding")
	}
	if f.state.Revision == in.Revision && f.state.Revision != 0 {
		if f.state.Digest != digest {
			return Worker{}, fmt.Errorf("inventory revision conflicts with accepted digest")
		}
		return worker, nil
	}
	next := fenceState{Version: 1, ClusterUID: f.cluster, Installation: f.install, NodeName: f.nodeName,
		NodeUID: f.nodeUID, Revision: in.Revision, Digest: digest, WorkerDigest: workerDigest}
	seq, err := f.journal.Begin(fenceOperation, next)
	if err != nil {
		return Worker{}, err
	}
	if err := f.journal.Commit(seq, next); err != nil {
		return Worker{}, err
	}
	f.state = next
	return worker, nil
}

func (f *Fence) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.journal.Close()
}
