package evpn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
)

func TestI2C0OVN2609NativeEVPNARPSuppression(t *testing.T) {
	if os.Getenv("I2C0_E2E") != "1" {
		t.Skip("set I2C0_E2E=1 to run I2-C.0 OVN 26.09 EVPN ARP suppression validation")
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
	workerNode := strings.TrimSpace(os.Getenv("I2C0_WORKER_NODE"))
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

	runtimeVersions := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovn-controller --version | head -n2; ovs-vswitchd --version | head -n1; uname -r; dpkg-query -W -f='${Package} ${Version}\\n' ovn-central ovn-host openvswitch-switch openvswitch-common | sort"))
	t.Logf("[I2-C.0] runtime versions:\n%s", runtimeVersions)
	allowNon2609 := strings.TrimSpace(os.Getenv("I2C0_ALLOW_NON2609")) == "1"
	if !allowNon2609 && !strings.Contains(runtimeVersions, "ovn-controller 26.09.0") {
		t.Fatalf("I2-C.0 requires OVN 26.09.0 runtime, got:\n%s", runtimeVersions)
	}

	const (
		frrEndpointNS     = "i2c0ns"
		frrEndpointHostIf = "frr-i2c0-h"
		frrEndpointIf     = "frr-i2c0-e"
		frrEndpointMAC    = "02:42:ac:19:00:22"
		frrEndpointIP     = "10.251.1.22"
	)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip netns del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true",
			frrEndpointNS, frrEndpointHostIf, frrEndpointIf))
	createEndpoint := func() {
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
	}
	deleteEndpoint := func() {
		mustDockerCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
			fmt.Sprintf("ip netns del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true; ip link del %s >/dev/null 2>&1 || true",
				frrEndpointNS, frrEndpointHostIf, frrEndpointIf))
	}
	createEndpoint()
	defer deleteEndpoint()

	asn := uint32(65036)
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

	testNS := "i2c0-evpn"
	nadName := "evpn-i2c0"
	podName := "h22-i2c0"
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
		t.Fatalf("apply i2c0 pod/NAD manifest: %v", err)
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
				RouteDistinguisher: "65036:1000",
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
	waitFor(t, 20*time.Second, 300*time.Millisecond, "importer sync", func() (bool, error) {
		if err := importer.ReconcileOnce(context.Background()); err != nil {
			return false, nil
		}
		fdb := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000")
		neigh := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", fmt.Sprintf("ip neigh show to %s dev br-evpn1000", frrEndpointIP))
		return strings.Contains(strings.ToLower(fdb), strings.ToLower(frrEndpointMAC)) && strings.Contains(strings.ToLower(neigh), strings.ToLower(frrEndpointMAC)), nil
	})

	lflows := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		"ovn-sbctl lflow-list ls-h22 | grep -E 'chk_evpn_arp|ls_in_pre_arp_nd_rsp' || true"))
	t.Logf("[I2-C.0] OVN logical flow evidence:\n%s", lflows)
	if !strings.Contains(lflows, "chk_evpn_arp") {
		t.Fatalf("missing chk_evpn_arp logical flow evidence on ls-h22\n%s", lflows)
	}

	podCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc", "timeout 8 tcpdump -eni net1 -c 8 arp")
	frrEndpointCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", fmt.Sprintf("timeout 8 ip netns exec %s tcpdump -eni %s -c 8 arp", frrEndpointNS, frrEndpointIf))
	frrUnderlayCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc", "timeout 8 tcpdump -ni eth1 -vv -c 12 udp port 4789")
	ovnUnderlayCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc", "timeout 8 tcpdump -ni eth0 -vv -c 12 udp port 4789")
	time.Sleep(1200 * time.Millisecond)
	arpProbeOut, _ := exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("arping -c 1 -w 2 -I net1 %s", frrEndpointIP)).CombinedOutput()
	podCapOut, _ := podCap.wait()
	frrEndpointCapOut, _ := frrEndpointCap.wait()
	frrUnderlayOut, _ := frrUnderlayCap.wait()
	ovnUnderlayOut, _ := ovnUnderlayCap.wait()
	t.Logf("[I2-C.0] ARP probe from pod:\n%s\n[pod-net1]\n%s\n[frr-endpoint]\n%s\n[frr-underlay]\n%s\n[ovn-underlay]\n%s",
		strings.TrimSpace(string(arpProbeOut)), strings.TrimSpace(podCapOut), strings.TrimSpace(frrEndpointCapOut), strings.TrimSpace(frrUnderlayOut), strings.TrimSpace(ovnUnderlayOut))

	skipARPAsserts := strings.TrimSpace(os.Getenv("I2C0_SKIP_ARP_ASSERTS")) == "1"
	if !skipARPAsserts {
		if !strings.Contains(strings.ToLower(podCapOut), "reply 10.251.1.22 is-at") {
			t.Fatalf("pod capture missing local ARP reply for remote EVPN endpoint\n%s", podCapOut)
		}
		if strings.Contains(strings.ToLower(frrEndpointCapOut), "who-has 10.251.1.22 tell 10.251.1.11") {
			t.Fatalf("FRR endpoint saw broadcast pod ARP request; suppression did not hold\n%s", frrEndpointCapOut)
		}
		if strings.Contains(strings.ToLower(frrUnderlayOut), "who-has 10.251.1.22 tell 10.251.1.11") || strings.Contains(strings.ToLower(ovnUnderlayOut), "who-has 10.251.1.22 tell 10.251.1.11") {
			t.Fatalf("underlay carried suppressed pod ARP broadcast request\n[frr-underlay]\n%s\n[ovn-underlay]\n%s", frrUnderlayOut, ovnUnderlayOut)
		}
	}

	podPingOut, podPingErr := exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("ping -c 3 -W 1 -I net1 %s", frrEndpointIP)).CombinedOutput()
	frrPingOut, frrPingErr := exec.Command("docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip netns exec %s ping -c 3 -W 1 %s", frrEndpointNS, podIP)).CombinedOutput()
	t.Logf("[I2-C.0] ICMP:\n[pod->FRR]\n%s\n[FRR->pod]\n%s", strings.TrimSpace(string(podPingOut)), strings.TrimSpace(string(frrPingOut)))
	if podPingErr != nil || !strings.Contains(string(podPingOut), "3 received") {
		diag := i2c2SinglePacketLocalization(t, ctx, kctx, ovnNS, testNS, podName, workerPod, frrEndpointNS, frrEndpointIf, podIP, podMAC, frrEndpointIP, frrEndpointMAC, workerVTEP, frrVTEPIP)
		matrix := i2c5ABMatrixInvestigation(t, ctx, kctx, ovnNS, testNS, podName, workerPod, frrEndpointNS, frrEndpointIf, podIP, podMAC, frrEndpointIP, frrEndpointMAC, workerVTEP, frrVTEPIP)
		t.Fatalf("pod -> FRR ICMP failed\n%s\n\n[I2-C.2 single-packet localization]\n%s\n\n[I2-C.5 controlled A/B matrix]\n%s", podPingOut, diag, matrix)
	}
	if frrPingErr != nil || !strings.Contains(string(frrPingOut), "3 received") {
		t.Fatalf("FRR -> pod ICMP failed\n%s", frrPingOut)
	}

	deleteEndpoint()
	waitForFRRRouteAbsent(t, frrEndpointMAC, frrEndpointIP, 30*time.Second)
	waitFor(t, 30*time.Second, 400*time.Millisecond, "importer withdraw cleanup", func() (bool, error) {
		_ = importer.ReconcileOnce(context.Background())
		fdb := strings.ToLower(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", "bridge fdb show dev vxlan1000"))
		neigh := strings.ToLower(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs", fmt.Sprintf("ip neigh show to %s dev br-evpn1000", frrEndpointIP)))
		return !strings.Contains(fdb, strings.ToLower(frrEndpointMAC)) && strings.TrimSpace(neigh) == "", nil
	})

	postWithdrawPodCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc", "timeout 6 tcpdump -eni net1 -c 6 arp")
	time.Sleep(900 * time.Millisecond)
	postWithdrawARPOut, _ := exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("arping -c 1 -w 2 -I net1 %s || true", frrEndpointIP)).CombinedOutput()
	postWithdrawPodCapOut, _ := postWithdrawPodCap.wait()
	t.Logf("[I2-C.0] post-withdraw ARP probe:\n%s\n[pod-net1]\n%s", strings.TrimSpace(string(postWithdrawARPOut)), strings.TrimSpace(postWithdrawPodCapOut))
	if strings.Contains(strings.ToLower(string(postWithdrawARPOut)), "reply from") || strings.Contains(strings.ToLower(postWithdrawPodCapOut), "reply 10.251.1.22 is-at") {
		t.Fatalf("stale ARP suppression/proxy reply remained after Type-2 withdrawal\n%s\n%s", postWithdrawARPOut, postWithdrawPodCapOut)
	}
}

func i2c2SinglePacketLocalization(t *testing.T, ctx context.Context, kctx, ovnNS, testNS, podName, workerPod, frrEndpointNS, frrEndpointIf, podIP, podMAC, frrIP, frrMAC, workerVTEP, frrVTEP string) string {
	t.Helper()

	lspName := fmt.Sprintf("%s_%s_net-h22_net1", testNS, podName)
	podOVSIf := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=name find Interface external_ids:iface-id=%s | head -n1", lspName)))
	podOFPort := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=ofport find Interface external_ids:iface-id=%s | head -n1", lspName)))
	evpnTunOfport := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-vsctl --data=bare --no-heading --columns=ofport find Interface name=ovn-evpn-4789 | head -n1"))
	if evpnTunOfport == "" {
		evpnTunOfport = parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
			"ovs-vsctl --data=bare --no-heading --columns=ofport find Interface type=vxlan options:key=flow options:remote_ip=flow options:local_ip=flow options:dst_port=4789 | head -n1"))
	}
	tunnelIfaceDump := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-vsctl --columns=name,ofport,type,options,statistics list Interface ovn-evpn-4789"))
	portStatsBefore := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[tunnel]'; ovs-ofctl dump-ports br-int %s; echo '[pod]'; ovs-ofctl dump-ports br-int %s", evpnTunOfport, podOFPort)))
	flow112Before := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-ofctl dump-flows br-int table=112 | grep -i 'dl_src=02:42:ac:19:00:22' || true"))
	ctFlowsBefore := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-ofctl dump-flows br-int | grep -Ei 'ct\\(|ct_state|ct_zone' | head -n 120 || true"))
	dpFlowsBefore := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-flows -m | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 120 || true"))
	ctStateBefore := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-conntrack 2>/dev/null | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 80 || true"))
	ovsLogTail := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"grep -Ei 'vxlan|tunnel|drop|error|warn' /var/log/openvswitch/ovs-vswitchd.log | tail -n 40 || true"))

	filter := fmt.Sprintf("icmp and ((src host %s and dst host %s) or (src host %s and dst host %s))", podIP, frrIP, frrIP, podIP)
	podCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -eni net1 -vv -c 4 '%s'", filter))
	ovnPodPortCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -eni %s -vv -c 4 '%s'", podOVSIf, filter))
	frrEndpointCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 10 ip netns exec %s tcpdump -eni %s -vv -c 4 '%s'", frrEndpointNS, frrEndpointIf, filter))
	frrBridgeCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -eni br1000 -vv -c 4 '%s'", filter))
	frrUnderlayCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -ni eth1 -vv -c 6 'udp dst port 4789 and host %s'", workerVTEP))
	ovnUnderlayCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 10 tcpdump -ni eth0 -vv -c 6 'udp dst port 4789 and host %s'", frrVTEP))
	time.Sleep(1200 * time.Millisecond)

	pingOut, pingErr := exec.Command("kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("ping -c 1 -W 1 -I net1 %s", frrIP)).CombinedOutput()

	podCapOut, _ := podCap.wait()
	ovnPodPortCapOut, _ := ovnPodPortCap.wait()
	frrEndpointCapOut, _ := frrEndpointCap.wait()
	frrBridgeCapOut, _ := frrBridgeCap.wait()
	frrUnderlayOut, _ := frrUnderlayCap.wait()
	ovnUnderlayOut, _ := ovnUnderlayCap.wait()
	portStatsAfterB := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[tunnel]'; ovs-ofctl dump-ports br-int %s; echo '[pod]'; ovs-ofctl dump-ports br-int %s", evpnTunOfport, podOFPort)))
	flow112AfterB := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-ofctl dump-flows br-int table=112 | grep -i 'dl_src=02:42:ac:19:00:22' || true"))
	dpFlowsAfterB := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-flows -m | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 120 || true"))
	ctStateAfterB := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-conntrack 2>/dev/null | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 80 || true"))

	reqID, reqSeq := parseICMPSignature(podCapOut, "request")
	if reqID == "" {
		reqID, reqSeq = parseICMPSignature(ovnPodPortCapOut, "request")
	}
	if reqID == "" {
		reqID, reqSeq = parseICMPSignature(ovnUnderlayOut, "request")
	}

	reqToken := ""
	if reqID != "" {
		reqToken = fmt.Sprintf("id %s, seq %s", reqID, reqSeq)
	}
	hasReq := func(s string) bool {
		return strings.Contains(s, "ICMP echo request") && (reqToken == "" || strings.Contains(s, reqToken))
	}
	hasReply := func(s string) bool {
		return strings.Contains(s, "ICMP echo reply") && (reqToken == "" || strings.Contains(s, reqToken))
	}

	reqStages := []struct {
		name string
		hit  bool
	}{
		{name: "pod net1", hit: hasReq(podCapOut)},
		{name: "ovn pod-facing interface", hit: hasReq(ovnPodPortCapOut)},
		{name: "ovn underlay vxlan", hit: hasReq(ovnUnderlayOut)},
		{name: "frr underlay vxlan", hit: hasReq(frrUnderlayOut)},
		{name: "frr br1000", hit: hasReq(frrBridgeCapOut)},
		{name: "frr endpoint veth", hit: hasReq(frrEndpointCapOut)},
	}
	replyStages := []struct {
		name string
		hit  bool
	}{
		{name: "frr endpoint veth", hit: hasReply(frrEndpointCapOut)},
		{name: "frr br1000", hit: hasReply(frrBridgeCapOut)},
		{name: "frr underlay vxlan", hit: hasReply(frrUnderlayOut)},
		{name: "ovn underlay vxlan", hit: hasReply(ovnUnderlayOut)},
		{name: "ovn pod-facing interface", hit: hasReply(ovnPodPortCapOut)},
		{name: "pod net1", hit: hasReply(podCapOut)},
	}

	firstFailure := "not-determined"
	for i, st := range reqStages {
		if !st.hit {
			if i == 0 {
				firstFailure = "request not emitted on pod net1"
			} else {
				firstFailure = fmt.Sprintf("request lost before %s", st.name)
			}
			goto done
		}
	}
	for i, st := range replyStages {
		if !st.hit {
			if i == 0 {
				firstFailure = "request reached FRR endpoint but reply not generated there"
			} else {
				firstFailure = fmt.Sprintf("reply lost before %s", st.name)
			}
			goto done
		}
	}
	firstFailure = "no drop in single-packet trace"

done:
	// Compare against known-working FRR->pod ingress request path.
	workPodCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
		fmt.Sprintf("timeout 8 tcpdump -eni net1 -vv -c 2 'icmp and src host %s and dst host %s and icmp[icmptype]=8'", frrIP, podIP))
	workOvnPodCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 8 tcpdump -eni %s -vv -c 2 'icmp and src host %s and dst host %s and icmp[icmptype]=8'", podOVSIf, frrIP, podIP))
	workOvnUnderlay := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
		fmt.Sprintf("timeout 8 tcpdump -ni eth0 -vv -c 2 'udp dst port 4789 and host %s'", frrVTEP))
	workFrrUnderlay := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("timeout 8 tcpdump -ni eth1 -vv -c 2 'udp dst port 4789 and host %s'", workerVTEP))
	time.Sleep(1000 * time.Millisecond)
	workPingOut, workPingErr := exec.Command("docker", "exec", frrContainerName(), "sh", "-lc",
		fmt.Sprintf("ip netns exec %s ping -c 1 -W 1 %s", frrEndpointNS, podIP)).CombinedOutput()
	workPodCapOut, _ := workPodCap.wait()
	workOvnPodCapOut, _ := workOvnPodCap.wait()
	workOvnUnderlayOut, _ := workOvnUnderlay.wait()
	workFrrUnderlayOut, _ := workFrrUnderlay.wait()
	portStatsAfterA := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[tunnel]'; ovs-ofctl dump-ports br-int %s; echo '[pod]'; ovs-ofctl dump-ports br-int %s", evpnTunOfport, podOFPort)))
	flow112AfterA := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-ofctl dump-flows br-int table=112 | grep -i 'dl_src=02:42:ac:19:00:22' || true"))
	dpFlowsAfterA := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-flows -m | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 120 || true"))
	ctStateAfterA := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-appctl dpctl/dump-conntrack 2>/dev/null | grep -E '10\\.251\\.1\\.(11|22)|icmp' | head -n 80 || true"))

	traceReq := ""
	traceReply := ""
	traceReqIn := ""
	if podOFPort != "" {
		traceReq = strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
			fmt.Sprintf("ovs-appctl ofproto/trace br-int 'in_port=%s,dl_src=%s,dl_dst=%s,dl_type=0x0800,nw_src=%s,nw_dst=%s,nw_proto=1,icmp_type=8,icmp_code=0'",
				podOFPort, podMAC, frrMAC, podIP, frrIP)))
	}
	if evpnTunOfport != "" {
		traceReqIn = strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
			fmt.Sprintf("ovs-appctl ofproto/trace br-int 'in_port=%s,tun_id=0x3e8,tun_src=%s,tun_dst=%s,dl_src=%s,dl_dst=%s,dl_type=0x0800,nw_src=%s,nw_dst=%s,nw_proto=1,icmp_type=8,icmp_code=0'",
				evpnTunOfport, frrVTEP, workerVTEP, frrMAC, podMAC, frrIP, podIP)))
		traceReply = strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
			fmt.Sprintf("ovs-appctl ofproto/trace br-int 'in_port=%s,tun_id=0x3e8,tun_src=%s,tun_dst=%s,dl_src=%s,dl_dst=%s,dl_type=0x0800,nw_src=%s,nw_dst=%s,nw_proto=1,icmp_type=0,icmp_code=0'",
				evpnTunOfport, frrVTEP, workerVTEP, frrMAC, podMAC, frrIP, podIP)))
	}

	return fmt.Sprintf(
		"pod_ovs_if=%s pod_ofport=%s evpn_tunnel_ofport=%s\nsingle_ping_err=%v\nsingle_ping_output:\n%s\npacket_signature=%s\nfirst_verified_failure=%s\n[tunnel-interface]\n%s\n[ovs-port-stats before]\n%s\n[ovs-port-stats after failing-reply probe]\n%s\n[ovs-port-stats after working-frro->pod probe]\n%s\n[table112 before]\n%s\n[table112 after failing-reply probe]\n%s\n[table112 after working probe]\n%s\n[ct-related-openflow before]\n%s\n[dpctl flows before]\n%s\n[conntrack before]\n%s\n[dpctl flows after failing-reply probe]\n%s\n[conntrack after failing-reply probe]\n%s\n[dpctl flows after working probe]\n%s\n[conntrack after working probe]\n%s\n[ovs-vswitchd-log-tail]\n%s\n[working FRR->pod ping]\nerr=%v\n%s\n[br-int-trace pod->frr]\n%s\n[br-int-trace frr->pod request]\n%s\n[br-int-trace frr->pod reply]\n%s\n[pod-net1]\n%s\n[ovn-pod-port]\n%s\n[ovn-underlay]\n%s\n[frr-underlay]\n%s\n[frr-br1000]\n%s\n[frr-endpoint]\n%s\n[working pod-net1]\n%s\n[working ovn-pod-port]\n%s\n[working ovn-underlay]\n%s\n[working frr-underlay]\n%s",
		podOVSIf, podOFPort, evpnTunOfport, pingErr, strings.TrimSpace(string(pingOut)), reqToken, firstFailure,
		tunnelIfaceDump, portStatsBefore, portStatsAfterB, portStatsAfterA, flow112Before, flow112AfterB, flow112AfterA,
		ctFlowsBefore, dpFlowsBefore, ctStateBefore, dpFlowsAfterB, ctStateAfterB, dpFlowsAfterA, ctStateAfterA, ovsLogTail,
		workPingErr, strings.TrimSpace(string(workPingOut)),
		traceReq, traceReqIn, traceReply,
		strings.TrimSpace(podCapOut), strings.TrimSpace(ovnPodPortCapOut), strings.TrimSpace(ovnUnderlayOut),
		strings.TrimSpace(frrUnderlayOut), strings.TrimSpace(frrBridgeCapOut), strings.TrimSpace(frrEndpointCapOut),
		strings.TrimSpace(workPodCapOut), strings.TrimSpace(workOvnPodCapOut), strings.TrimSpace(workOvnUnderlayOut), strings.TrimSpace(workFrrUnderlayOut),
	)
}

func parseICMPSignature(capture, kind string) (string, string) {
	re := regexp.MustCompile(fmt.Sprintf(`ICMP echo %s, id ([0-9]+), seq ([0-9]+)`, kind))
	m := re.FindStringSubmatch(capture)
	if len(m) != 3 {
		return "", ""
	}
	return m[1], m[2]
}

func i2c5ABMatrixInvestigation(t *testing.T, ctx context.Context, kctx, ovnNS, testNS, podName, workerPod, frrEndpointNS, frrEndpointIf, podIP, podMAC, frrIP, frrMAC, workerVTEP, frrVTEP string) string {
	t.Helper()

	lspName := fmt.Sprintf("%s_%s_net-h22_net1", testNS, podName)
	podOVSIf := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=name find Interface external_ids:iface-id=%s | head -n1", lspName)))
	podOFPort := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("ovs-vsctl --data=bare --no-heading --columns=ofport find Interface external_ids:iface-id=%s | head -n1", lspName)))
	evpnTunOfport := parseSingleLine(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		"ovs-vsctl --data=bare --no-heading --columns=ofport find Interface name=ovn-evpn-4789 | head -n1"))

	baseline := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[vtep-fdb]'; ovn-appctl -t ovn-controller evpn/vtep-fdb-list; echo '[vtep-arp]'; ovn-appctl -t ovn-controller evpn/vtep-arp-list; echo '[fdb-vxlan1000]'; bridge fdb show dev vxlan1000 | grep -Ei '%s|%s' || true; echo '[neigh-br-evpn1000]'; ip neigh show to %s dev br-evpn1000 || true",
			strings.ReplaceAll(strings.ToLower(podMAC), ":", "\\:"), strings.ReplaceAll(strings.ToLower(frrMAC), ":", "\\:"), frrIP)))

	type expResult struct {
		name             string
		triggerOut       string
		tunBefore        string
		podBefore        string
		tunAfter         string
		podAfter         string
		podCap           string
		ovnPortCap       string
		ovnUnderlayCap   string
		frrUnderlayCap   string
		id               string
		seq              string
		ingressReplySeen bool
		podReplySeen     bool
	}

	runExp := func(name, triggerCmd string) expResult {
		tunBefore, podBefore := i2c5PortCounters(t, ctx, kctx, ovnNS, workerPod, evpnTunOfport, podOFPort)
		podCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", testNS, "exec", podName, "-c", "tools", "--", "sh", "-lc",
			fmt.Sprintf("timeout 8 tcpdump -eni net1 -vv -c 6 'icmp and ((src host %s and dst host %s) or (src host %s and dst host %s))'", podIP, frrIP, frrIP, podIP))
		ovnPortCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
			fmt.Sprintf("timeout 8 tcpdump -eni %s -vv -c 6 'icmp and ((src host %s and dst host %s) or (src host %s and dst host %s))'", podOVSIf, podIP, frrIP, frrIP, podIP))
		ovnUnderlayCap := startCapturedCommand(t, "kubectl", "--context", kctx, "-n", ovnNS, "exec", workerPod, "-c", "ovn-ovs", "--", "sh", "-lc",
			fmt.Sprintf("timeout 8 tcpdump -ni eth0 -vv -c 6 'udp dst port 4789 and host %s'", frrVTEP))
		frrUnderlayCap := startCapturedCommand(t, "docker", "exec", frrContainerName(), "sh", "-lc",
			fmt.Sprintf("timeout 8 tcpdump -ni eth1 -vv -c 6 'udp dst port 4789 and host %s'", workerVTEP))
		time.Sleep(1000 * time.Millisecond)

		triggerOut, _ := exec.Command("sh", "-lc", triggerCmd).CombinedOutput()
		podCapOut, _ := podCap.wait()
		ovnPortCapOut, _ := ovnPortCap.wait()
		ovnUnderlayOut, _ := ovnUnderlayCap.wait()
		frrUnderlayOut, _ := frrUnderlayCap.wait()
		tunAfter, podAfter := i2c5PortCounters(t, ctx, kctx, ovnNS, workerPod, evpnTunOfport, podOFPort)

		id, seq := parseICMPSignature(podCapOut, "request")
		if id == "" {
			id, seq = parseICMPSignature(ovnUnderlayOut, "request")
		}
		if id == "" {
			id, seq = parseICMPSignature(ovnUnderlayOut, "reply")
		}
		token := ""
		if id != "" {
			token = fmt.Sprintf("id %s, seq %s", id, seq)
		}
		replyOnUnderlay := strings.Contains(ovnUnderlayOut, "ICMP echo reply") && (token == "" || strings.Contains(ovnUnderlayOut, token))
		replyOnPod := strings.Contains(podCapOut, "ICMP echo reply") && (token == "" || strings.Contains(podCapOut, token))

		return expResult{
			name:             name,
			triggerOut:       strings.TrimSpace(string(triggerOut)),
			tunBefore:        tunBefore,
			podBefore:        podBefore,
			tunAfter:         tunAfter,
			podAfter:         podAfter,
			podCap:           i2c5ShortICMPLines(podCapOut),
			ovnPortCap:       i2c5ShortICMPLines(ovnPortCapOut),
			ovnUnderlayCap:   i2c5ShortICMPLines(ovnUnderlayOut),
			frrUnderlayCap:   i2c5ShortICMPLines(frrUnderlayOut),
			id:               id,
			seq:              seq,
			ingressReplySeen: replyOnUnderlay,
			podReplySeen:     replyOnPod,
		}
	}

	expA := runExp("A: FRR -> Pod echo request", fmt.Sprintf("docker exec %s sh -lc 'ip netns exec %s ping -c1 -W1 %s'", frrContainerName(), frrEndpointNS, podIP))
	expB := runExp("B: Pod -> FRR request + reply", fmt.Sprintf("kubectl --context %s -n %s exec %s -c tools -- sh -lc 'ping -c1 -W1 -I net1 %s || true'", kctx, testNS, podName, frrIP))
	expC := runExp("C: FRR crafted echo-reply (no prior request)", fmt.Sprintf("docker exec %s sh -lc 'ip netns exec %s python3 - <<\"PY\"\nimport socket, struct\niface=\"%s\"\nsrc_mac=bytes.fromhex(\"%s\")\ndst_mac=bytes.fromhex(\"%s\")\nsrc_ip=\"%s\"\ndst_ip=\"%s\"\nident=0x55aa\nseq=1\n\ndef csum(data):\n    if len(data)%%2: data += b\"\\x00\"\n    s=0\n    for i in range(0,len(data),2):\n        s += (data[i] << 8) + data[i+1]\n    while s >> 16:\n        s = (s & 0xffff) + (s >> 16)\n    return (~s) & 0xffff\n\npayload=b\"I2C5C\"\nicmp=struct.pack(\"!BBHHH\",0,0,0,ident,seq)+payload\nicmp=struct.pack(\"!BBHHH\",0,0,csum(icmp),ident,seq)+payload\nver_ihl=(4<<4)|5\ntos=0\ntot=20+len(icmp)\nipid=0x3333\nflags_off=0\nttl=64\nproto=1\niph=struct.pack(\"!BBHHHBBH4s4s\",ver_ihl,tos,tot,ipid,flags_off,ttl,proto,0,socket.inet_aton(src_ip),socket.inet_aton(dst_ip))\niph=struct.pack(\"!BBHHHBBH4s4s\",ver_ihl,tos,tot,ipid,flags_off,ttl,proto,csum(iph),socket.inet_aton(src_ip),socket.inet_aton(dst_ip))\nframe=dst_mac+src_mac+b\"\\x08\\x00\"+iph+icmp\ns=socket.socket(socket.AF_PACKET,socket.SOCK_RAW)\ns.bind((iface,0))\ns.send(frame)\nPY'", frrContainerName(), frrEndpointNS, frrEndpointIf, strings.ReplaceAll(strings.ToLower(frrMAC), ":", ""), strings.ReplaceAll(strings.ToLower(podMAC), ":", ""), frrIP, podIP))
	expD := runExp("D: FRR -> Pod request immediately after B", fmt.Sprintf("docker exec %s sh -lc 'ip netns exec %s ping -c1 -W1 %s'", frrContainerName(), frrEndpointNS, podIP))

	afterState := strings.TrimSpace(mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[vtep-fdb]'; ovn-appctl -t ovn-controller evpn/vtep-fdb-list; echo '[vtep-arp]'; ovn-appctl -t ovn-controller evpn/vtep-arp-list; echo '[fdb-vxlan1000]'; bridge fdb show dev vxlan1000 | grep -Ei '%s|%s' || true; echo '[neigh-br-evpn1000]'; ip neigh show to %s dev br-evpn1000 || true; echo '[table112]'; ovs-ofctl dump-flows br-int table=112 | grep -i 'dl_src=02:42:ac:19:00:22' || true",
			strings.ReplaceAll(strings.ToLower(podMAC), ":", "\\:"), strings.ReplaceAll(strings.ToLower(frrMAC), ":", "\\:"), frrIP)))

	analysis := "inconclusive"
	if expA.podReplySeen && !expB.podReplySeen && expB.ingressReplySeen {
		analysis = "delivery differs by traffic initiation/state transition after pod-originated request, not by ICMP type alone"
	}
	if expC.ingressReplySeen && !expC.podReplySeen {
		analysis += "; crafted echo-reply alone also fails to reach pod after OVN tunnel ingress"
	}

	formatExp := func(e expResult) string {
		return fmt.Sprintf("[%s]\nsignature=id %s seq %s\ntrigger:\n%s\nport-counters: tunnel %s -> %s, pod %s -> %s\nreply_seen: underlay=%t pod=%t\n[pod-net1]\n%s\n[ovn-pod-port]\n%s\n[ovn-underlay]\n%s\n[frr-underlay]\n%s",
			e.name, e.id, e.seq, e.triggerOut, e.tunBefore, e.tunAfter, e.podBefore, e.podAfter, e.ingressReplySeen, e.podReplySeen,
			e.podCap, e.ovnPortCap, e.ovnUnderlayCap, e.frrUnderlayCap)
	}

	return fmt.Sprintf("pod_ovs_if=%s pod_ofport=%s tunnel_ofport=%s\n[baseline-state]\n%s\n%s\n%s\n%s\n%s\n[state-after-matrix]\n%s\n[hypothesis]\n%s",
		podOVSIf, podOFPort, evpnTunOfport, baseline,
		formatExp(expA), formatExp(expB), formatExp(expC), formatExp(expD),
		afterState, analysis)
}

func i2c5PortCounters(t *testing.T, ctx context.Context, kctx, ovnNS, workerPod, tunOfport, podOfport string) (string, string) {
	t.Helper()
	out := mustOVNExec(t, ctx, kctx, ovnNS, workerPod, "ovn-ovs",
		fmt.Sprintf("echo '[tun]'; ovs-ofctl dump-ports br-int %s; echo '[pod]'; ovs-ofctl dump-ports br-int %s", tunOfport, podOfport))
	re := regexp.MustCompile(`port\s+([0-9]+): rx pkts=([0-9]+).*\n\s*tx pkts=([0-9]+)`)
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) < 2 {
		return "?", "?"
	}
	return fmt.Sprintf("rx=%s tx=%s", matches[0][2], matches[0][3]), fmt.Sprintf("rx=%s tx=%s", matches[1][2], matches[1][3])
}

func i2c5ShortICMPLines(out string) string {
	lines := strings.Split(out, "\n")
	keep := make([]string, 0, len(lines))
	for _, l := range lines {
		ls := strings.TrimSpace(l)
		if strings.Contains(ls, "ICMP echo") || strings.Contains(ls, "VXLAN") || strings.Contains(ls, "vni 1000") || strings.Contains(ls, "packets captured") {
			keep = append(keep, ls)
		}
	}
	return strings.Join(keep, "\n")
}
