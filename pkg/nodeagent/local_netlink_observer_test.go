package nodeagent

import (
	"context"
	"errors"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

func TestNativeOVNPortProvenanceAndCNIIsolation(t *testing.T) {
	for _, cause := range []string{"ready", "down", "foreign-chassis", "wrong-datapath", "wrong-mapping", "stale-mac", "disconnected", "dynamic"} {
		t.Run(cause, func(t *testing.T) {
			cni, source := readyExportObserver(t)
			cfg := &cni.config.Networks[0]
			cfg.ExporterSource, cfg.BridgeDevice, cfg.VXLANDevice, cfg.AdvertiseDevice = "ovn-netlink", "br-test", "vx-test", "adv-test"
			source.data.Switches[0].OtherConfig = map[string]string{
				"dynamic-routing-vni": "100", "dynamic-routing-bridge-ifname": "br-test",
				"dynamic-routing-vxlan-ifname": "vx-test", "dynamic-routing-advertise-ifname": "adv-test",
				"dynamic-routing-redistribute": "fdb,ip",
			}
			pb := source.data.Bindings[0]
			pb.UUID, pb.Datapath, pb.MAC = "pb-uuid", "dp-uuid", append([]string(nil), source.data.LSPs[0].Addresses...)
			source.data.Datapaths = []*exportDatapath{{UUID: "dp-uuid", ExternalIDs: map[string]string{"logical-switch": "ls-uuid"}}}
			if desired, err := cni.DesiredLocalEndpoints(context.Background()); err != nil || len(desired) != 0 {
				t.Fatalf("CNI exported native-authority endpoint: %+v %v", desired, err)
			}
			switch cause {
			case "down":
				up := false
				pb.Up = &up
			case "foreign-chassis":
				chassis := "other"
				pb.Chassis = &chassis
			case "wrong-datapath":
				pb.Datapath = "other"
			case "wrong-mapping":
				source.data.Switches[0].OtherConfig["dynamic-routing-vni"] = "200"
			case "stale-mac":
				pb.MAC = []string{"02:00:00:00:00:99 10.77.0.11"}
			case "disconnected":
				source.err = errors.New("SB read incomplete")
			case "dynamic":
				pb.MAC, source.data.LSPs[0].Addresses = []string{"dynamic"}, []string{"dynamic"}
			}
			observer := &nativeOVNPorts{source: source, config: cni.config}
			desired, err := observer.DesiredLocalEndpoints(context.Background())
			if cause == "ready" {
				if err != nil || len(desired) != 1 || desired[0].MACOnly || len(desired[0].IPAddresses) != 1 {
					t.Fatalf("native provenance IP allowlist or identity missing: %+v %v", desired, err)
				}
				if desired[0].Incarnation != "pb-uuid" || desired[0].EndpointID != "ovn-netlink/lsp-uuid" {
					t.Fatal("native port incarnation mismatch")
				}
			} else if cause == "foreign-chassis" {
				if err != nil || len(desired) != 0 {
					t.Fatal("remote PB reflected into local export")
				}
			} else if err == nil {
				t.Fatalf("ambiguous local provenance accepted: %s %+v", cause, desired)
			}
		})
	}
}

type failingNativeObserver struct{}

func (failingNativeObserver) DesiredLocalEndpoints(context.Context) ([]evpn.LocalEndpoint, error) {
	return nil, errors.New("native dump interrupted")
}

func TestSelectedObserverFailsClosed(t *testing.T) {
	cni, _ := readyExportObserver(t)
	if desired, err := (selectedEndpointObserver{cni: cni, native: failingNativeObserver{}}).DesiredLocalEndpoints(context.Background()); err == nil || desired != nil {
		t.Fatal("partial network snapshot accepted")
	}
}
