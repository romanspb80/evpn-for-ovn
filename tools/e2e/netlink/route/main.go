package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type result struct {
	PathUUID string `json:"path_uuid,omitempty"`
	Action   string `json:"action"`
}

func main() {
	action := flag.String("action", "", "advertise or withdraw")
	endpointID := flag.String("endpoint-id", "", "unique local endpoint identifier")
	incarnation := flag.String("incarnation", "", "unique endpoint incarnation")
	networkID := flag.String("network-id", "", "test network identifier")
	mac := flag.String("mac", "", "endpoint MAC address")
	ip := flag.String("ip", "", "endpoint IPv4 address")
	vni := flag.Uint("vni", 0, "EVPN VNI")
	rd := flag.String("rd", "", "route distinguisher")
	rt := flag.String("rt", "", "route target")
	localVTEP := flag.String("local-vtep", "", "handler underlay address")
	pathUUID := flag.String("path-uuid", "", "exact UUID returned by advertise")
	flag.Parse()

	if *action != "advertise" && *action != "withdraw" {
		fatal(fmt.Errorf("action must be advertise or withdraw"))
	}
	if *vni == 0 || *vni > 16777215 {
		fatal(fmt.Errorf("VNI must be in 1..16777215"))
	}
	if net.ParseIP(*localVTEP) == nil {
		fatal(fmt.Errorf("local VTEP must be an IP address"))
	}
	endpoint := evpn.LocalEndpoint{
		NetworkID:          *networkID,
		EndpointID:         *endpointID,
		Incarnation:        *incarnation,
		MACAddress:         strings.ToLower(*mac),
		IPAddresses:        []string{*ip},
		VNI:                uint32(*vni),
		RouteDistinguisher: *rd,
		RouteTargets:       []string{*rt},
		LocalVTEP:          *localVTEP,
		NodeName:           "netlink-e2e",
		Chassis:            "netlink-e2e",
		Advertise:          true,
	}
	if err := endpoint.Validate(); err != nil {
		fatal(err)
	}

	address := strings.TrimSpace(os.Getenv("GOBGP_ENDPOINT"))
	if address == "" {
		fatal(fmt.Errorf("GOBGP_ENDPOINT is required"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal(fmt.Errorf("connect GoBGP: %w", err))
	}
	defer conn.Close()

	adapter := &evpn.GoBGPType2Adapter{Client: api.NewGoBgpServiceClient(conn)}
	out := result{Action: *action}
	switch *action {
	case "advertise":
		out.PathUUID, err = adapter.Add(ctx, evpn.Type2Route{Endpoint: endpoint})
	case "withdraw":
		if strings.TrimSpace(*pathUUID) == "" {
			fatal(fmt.Errorf("exact path UUID is required for withdrawal"))
		}
		err = adapter.Delete(ctx, evpn.OwnedType2Route{
			Endpoint: endpoint,
			PathUUID: *pathUUID,
		})
	}
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
