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
)

func TestI2B3aReverseEVPNPathAndVTEPValidation(t *testing.T) {
	if os.Getenv("I2B3_E2E") != "1" {
		t.Skip("set I2B3_E2E=1 to run I2-B.3a reverse EVPN path validation")
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
	workerNode := strings.TrimSpace(os.Getenv("I2B3_WORKER_NODE"))
	if workerNode == "" {
		workerNode = "kind0-worker"
	}
	localBGPIP := strings.TrimSpace(os.Getenv("GOBGP_LOCAL_IP"))
	if localBGPIP == "" {
		localBGPIP = "172.19.0.1"
	}

	frrNeighborIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	workerPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs", workerNode)

	workerSystemID := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", "ovs-vsctl get Open_vSwitch . external-ids:system-id",
	))
	if workerSystemID == "" {
		t.Fatalf("failed to resolve worker system-id from OVS external_ids")
	}
	workerVTEP := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", "ovs-vsctl get Open_vSwitch . external-ids:ovn-encap-ip",
	))
	if workerVTEP == "" {
		t.Fatalf("failed to resolve worker ovn-encap-ip from OVS external_ids")
	}
	sbEncapIP := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=ip find Encap chassis_name=%s | head -n1", workerSystemID),
	))
	if sbEncapIP != "" && sbEncapIP != workerVTEP {
		t.Fatalf("OVS ovn-encap-ip (%s) != SB Encap.ip (%s) for chassis %s", workerVTEP, sbEncapIP, workerSystemID)
	}
	workerChassisUUID := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s' | head -n1", workerSystemID),
	))
	if workerChassisUUID == "" {
		t.Fatalf("failed to resolve worker chassis UUID for system-id %s", workerSystemID)
	}
	t.Logf("[I2-B.3a] addresses: gobgp_bgp_source=%s ovn_worker_system_id=%s ovn_worker_chassis_uuid=%s ovn_worker_vtep=%s frr_bgp_neighbor=%s frr_vtep=%s", localBGPIP, workerSystemID, workerChassisUUID, workerVTEP, frrNeighborIP, frrVTEPIP)

	// Prepare FRR bridge endpoint used as a real Linux source endpoint.
	const (
		frrEndpointHostIf = "frri2b3h"
		frrEndpointIf     = "frri2b3ep"
		frrEndpointMAC    = "aa:bb:cc:dd:ee:33"
		frrEndpointIP     = "10.251.1.12"
	)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true", frrEndpointHostIf, frrEndpointIf))
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip link add %s type veth peer name %s; ip link set %s master br1000; ip link set %s up; ip link set dev %s address %s; ip addr add %s/24 dev %s; ip link set %s up",
			frrEndpointHostIf, frrEndpointIf, frrEndpointHostIf, frrEndpointHostIf, frrEndpointIf, frrEndpointMAC, frrEndpointIP, frrEndpointIf, frrEndpointIf))
	defer func() {
		_ = runDockerCommand("docker", "exec", frrContainerName(), "sh", "-lc", fmt.Sprintf("ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true", frrEndpointHostIf, frrEndpointIf))
	}()

	asn := uint32(65034)
	listenPort := reserveTCPPort(t)
	gobgp, advertised := newType2TestSpeaker(t, listenPort)
	defer gobgp.StopBgp(context.Background(), &api.StopBgpRequest{})
	if err := gobgp.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: localBGPIP, ListenPort: listenPort}}); err != nil {
		t.Fatalf("start local GoBGP speaker: %v", err)
	}
	peer := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: frrNeighborIP, PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: localBGPIP, RemotePort: 179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1, IdleHoldTimeAfterReset: 1}},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true},
		}},
	}
	if err := gobgp.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer}); err != nil {
		t.Fatalf("add GoBGP peer to FRR: %v", err)
	}
	setFRRNeighborShutdown(t, localBGPIP, false)
	configureFRRNeighbor(t, localBGPIP, asn)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)

	testNS := "i2b3a-evpn"
	nadName := "evpn-i2b3a"
	podName := "h22-i2b3a"
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
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"net-h22","logicalSwitch":"ls-h22","attachmentID":"net1","ipAddress":"10.251.1.11/24","macAddress":"0a:58:fb:01:00:11","timeoutSeconds":10}
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
		t.Fatalf("apply i2b3a pod/NAD manifest: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podName, "--timeout=180s")

	podMAC := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--",
		"sh", "-lc", "ip -o link show dev net1 | awk '{for(i=1;i<=NF;i++) if ($i==\"link/ether\") {print $(i+1); exit}}'",
	))
	podIP := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--",
		"sh", "-lc", "ip -4 -o addr show dev net1 | awk '{print $4}' | cut -d/ -f1 | head -n1",
	))
	if podMAC == "" || podIP == "" {
		t.Fatalf("failed to resolve pod net1 identity: mac=%q ip=%q", podMAC, podIP)
	}

	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		"ovn-nbctl --may-exist ls-add ls-h22; "+
			"ovn-nbctl set Logical_Switch ls-h22 other_config:dynamic-routing-vni=1000 "+
			"other_config:dynamic-routing-bridge-ifname=br-evpn1000 "+
			"other_config:dynamic-routing-vxlan-ifname=vxlan1000 "+
			"other_config:dynamic-routing-advertise-ifname=evpnadv1000 "+
			"other_config:dynamic-routing-redistribute=fdb,ip")

	source := &OVNCtlSource{
		Runner:      ExecRunner{},
		KubeContext: kctx,
		Namespace:   ovnNS,
		CentralPod:  centralPod,
		Container:   "ovn-ovs",
	}
	observer := &OVNEndpointObserver{
		Source:       source,
		LocalNode:    workerNode,
		LocalChassis: workerChassisUUID,
		Networks: map[string]EVPNNetworkConfig{
			"net-h22": {
				NetworkID:          "net-h22",
				Enabled:            true,
				VNI:                1000,
				RouteDistinguisher: "65034:1000",
				RouteTargets:       []string{"64512:1000"},
				LocalVTEP:          workerVTEP,
			},
		},
	}
	reconciler := NewRouteReconciler()

	waitFor(t, 30*time.Second, 400*time.Millisecond, "observer-driven GoBGP export for OVN endpoint", func() (bool, error) {
		desired, _, err := reconcileOnce(ctx, observer, reconciler, gobgp, advertised)
		if err != nil {
			return false, err
		}
		for _, ep := range desired {
			if strings.EqualFold(ep.MACAddress, podMAC) && primaryIP(ep.IPAddresses) == podIP {
				return true, nil
			}
		}
		return false, nil
	})

	waitForFRRRoutePresent(t, podMAC, podIP, 25*time.Second)
	frrTable := frrEVPNTable(t)
	if !strings.Contains(strings.ToLower(frrTable), strings.ToLower(podMAC)) ||
		!strings.Contains(frrTable, podIP) ||
		!strings.Contains(frrTable, "RT:64512:1000") ||
		!strings.Contains(frrTable, workerVTEP) {
		t.Fatalf("FRR EVPN RIB missing expected exported endpoint route or VTEP\n%s", frrTable)
	}

	waitForFRRVXLANFDBPresent(t, podMAC, workerVTEP, 25*time.Second)
	frrFDB := frrVXLANFDB(t)
	t.Logf("[I2-B.3a] FRR FDB for pod route: %s", strings.TrimSpace(frrFDB))

	// Verify GoBGP exported next-hop equals actual OVN worker VTEP.
	snapshot, err := (&GoBGPServerSnapshotSource{Server: gobgp}).Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot GoBGP routes: %v", err)
	}
	nextHopMatched := false
	for _, r := range snapshot {
		if strings.EqualFold(r.MAC, podMAC) && strings.TrimSpace(r.IP) == podIP {
			if strings.TrimSpace(r.NextHop) == workerVTEP {
				nextHopMatched = true
				break
			}
		}
	}
	if !nextHopMatched {
		t.Fatalf("GoBGP exported route for pod did not use actual OVN worker VTEP %s", workerVTEP)
	}

	lspName := fmt.Sprintf("%s_%s_net-h22_net1", testNS, podName)
	podOFPort := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=ofport find Interface external_ids:iface-id=%s | head -n1", lspName),
	))
	if podOFPort == "" {
		t.Fatalf("failed to resolve pod ofport for LSP %s", lspName)
	}
	beforeTX := brIntPortTxPackets(t, ctx, kctx, ovnNS, workerPod, podOFPort)

	// Minimal reverse probe: one raw Ethernet frame from FRR endpoint to OVN pod MAC.
	frrEpCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -eni %s -c 1 ether dst %s", frrEndpointIf, podMAC))
	frrUnderlayCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -ni eth1 -vv -c 2 udp port 4789 and host %s", workerVTEP))
	ovnUnderlayCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -ni eth0 -vv -c 2 udp port 4789 and host %s", frrVTEPIP))
	podIngressCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -eni net1 -c 1 ether dst %s", podMAC))

	time.Sleep(1200 * time.Millisecond)
	sendScript := fmt.Sprintf(`python3 - <<'PY'
import socket
iface = "%s"
dst = bytes.fromhex("%s")
src = bytes.fromhex("%s")
eth = dst + src + b"\x08\x00"
payload = b"\x45\x00\x00\x2e\x00\x01\x00\x00\x40\x11\x00\x00" + b"\x0a\xfb\x01\x0c" + b"\x0a\xfb\x01\x0b" + b"\x12\x34\x56\x78\x00\x1a\x00\x00" + b"A"*18
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind((iface, 0))
s.send(eth + payload)
PY`, frrEndpointIf, strings.ReplaceAll(strings.ToLower(podMAC), ":", ""), strings.ReplaceAll(strings.ToLower(frrEndpointMAC), ":", ""))
	if out, err := exec.Command("docker", "exec", frrContainerName(), "sh", "-lc", sendScript).CombinedOutput(); err != nil {
		t.Fatalf("send reverse raw frame from FRR endpoint failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	frrEpOut, _ := frrEpCap.wait()
	frrUnderlayOut, _ := frrUnderlayCap.wait()
	ovnUnderlayOut, _ := ovnUnderlayCap.wait()
	podIngressOut, _ := podIngressCap.wait()
	afterTX := brIntPortTxPackets(t, ctx, kctx, ovnNS, workerPod, podOFPort)

	t.Logf("[I2-B.3a] reverse packet evidence:\n[frr-endpoint]\n%s\n[frr-underlay]\n%s\n[ovn-underlay]\n%s\n[pod-net1]\n%s\n[br-int port %s tx-before=%d tx-after=%d]",
		strings.TrimSpace(frrEpOut), strings.TrimSpace(frrUnderlayOut), strings.TrimSpace(ovnUnderlayOut), strings.TrimSpace(podIngressOut), podOFPort, beforeTX, afterTX)

	if !strings.Contains(strings.ToLower(frrEpOut), strings.ToLower(podMAC)) {
		t.Fatalf("missing source endpoint frame capture toward pod MAC %s\n%s", podMAC, frrEpOut)
	}
	if !strings.Contains(frrUnderlayOut, "VXLAN") || !strings.Contains(frrUnderlayOut, "vni 1000") {
		t.Fatalf("missing FRR underlay VXLAN egress evidence\n%s", frrUnderlayOut)
	}
	if !strings.Contains(ovnUnderlayOut, "VXLAN") || !strings.Contains(ovnUnderlayOut, "vni 1000") {
		t.Fatalf("missing OVN underlay VXLAN ingress evidence\n%s", ovnUnderlayOut)
	}
	if afterTX <= beforeTX {
		t.Fatalf("missing OVN br-int forwarding evidence: pod ofport %s tx did not increase (before=%d after=%d)", podOFPort, beforeTX, afterTX)
	}
	if !strings.Contains(strings.ToLower(podIngressOut), strings.ToLower(podMAC)) {
		t.Fatalf("missing OVN pod net1 ingress evidence for pod MAC %s\n%s", podMAC, podIngressOut)
	}
}

func TestI2B3bBidirectionalEVPNICMP(t *testing.T) {
	if os.Getenv("I2B3B_E2E") != "1" {
		t.Skip("set I2B3B_E2E=1 to run I2-B.3b live bidirectional EVPN ICMP validation")
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
	workerNode := strings.TrimSpace(os.Getenv("I2B3_WORKER_NODE"))
	if workerNode == "" {
		workerNode = "kind0-worker"
	}
	localBGPIP := strings.TrimSpace(os.Getenv("GOBGP_LOCAL_IP"))
	if localBGPIP == "" {
		localBGPIP = "172.19.0.1"
	}
	frrNeighborIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	if frrVTEPIP == "" {
		t.Fatal("resolved FRR VTEP IP is empty")
	}
	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	workerPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs", workerNode)

	const (
		frrEndpointNS     = "i2b3cns"
		frrEndpointHostIf = "frr-i2b3c-h"
		frrEndpointIf     = "frr-i2b3c-e"
		frrEndpointMAC    = "02:42:ac:19:00:22"
		frrEndpointIP     = "10.251.1.22"
	)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip netns del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true",
			frrEndpointNS, frrEndpointHostIf, frrEndpointIf))
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf(
			"ip netns add %s; "+
				"ip link add %s type veth peer name %s; "+
				"ip link set %s master br1000; ip link set %s up; "+
				"ip link set %s netns %s; "+
				"ip -n %s link set lo up; "+
				"ip -n %s link set dev %s address %s; "+
				"ip -n %s addr add %s/24 dev %s; "+
				"ip -n %s link set %s up",
			frrEndpointNS,
			frrEndpointHostIf, frrEndpointIf,
			frrEndpointHostIf, frrEndpointHostIf,
			frrEndpointIf, frrEndpointNS,
			frrEndpointNS,
			frrEndpointNS, frrEndpointIf, frrEndpointMAC,
			frrEndpointNS, frrEndpointIP, frrEndpointIf,
			frrEndpointNS, frrEndpointIf))
	defer func() {
		_ = runDockerCommand("docker", "exec", frrContainerName(), "sh", "-lc",
			fmt.Sprintf("ip netns del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true",
				frrEndpointNS, frrEndpointHostIf, frrEndpointIf))
	}()
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip -n %s -4 -o addr show dev %s; (bridge link show 2>/dev/null | grep -F %s || true)", frrEndpointNS, frrEndpointIf, frrEndpointHostIf))

	// Start a live GoBGP speaker and establish the EVPN peer session to FRR.
	asn := uint32(65035)
	listenPort := reserveTCPPort(t)
	gobgp, advertised := newType2TestSpeaker(t, listenPort)
	defer gobgp.StopBgp(context.Background(), &api.StopBgpRequest{})
	if err := gobgp.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: localBGPIP, ListenPort: listenPort}}); err != nil {
		t.Fatalf("start local GoBGP speaker: %v", err)
	}
	peer := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: frrNeighborIP, PeerAsn: 64512},
		Transport: &api.Transport{LocalAddress: localBGPIP, RemotePort: 179},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1, IdleHoldTimeAfterReset: 1}},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true},
		}},
	}
	if err := gobgp.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer}); err != nil {
		t.Fatalf("add GoBGP peer to FRR: %v", err)
	}
	setFRRNeighborShutdown(t, localBGPIP, false)
	configureFRRNeighbor(t, localBGPIP, asn)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)

	testNS := "i2b3b-evpn"
	nadName := "evpn-i2b3b"
	podName := "h22-i2b3b"
	// Ensure OVN LS exists before Multus/evpn-cni ADD; the node-agent validates
	// logicalSwitch existence during pod sandbox setup.
	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		"ovn-nbctl --may-exist ls-add ls-h22; "+
			"ovn-nbctl set Logical_Switch ls-h22 other_config:dynamic-routing-vni=1000 "+
			"other_config:dynamic-routing-bridge-ifname=br-evpn1000 "+
			"other_config:dynamic-routing-vxlan-ifname=vxlan1000 "+
			"other_config:dynamic-routing-advertise-ifname=evpnadv1000 "+
			"other_config:dynamic-routing-redistribute=fdb,ip")
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
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"net-h22","logicalSwitch":"ls-h22","attachmentID":"net1","ipAddress":"10.251.1.11/24","macAddress":"0a:58:fb:01:00:44","timeoutSeconds":10}
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
		t.Fatalf("apply i2b3b pod/NAD manifest: %v", err)
	}
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podName, "--timeout=180s")

	workerSystemID := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", "ovs-vsctl get Open_vSwitch . external-ids:system-id",
	))
	workerChassisUUID := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s' | head -n1", workerSystemID),
	))
	if workerChassisUUID == "" {
		t.Fatalf("failed to resolve worker chassis UUID for system-id %s", workerSystemID)
	}
	workerVTEP := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", "ovs-vsctl get Open_vSwitch . external-ids:ovn-encap-ip",
	))
	if workerVTEP == "" {
		t.Fatalf("failed to resolve worker ovn-encap-ip from OVS external_ids")
	}
	ensureFRRVXLANBridge(t, "br1000", "vxlan1000", workerVTEP, 1000)
	setFRRVXLANRemote(t, "vxlan1000", workerVTEP)
	ensureFRRBridgeIP(t, "10.251.1.1/24", "br1000", true)
	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		"ovn-nbctl --may-exist ls-add ls-h22; "+
			"ovn-nbctl set Logical_Switch ls-h22 other_config:dynamic-routing-vni=1000 "+
			"other_config:dynamic-routing-bridge-ifname=br-evpn1000 "+
			"other_config:dynamic-routing-vxlan-ifname=vxlan1000 "+
			"other_config:dynamic-routing-advertise-ifname=evpnadv1000 "+
			"other_config:dynamic-routing-redistribute=fdb,ip")

	podMAC := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--",
		"sh", "-lc", "ip -o link show dev net1 | awk '{for(i=1;i<=NF;i++) if ($i==\"link/ether\") {print $(i+1); exit}}'",
	))
	podIP := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--",
		"sh", "-lc", "ip -4 -o addr show dev net1 | awk '{print $4}' | cut -d/ -f1 | head -n1",
	))
	if podMAC == "" || podIP == "" {
		t.Fatalf("failed to resolve pod net1 identity: mac=%q ip=%q", podMAC, podIP)
	}

	observer := &OVNEndpointObserver{
		Source:       &OVNCtlSource{Runner: ExecRunner{}, KubeContext: kctx, Namespace: ovnNS, CentralPod: centralPod, Container: "ovn-ovs"},
		LocalNode:    workerNode,
		LocalChassis: workerChassisUUID,
		Networks: map[string]EVPNNetworkConfig{
			"net-h22": {
				NetworkID:          "net-h22",
				Enabled:            true,
				VNI:                1000,
				RouteDistinguisher: "65034:1000",
				RouteTargets:       []string{"64512:1000"},
				LocalVTEP:          workerVTEP,
			},
		},
	}
	reconciler := NewRouteReconciler()
	waitFor(t, 30*time.Second, 400*time.Millisecond, "ovn endpoint export", func() (bool, error) {
		desired, _, err := reconcileOnce(ctx, observer, reconciler, gobgp, advertised)
		if err != nil {
			return false, err
		}
		for _, ep := range desired {
			if strings.EqualFold(ep.MACAddress, podMAC) && primaryIP(ep.IPAddresses) == podIP {
				return true, nil
			}
		}
		return false, nil
	})
	waitForFRRRoutePresent(t, podMAC, podIP, 25*time.Second)
	waitForFRRVXLANFDBPresent(t, podMAC, workerVTEP, 25*time.Second)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ping -I br1000 -c 1 -W 1 %s >/dev/null 2>&1 || true", frrEndpointIP))
	waitForFRRRoutePresent(t, frrEndpointMAC, frrEndpointIP, 25*time.Second)

	importer := NewRemoteType2Importer(
		&GoBGPServerSnapshotSource{Server: gobgp},
		&Type2RouteValidator{LocalVTEP: localBGPIP,
			Networks: []EVPNImportNetwork{{
				Name:           "ls-h22",
				VNI:            1000,
				RouteTarget:    "64512:1000",
				VXLANDevice:    "vxlan1000",
				BridgeDevice:   "br-evpn1000",
				NeighborDevice: "br-evpn1000",
			}},
		},
		&OVNNetlinkBackend{KubeContext: kctx, Namespace: ovnNS, Pod: workerPod, Container: "ovn-ovs"},
	)
	deadline := time.Now().Add(20 * time.Second)
	var lastReconcileErr error
	for time.Now().Before(deadline) {
		if err := importer.ReconcileOnce(context.Background()); err == nil {
			lastReconcileErr = nil
			break
		} else {
			lastReconcileErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastReconcileErr != nil {
		t.Fatalf("importer reconcile failed: %v", lastReconcileErr)
	}
	waitFor(t, 30*time.Second, 500*time.Millisecond, "FRR route import into OVN", func() (bool, error) {
		fdb := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")
		neigh := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", fmt.Sprintf("ip neigh show to %s dev br-evpn1000", frrEndpointIP))
		return strings.Contains(strings.ToLower(fdb), strings.ToLower(frrEndpointMAC)) && strings.Contains(strings.ToLower(neigh), strings.ToLower(frrEndpointMAC)), nil
	})
	waitFor(t, 20*time.Second, 400*time.Millisecond, "simultaneous bidirectional EVPN state", func() (bool, error) {
		frrTable := frrEVPNTable(t)
		frrFDB := frrVXLANFDB(t)
		ovnFDB := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")
		ovnNeigh := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", fmt.Sprintf("ip neigh show to %s dev br-evpn1000", frrEndpointIP))
		hasPodOnFRR := strings.Contains(strings.ToLower(frrTable), strings.ToLower(podMAC)) &&
			strings.Contains(frrTable, podIP) &&
			strings.Contains(frrTable, workerVTEP)
		hasPodFDBOnFRR := strings.Contains(strings.ToLower(frrFDB), strings.ToLower(podMAC)) &&
			strings.Contains(frrFDB, "dst "+workerVTEP)
		hasFRRRouteOnOVN := strings.Contains(strings.ToLower(ovnFDB), strings.ToLower(frrEndpointMAC)) &&
			strings.Contains(ovnFDB, "dst "+frrVTEPIP) &&
			strings.Contains(strings.ToLower(ovnNeigh), strings.ToLower(frrEndpointMAC))
		return hasPodOnFRR && hasPodFDBOnFRR && hasFRRRouteOnOVN, nil
	})
	t.Logf("[I2-B.3c] FRR endpoint: mac=%s ip=%s, OVN pod: mac=%s ip=%s, worker_vtep=%s, frr_vtep=%s",
		frrEndpointMAC, frrEndpointIP, podMAC, podIP, workerVTEP, frrVTEPIP)
	t.Logf("[I2-B.3c] FRR EVPN table:\n%s", strings.TrimSpace(frrEVPNTable(t)))
	t.Logf("[I2-B.3c] FRR VXLAN FDB:\n%s", strings.TrimSpace(frrVXLANFDB(t)))
	t.Logf("[I2-B.3c] OVN vxlan1000 FDB:\n%s", strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")))
	t.Logf("[I2-B.3c] OVN br-evpn1000 neighbor for FRR endpoint:\n%s", strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", fmt.Sprintf("ip neigh show to %s dev br-evpn1000", frrEndpointIP))))
	t.Logf("[I2-B.3c] FRR EVPN config:\n%s", strings.TrimSpace(mustDockerCommand(t, "docker", "exec", frrContainerName(), "vtysh", "-c", "show running-config")))
	t.Logf("[I2-B.3c] pod route get FRR endpoint:\n%s", strings.TrimSpace(mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc", fmt.Sprintf("ip route get %s", frrEndpointIP))))
	t.Logf("[I2-B.3c] FRR endpoint route get pod:\n%s", strings.TrimSpace(mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", fmt.Sprintf("ip netns exec %s ip route get %s", frrEndpointNS, podIP))))
	t.Logf("[I2-B.3d] OVN EVPN caches before ARP probe:\n[remote-vtep]\n%s\n[vtep-binding]\n%s\n[vtep-fdb]\n%s\n[vtep-arp]\n%s",
		strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovn-appctl -t ovn-controller evpn/remote-vtep-list")),
		strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovn-appctl -t ovn-controller evpn/vtep-binding-list")),
		strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovn-appctl -t ovn-controller evpn/vtep-fdb-list")),
		strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "ovn-appctl -t ovn-controller evpn/vtep-arp-list")))
	flowDumpBeforeARP := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"for t in 46 47 48 87 88; do echo \"=== table $t ===\"; ovs-ofctl dump-flows br-int table=$t; done"))
	_, _ = exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("arping -c 1 -I net1 %s >/tmp/i2b3d-arping.out 2>&1 || true; cat /tmp/i2b3d-arping.out", frrEndpointIP)).CombinedOutput()
	flowDumpAfterARP := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"for t in 46 47 48 87 88; do echo \"=== table $t ===\"; ovs-ofctl dump-flows br-int table=$t; done"))
	t.Logf("[I2-B.3d] br-int selected tables around single pod ARP probe:\n[before]\n%s\n\n[after]\n%s", flowDumpBeforeARP, flowDumpAfterARP)
	evpnTunOfport := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-vsctl --data=bare --no-heading --columns=ofport find Interface name=ovn-evpn-4789 | head -n1"))
	if evpnTunOfport == "" {
		evpnTunOfport = parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
			"ovs-vsctl --data=bare --no-heading --columns=ofport find Interface type=vxlan options:key=flow options:remote_ip=flow options:local_ip=flow options:dst_port=4789 | head -n1"))
	}
	if evpnTunOfport == "" {
		t.Fatal("failed to resolve ovn-evpn tunnel ofport for ofproto/trace")
	}
	traceARPReply := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-appctl ofproto/trace br-int 'in_port=%s,tun_id=0x3e8,tun_src=%s,tun_dst=%s,dl_src=%s,dl_dst=%s,dl_type=0x0806,arp_op=2,arp_spa=%s,arp_tpa=%s,arp_sha=%s,arp_tha=%s'",
			evpnTunOfport, frrVTEPIP, workerVTEP, frrEndpointMAC, podMAC, frrEndpointIP, podIP, frrEndpointMAC, podMAC)))
	traceICMPIn := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-appctl ofproto/trace br-int 'in_port=%s,tun_id=0x3e8,tun_src=%s,tun_dst=%s,dl_src=%s,dl_dst=%s,dl_type=0x0800,nw_src=%s,nw_dst=%s,nw_proto=1,icmp_type=8,icmp_code=0'",
			evpnTunOfport, frrVTEPIP, workerVTEP, frrEndpointMAC, podMAC, frrEndpointIP, podIP)))
	t.Logf("[I2-B.3e] ofproto/trace compare:\n[arp-reply]\n%s\n\n[icmp-inbound]\n%s", traceARPReply, traceICMPIn)
	lspName := fmt.Sprintf("%s_%s_net-h22_net1", testNS, podName)
	podOVSIf := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=name find Interface external_ids:iface-id=%s | head -n1", lspName)))
	if podOVSIf == "" {
		t.Fatalf("failed to resolve pod ovs interface for %s", lspName)
	}

	podCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc", "timeout 14 tcpdump -eni net1 -c 20 arp or icmp")
	ovnPodPortCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 14 tcpdump -eni %s -c 20 arp or icmp", podOVSIf))
	frrEndpointCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", fmt.Sprintf("timeout 14 ip netns exec %s tcpdump -eni %s -c 20 arp or icmp", frrEndpointNS, frrEndpointIf))
	frrBridgeCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", "timeout 14 tcpdump -eni br1000 -c 20 arp or icmp")
	frrUnderlayCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", "timeout 14 tcpdump -ni eth1 -vv -c 20 udp port 4789")
	ovnUnderlayCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc", "timeout 14 tcpdump -ni eth0 -vv -c 20 udp port 4789")
	time.Sleep(1200 * time.Millisecond)

	frrICMPOut, frrICMPErr := exec.Command("docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip netns exec %s ping -c 3 -W 1 %s", frrEndpointNS, podIP)).CombinedOutput()
	podICMPOut, podICMPErr := exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc", fmt.Sprintf("ping -c 3 -W 1 -I net1 %s", frrEndpointIP)).CombinedOutput()
	podCapOut, _ := podCap.wait()
	ovnPodPortCapOut, _ := ovnPodPortCap.wait()
	frrEndpointCapOut, _ := frrEndpointCap.wait()
	frrBridgeCapOut, _ := frrBridgeCap.wait()
	frrUnderlayOut, _ := frrUnderlayCap.wait()
	ovnUnderlayOut, _ := ovnUnderlayCap.wait()
	t.Logf("[I2-B.3b] pod->FRR ICMP:\n%s\n\n[FRR->pod ICMP]\n%s", strings.TrimSpace(string(podICMPOut)), strings.TrimSpace(string(frrICMPOut)))
	t.Logf("[I2-B.3c] packet evidence:\n[pod-net1]\n%s\n\n[ovn-pod-port %s]\n%s\n\n[frr-endpoint]\n%s\n\n[frr-br1000]\n%s\n\n[frr-underlay]\n%s\n\n[ovn-underlay]\n%s",
		strings.TrimSpace(podCapOut), podOVSIf, strings.TrimSpace(ovnPodPortCapOut), strings.TrimSpace(frrEndpointCapOut), strings.TrimSpace(frrBridgeCapOut), strings.TrimSpace(frrUnderlayOut), strings.TrimSpace(ovnUnderlayOut))
	if podICMPErr != nil {
		t.Fatalf("pod -> FRR ICMP failed: %v\n%s\n[pod-net1]\n%s\n[ovn-pod-port %s]\n%s\n[frr-endpoint]\n%s\n[frr-br1000]\n%s\n[frr-underlay]\n%s\n[ovn-underlay]\n%s",
			podICMPErr, string(podICMPOut), podCapOut, podOVSIf, ovnPodPortCapOut, frrEndpointCapOut, frrBridgeCapOut, frrUnderlayOut, ovnUnderlayOut)
	}
	if frrICMPErr != nil {
		t.Fatalf("FRR -> pod ICMP failed: %v\n%s\n[pod-net1]\n%s\n[frr-endpoint]\n%s\n[frr-underlay]\n%s\n[ovn-underlay]\n%s",
			frrICMPErr, string(frrICMPOut), podCapOut, frrEndpointCapOut, frrUnderlayOut, ovnUnderlayOut)
	}
	if !strings.Contains(string(podICMPOut), "3 packets transmitted") || !strings.Contains(string(podICMPOut), "3 received") {
		t.Fatalf("pod -> FRR ICMP did not complete successfully:\n%s", string(podICMPOut))
	}
	if !strings.Contains(string(frrICMPOut), "3 packets transmitted") || !strings.Contains(string(frrICMPOut), "3 received") {
		t.Fatalf("FRR -> pod ICMP did not complete successfully:\n%s", string(frrICMPOut))
	}
	if !strings.Contains(strings.ToLower(podCapOut), "echo request") || !strings.Contains(strings.ToLower(podCapOut), "echo reply") {
		t.Fatalf("pod capture missing ICMP request/reply evidence:\n%s", podCapOut)
	}
	if !strings.Contains(strings.ToLower(frrEndpointCapOut), "echo request") || !strings.Contains(strings.ToLower(frrEndpointCapOut), "echo reply") {
		t.Fatalf("FRR endpoint capture missing ICMP request/reply evidence:\n%s", frrEndpointCapOut)
	}
	if !strings.Contains(frrUnderlayOut, "VXLAN") || !strings.Contains(frrUnderlayOut, "vni 1000") {
		t.Fatalf("FRR underlay capture missing VXLAN VNI 1000 evidence:\n%s", frrUnderlayOut)
	}
	if !strings.Contains(ovnUnderlayOut, "VXLAN") || !strings.Contains(ovnUnderlayOut, "vni 1000") {
		t.Fatalf("OVN underlay capture missing VXLAN VNI 1000 evidence:\n%s", ovnUnderlayOut)
	}
}

type capturedCmd struct {
	cmd *exec.Cmd
	out bytes.Buffer
}

func startCapturedCommand(t *testing.T, name string, args ...string) *capturedCmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, name, args...)
	c := &capturedCmd{cmd: cmd}
	cmd.Stdout = &c.out
	cmd.Stderr = &c.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command %s %s failed: %v", name, strings.Join(args, " "), err)
	}
	return c
}

func (c *capturedCmd) wait() (string, error) {
	err := c.cmd.Wait()
	return c.out.String(), err
}

func brIntPortTxPackets(t *testing.T, ctx context.Context, kctx, ovnNS, workerPod, ofport string) int {
	t.Helper()
	out := mustKubectl(t, ctx, kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("ovs-ofctl dump-ports br-int %s", ofport))
	re := regexp.MustCompile(`tx pkts=(\d+)`)
	m := re.FindStringSubmatch(out)
	if len(m) != 2 {
		t.Fatalf("failed to parse tx packets for ofport %s from output:\n%s", ofport, out)
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("invalid tx packet counter %q for ofport %s: %v", m[1], ofport, err)
	}
	return v
}
