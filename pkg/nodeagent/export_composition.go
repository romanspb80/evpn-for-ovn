package nodeagent

import (
	"fmt"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent/ovnexport"
)

type exportPlatformBinding interface {
	beginCycle()
	prepare(evpn.ExporterConfig) error
	buildObserver(evpn.ExporterConfig, liveExportState) (evpn.EndpointObserver, *evpn.LocalOVNNetlinkObserver, error)
	update(evpn.ExporterConfig)
	wrapRecoveryApplier(evpn.Type2RouteApplier) evpn.Type2RouteApplier
	observations() []ExportObservation
}

type cniExportBinding struct {
	server                *CNIServer
	observer              *CNIEndpointObserver
	skipNetlinkProvenance bool
}

func ValidateNetlinkExportBenchmark(cfg evpn.ExporterConfig) error {
	if cfg.Recovery != nil {
		return fmt.Errorf("skipping Netlink export provenance is incompatible with durable export recovery")
	}
	for _, network := range cfg.Networks {
		if cfg.Enabled && network.Enabled && network.ExportSource() == "ovn-netlink" {
			return nil
		}
	}
	return fmt.Errorf("skipping Netlink export provenance requires enabled ovn-netlink export")
}

func (*cniExportBinding) beginCycle() {}
func (b *cniExportBinding) prepare(cfg evpn.ExporterConfig) error {
	if b.skipNetlinkProvenance && cfg.Enabled {
		if err := ValidateNetlinkExportBenchmark(cfg); err != nil {
			return err
		}
		b.server.operations.Lock()
		hasContinuity := b.server.continuity != nil
		b.server.operations.Unlock()
		if hasContinuity {
			return fmt.Errorf("skipping Netlink export provenance is incompatible with CNI continuity")
		}
	}
	if !cfg.Enabled || cfg.Recovery == nil {
		return nil
	}
	b.server.operations.Lock()
	defer b.server.operations.Unlock()
	c := b.server.continuity
	if c == nil {
		return fmt.Errorf("RECOVERY_BLOCKED: durable CNI continuity is required for recovered exports")
	}
	if c.nodeName != cfg.NodeName || c.domain.InstallationUID != cfg.Recovery.Domain.InstallationUID ||
		c.domain.NodeUID != cfg.Recovery.Domain.NodeUID {
		return fmt.Errorf("RECOVERY_BLOCKED: CNI and exporter node or installation recovery identities differ")
	}
	return nil
}

func (b *cniExportBinding) buildObserver(cfg evpn.ExporterConfig, source liveExportState) (evpn.EndpointObserver, *evpn.LocalOVNNetlinkObserver, error) {
	var native *evpn.LocalOVNNetlinkObserver
	for _, network := range cfg.Networks {
		if network.Enabled && network.ExportSource() == "ovn-netlink" {
			var err error
			if b.skipNetlinkProvenance {
				native, err = evpn.NewLocalOVNNetlinkBenchmarkObserver(cfg)
			} else {
				native, err = evpn.NewLocalOVNNetlinkObserver(cfg, &nativeOVNPorts{source: source, config: cfg})
			}
			if err != nil {
				return nil, native, err
			}
			break
		}
	}
	b.observer = &CNIEndpointObserver{server: b.server, source: source, config: cfg}
	selected := selectedEndpointObserver{cni: b.observer}
	networks := frameworkExportNetworks(cfg)
	if len(networks) != 0 {
		selected.framework = &ovnexport.FrameworkEndpointObserver{
			Source:    liveOVNExportState{nb: source.nb, sb: source.sb},
			LocalNode: cfg.NodeName, LocalChassis: cfg.LocalChassis, Networks: networks,
		}
	}
	if native != nil {
		selected.native = native
	}
	if cfg.Recovery != nil {
		return continuityExportObserver{b.server, selected}, native, nil
	}
	return selected, native, nil
}

func (b *cniExportBinding) update(cfg evpn.ExporterConfig) {
	b.observer.config = cfg
}

func (b *cniExportBinding) wrapRecoveryApplier(applier evpn.Type2RouteApplier) evpn.Type2RouteApplier {
	return continuityRouteApplier{b.server, applier}
}

func (b *cniExportBinding) observations() []ExportObservation {
	return append([]ExportObservation(nil), b.observer.Observations...)
}

func frameworkExportNetworks(cfg evpn.ExporterConfig) map[string]evpn.EVPNNetworkConfig {
	networks := make(map[string]evpn.EVPNNetworkConfig)
	for _, network := range cfg.Networks {
		if network.Enabled && network.ExportSource() == "framework" {
			networks[network.NetworkID] = network
		}
	}
	return networks
}
