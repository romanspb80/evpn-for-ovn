package evpn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runCmdInput(ctx context.Context, input string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func mustKubectl(t *testing.T, ctx context.Context, kctx string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"--context", kctx}, args...)
	out, err := runCmd(ctx, "kubectl", fullArgs...)
	if err != nil {
		t.Fatalf("kubectl %v: %v", args, err)
	}
	return out
}

func mustOVNExec(t *testing.T, ctx context.Context, kctx, ns, pod, container, cmd string) string {
	t.Helper()
	return mustKubectl(t, ctx, kctx, "-n", ns, "exec", pod, "-c", container, "--", "sh", "-lc", cmd)
}

func waitFor(t *testing.T, timeout time.Duration, tick time.Duration, what string, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err == nil && ok {
			return
		}
		lastErr = err
		time.Sleep(tick)
	}
	if lastErr != nil {
		t.Fatalf("timeout waiting for %s: %v", what, lastErr)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func parseSingleLine(out string) string {
	return strings.Trim(strings.TrimSpace(out), "\"")
}

func resolveCentralOVNPod(t *testing.T, ctx context.Context, kctx, ns string) string {
	t.Helper()
	out := mustKubectl(t, ctx, kctx, "-n", ns, "get", "pod", "-l", "app=ovn-ovs", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) != 1 {
		t.Fatalf("expected exactly one central ovn-ovs pod, got %d: %q", len(lines), out)
	}
	return lines[0]
}

func resolveNodePod(t *testing.T, ctx context.Context, kctx, ns, label, node string) string {
	t.Helper()
	out := mustKubectl(t, ctx, kctx, "-n", ns, "get", "pod", "-l", label, "--field-selector", "spec.nodeName="+node, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) != 1 {
		t.Fatalf("expected exactly one pod for label=%s node=%s, got %d: %q", label, node, len(lines), out)
	}
	return lines[0]
}

func applyRoutes(r *RouteReconciler, changes []Type2Route, advertised *GoBGPType2Adapter) error {
	for _, change := range changes {
		if err := r.Apply(context.Background(), change, advertised); err != nil {
			return err
		}
	}
	return nil
}

func reconcileOnce(ctx context.Context, obs EndpointObserver, r *RouteReconciler, _ *server.BgpServer, advertised *GoBGPType2Adapter) ([]LocalEndpoint, []Type2Route, error) {
	desired, err := obs.DesiredLocalEndpoints(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !r.synchronized {
		if err := r.Resync(ctx, advertised, r.AppliedRoutes()); err != nil {
			return nil, nil, err
		}
	}
	changes, err := r.Reconcile(desired)
	if err != nil {
		return nil, nil, err
	}
	if err := applyRoutes(r, changes, advertised); err != nil {
		return nil, nil, err
	}
	return desired, changes, nil
}

func TestI11bRealPodLifecycleToGoBGP(t *testing.T) {
	if os.Getenv("I11B_E2E") != "1" {
		t.Skip("set I11B_E2E=1 to run Kubernetes/OVN GoBGP lifecycle e2e")
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
	localNode := strings.TrimSpace(os.Getenv("I11B_LOCAL_NODE"))
	if localNode == "" {
		localNode = "kind0-control-plane"
	}
	remoteNode := strings.TrimSpace(os.Getenv("I11B_REMOTE_NODE"))
	if remoteNode == "" {
		remoteNode = "kind0-worker"
	}
	preserveOnFailure := strings.TrimSpace(os.Getenv("PRESERVE_ON_FAILURE")) == "1"
	succeeded := false
	testNS := "i11b-evpn"
	lsName := "ls-i11b"

	networkID := "net-i11b"
	disabledNetworkID := "net-i11b-disabled"
	rd := "65000:1100"
	rt := "65000:110"
	vni := uint32(110)
	localVTEP := "10.0.0.1"

	localMAC := "0a:58:fb:02:00:11"
	localIP := "10.251.2.11"
	remoteMAC := "0a:58:fb:02:00:12"
	remoteIP := "10.251.2.12"
	disabledMAC := "0a:58:fb:02:00:13"
	disabledIP := "10.251.2.13"

	nadLocal := "evpn-i11b-local"
	nadRemote := "evpn-i11b-remote"
	nadDisabled := "evpn-i11b-disabled"
	podLocal := "i11b-local"
	podRemote := "i11b-remote"
	podDisabled := "i11b-disabled"
	attachmentID := "net1"

	centralPod := resolveCentralOVNPod(t, ctx, kctx, ovnNS)
	controlOVSPod := resolveNodePod(t, ctx, kctx, ovnNS, "app=ovn-ovs-node-local-cp", localNode)

	systemID := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", controlOVSPod, "-c", "ovn-ovs-node-local", "--",
		"sh", "-lc", "ovs-vsctl --db=unix:/var/run/openvswitch/db.sock get Open_vSwitch . external-ids:system-id",
	))
	localChassis := parseSingleLine(mustKubectl(
		t, ctx, kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--",
		"sh", "-lc", fmt.Sprintf("ovn-sbctl --data=bare --no-heading --columns=_uuid find Chassis name='%s'", systemID),
	))
	if localChassis == "" {
		t.Fatalf("failed to resolve local chassis for system-id=%s", systemID)
	}
	t.Logf("local ownership source: node=%s system-id=%s chassis=%s", localNode, systemID, localChassis)

	cleanup := func() {
		if !succeeded {
			_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "get", "pods", "-o", "wide")
			_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "describe", "pod", podLocal, podRemote, podDisabled)
		}
		if preserveOnFailure && !succeeded {
			return
		}
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "pod", podLocal, podRemote, podDisabled, "--ignore-not-found=true", "--wait=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", testNS, "delete", "network-attachment-definition", nadLocal, nadRemote, nadDisabled, "--ignore-not-found=true")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "-n", ovnNS, "exec", centralPod, "-c", "ovn-ovs", "--", "sh", "-lc", "ovn-nbctl --if-exists lsp-del i11b-orphan; ovn-nbctl --if-exists ls-del ls-i11b")
		_, _ = runCmd(ctx, "kubectl", "--context", kctx, "delete", "namespace", testNS, "--ignore-not-found=true", "--wait=true")
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
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"%s","logicalSwitch":"%s","attachmentID":"%s","ipAddress":"%s/24","macAddress":"%s","timeoutSeconds":10}
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |-
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"%s","logicalSwitch":"%s","attachmentID":"%s","ipAddress":"%s/24","macAddress":"%s","timeoutSeconds":10}
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |-
    {"cniVersion":"1.0.0","name":"%s","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"%s","logicalSwitch":"%s","attachmentID":"%s","ipAddress":"%s/24","macAddress":"%s","timeoutSeconds":10}
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
  - name: bb
    image: busybox:1.36
    command: ["sh","-lc","sleep 3600"]
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
  - name: bb
    image: busybox:1.36
    command: ["sh","-lc","sleep 3600"]
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
  - name: bb
    image: busybox:1.36
    command: ["sh","-lc","sleep 3600"]
`, testNS, nadLocal, testNS, nadLocal, networkID, lsName, attachmentID, localIP, localMAC,
		nadRemote, testNS, nadRemote, networkID, lsName, attachmentID, remoteIP, remoteMAC,
		nadDisabled, testNS, nadDisabled, disabledNetworkID, lsName, attachmentID, disabledIP, disabledMAC,
		podLocal, testNS, nadLocal, localNode,
		podRemote, testNS, nadRemote, remoteNode,
		podDisabled, testNS, nadDisabled, localNode)

	if _, err := runCmdInput(ctx, manifest, "kubectl", "--context", kctx, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply i11b manifest: %v", err)
	}

	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podLocal, "--timeout=240s")
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podRemote, "--timeout=240s")
	mustKubectl(t, ctx, kctx, "-n", testNS, "wait", "--for=condition=Ready", "pod/"+podDisabled, "--timeout=240s")
	mustKubectl(t, ctx, kctx, "-n", testNS, "exec", podLocal, "--", "sh", "-lc", "ip -4 addr show dev net1 | grep -q "+localIP+"/24")
	t.Logf("real pod creation: namespace=%s local=%s remote=%s disabled=%s", testNS, podLocal, podRemote, podDisabled)

	localLSP := fmt.Sprintf("%s_%s_%s_%s", testNS, podLocal, networkID, attachmentID)
	remoteLSP := fmt.Sprintf("%s_%s_%s_%s", testNS, podRemote, networkID, attachmentID)
	disabledLSP := fmt.Sprintf("%s_%s_%s_%s", testNS, podDisabled, disabledNetworkID, attachmentID)

	source := &OVNCtlSource{
		Runner:      ExecRunner{},
		KubeContext: kctx,
		Namespace:   ovnNS,
		CentralPod:  centralPod,
		Container:   "ovn-ovs",
	}

	waitFor(t, 30*time.Second, 300*time.Millisecond, "ovn lsp + chassis bindings", func() (bool, error) {
		ports, err := source.ListLogicalSwitchPorts(ctx)
		if err != nil {
			return false, err
		}
		bindings, err := source.ListPortBindings(ctx)
		if err != nil {
			return false, err
		}
		lspSeen := map[string]bool{}
		for _, p := range ports {
			lspSeen[p.Name] = true
		}
		bound := map[string]string{}
		for _, b := range bindings {
			bound[b.LogicalPort] = b.Chassis
		}
		return lspSeen[localLSP] && lspSeen[remoteLSP] && lspSeen[disabledLSP] &&
			bound[localLSP] == localChassis && bound[disabledLSP] == localChassis && bound[remoteLSP] != "", nil
	})

	// Negative setup: unbound endpoint exists in NB but has no Port_Binding.
	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs",
		fmt.Sprintf("ovn-nbctl --if-exists lsp-del i11b-orphan; ovn-nbctl lsp-add %s i11b-orphan; ovn-nbctl lsp-set-addresses i11b-orphan \"%s %s\"; ovn-nbctl set Logical_Switch_Port i11b-orphan external_ids:evpn.framework/endpoint-id=ep-orphan external_ids:evpn.framework/network-id=%s external_ids:evpn.framework/attachment-id=%s external_ids:evpn.framework/workload-uid=uid-orphan external_ids:evpn.framework/incarnation=inc-orphan",
			lsName, "0a:58:fb:02:00:99", "10.251.2.99", networkID, attachmentID))

	portA := reserveTCPPort(t)
	portB := reserveTCPPort(t)
	speakerA, advertised := newType2TestSpeaker(t, portA, portB)
	defer speakerA.StopBgp(context.Background(), &api.StopBgpRequest{})
	speakerB := server.NewBgpServer()
	go speakerB.Serve()
	defer speakerB.StopBgp(context.Background(), &api.StopBgpRequest{})

	if err := speakerA.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: 65110, RouterId: "10.0.0.11", ListenPort: portA}}); err != nil {
		t.Fatalf("start speaker A: %v", err)
	}
	if err := speakerB.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: 65111, RouterId: "10.0.0.12", ListenPort: portB}}); err != nil {
		t.Fatalf("start speaker B: %v", err)
	}
	if err := speakerA.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65111},
			Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(portB)},
			AfiSafis:  []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true}}},
		},
	}); err != nil {
		t.Fatalf("add peer A->B: %v", err)
	}
	if err := speakerB.AddPeer(context.Background(), &api.AddPeerRequest{
		Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65110},
			Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(portA)},
			AfiSafis:  []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}, Enabled: true}}},
		},
	}); err != nil {
		t.Fatalf("add peer B->A: %v", err)
	}
	waitForEstablished(t, speakerA, 65111, 20*time.Second)
	waitForEstablished(t, speakerB, 65110, 20*time.Second)
	t.Logf("BGP established: A(65110)<->B(65111)")

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
	reconciler := NewRouteReconciler()

	expectedLocal := LocalEndpoint{
		NetworkID:          networkID,
		EndpointID:         "placeholder",
		Incarnation:        "placeholder",
		MACAddress:         localMAC,
		IPAddresses:        []string{localIP},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		LocalVTEP:          localVTEP,
		Advertise:          true,
	}

	waitFor(t, 30*time.Second, 300*time.Millisecond, "observer + route advertisement", func() (bool, error) {
		desired, _, err := reconcileOnce(ctx, observer, reconciler, speakerA, advertised)
		if err != nil {
			return false, err
		}
		var ids []string
		for _, ep := range desired {
			ids = append(ids, ep.EndpointID+"/"+ep.Incarnation+"@"+ep.Chassis+" "+ep.MACAddress+" "+primaryIP(ep.IPAddresses))
		}
		t.Logf("observer desired endpoints: %v", ids)

		if len(desired) != 1 || strings.TrimSpace(primaryIP(desired[0].IPAddresses)) != localIP || strings.TrimSpace(desired[0].MACAddress) != localMAC {
			return false, nil
		}
		observed, err := observeRouteOnPeer(speakerB, expectedLocal)
		if err != nil {
			return false, err
		}
		return observed != nil, nil
	})

	obsLocal, err := observeRouteOnPeer(speakerB, expectedLocal)
	if err != nil || obsLocal == nil {
		t.Fatalf("missing remote type-2 for local endpoint: err=%v", err)
	}
	t.Logf("remote route for local pod: type=%d rd=%s mac=%s ip=%s labels=%v next-hop=%s rt=%t", obsLocal.routeType, obsLocal.rd, obsLocal.mac, obsLocal.ip, obsLocal.labels, obsLocal.nextHop, obsLocal.hasRouteTarget)

	expectedRemote := expectedLocal
	expectedRemote.MACAddress = remoteMAC
	expectedRemote.IPAddresses = []string{remoteIP}
	expectedDisabled := expectedLocal
	expectedDisabled.MACAddress = disabledMAC
	expectedDisabled.IPAddresses = []string{disabledIP}
	expectedOrphan := expectedLocal
	expectedOrphan.MACAddress = "0a:58:fb:02:00:99"
	expectedOrphan.IPAddresses = []string{"10.251.2.99"}

	if got, _ := observeRouteOnPeer(speakerB, expectedRemote); got != nil {
		t.Fatalf("remote-chassis endpoint was incorrectly advertised: %+v", got)
	}
	if got, _ := observeRouteOnPeer(speakerB, expectedDisabled); got != nil {
		t.Fatalf("disabled-network endpoint was incorrectly advertised: %+v", got)
	}
	if got, _ := observeRouteOnPeer(speakerB, expectedOrphan); got != nil {
		t.Fatalf("unbound endpoint was incorrectly advertised: %+v", got)
	}

	desired1, changes1, err := reconcileOnce(ctx, observer, reconciler, speakerA, advertised)
	if err != nil {
		t.Fatalf("reconcile once for idempotency: %v", err)
	}
	_, changes2, err := reconcileOnce(ctx, observer, reconciler, speakerA, advertised)
	if err != nil {
		t.Fatalf("reconcile second time for idempotency: %v", err)
	}
	if len(desired1) != 1 || len(changes2) != 0 {
		t.Fatalf("expected idempotent observation: desired=%d changes1=%d changes2=%d", len(desired1), len(changes1), len(changes2))
	}

	mustKubectl(t, ctx, kctx, "-n", testNS, "delete", "pod", podLocal, "--wait=true", "--timeout=240s")
	waitFor(t, 30*time.Second, 300*time.Millisecond, "route withdrawal after pod delete", func() (bool, error) {
		_, _, err := reconcileOnce(ctx, observer, reconciler, speakerA, advertised)
		if err != nil {
			return false, err
		}
		obs, err := observeRouteOnPeer(speakerB, expectedLocal)
		if err != nil {
			return false, err
		}
		return obs == nil, nil
	})

	waitFor(t, 30*time.Second, 300*time.Millisecond, "ovn cleanup for local lsp", func() (bool, error) {
		ports, err := source.ListLogicalSwitchPorts(ctx)
		if err != nil {
			return false, err
		}
		bindings, err := source.ListPortBindings(ctx)
		if err != nil {
			return false, err
		}
		lspExists := false
		for _, p := range ports {
			if p.Name == localLSP {
				lspExists = true
				break
			}
		}
		bindingExists := false
		for _, b := range bindings {
			if b.LogicalPort == localLSP {
				bindingExists = true
				break
			}
		}
		return !lspExists && !bindingExists, nil
	})
	t.Log("remote withdrawal verified after real pod deletion")

	mustKubectl(t, ctx, kctx, "-n", testNS, "delete", "pod", podRemote, podDisabled, "--ignore-not-found=true", "--wait=true")
	mustOVNExec(t, ctx, kctx, ovnNS, centralPod, "ovn-ovs", "ovn-nbctl --if-exists lsp-del i11b-orphan")

	// Existing stale-incarnation unit path remains required for stale delete safety.
	old := LocalEndpoint{
		NetworkID:          networkID,
		EndpointID:         "stale-endpoint",
		Incarnation:        "inc-1",
		MACAddress:         "0a:58:fb:02:00:21",
		IPAddresses:        []string{"10.251.2.21"},
		VNI:                vni,
		RouteDistinguisher: rd,
		RouteTargets:       []string{rt},
		LocalVTEP:          localVTEP,
		Advertise:          true,
	}
	newer := old
	newer.Incarnation = "inc-2"
	staleAPI := &memoryType2API{}
	rStale := syncedReconciler(t, staleAPI)
	applyType2(t, rStale, staleAPI, planType2(t, rStale, old))
	changes, err := rStale.Reconcile([]LocalEndpoint{newer})
	if err != nil {
		t.Fatalf("stale-safety reconcile newer: %v", err)
	}
	if len(changes) != 2 || changes[0].Action != RouteWithdraw || changes[1].Action != RouteAdvertise {
		t.Fatalf("stale-safety expected withdraw+advertise, got %#v", changes)
	}
	succeeded = true
}
