package evpn

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Read-only verification of the actual Node-Agent's routes, not a test writer.
func TestOperatorManagedType2Wire_Live(t *testing.T) {
	if os.Getenv("EVPN_OPERATOR_TYPE2_LIVE") != "disposable" {
		t.Skip("requires operator-managed disposable Type-2 environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	adapter := &GoBGPType2Adapter{Client: api.NewGoBgpServiceClient(conn)}
	paths, err := adapter.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	localVTEP, remoteVTEP := os.Getenv("EVPN_OPERATOR_LOCAL_VTEP"), os.Getenv("EVPN_OPERATOR_REMOTE_VTEP")
	if localVTEP == "" || remoteVTEP == "" {
		t.Fatal("explicit VTEP expectations required")
	}
	seen := make(map[string]int)
	for _, path := range paths {
		route := path.NLRI
		if route == nil {
			t.Fatal("non-Type-2 path in Type-2 snapshot")
		}
		mac := route.MAC
		switch mac {
		case "0a:58:f0:00:00:0a", "0a:58:f0:00:00:0b":
			ip := "10.244.240.10"
			if mac == "0a:58:f0:00:00:0b" {
				ip = "10.244.240.11"
			}
			macOnly := route.IP == ""
			ips := []string{ip}
			key := mac + "|" + ip
			if macOnly {
				ips = nil
				key = mac + "|"
			}
			want := LocalEndpoint{NetworkID: "n24", EndpointID: mac, Incarnation: "wire-expectation",
				MACAddress: mac, IPAddresses: ips, MACOnly: macOnly, VNI: 1000, RouteDistinguisher: localVTEP + ":1000",
				RouteTargets: []string{"64512:1000"}, LocalVTEP: localVTEP, Advertise: true}
			if _, err := BuildType2Route(want); err != nil {
				t.Fatalf("invalid wire assertion expectation: %v", err)
			}
			if !path.Local || !ribPathMatchesEndpoint(path, want) {
				t.Fatalf("local NLRI/origin/RD/RT/VNI/next-hop mismatch: %+v", path)
			}
			if route.EthernetTag != 0 {
				t.Fatalf("unexpected Ethernet Tag: %d", route.EthernetTag)
			}
			seen[key]++
		case "0a:58:f0:01:00:0c":
			if path.Local || route.RD != "198.18.0.3:1000" || route.EthernetTag != 0 ||
				len(route.Labels) != 1 || route.Labels[0] != 1000 || path.PathAttributes.NextHop != remoteVTEP {
				t.Fatalf("remote NLRI/RD/VNI/next-hop/origin-location mismatch: %+v", path)
			}
			ip := route.IP
			if ip != "" && ip != "10.244.240.12" {
				t.Fatalf("unexpected remote IP: %s", ip)
			}
			rt := false
			for _, community := range path.PathAttributes.ExtendedCommunities {
				if community.Kind == Type2RouteTargetCommunity && community.Value == "64512:1000" {
					rt = true
				}
			}
			if !rt {
				t.Fatal("remote import RT absent")
			}
			seen[mac+"|"+ip]++
		default:
			t.Fatalf("unexpected Type-2 endpoint in isolated speaker: %s", mac)
		}
	}
	for _, key := range []string{"0a:58:f0:00:00:0a|", "0a:58:f0:00:00:0a|10.244.240.10", "0a:58:f0:00:00:0b|", "0a:58:f0:00:00:0b|10.244.240.11", "0a:58:f0:01:00:0c|", "0a:58:f0:01:00:0c|10.244.240.12"} {
		if seen[key] != 1 {
			t.Fatalf("route missing or duplicated: %s count=%d", key, seen[key])
		}
	}
	t.Log("real local and learned Type-2 MAC/IP, RD, RT, Ethernet Tag, VNI, next-hop, origin and duplicate counts verified")
}
