package evpn

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// ReceivedType2Route and RouteSnapshotSource are retained for source
// compatibility. New provider implementations should use Type2RouteObservation
// and Type2RouteSnapshotSource.
type ReceivedType2Route struct {
	MAC     string
	IP      string
	RD      string
	Labels  []uint32
	NextHop string
	Attrs   []bgp.PathAttributeInterface
}

type RouteSnapshotSource interface {
	Snapshot(ctx context.Context) ([]ReceivedType2Route, error)
}

// Deprecated: implement Type2RouteSnapshotSource instead.
type GoBGPServerSnapshotSource struct {
	Server *server.BgpServer
}

func (s *GoBGPServerSnapshotSource) Snapshot(_ context.Context) ([]ReceivedType2Route, error) {
	if s == nil || s.Server == nil {
		return nil, fmt.Errorf("gobgp snapshot source is not configured")
	}

	out := make([]ReceivedType2Route, 0, 8)
	err := s.Server.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    bgp.RF_EVPN,
	}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		if prefix == nil {
			return
		}
		evpnNLRI, ok := prefix.(*bgp.EVPNNLRI)
		if !ok || evpnNLRI.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
			return
		}
		macRoute, ok := evpnNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if !ok {
			return
		}
		ip := strings.TrimSpace(macRoute.IPAddress.String())
		if ip == "<nil>" {
			ip = ""
		}

		for _, path := range paths {
			if path.Withdrawal || path.Stale {
				continue
			}
			nextHop := pathNextHop(path.Attrs)
			remoteVTEP := strings.TrimSpace(path.PeerAddress.String())
			if remoteVTEP == "" || remoteVTEP == "<nil>" || remoteVTEP == "invalid IP" || net.ParseIP(remoteVTEP) == nil {
				remoteVTEP = strings.TrimSpace(nextHop)
			}
			out = append(out, ReceivedType2Route{
				MAC:     strings.ToLower(strings.TrimSpace(macRoute.MacAddress.String())),
				IP:      ip,
				RD:      strings.TrimSpace(macRoute.RD.String()),
				Labels:  append([]uint32(nil), macRoute.Labels...),
				NextHop: remoteVTEP,
				Attrs:   path.Attrs,
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *GoBGPServerSnapshotSource) SnapshotType2(ctx context.Context) (Type2RouteSnapshot, error) {
	routes, err := s.Snapshot(ctx)
	if err != nil {
		return Type2RouteSnapshot{}, err
	}
	snapshot := Type2RouteSnapshot{Routes: make([]Type2RouteObservation, 0, len(routes)), Complete: true}
	for _, route := range routes {
		targets, err := routeTargetsFromGoBGP(route.Attrs)
		if err != nil {
			return Type2RouteSnapshot{}, err
		}
		snapshot.Routes = append(snapshot.Routes, Type2RouteObservation{
			MAC: route.MAC, IP: route.IP, RD: route.RD, Labels: append([]uint32(nil), route.Labels...),
			NextHop: route.NextHop, RouteTargets: targets,
		})
	}
	return snapshot, nil
}

type legacyType2RouteSnapshotSource struct {
	source RouteSnapshotSource
}

func (s legacyType2RouteSnapshotSource) SnapshotType2(ctx context.Context) (Type2RouteSnapshot, error) {
	if s.source == nil {
		return Type2RouteSnapshot{}, fmt.Errorf("Type-2 route snapshot source required")
	}
	routes, err := s.source.Snapshot(ctx)
	if err != nil {
		return Type2RouteSnapshot{}, err
	}
	snapshot := Type2RouteSnapshot{Routes: make([]Type2RouteObservation, 0, len(routes)), Complete: true}
	for _, route := range routes {
		attributes := append([]bgp.PathAttributeInterface(nil), route.Attrs...)
		targets, err := routeTargetsFromGoBGP(attributes)
		if err != nil {
			return Type2RouteSnapshot{}, err
		}
		snapshot.Routes = append(snapshot.Routes, Type2RouteObservation{
			MAC: route.MAC, IP: route.IP, RD: route.RD, Labels: append([]uint32(nil), route.Labels...),
			NextHop: route.NextHop, RouteTargets: targets,
			targetMatch: func(want string) (bool, error) {
				return pathHasRouteTarget(attributes, want)
			},
		})
	}
	return snapshot, nil
}

func routeTargetsFromGoBGP(attrs []bgp.PathAttributeInterface) ([]string, error) {
	var targets []string
	for _, attribute := range attrs {
		communities, ok := attribute.(*bgp.PathAttributeExtendedCommunities)
		if !ok {
			continue
		}
		for _, community := range communities.Value {
			typ, subtype := community.GetTypes()
			if subtype != bgp.EC_SUBTYPE_ROUTE_TARGET || (typ != bgp.EC_TYPE_TRANSITIVE_TWO_OCTET_AS_SPECIFIC &&
				typ != bgp.EC_TYPE_TRANSITIVE_IP4_SPECIFIC && typ != bgp.EC_TYPE_TRANSITIVE_FOUR_OCTET_AS_SPECIFIC) {
				continue
			}
			encoded, err := community.Serialize()
			if err != nil {
				return nil, err
			}
			target, err := bgp.ParseRouteTarget(community.String())
			if err != nil {
				return nil, err
			}
			canonical, err := target.Serialize()
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(encoded, canonical) {
				continue
			}
			targets = append(targets, target.String())
		}
	}
	return targets, nil
}

func pathNextHop(attrs []bgp.PathAttributeInterface) string {
	for _, attr := range attrs {
		switch a := attr.(type) {
		case *bgp.PathAttributeNextHop:
			return a.Value.String()
		case *bgp.PathAttributeMpReachNLRI:
			return a.Nexthop.String()
		}
	}
	return ""
}

func pathHasRouteTarget(attrs []bgp.PathAttributeInterface, wantRT string) (bool, error) {
	want, err := bgp.ParseRouteTarget(wantRT)
	if err != nil {
		return false, err
	}
	wantBytes, err := want.Serialize()
	if err != nil {
		return false, err
	}
	for _, attr := range attrs {
		ext, ok := attr.(*bgp.PathAttributeExtendedCommunities)
		if !ok {
			continue
		}
		for _, community := range ext.Value {
			gotBytes, err := community.Serialize()
			if err != nil {
				return false, err
			}
			if bytes.Equal(gotBytes, wantBytes) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Validate retains the original GoBGP-shaped API while delegating policy to
// the provider-neutral validator.
func (v *Type2RouteValidator) Validate(route ReceivedType2Route) (*ImportedType2Route, bool, error) {
	attributes := append([]bgp.PathAttributeInterface(nil), route.Attrs...)
	return v.ValidateObservation(Type2RouteObservation{
		MAC: route.MAC, IP: route.IP, RD: route.RD, Labels: append([]uint32(nil), route.Labels...),
		NextHop: route.NextHop,
		targetMatch: func(want string) (bool, error) {
			return pathHasRouteTarget(attributes, want)
		},
	})
}
