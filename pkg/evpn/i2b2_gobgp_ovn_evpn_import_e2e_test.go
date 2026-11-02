package evpn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func TestGoBGPFRRType2OVNNetlinkImport(t *testing.T) {
	if os.Getenv("I2B2_E2E") != "1" {
		t.Skip("set I2B2_E2E=1 to run OVN netlink EVPN Type-2 importer e2e")
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
	workerNode := strings.TrimSpace(os.Getenv("I2B2_WORKER_NODE"))
	if workerNode == "" {
		workerNode = "kind0-worker"
	}
	localVTEP := strings.TrimSpace(os.Getenv("I2B2_LOCAL_VTEP"))
	if localVTEP == "" {
		localVTEP = "172.19.0.2"
	}

	frrIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	localIP := strings.TrimSpace(os.Getenv("GOBGP_LOCAL_IP"))
	if localIP == "" {
		localIP = "172.19.0.1"
	}
	remoteMAC := "12:87:9b:c4:17:56"
	remoteIP := "192.168.100.1"

	asn := uint32(65032)
	listenPort := reserveTCPPort(t)
	gobgp := server.NewBgpServer()
	go gobgp.Serve()
	defer gobgp.StopBgp(context.Background(), &api.StopBgpRequest{})
	if err := gobgp.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: localIP, ListenPort: listenPort}}); err != nil {
		t.Fatalf("start local GoBGP speaker: %v", err)
	}
	peer := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: frrIP, PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: localIP, RemotePort: 179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1, IdleHoldTimeAfterReset: 1}},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true},
		}},
	}
	if err := gobgp.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer}); err != nil {
		t.Fatalf("add GoBGP peer to FRR: %v", err)
	}
	setFRRNeighborShutdown(t, localIP, false)
	configureFRRNeighbor(t, localIP, asn)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)

	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)
	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
	defer ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)

	remoteEndpoint := LocalEndpoint{
		NetworkID:          "net-i2b2-remote",
		EndpointID:         "ep-i2b2-remote",
		Incarnation:        "inc-i2b2-remote-1",
		MACAddress:         remoteMAC,
		IPAddresses:        []string{remoteIP + "/24", remoteIP},
		VNI:                1000,
		RouteDistinguisher: "172.19.0.5:2",
		RouteTargets:       []string{"64512:1000"},
		LocalVTEP:          frrVTEPIP,
		Advertise:          true,
	}
	waitForGoBGPRoutePresent(t, gobgp, remoteEndpoint, 25*time.Second)

	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	workerPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs", workerNode)
	mustOVNExec(
		t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		"ovn-nbctl --may-exist ls-add ls-h22; "+
			"ovn-nbctl set Logical_Switch ls-h22 other_config:dynamic-routing-vni=1000 "+
			"other_config:dynamic-routing-bridge-ifname=br-evpn1000 "+
			"other_config:dynamic-routing-vxlan-ifname=vxlan1000 "+
			"other_config:dynamic-routing-advertise-ifname=evpnadv1000 "+
			"other_config:dynamic-routing-redistribute=fdb,ip",
	)

	ovsExternalIDs := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovs-vsctl get Open_vSwitch . external_ids")
	if !strings.Contains(ovsExternalIDs, "ovn-evpn-vxlan-ports") {
		t.Fatalf("OVN worker missing ovn-evpn-vxlan-ports external_ids:\n%s", ovsExternalIDs)
	}

	importer := NewRemoteType2Importer(
		&GoBGPServerSnapshotSource{Server: gobgp},
		&Type2RouteValidator{
			LocalVTEP: localIP,
			Networks: []EVPNImportNetwork{
				{
					Name:           "ls-h22",
					VNI:            1000,
					RouteTarget:    "64512:1000",
					VXLANDevice:    "vxlan1000",
					BridgeDevice:   "br-evpn1000",
					NeighborDevice: "br-evpn1000",
				},
			},
		},
		&OVNNetlinkBackend{
			KubeContext: kctx,
			Namespace:   ovnNS,
			Pod:         workerPod,
			Container:   "ovn-ovs",
		},
	)

	reconcileUntil := func(timeout time.Duration) {
		deadline := time.Now().Add(timeout)
		var lastErr error
		for time.Now().Before(deadline) {
			lastErr = importer.ReconcileOnce(context.Background())
			if lastErr == nil {
				time.Sleep(200 * time.Millisecond)
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		if lastErr != nil {
			t.Fatalf("importer reconcile failed: %v", lastErr)
		}
	}
	reconcileUntil(20 * time.Second)

	waitFor(t, 25*time.Second, 500*time.Millisecond, "OVN netlink import state", func() (bool, error) {
		fdb := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")
		neigh := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ip neigh show to "+remoteIP+" dev br-evpn1000")
		if !strings.Contains(strings.ToLower(fdb), strings.ToLower(remoteMAC)) || !strings.Contains(fdb, "dst "+frrVTEPIP) {
			return false, nil
		}
		if !strings.Contains(strings.ToLower(neigh), strings.ToLower(remoteMAC)) {
			return false, nil
		}
		cache := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovn-appctl -t ovn-controller evpn/vtep-fdb-list")
		return strings.Contains(strings.ToLower(cache), strings.ToLower(remoteMAC)), nil
	})

	testNS := "i2b2-evpn"
	nadName := "evpn-i2b2"
	podName := "h22-i2b2"
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
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"net-h22","logicalSwitch":"ls-h22","attachmentID":"net1","ipAddress":"10.251.1.11/24","macAddress":"0a:58:fb:01:00:22","timeoutSeconds":10}
---
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
  - name: tools
    image: nicolaka/netshoot:v0.13
    command: ["sh","-lc","sleep 3600"]
`, testNS, nadName, testNS, nadName, podName, testNS, nadName, workerNode)

	defer func() {
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "pod", podName, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "network-attachment-definition", nadName, "--ignore-not-found=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "delete", "namespace", testNS, "--ignore-not-found=true", "--wait=true")
	}()
	if _, err := runCmdInput(ctx, manifest, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply i2b2 pod/NAD manifest: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podName, "--timeout=180s")

	flowBefore := i2b2Table87Packets(t, ctx, kctx, ovnNS, workerPod, remoteMAC)
	captureWithRoute := i2b2CaptureFRRUDP4789(t, func() {
		mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
			fmt.Sprintf("nping --interface net1 --send-eth --udp -p 33434 --source-ip 10.251.1.11 --dest-ip %s --dest-mac %s -c 3 --quiet", remoteIP, remoteMAC))
	})
	flowAfter := i2b2Table87Packets(t, ctx, kctx, ovnNS, workerPod, remoteMAC)
	if flowAfter <= flowBefore {
		t.Fatalf("table 87 remote-MAC counter did not increase (before=%d after=%d)", flowBefore, flowAfter)
	}
	if !strings.Contains(captureWithRoute, "VXLAN") || !strings.Contains(captureWithRoute, "vni 1000") {
		t.Fatalf("expected VXLAN VNI 1000 packet on FRR underlay, got:\n%s", captureWithRoute)
	}

	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)
	waitForGoBGPRouteAbsent(t, gobgp, remoteEndpoint, 20*time.Second)
	reconcileUntil(20 * time.Second)
	waitFor(t, 20*time.Second, 500*time.Millisecond, "withdrawn OVN netlink import state", func() (bool, error) {
		fdb := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")
		neigh := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ip neigh show to "+remoteIP+" dev br-evpn1000")
		return !strings.Contains(strings.ToLower(fdb), strings.ToLower(remoteMAC)) &&
			!strings.Contains(strings.ToLower(neigh), strings.ToLower(remoteMAC)), nil
	})

	flowBeforeWithdrawProbe := i2b2Table87Packets(t, ctx, kctx, ovnNS, workerPod, remoteMAC)
	captureAfterWithdraw := i2b2CaptureFRRUDP4789(t, func() {
		mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
			fmt.Sprintf("nping --interface net1 --send-eth --udp -p 33434 --source-ip 10.251.1.11 --dest-ip %s --dest-mac %s -c 3 --quiet", remoteIP, remoteMAC))
	})
	flowAfterWithdrawProbe := i2b2Table87Packets(t, ctx, kctx, ovnNS, workerPod, remoteMAC)
	if flowAfterWithdrawProbe > flowBeforeWithdrawProbe {
		t.Fatalf("stale unicast flow activity after withdrawal (table87 counter increased for withdrawn MAC %s): before=%d after=%d\n%s",
			remoteMAC, flowBeforeWithdrawProbe, flowAfterWithdrawProbe, captureAfterWithdraw)
	}
	if i2b2ContainsWithdrawnProbe(captureAfterWithdraw, remoteIP) {
		t.Fatalf("unexpected withdrawn unicast probe forwarding after withdrawal:\n%s", captureAfterWithdraw)
	}
	if strings.Contains(captureAfterWithdraw, "vni 1000") {
		t.Logf("observed VXLAN traffic after withdrawal, but no withdrawn unicast signature (likely unrelated BUM):\n%s", captureAfterWithdraw)
	}

	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
	waitForGoBGPRoutePresent(t, gobgp, remoteEndpoint, 20*time.Second)
	reconcileUntil(20 * time.Second)
	captureRestored := i2b2CaptureFRRUDP4789(t, func() {
		mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
			fmt.Sprintf("nping --interface net1 --send-eth --udp -p 33434 --source-ip 10.251.1.11 --dest-ip %s --dest-mac %s -c 3 --quiet", remoteIP, remoteMAC))
	})
	if !strings.Contains(captureRestored, "vni 1000") {
		t.Fatalf("expected VXLAN forwarding to recover after re-advertisement:\n%s", captureRestored)
	}

	waitForEstablished(t, gobgp, 64512, 10*time.Second)
	t.Logf("[I2-B.2] FRR->GoBGP Type-2 import -> OVN netlink -> OVN VXLAN delivery validated (local_vtep=%s remote_vtep=%s)", localVTEP, frrVTEPIP)
}

func i2b2CaptureFRRUDP4789(t *testing.T, send func()) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", frrContainerName(), "sh", "-lc", "timeout 10 tcpdump -ni eth1 -vv -c 4 udp port 4789")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start FRR tcpdump: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	send()
	_ = cmd.Wait()
	return out.String()
}

func i2b2ContainsWithdrawnProbe(capture, remoteIP string) bool {
	s := strings.ToLower(capture)
	needle := strings.ToLower("> " + remoteIP + ".33434")
	return strings.Contains(s, "vxlan") && strings.Contains(s, needle)
}

func i2b2Table87Packets(t *testing.T, ctx context.Context, kctx, ovnNS, workerPod, remoteMAC string) int {
	t.Helper()
	flows := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovs-ofctl dump-flows br-int table=87")
	re := regexp.MustCompile(`n_packets=(\d+)`)
	maxPackets := 0
	want := "dl_dst=" + strings.ToLower(strings.TrimSpace(remoteMAC))
	for _, line := range strings.Split(flows, "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, want) {
			continue
		}
		m := re.FindStringSubmatch(line)
		if len(m) != 2 {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if v > maxPackets {
			maxPackets = v
		}
	}
	return maxPackets
}
