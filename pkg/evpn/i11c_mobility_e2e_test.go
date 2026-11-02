package evpn

import (
	"context"
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

type ribRouteView struct {
	RouteType uint8
	RD        string
	MAC       string
	IP        string
	Labels    []uint32
	NextHops  []string
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func routeViewOnPeer(s *server.BgpServer, endpoint LocalEndpoint) (*ribRouteView, error) {
	wantIP := primaryIP(endpoint.IPAddresses)
	var view *ribRouteView

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
		if macRoute.MacAddress.String() != endpoint.MACAddress || macRoute.IPAddress.String() != wantIP {
			return
		}
		v := &ribRouteView{
			RouteType: evpnNLRI.RouteType,
			RD:        macRoute.RD.String(),
			MAC:       macRoute.MacAddress.String(),
			IP:        macRoute.IPAddress.String(),
			Labels:    append([]uint32(nil), macRoute.Labels...),
		}
		for _, p := range paths {
			for _, attr := range p.Attrs {
				switch a := attr.(type) {
				case *bgp.PathAttributeMpReachNLRI:
					v.NextHops = append(v.NextHops, a.Nexthop.String())
				case *bgp.PathAttributeNextHop:
					v.NextHops = append(v.NextHops, a.Value.String())
				}
			}
		}
		v.NextHops = uniqueStrings(v.NextHops)
		view = v
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

func runBothReconcilers(ctx context.Context, obsA, obsB EndpointObserver, recA, recB *RouteReconciler, spkA, spkB *server.BgpServer, advA, advB *GoBGPType2Adapter) ([]LocalEndpoint, []Type2Route, []LocalEndpoint, []Type2Route, error) {
	desiredA, changesA, err := reconcileOnce(ctx, obsA, recA, spkA, advA)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	desiredB, changesB, err := reconcileOnce(ctx, obsB, recB, spkB, advB)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return desiredA, changesA, desiredB, changesB, nil
}

func TestI11cMobilityAcrossNodes(t *testing.T) {
	if os.Getenv("I11C_E2E") != "1" {
		t.Skip("set I11C_E2E=1 to run mobility e2e")
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
	controlNode := strings.TrimSpace(os.Getenv("I11C_CONTROL_NODE"))
	if controlNode == "" {
		controlNode = "kind0-control-plane"
	}
	workerNode := strings.TrimSpace(os.Getenv("I11C_WORKER_NODE"))
	if workerNode == "" {
		workerNode = "kind0-worker"
	}

	testNS := "i11c-evpn"
	lsName := "ls-i11c"
	nadName := "evpn-i11c"
	podName := "i11c-mobility"
	networkID := "net-i11c"
	attachmentID := "net1"
	ipCIDR := "10.251.3.11/24"
	ipHost := "10.251.3.11"
	mac := "0a:58:fb:03:00:11"
	rd := "65000:1200"
	rt := "65000:120"
	vni := uint32(120)
	vtepA := "10.0.0.21"
	vtepB := "10.0.0.22"

	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	controlOVSPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs-node-local-cp", controlNode)
	workerOVSPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs", workerNode)

	controlSystemID := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", controlOVSPod, "-c", "ovn-ovs-node-local", "--", "sh", "-lc", "ovs-vsctl --db=unix:/var/run/openvswitch/db.sock get Open_vSwitch . external-ids:system-id"))
	workerSystemID := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", workerOVSPod, "-c", "ovn-ovs", "--", "sh", "-lc", "ovs-vsctl --db=unix:/var/run/openvswitch/db.sock get Open_vSwitch . external-ids:system-id"))
	controlChassis := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s'", controlSystemID)))
	workerChassis := parseSingleLine(mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s'", workerSystemID)))
	if controlChassis == "" || workerChassis == "" {
		t.Fatalf("failed to resolve chassis ids control=%q worker=%q", controlChassis, workerChassis)
	}

	cleanup := func() {
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "pod", podName, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "network-attachment-definition", nadName, "--ignore-not-found=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "delete", "namespace", testNS, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", "ovn-nbctl --if-exists ls-del ls-i11c")
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
`, testNS, nadName, testNS, nadName, networkID, lsName, attachmentID, ipCIDR, mac)
	if _, err := runCmdInput(ctx, manifest, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply namespace/nad: %v", err)
	}

	makePodManifest := func(nodeName string) string {
		return fmt.Sprintf(`
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
`, podName, testNS, nadName, nodeName)
	}

	lspName := fmt.Sprintf("%s_%s_%s_%s", testNS, podName, networkID, attachmentID)
	source := &OVNCtlSource{
		Runner:      ExecRunner{},
		KubeContext: kctx,
		Namespace:   ovnNS,
		CentralPod:  centralPod,
		Container:   "ovn-ovs",
	}

	obsA := &OVNEndpointObserver{
		Source:       source,
		LocalNode:    controlNode,
		LocalChassis: controlChassis,
		Networks: map[string]EVPNNetworkConfig{
			networkID: {
				NetworkID:          networkID,
				Enabled:            true,
				VNI:                vni,
				RouteDistinguisher: rd,
				RouteTargets:       []string{rt},
				LocalVTEP:          vtepA,
			},
		},
	}
	obsB := &OVNEndpointObserver{
		Source:       source,
		LocalNode:    workerNode,
		LocalChassis: workerChassis,
		Networks: map[string]EVPNNetworkConfig{
			networkID: {
				NetworkID:          networkID,
				Enabled:            true,
				VNI:                vni,
				RouteDistinguisher: rd,
				RouteTargets:       []string{rt},
				LocalVTEP:          vtepB,
			},
		},
	}

	portA := reserveTCPPort(t)
	portB := reserveTCPPort(t)
	portC := reserveTCPPort(t)
	speakerA, advA := newType2TestSpeaker(t, portA, portB, portC)
	defer speakerA.StopBgp(context.Background(), &api.StopBgpRequest{})
	speakerB, advB := newType2TestSpeaker(t, portA, portB, portC)
	defer speakerB.StopBgp(context.Background(), &api.StopBgpRequest{})
	speakerC := server.NewBgpServer()
	go speakerC.Serve()
	defer speakerC.StopBgp(context.Background(), &api.StopBgpRequest{})

	if err := speakerA.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65210, RouterId: "10.0.0.31", ListenPort: portA}}); err != nil {
		t.Fatalf("start speaker A: %v", err)
	}
	if err := speakerB.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65211, RouterId: "10.0.0.32", ListenPort: portB}}); err != nil {
		t.Fatalf("start speaker B: %v", err)
	}
	if err := speakerC.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65212, RouterId: "10.0.0.33", ListenPort: portC}}); err != nil {
		t.Fatalf("start speaker C: %v", err)
	}

	addPeer := func(s *server.BgpServer, neigh string, localAddr string, asn uint32, remotePort int32) {
		if err := s.AddPeer(ctx, &api.AddPeerRequest{
			Peer: &api.Peer{
				Conf:      &api.PeerConf{NeighborAddress: neigh, PeerAsn: asn},
				Transport: &api.Transport{LocalAddress: localAddr, RemotePort: uint32(remotePort)},
				AfiSafis:  []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true}}},
			},
		}); err != nil {
			t.Fatalf("add peer neigh=%s local=%s asn=%d: %v", neigh, localAddr, asn, err)
		}
	}
	addPeer(speakerA, "127.0.0.1", "127.0.0.1", 65212, portC)
	addPeer(speakerB, "127.0.0.1", "127.0.0.2", 65212, portC)
	addPeer(speakerC, "127.0.0.1", "127.0.0.1", 65210, portA)
	addPeer(speakerC, "127.0.0.2", "127.0.0.1", 65211, portB)

	waitForEstablished(t, speakerA, 65212, 20*time.Second)
	waitForEstablished(t, speakerB, 65212, 20*time.Second)
	waitForEstablished(t, speakerC, 65210, 20*time.Second)
	waitForEstablished(t, speakerC, 65211, 20*time.Second)

	recA := NewRouteReconciler()
	recB := NewRouteReconciler()

	// Initial ownership on control-plane (speaker A).
	if _, err := runCmdInput(ctx, makePodManifest(controlNode), "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("create control-plane pod: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podName, "--timeout=240s")
	mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podName, "--", "sh", "-lc", "ip -4 addr show dev net1 | grep -q "+ipCIDR)

	waitFor(t, 40*time.Second, 300*time.Millisecond, "initial control chassis binding", func() (bool, error) {
		bindings, err := source.ListPortBindings(ctx)
		if err != nil {
			return false, err
		}
		for _, b := range bindings {
			if b.LogicalPort == lspName {
				return b.Chassis == controlChassis, nil
			}
		}
		return false, nil
	})

	initialEndpoint := LocalEndpoint{
		NetworkID:          networkID,
		MACAddress:         mac,
		IPAddresses:        []string{ipHost},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		Advertise:          true,
	}

	waitFor(t, 40*time.Second, 300*time.Millisecond, "initial advertisement from speaker A only", func() (bool, error) {
		desiredA, _, desiredB, _, err := runBothReconcilers(ctx, obsA, obsB, recA, recB, speakerA, speakerB, advA, advB)
		if err != nil {
			return false, err
		}
		if len(desiredA) != 1 || len(desiredB) != 0 {
			return false, nil
		}
		view, err := routeViewOnPeer(speakerC, initialEndpoint)
		if err != nil || view == nil {
			return false, err
		}
		return len(view.NextHops) == 1 && view.NextHops[0] == vtepA, nil
	})

	viewBefore, err := routeViewOnPeer(speakerC, initialEndpoint)
	if err != nil || viewBefore == nil {
		t.Fatalf("missing initial route on speaker C: %v", err)
	}
	t.Logf("initial ownership: control chassis=%s route nexthops=%v rd=%s labels=%v", controlChassis, viewBefore.NextHops, viewBefore.RD, viewBefore.Labels)

	// Mobility to worker: delete then recreate same MAC/IP with new incarnation.
	mustKubectl(t, ctx, kctx, "-n", testNS, "delete", "pod", podName, "--wait=true", "--timeout=240s")
	waitFor(t, 40*time.Second, 300*time.Millisecond, "old lsp cleanup", func() (bool, error) {
		bindings, err := source.ListPortBindings(ctx)
		if err != nil {
			return false, err
		}
		for _, b := range bindings {
			if b.LogicalPort == lspName && b.Chassis != "" {
				return false, nil
			}
		}
		return true, nil
	})
	waitFor(t, 40*time.Second, 300*time.Millisecond, "old route withdrawal", func() (bool, error) {
		_, _, _, _, err := runBothReconcilers(ctx, obsA, obsB, recA, recB, speakerA, speakerB, advA, advB)
		if err != nil {
			return false, err
		}
		view, err := routeViewOnPeer(speakerC, initialEndpoint)
		if err != nil {
			return false, err
		}
		return view == nil, nil
	})

	if _, err := runCmdInput(ctx, makePodManifest(workerNode), "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("create worker pod: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podName, "--timeout=240s")

	waitFor(t, 40*time.Second, 300*time.Millisecond, "worker ownership and new route", func() (bool, error) {
		desiredA, _, desiredB, _, err := runBothReconcilers(ctx, obsA, obsB, recA, recB, speakerA, speakerB, advA, advB)
		if err != nil {
			return false, err
		}
		if len(desiredA) != 0 || len(desiredB) != 1 {
			return false, nil
		}
		bindings, err := source.ListPortBindings(ctx)
		if err != nil {
			return false, err
		}
		ownedByWorker := false
		for _, b := range bindings {
			if b.LogicalPort == lspName {
				ownedByWorker = b.Chassis == workerChassis
				break
			}
		}
		if !ownedByWorker {
			return false, nil
		}
		view, err := routeViewOnPeer(speakerC, initialEndpoint)
		if err != nil || view == nil {
			return false, err
		}
		// Converged state must not keep old next-hop.
		return len(view.NextHops) == 1 && view.NextHops[0] == vtepB, nil
	})

	viewAfter, err := routeViewOnPeer(speakerC, initialEndpoint)
	if err != nil || viewAfter == nil {
		t.Fatalf("missing post-mobility route on speaker C: %v", err)
	}
	if viewAfter.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT || viewAfter.RD != rd || viewAfter.MAC != mac || viewAfter.IP != ipHost || len(viewAfter.Labels) == 0 || viewAfter.Labels[0] != vni {
		t.Fatalf("unexpected post-mobility route view: %+v", viewAfter)
	}
	t.Logf("new ownership: worker chassis=%s route nexthops=%v rd=%s labels=%v", workerChassis, viewAfter.NextHops, viewAfter.RD, viewAfter.Labels)

	// Stale-event protection in remote RIB: repeated delayed old-node deletion/reconcile cannot remove worker-owned route.
	for i := 0; i < 3; i++ {
		if _, _, err := reconcileOnce(ctx, obsA, recA, speakerA, advA); err != nil {
			t.Fatalf("stale-event simulate on A reconcile #%d: %v", i+1, err)
		}
	}
	viewAfterStale, err := routeViewOnPeer(speakerC, initialEndpoint)
	if err != nil || viewAfterStale == nil || len(viewAfterStale.NextHops) != 1 || viewAfterStale.NextHops[0] != vtepB {
		t.Fatalf("stale-event changed remote rib unexpectedly: view=%+v err=%v", viewAfterStale, err)
	}

	// Reconciler-level stale-incarnation safety.
	old := LocalEndpoint{
		NetworkID:          networkID,
		EndpointID:         "mobility-ep",
		Incarnation:        "inc-old",
		MACAddress:         mac,
		IPAddresses:        []string{ipHost},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		LocalVTEP:          vtepA,
		Advertise:          true,
	}
	newer := old
	newer.Incarnation = "inc-new"
	newer.LocalVTEP = vtepB
	staleAPI := &memoryType2API{}
	staleRec := syncedReconciler(t, staleAPI)
	applyType2(t, staleRec, staleAPI, planType2(t, staleRec, old))
	staleChanges, err := staleRec.Reconcile([]LocalEndpoint{newer})
	if err != nil {
		t.Fatalf("stale reconcile newer: %v", err)
	}
	if len(staleChanges) != 2 || staleChanges[0].Action != RouteWithdraw || staleChanges[1].Action != RouteAdvertise {
		t.Fatalf("unexpected stale changes: %#v", staleChanges)
	}
}
