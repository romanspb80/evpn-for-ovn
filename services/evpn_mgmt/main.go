package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osrg/gobgp/v4/api"

	pb "github.com/romanspb80/evpn-for-ovn/gen/evpn_mgmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type server struct {
	pb.UnimplementedEvpnServiceServer
	Client          api.GoBgpServiceClient
	mu              sync.Mutex
	provisionedVnis map[int32]string
	speakerMu       sync.Mutex
	speakerStarted  bool
}

func gobgpMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOBGP_MODE")))
	if mode != "" {
		return mode
	}
	addr := strings.TrimSpace(os.Getenv("GOBGP_ADDR"))
	if strings.Contains(addr, "k8gobgp") || strings.Contains(addr, ":179") {
		return "k8gobgp"
	}
	return "legacy"
}

func gobgpGRPCAddr() string {
	for _, key := range []string{"GOBGP_ENDPOINT", "GOBGP_GRPC_ADDR", "GOBGP_ADDR"} {
		if addr := strings.TrimSpace(os.Getenv(key)); addr != "" {
			return addr
		}
	}
	return "127.0.0.1:50051"
}

func shouldUseGoBgpGrpcClient(mode, addr string) bool {
	if strings.TrimSpace(addr) == "" {
		return false
	}
	if mode != "k8gobgp" {
		return true
	}
	endpoint := strings.ToLower(strings.TrimSpace(addr))
	if strings.HasPrefix(endpoint, "unix://") {
		return false
	}
	if strings.Contains(endpoint, ":179") {
		return false
	}
	return true
}

func (s *server) runningSpeaker(ctx context.Context) (*api.Global, bool) {
	if s.Client == nil {
		return nil, false
	}
	resp, err := s.Client.GetBgp(ctx, &api.GetBgpRequest{})
	if err != nil || resp == nil || resp.Global == nil {
		return nil, false
	}
	globalCfg := resp.Global
	if globalCfg.Asn == 0 {
		return nil, false
	}
	routerID := strings.TrimSpace(globalCfg.RouterId)
	if routerID == "" || net.ParseIP(routerID) == nil {
		return nil, false
	}
	return globalCfg, true
}

func (s *server) isSpeakerRunning(ctx context.Context) bool {
	_, ok := s.runningSpeaker(ctx)
	return ok
}

func (s *server) AddSpeaker(ctx context.Context, req *pb.Speaker) (*pb.Status, error) {
	if s.Client == nil {
		return &pb.Status{
			Success: false,
			Message: "GoBGP gRPC client is unavailable. Configure a reachable TCP endpoint via GOBGP_ENDPOINT/GOBGP_GRPC_ADDR (e.g. k8gobgp.k8gobgp-system.svc.cluster.local:50051) or use BGPConfiguration CRDs.",
		}, nil
	}

	asnRaw := ""
	if req != nil {
		asnRaw = strconv.FormatInt(int64(req.GetAsn()), 10)
	}
	if asnRaw == "" || req == nil || req.GetAsn() == 0 {
		asnRaw = strings.TrimSpace(os.Getenv("GOBGP_ASN"))
	}
	if asnRaw == "" {
		return &pb.Status{
			Success: false,
			Message: "GOBGP_ASN is not set; configure it through the deployment environment or management request",
		}, nil
	}
	asn, err := strconv.ParseUint(asnRaw, 10, 32)
	if err != nil {
		return &pb.Status{
			Success: false,
			Message: fmt.Sprintf("invalid GOBGP_ASN=%q: %v", asnRaw, err),
		}, nil
	}

	routerID := ""
	if req != nil {
		routerID = strings.TrimSpace(req.GetRouterId())
	}
	if routerID == "" {
		routerID = strings.TrimSpace(os.Getenv("GOBGP_ROUTER_ID"))
	}
	if routerID == "" {
		return &pb.Status{
			Success: false,
			Message: "GOBGP_ROUTER_ID is not set; configure it through the deployment environment or management request",
		}, nil
	}

	s.speakerMu.Lock()
	defer s.speakerMu.Unlock()

	globalCfg := &api.Global{
		Asn:        uint32(asn),
		RouterId:   routerID,
		ListenPort: 179,
	}

	if s.isSpeakerRunning(ctx) {
		log.Printf("BGP speaker already active: ASN=%d router-id=%s; reusing running speaker instead of rebind", asn, routerID)
		if current, ok := s.runningSpeaker(ctx); ok && current.Asn == uint32(asn) && strings.TrimSpace(current.RouterId) == routerID {
			return &pb.Status{
				Success: true,
				Message: fmt.Sprintf("speaker already running (ASN=%d router-id=%s)", asn, routerID),
			}, nil
		}
	}

	if err := os.Setenv("GOBGP_ASN", asnRaw); err != nil {
		log.Printf("AddSpeaker: failed to persist GOBGP_ASN=%s: %v", asnRaw, err)
	}
	if err := os.Setenv("GOBGP_ROUTER_ID", routerID); err != nil {
		log.Printf("AddSpeaker: failed to persist GOBGP_ROUTER_ID=%s: %v", routerID, err)
	}

	_, err = s.Client.StartBgp(ctx, &api.StartBgpRequest{Global: globalCfg})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "address already in use") || strings.Contains(msg, "already started") || strings.Contains(msg, "started") || strings.Contains(msg, "already exists") {
			log.Printf("AddSpeaker: speaker already started or re-applied: ASN=%d router-id=%s; continuing (%v)", asn, routerID, err)
		} else {
			log.Printf("AddSpeaker: failed to start GoBGP speaker: %v", err)
			return &pb.Status{
				Success: false,
				Message: fmt.Sprintf("failed to start BGP speaker: %v", err),
			}, nil
		}
	}

	if s.isSpeakerRunning(ctx) {
		return &pb.Status{
			Success: true,
			Message: fmt.Sprintf("speaker started (ASN=%d router-id=%s)", asn, routerID),
		}, nil
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s.isSpeakerRunning(ctx) {
			s.speakerStarted = true
			log.Printf("BGP speaker ready: ASN=%d router-id=%s", asn, routerID)
			return &pb.Status{
				Success: true,
				Message: fmt.Sprintf("speaker started (ASN=%d router-id=%s)", asn, routerID),
			}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	s.speakerStarted = false
	log.Printf("AddSpeaker: speaker did not become ready within timeout: ASN=%d router-id=%s", asn, routerID)
	return &pb.Status{
		Success: false,
		Message: fmt.Sprintf("speaker is not ready yet (ASN=%d router-id=%s); retry AddSpeaker", asn, routerID),
	}, nil
}

func (s *server) DelSpeaker(ctx context.Context, req *pb.SpeakerRequest) (*pb.Status, error) {
	s.speakerMu.Lock()
	if !s.isSpeakerRunning(ctx) {
		s.speakerStarted = false
		s.speakerMu.Unlock()
		return &pb.Status{
			Success: true,
			Message: "speaker not started",
		}, nil
	}

	if _, err := s.Client.StopBgp(ctx, &api.StopBgpRequest{}); err != nil {
		s.speakerMu.Unlock()
		log.Printf("DelSpeaker: failed to stop GoBGP speaker: %v", err)
		return &pb.Status{
			Success: false,
			Message: fmt.Sprintf("failed to stop BGP speaker: %v", err),
		}, nil
	}

	s.speakerStarted = false
	s.speakerMu.Unlock()

	log.Println("BGP speaker stopped")
	return &pb.Status{
		Success: true,
		Message: "speaker stopped",
	}, nil
}

func (s *server) GetSpeaker(ctx context.Context, req *pb.SpeakerRequest) (*pb.SpeakerResponse, error) {
	status := &pb.SpeakerResponse{
		Speaker: &pb.Speaker{
			Id:          "gobgp",
			Name:        "gobgp",
			Description: "GoBGP EVPN speaker",
		},
	}
	if globalCfg, ok := s.runningSpeaker(ctx); ok {
		s.speakerStarted = true
		status.Speaker.Asn = int32(globalCfg.Asn)
		status.Speaker.RouterId = strings.TrimSpace(globalCfg.RouterId)
		return status, nil
	}
	s.speakerStarted = false
	if asnRaw := strings.TrimSpace(os.Getenv("GOBGP_ASN")); asnRaw != "" {
		if asn, err := strconv.ParseUint(asnRaw, 10, 32); err == nil {
			status.Speaker.Asn = int32(asn)
		}
	}
	status.Speaker.RouterId = strings.TrimSpace(os.Getenv("GOBGP_ROUTER_ID"))
	if !s.speakerStarted {
		status.Speaker.Description = "GoBGP EVPN speaker (stopped)"
	}
	return status, nil
}

func sameSubnet(ip4, peerIP4 net.IP) bool {
	if ip4 == nil || peerIP4 == nil {
		return false
	}
	if peerIP4[0] == 172 && peerIP4[1] == 19 && ip4[0] == 172 && ip4[1] == 19 {
		return true
	}
	if peerIP4[0] == 10 && peerIP4[1] == 244 && ip4[0] == 10 && ip4[1] == 244 {
		return true
	}
	return peerIP4[0] == ip4[0] && peerIP4[1] == ip4[1]
}

func localSourceAddressForPeer(peerAddr string) string {
	peerIP := net.ParseIP(peerAddr)
	if peerIP != nil {
		peerIP4 := peerIP.To4()
		if peerIP4 != nil {
			// Prefer the GoBGP service name when it resolves to the same kind-network
			// as the FRR peer. This keeps the BGP source on the host network instead of
			// the cluster-overlay address of the evpn-mgmt pod.
			for _, candidate := range []string{
				strings.TrimSpace(os.Getenv("GOBGP_LOCAL_ADDRESS")),
				strings.TrimSpace(os.Getenv("GOBGP_ADDR")),
				"gobgp.evpn-system.svc.cluster.local",
				"k8gobgp.k8gobgp-system.svc.cluster.local",
			} {
				if candidate == "" {
					continue
				}
				host := candidate
				if i := strings.Index(host, ":"); i >= 0 {
					host = host[:i]
				}
				if host == "" {
					continue
				}
				ips, err := net.LookupIP(host)
				if err != nil {
					continue
				}
				for _, ip := range ips {
					ip4 := ip.To4()
					if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() {
						continue
					}
					if sameSubnet(ip4, peerIP4) {
						return ip4.String()
					}
				}
			}

			ifaces, err := net.Interfaces()
			if err == nil {
				for _, iface := range ifaces {
					if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
						continue
					}
					addrs, err := iface.Addrs()
					if err != nil {
						continue
					}
					for _, addr := range addrs {
						ipNet, ok := addr.(*net.IPNet)
						if !ok || ipNet == nil || ipNet.IP == nil {
							continue
						}
						ip4 := ipNet.IP.To4()
						if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() {
							continue
						}
						if sameSubnet(ip4, peerIP4) {
							return ip4.String()
						}
					}
				}
			}
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet == nil || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

func (s *server) AddNeighbor(ctx context.Context, req *pb.Neighbor) (*pb.Status, error) {
	if req == nil {
		return &pb.Status{Success: false, Message: "neighbor config is nil"}, nil
	}
	if s.Client == nil {
		return &pb.Status{Success: false, Message: "k8gobgp mode does not expose the GoBGP gRPC peer API; use a legacy gobgpd endpoint or configure the upstream BGPConfiguration CRD instead"}, nil
	}
	if !s.isSpeakerRunning(ctx) {
		return &pb.Status{Success: false, Message: "bgp server hasn't started yet; call AddSpeaker first"}, nil
	}

	localAddress := strings.TrimSpace(os.Getenv("GOBGP_LOCAL_ADDRESS"))
	if localAddress == "" {
		localAddress = strings.TrimSpace(os.Getenv("POD_IP"))
	}
	if localAddress != "" {
		if parsedLocal := net.ParseIP(localAddress); parsedLocal != nil {
			parsedPeer := net.ParseIP(req.NeighborAddress)
			if parsedPeer == nil || !sameSubnet(parsedLocal.To4(), parsedPeer.To4()) {
				localAddress = ""
			}
		} else {
			localAddress = ""
		}
	}
	if localAddress == "" {
		localAddress = localSourceAddressForPeer(req.NeighborAddress)
	}
	if localAddress == "" {
		localAddress = strings.TrimSpace(os.Getenv("GOBGP_ROUTER_ID"))
	}
	if localAddress != "" {
		parsedLocal := net.ParseIP(localAddress)
		parsedPeer := net.ParseIP(req.NeighborAddress)
		if parsedLocal == nil || parsedPeer == nil || !sameSubnet(parsedLocal.To4(), parsedPeer.To4()) {
			localAddress = ""
		}
	}
	if localAddress == "" {
		log.Printf("AddNeighbor: no valid local source address for peer %s; falling back to automatic resolution", req.NeighborAddress)
	}

	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: req.NeighborAddress,
			PeerAsn:         uint32(req.PeerAsn),
		},
		Transport: &api.Transport{
			LocalAddress: localAddress,
		},
		AfiSafis: []*api.AfiSafi{
			{
				Config: &api.AfiSafiConfig{
					Family:  &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
					Enabled: true,
				},
			},
		},
	}

	if _, err := s.Client.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
		log.Printf("AddPeer error: %v", err)
		return &pb.Status{
			Success: false,
			Message: fmt.Sprintf("AddPeer failed: %v", err),
		}, nil
	}

	log.Println("BGP peer successfully added")
	return &pb.Status{Success: true, Message: "ok"}, nil
}

func (s *server) GetNeighbors(ctx context.Context, req *pb.Empty) (*pb.NeighborsList, error) {
	// Call GoBGP to list all peers
	peers, err := s.Client.ListPeer(context.Background(), &api.ListPeerRequest{})
	if err != nil {
		log.Printf("ListPeer error: %v", err)
		return &pb.NeighborsList{
			Neighbors: []*pb.Neighbor{},
		}, nil
	}

	// Convert GoBGP peers to proto Neighbors
	neighbors := make([]*pb.Neighbor, 0)

	for {
		msg, err := peers.Recv()
		if err != nil {
			// End of stream
			break
		}

		if msg.Peer != nil {
			neighbor := &pb.Neighbor{
				NeighborAddress: msg.Peer.Conf.NeighborAddress,
				PeerAsn:         int32(msg.Peer.Conf.PeerAsn),
			}
			neighbors = append(neighbors, neighbor)
		}
	}

	log.Printf("Retrieved %d neighbors from GoBGP", len(neighbors))

	return &pb.NeighborsList{
		Neighbors: neighbors,
	}, nil
}

func logicalSwitchName() string {
	return ovnLogicalSwitchName()
}

func (s *server) AddNetwork(ctx context.Context, req *pb.Network) (*pb.Status, error) {
	if req == nil {
		return &pb.Status{Success: false, Message: "network config is nil"}, nil
	}
	if req.Vni > 0 {
		return s.ProvisionVni(ctx, &pb.VniConfig{Vni: req.Vni, LogicalSwitch: logicalSwitchName(), Enabled: true})
	}
	return &pb.Status{Success: true, Message: "no VNI configured"}, nil
}

func (s *server) ProvisionVni(ctx context.Context, req *pb.VniConfig) (*pb.Status, error) {
	if req == nil {
		return &pb.Status{Success: false, Message: "VNI config is nil"}, nil
	}
	if req.Vni <= 0 {
		return &pb.Status{Success: false, Message: "VNI must be > 0"}, nil
	}
	logicalSwitch := strings.TrimSpace(req.LogicalSwitch)
	if logicalSwitch == "" {
		logicalSwitch = logicalSwitchName()
	}

	s.mu.Lock()
	if s.provisionedVnis == nil {
		s.provisionedVnis = make(map[int32]string)
	}
	s.provisionedVnis[req.Vni] = logicalSwitch
	s.mu.Unlock()
	log.Printf("Provisioned VNI=%d on Logical_Switch=%s enabled=%t", req.Vni, logicalSwitch, req.Enabled)
	return &pb.Status{Success: true, Message: fmt.Sprintf("VNI %d provisioned on %s", req.Vni, logicalSwitch)}, nil
}

func (s *server) DeleteVni(ctx context.Context, req *pb.VniRequest) (*pb.Status, error) {
	if req == nil || req.Vni <= 0 {
		return &pb.Status{Success: false, Message: "VNI must be > 0"}, nil
	}
	s.mu.Lock()
	if s.provisionedVnis != nil {
		delete(s.provisionedVnis, req.Vni)
	}
	s.mu.Unlock()
	log.Printf("Deleted VNI=%d from provisioning set", req.Vni)
	return &pb.Status{Success: true, Message: fmt.Sprintf("VNI %d deleted", req.Vni)}, nil
}

func (s *server) GetVnis(ctx context.Context, req *pb.Empty) (*pb.VnisList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vnis := make([]int32, 0, len(s.provisionedVnis))
	for vni := range s.provisionedVnis {
		vnis = append(vnis, vni)
	}
	return &pb.VnisList{Vni: vnis}, nil
}

func (s *server) isVniProvisioned(vni int32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provisionedVnis == nil {
		return false
	}
	_, ok := s.provisionedVnis[vni]
	return ok
}

func (s *server) AddClient(ctx context.Context, req *pb.Client) (*pb.Status, error) {
	if req == nil {
		return &pb.Status{Success: false, Message: "client is nil"}, nil
	}
	if !s.isSpeakerRunning(ctx) {
		return &pb.Status{Success: false, Message: "bgp server hasn't started yet; call AddSpeaker first"}, nil
	}

	// Parse RD string format "IP:number"
	rdParts := strings.Split(req.Rd, ":")
	rdAdmin := req.Rd
	rdAssigned := uint32(req.Label)

	if len(rdParts) == 2 {
		rdAdmin = rdParts[0]
		if assignedVal, err := strconv.ParseUint(rdParts[1], 10, 32); err == nil {
			rdAssigned = uint32(assignedVal)
		}
	}

	rd := &api.RouteDistinguisher{Rd: &api.RouteDistinguisher_IpAddress{
		IpAddress: &api.RouteDistinguisherIPAddress{
			Admin:    rdAdmin,
			Assigned: rdAssigned,
		},
	}}

	nlri := &api.NLRI{
		Nlri: &api.NLRI_EvpnMacadv{
			EvpnMacadv: &api.EVPNMACIPAdvertisementRoute{
				Rd: rd,
				//Unspecified ESI
				Esi: &api.EthernetSegmentIdentifier{
					Type:  0,
					Value: make([]byte, 10),
				},
				//Unspecified Ethernet Tag
				EthernetTag: 0,
				MacAddress:  req.Mac,
				IpAddress:   req.Ip,
				Labels:      []uint32{uint32(req.Label)},
			},
		},
	}

	attrs := []*api.Attribute{
		{
			Attr: &api.Attribute_Origin{Origin: &api.OriginAttribute{
				Origin: 0,
			}},
		},
		{
			//Unspecified NextHop
			Attr: &api.Attribute_NextHop{NextHop: &api.NextHopAttribute{
				NextHop: "0.0.0.0",
			}},
		},
	}

	if _, err := s.Client.AddPath(context.TODO(), &api.AddPathRequest{
		//TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Path: &api.Path{
			Nlri:   nlri,
			Pattrs: attrs,
			Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
		},
	}); err != nil {
		log.Printf("failed to run AddPath call with error: %v", err)
		return &pb.Status{
			Success: false,
			Message: fmt.Sprintf("AddPath failed: %v", err),
		}, nil
	}

	log.Println("EVPN MAC/IP route successfully added")

	return &pb.Status{
		Success: true,
		Message: "ok",
	}, nil
}

func (s *server) GetClients(ctx context.Context, req *pb.Empty) (*pb.ClientsList, error) {
	// Query GoBGP for all EVPN MAC/IP advertisement routes
	listReq := &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
	}

	stream, err := s.Client.ListPath(context.Background(), listReq)
	if err != nil {
		log.Printf("ListPath error: %v", err)
		return &pb.ClientsList{Clients: []*pb.Client{}}, nil
	}

	clients := make([]*pb.Client, 0)

	for {
		msg, err := stream.Recv()
		if err != nil {
			// End of stream
			break
		}

		if msg.Destination != nil && msg.Destination.Paths != nil {
			// Check each path in the destination
			for _, path := range msg.Destination.Paths {
				if path.Nlri != nil {
					// Check if this is an EVPN MAC/IP advertisement
					if evpnMac := path.Nlri.GetEvpnMacadv(); evpnMac != nil {
						client := &pb.Client{
							Mac: evpnMac.MacAddress,
							Ip:  evpnMac.IpAddress,
						}

						// Extract label if available
						if len(evpnMac.Labels) > 0 {
							client.Label = int32(evpnMac.Labels[0])
						}

						// Extract RD (Route Distinguisher) if available
						if evpnMac.Rd != nil {
							if rdIp := evpnMac.Rd.GetIpAddress(); rdIp != nil {
								client.Rd = rdIp.Admin + ":" + strconv.FormatUint(uint64(rdIp.Assigned), 10)
							}
						}

						clients = append(clients, client)
					}
				}
			}
		}
	}

	log.Printf("Retrieved %d clients from GoBGP", len(clients))

	return &pb.ClientsList{
		Clients: clients,
	}, nil
}

func (s *server) DelClient(ctx context.Context, req *pb.ClientRequest) (*pb.Status, error) {
	log.Printf("DelClient called with id=%s", req.Id)

	// List all EVPN paths and delete those matching the IP or MAC (id field)
	listReq := &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
	}
	stream, err := s.Client.ListPath(context.Background(), listReq)
	if err != nil {
		return &pb.Status{Success: false, Message: fmt.Sprintf("ListPath error: %v", err)}, nil
	}

	deleted := 0
	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		if msg.Destination == nil {
			continue
		}
		for _, path := range msg.Destination.Paths {
			if path.Nlri == nil {
				continue
			}
			evpnMac := path.Nlri.GetEvpnMacadv()
			if evpnMac == nil {
				continue
			}
			if evpnMac.IpAddress != req.Id && evpnMac.MacAddress != req.Id {
				continue
			}
			if _, delErr := s.Client.DeletePath(context.Background(), &api.DeletePathRequest{
				Path: &api.Path{
					Nlri:   path.Nlri,
					Pattrs: path.Pattrs,
					Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
				},
			}); delErr != nil {
				log.Printf("DeletePath error for %s: %v", req.Id, delErr)
			} else {
				deleted++
				log.Printf("Deleted EVPN path for %s (MAC=%s IP=%s)", req.Id, evpnMac.MacAddress, evpnMac.IpAddress)
			}
		}
	}

	log.Printf("DelClient: removed %d EVPN path(s) for id=%s", deleted, req.Id)
	return &pb.Status{Success: true, Message: fmt.Sprintf("deleted %d path(s)", deleted)}, nil
}

func main() {
	gobgpAddr := gobgpGRPCAddr()
	mode := gobgpMode()
	var client api.GoBgpServiceClient
	var conn *grpc.ClientConn
	var err error

	if shouldUseGoBgpGrpcClient(mode, gobgpAddr) {
		conn, err = grpc.NewClient(
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
		client = api.NewGoBgpServiceClient(conn)
		if mode == "k8gobgp" {
			log.Printf("GOBGP_MODE=k8gobgp: enabling GoBGP gRPC compatibility path via %q", gobgpAddr)
		}
	} else {
		log.Printf("GOBGP_MODE=k8gobgp: no usable external GoBGP gRPC endpoint configured (addr=%q). The evpn-mgmt API remains active, but speaker/peer operations must be managed via BGPConfiguration CRDs or by setting a TCP gRPC endpoint (for example k8gobgp.k8gobgp-system.svc.cluster.local:50051).", gobgpAddr)
	}

	lis, err := net.Listen("tcp", ":50052")
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer()

	pb.RegisterEvpnServiceServer(grpcServer, &server{
		Client:          client,
		provisionedVnis: map[int32]string{1000: logicalSwitchName()},
	})

	log.Println("gRPC server started on :50052")

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
