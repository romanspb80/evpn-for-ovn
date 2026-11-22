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

	"github.com/osrg/gobgp/v4/api"

	pb "github.com/romanspb80/evpn-for-ovn/gen/evpn_mgmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type server struct {
	pb.UnimplementedEvpnServiceServer
	Client          api.GoBgpServiceClient
	mu              sync.Mutex
	provisionedVnis map[int32]string
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

func (s *server) AddSpeaker(ctx context.Context, req *pb.Speaker) (*pb.Status, error) {
	return nil, status.Error(codes.PermissionDenied, "legacy speaker management is disabled; use k8gobgp")
}

func (s *server) DelSpeaker(ctx context.Context, req *pb.SpeakerRequest) (*pb.Status, error) {
	return nil, status.Error(codes.PermissionDenied, "legacy speaker management is disabled; use k8gobgp")
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
	return nil, status.Error(codes.PermissionDenied, "legacy peer configuration is disabled; use the authorized speaker configuration authority")
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
	return nil, status.Error(codes.PermissionDenied, "legacy Type-2 route writes are disabled; use an authorized exporter")
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
	return nil, status.Error(codes.PermissionDenied, "legacy Type-2 route writes are disabled; use the route owner for withdrawal")
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
