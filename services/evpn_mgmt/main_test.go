package main

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/osrg/gobgp/v4/api"
	pb "github.com/romanspb80/evpn-for-ovn/gen/evpn_mgmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type rejectingPeerProbe struct {
	api.GoBgpServiceClient
	calls int
}

type speakerMutationProbe struct {
	api.GoBgpServiceClient
	calls int
}

type routeMutationProbe struct {
	api.GoBgpServiceClient
	adds    int
	deletes int
}

func (p *routeMutationProbe) AddPath(context.Context, *api.AddPathRequest, ...grpc.CallOption) (*api.AddPathResponse, error) {
	p.adds++
	return &api.AddPathResponse{}, nil
}

func (p *routeMutationProbe) DeletePath(context.Context, *api.DeletePathRequest, ...grpc.CallOption) (*api.DeletePathResponse, error) {
	p.deletes++
	return &api.DeletePathResponse{}, nil
}

func (p *speakerMutationProbe) GetBgp(context.Context, *api.GetBgpRequest, ...grpc.CallOption) (*api.GetBgpResponse, error) {
	p.calls++
	return &api.GetBgpResponse{Global: &api.Global{Asn: 65000, RouterId: "192.0.2.1"}}, nil
}

func (p *speakerMutationProbe) StartBgp(context.Context, *api.StartBgpRequest, ...grpc.CallOption) (*api.StartBgpResponse, error) {
	p.calls++
	return &api.StartBgpResponse{}, nil
}

func (p *speakerMutationProbe) StopBgp(context.Context, *api.StopBgpRequest, ...grpc.CallOption) (*api.StopBgpResponse, error) {
	p.calls++
	return &api.StopBgpResponse{}, nil
}

func TestLegacySpeakerMutationIsDisabled(t *testing.T) {
	t.Setenv("GOBGP_ASN", "64512")
	t.Setenv("GOBGP_ROUTER_ID", "192.0.2.10")
	probe := &speakerMutationProbe{}
	srv := &server{Client: probe, speakerStarted: true}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterEvpnServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	conn, err := grpc.NewClient("passthrough:///management", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewEvpnServiceClient(conn)
	for _, request := range []*pb.Speaker{{}, {Asn: 65001, RouterId: "192.0.2.20"}} {
		response, err := client.AddSpeaker(context.Background(), request)
		if response != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("AddSpeaker(%v) = %v, %v; want PermissionDenied", request, response, err)
		}
	}
	for _, request := range []*pb.SpeakerRequest{{}, {Id: "gobgp"}} {
		response, err := client.DelSpeaker(context.Background(), request)
		if response != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("DelSpeaker(%v) = %v, %v; want PermissionDenied", request, response, err)
		}
	}
	if probe.calls != 0 || !srv.speakerStarted || os.Getenv("GOBGP_ASN") != "64512" || os.Getenv("GOBGP_ROUTER_ID") != "192.0.2.10" {
		t.Fatalf("rejected speaker operations changed state: calls=%d started=%t ASN=%q routerID=%q",
			probe.calls, srv.speakerStarted, os.Getenv("GOBGP_ASN"), os.Getenv("GOBGP_ROUTER_ID"))
	}
}

func (p *rejectingPeerProbe) AddPeer(context.Context, *api.AddPeerRequest, ...grpc.CallOption) (*api.AddPeerResponse, error) {
	p.calls++
	return &api.AddPeerResponse{}, nil
}

func TestLegacyNeighborMutationIsDisabled(t *testing.T) {
	probe := &rejectingPeerProbe{}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterEvpnServiceServer(grpcServer, &server{Client: probe})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	conn, err := grpc.NewClient("passthrough:///management", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewEvpnServiceClient(conn)
	for _, request := range []*pb.Neighbor{{}, {NeighborAddress: "192.0.2.1", PeerAsn: 65001}} {
		response, err := client.AddNeighbor(context.Background(), request)
		if response != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("AddNeighbor(%v) = %v, %v; want PermissionDenied", request, response, err)
		}
	}
	response, err := client.DelNeighbor(context.Background(), &pb.NeighborRequest{Id: "192.0.2.1"})
	if response != nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("DelNeighbor = %v, %v; want Unimplemented", response, err)
	}
	if probe.calls != 0 {
		t.Fatalf("unexpected AddPeer calls: %d", probe.calls)
	}
}

func TestLegacyRouteWritesAreDisabled(t *testing.T) {
	probe := &routeMutationProbe{}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterEvpnServiceServer(grpcServer, &server{Client: probe})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient("passthrough:///management", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewEvpnServiceClient(conn)
	ctx := context.Background()

	for _, request := range []*pb.Client{
		{},
		{Mac: "02:00:00:00:00:01", Ip: "192.0.2.1", Rd: "192.0.2.2:1000", Label: 1000},
	} {
		response, err := client.AddClient(ctx, request)
		if response != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("AddClient(%v) = %v, %v; want PermissionDenied", request, response, err)
		}
	}
	for _, id := range []string{"", "192.0.2.1", "02:00:00:00:00:01"} {
		response, err := client.DelClient(ctx, &pb.ClientRequest{Id: id})
		if response != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("DelClient(%q) = %v, %v; want PermissionDenied", id, response, err)
		}
		if probe.adds != 0 || probe.deletes != 0 {
			t.Fatalf("rejected route requests reached GoBGP: adds=%d deletes=%d", probe.adds, probe.deletes)
		}
	}

	provisioned, err := client.ProvisionVni(ctx, &pb.VniConfig{Vni: 1000, LogicalSwitch: "test"})
	if err != nil || !provisioned.GetSuccess() {
		t.Fatalf("ProvisionVni = %v, %v", provisioned, err)
	}
	vnis, err := client.GetVnis(ctx, &pb.Empty{})
	if err != nil || len(vnis.GetVni()) != 1 || vnis.GetVni()[0] != 1000 {
		t.Fatalf("GetVnis = %v, %v", vnis, err)
	}
}
