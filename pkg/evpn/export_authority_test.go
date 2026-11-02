package evpn

import "testing"

func TestExporterAuthorityExclusive(t *testing.T) {
	first, err := AcquireExporterAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireExporterAuthority(); err == nil {
		second.Close()
		t.Fatal("concurrent exporter accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireExporterAuthority()
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestExporterSourceSelection(t *testing.T) {
	if (EVPNNetworkConfig{}).ExportSource() != "cni" {
		t.Fatal("legacy default changed")
	}
	cfg := ExporterConfig{Enabled: true, DryRun: true, NodeName: "node", LocalChassis: "chassis",
		ASN: 64512, RouterID: "192.0.2.1", GRPCEndpoint: "127.0.0.1:50051",
		Networks: []EVPNNetworkConfig{{NetworkID: "n", LogicalSwitch: "ls", VNI: 100,
			LocalVTEP: "192.0.2.1", RouteDistinguisher: "64512:100", RouteTargets: []string{"64512:100"},
			ExporterSource: "ovn-netlink", BridgeDevice: "br-test", VXLANDevice: "vx-test", AdvertiseDevice: "adv-test"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	framework := cfg
	framework.Networks = append([]EVPNNetworkConfig(nil), cfg.Networks...)
	framework.Networks[0].ExporterSource = "framework"
	framework.Networks[0].BridgeDevice = ""
	framework.Networks[0].VXLANDevice = ""
	framework.Networks[0].AdvertiseDevice = ""
	if err := framework.Validate(); err != nil {
		t.Fatalf("framework OVN observer configuration rejected: %v", err)
	}
	framework.Networks[0].BridgeDevice = "br-test"
	if err := framework.Validate(); err == nil {
		t.Fatal("framework observer accepted Netlink devices")
	}
	for _, source := range []string{"cni", "mixed", "OVN-Netlink"} {
		cfg.Networks[0].ExporterSource = source
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid native authority %s accepted", source)
		}
	}
}
