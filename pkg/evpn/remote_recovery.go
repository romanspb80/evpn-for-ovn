package evpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"golang.org/x/sys/unix"
)

// The generic journal version and this resource-specific version are separate.
type remoteReceiptState struct {
	Version    int
	Controller string
	Target     string
	Networks   []RemoteNetlinkNetwork
	Devices    map[string]RemoteDeviceIdentity
	Receipts   map[string]*RemoteNetlinkReceipt
	Latest     map[string]uint64
	Snapshot   RemoteImportSnapshot
	Quarantine string
}

func closeOwnershipJournal(j *ownership.Journal) error {
	if j == nil {
		return nil
	}
	return j.Close()
}

func (s *ownedNetlinkState) durableState() remoteReceiptState {
	return remoteReceiptState{1, s.controller, s.target, s.config.Networks,
		s.devices, s.receipts, s.latest, s.snapshot, s.blocked}
}

func (s *ownedNetlinkState) openRecovery(boot, namespace string) error {
	s.recoveryState = "VERIFYING_OWNERSHIP"
	d := s.config.Recovery.Domain
	if d.BootID != boot || d.NetworkNamespace != namespace {
		return fmt.Errorf("%w: actual boot/network namespace differs from journal domain", ownership.ErrBlocked)
	}
	path := filepath.Join(s.config.StateDirectory, "receipts-v1")
	marker := filepath.Join(s.config.StateDirectory, "remote-netlink-owner")
	_, markerErr := os.Lstat(marker)
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return fmt.Errorf("%w: inspect legacy marker: %v", ownership.ErrBlocked, markerErr)
	}
	if markerErr == nil {
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("%w: marker-only installation has no valid receipt journal; explicit administrator migration required: %v", ownership.ErrBlocked, err)
		}
	}
	j, err := ownership.Open(path, d, true)
	if err != nil {
		return err
	}
	s.journal = j
	state, pending, err := j.Snapshot()
	if err != nil {
		return err
	}
	if pending != nil {
		return fmt.Errorf("%w: unresolved importer mutation %s sequence %d; do not infer ownership from kernel tuples",
			ownership.ErrBlocked, pending.Operation, pending.Sequence)
	}
	if string(state) == "null" {
		if markerErr == nil {
			return fmt.Errorf("%w: marker exists without durable importer receipts", ownership.ErrBlocked)
		}
		return nil
	}
	var r remoteReceiptState
	decoder := json.NewDecoder(bytes.NewReader(state))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return fmt.Errorf("%w: invalid importer receipt state: %v", ownership.ErrBlocked, err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("%w: trailing importer state", ownership.ErrBlocked)
	}
	if r.Quarantine != "" {
		return fmt.Errorf("%w: durable importer quarantine: %s", ownership.ErrBlocked, r.Quarantine)
	}
	controller, err := uuid.Parse(r.Controller)
	if err != nil || controller == uuid.Nil || r.Version != 1 || r.Target != s.target ||
		!reflect.DeepEqual(r.Networks, s.config.Networks) || len(r.Devices) != 2*len(r.Networks) ||
		r.Receipts == nil || r.Latest == nil {
		return fmt.Errorf("%w: invalid importer version, domain, devices or controller", ownership.ErrBlocked)
	}
	s.controller, s.devices, s.receipts, s.latest, s.snapshot =
		r.Controller, r.Devices, r.Receipts, r.Latest, r.Snapshot
	s.markerPath = marker
	if err := s.verifyMarker(); err != nil {
		return err
	}
	return s.validateReceipts()
}

func (s *ownedNetlinkState) verifyMarker() error {
	f, err := os.OpenFile(s.markerPath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("%w: journal requires its original marker: %v", ownership.ErrBlocked, err)
	}
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0600 || stat.Nlink != 1 {
		return fmt.Errorf("%w: unsafe importer marker", ownership.ErrBlocked)
	}
	value, err := io.ReadAll(io.LimitReader(f, 513))
	if err != nil || string(value) != s.controller+"\n"+s.target+"\n" {
		return fmt.Errorf("%w: importer marker differs from durable controller/domain: %v", ownership.ErrBlocked, err)
	}
	return nil
}

func (s *ownedNetlinkState) validateReceipts() error {
	for _, n := range s.config.Networks {
		br, brOK := s.devices[n.BridgeDevice]
		vx, vxOK := s.devices[n.VXLANDevice]
		if !brOK || !vxOK || br.Name != n.BridgeDevice || vx.Name != n.VXLANDevice ||
			br.Ifindex <= 0 || vx.Ifindex <= 0 || br.Kind != "bridge" || vx.Kind != "vxlan" ||
			vx.VNI != int(n.VNI) || vx.MasterIndex != br.Ifindex {
			return fmt.Errorf("%w: invalid durable device topology", ownership.ErrBlocked)
		}
	}
	slots := map[string]bool{}
	for key, r := range s.receipts {
		if r == nil || r.Controller != s.controller || r.Target != s.target ||
			r.Endpoint.Key != key || key != remoteHash([]string{r.Endpoint.NetworkID, r.Endpoint.MAC, r.Endpoint.IP}) ||
			r.Incarnation == 0 || r.Generation == 0 || r.Generation > s.snapshot.Generation ||
			s.latest[key] != r.Incarnation || r.Endpoint.ValidationState != "validated" || len(r.Endpoint.Routes) == 0 {
			return fmt.Errorf("%w: invalid durable importer receipt identity/generation", ownership.ErrBlocked)
		}
		switch r.Status {
		case "partially-applied", "applied", "partially-withdrawn":
		default:
			return fmt.Errorf("%w: invalid durable route status %q", ownership.ErrBlocked, r.Status)
		}
		if r.Retiring != (r.Status == "partially-withdrawn") {
			return fmt.Errorf("%w: inconsistent retirement evidence", ownership.ErrBlocked)
		}
		found := false
		for _, n := range s.config.Networks {
			found = found || n == r.Network
		}
		if !found || r.Network.NetworkID != r.Endpoint.NetworkID || r.Network.VNI != r.Endpoint.VNI ||
			!reflect.DeepEqual(r.Devices, []RemoteDeviceIdentity{s.devices[r.Network.BridgeDevice], s.devices[r.Network.VXLANDevice]}) {
			return fmt.Errorf("%w: receipt device/network mapping conflict", ownership.ErrBlocked)
		}
		expected := s.newReceipt(r.Network, receiptRoute(r))
		if expected.FDB.Request != r.FDB.Request || (expected.Neighbor == nil) != (r.Neighbor == nil) ||
			(expected.Neighbor != nil && expected.Neighbor.Request != r.Neighbor.Request) {
			return fmt.Errorf("%w: durable kernel tuple differs from validated endpoint", ownership.ErrBlocked)
		}
		for _, o := range []*RemoteObjectReceipt{&r.FDB, r.Neighbor} {
			if o == nil {
				continue
			}
			slot := fmt.Sprintf("%d/%d/%s", o.Request.Family, o.Request.Ifindex, o.Request.IP)
			if o.Request.Family == unix.AF_BRIDGE {
				slot = fmt.Sprintf("%d/%d/%s", o.Request.Family, o.Request.Ifindex, o.Request.MAC)
			}
			if slots[slot] {
				return fmt.Errorf("%w: duplicate durable kernel slot", ownership.ErrBlocked)
			}
			slots[slot] = true
			switch o.Status {
			case "confirmed":
				want := o.Request
				want.Type = o.Observed.Type
				if !o.ACK || o.DeleteACK || o.Observed != want ||
					(o.Request.Family == unix.AF_BRIDGE && o.Observed.Type != o.Request.Type) ||
					(o.Request.Family != unix.AF_BRIDGE && o.Observed.Type != unix.RTN_UNICAST && o.Observed.Type != unix.RTN_BROADCAST) {
					return fmt.Errorf("%w: invalid confirmed kernel readback", ownership.ErrBlocked)
				}
			case "unattempted", "failed":
				if o.ACK || o.DeleteACK || o.Observed != (RemoteKernelEntry{}) {
					return fmt.Errorf("%w: unconfirmed receipt contains acknowledgement evidence", ownership.ErrBlocked)
				}
			case "absent-confirmed":
			default:
				return fmt.Errorf("%w: unresolved kernel receipt status %q", ownership.ErrBlocked, o.Status)
			}
		}
	}
	return nil
}

func receiptRoute(r *RemoteNetlinkReceipt) ImportedType2Route {
	ep := r.Endpoint
	return ImportedType2Route{Key: ep.Key, NetworkName: ep.NetworkID, VNI: ep.VNI, MAC: ep.MAC, IP: ep.IP,
		RemoteVTEP: ep.RemoteVTEP, VXLANDevice: r.Network.VXLANDevice,
		BridgeDevice: r.Network.BridgeDevice, NeighborDevice: r.Network.BridgeDevice,
		incarnation: r.Incarnation, generation: r.Generation, validated: &ep, retiring: r.Retiring}
}

func (s *ownedNetlinkState) hasConfirmedEntry(entry RemoteKernelEntry) bool {
	for _, r := range s.receipts {
		for _, o := range []*RemoteObjectReceipt{&r.FDB, r.Neighbor} {
			if o != nil && o.Status == "confirmed" && o.ACK && o.Observed == entry {
				return true
			}
		}
	}
	return false
}

func (s *ownedNetlinkState) verifyRecovered(ctx context.Context) error {
	if err := s.validateReceipts(); err != nil {
		return err
	}
	for _, r := range s.receipts {
		for _, o := range []*RemoteObjectReceipt{&r.FDB, r.Neighbor} {
			if o == nil {
				continue
			}
			found, err := s.slot(ctx, o.Request)
			if err != nil {
				return err
			}
			if o.Status == "confirmed" && len(found) == 1 && found[0] == o.Observed {
				continue
			}
			if len(found) != 0 {
				s.blocked = "foreign/conflicting kernel slot during recovery"
				return s.quarantineError()
			}
			// Authoritatively observed absence is not an ownership claim.
			// Missing desired objects can be recreated only after fresh BGP polls.
			if r.Retiring {
				o.Status = "absent-confirmed"
			} else {
				o.Status, o.ACK, o.DeleteACK = "unattempted", false, false
				o.Observed = RemoteKernelEntry{}
				r.Status = "partially-applied"
			}
		}
	}
	return s.checkpoint()
}

func (s *ownedNetlinkState) boundary(name string) {
	if s.fault != nil {
		s.fault(name)
	}
}

func (s *ownedNetlinkState) beginMutation(operation string, object *RemoteObjectReceipt) (uint64, error) {
	if s.journal == nil {
		return 0, nil
	}
	seq, err := s.journal.Begin(operation, struct {
		State  remoteReceiptState
		Object RemoteObjectReceipt
	}{s.durableState(), *object})
	if err != nil {
		s.blocked, s.recoveryState = err.Error(), "RECOVERY_BLOCKED"
	}
	return seq, err
}

func (s *ownedNetlinkState) ackMutation(sequence uint64) error {
	if s.journal == nil {
		return nil
	}
	if err := s.journal.Commit(sequence, s.durableState()); err != nil {
		s.blocked, s.recoveryState = err.Error(), "RECOVERY_BLOCKED"
		return err
	}
	s.boundary("after-ack")
	return nil
}

func (s *ownedNetlinkState) checkpoint() error {
	if s.journal == nil {
		return nil
	}
	seq, err := s.journal.Begin("importer-checkpoint", s.durableState())
	if err != nil {
		s.blocked, s.recoveryState = err.Error(), "RECOVERY_BLOCKED"
		return err
	}
	return s.ackMutation(seq)
}

func (s *ownedNetlinkState) quarantineError() error {
	err := fmt.Errorf("%w: remote ownership quarantined: %s", ownership.ErrBlocked, s.blocked)
	if s.journal != nil {
		// An existing pending intent already fences recovery. Never overwrite it.
		if _, pending, readErr := s.journal.Snapshot(); readErr == nil && pending == nil {
			return errors.Join(err, s.checkpoint())
		}
	}
	return err
}

func (b *OVNNetlinkBackend) RecoveryState() string {
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked != "" {
		return "RECOVERY_BLOCKED"
	}
	return s.recoveryState
}

func (i *RemoteType2Importer) restoreReceipts() error {
	s := i.ownedBackend.owned
	if s.journal == nil {
		return nil
	}
	i.confirmedGeneration, i.confirmedDigest = s.snapshot.Generation, remoteHash(s.snapshot.Endpoints)
	i.verified.confirmed = copyRemoteSnapshot(s.snapshot)
	i.verified.confirmed.Authoritative = false
	i.verified.pending = "" // A recovered snapshot never substitutes for fresh observations.
	keys := make([]string, 0, len(s.receipts))
	for key, epoch := range s.latest {
		if epoch > i.epoch {
			i.epoch = epoch
		}
		if _, ok := s.receipts[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := s.receipts[key]
		// Re-run policy validation before allowing recovered receipts to withdraw.
		ipConfirmed := r.Endpoint.IP == ""
		for _, route := range r.Endpoint.Routes {
			n, err := i.verified.config.ValidateRoute(route.Route)
			if err != nil || n.NetworkID != r.Endpoint.NetworkID || n.VNI != r.Endpoint.VNI ||
				route.Route.MAC != r.Endpoint.MAC || route.Route.NextHop != r.Endpoint.RemoteVTEP ||
				(route.Route.IP != "" && route.Route.IP != r.Endpoint.IP) ||
				route.NetworkID != r.Endpoint.NetworkID || route.VNI != r.Endpoint.VNI || route.Route.Source.Key == "" {
				return fmt.Errorf("%w: recovered receipt violates current import policy: %v", ownership.ErrBlocked, err)
			}
			ipConfirmed = ipConfirmed || route.Route.IP == r.Endpoint.IP
		}
		if !ipConfirmed {
			return fmt.Errorf("%w: recovered IP lacks validated source evidence", ownership.ErrBlocked)
		}
		i.active[key] = receiptRoute(r)
	}
	return nil
}
