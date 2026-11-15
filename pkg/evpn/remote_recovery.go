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
	"strings"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The generic journal version and this resource-specific version are separate.
type remoteReceiptState struct {
	Version     int
	Controller  string
	Target      string
	Networks    []RemoteNetlinkNetwork
	Devices     map[string]RemoteDeviceIdentity
	Receipts    map[string]*RemoteNetlinkReceipt
	Latest      map[string]uint64
	Snapshot    RemoteImportSnapshot
	Quarantine  string
	Quarantines map[string]RemoteSlotQuarantine
}

func closeOwnershipJournal(j *ownership.Journal) error {
	if j == nil {
		return nil
	}
	return j.Close()
}

func (s *ownedNetlinkState) durableState() remoteReceiptState {
	if s.quarantines == nil {
		s.quarantines = map[string]RemoteSlotQuarantine{}
	}
	return remoteReceiptState{Version: 2, Controller: s.controller, Target: s.target,
		Networks: s.config.Networks, Devices: s.devices, Receipts: s.receipts,
		Latest: s.latest, Snapshot: s.snapshot, Quarantine: s.blocked,
		Quarantines: s.quarantines}
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
		s.quarantines = map[string]RemoteSlotQuarantine{}
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
	switch r.Version {
	case 1:
		if r.Quarantine != "" {
			return fmt.Errorf("%w: durable importer quarantine: %s", ownership.ErrBlocked, r.Quarantine)
		}
		if r.Quarantines != nil {
			return fmt.Errorf("%w: version 1 importer state contains version 2 slot quarantines", ownership.ErrBlocked)
		}
		r.Version = 2
		r.Quarantines = map[string]RemoteSlotQuarantine{}
		s.schemaMigration = true
	case 2:
		if r.Quarantine != "" {
			return fmt.Errorf("%w: durable importer quarantine: %s", ownership.ErrBlocked, r.Quarantine)
		}
		if r.Quarantines == nil {
			return fmt.Errorf("%w: version 2 importer state has no slot quarantine map", ownership.ErrBlocked)
		}
	default:
		return fmt.Errorf("%w: unsupported importer receipt state version %d", ownership.ErrBlocked, r.Version)
	}
	controller, err := uuid.Parse(r.Controller)
	if err != nil || controller == uuid.Nil || r.Version != 2 || r.Target != s.target ||
		!reflect.DeepEqual(r.Networks, s.config.Networks) || len(r.Devices) != 2*len(r.Networks) ||
		r.Receipts == nil || r.Latest == nil {
		return fmt.Errorf("%w: invalid importer version, domain, devices or controller", ownership.ErrBlocked)
	}
	s.controller, s.devices, s.receipts, s.latest, s.snapshot, s.quarantines =
		r.Controller, r.Devices, r.Receipts, r.Latest, r.Snapshot, r.Quarantines
	s.markerPath = marker
	if err := s.verifyMarker(); err != nil {
		return err
	}
	if err := s.validateReceipts(); err != nil {
		return err
	}
	return s.validateQuarantines()
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

func (s *ownedNetlinkState) deviceForSlot(slot RemoteKernelSlot) (RemoteNetlinkNetwork, RemoteDeviceIdentity, bool) {
	for _, network := range s.config.Networks {
		name := network.BridgeDevice
		if slot.Family == unix.AF_BRIDGE {
			name = network.VXLANDevice
		}
		device, ok := s.devices[name]
		if ok && device.Ifindex == slot.Ifindex {
			return network, device, true
		}
	}
	return RemoteNetlinkNetwork{}, RemoteDeviceIdentity{}, false
}

func (s *ownedNetlinkState) validateQuarantines() error {
	if s.quarantines == nil {
		return fmt.Errorf("%w: missing slot quarantine map", ownership.ErrBlocked)
	}
	for key, quarantine := range s.quarantines {
		slot, err := remoteKernelSlot(slotRequest(quarantine.Slot))
		if err != nil || key != remoteKernelSlotKey(slot) || slot != quarantine.Slot ||
			quarantine.Target != s.target || quarantine.Reason == "" {
			return fmt.Errorf("%w: invalid durable slot quarantine %q", ownership.ErrBlocked, key)
		}
		network, device, ok := s.deviceForSlot(slot)
		if !ok || quarantine.NetworkID != network.NetworkID || quarantine.Device != device ||
			quarantine.Domain != s.config.Recovery.Domain {
			return fmt.Errorf("%w: slot quarantine domain/device mismatch %q", ownership.ErrBlocked, key)
		}
		if quarantine.Resolved && strings.TrimSpace(quarantine.Resolution) == "" {
			return fmt.Errorf("%w: resolved slot quarantine lacks resolution evidence %q", ownership.ErrBlocked, key)
		}
		if !quarantine.Resolved && quarantine.Resolution != "" {
			return fmt.Errorf("%w: active slot quarantine carries resolution evidence %q", ownership.ErrBlocked, key)
		}
		if len(quarantine.Observed) == 0 {
			return fmt.Errorf("%w: slot quarantine lacks observed kernel evidence %q", ownership.ErrBlocked, key)
		}
		for _, observed := range quarantine.Observed {
			observedSlot, err := remoteKernelSlot(observed)
			if err != nil || observedSlot != slot {
				return fmt.Errorf("%w: slot quarantine contains mismatched observation %q", ownership.ErrBlocked, key)
			}
		}
	}
	return nil
}

func (s *ownedNetlinkState) scanStartupKernelState(ctx context.Context, network RemoteNetlinkNetwork) error {
	scopes := []struct{ index, family int }{
		{s.devices[network.VXLANDevice].Ifindex, unix.AF_BRIDGE},
		{s.devices[network.BridgeDevice].Ifindex, unix.AF_INET},
		{s.devices[network.BridgeDevice].Ifindex, unix.AF_INET6},
	}
	for _, scope := range scopes {
		if err := s.check(ctx); err != nil {
			return err
		}
		entries, err := s.kernel.NeighList(scope.index, scope.family)
		if err != nil {
			return fmt.Errorf("complete kernel dump required: %w", err)
		}
		bySlot := map[string][]RemoteKernelEntry{}
		for _, neighbor := range entries {
			entry := kernelEntry(neighbor)
			if entry.Flags&netlink.NTF_EXT_LEARNED == 0 &&
				!(scope.family == unix.AF_BRIDGE && entry.IP != "") {
				continue
			}
			slot, err := remoteKernelSlot(entry)
			if err != nil {
				return fmt.Errorf("cannot identify managed kernel slot: %w", err)
			}
			bySlot[remoteKernelSlotKey(slot)] = append(bySlot[remoteKernelSlotKey(slot)], entry)
		}
		keys := make([]string, 0, len(bySlot))
		for key := range bySlot {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			observed := bySlot[key]
			sort.Slice(observed, func(i, j int) bool { return remoteHash(observed[i]) < remoteHash(observed[j]) })
			slot, _ := remoteKernelSlot(observed[0])
			if prior, ok := s.quarantines[key]; ok && !prior.Resolved {
				if err := s.recordSlotQuarantine(slot, observed, prior.Reason); err != nil && !isSlotQuarantineError(err) {
					return err
				}
				continue
			}
			if s.slotHasConfirmedReceipt(slot, observed) {
				continue
			}
			reason := "unreceipted kernel state; no startup adoption"
			if s.receiptExistsForSlot(slot) {
				reason = "kernel state conflicts with durable owned receipt"
			}
			if err := s.recordSlotQuarantine(slot, observed, reason); err != nil && !isSlotQuarantineError(err) {
				return err
			}
		}
	}
	return nil
}

func isSlotQuarantineError(err error) bool {
	var quarantine *slotQuarantineError
	return errors.As(err, &quarantine)
}

func (s *ownedNetlinkState) receiptExistsForSlot(slot RemoteKernelSlot) bool {
	for _, receipt := range s.receipts {
		for _, object := range []*RemoteObjectReceipt{&receipt.FDB, receipt.Neighbor} {
			if object != nil && sameKernelSlot(object.Request, slotRequest(slot)) {
				return true
			}
		}
	}
	return false
}

func (s *ownedNetlinkState) slotHasConfirmedReceipt(slot RemoteKernelSlot, observed []RemoteKernelEntry) bool {
	if len(observed) != 1 {
		return false
	}
	for _, receipt := range s.receipts {
		for _, object := range []*RemoteObjectReceipt{&receipt.FDB, receipt.Neighbor} {
			if object != nil && object.Status == "confirmed" && object.ACK && !object.DeleteACK &&
				sameKernelSlot(object.Request, slotRequest(slot)) && object.Observed == observed[0] {
				return true
			}
		}
	}
	return false
}

func (s *ownedNetlinkState) recordSlotQuarantine(slot RemoteKernelSlot, observed []RemoteKernelEntry, reason string) error {
	if s.journal == nil {
		s.blocked = "unreceipted remote kernel state; durable quarantine unavailable"
		return s.quarantineError()
	}
	if s.quarantines == nil {
		s.quarantines = map[string]RemoteSlotQuarantine{}
	}
	key := remoteKernelSlotKey(slot)
	network, device, ok := s.deviceForSlot(slot)
	if !ok {
		s.blocked = "slot quarantine is outside the verified device topology"
		return s.quarantineError()
	}
	quarantine, exists := s.quarantines[key]
	if exists && (quarantine.Target != s.target || quarantine.NetworkID != network.NetworkID ||
		quarantine.Device != device || (quarantine.Domain != (ownership.Domain{}) &&
		quarantine.Domain != s.config.Recovery.Domain)) {
		s.blocked = "durable slot quarantine identity changed"
		return s.quarantineError()
	}
	changed := !exists
	if !exists {
		quarantine = RemoteSlotQuarantine{Slot: slot, Target: s.target, NetworkID: network.NetworkID,
			Device: device, Domain: s.config.Recovery.Domain, Reason: reason}
	}
	if quarantine.Resolved && len(observed) != 0 {
		quarantine.Resolved, quarantine.Resolution = false, ""
		changed = true
	}
	for _, entry := range observed {
		found := false
		for _, prior := range quarantine.Observed {
			found = found || prior == entry
		}
		if !found {
			quarantine.Observed = append(quarantine.Observed, entry)
			changed = true
		}
	}
	sort.Slice(quarantine.Observed, func(i, j int) bool {
		return remoteHash(quarantine.Observed[i]) < remoteHash(quarantine.Observed[j])
	})
	if changed {
		s.quarantines[key] = quarantine
		if err := s.checkpoint(); err != nil {
			return fmt.Errorf("persist slot quarantine: %w", err)
		}
	}
	return &slotQuarantineError{slot: slot, reason: quarantine.Reason}
}

func (b *OVNNetlinkBackend) Quarantines() []RemoteSlotQuarantine {
	if b == nil || b.owned == nil {
		return nil
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.quarantines))
	for key := range s.quarantines {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]RemoteSlotQuarantine, 0, len(keys))
	for _, key := range keys {
		quarantine := s.quarantines[key]
		quarantine.Observed = append([]RemoteKernelEntry(nil), quarantine.Observed...)
		result = append(result, quarantine)
	}
	return result
}

func (b *OVNNetlinkBackend) ResolveQuarantinedSlot(ctx context.Context, slot RemoteKernelSlot, evidence string) error {
	if b == nil || b.owned == nil {
		return fmt.Errorf("remote Netlink disabled: explicit owned backend required")
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return fmt.Errorf("explicit resolution evidence required")
	}
	s := b.owned
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return fmt.Errorf("durable slot quarantine required for resolution")
	}
	if err := s.check(ctx); err != nil {
		return err
	}
	canonical, err := remoteKernelSlot(slotRequest(slot))
	if err != nil || canonical != slot {
		return fmt.Errorf("invalid quarantined slot identity: %v", err)
	}
	key := remoteKernelSlotKey(slot)
	quarantine, ok := s.quarantines[key]
	if !ok {
		return fmt.Errorf("slot %s is not quarantined", key)
	}
	if quarantine.Resolved {
		if quarantine.Resolution == evidence {
			return nil
		}
		return fmt.Errorf("slot %s was already resolved with different evidence", key)
	}
	found, err := s.slot(ctx, slotRequest(slot))
	if err != nil {
		return err
	}
	if len(found) != 0 {
		return s.recordSlotQuarantine(slot, found, quarantine.Reason)
	}
	quarantine.Resolved, quarantine.Resolution = true, evidence
	s.quarantines[key] = quarantine
	return s.checkpoint()
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
			slot, slotErr := remoteKernelSlot(o.Request)
			if slotErr != nil {
				return fmt.Errorf("%w: invalid recovered kernel slot: %v", ownership.ErrBlocked, slotErr)
			}
			if quarantine, ok := s.quarantines[remoteKernelSlotKey(slot)]; ok && !quarantine.Resolved {
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
				if err := s.recordSlotQuarantine(slot, found, "kernel state conflicts with durable owned receipt during recovery"); err != nil && !isSlotQuarantineError(err) {
					return err
				}
				continue
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
	for _, quarantine := range s.quarantines {
		if !quarantine.Resolved {
			return "RECOVERY_PARTIAL"
		}
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
