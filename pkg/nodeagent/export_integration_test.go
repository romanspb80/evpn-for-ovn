package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestCNIExporterRIB_Live(t *testing.T) {
	if os.Getenv("EVPN_EXPORT_LIVE") != "disposable" {
		t.Skip("requires disposable Multus/k8gobgp runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	value, err := os.ReadFile("/run/evpn-node-agent/export-status.json")
	if err != nil {
		t.Fatal(err)
	}
	var state ExportStatus
	if err := json.Unmarshal(value, &state); err != nil {
		t.Fatal(err)
	}
	if time.Since(state.Time) > 12*time.Second {
		t.Fatal("exporter status stale")
	}
	conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	adapter := &evpn.GoBGPType2Adapter{Client: api.NewGoBgpServiceClient(conn)}
	if os.Getenv("EVPN_EXPORT_STAGE") == "seed-conflict" {
		if len(state.Result.Applied) == 0 {
			t.Fatal("need prior confirmed endpoint for test conflict")
		}
		ep := state.Result.Applied[0].Endpoint
		ep.RouteTargets = []string{"64512:65000"}
		route, err := evpn.BuildType2Route(ep)
		if err != nil {
			t.Fatal(err)
		}
		id, err := adapter.Add(ctx, route)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("test-only conflicting route, distinct owner UUID=%s endpoint=%s RT=64512:65000", id, ep.EndpointID)
		return
	}
	if os.Getenv("EVPN_EXPORT_STAGE") == "seed-foreign" {
		foreign := evpn.LocalEndpoint{NetworkID: "foreign", EndpointID: "foreign", Incarnation: "other-owner", MACAddress: "0a:58:f0:00:00:0a", IPAddresses: []string{"10.244.240.10"}, VNI: 100, LocalVTEP: os.Getenv("EVPN_EXPORT_VTEP"), RouteDistinguisher: "64512:32999", RouteTargets: []string{"64512:100"}, Advertise: true}
		route, err := evpn.BuildType2Route(foreign)
		if err != nil {
			t.Fatal(err)
		}
		id, err := adapter.Add(ctx, route)
		if err != nil {
			t.Fatal(err)
		}
		value, err := json.Marshal(evpn.OwnedType2Route{Endpoint: foreign, PathUUID: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/run/evpn-node-agent/foreign.json", value, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("foreign UUID=%s", id)
		return
	}
	if os.Getenv("EVPN_EXPORT_STAGE") == "delete-foreign" {
		value, err := os.ReadFile("/run/evpn-node-agent/foreign.json")
		if err != nil {
			t.Fatal(err)
		}
		var owned evpn.OwnedType2Route
		if err := json.Unmarshal(value, &owned); err != nil {
			t.Fatal(err)
		}
		if err := adapter.Delete(ctx, owned); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantErr := os.Getenv("EVPN_EXPORT_ERROR")
	if wantErr != "" {
		if !strings.Contains(state.Error, wantErr) {
			t.Fatalf("expected error %q, got %q", wantErr, state.Error)
		}
	} else if state.Error != "" {
		t.Fatal(state.Error)
	}
	want, err := strconv.Atoi(os.Getenv("EVPN_EXPORT_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := strconv.Atoi(os.Getenv("EVPN_EXPORT_DESIRED"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Result.Desired) != desired {
		t.Fatalf("desired=%d want=%d state=%+v", len(state.Result.Desired), desired, state)
	}
	for _, endpoint := range state.Result.Desired {
		ip, mac := "10.244.240.10", "0a:58:f0:00:00:0a"
		if endpoint.MACAddress == "0a:58:f0:00:00:0b" {
			ip, mac = "10.244.240.11", "0a:58:f0:00:00:0b"
		}
		if endpoint.MACAddress != mac || len(endpoint.IPAddresses) != 1 || endpoint.IPAddresses[0] != ip ||
			endpoint.Incarnation == "" || endpoint.NetworkID != "n24" || !strings.HasPrefix(endpoint.EndpointID, "n24|s|") ||
			endpoint.RouteDistinguisher != "64512:100" || endpoint.VNI != 100 || endpoint.LocalVTEP != os.Getenv("EVPN_EXPORT_VTEP") ||
			len(endpoint.RouteTargets) != 1 || endpoint.RouteTargets[0] != "64512:100" {
			t.Fatalf("incorrect observed endpoint: %+v", endpoint)
		}
		route, err := evpn.BuildType2Route(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(struct {
			NLRI       interface{}
			Attributes []bgp.PathAttributeInterface
		}{route.NLRI, route.PathAttributes})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("desired Type-2 dry-run=%v endpoint=%s incarnation=%s wire=%s", state.Result.DryRun, endpoint.EndpointID, endpoint.Incarnation, wire)
	}
	applied := want
	if value := os.Getenv("EVPN_EXPORT_APPLIED"); value != "" {
		applied, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !state.Result.DryRun && len(state.Result.Applied) != applied {
		t.Fatalf("confirmed applied=%d want=%d", len(state.Result.Applied), applied)
	}
	if os.Getenv("EVPN_EXPORT_STAGE") == "unconfigured" || os.Getenv("EVPN_EXPORT_STAGE") == "restarted-unconfigured" {
		global, err := adapter.Client.GetBgp(ctx, &api.GetBgpRequest{})
		if err != nil || global.GetGlobal().GetAsn() != 0 || len(state.Result.Applied) != applied {
			t.Fatalf("expected actual unconfigured speaker with no receipts: %v %v %+v", global, err, state)
		}
		if _, err := adapter.Snapshot(ctx); err == nil || !strings.Contains(err.Error(), "hasn't started") {
			t.Fatalf("unexpected unconfigured ListPath result: %v", err)
		}
		t.Log("actual speaker ASN=0, ListPath rejects unstarted server; exporter readiness blocks mutations")
		return
	}
	paths, err := adapter.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ownCount, foreignCount := 0, 0
	for _, path := range paths {
		nlri := path.NLRI.RouteTypeData.(*bgp.EVPNMacIPAdvertisementRoute)
		if nlri.RD.String() == "64512:32999" {
			foreignCount++
			continue
		}
		if nlri.RD.String() != "64512:100" {
			t.Fatalf("unexpected RIB prefix: %v", path.NLRI)
		}
		ownCount++
		if os.Getenv("EVPN_EXPORT_UNCLAIMED") == "yes" {
			t.Logf("unclaimed namespace path preserved, NOT adopted: NLRI=%s attributes=%v", path.NLRI, path.PathAttributes)
			continue
		}
		matched := false
		for _, owned := range state.Result.Applied {
			route, err := evpn.BuildType2Route(owned.Endpoint)
			if err != nil {
				t.Fatal(err)
			}
			want, err := route.NLRI.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			got, err := path.NLRI.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want, got) {
				continue
			}
			nh, rt := false, false
			for _, attr := range path.PathAttributes {
				switch a := attr.(type) {
				case *bgp.PathAttributeMpReachNLRI:
					nh = a.Nexthop.String() == owned.Endpoint.LocalVTEP
				case *bgp.PathAttributeExtendedCommunities:
					rt = len(a.Value) == 1 && a.Value[0].String() == "64512:100"
				}
			}
			if !nh || !rt || nlri.ETag != 0 || len(nlri.Labels) != 1 || nlri.Labels[0] != 100 || owned.PathUUID == "" {
				t.Fatal("incorrect Type-2 attributes/receipt")
			}
			matched = true
			t.Logf("actual RIB UUID-receipt=%s RD=%s MAC=%s IP=%s label=%v next-hop=%s RT=64512:100 endpoint=%s incarnation=%s", owned.PathUUID, nlri.RD, nlri.MacAddress, nlri.IPAddress, nlri.Labels, owned.Endpoint.LocalVTEP, owned.Endpoint.EndpointID, owned.Endpoint.Incarnation)
		}
		if !matched {
			t.Fatal("RIB path has no matching confirmed applied endpoint")
		}
	}
	expectedForeign := 0
	if os.Getenv("EVPN_EXPORT_FOREIGN") == "yes" {
		expectedForeign = 1
	}
	if ownCount != want || foreignCount != expectedForeign {
		t.Fatalf("RIB owned=%d foreign=%d, want %d/%d", ownCount, foreignCount, want, expectedForeign)
	}
	t.Logf("status=%+v RIB owned=%d foreign=%d", state, ownCount, foreignCount)
}
