package evpn

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// GoBGPType2Adapter must be pinned to one speaker, not a load-balanced service.
// GoBGP has no compare-and-swap AddPath: callers must ensure exclusive local
// writer ownership of each wire route across controllers.
type GoBGPType2Adapter struct {
	Client api.GoBgpServiceClient
	Gate   ReadinessGate
	mu     sync.Mutex
}

func (a *GoBGPType2Adapter) Snapshot(ctx context.Context) ([]Type2RIBPath, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("GoBGP client is required")
	}
	if a.Gate != nil {
		if err := a.Gate.Check(ctx); err != nil {
			return nil, err
		}
	}
	stream, err := a.Client.ListPath(ctx, &api.ListPathRequest{
		TableType:        api.TableType_TABLE_TYPE_GLOBAL,
		Family:           &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
		EnableOnlyBinary: true,
	})
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, fmt.Errorf("nil GoBGP RIB stream")
	}
	var paths []Type2RIBPath
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if a.Gate != nil {
				if err := a.Gate.Check(ctx); err != nil {
					return nil, err
				}
			}
			return paths, nil
		}
		if err != nil {
			return nil, err
		}
		if response == nil || response.Destination == nil || len(response.Destination.Paths) == 0 {
			return nil, fmt.Errorf("incomplete GoBGP RIB response")
		}
		for _, p := range response.Destination.Paths {
			if p == nil {
				return nil, fmt.Errorf("nil GoBGP RIB path")
			}
			nlri, err := apiutil.GetNativeNlri(p)
			if err != nil {
				return nil, err
			}
			evpn, ok := nlri.(*bgp.EVPNNLRI)
			if !ok || evpn.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
				continue
			}
			attrs, err := apiutil.GetNativePathAttributes(p)
			if err != nil {
				return nil, err
			}
			domainNLRI, err := type2NLRIFromGoBGP(evpn)
			if err != nil {
				return nil, err
			}
			domainAttrs, err := type2PathAttributesFromGoBGP(attrs)
			if err != nil {
				return nil, err
			}
			id := ""
			if len(p.Uuid) != 0 {
				value, err := uuid.FromBytes(p.Uuid)
				if err != nil || value == uuid.Nil {
					return nil, fmt.Errorf("invalid GoBGP RIB UUID")
				}
				id = value.String()
			}
			paths = append(paths, Type2RIBPath{
				PathUUID: id, Local: id != "" || (p.NeighborIp == "" && !p.IsFromExternal),
				NLRI: &domainNLRI, PathAttributes: domainAttrs,
			})
		}
	}
}

func (a *GoBGPType2Adapter) Add(ctx context.Context, route Type2Route) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	want, err := BuildType2Route(route.Endpoint)
	if err != nil {
		return "", err
	}
	nlri, attrs, err := type2RouteToGoBGP(want)
	if err != nil {
		return "", err
	}
	paths, err := a.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if path.Local && path.NLRI != nil && path.NLRI.Equal(want.NLRI) {
			return "", fmt.Errorf("%w: local wire route already exists", ErrRouteOwnership)
		}
	}
	path, err := apiutil.NewPath(bgp.RF_EVPN, nlri, false, attrs, time.Now())
	if err != nil {
		return "", err
	}
	if want.Endpoint.MACOnly {
		// Pinned apiutil marshals an absent netip.Addr as "invalid IP".
		// The supported binary NLRI API preserves the standard zero IP length.
		path.NlriBinary, err = nlri.Serialize()
		if err != nil {
			return "", err
		}
		path.Nlri = nil
	}
	if a.Gate != nil {
		if err := a.Gate.Check(ctx); err != nil {
			return "", err
		}
	}
	response, err := a.Client.AddPath(ctx, &api.AddPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path,
	})
	if err != nil {
		return "", err
	}
	id, err := uuid.FromBytes(response.GetUuid())
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("%w: AddPath returned no valid UUID", ErrRouteOwnership)
	}
	return id.String(), nil
}

func (a *GoBGPType2Adapter) Delete(ctx context.Context, owned OwnedType2Route) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, err := uuid.Parse(owned.PathUUID)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf("%w: exact UUID required for DeletePath", ErrRouteOwnership)
	}
	paths, err := a.Snapshot(ctx)
	if err != nil {
		return err
	}
	want, err := BuildType2Route(owned.Endpoint)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !path.Local || path.NLRI == nil || !path.NLRI.Equal(want.NLRI) {
			continue
		}
		if path.PathUUID != "" && path.PathUUID != id.String() {
			return fmt.Errorf("%w: wire route has another UUID", ErrRouteOwnership)
		}
		if !ribPathMatchesEndpoint(path, owned.Endpoint) {
			return fmt.Errorf("%w: UUID payload changed", ErrRouteOwnership)
		}
		// GoBGP v4.5.0 ListPath omits UUIDs. The trusted AddPath receipt is
		// still an exact deletion handle; never fall back to deleting NLRI.
		if a.Gate != nil {
			if err := a.Gate.Check(ctx); err != nil {
				return err
			}
		}
		_, err = a.Client.DeletePath(ctx, &api.DeletePathRequest{
			TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: id[:],
		})
		return err
	}
	// A complete successful RIB read proves the entire wire route is absent.
	return nil
}

func type2NLRIFromGoBGP(nlri *bgp.EVPNNLRI) (Type2NLRI, error) {
	var result Type2NLRI
	if nlri == nil || nlri.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
		return result, fmt.Errorf("expected EVPN Type-2 NLRI")
	}
	route, ok := nlri.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if !ok || route.RD == nil || route.MacAddressLength != 48 || len(route.MacAddress) != 6 {
		return result, fmt.Errorf("invalid EVPN Type-2 NLRI data")
	}
	esi, err := route.ESI.Serialize()
	if err != nil {
		return result, err
	}
	result = Type2NLRI{RD: route.RD.String(), ESI: hex.EncodeToString(esi), EthernetTag: route.ETag,
		MAC: route.MacAddress.String(), Labels: append([]uint32(nil), route.Labels...)}
	if route.IPAddressLength != 0 {
		if (route.IPAddressLength != 32 || !route.IPAddress.Is4()) && (route.IPAddressLength != 128 || !route.IPAddress.Is6()) {
			return Type2NLRI{}, fmt.Errorf("inconsistent EVPN Type-2 IP length")
		}
		result.IP = route.IPAddress.String()
	} else if route.IPAddress.IsValid() {
		return Type2NLRI{}, fmt.Errorf("EVPN Type-2 IP present with zero length")
	}
	if _, err := bgp.ParseRouteDistinguisher(result.RD); err != nil {
		return Type2NLRI{}, fmt.Errorf("invalid EVPN Type-2 RD: %w", err)
	}
	if err := result.Validate(); err != nil {
		return Type2NLRI{}, err
	}
	return result, nil
}

func type2PathAttributesFromGoBGP(attrs []bgp.PathAttributeInterface) (Type2PathAttributes, error) {
	result := Type2PathAttributes{NextHop: pathNextHop(attrs)}
	for _, attr := range attrs {
		switch value := attr.(type) {
		case *bgp.PathAttributeOrigin:
			result.Origin = strconv.Itoa(int(value.Value))
			if value.Value == bgp.BGP_ORIGIN_ATTR_TYPE_IGP {
				result.Origin = Type2OriginIGP
			}
		case *bgp.PathAttributeExtendedCommunities:
			for _, community := range value.Value {
				encoded, err := community.Serialize()
				if err != nil {
					return Type2PathAttributes{}, err
				}
				switch c := community.(type) {
				case *bgp.EncapExtended:
					value := strconv.Itoa(int(c.TunnelType))
					if c.TunnelType == bgp.TUNNEL_TYPE_VXLAN {
						value = "vxlan"
					}
					result.ExtendedCommunities = append(result.ExtendedCommunities, Type2ExtendedCommunity{
						Kind: Type2EncapsulationCommunity, Value: value,
					})
				case *bgp.MacMobilityExtended:
					result.ExtendedCommunities = append(result.ExtendedCommunities, Type2ExtendedCommunity{
						Kind: Type2MACMobilityCommunity, Sequence: c.Sequence, Sticky: c.IsSticky,
					})
				default:
					if target, err := bgp.ParseRouteTarget(community.String()); err == nil {
						result.ExtendedCommunities = append(result.ExtendedCommunities, Type2ExtendedCommunity{
							Kind: Type2RouteTargetCommunity, Value: target.String(),
						})
					} else {
						result.ExtendedCommunities = append(result.ExtendedCommunities, Type2ExtendedCommunity{
							Kind: Type2OpaqueCommunity, Value: hex.EncodeToString(encoded),
						})
					}
				}
			}
		}
	}
	return result, nil
}

func type2RouteToGoBGP(route Type2Route) (*bgp.EVPNNLRI, []bgp.PathAttributeInterface, error) {
	rd, err := bgp.ParseRouteDistinguisher(route.NLRI.RD)
	if err != nil {
		return nil, nil, err
	}
	mac, err := net.ParseMAC(route.NLRI.MAC)
	if err != nil || len(mac) != 6 {
		return nil, nil, fmt.Errorf("invalid Type-2 MAC")
	}
	esiBytes, err := hex.DecodeString(route.NLRI.ESI)
	if err != nil || len(esiBytes) != 10 {
		return nil, nil, fmt.Errorf("invalid Type-2 ESI")
	}
	ip := netip.Addr{}
	ipLength := uint8(0)
	if route.NLRI.IP != "" {
		ip, err = netip.ParseAddr(route.NLRI.IP)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid Type-2 IP: %w", err)
		}
		if ip.Is4() {
			ipLength = 32
		} else if ip.Is6() {
			ipLength = 128
		} else {
			return nil, nil, fmt.Errorf("invalid Type-2 IP family")
		}
	}
	if route.PathAttributes.Origin != Type2OriginIGP {
		return nil, nil, fmt.Errorf("unsupported Type-2 origin %q", route.PathAttributes.Origin)
	}
	nextHop, err := netip.ParseAddr(route.PathAttributes.NextHop)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid Type-2 next-hop: %w", err)
	}
	communities := make([]bgp.ExtendedCommunityInterface, 0, len(route.PathAttributes.ExtendedCommunities))
	for _, community := range route.PathAttributes.ExtendedCommunities {
		switch community.Kind {
		case Type2RouteTargetCommunity:
			target, err := bgp.ParseRouteTarget(community.Value)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid Type-2 route target: %w", err)
			}
			communities = append(communities, target)
		case Type2EncapsulationCommunity:
			var tunnelType uint64
			if community.Value == "vxlan" {
				tunnelType = uint64(bgp.TUNNEL_TYPE_VXLAN)
			} else {
				tunnelType, err = strconv.ParseUint(community.Value, 10, 16)
				if err != nil {
					return nil, nil, fmt.Errorf("invalid Type-2 encapsulation: %w", err)
				}
			}
			communities = append(communities, &bgp.EncapExtended{TunnelType: bgp.TunnelType(tunnelType)})
		case Type2MACMobilityCommunity:
			communities = append(communities, &bgp.MacMobilityExtended{
				Sequence: community.Sequence, IsSticky: community.Sticky,
			})
		default:
			return nil, nil, fmt.Errorf("unsupported Type-2 extended community %q", community.Kind)
		}
	}
	if len(route.NLRI.Labels) == 0 {
		return nil, nil, fmt.Errorf("Type-2 labels required")
	}
	nlri := bgp.NewEVPNNLRI(bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT, &bgp.EVPNMacIPAdvertisementRoute{
		RD: rd, ESI: bgp.EthernetSegmentIdentifier{Type: bgp.ESIType(esiBytes[0]), Value: append([]byte(nil), esiBytes[1:]...)},
		ETag: route.NLRI.EthernetTag, MacAddressLength: 48, MacAddress: mac,
		IPAddressLength: ipLength, IPAddress: ip, Labels: append([]uint32(nil), route.NLRI.Labels...),
	})
	nextHopAttr, err := bgp.NewPathAttributeNextHop(nextHop)
	if err != nil {
		return nil, nil, err
	}
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP),
		nextHopAttr,
		bgp.NewPathAttributeExtendedCommunities(communities),
	}
	return nlri, attrs, nil
}

func type2WirePayload(endpoint LocalEndpoint) ([]byte, [][]byte, error) {
	route, err := BuildType2Route(endpoint)
	if err != nil {
		return nil, nil, err
	}
	nlri, attrs, err := type2RouteToGoBGP(route)
	if err != nil {
		return nil, nil, err
	}
	nlriBytes, err := nlri.Serialize()
	if err != nil {
		return nil, nil, err
	}
	attributeBytes := make([][]byte, 0, len(attrs))
	for _, attribute := range attrs {
		encoded, err := attribute.Serialize()
		if err != nil {
			return nil, nil, err
		}
		attributeBytes = append(attributeBytes, encoded)
	}
	return nlriBytes, attributeBytes, nil
}
