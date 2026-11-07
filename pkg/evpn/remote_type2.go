package evpn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"

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

type RemoteRIBSnapshot struct {
	Paths    []*api.Path
	Complete bool
}

type RemoteRIBSource interface {
	ReadRemoteRIB(context.Context) (RemoteRIBSnapshot, error)
}

type GoBGPRemoteRIBSource struct {
	Client RemotePathReader
	Gate   ReadinessGate
}

func (s *GoBGPRemoteRIBSource) ReadRemoteRIB(ctx context.Context) (RemoteRIBSnapshot, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.Client == nil || s.Gate == nil {
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
		res, err := stream.Recv()
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
		if res == nil || res.Destination == nil || len(res.Destination.Paths) == 0 {
			return RemoteRIBSnapshot{}, fmt.Errorf("incomplete remote RIB destination")
		}
		for _, path := range res.Destination.Paths {
			if path == nil {
				return RemoteRIBSnapshot{}, fmt.Errorf("nil remote RIB path")
			}
			result.Paths = append(result.Paths, proto.Clone(path).(*api.Path))
		}
	}
}

type RemotePathIdentity struct {
	Key             string
	NeighborIP      string
	SourceID        string
	SourceASN       uint32
	Identifier      uint32
	LocalIdentifier uint32
}

type DecodedRemoteType2 struct {
	Source         RemotePathIdentity
	Classification string
	RouteType      uint8
	RD             string
	EthernetTag    uint32
	ESI            string
	MAC            string
	IP             string
	Labels         []uint32
	RouteTargets   []string
	Encapsulation  []string
	NextHop        string
	Best           bool
	Stale          bool
	Filtered       bool
	NexthopInvalid bool
	AttributeKey   string
}

func remoteHash(value interface{}) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func unicastIPv4(value string) bool {
	ip, err := netip.ParseAddr(value)
	return err == nil && ip.Is4() && ip.IsGlobalUnicast() && !ip.IsLoopback()
}

func DecodeRemoteType2(path *api.Path) (DecodedRemoteType2, error) {
	r := DecodedRemoteType2{Classification: "invalid"}
	if path == nil || path.Family.GetAfi() != api.Family_AFI_L2VPN || path.Family.GetSafi() != api.Family_SAFI_EVPN {
		return r, fmt.Errorf("expected EVPN AFI/SAFI")
	}
	nlri, err := apiutil.GetNativeNlri(path)
	if err != nil {
		return r, fmt.Errorf("decode EVPN NLRI: %w", err)
	}
	n, ok := nlri.(*bgp.EVPNNLRI)
	if !ok {
		return r, fmt.Errorf("non-EVPN NLRI in EVPN table")
	}
	r.RouteType = n.RouteType
	wire, err := n.Serialize()
	if err != nil {
		return r, err
	}
	r.Source = RemotePathIdentity{NeighborIP: path.NeighborIp, SourceID: path.SourceId, SourceASN: path.SourceAsn,
		Identifier: path.Identifier, LocalIdentifier: path.LocalIdentifier}
	r.Source.Key = remoteHash(struct {
		NLRI   string
		Source RemotePathIdentity
	}{hex.EncodeToString(wire), r.Source})
	r.Best, r.Stale, r.Filtered, r.NexthopInvalid = path.Best, path.Stale, path.Filtered, path.IsNexthopInvalid
	switch {
	case path.IsWithdraw:
		r.Classification = "withdrawn"
	case path.NeighborIp == "" && !path.IsFromExternal:
		r.Classification = "local"
	case net.ParseIP(path.NeighborIp) != nil && net.ParseIP(path.SourceId) != nil && path.SourceAsn != 0:
		r.Classification = "remote"
	default:
		return r, fmt.Errorf("ambiguous route source")
	}
	if n.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
		return r, nil
	}
	m, ok := n.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
	if !ok || m.RD == nil {
		return r, fmt.Errorf("missing Type-2 route data or RD")
	}
	r.RD, r.MAC, r.EthernetTag = m.RD.String(), m.MacAddress.String(), m.ETag
	if m.IPAddressLength != 0 {
		r.IP = m.IPAddress.String()
		if (m.IPAddressLength != 32 || !m.IPAddress.Is4()) && (m.IPAddressLength != 128 || !m.IPAddress.Is6()) {
			return r, fmt.Errorf("inconsistent IP length")
		}
	}
	if m.MacAddressLength != 48 || len(m.MacAddress) != 6 {
		return r, fmt.Errorf("invalid MAC length")
	}
	esi, err := m.ESI.Serialize()
	if err != nil {
		return r, err
	}
	r.ESI, r.Labels = hex.EncodeToString(esi), append([]uint32(nil), m.Labels...)
	if path.IsWithdraw || r.Classification == "local" {
		return r, nil
	}
	for _, attr := range path.Pattrs {
		if attr == nil {
			return r, fmt.Errorf("nil path attribute")
		}
		if reach := attr.GetMpReach(); reach != nil && len(reach.NextHops) != 1 {
			return r, fmt.Errorf("exactly one MP_REACH next-hop required")
		}
	}
	attrs, err := apiutil.GetNativePathAttributes(path)
	if err != nil {
		return r, fmt.Errorf("decode path attributes: %w", err)
	}
	var attributeBytes []string
	seen := map[bgp.BGPAttrType]bool{}
	for _, attr := range attrs {
		if seen[attr.GetType()] {
			return r, fmt.Errorf("duplicate path attribute %d", attr.GetType())
		}
		seen[attr.GetType()] = true
		b, err := attr.Serialize()
		if err != nil {
			return r, err
		}
		attributeBytes = append(attributeBytes, hex.EncodeToString(b))
		switch a := attr.(type) {
		case *bgp.PathAttributeOrigin:
			if a.Value > bgp.BGP_ORIGIN_ATTR_TYPE_INCOMPLETE {
				return r, fmt.Errorf("invalid origin")
			}
		case *bgp.PathAttributeMpReachNLRI:
			if a.AFI != bgp.AFI_L2VPN || a.SAFI != bgp.SAFI_EVPN || a.LinkLocalNexthop.IsValid() {
				return r, fmt.Errorf("unsupported MP_REACH family/next-hop")
			}
			if len(a.Value) != 1 || a.Value[0].NLRI == nil {
				return r, fmt.Errorf("exactly one matching MP_REACH NLRI required")
			}
			reachWire, err := a.Value[0].NLRI.Serialize()
			if err != nil || !bytes.Equal(wire, reachWire) {
				return r, fmt.Errorf("MP_REACH NLRI differs from path NLRI")
			}
			r.NextHop = a.Nexthop.String()
		case *bgp.PathAttributeNextHop:
			return r, fmt.Errorf("EVPN requires MP_REACH next-hop")
		case *bgp.PathAttributeExtendedCommunities:
			for _, community := range a.Value {
				typ, subtype := community.GetTypes()
				if subtype == bgp.EC_SUBTYPE_ROUTE_TARGET && (typ == bgp.EC_TYPE_TRANSITIVE_TWO_OCTET_AS_SPECIFIC ||
					typ == bgp.EC_TYPE_TRANSITIVE_IP4_SPECIFIC || typ == bgp.EC_TYPE_TRANSITIVE_FOUR_OCTET_AS_SPECIFIC) {
					r.RouteTargets = append(r.RouteTargets, community.String())
				} else if encap, ok := community.(*bgp.EncapExtended); ok {
					r.Encapsulation = append(r.Encapsulation, encap.TunnelType.String())
				} else {
					return r, fmt.Errorf("unsupported extended community %s", community.String())
				}
			}
		case *bgp.PathAttributeTunnelEncap:
			return r, fmt.Errorf("tunnel encapsulation attribute unsupported; VXLAN extended community required")
		case *bgp.PathAttributeAsPath, *bgp.PathAttributeMultiExitDisc, *bgp.PathAttributeLocalPref,
			*bgp.PathAttributeAtomicAggregate, *bgp.PathAttributeAggregator, *bgp.PathAttributeCommunities,
			*bgp.PathAttributeAs4Path, *bgp.PathAttributeAs4Aggregator:
		default:
			return r, fmt.Errorf("unsupported path attribute %d", attr.GetType())
		}
	}
	if !seen[bgp.BGP_ATTR_TYPE_ORIGIN] || !seen[bgp.BGP_ATTR_TYPE_MP_REACH_NLRI] ||
		!seen[bgp.BGP_ATTR_TYPE_EXTENDED_COMMUNITIES] {
		return r, fmt.Errorf("missing required origin, MP_REACH or extended communities")
	}
	sort.Strings(attributeBytes)
	r.AttributeKey = remoteHash(attributeBytes)
	sort.Strings(r.RouteTargets)
	sort.Strings(r.Encapsulation)
	return r, nil
}

type RemoteImportNetwork struct {
	NetworkID          string
	VNI                uint32
	ImportRouteTargets []string
	AllowedVTEPs       []string
}

type RemoteImportConfig struct {
	LocalVTEP string
	Networks  []RemoteImportNetwork
}

func (c RemoteImportConfig) Validate() error {
	if !unicastIPv4(c.LocalVTEP) || len(c.Networks) == 0 {
		return fmt.Errorf("explicit local IPv4 VTEP and import networks required")
	}
	names, vnis := map[string]bool{}, map[uint32]bool{}
	for _, n := range c.Networks {
		if strings.TrimSpace(n.NetworkID) == "" || names[n.NetworkID] || n.VNI == 0 || n.VNI > 16777215 || vnis[n.VNI] {
			return fmt.Errorf("unique network IDs and explicit unique VNIs in 1..16777215 required")
		}
		names[n.NetworkID], vnis[n.VNI] = true, true
		if len(n.ImportRouteTargets) == 0 || len(n.AllowedVTEPs) == 0 {
			return fmt.Errorf("network %s requires explicit import RT and VTEP allowlists", n.NetworkID)
		}
		for _, rt := range n.ImportRouteTargets {
			parsed, err := bgp.ParseRouteTarget(rt)
			if err != nil || parsed.String() != rt {
				return fmt.Errorf("network %s invalid/noncanonical import RT %q", n.NetworkID, rt)
			}
		}
		for _, ip := range n.AllowedVTEPs {
			if !unicastIPv4(ip) || ip == c.LocalVTEP {
				return fmt.Errorf("network %s invalid/locally owned remote VTEP %q", n.NetworkID, ip)
			}
		}
	}
	return nil
}

type ValidatedRemoteRoute struct {
	NetworkID string
	VNI       uint32
	Route     DecodedRemoteType2
}

func (c RemoteImportConfig) ValidateRoute(r DecodedRemoteType2) (ValidatedRemoteRoute, error) {
	if r.Classification != "remote" || r.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT ||
		r.Stale || r.Filtered || r.NexthopInvalid {
		return ValidatedRemoteRoute{}, fmt.Errorf("not an eligible current remote Type-2 path")
	}
	if _, err := bgp.ParseRouteDistinguisher(r.RD); err != nil {
		return ValidatedRemoteRoute{}, fmt.Errorf("invalid RD")
	}
	mac, err := net.ParseMAC(r.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || r.MAC == "00:00:00:00:00:00" ||
		(r.IP != "" && !unicastIPv4(r.IP)) {
		return ValidatedRemoteRoute{}, fmt.Errorf("unsupported unicast MAC/optional IPv4")
	}
	if r.ESI != "00000000000000000000" || r.EthernetTag != 0 || len(r.Labels) != 1 ||
		len(r.Encapsulation) != 1 || r.Encapsulation[0] != "vxlan" {
		return ValidatedRemoteRoute{}, fmt.Errorf("only zero ESI/tag, one VXLAN VNI and explicit VXLAN encapsulation supported")
	}
	if !unicastIPv4(r.NextHop) || r.NextHop == c.LocalVTEP {
		return ValidatedRemoteRoute{}, fmt.Errorf("invalid or local path next-hop")
	}
	for _, n := range c.Networks {
		if n.VNI != r.Labels[0] {
			continue
		}
		if !containsRemote(n.AllowedVTEPs, r.NextHop) {
			return ValidatedRemoteRoute{}, fmt.Errorf("remote VTEP not allowlisted")
		}
		for _, rt := range n.ImportRouteTargets {
			if containsRemote(r.RouteTargets, rt) {
				return ValidatedRemoteRoute{NetworkID: n.NetworkID, VNI: n.VNI, Route: r}, nil
			}
		}
		return ValidatedRemoteRoute{}, fmt.Errorf("no import RT intersection")
	}
	return ValidatedRemoteRoute{}, fmt.Errorf("no configured VNI mapping")
}

func containsRemote(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

type DesiredRemoteEndpoint struct {
	Key             string
	NetworkID       string
	VNI             uint32
	MAC             string
	IP              string
	RemoteVTEP      string
	ValidationState string
	Routes          []ValidatedRemoteRoute
}

type RemoteImportIssue struct {
	PathKey string
	Reason  string
}

type RemoteImportSnapshot struct {
	Authoritative bool
	Generation    uint64
	Endpoints     []DesiredRemoteEndpoint
	Issues        []RemoteImportIssue
}

// ReadOnlyType2Importer has no applied state, route applier or dataplane backend.
// Two equivalent complete observations are required before publishing changes.
type ReadOnlyType2Importer struct {
	mu        sync.Mutex
	source    RemoteRIBSource
	config    RemoteImportConfig
	confirmed RemoteImportSnapshot
	pending   string
}

func NewReadOnlyType2Importer(source RemoteRIBSource, config RemoteImportConfig) (*ReadOnlyType2Importer, error) {
	if source == nil {
		return nil, fmt.Errorf("remote RIB source required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	copy := config
	copy.Networks = append([]RemoteImportNetwork(nil), config.Networks...)
	for i := range copy.Networks {
		copy.Networks[i].ImportRouteTargets = append([]string(nil), config.Networks[i].ImportRouteTargets...)
		copy.Networks[i].AllowedVTEPs = append([]string(nil), config.Networks[i].AllowedVTEPs...)
	}
	return &ReadOnlyType2Importer{source: source, config: copy}, nil
}

// RebindSource retains confirmed generations but requires two fresh complete
// observations after a transport/provider incarnation change.
func (i *ReadOnlyType2Importer) RebindSource(source RemoteRIBSource) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if source == nil {
		return fmt.Errorf("remote RIB source required")
	}
	i.source, i.pending = source, ""
	i.confirmed.Authoritative = false
	return nil
}

func copyRemoteSnapshot(s RemoteImportSnapshot) RemoteImportSnapshot {
	s.Endpoints = append([]DesiredRemoteEndpoint(nil), s.Endpoints...)
	s.Issues = append([]RemoteImportIssue(nil), s.Issues...)
	for i := range s.Endpoints {
		s.Endpoints[i].Routes = append([]ValidatedRemoteRoute(nil), s.Endpoints[i].Routes...)
		for j := range s.Endpoints[i].Routes {
			r := &s.Endpoints[i].Routes[j].Route
			r.Labels = append([]uint32(nil), r.Labels...)
			r.RouteTargets = append([]string(nil), r.RouteTargets...)
			r.Encapsulation = append([]string(nil), r.Encapsulation...)
		}
	}
	return s
}

func (i *ReadOnlyType2Importer) Poll(ctx context.Context) (RemoteImportSnapshot, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	uncertain := func(err error, issues []RemoteImportIssue) (RemoteImportSnapshot, error) {
		i.pending = ""
		s := copyRemoteSnapshot(i.confirmed)
		s.Authoritative = false
		s.Issues = append(s.Issues, issues...)
		return s, err
	}
	raw, err := i.source.ReadRemoteRIB(ctx)
	if err != nil {
		return uncertain(err, nil)
	}
	if !raw.Complete || ctx.Err() != nil {
		return uncertain(fmt.Errorf("incomplete/cancelled RIB observation"), nil)
	}
	next := RemoteImportSnapshot{}
	endpoints := map[string]DesiredRemoteEndpoint{}
	identities := map[string]string{}
	macs, ips := map[string]string{}, map[string]string{}
	var quarantined []DecodedRemoteType2
	for _, path := range raw.Paths {
		r, err := DecodeRemoteType2(path)
		if err != nil {
			return uncertain(fmt.Errorf("invalid raw BGP path: %w", err), []RemoteImportIssue{{PathKey: r.Source.Key, Reason: err.Error()}})
		}
		if r.Classification == "local" || r.Classification == "withdrawn" {
			continue
		}
		if r.Stale || r.Filtered || r.NexthopInvalid {
			return uncertain(fmt.Errorf("remote path validity uncertain"), nil)
		}
		if previous, ok := identities[r.Source.Key]; ok {
			if previous != r.AttributeKey {
				return uncertain(fmt.Errorf("conflicting payloads for the same remote path identity"), nil)
			}
			continue
		}
		identities[r.Source.Key] = r.AttributeKey
		valid, err := i.config.ValidateRoute(r)
		if err != nil {
			next.Issues = append(next.Issues, RemoteImportIssue{PathKey: r.Source.Key, Reason: err.Error()})
			quarantined = append(quarantined, r)
			continue
		}
		macKey := valid.NetworkID + "/" + r.MAC
		ipKey := ""
		if r.IP != "" {
			ipKey = valid.NetworkID + "/" + r.IP
		}
		key, exists := macs[macKey]
		if exists {
			ep := endpoints[key]
			if ep.RemoteVTEP != r.NextHop {
				return uncertain(fmt.Errorf("conflicting remote VTEPs"), nil)
			}
			if ep.IP != "" && r.IP != "" && ep.IP != r.IP {
				return uncertain(fmt.Errorf("conflicting IPv4 bindings"), nil)
			}
			if ep.IP == "" && r.IP != "" {
				nextKey := remoteHash([]string{valid.NetworkID, r.MAC, r.IP})
				if previous, ok := ips[ipKey]; ok && previous != key {
					return uncertain(fmt.Errorf("conflicting IPv4 bindings"), nil)
				}
				delete(endpoints, key)
				ep.Key, ep.IP = nextKey, r.IP
				key = nextKey
				endpoints[key] = ep
				macs[macKey] = key
			}
		} else {
			key = remoteHash([]string{valid.NetworkID, r.MAC, r.IP})
			if r.IP != "" {
				if previous, ok := ips[ipKey]; ok && previous != key {
					return uncertain(fmt.Errorf("conflicting IPv4 bindings"), nil)
				}
			}
			endpoints[key] = DesiredRemoteEndpoint{Key: key, NetworkID: valid.NetworkID, VNI: valid.VNI,
				MAC: r.MAC, IP: r.IP, RemoteVTEP: r.NextHop, ValidationState: "validated"}
			macs[macKey] = key
		}
		if r.IP != "" {
			if previous, ok := ips[ipKey]; ok && previous != key {
				return uncertain(fmt.Errorf("conflicting IPv4 bindings"), nil)
			}
			ips[ipKey] = key
		}
		ep := endpoints[key]
		ep.Routes = append(ep.Routes, valid)
		endpoints[key] = ep
	}
	for _, r := range quarantined {
		for _, network := range i.config.Networks {
			if len(r.Labels) == 0 || network.VNI != r.Labels[0] {
				continue
			}
			overlap := func(ep DesiredRemoteEndpoint) bool {
				return ep.NetworkID == network.NetworkID &&
					(ep.MAC == r.MAC || (r.IP != "" && ep.IP == r.IP))
			}
			for _, ep := range endpoints {
				if overlap(ep) {
					return uncertain(fmt.Errorf("quarantined route conflicts with validated endpoint"), next.Issues)
				}
			}
			for _, ep := range i.confirmed.Endpoints {
				if overlap(ep) {
					return uncertain(fmt.Errorf("quarantined route conflicts with confirmed endpoint"), next.Issues)
				}
			}
		}
	}
	for _, ep := range endpoints {
		sort.Slice(ep.Routes, func(a, b int) bool { return ep.Routes[a].Route.Source.Key < ep.Routes[b].Route.Source.Key })
		next.Endpoints = append(next.Endpoints, ep)
	}
	sort.Slice(next.Endpoints, func(a, b int) bool { return next.Endpoints[a].Key < next.Endpoints[b].Key })
	sort.Slice(next.Issues, func(a, b int) bool {
		if next.Issues[a].PathKey == next.Issues[b].PathKey {
			return next.Issues[a].Reason < next.Issues[b].Reason
		}
		return next.Issues[a].PathKey < next.Issues[b].PathKey
	})
	key := remoteHash(next)
	if i.pending != key {
		i.pending = key
		s := copyRemoteSnapshot(i.confirmed)
		s.Authoritative = false
		s.Issues = next.Issues
		return s, nil
	}
	next.Generation = i.confirmed.Generation
	comparison := copyRemoteSnapshot(i.confirmed)
	comparison.Authoritative, comparison.Generation = false, 0
	if remoteHash(comparison) != key {
		next.Generation++
	}
	next.Authoritative = true
	i.confirmed = next
	return copyRemoteSnapshot(next), nil
}
