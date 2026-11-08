package evpn

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

type observedType2Route struct {
	routeType      uint8
	mac            string
	ip             string
	rd             string
	labels         []uint32
	nextHop        string
	afi            uint16
	safi           uint8
	hasRouteTarget bool
}

func reserveTCPPort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp port: %v", err)
	}
	defer l.Close()
	return int32(l.Addr().(*net.TCPAddr).Port)
}

func waitForEstablished(t *testing.T, s *server.BgpServer, peerASN uint32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		established := false
		err := s.ListPeer(context.Background(), &api.ListPeerRequest{}, func(p *api.Peer) {
			if p == nil || p.Conf == nil || p.State == nil {
				return
			}
			if p.Conf.PeerAsn == peerASN && p.State.SessionState == api.PeerState_SESSION_STATE_ESTABLISHED {
				established = true
			}
		})
		if err == nil && established {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peer ASN %d did not reach ESTABLISHED state", peerASN)
}

func hasRouteTarget(attrs []bgp.PathAttributeInterface, wantRT string) (bool, error) {
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

func observeRouteOnPeer(s *server.BgpServer, endpoint LocalEndpoint) (*observedType2Route, error) {
	wantIP := primaryIP(endpoint.IPAddresses)
	var observed *observedType2Route
	var parseErr error

	err := s.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    bgp.RF_EVPN,
	}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		if prefix == nil {
			return
		}
		evpnNLRI, ok := prefix.(*bgp.EVPNNLRI)
		if !ok {
			return
		}
		macRoute, ok := evpnNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if !ok {
			return
		}
		if macRoute.MacAddress.String() != endpoint.MACAddress || macRoute.IPAddress.String() != wantIP {
			return
		}

		obs := &observedType2Route{
			routeType: evpnNLRI.RouteType,
			mac:       macRoute.MacAddress.String(),
			ip:        macRoute.IPAddress.String(),
			rd:        macRoute.RD.String(),
			labels:    append([]uint32(nil), macRoute.Labels...),
		}

		for _, p := range paths {
			for _, attr := range p.Attrs {
				switch a := attr.(type) {
				case *bgp.PathAttributeNextHop:
					obs.nextHop = a.Value.String()
				case *bgp.PathAttributeMpReachNLRI:
					obs.nextHop = a.Nexthop.String()
					obs.afi = a.AFI
					obs.safi = a.SAFI
				}
			}
			foundRT, err := hasRouteTarget(p.Attrs, endpoint.RouteTargets[0])
			if err != nil {
				parseErr = err
				return
			}
			if foundRT {
				obs.hasRouteTarget = true
			}
		}
		observed = obs
	})
	if err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return observed, nil
}

func waitForRoutePresent(t *testing.T, s *server.BgpServer, endpoint LocalEndpoint, timeout time.Duration) *observedType2Route {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		observed, err := observeRouteOnPeer(s, endpoint)
		if err == nil && observed != nil {
			return observed
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("remote peer did not receive route for endpoint %s", endpoint.EndpointID)
	return nil
}

func waitForRouteAbsent(t *testing.T, s *server.BgpServer, endpoint LocalEndpoint, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		observed, err := observeRouteOnPeer(s, endpoint)
		if err == nil && observed == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("remote peer still has route for endpoint %s", endpoint.EndpointID)
}

func applyRouteChanges(t *testing.T, r *RouteReconciler, changes []Type2Route, advertised *GoBGPType2Adapter) {
	t.Helper()
	if err := applyRoutes(r, changes, advertised); err != nil {
		t.Fatal(err)
	}
}

func TestLiveGoBGPType2RouteExchange(t *testing.T) {
	portA := reserveTCPPort(t)
	portB := reserveTCPPort(t)

	speakerA, advertised := newType2TestSpeaker(t, portA, portB)
	defer speakerA.StopBgp(context.Background(), &api.StopBgpRequest{})

	speakerB := server.NewBgpServer()
	go speakerB.Serve()
	defer speakerB.StopBgp(context.Background(), &api.StopBgpRequest{})

	if err := speakerA.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{Asn: 65000, RouterId: "10.0.0.1", ListenPort: portA},
	}); err != nil {
		t.Fatalf("start speaker A: %v", err)
	}
	if err := speakerB.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{Asn: 65001, RouterId: "10.0.0.2", ListenPort: portB},
	}); err != nil {
		t.Fatalf("start speaker B: %v", err)
	}

	peerA := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65001},
		Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(portB)},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1, IdleHoldTimeAfterReset: 1}},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{
				Family:  &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
				Enabled: true,
			},
		}},
	}
	peerB := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000},
		Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(portA)},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1, IdleHoldTimeAfterReset: 1}},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{
				Family:  &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
				Enabled: true,
			},
		}},
	}
	if err := speakerA.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peerA}); err != nil {
		t.Fatalf("add peer A->B: %v", err)
	}
	if err := speakerB.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peerB}); err != nil {
		t.Fatalf("add peer B->A: %v", err)
	}

	waitForEstablished(t, speakerA, 65001, 20*time.Second)
	waitForEstablished(t, speakerB, 65000, 20*time.Second)
	t.Logf("BGP sessions established: A(65000)<->B(65001) on ports %d/%d", portA, portB)

	endpoint := LocalEndpoint{
		NetworkID:          "net-i11a",
		EndpointID:         "endpoint-i11a-1",
		Incarnation:        "inc-i11a-1",
		MACAddress:         "00:11:22:33:44:55",
		IPAddresses:        []string{"10.10.10.10/24", "10.10.10.10"},
		VNI:                100,
		RouteDistinguisher: "65000:1000",
		RouteTargets:       []string{"65000:100"},
		LocalVTEP:          "10.0.0.1",
		NodeName:           "speaker-a-node",
		Chassis:            "speaker-a-chassis",
		Advertise:          true,
	}

	reconciler := NewRouteReconciler()
	if err := reconciler.Resync(context.Background(), advertised, nil); err != nil {
		t.Fatal(err)
	}

	changes, err := reconciler.Reconcile([]LocalEndpoint{endpoint})
	if err != nil {
		t.Fatalf("reconcile advertise: %v", err)
	}
	applyRouteChanges(t, reconciler, changes, advertised)

	observed := waitForRoutePresent(t, speakerB, endpoint, 10*time.Second)
	t.Logf(
		"received EVPN route on speaker B: type=%d rd=%s mac=%s ip=%s labels=%v next-hop=%s afi=%d safi=%d rt_present=%t",
		observed.routeType, observed.rd, observed.mac, observed.ip, observed.labels, observed.nextHop, observed.afi, observed.safi, observed.hasRouteTarget,
	)

	if observed.routeType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
		t.Fatalf("unexpected EVPN route type: got=%d want=%d", observed.routeType, bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT)
	}
	if observed.rd != endpoint.RouteDistinguisher {
		t.Fatalf("unexpected RD: got=%s want=%s", observed.rd, endpoint.RouteDistinguisher)
	}
	if observed.mac != endpoint.MACAddress {
		t.Fatalf("unexpected MAC: got=%s want=%s", observed.mac, endpoint.MACAddress)
	}
	if observed.ip != primaryIP(endpoint.IPAddresses) {
		t.Fatalf("unexpected IP: got=%s want=%s", observed.ip, primaryIP(endpoint.IPAddresses))
	}
	if len(observed.labels) == 0 || observed.labels[0] != endpoint.VNI {
		t.Fatalf("unexpected VNI/label encoding: got=%v want first label=%d", observed.labels, endpoint.VNI)
	}
	if !observed.hasRouteTarget {
		t.Fatalf("missing expected route-target %s in received attributes", endpoint.RouteTargets[0])
	}
	if observed.nextHop != endpoint.LocalVTEP {
		t.Fatalf("unexpected next-hop: got=%s want=%s", observed.nextHop, endpoint.LocalVTEP)
	}
	if observed.afi != uint16(bgp.AFI_L2VPN) || observed.safi != uint8(bgp.SAFI_EVPN) {
		t.Fatalf("unexpected AFI/SAFI in received route: got=%d/%d want=%d/%d", observed.afi, observed.safi, bgp.AFI_L2VPN, bgp.SAFI_EVPN)
	}

	changes, err = reconciler.Reconcile(nil)
	if err != nil {
		t.Fatalf("reconcile withdraw: %v", err)
	}
	applyRouteChanges(t, reconciler, changes, advertised)
	waitForRouteAbsent(t, speakerB, endpoint, 10*time.Second)
	t.Log("withdraw verified: route disappeared from speaker B")
}

func TestLiveGoBGPType2RouteExchange_UsesRouteBuilder(t *testing.T) {
	route, err := BuildType2Route(LocalEndpoint{
		NetworkID:          "net-i11a",
		EndpointID:         "endpoint-i11a-builder",
		Incarnation:        "inc-i11a-builder",
		MACAddress:         "00:aa:bb:cc:dd:ee",
		IPAddresses:        []string{"10.20.30.40/24"},
		VNI:                500,
		RouteDistinguisher: "65000:500",
		RouteTargets:       []string{"65000:500"},
		LocalVTEP:          "10.0.0.1",
		Advertise:          true,
	})
	if err != nil {
		t.Fatalf("build route: %v", err)
	}
	if route.NLRI.MAC != "00:aa:bb:cc:dd:ee" || route.NLRI.IP != "10.20.30.40" || route.NLRI.Labels[0] != 500 {
		t.Fatalf("unexpected Type-2 domain route: %+v", route.NLRI)
	}
	if _, _, err := type2RouteToGoBGP(route); err != nil {
		t.Fatalf("GoBGP boundary conversion failed: %v", err)
	}
}
