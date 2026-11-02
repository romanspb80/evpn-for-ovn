package evpn

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func frrContainerName() string {
	if v := strings.TrimSpace(os.Getenv("FRR_CONTAINER")); v != "" {
		return v
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			if strings.Contains(name, "frr") {
				return name
			}
		}
	}
	return "clab-evpn-kind-frr-frr"
}

func resolveFRRNeighborIP(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("FRR_NEIGHBOR_IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GOBGP_NEIGHBOR_IP")); v != "" {
		return v
	}
	out, err := exec.Command("docker", "inspect", frrContainerName(), "--format", "{{range $k,$v := .NetworkSettings.Networks}}{{printf \"%s %s\\n\" $k $v.IPAddress}}{{end}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect FRR container network addresses: %v (%s)", err, out)
	}
	preferred := []string{"kind", "clab", "bridge", "docker"}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] == "" {
			continue
		}
		network, ip := fields[0], fields[1]
		if network == "none" || strings.EqualFold(network, "host") {
			continue
		}
		seen[network] = ip
	}
	for _, network := range preferred {
		if ip, ok := seen[network]; ok {
			return ip
		}
	}
	for _, ip := range seen {
		return ip
	}
	if outStr := strings.TrimSpace(string(out)); outStr != "" {
		return strings.Fields(outStr)[len(strings.Fields(outStr))-1]
	}
	t.Fatalf("FRR container %s has no usable network address for BGP peer reachability", frrContainerName())
	return ""
}

func resolveFRRUnderlayVTEPIP(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("FRR_VTEP_IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("OVN_VTEP_IP")); v != "" {
		return v
	}
	out, err := exec.Command("docker", "exec", frrContainerName(), "sh", "-c", "ip -4 -o addr show dev eth1 | awk '{print $4}' | cut -d/ -f1").CombinedOutput()
	ip := strings.TrimSpace(string(out))
	if err == nil && net.ParseIP(ip) != nil && strings.Count(ip, ".") == 3 {
		return ip
	}
	if v := strings.TrimSpace(os.Getenv("FRR_NEIGHBOR_IP")); v != "" {
		return v
	}
	return resolveFRRNeighborIP(t)
}

func resolveGoBGPLocalIP(t *testing.T, peerIP string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("GOBGP_LOCAL_IP")); v != "" {
		return v
	}
	if peerIP == "" {
		peerIP = resolveFRRNeighborIP(t)
	}
	out, err := exec.Command("ip", "route", "get", peerIP).CombinedOutput()
	if err == nil {
		fields := strings.Fields(string(out))
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "src" {
				if ip := strings.TrimSpace(fields[i+1]); ip != "" {
					return ip
				}
			}
		}
	}
	if out, err = exec.Command("docker", "network", "inspect", "clab", "--format", "{{(index .IPAM.Config 0).Gateway}}").CombinedOutput(); err == nil {
		if ip := strings.TrimSpace(string(out)); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return "127.0.0.1"
}

func clearFRRNeighbors(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "vtysh", "-c", "show running-config").CombinedOutput()
	if err != nil {
		t.Fatalf("read FRR running config for cleanup: %v (%s)", err, out)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "neighbor" {
			continue
		}
		ip := fields[1]
		if net.ParseIP(ip) == nil || ip == "" {
			continue
		}
		seen[ip] = true
	}
	for ip := range seen {
		cmd := []string{"docker", "exec", frrContainerName(), "vtysh",
			"-c", "configure terminal",
			"-c", "router bgp 64512",
			"-c", "no neighbor " + ip,
			"-c", "end",
		}
		if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil && !strings.Contains(string(out), "not found") && !strings.Contains(string(out), "no such neighbor") {
			t.Fatalf("remove stale FRR neighbor %s: %v (%s)", ip, err, out)
		}
	}
}

func configureFRRNeighbor(t *testing.T, localIP string, localASN uint32) {
	t.Helper()
	clearFRRNeighbors(t)
	cmd := []string{"docker", "exec", frrContainerName(), "vtysh",
		"-c", "configure terminal",
		"-c", "router bgp 64512",
		"-c", "no neighbor " + localIP,
		"-c", "neighbor " + localIP + " remote-as " + strconv.FormatUint(uint64(localASN), 10),
		"-c", "address-family l2vpn evpn",
		"-c", "neighbor " + localIP + " activate",
		"-c", "exit-address-family",
		"-c", "exit",
	}
	if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil && !strings.Contains(string(out), "not found") {
		t.Fatalf("configure neighbor on FRR for %s: %v (%s)", localIP, err, out)
	}
}

func setFRRRouterID(t *testing.T, routerID string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "vtysh",
		"-c", "configure terminal",
		"-c", "router bgp 64512",
		"-c", "bgp router-id "+routerID,
		"-c", "end",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("set FRR router-id %s: %v (%s)", routerID, err, out)
	}
}

func setFRRNeighborShutdown(t *testing.T, localIP string, shutdown bool) {
	t.Helper()
	if shutdown {
		cmd := []string{"docker", "exec", frrContainerName(), "vtysh",
			"-c", "configure terminal",
			"-c", "router bgp 64512",
			"-c", "neighbor " + localIP + " shutdown",
			"-c", "exit",
		}
		if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("shutdown FRR neighbor %s: %v (%s)", localIP, err, out)
		}
		return
	}
	configureFRRNeighbor(t, localIP, 65010)
}

func frrEVPNTable(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "vtysh", "-c", "show bgp l2vpn evpn").CombinedOutput()
	if err != nil {
		t.Fatalf("show FRR EVPN table: %v (%s)", err, out)
	}
	return string(out)
}

func waitForFRRRoutePresent(t *testing.T, routeMAC, routeIP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = frrEVPNTable(t)
		if strings.Contains(last, routeMAC) && strings.Contains(last, routeIP) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("FRR EVPN table never contained MAC %s and IP %s within %s\n%s", routeMAC, routeIP, timeout, last)
}

func waitForFRRRouteAbsent(t *testing.T, routeMAC, routeIP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = frrEVPNTable(t)
		if !strings.Contains(last, routeMAC) && !strings.Contains(last, routeIP) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("FRR EVPN table still contained MAC %s / IP %s after %s\n%s", routeMAC, routeIP, timeout, last)
}

func waitForGoBGPRoutePresent(t *testing.T, s *server.BgpServer, endpoint LocalEndpoint, timeout time.Duration) *observedType2Route {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obs, err := observeRouteOnPeer(s, endpoint)
		if err == nil && obs != nil {
			return obs
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GoBGP did not receive route for endpoint %s within %s", endpoint.EndpointID, timeout)
	return nil
}

func waitForGoBGPRouteAbsent(t *testing.T, s *server.BgpServer, endpoint LocalEndpoint, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obs, err := observeRouteOnPeer(s, endpoint)
		if err == nil && obs == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GoBGP still has route for endpoint %s after %s", endpoint.EndpointID, timeout)
}

func countEvpnRoutesOnPeer(t *testing.T, s *server.BgpServer) int {
	t.Helper()
	count := 0
	err := s.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_EVPN}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		if prefix != nil {
			if _, ok := prefix.(*bgp.EVPNNLRI); ok {
				count++
			}
		}
	})
	if err != nil {
		t.Fatalf("list EVPN paths on peer: %v", err)
	}
	return count
}

func ensureFRRBridgeIP(t *testing.T, ipCIDR, dev string, add bool) {
	t.Helper()
	action := "add"
	if !add {
		action = "del"
	}
	cmd := []string{"docker", "exec", frrContainerName(), "ip", "addr", action, ipCIDR, "dev", dev}
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err == nil {
		return
	}
	text := strings.ToLower(string(out))
	if !add && (strings.Contains(text, "no such file") || strings.Contains(text, "cannot find") || strings.Contains(text, "not found")) {
		return
	}
	if add && (strings.Contains(text, "file exists") || strings.Contains(text, "already assigned") || strings.Contains(text, "exists")) {
		return
	}
	t.Fatalf("%s FRR address %s on %s: %v (%s)", action, ipCIDR, dev, err, out)
}

func ensureFRRVXLANBridge(t *testing.T, bridgeName, vxlanName, remoteVTEP string, vni int) {
	t.Helper()
	run := func(args ...string) string {
		out, err := exec.Command("docker", append([]string{"exec", frrContainerName()}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker exec %s failed: %v (%s)", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	tryAdd := func(args ...string) {
		out, err := exec.Command("docker", append([]string{"exec", frrContainerName()}, args...)...).CombinedOutput()
		if err == nil {
			return
		}
		text := strings.ToLower(string(out))
		if strings.Contains(text, "file exists") || strings.Contains(text, "already exists") {
			return
		}
		t.Fatalf("docker exec %s failed: %v (%s)", strings.Join(args, " "), err, out)
	}

	tryAdd("ip", "link", "add", bridgeName, "type", "bridge")
	tryAdd("ip", "link", "add", vxlanName, "type", "vxlan", "id", strconv.Itoa(vni), "remote", remoteVTEP, "dstport", "4789")
	run("ip", "link", "set", vxlanName, "master", bridgeName)
	run("ip", "link", "set", bridgeName, "up")
	run("ip", "link", "set", vxlanName, "up")
	vx := run("ip", "-d", "link", "show", vxlanName)
	if !strings.Contains(vx, "vxlan id "+strconv.Itoa(vni)) {
		t.Fatalf("FRR vxlan %s missing expected VNI %d:\n%s", vxlanName, vni, vx)
	}
}

func setFRRVXLANRemote(t *testing.T, vxlanName, remoteVTEP string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "ip", "link", "set", "dev", vxlanName, "type", "vxlan", "remote", remoteVTEP).CombinedOutput()
	if err != nil {
		t.Fatalf("set FRR VXLAN remote on %s to %s: %v (%s)", vxlanName, remoteVTEP, err, out)
	}
}

func runDockerCommand(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func frrVXLANFDB(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "bridge", "fdb", "show", "dev", "vxlan1000").CombinedOutput()
	if err != nil {
		t.Fatalf("show FRR VXLAN FDB: %v (%s)", err, out)
	}
	return string(out)
}

func waitForFRRVXLANFDBPresent(t *testing.T, routeMAC, remoteVTEP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = frrVXLANFDB(t)
		if strings.Contains(last, routeMAC) && strings.Contains(last, remoteVTEP) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("FRR VXLAN FDB never contained MAC %s via remote VTEP %s within %s\n%s", routeMAC, remoteVTEP, timeout, last)
}

func waitForFRRVXLANFDBAbsent(t *testing.T, routeMAC, remoteVTEP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = frrVXLANFDB(t)
		if (!strings.Contains(last, routeMAC) || !strings.Contains(last, remoteVTEP)) && !strings.Contains(last, routeMAC) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("FRR VXLAN FDB still contained MAC %s via remote VTEP %s after %s\n%s", routeMAC, remoteVTEP, timeout, last)
}

func waitForHostVXLANFDBPresent(t *testing.T, vxlanName, routeMAC, remoteVTEP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanName)
		if strings.Contains(strings.ToLower(last), strings.ToLower(routeMAC)) && strings.Contains(last, remoteVTEP) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("host VXLAN FDB on %s never contained MAC %s via %s within %s\n%s", vxlanName, routeMAC, remoteVTEP, timeout, last)
}

func waitForHostVXLANFDBAbsent(t *testing.T, vxlanName, routeMAC, remoteVTEP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanName)
		if !strings.Contains(strings.ToLower(last), strings.ToLower(routeMAC)) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("host VXLAN FDB on %s still contained MAC %s via %s after %s\n%s", vxlanName, routeMAC, remoteVTEP, timeout, last)
}

func TestGoBGPFRRType2Interop(t *testing.T) {
	if os.Getenv("I12_E2E") != "1" {
		t.Skip("set I12_E2E=1 to run GoBGP ↔ FRR EVPN Type-2 interop test")
	}

	frrIP := resolveFRRNeighborIP(t)
	localIP := resolveGoBGPLocalIP(t, frrIP)

	t.Run("GoBGP_to_FRR", func(t *testing.T) {
		asn := uint32(65010)
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
		setFRRRouterID(t, frrIP)
		waitForEstablished(t, gobgp, 64512, 20*time.Second)
		ensureFRRVXLANBridge(t, "br1000", "vxlan1000", localIP, 1000)
		setFRRVXLANRemote(t, "vxlan1000", localIP)
		ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
		defer ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)

		endpoint := LocalEndpoint{
			NetworkID:          "net-i12-a",
			EndpointID:         "ep-i12-a",
			Incarnation:        "inc-i12-a-1",
			MACAddress:         "00:11:22:33:44:66",
			IPAddresses:        []string{"10.30.40.50/24", "10.30.40.50"},
			VNI:                1010,
			RouteDistinguisher: "65010:1010",
			RouteTargets:       []string{"65010:1010"},
			LocalVTEP:          localIP,
			Advertise:          true,
		}
		route, err := BuildType2Route(endpoint)
		if err != nil {
			t.Fatalf("build Type-2 route: %v", err)
		}
		if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
			t.Fatalf("advertise GoBGP Type-2 route: %v", err)
		}
		waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
		frrTable := frrEVPNTable(t)
		t.Logf("[GoBGP -> FRR] FRR EVPN table:\n%s", frrTable)
		if !strings.Contains(frrTable, "Route Distinguisher: 65010:1010") || !strings.Contains(frrTable, endpoint.MACAddress) {
			t.Fatalf("FRR EVPN table missing advertised route\n%s", frrTable)
		}

		if err := gobgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
			t.Fatalf("withdraw GoBGP Type-2 route: %v", err)
		}
		waitForFRRRouteAbsent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
		t.Log("[GoBGP -> FRR] withdrawal verified: route disappeared from FRR EVPN RIB")
	})

	t.Run("FRR_to_GoBGP", func(t *testing.T) {
		asn := uint32(65020)
		listenPort := reserveTCPPort(t)
		gobgp := server.NewBgpServer()
		go gobgp.Serve()
		defer gobgp.StopBgp(context.Background(), &api.StopBgpRequest{})

		if err := gobgp.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: localIP, ListenPort: listenPort}}); err != nil {
			t.Fatalf("start local GoBGP speaker: %v", err)
		}
		setFRRNeighborShutdown(t, localIP, false)
		configureFRRNeighbor(t, localIP, asn)

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
		waitForEstablished(t, gobgp, 64512, 20*time.Second)

		endpoint := LocalEndpoint{
			NetworkID:          "net-i12-b",
			EndpointID:         "ep-i12-b",
			Incarnation:        "inc-i12-b-1",
			MACAddress:         "12:87:9b:c4:17:56",
			IPAddresses:        []string{"192.168.100.1/24", "192.168.100.1"},
			VNI:                1000,
			RouteDistinguisher: fmt.Sprintf("%s:2", frrIP),
			RouteTargets:       []string{"64512:1000"},
			LocalVTEP:          frrIP,
			Advertise:          true,
		}
		frRBridgeDev := "br1000"
		ensureFRRBridgeIP(t, "192.168.100.1/24", frRBridgeDev, true)
		observed := waitForGoBGPRoutePresent(t, gobgp, endpoint, 20*time.Second)
		if observed.routeType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
			t.Fatalf("unexpected route type from FRR: got=%d want=%d", observed.routeType, bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT)
		}
		if observed.mac != endpoint.MACAddress {
			t.Fatalf("unexpected route MAC from FRR: got=%s want=%s", observed.mac, endpoint.MACAddress)
		}
		if observed.ip != primaryIP(endpoint.IPAddresses) {
			t.Fatalf("unexpected route IP from FRR: got=%s want=%s", observed.ip, primaryIP(endpoint.IPAddresses))
		}
		if len(observed.labels) == 0 || observed.labels[0] != endpoint.VNI {
			t.Fatalf("unexpected VNI/label from FRR: got=%v want=%d", observed.labels, endpoint.VNI)
		}
		if !observed.hasRouteTarget {
			t.Fatalf("FRR route missing expected RT %s", endpoint.RouteTargets[0])
		}
		t.Logf("[FRR -> GoBGP] observed route: type=%d rd=%s mac=%s ip=%s labels=%v next-hop=%s rt=%t", observed.routeType, observed.rd, observed.mac, observed.ip, observed.labels, observed.nextHop, observed.hasRouteTarget)

		beforeDelete := countEvpnRoutesOnPeer(t, gobgp)
		if beforeDelete == 0 {
			t.Fatal("FRR EVPN RIB unexpectedly empty before withdrawal check")
		}
		t.Logf("[FRR -> GoBGP] deleting FRR-originated EVPN endpoint via: docker exec %s ip del 192.168.100.1/24 dev %s", frrContainerName(), frRBridgeDev)
		ensureFRRBridgeIP(t, "192.168.100.1/24", frRBridgeDev, false)
		waitForGoBGPRouteAbsent(t, gobgp, endpoint, 20*time.Second)
		waitForEstablished(t, gobgp, 64512, 10*time.Second)
		if afterDeleteCount := countEvpnRoutesOnPeer(t, gobgp); afterDeleteCount == 0 {
			t.Log("[FRR -> GoBGP] specific IPv4 EVPN route withdrawn; unrelated FRR EVPN routes were not present in this test")
		}
		ensureFRRBridgeIP(t, "192.168.100.1/24", frRBridgeDev, true)
		waitForGoBGPRoutePresent(t, gobgp, endpoint, 20*time.Second)
		t.Logf("[FRR -> GoBGP] withdrawal verified: route %s/%s disappeared from GoBGP EVPN RIB while the peer remained ESTABLISHED; unrelated EVPN routes remained present or no additional route existed in the test topology", endpoint.MACAddress, primaryIP(endpoint.IPAddresses))
	})
}

func TestGoBGPFRRType2VXLANDataPlaneInterop(t *testing.T) {
	if os.Getenv("I2A_E2E") != "1" {
		t.Skip("set I2A_E2E=1 to run GoBGP ↔ FRR EVPN VXLAN dataplane interop")
	}

	frrIP := resolveFRRNeighborIP(t)
	localIP := resolveGoBGPLocalIP(t, frrIP)

	asn := uint32(65030)
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
	setFRRRouterID(t, frrIP)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)

	bridgeName := "br1000"
	vxlanName := "vxlan1000"
	ensureFRRVXLANBridge(t, bridgeName, vxlanName, localIP, 1000)

	endpoint := LocalEndpoint{
		NetworkID:          "net-i2a",
		EndpointID:         "ep-i2a",
		Incarnation:        "inc-i2a-1",
		MACAddress:         "00:11:22:33:44:88",
		IPAddresses:        []string{"10.40.50.60/24", "10.40.50.60"},
		VNI:                1000,
		RouteDistinguisher: "65030:1000",
		RouteTargets:       []string{"65030:1000"},
		LocalVTEP:          localIP,
		Advertise:          true,
	}
	route, err := BuildType2Route(endpoint)
	if err != nil {
		t.Fatalf("build Type-2 route: %v", err)
	}
	if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
		t.Fatalf("advertise GoBGP Type-2 route: %v", err)
	}
	waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
	waitForFRRVXLANFDBPresent(t, endpoint.MACAddress, localIP, 20*time.Second)
	t.Logf("[GoBGP -> FRR dataplane] VXLAN FDB on %s contains MAC %s via VTEP %s:\n%s", vxlanName, endpoint.MACAddress, localIP, frrVXLANFDB(t))

	if err := gobgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
		t.Fatalf("withdraw GoBGP Type-2 route: %v", err)
	}
	waitForFRRRouteAbsent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
	waitForFRRVXLANFDBAbsent(t, endpoint.MACAddress, localIP, 20*time.Second)
	t.Log("[GoBGP -> FRR dataplane] FDB cleanup verified: route removed while the peer stayed ESTABLISHED")
}

func TestGoBGPFRRType2VXLANPacketForwarding(t *testing.T) {
	if os.Getenv("I2A1_E2E") != "1" {
		t.Skip("set I2A1_E2E=1 to run GoBGP ↔ FRR VXLAN packet forwarding validation")
	}

	frrIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	localIP := resolveGoBGPLocalIP(t, frrIP)
	if frrIP == "" {
		t.Fatal("FRR neighbor IP is empty")
	}
	if frrVTEPIP == "" {
		t.Fatal("FRR VTEP IP is empty")
	}

	asn := uint32(65031)
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
	setFRRRouterID(t, frrVTEPIP)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)
	ensureFRRVXLANBridge(t, "br1000", "vxlan1000", localIP, 1000)
	setFRRVXLANRemote(t, "vxlan1000", localIP)
	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
	defer ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)

	endpoint := LocalEndpoint{
		NetworkID:          "net-i2a1",
		EndpointID:         "ep-i2a1",
		Incarnation:        "inc-i2a1-1",
		MACAddress:         "00:11:22:33:44:99",
		IPAddresses:        []string{"192.168.100.10/24", "192.168.100.10"},
		VNI:                1000,
		RouteDistinguisher: "65031:1000",
		RouteTargets:       []string{"65031:1000"},
		LocalVTEP:          localIP,
		Advertise:          true,
	}
	route, err := BuildType2Route(endpoint)
	if err != nil {
		t.Fatalf("build Type-2 route: %v", err)
	}
	if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
		t.Fatalf("advertise GoBGP Type-2 route: %v", err)
	}
	waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
	waitForFRRVXLANFDBPresent(t, endpoint.MACAddress, localIP, 20*time.Second)
	t.Logf("[I2-A.1] FRR EVPN table before packet forwarding:\n%s", frrEVPNTable(t))
	t.Logf("[I2-A.1] FRR VXLAN FDB before packet forwarding:\n%s", frrVXLANFDB(t))
	frrRemoteMAC := frrInterfaceMAC(t, "br1000")
	bridgeHost := "br-vxlan-i2a1"
	vxlanHost := "vxlan-i2a1"
	nsName := "evpn-i2a1"
	underlayDev, underlaySrc := resolveHostUnderlayPath(t, frrVTEPIP)
	cleanupStaticState := func() {
		_ = runHostCommand(t, "ip", "netns", "del", nsName)
		_ = runHostCommand(t, "ip", "link", "set", bridgeHost, "down")
		_ = runHostCommand(t, "ip", "link", "del", bridgeHost)
		_ = runHostCommand(t, "ip", "link", "del", vxlanHost)
		_ = runHostCommand(t, "ip", "link", "del", "veth-i2a1-host")
		_ = runDockerCommand("docker", "exec", frrContainerName(), "bridge", "fdb", "del", endpoint.MACAddress, "dev", "vxlan1000", "dst", localIP, "self")
		_ = runDockerCommand("docker", "exec", frrContainerName(), "ip", "neigh", "del", "192.168.100.10", "dev", "br1000")
	}
	cleanupStaticState()
	defer cleanupStaticState()

	mustHostCommand(t, "sudo", "-n", "ip", "netns", "add", nsName)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", bridgeHost, "type", "bridge")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", bridgeHost, "up")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", "veth-i2a1-host", "type", "veth", "peer", "name", "veth-i2a1-ns")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a1-host", "master", bridgeHost)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a1-host", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a1-ns", "netns", nsName)
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "lo", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "addr", "add", "192.168.100.10/24", "dev", "veth-i2a1-ns")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "dev", "veth-i2a1-ns", "address", endpoint.MACAddress)
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "veth-i2a1-ns", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", vxlanHost, "type", "vxlan", "id", "1000", "dev", underlayDev, "local", underlaySrc, "remote", frrVTEPIP, "dstport", "4789")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", vxlanHost, "master", bridgeHost)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", vxlanHost, "up")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "route", "replace", "192.168.100.0/24", "dev", "veth-i2a1-ns")
	mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "replace", frrRemoteMAC, "dev", vxlanHost, "dst", frrVTEPIP, "self", "permanent")
	mustHostCommand(t, "sudo", "-n", "ip", "neigh", "replace", "192.168.100.1", "lladdr", frrRemoteMAC, "dev", bridgeHost, "nud", "permanent")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "neigh", "replace", "192.168.100.1", "lladdr", frrRemoteMAC, "dev", "veth-i2a1-ns", "nud", "permanent")

	setFRRVXLANRemote(t, "vxlan1000", localIP)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "ip", "neigh", "replace", "192.168.100.10", "lladdr", endpoint.MACAddress, "dev", "br1000", "nud", "permanent")
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "bridge", "fdb", "replace", endpoint.MACAddress, "dev", "vxlan1000", "dst", localIP, "self", "permanent")
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "bridge", "fdb", "show", "dev", "vxlan1000")

	vxlanShowHost := mustHostCommand(t, "sudo", "-n", "ip", "-d", "link", "show", vxlanHost)
	if !strings.Contains(vxlanShowHost, "id 1000") || !strings.Contains(vxlanShowHost, "dstport 4789") || !strings.Contains(vxlanShowHost, "remote "+frrVTEPIP) {
		t.Fatalf("unexpected host VXLAN config:\n%s", vxlanShowHost)
	}
	vxlanShowFRR := mustDockerCommand(t, "docker", "exec", frrContainerName(), "ip", "-d", "link", "show", "vxlan1000")
	if !strings.Contains(vxlanShowFRR, "id 1000") || !strings.Contains(vxlanShowFRR, "dstport 4789") || !strings.Contains(vxlanShowFRR, "remote "+localIP) {
		t.Fatalf("unexpected FRR VXLAN config:\n%s", vxlanShowFRR)
	}

	traceFile := "/tmp/i2a1-vxlan-trace.txt"
	_ = os.Remove(traceFile)
	go func() {
		traceCmd := fmt.Sprintf("timeout 8 tcpdump -ni %s -c 10 udp port 4789 > %s 2>&1 || true", vxlanHost, traceFile)
		_ = exec.Command("sudo", "-n", "bash", "-lc", traceCmd).Run()
	}()
	if out, err := exec.Command("sudo", "-n", "ip", "netns", "exec", nsName, "ping", "-c", "3", "-W", "1", "192.168.100.1").CombinedOutput(); err != nil {
		t.Fatalf("host-side ping to FRR failed: %v (%s)", err, out)
	}
	if out, err := exec.Command("docker", "exec", frrContainerName(), "ping", "-I", "br1000", "-c", "3", "-W", "1", "192.168.100.10").CombinedOutput(); err != nil {
		t.Fatalf("FRR-side ping to host failed: %v (%s)", err, out)
	}
	if vxlanDump, err := os.ReadFile(traceFile); err == nil {
		t.Logf("[I2-A.1] VXLAN packet trace:\n%s", strings.TrimSpace(string(vxlanDump)))
	}
	fdbHost := mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanHost)
	fdbFRR := mustDockerCommand(t, "docker", "exec", frrContainerName(), "bridge", "fdb", "show", "dev", "vxlan1000")
	if !strings.Contains(fdbFRR, "extern_learn") {
		t.Fatalf("FRR vxlan1000 FDB does not show EVPN-learned extern_learn entries:\n%s", fdbFRR)
	}
	if !strings.Contains(fdbFRR, "dst "+localIP+" self permanent") {
		t.Fatalf("FRR vxlan1000 FDB does not show static test-owned entry via %s:\n%s", localIP, fdbFRR)
	}
	t.Logf("[I2-A.1] host FDB:\n%s\n[FRR] FDB:\n%s", fdbHost, fdbFRR)

	if err := gobgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
		t.Fatalf("withdraw GoBGP Type-2 route during forwarding validation: %v", err)
	}
	waitForFRRRouteAbsent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 15*time.Second)
	mustDockerCommand(t, "docker", "exec", frrContainerName(), "bridge", "fdb", "del", endpoint.MACAddress, "dev", "vxlan1000", "dst", localIP, "self")
	mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "del", frrRemoteMAC, "dev", vxlanHost, "dst", frrVTEPIP, "self")
	waitForEstablished(t, gobgp, 64512, 10*time.Second)
	t.Log("[I2-A.1] real VXLAN forwarding validated: packet path succeeded and route withdrawal left the BGP session ESTABLISHED")
}

func TestGoBGPFRRType2Lifecycle(t *testing.T) {
	if os.Getenv("I2A3_E2E") != "1" {
		t.Skip("set I2A3_E2E=1 to run GoBGP ↔ FRR EVPN Type-2 lifecycle validation")
	}

	frrIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	localIP := resolveGoBGPLocalIP(t, frrIP)
	if frrIP == "" {
		t.Fatal("FRR neighbor IP is empty")
	}
	if frrVTEPIP == "" {
		t.Fatal("FRR VTEP IP is empty")
	}

	endpoint := LocalEndpoint{
		NetworkID:          "net-i2a3",
		EndpointID:         "ep-i2a3",
		Incarnation:        "inc-i2a3-1",
		MACAddress:         "00:11:22:33:44:bb",
		IPAddresses:        []string{"192.168.101.10/24", "192.168.101.10"},
		VNI:                1010,
		RouteDistinguisher: "65040:1010",
		RouteTargets:       []string{"65040:1010"},
		LocalVTEP:          localIP,
		Advertise:          true,
	}

	newSpeaker := func(t *testing.T) *server.BgpServer {
		t.Helper()
		asn := uint32(65040)
		listenPort := reserveTCPPort(t)
		gobgp := server.NewBgpServer()
		go gobgp.Serve()
		t.Cleanup(func() { gobgp.StopBgp(context.Background(), &api.StopBgpRequest{}) })
		if err := gobgp.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: asn, RouterId: localIP, ListenPort: listenPort}}); err != nil {
			t.Fatalf("start GoBGP speaker: %v", err)
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
			t.Fatalf("add GoBGP peer: %v", err)
		}
		setFRRNeighborShutdown(t, localIP, false)
		configureFRRNeighbor(t, localIP, asn)
		setFRRRouterID(t, frrVTEPIP)
		waitForEstablished(t, gobgp, 64512, 20*time.Second)
		return gobgp
	}

	advertiseRoute := func(t *testing.T, gobgp *server.BgpServer) {
		t.Helper()
		route, err := BuildType2Route(endpoint)
		if err != nil {
			t.Fatalf("build Type-2 route: %v", err)
		}
		if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
			t.Fatalf("advertise Type-2 route: %v", err)
		}
		waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 20*time.Second)
		waitForFRRVXLANFDBPresent(t, endpoint.MACAddress, localIP, 20*time.Second)
	}

	withdrawRoute := func(t *testing.T, gobgp *server.BgpServer) {
		t.Helper()
		route, err := BuildType2Route(endpoint)
		if err != nil {
			t.Fatalf("build Type-2 route for withdrawal: %v", err)
		}
		if err := gobgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
			t.Fatalf("withdraw Type-2 route: %v", err)
		}
		waitForFRRRouteAbsent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 20*time.Second)
		waitForFRRVXLANFDBAbsent(t, endpoint.MACAddress, localIP, 20*time.Second)
	}

	t.Run("announce", func(t *testing.T) {
		gobgp := newSpeaker(t)
		advertiseRoute(t, gobgp)
		frrTable := frrEVPNTable(t)
		if !strings.Contains(frrTable, endpoint.MACAddress) || !strings.Contains(frrTable, primaryIP(endpoint.IPAddresses)) {
			t.Fatalf("FRR EVPN table missing advertised lifecycle route:\n%s", frrTable)
		}
	})

	t.Run("withdraw_and_reannounce", func(t *testing.T) {
		gobgp := newSpeaker(t)
		advertiseRoute(t, gobgp)
		withdrawRoute(t, gobgp)
		advertiseRoute(t, gobgp)
		if got := strings.Count(frrEVPNTable(t), endpoint.MACAddress); got < 1 {
			t.Fatalf("FRR EVPN table did not restore the advertised route after reannounce: %s", frrEVPNTable(t))
		}
	})

	t.Run("duplicate_advertisement", func(t *testing.T) {
		gobgp := newSpeaker(t)
		advertiseRoute(t, gobgp)
		route, err := BuildType2Route(endpoint)
		if err != nil {
			t.Fatalf("build duplicate route: %v", err)
		}
		if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
			t.Fatalf("duplicate advertise route: %v", err)
		}
		waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 20*time.Second)
		if got := strings.Count(frrEVPNTable(t), endpoint.MACAddress); got != 1 {
			t.Fatalf("duplicate EVPN route count mismatch for MAC %s: got %d (%s)", endpoint.MACAddress, got, frrEVPNTable(t))
		}
	})

	t.Run("session_recovery", func(t *testing.T) {
		gobgp := newSpeaker(t)
		advertiseRoute(t, gobgp)
		setFRRNeighborShutdown(t, localIP, true)
		time.Sleep(2 * time.Second)
		setFRRNeighborShutdown(t, localIP, false)
		configureFRRNeighbor(t, localIP, 65040)
		waitForEstablished(t, gobgp, 64512, 20*time.Second)
		readvertise := func() {
			route, err := BuildType2Route(endpoint)
			if err != nil {
				t.Fatalf("build reannounce route: %v", err)
			}
			if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: route.NLRI, Attrs: route.PathAttributes}}}); err != nil {
				t.Fatalf("reannounce after session recovery: %v", err)
			}
		}
		readvertise()
		waitForFRRRoutePresent(t, endpoint.MACAddress, primaryIP(endpoint.IPAddresses), 20*time.Second)
		if got := strings.Count(frrEVPNTable(t), endpoint.MACAddress); got < 1 {
			t.Fatalf("session recovery did not restore EVPN route: %s", frrEVPNTable(t))
		}
	})

	t.Run("mac_mobility", func(t *testing.T) {
		t.Skip("single-VTEP FRR lab cannot represent a valid EVPN MAC move across two remote VTEPs; dual-VTEP topology required for mobility assertion")
	})
}

func TestGoBGPFRRType2VXLANRouteImport(t *testing.T) {
	if os.Getenv("I2A2_E2E") != "1" {
		t.Skip("set I2A2_E2E=1 to run GoBGP EVPN Type-2 remote route importer validation")
	}

	frrIP := resolveFRRNeighborIP(t)
	frrVTEPIP := resolveFRRUnderlayVTEPIP(t)
	localIP := resolveGoBGPLocalIP(t, frrIP)

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
	setFRRRouterID(t, frrVTEPIP)
	waitForEstablished(t, gobgp, 64512, 20*time.Second)

	ensureFRRVXLANBridge(t, "br1000", "vxlan1000", localIP, 1000)
	setFRRVXLANRemote(t, "vxlan1000", localIP)
	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
	remoteMAC := frrInterfaceMAC(t, "br1000")

	localEndpoint := LocalEndpoint{
		NetworkID:          "net-i2a2-local",
		EndpointID:         "ep-i2a2-local",
		Incarnation:        "inc-i2a2-local-1",
		MACAddress:         "00:11:22:33:44:aa",
		IPAddresses:        []string{"192.168.100.10/24", "192.168.100.10"},
		VNI:                1000,
		RouteDistinguisher: "65032:1000",
		RouteTargets:       []string{"65032:1000"},
		LocalVTEP:          localIP,
		Advertise:          true,
	}
	localRoute, err := BuildType2Route(localEndpoint)
	if err != nil {
		t.Fatalf("build local Type-2 route: %v", err)
	}
	if _, err := gobgp.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: localRoute.NLRI, Attrs: localRoute.PathAttributes}}}); err != nil {
		t.Fatalf("advertise local Type-2 route: %v", err)
	}
	defer func() {
		_ = gobgp.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{{Family: bgp.RF_EVPN, Nlri: localRoute.NLRI, Attrs: localRoute.PathAttributes}}})
	}()
	waitForFRRRoutePresent(t, localEndpoint.MACAddress, primaryIP(localEndpoint.IPAddresses), 15*time.Second)
	waitForFRRVXLANFDBPresent(t, localEndpoint.MACAddress, localIP, 20*time.Second)

	remoteEndpoint := LocalEndpoint{
		NetworkID:          "net-i2a2-remote",
		EndpointID:         "ep-i2a2-remote",
		Incarnation:        "inc-i2a2-remote-1",
		MACAddress:         remoteMAC,
		IPAddresses:        []string{"192.168.100.1/24", "192.168.100.1"},
		VNI:                1000,
		RouteDistinguisher: fmt.Sprintf("%s:2", frrVTEPIP),
		RouteTargets:       []string{"64512:1000"},
		LocalVTEP:          frrVTEPIP,
		Advertise:          true,
	}
	received := waitForGoBGPRoutePresent(t, gobgp, remoteEndpoint, 20*time.Second)
	t.Logf("[I2-A.2] GoBGP observed FRR route: rd=%s mac=%s ip=%s nexthop=%s labels=%v hasRT=%t", received.rd, received.mac, received.ip, received.nextHop, received.labels, received.hasRouteTarget)
	if strings.TrimSpace(received.nextHop) != strings.TrimSpace(frrVTEPIP) {
		t.Fatalf("unexpected FRR Type-2 next-hop: got=%s want=%s", received.nextHop, frrVTEPIP)
	}

	bridgeHost := "br-vxlan-i2a2"
	vxlanHost := "vxlan-i2a2"
	nsName := "evpn-i2a2"
	cleanup := func() {
		_ = runHostCommand(t, "ip", "netns", "del", nsName)
		_ = runHostCommand(t, "ip", "link", "set", bridgeHost, "down")
		_ = runHostCommand(t, "ip", "link", "del", bridgeHost)
		_ = runHostCommand(t, "ip", "link", "del", vxlanHost)
		_ = runHostCommand(t, "ip", "link", "del", "veth-i2a2-host")
		_ = runDockerCommand("docker", "exec", frrContainerName(), "bridge", "fdb", "del", localEndpoint.MACAddress, "dev", "vxlan1000", "dst", localIP, "self")
		_ = runDockerCommand("docker", "exec", frrContainerName(), "ip", "neigh", "del", "192.168.100.10", "dev", "br1000")
	}
	cleanup()
	defer cleanup()

	mustHostCommand(t, "sudo", "-n", "ip", "netns", "add", nsName)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", bridgeHost, "type", "bridge")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", bridgeHost, "up")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", "veth-i2a2-host", "type", "veth", "peer", "name", "veth-i2a2-ns")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a2-host", "master", bridgeHost)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a2-host", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", "veth-i2a2-ns", "netns", nsName)
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "lo", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "addr", "add", "192.168.100.10/24", "dev", "veth-i2a2-ns")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "dev", "veth-i2a2-ns", "address", localEndpoint.MACAddress)
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "link", "set", "veth-i2a2-ns", "up")
	mustHostCommand(t, "sudo", "-n", "ip", "netns", "exec", nsName, "ip", "route", "replace", "192.168.100.0/24", "dev", "veth-i2a2-ns")
	underlayDev, underlaySrc := resolveHostUnderlayPath(t, frrVTEPIP)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "add", vxlanHost, "type", "vxlan", "id", "1000", "dev", underlayDev, "local", underlaySrc, "remote", frrVTEPIP, "dstport", "4789")
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", vxlanHost, "master", bridgeHost)
	mustHostCommand(t, "sudo", "-n", "ip", "link", "set", vxlanHost, "up")

	beforeHostFDB := mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanHost)
	if strings.Contains(strings.ToLower(beforeHostFDB), strings.ToLower(remoteEndpoint.MACAddress)) {
		t.Fatalf("unexpected stale host FDB entry before importer for MAC %s:\n%s", remoteEndpoint.MACAddress, beforeHostFDB)
	}

	importer := NewRemoteType2Importer(
		&GoBGPServerSnapshotSource{Server: gobgp},
		&Type2RouteValidator{
			LocalVTEP: localIP,
			Networks: []EVPNImportNetwork{
				{
					Name:           "i2a2-vni1000",
					VNI:            1000,
					RouteTarget:    "64512:1000",
					VXLANDevice:    vxlanHost,
					BridgeDevice:   bridgeHost,
					NeighborDevice: bridgeHost,
				},
			},
		},
		&LinuxVXLANBackend{},
	)
	reconcileUntil := func(timeout time.Duration) {
		deadline := time.Now().Add(timeout)
		var lastErr error
		for time.Now().Before(deadline) {
			lastErr = importer.ReconcileOnce(context.Background())
			if lastErr != nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if lastErr != nil {
			t.Fatalf("importer reconcile failed: %v", lastErr)
		}
	}

	reconcileUntil(15 * time.Second)
	fdbInstalled := mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanHost)
	if !strings.Contains(fdbInstalled, "00:00:00:00:00:00 dst "+strings.TrimSpace(frrVTEPIP)) {
		t.Fatalf("importer did not install expected flood entry on %s via %s\n%s", vxlanHost, frrVTEPIP, fdbInstalled)
	}
	if out, err := exec.Command("sudo", "-n", "ip", "netns", "exec", nsName, "ping", "-c", "3", "-W", "1", "192.168.100.1").CombinedOutput(); err != nil {
		t.Fatalf("I2-A.2 host endpoint -> FRR ping failed: %v (%s)", err, out)
	}
	if out, err := exec.Command("docker", "exec", frrContainerName(), "ping", "-I", "br1000", "-c", "3", "-W", "1", "192.168.100.10").CombinedOutput(); err != nil {
		t.Fatalf("I2-A.2 FRR -> host endpoint ping failed: %v (%s)", err, out)
	}

	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", false)
	waitForGoBGPRouteAbsent(t, gobgp, remoteEndpoint, 20*time.Second)
	if snapshot, snapErr := (&GoBGPServerSnapshotSource{Server: gobgp}).Snapshot(context.Background()); snapErr == nil {
		validator := &Type2RouteValidator{
			LocalVTEP: localIP,
			Networks: []EVPNImportNetwork{{
				Name:        "i2a2-vni1000",
				VNI:         1000,
				RouteTarget: "64512:1000",
			}},
		}
		for _, r := range snapshot {
			if parsed, ok, _ := validator.Validate(r); ok {
				t.Logf("[I2-A.2] post-withdraw still importable route: mac=%s ip=%s next-hop=%s parsed=%+v", r.MAC, r.IP, r.NextHop, parsed)
			}
		}
	}
	reconcileUntil(20 * time.Second)
	fdbAfterWithdraw := mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanHost)
	if strings.Contains(fdbAfterWithdraw, "00:00:00:00:00:00 dst "+strings.TrimSpace(frrVTEPIP)) {
		t.Fatalf("importer did not remove flood entry after withdrawal on %s\n%s", vxlanHost, fdbAfterWithdraw)
	}
	waitForEstablished(t, gobgp, 64512, 10*time.Second)

	ensureFRRBridgeIP(t, "192.168.100.1/24", "br1000", true)
	waitForGoBGPRoutePresent(t, gobgp, remoteEndpoint, 20*time.Second)
	reconcileUntil(20 * time.Second)
	fdbReinstalled := mustHostCommand(t, "sudo", "-n", "bridge", "fdb", "show", "dev", vxlanHost)
	if !strings.Contains(fdbReinstalled, "00:00:00:00:00:00 dst "+strings.TrimSpace(frrVTEPIP)) {
		t.Fatalf("importer did not restore flood entry after re-advertisement on %s\n%s", vxlanHost, fdbReinstalled)
	}

	if out, err := exec.Command("sudo", "-n", "ip", "netns", "exec", nsName, "ping", "-c", "3", "-W", "1", "192.168.100.1").CombinedOutput(); err != nil {
		t.Fatalf("I2-A.2 host endpoint -> FRR re-advertisement ping failed: %v (%s)", err, out)
	}
	t.Log("[I2-A.2] automatic GoBGP-side remote route import validated: install, withdraw cleanup, and re-advertisement restore")
}

func resolveHostUnderlayPath(t *testing.T, remoteIP string) (string, string) {
	t.Helper()
	out, err := exec.Command("ip", "-4", "route", "get", remoteIP).CombinedOutput()
	if err != nil {
		t.Fatalf("resolve host underlay route to %s: %v (%s)", remoteIP, err, out)
	}
	fields := strings.Fields(string(out))
	dev := ""
	src := ""
	for i := 0; i < len(fields)-1; i++ {
		switch fields[i] {
		case "dev":
			dev = fields[i+1]
		case "src":
			src = fields[i+1]
		}
	}
	if dev == "" || src == "" {
		t.Fatalf("unable to parse host underlay route for %s from: %s", remoteIP, strings.TrimSpace(string(out)))
	}
	return dev, src
}

func frrInterfaceMAC(t *testing.T, ifName string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", frrContainerName(), "cat", "/sys/class/net/"+ifName+"/address").CombinedOutput()
	if err != nil {
		t.Fatalf("read FRR interface MAC %s: %v (%s)", ifName, err, out)
	}
	mac := strings.TrimSpace(string(out))
	if mac == "" {
		t.Fatalf("FRR interface %s has empty MAC", ifName)
	}
	return mac
}

func runHostCommand(t *testing.T, args ...string) error {
	t.Helper()
	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo %s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mustHostCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := append([]string{"-n"}, args...)
	out, err := exec.Command("sudo", cmd...).CombinedOutput()
	if err != nil {
		t.Fatalf("sudo %s failed: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func mustDockerCommand(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}
