package nodeagent

import (
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

func TestCNIExporterRecoveryIdentityBinding(t *testing.T) {
	domain := ownership.Domain{InstallationUID: "installation", NodeUID: "worker-a"}
	cfg := evpn.ExporterConfig{Enabled: true, NodeName: "worker-a", Recovery: &evpn.ExportRecoveryConfig{Domain: domain}}
	server := &CNIServer{continuity: &cniContinuity{nodeName: "worker-a", domain: domain}}
	binding := &cniExportBinding{server: server}
	if err := binding.prepare(cfg); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*evpn.ExporterConfig){
		func(c *evpn.ExporterConfig) { c.NodeName = "worker-b" },
		func(c *evpn.ExporterConfig) { c.Recovery.Domain.NodeUID = "worker-b" },
		func(c *evpn.ExporterConfig) { c.Recovery.Domain.InstallationUID = "other" },
	} {
		bad := cfg
		recovery := *cfg.Recovery
		bad.Recovery = &recovery
		mutate(&bad)
		if err := binding.prepare(bad); err == nil {
			t.Fatal("mismatched exporter identity accepted")
		}
	}
	server.continuity = nil
	if err := binding.prepare(cfg); err == nil {
		t.Fatal("missing CNI continuity accepted")
	}
}
