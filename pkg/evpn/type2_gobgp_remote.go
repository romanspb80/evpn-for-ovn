package evpn

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sort"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// RemotePathReader deliberately excludes every route/configuration mutation RPC.
type RemotePathReader interface {
	ListPath(context.Context, *api.ListPathRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[api.ListPathResponse], error)
}

// RemoteRIBSnapshot is a GoBGP compatibility representation. New importer
// integrations should consume RemoteType2Snapshot instead.
type RemoteRIBSnapshot struct {
	Paths    []*api.Path
	Complete bool
}

type RemoteRIBSource interface {
	ReadRemoteRIB(context.Context) (RemoteRIBSnapshot, error)
}

// GoBGPRemoteRIBSource is the concrete, read-only GoBGP provider adapter.
//
// Deprecated: use RemoteType2RIBSource and ReadRemoteType2RIB.
type GoBGPRemoteRIBSource struct {
	Client RemotePathReader
	Gate   ReadinessGate
}

func (s *GoBGPRemoteRIBSource) ReadRemoteRIB(ctx context.Context) (RemoteRIBSnapshot, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s == nil || s.Client == nil || s.Gate == nil {
		return RemoteRIBSnapshot{}, fmt.Errorf("remote RIB reader and actual speaker readiness gate required")
	}
	if err := s.Gate.Check(ctx); err != nil {
		return RemoteRIBSnapshot{}, err
	}
	stream, err := s.Client.ListPath(ctx, &api.ListPathRequest{
		TableType:        api.TableType_TABLE_TYPE_GLOBAL,
		Family:           &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
		EnableOnlyBinary: true,
	})
	if err != nil {
		return RemoteRIBSnapshot{}, err
	}
	if stream == nil {
		return RemoteRIBSnapshot{}, fmt.Errorf("nil remote RIB stream")
	}
	result := RemoteRIBSnapshot{}
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			if err := ctx.Err(); err != nil {
				return RemoteRIBSnapshot{}, err
			}
			if err := s.Gate.Check(ctx); err != nil {
				return RemoteRIBSnapshot{}, err
			}
			result.Complete = true
			return result, nil
		}
		if err != nil {
			return RemoteRIBSnapshot{}, fmt.Errorf("incomplete remote RIB stream: %w", err)
		}
		if response == nil || response.Destination == nil || len(response.Destination.Paths) == 0 {
			return RemoteRIBSnapshot{}, fmt.Errorf("incomplete remote RIB destination")
		}
		for _, path := range response.Destination.Paths {
			if path == nil {
				return RemoteRIBSnapshot{}, fmt.Errorf("nil remote RIB path")
			}
			result.Paths = append(result.Paths, proto.Clone(path).(*api.Path))
		}
	}
}

// DecodeRemoteType2 translates one GoBGP path into the provider-neutral route
// observation consumed by import policy.
func DecodeRemoteType2(path *api.Path) (DecodedRemoteType2, error) {
	result := DecodedRemoteType2{Classification: "invalid"}
	if path == nil || path.Family.GetAfi() != api.Family_AFI_L2VPN || path.Family.GetSafi() != api.Family_SAFI_EVPN {
		return result, fmt.Errorf("expected EVPN AFI/SAFI")
	}
	nlri, err := apiutil.GetNativeNlri(path)
	if err != nil {
		return result, fmt.Errorf("decode EVPN NLRI: %w", err)
	}
	evpnNLRI, ok := nlri.(*bgp.EVPNNLRI)
	if !ok {
		return result, fmt.Errorf("non-EVPN NLRI in EVPN table")
	}
	result.RouteType = evpnNLRI.RouteType
	wire, err := evpnNLRI.Serialize()
	if err != nil {
		return result, err
	}
	result.Source = RemotePathIdentity{
		NeighborIP: path.NeighborIp, SourceID: path.SourceId, SourceASN: path.SourceAsn,
		Identifier: path.Identifier, LocalIdentifier: path.LocalIdentifier,
	}
	result.Source.Key = remoteHash(struct {
		NLRI   string
		Source RemotePathIdentity
	}{hex.EncodeToString(wire), result.Source})
	result.Best, result.Stale, result.Filtered, result.NexthopInvalid =
		path.Best, path.Stale, path.Filtered, path.IsNexthopInvalid
	switch {
	case path.IsWithdraw:
		result.Classification = "withdrawn"
	case path.NeighborIp == "" && !path.IsFromExternal:
		result.Classification = "local"
	case net.ParseIP(path.NeighborIp) != nil && net.ParseIP(path.SourceId) != nil && path.SourceAsn != 0:
		result.Classification = "remote"
	default:
		return result, fmt.Errorf("ambiguous route source")
	}
	if evpnNLRI.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
		return result, nil
	}
	route, ok := evpnNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if !ok || route.RD == nil {
		return result, fmt.Errorf("missing Type-2 route data or RD")
	}
	result.RD, result.MAC, result.EthernetTag = route.RD.String(), route.MacAddress.String(), route.ETag
	if route.IPAddressLength != 0 {
		result.IP = route.IPAddress.String()
		if (route.IPAddressLength != 32 || !route.IPAddress.Is4()) &&
			(route.IPAddressLength != 128 || !route.IPAddress.Is6()) {
			return result, fmt.Errorf("inconsistent IP length")
		}
	}
	if route.MacAddressLength != 48 || len(route.MacAddress) != 6 {
		return result, fmt.Errorf("invalid MAC length")
	}
	esi, err := route.ESI.Serialize()
	if err != nil {
		return result, err
	}
	result.ESI, result.Labels = hex.EncodeToString(esi), append([]uint32(nil), route.Labels...)
	if path.IsWithdraw || result.Classification == "local" {
		return result, nil
	}
	for _, attribute := range path.Pattrs {
		if attribute == nil {
			return result, fmt.Errorf("nil path attribute")
		}
		if reach := attribute.GetMpReach(); reach != nil && len(reach.NextHops) != 1 {
			return result, fmt.Errorf("exactly one MP_REACH next-hop required")
		}
	}
	attributes, err := apiutil.GetNativePathAttributes(path)
	if err != nil {
		return result, fmt.Errorf("decode path attributes: %w", err)
	}
	var attributeBytes []string
	seen := map[bgp.BGPAttrType]bool{}
	for _, attribute := range attributes {
		if seen[attribute.GetType()] {
			return result, fmt.Errorf("duplicate path attribute %d", attribute.GetType())
		}
		seen[attribute.GetType()] = true
		encoded, err := attribute.Serialize()
		if err != nil {
			return result, err
		}
		attributeBytes = append(attributeBytes, hex.EncodeToString(encoded))
		switch value := attribute.(type) {
		case *bgp.PathAttributeOrigin:
			if value.Value > bgp.BGP_ORIGIN_ATTR_TYPE_INCOMPLETE {
				return result, fmt.Errorf("invalid origin")
			}
		case *bgp.PathAttributeMpReachNLRI:
			if value.AFI != bgp.AFI_L2VPN || value.SAFI != bgp.SAFI_EVPN || value.LinkLocalNexthop.IsValid() {
				return result, fmt.Errorf("unsupported MP_REACH family/next-hop")
			}
			if len(value.Value) != 1 || value.Value[0].NLRI == nil {
				return result, fmt.Errorf("exactly one matching MP_REACH NLRI required")
			}
			reachWire, err := value.Value[0].NLRI.Serialize()
			if err != nil || !bytes.Equal(wire, reachWire) {
				return result, fmt.Errorf("MP_REACH NLRI differs from path NLRI")
			}
			result.NextHop = value.Nexthop.String()
		case *bgp.PathAttributeNextHop:
			return result, fmt.Errorf("EVPN requires MP_REACH next-hop")
		case *bgp.PathAttributeExtendedCommunities:
			for _, community := range value.Value {
				typ, subtype := community.GetTypes()
				if subtype == bgp.EC_SUBTYPE_ROUTE_TARGET && (typ == bgp.EC_TYPE_TRANSITIVE_TWO_OCTET_AS_SPECIFIC ||
					typ == bgp.EC_TYPE_TRANSITIVE_IP4_SPECIFIC || typ == bgp.EC_TYPE_TRANSITIVE_FOUR_OCTET_AS_SPECIFIC) {
					result.RouteTargets = append(result.RouteTargets, community.String())
				} else if encapsulation, ok := community.(*bgp.EncapExtended); ok {
					result.Encapsulation = append(result.Encapsulation, encapsulation.TunnelType.String())
				} else {
					return result, fmt.Errorf("unsupported extended community %s", community.String())
				}
			}
		case *bgp.PathAttributeTunnelEncap:
			return result, fmt.Errorf("tunnel encapsulation attribute unsupported; VXLAN extended community required")
		case *bgp.PathAttributeAsPath, *bgp.PathAttributeMultiExitDisc, *bgp.PathAttributeLocalPref,
			*bgp.PathAttributeAtomicAggregate, *bgp.PathAttributeAggregator, *bgp.PathAttributeCommunities,
			*bgp.PathAttributeAs4Path, *bgp.PathAttributeAs4Aggregator:
		default:
			return result, fmt.Errorf("unsupported path attribute %d", attribute.GetType())
		}
	}
	if !seen[bgp.BGP_ATTR_TYPE_ORIGIN] || !seen[bgp.BGP_ATTR_TYPE_MP_REACH_NLRI] ||
		!seen[bgp.BGP_ATTR_TYPE_EXTENDED_COMMUNITIES] {
		return result, fmt.Errorf("missing required origin, MP_REACH or extended communities")
	}
	sort.Strings(attributeBytes)
	result.AttributeKey = remoteHash(attributeBytes)
	sort.Strings(result.RouteTargets)
	sort.Strings(result.Encapsulation)
	return result, nil
}
