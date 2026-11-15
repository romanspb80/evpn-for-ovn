package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"

	"github.com/google/uuid"
	evpnv1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	"github.com/romanspb80/evpn-for-ovn/pkg/workerinventory"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workerInventoryName = "evpn-worker-inventory"
	reservationsName    = "evpn-worker-reservations"
)

type inventoryReservation struct {
	InstallationUID string                 `json:"installationUID"`
	Worker          workerinventory.Worker `json:"worker"`
}

type inventoryHead struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type reservationState struct {
	Version      int                      `json:"version"`
	ClusterUID   string                   `json:"clusterUID"`
	Reservations []inventoryReservation   `json:"reservations"`
	Heads        map[string]inventoryHead `json:"heads"`
}

// prepareWorkerInventory verifies the exact selected Node set and reserves
// identities before returning a publishable inventory resource.
func (r *EvpnCoreReconciler) prepareWorkerInventory(ctx context.Context, core *evpnv1.EvpnCore) (*unstructured.Unstructured, error) {
	spec := core.Spec.WorkerInventory
	if spec == nil {
		return nil, nil
	}
	in := workerinventory.Inventory{Version: 1, Revision: spec.Revision, ClusterUID: spec.ClusterUID,
		InstallationUID: string(core.UID), Workers: spec.Workers}
	data, digest, err := in.Canonical()
	if err != nil {
		return nil, fmt.Errorf("invalid worker inventory: %w", err)
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, err
	}
	var nodes corev1.NodeList
	if err := r.reader().List(ctx, &nodes, client.MatchingLabels(core.Spec.NodeAgent.NodeSelector)); err != nil {
		return nil, fmt.Errorf("list selected Nodes: %w", err)
	}
	if len(nodes.Items) != len(in.Workers) {
		return nil, fmt.Errorf("selected Node count %d does not match worker inventory count %d", len(nodes.Items), len(in.Workers))
	}
	for _, node := range nodes.Items {
		if !node.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("selected Node %s is deleting", node.Name)
		}
		if _, err := in.Select(node.Name, string(node.UID)); err != nil {
			return nil, fmt.Errorf("selected Node %s UID %s: %w", node.Name, node.UID, err)
		}
	}
	if err := r.reserveWorkers(ctx, core, in, digest); err != nil {
		return nil, err
	}
	appNS, _ := namespaces(core.Spec)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": workerInventoryName, "namespace": appNS},
		"data": map[string]interface{}{"inventory.json": string(data), "sha256": digest,
			"revision": strconv.FormatUint(in.Revision, 10), "installationUID": in.InstallationUID},
	}}
	return obj, nil
}

func (r *EvpnCoreReconciler) reserveWorkers(ctx context.Context, core *evpnv1.EvpnCore, in workerinventory.Inventory, digest string) error {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: LockNamespace, Name: reservationsName}
	if err := r.reader().Get(ctx, key, &cm); err != nil {
		return fmt.Errorf("administrator-provisioned inventory reservations unavailable: %w", err)
	}
	if !cm.DeletionTimestamp.IsZero() || cm.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return &ownershipConflict{"inventory reservation ConfigMap is deleting or lacks its required administrator-provisioned marker"}
	}
	var state reservationState
	decoder := json.NewDecoder(bytes.NewBufferString(cm.Data["reservations.json"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("invalid inventory reservations: %w", err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("inventory reservations contain trailing data")
	}
	if state.Version != 1 || state.ClusterUID != in.ClusterUID || state.Heads == nil || state.Reservations == nil {
		return fmt.Errorf("inventory reservation scope or state is invalid")
	}
	for installation, head := range state.Heads {
		id, err := uuid.Parse(installation)
		hash, hashErr := hex.DecodeString(head.Digest)
		if err != nil || id == uuid.Nil || id.String() != installation || head.Revision == 0 ||
			hashErr != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != head.Digest {
			return fmt.Errorf("invalid retained inventory revision")
		}
	}
	for i, reservation := range state.Reservations {
		if _, ok := state.Heads[reservation.InstallationUID]; !ok {
			return fmt.Errorf("retained inventory reservation has no revision history")
		}
		old := workerinventory.Inventory{Version: 1, Revision: 1, ClusterUID: state.ClusterUID,
			InstallationUID: reservation.InstallationUID, Workers: []workerinventory.Worker{reservation.Worker}}
		canonical, _, err := old.Canonical()
		if err != nil {
			return fmt.Errorf("invalid retained inventory reservation: %w", err)
		}
		var normalized workerinventory.Inventory
		if err := json.Unmarshal(canonical, &normalized); err != nil || !reflect.DeepEqual(normalized.Workers[0], reservation.Worker) {
			return fmt.Errorf("retained inventory reservation is not canonical")
		}
		for _, other := range state.Reservations[:i] {
			a, b := reservation.Worker, other.Worker
			if a.NodeName == b.NodeName || a.NodeUID == b.NodeUID || a.Chassis == b.Chassis ||
				a.SpeakerRouterID == b.SpeakerRouterID || a.LocalVTEP == b.LocalVTEP {
				return fmt.Errorf("retained inventory reservations conflict")
			}
		}
	}
	head := state.Heads[in.InstallationUID]
	if in.Revision < head.Revision || (in.Revision == head.Revision && head.Revision != 0 && head.Digest != digest) {
		return fmt.Errorf("inventory revision is stale or conflicts with its reserved digest")
	}
	for _, worker := range in.Workers {
		found := false
		for _, reservation := range state.Reservations {
			old := reservation.Worker
			same := reservation.InstallationUID == in.InstallationUID && old.NodeUID == worker.NodeUID && old.NodeName == worker.NodeName
			if same {
				if !reflect.DeepEqual(old, worker) {
					return fmt.Errorf("worker %s differs from its reserved assignment", worker.NodeName)
				}
				found = true
				continue
			}
			if old.NodeName == worker.NodeName || old.NodeUID == worker.NodeUID || old.Chassis == worker.Chassis ||
				old.SpeakerRouterID == worker.SpeakerRouterID || old.LocalVTEP == worker.LocalVTEP {
				return fmt.Errorf("worker %s conflicts with a retained worker identity reservation", worker.NodeName)
			}
		}
		if !found {
			state.Reservations = append(state.Reservations, inventoryReservation{InstallationUID: in.InstallationUID, Worker: worker})
		}
	}
	state.Heads[in.InstallationUID] = inventoryHead{Revision: in.Revision, Digest: digest}
	next, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if cm.Data["reservations.json"] == string(next) {
		return nil
	}
	if _, err := r.currentSnapshot(ctx, core); err != nil {
		return err
	}
	cm.Data["reservations.json"] = string(next)
	if err := r.Update(ctx, &cm); err != nil {
		return fmt.Errorf("persist inventory reservations: %w", err)
	}
	return nil
}
