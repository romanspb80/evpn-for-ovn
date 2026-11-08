package evpn

import "context"

// EndpointObserver returns a complete observation of endpoints authorized for
// advertisement. Implementations must return an error when the observation is
// incomplete or uncertain; reconciliation retains applied routes on error.
type EndpointObserver interface {
	DesiredLocalEndpoints(ctx context.Context) ([]LocalEndpoint, error)
}

// EVPNNetworkConfig defines explicit advertisement settings for a network.
type EVPNNetworkConfig struct {
	NetworkID          string
	LogicalSwitch      string
	Enabled            bool
	VNI                uint32
	RouteDistinguisher string
	RouteTargets       []string
	LocalVTEP          string
	ExporterSource     string
	BridgeDevice       string
	VXLANDevice        string
	AdvertiseDevice    string
}

func (c EVPNNetworkConfig) ExportSource() string {
	if c.ExporterSource == "" {
		return "cni"
	}
	return c.ExporterSource
}
