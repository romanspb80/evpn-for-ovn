package nodeagent

import (
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

func TestExporterAuthorityReload(t *testing.T) {
	old := evpn.ExporterConfig{Networks: []evpn.EVPNNetworkConfig{{NetworkID: "n"}}}
	next := evpn.ExporterConfig{Networks: []evpn.EVPNNetworkConfig{{NetworkID: "n", ExporterSource: "cni"}}}
	if err := validateExporterAuthorityReload(old, next); err != nil {
		t.Fatal(err)
	}
	next.Networks[0].ExporterSource = "ovn-netlink"
	if err := validateExporterAuthorityReload(old, next); err == nil {
		t.Fatal("live exporter handoff accepted")
	}
	old = next
	next = evpn.ExporterConfig{}
	if err := validateExporterAuthorityReload(old, next); err == nil {
		t.Fatal("native ownership removal accepted")
	}
}
