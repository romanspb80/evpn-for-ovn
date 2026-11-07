package evpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

var (
	ErrStaleRouteOperation = errors.New("stale route operation")
	ErrRouteOrder          = errors.New("previous wire-route withdrawal is not acknowledged")
	ErrRouteOwnership      = errors.New("route ownership cannot be verified")
	ErrRouteResyncRequired = errors.New("successful RIB resynchronization is required")
)

type OwnedType2Route struct {
	Endpoint LocalEndpoint
	PathUUID string
}

type Type2RIBPath struct {
	PathUUID       string
	Local          bool
	NLRI           *bgp.EVPNNLRI
	PathAttributes []bgp.PathAttributeInterface
}

// Type2RIBSource must return a complete snapshot from one pinned local speaker.
// An unavailable or incomplete RIB must return an error, never an empty snapshot.
type Type2RIBSource interface {
	Snapshot(context.Context) ([]Type2RIBPath, error)
}

// Type2RouteApplier returns the exact AddPath UUID and deletes only that UUID.
// It must reject an Add that would replace an unclaimed local wire route.
type Type2RouteApplier interface {
	Add(context.Context, Type2Route) (string, error)
	Delete(context.Context, OwnedType2Route) error
}

type routeOperation struct {
	action   RouteAction
	endpoint LocalEndpoint
	wireKey  string
	pathUUID string
}

func cloneLocalEndpoint(ep LocalEndpoint) LocalEndpoint {
	ep.IPAddresses = append([]string(nil), ep.IPAddresses...)
	ep.RouteTargets = append([]string(nil), ep.RouteTargets...)
	return ep
}

// Apply serializes validation, external application and acknowledgement. Adapters
// must not call back into the reconciler. Only the current plan can execute.
func (r *RouteReconciler) Apply(ctx context.Context, delta Type2Route, api Type2RouteApplier) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.synchronized {
		return ErrRouteResyncRequired
	}
	op := delta.operation
	if op == nil || !r.pending[op] {
		return ErrStaleRouteOperation
	}
	if api == nil {
		return fmt.Errorf("route applier is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, exists := r.applied[op.wireKey]
	switch op.action {
	case RouteAdvertise:
		if exists {
			return ErrRouteOrder
		}
		route, err := BuildType2Route(cloneLocalEndpoint(op.endpoint))
		if err != nil {
			return err
		}
		id, err := api.Add(ctx, route)
		if err != nil {
			return fmt.Errorf("advertise %s: %w", op.wireKey, err)
		}
		if id == "" {
			r.synchronized = false
			return fmt.Errorf("%w: AddPath returned no UUID", ErrRouteOwnership)
		}
		if r.applied == nil {
			r.applied = make(map[string]OwnedType2Route)
		}
		r.applied[op.wireKey] = OwnedType2Route{Endpoint: cloneLocalEndpoint(op.endpoint), PathUUID: id}
	case RouteWithdraw:
		if !exists || current.PathUUID != op.pathUUID || !current.Endpoint.Equivalent(op.endpoint) {
			return ErrStaleRouteOperation
		}
		if err := api.Delete(ctx, OwnedType2Route{Endpoint: cloneLocalEndpoint(current.Endpoint), PathUUID: current.PathUUID}); err != nil {
			return fmt.Errorf("withdraw %s: %w", op.wireKey, err)
		}
		delete(r.applied, op.wireKey)
	default:
		return fmt.Errorf("unsupported route action %q", op.action)
	}
	delete(r.pending, op)
	return nil
}

func (r *RouteReconciler) AppliedRoutes() []OwnedType2Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.applied))
	for key := range r.applied {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]OwnedType2Route, 0, len(keys))
	for _, key := range keys {
		owned := r.applied[key]
		owned.Endpoint = cloneLocalEndpoint(owned.Endpoint)
		out = append(out, owned)
	}
	return out
}

// Resync adopts only trusted ownership receipts matched to the actual RIB.
// Receipts must come from successful applications by this controller, not from
// MAC/IP matching or arbitrary RIB enumeration. Missing receipts preserve unknown
// paths and block conflicting Adds. A missing claimed UUID fails closed.
func (r *RouteReconciler) Resync(ctx context.Context, source Type2RIBSource, receipts []OwnedType2Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synchronized = false
	r.pending = nil
	if source == nil {
		return fmt.Errorf("RIB source is required")
	}

	paths, err := source.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("read local Type-2 RIB: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	claims := make(map[string]OwnedType2Route, len(receipts))
	for _, receipt := range receipts {
		if receipt.PathUUID == "" {
			return fmt.Errorf("%w: receipt has no UUID", ErrRouteOwnership)
		}
		if _, duplicate := claims[receipt.PathUUID]; duplicate {
			return fmt.Errorf("%w: duplicate UUID receipt", ErrRouteOwnership)
		}
		if _, err := BuildType2Route(receipt.Endpoint); err != nil {
			return fmt.Errorf("invalid ownership receipt: %w", err)
		}
		claims[receipt.PathUUID] = receipt
	}
	applied := make(map[string]OwnedType2Route)
	foreign := make(map[string]bool)
	for _, path := range paths {
		if path.NLRI == nil {
			return fmt.Errorf("RIB snapshot contains a nil NLRI")
		}
		if !path.Local {
			continue
		}
		key := path.NLRI.String()
		receipt, owned := claims[path.PathUUID]
		if !owned {
			foreign[key] = true
			continue
		}
		if !ribPathMatchesEndpoint(path, receipt.Endpoint) {
			return fmt.Errorf("%w: UUID %s payload differs from receipt", ErrRouteOwnership, path.PathUUID)
		}
		if _, duplicate := applied[key]; duplicate {
			return fmt.Errorf("%w: multiple owned paths for %s", ErrRouteOwnership, key)
		}
		receipt.Endpoint = cloneLocalEndpoint(receipt.Endpoint)
		applied[key] = receipt
		delete(claims, path.PathUUID)
	}
	if len(claims) != 0 {
		return fmt.Errorf("%w: claimed UUID not present in local RIB", ErrRouteOwnership)
	}
	for key := range applied {
		if foreign[key] {
			return fmt.Errorf("%w: owned and foreign paths share %s", ErrRouteOwnership, key)
		}
	}
	r.applied, r.foreign, r.synchronized = applied, foreign, true
	return nil
}

// Only ExportRecovery may use this path: payload checks are readback of a
// durable UUID in a verified incarnation, not acquisition of RIB ownership.
func (r *RouteReconciler) resyncDurable(ctx context.Context, source Type2RIBSource, receipts []OwnedType2Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synchronized, r.pending = false, nil
	paths, err := source.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w: recovery RIB unavailable: %v", ErrRouteOwnership, err)
	}
	claims := map[string]OwnedType2Route{}
	for _, receipt := range receipts {
		route, err := BuildType2Route(receipt.Endpoint)
		if err != nil || receipt.PathUUID == "" {
			return fmt.Errorf("%w: invalid durable receipt", ErrRouteOwnership)
		}
		key := route.NLRI.String()
		if _, duplicate := claims[key]; duplicate {
			return fmt.Errorf("%w: duplicate durable wire route", ErrRouteOwnership)
		}
		claims[key] = receipt
	}
	applied, foreign := map[string]OwnedType2Route{}, map[string]bool{}
	for _, path := range paths {
		if path.NLRI == nil {
			return fmt.Errorf("%w: nil recovery NLRI", ErrRouteOwnership)
		}
		if !path.Local {
			continue
		}
		key := path.NLRI.String()
		receipt, claimed := claims[key]
		if !claimed {
			foreign[key] = true
			continue
		}
		if _, duplicate := applied[key]; duplicate {
			return fmt.Errorf("%w: multiple local paths share durable route", ErrRouteOwnership)
		}
		if !ribPathMatchesEndpoint(path, receipt.Endpoint) || (path.PathUUID != "" && path.PathUUID != receipt.PathUUID) {
			return fmt.Errorf("%w: durable UUID payload conflict", ErrRouteOwnership)
		}
		applied[key] = receipt
	}
	if len(applied) != len(claims) {
		return fmt.Errorf("%w: durable receipt disappeared during recovery", ErrRouteOwnership)
	}
	r.applied, r.foreign, r.synchronized = applied, foreign, true
	return ctx.Err()
}

func ribPathMatchesEndpoint(path Type2RIBPath, ep LocalEndpoint) bool {
	want, err := BuildType2Route(ep)
	if err != nil || path.NLRI == nil {
		return false
	}
	gotNLRI, err := path.NLRI.Serialize()
	if err != nil {
		return false
	}
	wantNLRI, err := want.NLRI.Serialize()
	if err != nil || !bytes.Equal(gotNLRI, wantNLRI) || pathNextHop(path.PathAttributes) != pathNextHop(want.PathAttributes) {
		return false
	}
	originMatches := false
	for _, attr := range path.PathAttributes {
		if origin, ok := attr.(*bgp.PathAttributeOrigin); ok {
			originMatches = origin.Value == bgp.BGP_ORIGIN_ATTR_TYPE_IGP
		}
	}
	if !originMatches {
		return false
	}
	communities := func(attrs []bgp.PathAttributeInterface) ([]string, error) {
		var communities []string
		for _, attr := range attrs {
			if ext, ok := attr.(*bgp.PathAttributeExtendedCommunities); ok {
				for _, community := range ext.Value {
					value, err := community.Serialize()
					if err != nil {
						return nil, err
					}
					communities = append(communities, string(value))
				}
			}
		}
		return communities, nil
	}
	wantRT, err := communities(want.PathAttributes)
	if err != nil {
		return false
	}
	filteredRT := make([]string, 0, len(path.PathAttributes))
	for _, attr := range path.PathAttributes {
		ext, ok := attr.(*bgp.PathAttributeExtendedCommunities)
		if !ok {
			continue
		}
		for _, community := range ext.Value {
			if mobility, ok := community.(*bgp.MacMobilityExtended); ok &&
				mobility.Sequence == 0 && !mobility.IsSticky {
				continue
			}
			value, err := community.Serialize()
			if err != nil {
				return false
			}
			filteredRT = append(filteredRT, string(value))
		}
	}
	return equalStringSet(filteredRT, wantRT)
}
