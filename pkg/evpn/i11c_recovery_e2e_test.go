package evpn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func sessionStateForPeer(s *server.BgpServer, peerASN uint32) (api.PeerState_SessionState, bool, error) {
	state := api.PeerState_SESSION_STATE_UNSPECIFIED
	found := false
	err := s.ListPeer(context.Background(), &api.ListPeerRequest{}, func(p *api.Peer) {
		if p == nil || p.Conf == nil || p.State == nil {
			return
		}
		if p.Conf.PeerAsn == peerASN {
			found = true
			state = p.State.SessionState
		}
	})
	return state, found, err
}

func waitForSessionState(t *testing.T, s *server.BgpServer, peerASN uint32, want api.PeerState_SessionState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, found, err := sessionStateForPeer(s, peerASN)
		if err == nil && found && got == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("peer ASN %d did not reach state %s", peerASN, want.String())
}

func waitForSessionNotEstablished(t *testing.T, s *server.BgpServer, peerASN uint32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, found, err := sessionStateForPeer(s, peerASN)
		if err == nil {
			if !found || got != api.PeerState_SESSION_STATE_ESTABLISHED {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("peer ASN %d remained ESTABLISHED", peerASN)
}

func routePathCountOnPeer(s *server.BgpServer, endpoint LocalEndpoint) (int, error) {
	wantIP := primaryIP(endpoint.IPAddresses)
	count := 0
	err := s.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    bgp.RF_EVPN,
	}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		evpnNLRI, ok := prefix.(*bgp.EVPNNLRI)
		if !ok {
			return
		}
		macRoute, ok := evpnNLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if !ok {
			return
		}
		if macRoute.MacAddress.String() == endpoint.MACAddress && macRoute.IPAddress.String() == wantIP {
			count += len(paths)
		}
	})
	return count, err
}

func makeRecoveryAdvertiser(t *testing.T, ctx context.Context, asn uint32, routerID string, listenPort int32, peerAddr string, peerASN uint32, peerPort int32) (*server.BgpServer, *GoBGPType2Adapter) {
	t.Helper()
	s, adapter := newType2TestSpeaker(t, listenPort, peerPort)
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: routerID, ListenPort: listenPort}}); err != nil {
		t.Fatalf("start advertiser speaker: %v", err)
	}
	if err := s.AddPeer(ctx, &api.AddPeerRequest{
		Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: peerAddr, PeerAsn: peerASN},
			Transport: &api.Transport{LocalAddress: peerAddr, RemotePort: uint32(peerPort)},
			AfiSafis: []*api.AfiSafi{{
				Config: &api.AfiSafiConfig{
					Family:  &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
					Enabled: true,
				},
			}},
		},
	}); err != nil {
		t.Fatalf("add advertiser peer: %v", err)
	}
	return s, adapter
}

func TestI11cRecoveryRestartAndOutage(t *testing.T) {
	if os.Getenv("I11C_RECOVERY_E2E") != "1" {
		t.Skip("set I11C_RECOVERY_E2E=1 to run recovery e2e")
	}

	ctx := context.Background()
	kctx := strings.TrimSpace(os.Getenv("KUBECTL_CONTEXT"))
	if kctx == "" {
		kctx = "kind-kind0"
	}
	ovnNS := strings.TrimSpace(os.Getenv("OVN_NAMESPACE"))
	if ovnNS == "" {
		ovnNS = "ovn-system"
	}
	localNode := strings.TrimSpace(os.Getenv("I11C_RECOVERY_LOCAL_NODE"))
	if localNode == "" {
		localNode = "kind0-control-plane"
	}

	testNS := "i11c-recovery"
	lsName := "ls-i11r"
	nadA := "evpn-i11r-a"
	nadB := "evpn-i11r-b"
	podA := "i11r-pod-a"
	podB := "i11r-pod-b"
	networkID := "net-i11r"
	attachmentID := "net1"
	ipA := "10.251.4.11"
	ipACIDR := "10.251.4.11/24"
	macA := "0a:58:fb:04:00:11"
	ipB := "10.251.4.12"
	ipBCIDR := "10.251.4.12/24"
	macB := "0a:58:fb:04:00:12"
	rd := "65000:1300"
	rt := "65000:130"
	vni := uint32(130)
	localVTEP := "10.0.0.41"

	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	localOVSPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs-node-local-cp", localNode)
	systemID := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", localOVSPod, "-c", "ovn-ovs-node-local", "--", "sh", "-lc", "ovs-vsctl --db=unix:/var/run/openvswitch/db.sock get Open_vSwitch . external-ids:system-id"))
	localChassis := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s'", systemID)))
	if localChassis == "" {
		t.Fatalf("failed to resolve local chassis")
	}

	cleanup := func() {
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "pod", podA, podB, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "network-attachment-definition", nadA, nadB, "--ignore-not-found=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "delete", "namespace", testNS, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", "ovn-nbctl --if-exists ls-del ls-i11r")
	}
	defer cleanup()

	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs", fmt.Sprintf("uuid=$(ovn-nbctl --if-exists --data=bare --no-heading get Logical_Switch '%s' _uuid | tr -d '[:space:]'); if [ -z \"$uuid\" ]; then ovn-nbctl ls-add '%s'; fi", lsName, lsName))

	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |-
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"%s","logicalSwitch":"%s","attachmentID":"%s","ipAddress":"%s","macAddress":"%s","timeoutSeconds":10}
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |-
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"%s","logicalSwitch":"%s","attachmentID":"%s","ipAddress":"%s","macAddress":"%s","timeoutSeconds":10}
`, testNS, nadA, testNS, nadA, networkID, lsName, attachmentID, ipACIDR, macA, nadB, testNS, nadB, networkID, lsName, attachmentID, ipBCIDR, macB)
	if _, err := runCmdInput(ctx, manifest, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply namespace and nads: %v", err)
	}

	source := &OVNCtlSource{
		Runner:      ExecRunner{},
		KubeContext: kctx,
		Namespace:   ovnNS,
		CentralPod:  centralPod,
		Container:   "ovn-ovs",
	}
	observer := &OVNEndpointObserver{
		Source:       source,
		LocalNode:    localNode,
		LocalChassis: localChassis,
		Networks: map[string]EVPNNetworkConfig{
			networkID: {
				NetworkID:          networkID,
				Enabled:            true,
				VNI:                vni,
				RouteDistinguisher: rd,
				RouteTargets:       []string{rt},
				LocalVTEP:          localVTEP,
			},
		},
	}

	portAdv := reserveTCPPort(t)
	portRecv := reserveTCPPort(t)

	receiver := server.NewBgpServer()
	go receiver.Serve()
	defer receiver.StopBgp(ctx, &api.StopBgpRequest{})
	if err := receiver.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65312, RouterId: "10.0.0.52", ListenPort: portRecv}}); err != nil {
		t.Fatalf("start receiver: %v", err)
	}

	advertiser, advertised := makeRecoveryAdvertiser(t, ctx, 65311, "10.0.0.51", portAdv, "127.0.0.1", 65312, portRecv)
	defer advertiser.StopBgp(ctx, &api.StopBgpRequest{})

	if err := receiver.AddPeer(ctx, &api.AddPeerRequest{
		Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65311},
			Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(portAdv)},
			AfiSafis: []*api.AfiSafi{{
				Config: &api.AfiSafiConfig{
					Family:  &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
					Enabled: true,
				},
			}},
		},
	}); err != nil {
		t.Fatalf("add receiver peer: %v", err)
	}

	waitForSessionState(t, advertiser, 65312, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)
	waitForSessionState(t, receiver, 65311, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)

	reconciler := NewRouteReconciler()

	createPodA := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  annotations:
    k8s.v1.cni.cncf.io/networks: '[{"name":"%s","interface":"net1"}]'
spec:
  nodeName: %s
  containers:
  - name: bb
    image: busybox:1.36
    command: ["sh","-lc","sleep 3600"]
`, podA, testNS, nadA, localNode)
	if _, err := runCmdInput(ctx, createPodA, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("create pod A: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podA, "--timeout=240s")

	expectA := LocalEndpoint{
		NetworkID:          networkID,
		MACAddress:         macA,
		IPAddresses:        []string{ipA},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		LocalVTEP:          localVTEP,
		Advertise:          true,
	}
	waitFor(t, 40*time.Second, 300*time.Millisecond, "initial advertisement", func() (bool, error) {
		desired, _, err := reconcileOnce(ctx, observer, reconciler, advertiser, advertised)
		if err != nil {
			return false, err
		}
		if len(desired) != 1 {
			return false, nil
		}
		view, err := routeViewOnPeer(receiver, expectA)
		if err != nil || view == nil {
			return false, err
		}
		return len(view.NextHops) == 1 && view.NextHops[0] == localVTEP, nil
	})
	viewInitial, _ := routeViewOnPeer(receiver, expectA)
	t.Logf("remote RIB initial: mac=%s ip=%s nexthops=%v rd=%s labels=%v", viewInitial.MAC, viewInitial.IP, viewInitial.NextHops, viewInitial.RD, viewInitial.Labels)

	// Reconciler restart: verify retained ownership receipts against the real RIB.
	receipts := reconciler.AppliedRoutes()
	reconciler = NewRouteReconciler()
	if err := reconciler.Resync(ctx, advertised, receipts); err != nil {
		if !errors.Is(err, ErrRouteOwnership) {
			t.Fatalf("reconciler restart resync: %v", err)
		}
		t.Logf("restart adoption unavailable: RIB lacks verified UUIDs; writes remain disabled: %v", err)
		changes, err := reconciler.Reconcile([]LocalEndpoint{receipts[0].Endpoint})
		if err != nil || len(changes) != 1 {
			t.Fatalf("restart pending plan: changes=%v err=%v", changes, err)
		}
		if err := reconciler.Apply(ctx, changes[0], advertised); !errors.Is(err, ErrRouteResyncRequired) {
			t.Fatalf("restart allowed an unverified write: %v", err)
		}
	} else {
		_, restartChanges, err := reconcileOnce(ctx, observer, reconciler, advertiser, advertised)
		if err != nil {
			t.Fatalf("reconciler restart reconcile: %v", err)
		}
		if len(restartChanges) != 0 {
			t.Fatalf("expected no duplicate route changes after verified adoption, got %d", len(restartChanges))
		}
	}
	count, err := routePathCountOnPeer(receiver, expectA)
	if err != nil {
		t.Fatalf("count remote paths after reconciler restart: %v", err)
	}
	if count != 1 {
		t.Fatalf("unexpected path count after reconciler restart: got=%d want=1", count)
	}
	t.Log("reconciler restart preserved the existing route without unverified writes")

	// GoBGP speaker restart: session drops, route disappears, session re-establishes, route republished.
	if err := advertiser.StopBgp(ctx, &api.StopBgpRequest{}); err != nil {
		t.Fatalf("stop advertiser before restart: %v", err)
	}
	waitForSessionNotEstablished(t, receiver, 65311, 20*time.Second)
	waitFor(t, 30*time.Second, 300*time.Millisecond, "route invalidated after advertiser stop", func() (bool, error) {
		v, err := routeViewOnPeer(receiver, expectA)
		if err != nil {
			return false, err
		}
		return v == nil, nil
	})

	advertiser, advertised = makeRecoveryAdvertiser(t, ctx, 65311, "10.0.0.51", portAdv, "127.0.0.1", 65312, portRecv)
	defer advertiser.StopBgp(ctx, &api.StopBgpRequest{})
	waitForSessionState(t, advertiser, 65312, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)
	waitForSessionState(t, receiver, 65311, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)

	// Restarted process cannot trust in-memory state; rebuild and advertise desired from observer.
	reconciler = NewRouteReconciler()
	reAdvertised := false
	waitFor(t, 40*time.Second, 300*time.Millisecond, "route republished after advertiser restart", func() (bool, error) {
		desired, changes, err := reconcileOnce(ctx, observer, reconciler, advertiser, advertised)
		if err != nil {
			return false, err
		}
		if len(changes) > 0 {
			reAdvertised = true
		}
		if len(desired) != 1 {
			return false, nil
		}
		v, err := routeViewOnPeer(receiver, expectA)
		if err != nil || v == nil {
			return false, err
		}
		return reAdvertised, nil
	})
	viewAfterRestart, _ := routeViewOnPeer(receiver, expectA)
	t.Logf("remote RIB after speaker restart: mac=%s ip=%s nexthops=%v", viewAfterRestart.MAC, viewAfterRestart.IP, viewAfterRestart.NextHops)

	// Endpoint changes during BGP outage.
	if err := advertiser.StopBgp(ctx, &api.StopBgpRequest{}); err != nil {
		t.Fatalf("stop advertiser for outage scenario: %v", err)
	}
	waitForSessionNotEstablished(t, receiver, 65311, 20*time.Second)
	waitFor(t, 30*time.Second, 300*time.Millisecond, "route removed during outage", func() (bool, error) {
		v, err := routeViewOnPeer(receiver, expectA)
		if err != nil {
			return false, err
		}
		return v == nil, nil
	})

	mustKubectl(t, ctx, kctx, "-n", testNS, "delete", "pod", podA, "--wait=true", "--timeout=240s")
	createPodB := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  annotations:
    k8s.v1.cni.cncf.io/networks: '[{"name":"%s","interface":"net1"}]'
spec:
  nodeName: %s
  containers:
  - name: bb
    image: busybox:1.36
    command: ["sh","-lc","sleep 3600"]
`, podB, testNS, nadB, localNode)
	if _, err := runCmdInput(ctx, createPodB, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("create pod B during outage: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podB, "--timeout=240s")

	advertiser, advertised = makeRecoveryAdvertiser(t, ctx, 65311, "10.0.0.51", portAdv, "127.0.0.1", 65312, portRecv)
	defer advertiser.StopBgp(ctx, &api.StopBgpRequest{})
	waitForSessionState(t, advertiser, 65312, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)
	waitForSessionState(t, receiver, 65311, api.PeerState_SESSION_STATE_ESTABLISHED, 20*time.Second)

	reconciler = NewRouteReconciler()
	expectB := LocalEndpoint{
		NetworkID:          networkID,
		MACAddress:         macB,
		IPAddresses:        []string{ipB},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		LocalVTEP:          localVTEP,
		Advertise:          true,
	}
	waitFor(t, 50*time.Second, 300*time.Millisecond, "recover desired state after outage endpoint changes", func() (bool, error) {
		desired, _, err := reconcileOnce(ctx, observer, reconciler, advertiser, advertised)
		if err != nil {
			return false, err
		}
		if len(desired) != 1 {
			return false, nil
		}
		vOld, err := routeViewOnPeer(receiver, expectA)
		if err != nil {
			return false, err
		}
		vNew, err := routeViewOnPeer(receiver, expectB)
		if err != nil {
			return false, err
		}
		return vOld == nil && vNew != nil && len(vNew.NextHops) == 1 && vNew.NextHops[0] == localVTEP, nil
	})
	oldAfterRecovery, _ := routeViewOnPeer(receiver, expectA)
	newAfterRecovery, _ := routeViewOnPeer(receiver, expectB)
	t.Logf("remote RIB after outage changes: oldRoutePresent=%t newRoutePresent=%t newNexthops=%v", oldAfterRecovery != nil, newAfterRecovery != nil, newAfterRecovery.NextHops)
	if oldAfterRecovery != nil {
		t.Fatalf("stale route still present after recovery: %+v", oldAfterRecovery)
	}

	// Final cleanup: deletion withdraws remaining route.
	mustKubectl(t, ctx, kctx, "-n", testNS, "delete", "pod", podB, "--wait=true", "--timeout=240s")
	waitFor(t, 40*time.Second, 300*time.Millisecond, "final withdrawal", func() (bool, error) {
		_, _, err := reconcileOnce(ctx, observer, reconciler, advertiser, advertised)
		if err != nil {
			return false, err
		}
		v, err := routeViewOnPeer(receiver, expectB)
		if err != nil {
			return false, err
		}
		return v == nil, nil
	})
}
