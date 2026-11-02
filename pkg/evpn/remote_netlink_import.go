package evpn

import (
	"context"
	"fmt"
	"reflect"
	"sort"
)

// NewValidatedRemoteType2Importer reuses the existing reconciliation loop.
// The read-only importer is its only BGP observation and policy authority.
func NewValidatedRemoteType2Importer(reader *ReadOnlyType2Importer, backend *OVNNetlinkBackend) (*RemoteType2Importer, error) {
	if reader == nil || backend == nil || backend.owned == nil {
		return nil, fmt.Errorf("verified importer and explicitly owned Netlink backend required")
	}
	for _, n := range backend.owned.config.Networks {
		found := false
		for _, policy := range reader.config.Networks {
			found = found || (policy.NetworkID == n.NetworkID && policy.VNI == n.VNI)
		}
		if !found {
			return nil, fmt.Errorf("Netlink network %s missing from verified import policy", n.NetworkID)
		}
	}
	i := NewRemoteType2Importer(nil, nil, backend)
	i.verified, i.ownedBackend = reader, backend
	return i, nil
}

func (i *RemoteType2Importer) LastSnapshot() RemoteImportSnapshot {
	i.mu.Lock()
	defer i.mu.Unlock()
	return copyRemoteSnapshot(i.last)
}

func (i *RemoteType2Importer) reconcileValidated(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	snapshot, err := i.verified.Poll(ctx)
	i.last = copyRemoteSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("remote observation frozen: %w", err)
	}
	if !snapshot.Authoritative {
		return fmt.Errorf("remote observation non-authoritative: all Netlink mutations frozen")
	}
	return i.applyValidatedSnapshot(ctx, snapshot)
}

func (i *RemoteType2Importer) applyValidatedSnapshot(ctx context.Context, snapshot RemoteImportSnapshot) error {
	if !snapshot.Authoritative {
		return fmt.Errorf("non-authoritative snapshot cannot mutate Netlink")
	}
	if snapshot.Generation < i.confirmedGeneration ||
		(snapshot.Generation == i.confirmedGeneration && i.confirmedDigest != "" &&
			i.confirmedDigest != remoteHash(snapshot.Endpoints)) {
		return fmt.Errorf("stale or conflicting importer generation")
	}
	desired := make(map[string]ImportedType2Route, len(snapshot.Endpoints))
	slots := map[string]bool{}
	for _, ep := range snapshot.Endpoints {
		if ep.ValidationState != "validated" || len(ep.Routes) == 0 || snapshot.Generation == 0 ||
			ep.Key != remoteHash([]string{ep.NetworkID, ep.MAC, ep.IP}) {
			return fmt.Errorf("invalid validated endpoint identity")
		}
		ipConfirmed := ep.IP == ""
		for _, route := range ep.Routes {
			valid, err := i.verified.config.ValidateRoute(route.Route)
			if err != nil || valid.NetworkID != ep.NetworkID || valid.VNI != ep.VNI ||
				route.NetworkID != ep.NetworkID || route.VNI != ep.VNI ||
				route.Route.MAC != ep.MAC || (route.Route.IP != ep.IP && route.Route.IP != "") ||
				route.Route.NextHop != ep.RemoteVTEP ||
				route.Route.Source.Key == "" {
				return fmt.Errorf("endpoint differs from validated source evidence: %v", err)
			}
			ipConfirmed = ipConfirmed || route.Route.IP == ep.IP
		}
		if !ipConfirmed {
			return fmt.Errorf("endpoint IP lacks matching validated source evidence")
		}
		var network *RemoteNetlinkNetwork
		for _, n := range i.ownedBackend.owned.config.Networks {
			if n.NetworkID == ep.NetworkID && n.VNI == ep.VNI {
				copy := n
				network = &copy
			}
		}
		if network == nil {
			return fmt.Errorf("endpoint network has no exclusive writer mapping")
		}
		keys := []string{network.VXLANDevice + "/mac/" + ep.MAC}
		if ep.IP != "" {
			keys = append(keys, network.BridgeDevice+"/ip/"+ep.IP)
		}
		for _, key := range keys {
			if slots[key] {
				return fmt.Errorf("desired endpoints alias kernel slot %s", key)
			}
			slots[key] = true
		}
		copy := ep
		desired[ep.Key] = ImportedType2Route{Key: ep.Key, NetworkName: ep.NetworkID, VNI: ep.VNI,
			MAC: ep.MAC, IP: ep.IP, RemoteVTEP: ep.RemoteVTEP, VXLANDevice: network.VXLANDevice,
			BridgeDevice: network.BridgeDevice, NeighborDevice: network.BridgeDevice,
			generation: snapshot.Generation, validated: &copy}
	}
	i.confirmedGeneration, i.confirmedDigest = snapshot.Generation, remoteHash(snapshot.Endpoints)
	// Deterministic withdrawal-first order also handles MAC/IP/VNI remapping.
	keys := make([]string, 0, len(i.active))
	for key := range i.active {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		current := i.active[key]
		want, ok := desired[key]
		if !ok || current.retiring || !reflect.DeepEqual(current.validated, want.validated) ||
			current.VXLANDevice != want.VXLANDevice || current.NeighborDevice != want.NeighborDevice {
			current.retiring = true
			i.active[key] = current
			if err := i.ownedBackend.Withdraw(ctx, current); err != nil {
				return err
			}
			delete(i.active, key)
		}
	}
	keys = keys[:0]
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		route, ok := i.active[key]
		if !ok {
			i.epoch++
			route = desired[key]
			route.incarnation = i.epoch
			// Retain attempted routes even on partial Apply: their receipts are
			// necessary to retry or withdraw a confirmed first step.
			i.active[key] = route
		}
		if err := i.ownedBackend.Apply(ctx, route); err != nil {
			return err
		}
	}
	return nil
}
