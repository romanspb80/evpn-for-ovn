package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	ovsdbpb "github.com/romanspb80/evpn-for-ovn/gen/ovsdb_mgmt"

	"github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type handlerMode string

const (
	handlerModeOVSDB    handlerMode = "ovsdb"
	handlerModeNetlink  handlerMode = "netlink"
	handlerModeDisabled handlerMode = "disabled"
)

type netlinkConfig struct {
	BridgeDevice      string
	VxlanDevice       string
	AdvertiseDevice   string
	VxlanPrefix       string
	VxlanPort         int
	ManageVxlan       bool
	LogicalSwitchName string
}

func resolveHandlerMode() handlerMode {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("BGP_AD_HANDLER_MODE")))
	if mode == "" {
		mode = string(handlerModeOVSDB)
	}
	switch mode {
	case string(handlerModeOVSDB), string(handlerModeNetlink), string(handlerModeDisabled):
		return handlerMode(mode)
	default:
		log.Printf("unsupported BGP_AD_HANDLER_MODE=%q; defaulting to disabled pending k8gobgp-native EVPN route handling", mode)
		return handlerModeDisabled
	}
}

func configuredVniSet() map[int]struct{} {
	raw := strings.TrimSpace(os.Getenv("DYNAMIC_ROUTING_VNIS"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("DYNAMIC_ROUTING_VNI"))
	}
	if raw == "" {
		raw = "1000"
	}
	set := make(map[int]struct{})
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		vni, err := strconv.Atoi(entry)
		if err != nil || vni <= 0 {
			log.Printf("skip invalid configured VNI %q (expected integer > 0)", entry)
			continue
		}
		set[vni] = struct{}{}
	}
	if len(set) == 0 {
		set[1000] = struct{}{}
	}
	return set
}

func isVniProvisioned(vni int) bool {
	configured := configuredVniSet()
	_, ok := configured[vni]
	return ok
}

func defaultVNI() int {
	configured := configuredVniSet()
	chosen := 0
	for vni := range configured {
		if chosen == 0 || vni < chosen {
			chosen = vni
		}
	}
	if chosen <= 0 {
		return 1000
	}
	return chosen
}

func loadNetlinkConfig() netlinkConfig {
	cfg := netlinkConfig{
		BridgeDevice:      strings.TrimSpace(os.Getenv("NETLINK_BRIDGE_DEV")),
		VxlanDevice:       strings.TrimSpace(os.Getenv("NETLINK_VXLAN_DEV")),
		AdvertiseDevice:   strings.TrimSpace(os.Getenv("NETLINK_ADVERTISE_DEV")),
		VxlanPrefix:       strings.TrimSpace(os.Getenv("NETLINK_VXLAN_PREFIX")),
		ManageVxlan:       strings.EqualFold(strings.TrimSpace(os.Getenv("NETLINK_MANAGE_VXLAN")), "true"),
		LogicalSwitchName: strings.TrimSpace(os.Getenv("NETLINK_LOGICAL_SWITCH_NAME")),
	}
	cfg.VxlanPort = 4789
	if raw := strings.TrimSpace(os.Getenv("NETLINK_VXLAN_PORT")); raw != "" {
		if port, err := strconv.Atoi(raw); err == nil && port > 0 {
			cfg.VxlanPort = port
		} else {
			log.Printf("netlink: invalid NETLINK_VXLAN_PORT=%q, defaulting to 4789", raw)
		}
	}
	if cfg.BridgeDevice == "" {
		cfg.BridgeDevice = fmt.Sprintf("br-evpn%d", defaultVNI())
	}
	if cfg.VxlanDevice == "" {
		cfg.VxlanDevice = fmt.Sprintf("vxlan%d", defaultVNI())
	}
	if cfg.VxlanPrefix == "" {
		cfg.VxlanPrefix = "vxlan"
	}
	if cfg.AdvertiseDevice == "" {
		cfg.AdvertiseDevice = fmt.Sprintf("evpnadv%d", defaultVNI())
	}
	if cfg.LogicalSwitchName == "" {
		cfg.LogicalSwitchName = "evpn-network"
	}
	return cfg
}

func resolveNetworkName() string {
	for _, key := range []string{"NETWORK_NAME", "OVN_LS_NAME", "NODE_NAME", "K8S_NODE_NAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" && hostname != "localhost" {
		return hostname
	}
	return "ovn-local"
}

func ovsdbWithRetry(ctx context.Context, client ovsdbpb.OvsdbServiceClient, op string, call func() (*ovsdbpb.Status, error)) (*ovsdbpb.Status, error) {
	if client == nil {
		return nil, fmt.Errorf("ovsdb client is nil")
	}

	var lastStatus *ovsdbpb.Status
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		status, err := call()
		if err == nil && status != nil && status.Success {
			return status, nil
		}
		if status != nil {
			lastStatus = status
		}
		if err != nil {
			lastErr = err
		}

		reason := ""
		if status != nil && status.Message != "" {
			reason = status.Message
		}
		if err != nil {
			if reason != "" {
				reason += "; "
			}
			reason += err.Error()
		}
		if reason == "" {
			reason = "empty response"
		}
		if !strings.Contains(reason, "not connected") && !strings.Contains(reason, "connection refused") && !strings.Contains(reason, "unavailable") && !strings.Contains(reason, "transport is closing") && !strings.Contains(reason, "temporarily unavailable") {
			return status, err
		}
		log.Printf("%s hit transient OVSDB failure; retrying in %ds (attempt %d/8): %s", op, 1+(attempt/2), attempt+1, reason)
		time.Sleep(500 * time.Duration(attempt+1) * time.Millisecond)
	}

	if lastStatus != nil {
		return lastStatus, lastErr
	}
	return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("%s failed after retries", op)}, nil
}

func gobgpGrpcAddr() string {
	for _, key := range []string{"GOBGP_ENDPOINT", "GOBGP_GRPC_ADDR", "GOBGP_ADDR"} {
		if addr := strings.TrimSpace(os.Getenv(key)); addr != "" {
			return addr
		}
	}
	return "127.0.0.1:50051"
}

func gobgpControllerMode() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOBGP_MODE")))
	if mode != "k8gobgp" {
		return false
	}
	addr := strings.ToLower(strings.TrimSpace(gobgpGrpcAddr()))
	if addr == "" || strings.HasPrefix(addr, "unix://") || strings.Contains(addr, ":179") {
		return true
	}
	return false
}

func main() {
	ctx := context.Background()
	mode := resolveHandlerMode()
	if mode == handlerModeDisabled {
		log.Printf("handler disabled: bgp_ad_handler is intentionally kept alive without EVPN route processing.")
		for {
			time.Sleep(30 * time.Second)
		}
	}
	if gobgpControllerMode() && mode != handlerModeOVSDB {
		log.Printf("k8gobgp controller mode detected with non-OVSDB handler; this deployment will remain idle until a native k8gobgp EVPN route consumer is implemented.")
		for {
			time.Sleep(30 * time.Second)
		}
	}
	netlinkCfg := loadNetlinkConfig()
	log.Printf("BGP AD handler mode: %s", mode)

	networkName := resolveNetworkName()
	log.Printf("Using network name: %s", networkName)

	var ovsdbClient ovsdbpb.OvsdbServiceClient
	var ovsdbConn *grpc.ClientConn
	var err error

	if mode == handlerModeOVSDB {
		// =========================
		// Connect to OVSDB gRPC Server
		// =========================
		ovsdbAddr := os.Getenv("OVSDB_ADDR")
		if ovsdbAddr == "" {
			ovsdbAddr = "127.0.0.1:50053"
		}
		ovsdbConn, err = grpc.NewClient(
			ovsdbAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			log.Fatalf("Failed to connect to OVSDB gRPC server: %v", err)
		}
		defer ovsdbConn.Close()

		ovsdbClient = ovsdbpb.NewOvsdbServiceClient(ovsdbConn)
		log.Printf("Connected to OVSDB gRPC server on %s", ovsdbAddr)

		// =========================
		// Ensure network exists
		// =========================
		addNetReq := &ovsdbpb.AddNetworkRequest{Name: networkName}
		netResp, netErr := ovsdbWithRetry(ctx, ovsdbClient, "AddNetwork", func() (*ovsdbpb.Status, error) {
			return ovsdbClient.AddNetwork(ctx, addNetReq)
		})
		if netErr != nil {
			log.Fatalf("Failed to add/ensure network: %v", netErr)
		}
		if netResp == nil || !netResp.Success {
			log.Fatalf("Failed to add network: %v", netResp)
		}
		log.Printf("Network ensured: %s (%s)", networkName, netResp.Message)
	}

	// =========================
	// Connect to GoBGP
	// =========================
	gobgpAddr := gobgpGrpcAddr()
	conn, err := grpc.NewClient(
		gobgpAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("gRPC dial error: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("failed to close connection: %v", err)
		}
	}()

	bgpClient := api.NewGoBgpServiceClient(conn)
	// Get local BGP router ID
	globalResp, err := bgpClient.GetBgp(context.Background(), &api.GetBgpRequest{})
	if err != nil {
		log.Fatalf("Failed to get BGP global config: %v", err)
	}
	localRouterID := globalResp.Global.RouterId
	log.Printf("Local BGP Router ID: %s", localRouterID)

	listStream, err := bgpClient.ListPath(ctx, &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
	})
	if err != nil {
		log.Printf("ListPath error: %v", err)
	} else {
		for {
			msg, recvErr := listStream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				log.Printf("ListPath recv error: %v", recvErr)
				break
			}
			if msg != nil && msg.Destination != nil {
				for _, path := range msg.Destination.Paths {
					handlePath(path, ctx, ovsdbClient, localRouterID, networkName, mode, netlinkCfg)
				}
			}
		}
	}

	log.Println("Waiting for EVPN MAC/IP updates...")
	for {
		stream, err := bgpClient.WatchEvent(context.Background(), &api.WatchEventRequest{
			Table: &api.WatchEventRequest_Table{
				Filters: []*api.WatchEventRequest_Table_Filter{
					{
						Type: api.WatchEventRequest_Table_Filter_TYPE_BEST,
					},
				},
			},
		})
		if err != nil {
			log.Printf("WatchEvent error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		for {
			ev, err := stream.Recv()
			if err != nil {
				log.Printf("stream recv error: %v", err)
				time.Sleep(2 * time.Second)
				break
			}

			if table := ev.GetTable(); table != nil {
				for _, path := range table.Paths {
					handlePath(path, ctx, ovsdbClient, localRouterID, networkName, mode, netlinkCfg)
				}
			}
		}
	}
}

// isValidIP checks if a string is a valid IPv4 or IPv6 address
func isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

func truncateOVSName(name string) string {
	if len(name) <= 13 {
		return name
	}
	return name[:13]
}

func localClientPortName(ip string) string {
	sanitizedIP := strings.ReplaceAll(strings.ReplaceAll(ip, ":", "-"), ".", "-")
	return fmt.Sprintf("k8s-%s", sanitizedIP)
}

func legacyLocalClientPortName(ip string) string {
	sanitizedIP := strings.ReplaceAll(strings.ReplaceAll(ip, ":", "-"), ".", "-")
	return fmt.Sprintf("client-%s", sanitizedIP)
}

func handlePath(path *api.Path, ctx context.Context, ovsdbClient ovsdbpb.OvsdbServiceClient, localRouterID string, networkName string, mode handlerMode, netlinkCfg netlinkConfig) {
	if mode == handlerModeNetlink {
		handlePathNetlink(path, localRouterID, netlinkCfg)
		return
	}
	handlePathOVSDB(path, ctx, ovsdbClient, localRouterID, networkName)
}

func handlePathOVSDB(path *api.Path, ctx context.Context, ovsdbClient ovsdbpb.OvsdbServiceClient, localRouterID string, networkName string) {
	if path.Family.GetAfi() != api.Family_AFI_L2VPN ||
		path.Family.GetSafi() != api.Family_SAFI_EVPN {
		return
	}

	nlri := path.GetNlri()
	if nlri == nil {
		return
	}

	switch evpn := nlri.Nlri.(type) {
	case *api.NLRI_EvpnMacadv:
		mac := evpn.EvpnMacadv

		// Define variables for the logged values
		rd := mac.Rd
		admin := mac.Rd.GetIpAddress().Admin
		assigned := mac.Rd.GetIpAddress().Assigned
		esi := mac.Esi.GetValue()
		etag := mac.EthernetTag
		macAddr := mac.MacAddress
		ip := mac.IpAddress
		labels := mac.Labels
		attrs := path.GetPattrs()
		withdraw := path.IsWithdraw

		// Extract just the IP address if it contains CIDR notation (e.g., "192.168.101.5/24" -> "192.168.101.5")
		if strings.Contains(ip, "/") {
			parts := strings.Split(ip, "/")
			ip = parts[0]
			log.Printf("Extracted IP from CIDR notation: %s", ip)
		}

		// Validate IP address format
		if ip == "" || !isValidIP(ip) {
			log.Printf("Skipping update with invalid IP: %s", ip)
			return
		}

		// Safely extract NextHop by searching for MpReach attribute
		var nextHop string
		for _, attr := range attrs {
			if mpReach := attr.GetMpReach(); mpReach != nil && len(mpReach.NextHops) > 0 {
				nextHop = mpReach.NextHops[0]
				break
			}
		}
		if nextHop == "" {
			log.Print("Warning: No valid NextHop found in attributes")
		}

		// === BGP SPEAKER IDENTIFICATION ===
		// Extract BGP speaker information from the Path
		neighborIp := path.GetNeighborIp()         // IP address of the peer sending this update
		sourceAsn := path.GetSourceAsn()           // ASN of the peer (source of the route)
		sourceId := path.GetSourceId()             // Router ID of the peer
		isFromExternal := path.GetIsFromExternal() // Whether this came from external peer

		// Use log.Print with the variables
		log.Print("=== EVPN MAC/IP Advertisement (RT-2) ===")
		log.Print("RD:", rd)
		log.Print("Admin:", admin)
		log.Print("Assigned:", assigned)
		log.Print("ESI:", esi)
		log.Print("EVI (etag):", etag)
		log.Print("MAC:", macAddr)
		log.Print("IP:", ip)
		log.Print("Labels/VNI:", labels)
		log.Print("NextHop:", nextHop)
		log.Print("Withdraw:", withdraw)
		log.Print("--- BGP Speaker Source ---")
		log.Print("Neighbor IP:", neighborIp)
		log.Print("Source ASN:", sourceAsn)
		log.Print("Source Router ID:", sourceId)
		log.Print("From External Peer:", isFromExternal)

		// === Dynamic Network Discovery ===
		// Get the network (should exist, created in main())
		getNetReq := &ovsdbpb.GetNetworkRequest{Name: networkName}
		getNetResp, gerr := ovsdbClient.GetNetwork(ctx, getNetReq)
		if gerr != nil {
			log.Printf("GetNetwork failed for %s: %v; trying AddNetwork", networkName, gerr)
			addResp, addErr := ovsdbClient.AddNetwork(ctx, &ovsdbpb.AddNetworkRequest{Name: networkName})
			if addErr != nil {
				log.Printf("AddNetwork fallback failed for %s: %v", networkName, addErr)
				return
			}
			if !addResp.Success {
				log.Printf("AddNetwork fallback returned unsuccessful status for %s: %s", networkName, addResp.Message)
				return
			}
			log.Printf("Network ensured via AddNetwork fallback: %s", networkName)
		} else if getNetResp.Network == nil || getNetResp.Network.Name == "" {
			log.Printf("Network not found: %s; trying AddNetwork", networkName)
			addResp, addErr := ovsdbClient.AddNetwork(ctx, &ovsdbpb.AddNetworkRequest{Name: networkName})
			if addErr != nil {
				log.Printf("AddNetwork fallback failed for %s: %v", networkName, addErr)
				return
			}
			if !addResp.Success {
				log.Printf("AddNetwork fallback returned unsuccessful status for %s: %s", networkName, addResp.Message)
				return
			}
			log.Printf("Network ensured via AddNetwork fallback: %s", networkName)
		} else {
			log.Printf("Network found: %s (UUID: %s)", getNetResp.Network.Name, getNetResp.Network.Uuid)
		}

		// === OVSDB Operations via gRPC ===
		// Construct transit LSP name from route source.
		// IMPORTANT: avoid collision with the LogicalSwitch name itself; otherwise all
		// learned MAC/IP addresses are appended to that single port.
		sourceIdShort := strings.ReplaceAll(strings.ReplaceAll(sourceId, ".", "-"), ":", "-")
		if len(sourceIdShort) > 8 {
			sourceIdShort = sourceIdShort[:8]
		}
		neighborShort := strings.ReplaceAll(strings.ReplaceAll(neighborIp, ".", "-"), ":", "-")
		if len(neighborShort) > 8 {
			neighborShort = neighborShort[:8]
		}

		idPart := sourceIdShort
		if idPart == "" {
			idPart = neighborShort
		}
		if idPart == "" {
			idPart = "unknown"
		}
		// 13-char limit: "rt-" + up to 10 chars.
		if len(idPart) > 10 {
			idPart = idPart[:10]
		}
		lspName := "rt-" + idPart
		vxlanName := lspName

		// Get VNI from labels (first label value)
		vniValue := 0
		if len(labels) > 0 {
			vniValue = int(labels[0])
		}
		if !isVniProvisioned(vniValue) {
			log.Printf("Skipping EVPN Type-2 MAC=%s IP=%s VNI=%d: dynamic-routing-vni not provisioned on Logical_Switch", macAddr, ip, vniValue)
			return
		}
		vni := fmt.Sprintf("%d", vniValue)
		isLocalOrigin := (sourceId != "" && sourceId == localRouterID) || nextHop == "0.0.0.0"
		isRemoteSpeaker := neighborIp != "" && sourceId != localRouterID

		if withdraw {
			if isRemoteSpeaker {
				if err := withdrawRemoteOVSDBRoute(ctx, ovsdbClient, networkName, lspName, vxlanName, macAddr, ip); err != nil {
					log.Printf("withdraw remote route failed for MAC=%s IP=%s VNI=%s: %v", macAddr, ip, vni, err)
				}
			} else if isLocalOrigin {
				if err := withdrawLocalOVSDBRoute(ctx, ovsdbClient, networkName, localClientPortName(ip), legacyLocalClientPortName(ip)); err != nil {
					log.Printf("withdraw local route failed for MAC=%s IP=%s VNI=%s: %v", macAddr, ip, vni, err)
				}
			} else {
				log.Printf("withdraw route ignored for MAC=%s IP=%s VNI=%s: unable to classify route (source=%s neighbor=%s)", macAddr, ip, vni, sourceId, neighborIp)
			}
			return
		}

		// Accept into transit/VTEP LSP only routes learned from a different speaker.
		// Locally originated routes (e.g. injected by local AddClient) must not pollute
		// the shared transit port addresses for the active LogicalSwitch.
		//isLocalOrigin := (sourceId != "" && sourceId == localRouterID) || nextHop == "0.0.0.0"
		//isFromDifferentSpeaker := sourceId != "" && sourceId != localRouterID
		//isFromNeighbor := neighborIp != ""
		//if !isLocalOrigin && (isFromDifferentSpeaker || isFromNeighbor) {
		if neighborIp != "" && sourceId != localRouterID {
			// If this MAC/IP is already represented by a local client LSP, do not re-import it
			// via transit VTEP from FRR (prevents local endpoint from polluting transit ports).
			localPortNames := []string{localClientPortName(ip), legacyLocalClientPortName(ip)}
			addr := fmt.Sprintf("%s %s", macAddr, ip)
			for _, localPortName := range localPortNames {
				localPortResp, lerr := ovsdbClient.GetLogicalSwitchPort(ctx, &ovsdbpb.LogicalSwitchPortRequest{Name: localPortName})
				if lerr != nil || localPortResp == nil || localPortResp.LogicalSwitchPort == nil {
					continue
				}
				for _, existing := range localPortResp.LogicalSwitchPort.Addresses {
					if existing == addr {
						cleanupReq := &ovsdbpb.DelClientRequest{LspName: lspName, Mac: macAddr, Ip: ip}
						if cleanupResp, cerr := ovsdbClient.DelClient(ctx, cleanupReq); cerr != nil {
							log.Printf("Reflected-route cleanup on %s failed: %v", lspName, cerr)
						} else if cleanupResp != nil && cleanupResp.Success {
							log.Printf("Reflected-route cleanup removed %s from %s", addr, lspName)
						}
						if lspName != networkName {
							legacyReq := &ovsdbpb.DelClientRequest{LspName: networkName, Mac: macAddr, Ip: ip}
							if legacyResp, cerr := ovsdbClient.DelClient(ctx, legacyReq); cerr != nil {
								log.Printf("Legacy reflected-route cleanup on %s failed: %v", networkName, cerr)
							} else if legacyResp != nil && legacyResp.Success {
								log.Printf("Legacy reflected-route cleanup removed %s from %s", addr, networkName)
							}
						}
						log.Printf("Skipping reflected remote update for local endpoint %s on %s", addr, localPortName)
						return
					}
				}
			}

			// Use BGP neighbor transport IP as VXLAN remote endpoint.
			// This keeps OVS side pointed to the actual FRR peer (e.g. 172.17.0.2),
			// instead of EVPN next-hop/router-id values such as 1.1.1.1.
			tunnelRemoteIP := neighborIp
			if net.ParseIP(tunnelRemoteIP) == nil {
				tunnelRemoteIP = nextHop
			}

			// Always call AddVtep to reconcile existing interface options (remote_ip/key/dst_port)
			// and repair stale values from previous runs.
			addVtepReq := &ovsdbpb.AddVtepRequest{
				LsName:    networkName,
				LspName:   lspName,
				VxlanName: vxlanName,
				RemoteIp:  tunnelRemoteIP,
				Key:       vni,
				DstPort:   "4789",
			}

			status, err := ovsdbWithRetry(ctx, ovsdbClient, "AddVtep", func() (*ovsdbpb.Status, error) {
				return ovsdbClient.AddVtep(ctx, addVtepReq)
			})
			if err != nil {
				log.Printf("AddVtep gRPC call failed: %v", err)
				return
			}
			if !status.Success {
				log.Printf("AddVtep failed: %s", status.Message)
				return
			}
			log.Printf("VTEP ensured via gRPC: lspName=%s, vxlanName=%s, remoteIP=%s, vni=%s, network=%s", lspName, vxlanName, tunnelRemoteIP, vni, networkName)

			// Add client (MAC/IP) to LSP via gRPC
			addClientReq := &ovsdbpb.AddClientRequest{
				LspName: lspName,
				Mac:     macAddr,
				Ip:      ip,
			}

			status, err = ovsdbWithRetry(ctx, ovsdbClient, "AddClient", func() (*ovsdbpb.Status, error) {
				return ovsdbClient.AddClient(ctx, addClientReq)
			})
			if err != nil {
				log.Printf("AddClient gRPC call failed: %v", err)
				return
			}
			if !status.Success {
				log.Printf("AddClient failed: %s", status.Message)
				return
			}
			log.Printf("Client mac=%s, ip=%s added to transit LSP %s from Neighbor %s\n", macAddr, ip, lspName, neighborIp)
		} else {
			log.Printf("Treating route as local-origin (sourceId=%s localRouterID=%s nextHop=%s); provisioning local client port", sourceId, localRouterID, nextHop)
			// Create Logical Switch Port for client and a port in br-int, then bind them by name
			clientName := localClientPortName(ip)
			addr := fmt.Sprintf("%s %s", macAddr, ip)
			lspPort := &ovsdbpb.LogicalSwitchPort{
				Name:      clientName,
				Addresses: []string{addr},
				LsName:    networkName,
			}

			status, err := ovsdbWithRetry(ctx, ovsdbClient, "AddLogicalSwitchPort", func() (*ovsdbpb.Status, error) {
				return ovsdbClient.AddLogicalSwitchPort(ctx, lspPort)
			})
			if err != nil {
				log.Printf("AddLogicalSwitchPort gRPC call failed: %v", err)
				return
			}
			if !status.Success {
				log.Printf("AddLogicalSwitchPort failed: %s", status.Message)
				return
			}
			log.Printf("LogicalSwitchPort created via gRPC: %s (network: %s)", clientName, networkName)

			// br-int port creation handled server-side in AddLogicalSwitchPort
			log.Printf("Requested creation of OVS port %s on br-int via OVSDB gRPC server", clientName)
		}

		log.Printf("AddClient XXXXXX")

	default:
		// others EVPN типы (RT-3, RT-5 и т.д.)
	}
}

func withdrawRemoteOVSDBRoute(ctx context.Context, ovsdbClient ovsdbpb.OvsdbServiceClient, networkName string, lspName string, vxlanName string, macAddr string, ip string) error {
	delim := &ovsdbpb.DelClientRequest{LspName: lspName, Mac: macAddr, Ip: ip}
	if status, err := ovsdbClient.DelClient(ctx, delim); err != nil {
		return fmt.Errorf("DelClient(%s) failed: %w", lspName, err)
	} else if status == nil || !status.Success {
		log.Printf("withdraw remote route: DelClient(%s) returned unsuccessful status", lspName)
	}

	if lspName != networkName {
		legacy := &ovsdbpb.DelClientRequest{LspName: networkName, Mac: macAddr, Ip: ip}
		if status, err := ovsdbClient.DelClient(ctx, legacy); err != nil {
			log.Printf("withdraw remote route: best-effort DelClient(%s) failed: %v", networkName, err)
		} else if status != nil && status.Success {
			log.Printf("withdraw remote route: removed stale tuple from %s", networkName)
		}
	}

	lspResp, err := ovsdbClient.GetLogicalSwitchPort(ctx, &ovsdbpb.LogicalSwitchPortRequest{Name: lspName})
	if err != nil {
		return fmt.Errorf("GetLogicalSwitchPort(%s) failed: %w", lspName, err)
	}
	if lspResp != nil && lspResp.LogicalSwitchPort != nil && len(lspResp.LogicalSwitchPort.Addresses) == 0 {
		if status, err := ovsdbClient.DelVtep(ctx, &ovsdbpb.DelVtepRequest{LsName: networkName, LspName: lspName, VxlanName: vxlanName}); err != nil {
			return fmt.Errorf("DelVtep(%s) failed: %w", lspName, err)
		} else if status == nil || !status.Success {
			return fmt.Errorf("DelVtep(%s) returned unsuccessful status", lspName)
		}
		log.Printf("withdraw remote route: removed empty VTEP %s", lspName)
	}
	return nil
}

func withdrawLocalOVSDBRoute(ctx context.Context, ovsdbClient ovsdbpb.OvsdbServiceClient, networkName string, localPortNames ...string) error {
	for _, portName := range localPortNames {
		if portName == "" {
			continue
		}
		resp, err := ovsdbClient.GetLogicalSwitchPort(ctx, &ovsdbpb.LogicalSwitchPortRequest{Name: portName})
		if err != nil {
			log.Printf("withdraw local route: GetLogicalSwitchPort(%s) failed: %v", portName, err)
			continue
		}
		if resp == nil || resp.LogicalSwitchPort == nil {
			continue
		}
		if status, err := ovsdbClient.DelLogicalSwitchPort(ctx, &ovsdbpb.LogicalSwitchPortRequest{Name: portName}); err != nil {
			log.Printf("withdraw local route: DelLogicalSwitchPort(%s) failed: %v", portName, err)
			continue
		} else if status != nil && status.Success {
			log.Printf("withdraw local route: deleted local port %s from %s", portName, networkName)
		}
	}
	return nil
}

func handlePathNetlink(path *api.Path, localRouterID string, cfg netlinkConfig) {
	if path.Family.GetAfi() != api.Family_AFI_L2VPN || path.Family.GetSafi() != api.Family_SAFI_EVPN {
		return
	}
	if path.GetNlri() == nil {
		return
	}

	evpnMac := path.GetNlri().GetEvpnMacadv()
	if evpnMac == nil {
		return
	}

	macAddr := strings.TrimSpace(evpnMac.MacAddress)
	clientIP := strings.TrimSpace(evpnMac.IpAddress)
	if strings.Contains(clientIP, "/") {
		clientIP = strings.SplitN(clientIP, "/", 2)[0]
	}
	if macAddr == "" {
		return
	}
	if clientIP != "" && net.ParseIP(clientIP) == nil {
		log.Printf("netlink: skip invalid client IP %q", clientIP)
		return
	}

	remoteVTEP := remoteVTEPFromPath(path)
	if remoteVTEP == "" {
		log.Printf("netlink: route without valid remote VTEP (neighbor=%q source=%q); reconciling local kernel state only", path.GetNeighborIp(), path.GetSourceId())
	}

	sourceID := path.GetSourceId()
	isLocalOrigin := sourceID == localRouterID || remoteVTEP == localRouterID
	if isLocalOrigin {
		log.Printf("netlink: local-origin route MAC=%s IP=%s source=%s localRouterID=%s remoteVTEP=%s; reconciling local kernel state", macAddr, clientIP, sourceID, localRouterID, remoteVTEP)
	}
	vni := 0
	if len(evpnMac.Labels) > 0 {
		vni = int(evpnMac.Labels[0])
	}
	if vni <= 0 {
		log.Printf("netlink: skip route with empty/invalid VNI MAC=%s IP=%s labels=%v", macAddr, clientIP, evpnMac.Labels)
		return
	}
	if !isVniProvisioned(vni) {
		log.Printf("netlink: skip EVPN Type-2 MAC=%s IP=%s VNI=%d: dynamic-routing-vni not provisioned on Logical_Switch", macAddr, clientIP, vni)
		return
	}

	eventAction := "ADD"
	if path.IsWithdraw {
		eventAction = "DEL"
	}
	log.Printf(
		"netlink: EVPN Type-2 %s VNI=%d MAC=%s IP=%s VTEP=%s source=%s neighbor=%s",
		eventAction,
		vni,
		macAddr,
		clientIP,
		remoteVTEP,
		sourceID,
		strings.TrimSpace(path.GetNeighborIp()),
	)

	if err := applyNetlinkClient(path.IsWithdraw, macAddr, clientIP, remoteVTEP, vni, cfg); err != nil {
		log.Printf("netlink: failed to apply route MAC=%s IP=%s VNI=%d withdraw=%v: %v", macAddr, clientIP, vni, path.IsWithdraw, err)
		return
	}

	action := "installed"
	if path.IsWithdraw {
		action = "removed"
	}
	log.Printf("netlink: %s MAC=%s IP=%s VNI=%d remoteVTEP=%s", action, macAddr, clientIP, vni, remoteVTEP)
}

func firstMpReachNextHop(attrs []*api.Attribute) string {
	for _, attr := range attrs {
		if mpReach := attr.GetMpReach(); mpReach != nil && len(mpReach.NextHops) > 0 {
			return strings.TrimSpace(mpReach.NextHops[0])
		}
	}
	return ""
}

func remoteVTEPFromPath(path *api.Path) string {
	if path == nil {
		return ""
	}
	nextHop := firstMpReachNextHop(path.GetPattrs())
	ip := net.ParseIP(nextHop)
	if ip == nil || ip.IsUnspecified() {
		return ""
	}
	return nextHop
}

// This handler reconciles Linux EVPN integration state only. OVS tunnel ports
// and br-int OpenFlow programming remain the responsibility of
// ovn-controller, which consumes the kernel state through Netlink.
func applyNetlinkClient(withdraw bool, macAddr string, clientIP string, remoteVTEP string, vni int, cfg netlinkConfig) error {
	hw, err := net.ParseMAC(macAddr)
	if err != nil {
		return fmt.Errorf("invalid mac %q: %w", macAddr, err)
	}
	var clientAddr net.IP
	if clientIP != "" {
		clientAddr = net.ParseIP(clientIP)
		if clientAddr == nil {
			return fmt.Errorf("invalid client IP %q", clientIP)
		}
	}
	remoteAddr := net.ParseIP(remoteVTEP)
	hasRemote := remoteAddr != nil

	bridgeLink, err := ensureBridgeLink(cfg.BridgeDevice)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(bridgeLink); err != nil {
		return fmt.Errorf("link up %s: %w", bridgeLink.Attrs().Name, err)
	}

	var vxlanLink netlink.Link
	if hasRemote {
		vxlanLink, err = resolveVxlanLink(vni, cfg)
		if err != nil {
			return err
		}
		if cfg.ManageVxlan {
			if err := netlink.LinkSetUp(vxlanLink); err != nil {
				return fmt.Errorf("link up %s: %w", vxlanLink.Attrs().Name, err)
			}
			if err := netlink.LinkSetMaster(vxlanLink, bridgeLink); err != nil {
				return fmt.Errorf("attach vxlan %s to bridge %s: %w", vxlanLink.Attrs().Name, bridgeLink.Attrs().Name, err)
			}
		}
		if err := setRemoteVtepNeighbor(vxlanLink.Attrs().Name, remoteVTEP, withdraw); err != nil {
			return err
		}

		localOnBridge, err := isMacLearnedOnNonVxlanPort(bridgeLink.Attrs().Name, vxlanLink.Attrs().Name, macAddr)
		if err != nil {
			return fmt.Errorf("check local bridge ownership for %s: %w", macAddr, err)
		}
		if localOnBridge && !withdraw {
			if err := removeMacFromVxlanFdb(vxlanLink.Attrs().Name, macAddr); err != nil {
				return fmt.Errorf("remove conflicting remote fdb for %s on %s: %w", macAddr, vxlanLink.Attrs().Name, err)
			}
			hasRemote = false
		}
	}

	if hasRemote {
		if err := setRemoteMacFdbEntry(vxlanLink.Attrs().Name, macAddr, remoteVTEP, withdraw); err != nil {
			return err
		}
		if err := setFloodEntry(vxlanLink.Attrs().Name, remoteVTEP, withdraw); err != nil {
			return err
		}
	}

	if clientAddr != nil {
		neighborDevice := cfg.AdvertiseDevice
		if _, err := netlink.LinkByName(neighborDevice); err != nil {
			neighborDevice = cfg.BridgeDevice
		}
		neighborLink, err := netlink.LinkByName(neighborDevice)
		if err != nil {
			return fmt.Errorf("neighbor/interface %s not found: %w", neighborDevice, err)
		}
		family := syscall.AF_INET
		if clientAddr.To4() == nil {
			family = syscall.AF_INET6
		}
		neighborEntry := &netlink.Neigh{
			LinkIndex:    neighborLink.Attrs().Index,
			Family:       family,
			State:        netlink.NUD_PERMANENT,
			HardwareAddr: hw,
			IP:           clientAddr,
		}
		if withdraw {
			_ = netlink.NeighDel(neighborEntry)
		} else {
			if err := netlink.NeighSet(neighborEntry); err != nil {
				return fmt.Errorf("neighbor set on %s: %w", neighborDevice, err)
			}
		}
	}

	return nil
}

func setRemoteVtepNeighbor(vxlanName string, remoteVTEP string, withdraw bool) error {
	if remoteVTEP == "" {
		return nil
	}
	ip := net.ParseIP(remoteVTEP)
	if ip == nil {
		return fmt.Errorf("invalid remote VTEP %q", remoteVTEP)
	}
	args := []string{"neigh"}
	if withdraw {
		args = append(args, "del")
	} else {
		args = append(args, "replace")
	}
	args = append(args, remoteVTEP, "dev", vxlanName, "lladdr", "00:00:00:00:00:00", "nud", "permanent")
	cmd := exec.Command("ip", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if withdraw && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("ip %s failed: %w: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

func isMacLearnedOnNonVxlanPort(bridgeName string, vxlanName string, macAddr string) (bool, error) {
	out, err := exec.Command("bridge", "fdb", "show", "br", bridgeName).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("bridge fdb show br %s failed: %w: %s", bridgeName, err, strings.TrimSpace(string(out)))
	}
	target := strings.ToLower(strings.TrimSpace(macAddr))
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}
		if strings.ToLower(fields[0]) != target {
			continue
		}
		devIdx := -1
		for i := 1; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				devIdx = i + 1
				break
			}
		}
		if devIdx == -1 || devIdx >= len(fields) {
			continue
		}
		dev := fields[devIdx]
		if dev != "" && dev != vxlanName {
			return true, nil
		}
	}
	return false, nil
}

func removeMacFromVxlanFdb(vxlanName string, macAddr string) error {
	out, err := exec.Command("bridge", "fdb", "show", "dev", vxlanName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("bridge fdb show dev %s failed: %w: %s", vxlanName, err, strings.TrimSpace(string(out)))
	}
	target := strings.ToLower(strings.TrimSpace(macAddr))
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || strings.ToLower(fields[0]) != target {
			continue
		}

		// Reconstruct fdb delete arguments from existing entry to remove all variants.
		args := []string{"fdb", "del", fields[0], "dev", vxlanName}
		for i := 1; i < len(fields); i++ {
			switch fields[i] {
			case "dst":
				if i+1 < len(fields) {
					args = append(args, "dst", fields[i+1])
					i++
				}
			case "self", "permanent", "router", "static", "extern_learn", "master", "vlan":
				if fields[i] == "vlan" && i+1 < len(fields) {
					args = append(args, "vlan", fields[i+1])
					i++
					continue
				}
				args = append(args, fields[i])
			}
		}
		if delOut, delErr := exec.Command("bridge", args...).CombinedOutput(); delErr != nil {
			msg := strings.TrimSpace(string(delOut))
			if !strings.Contains(msg, "No such file") && !strings.Contains(msg, "Cannot find") {
				return fmt.Errorf("bridge %s failed: %w: %s", strings.Join(args, " "), delErr, msg)
			}
		}
	}
	return nil
}

func setRemoteMacFdbEntry(vxlanName string, hwAddr string, remoteVTEP string, withdraw bool) error {
	args := []string{"fdb"}
	if withdraw {
		args = append(args, "del")
	} else {
		args = append(args, "replace")
	}
	args = append(args,
		hwAddr,
		"dev", vxlanName,
		"dst", remoteVTEP,
		"self",
		"permanent",
	)
	cmd := exec.Command("bridge", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if withdraw && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("bridge fdb %s on %s failed: %w: %s", strings.Join(args[1:], " "), vxlanName, err, msg)
	}
	return nil
}

func setFloodEntry(vxlanName string, remoteVTEP string, withdraw bool) error {
	args := []string{"fdb"}
	if withdraw {
		args = append(args, "del")
	} else {
		// Non-unicast flood entries should be appended; replace is rejected by bridge(8).
		args = append(args, "append")
	}
	args = append(args,
		"00:00:00:00:00:00",
		"dev", vxlanName,
		"dst", remoteVTEP,
		"self",
		"permanent",
	)
	cmd := exec.Command("bridge", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if !withdraw && strings.Contains(msg, "File exists") {
			return nil
		}
		if withdraw && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("bridge fdb %s on %s failed: %w: %s", strings.Join(args[1:], " "), vxlanName, err, msg)
	}
	return nil
}

func ensureBridgeLink(name string) (netlink.Link, error) {
	if link, err := netlink.LinkByName(name); err == nil {
		return link, nil
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, fmt.Errorf("create bridge %s: %w", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("resolve bridge %s after create: %w", name, err)
	}
	return link, nil
}

func resolveVxlanLink(vni int, cfg netlinkConfig) (netlink.Link, error) {
	if cfg.VxlanDevice != "" {
		link, err := netlink.LinkByName(cfg.VxlanDevice)
		if err == nil {
			return link, nil
		}
		if !cfg.ManageVxlan {
			return nil, fmt.Errorf("vxlan device %s not found (managed by ovn-controller)", cfg.VxlanDevice)
		}
		vx := &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: cfg.VxlanDevice},
			VxlanId:   vni,
			Port:      cfg.VxlanPort,
			Learning:  true,
			Proxy:     false,
		}
		if err := netlink.LinkAdd(vx); err != nil {
			return nil, fmt.Errorf("create vxlan device %s: %w", cfg.VxlanDevice, err)
		}
		link, err = netlink.LinkByName(cfg.VxlanDevice)
		if err != nil {
			return nil, fmt.Errorf("resolve vxlan device %s after create: %w", cfg.VxlanDevice, err)
		}
		return link, nil
	}

	links, err := netlink.LinkList()
	if err == nil {
		for _, link := range links {
			vx, ok := link.(*netlink.Vxlan)
			if !ok {
				continue
			}
			if vx.VxlanId == vni {
				return link, nil
			}
		}
	}
	if !cfg.ManageVxlan {
		return nil, fmt.Errorf("vxlan interface for VNI %d not found (managed by ovn-controller)", vni)
	}

	candidate := cfg.VxlanPrefix + strconv.Itoa(vni)
	link, err := netlink.LinkByName(candidate)
	if err == nil {
		return link, nil
	}

	vx := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: candidate},
		VxlanId:   vni,
		Port:      cfg.VxlanPort,
		Learning:  true,
		Proxy:     false,
	}
	if err := netlink.LinkAdd(vx); err != nil {
		return nil, fmt.Errorf("create vxlan interface %s for VNI %d: %w", candidate, vni, err)
	}
	link, err = netlink.LinkByName(candidate)
	if err != nil {
		return nil, fmt.Errorf("resolve vxlan interface %s after create: %w", candidate, err)
	}
	return link, nil
}
