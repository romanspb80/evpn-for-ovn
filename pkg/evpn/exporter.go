package evpn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// ExporterConfig is node-local; networks and dry-run can be reloaded. A disabled
// configuration never opens a GoBGP connection or withdraws existing routes.
type ExporterConfig struct {
	Enabled           bool
	DryRun            bool
	ProcessContinuity bool
	NodeName          string
	LocalChassis      string
	GRPCEndpoint      string
	ASN               uint32
	RouterID          string
	Networks          []EVPNNetworkConfig
}

func (c ExporterConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.NodeName == "" || c.LocalChassis == "" || c.ASN == 0 {
		return fmt.Errorf("export requires node name, local chassis and nonzero expected ASN")
	}
	rid, err := netip.ParseAddr(c.RouterID)
	if err != nil || !rid.Is4() || rid.IsUnspecified() || rid.IsMulticast() {
		return fmt.Errorf("expected router ID must be a unicast IPv4 address")
	}
	if strings.HasPrefix(c.GRPCEndpoint, "unix:///") {
		if strings.TrimPrefix(c.GRPCEndpoint, "unix://") == "/" {
			return fmt.Errorf("node-local gRPC socket path is required")
		}
	} else {
		if c.ProcessContinuity {
			return fmt.Errorf("process continuity requires a node-local Unix socket")
		}
		host, port, err := net.SplitHostPort(c.GRPCEndpoint)
		ip := net.ParseIP(host)
		number, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || number == 0 {
			return fmt.Errorf("GoBGP endpoint must be an absolute Unix socket or loopback host:port, not a cluster Service")
		}
	}
	seen, switches := map[string]bool{}, map[string]bool{}
	devices, nativeVNIs := map[string]bool{}, map[uint32]bool{}
	for _, cfg := range c.Networks {
		if cfg.NetworkID == "" || cfg.LogicalSwitch == "" || seen[cfg.NetworkID] || switches[cfg.LogicalSwitch] {
			return fmt.Errorf("networks require unique nonempty NetworkID and LogicalSwitch")
		}
		seen[cfg.NetworkID], switches[cfg.LogicalSwitch] = true, true
		switch cfg.ExportSource() {
		case "cni":
			if cfg.BridgeDevice != "" || cfg.VXLANDevice != "" || cfg.AdvertiseDevice != "" {
				return fmt.Errorf("network %s: Netlink devices require ovn-netlink authority", cfg.NetworkID)
			}
		case "framework":
			if cfg.BridgeDevice != "" || cfg.VXLANDevice != "" || cfg.AdvertiseDevice != "" {
				return fmt.Errorf("network %s: Netlink devices require ovn-netlink authority", cfg.NetworkID)
			}
		case "ovn-netlink":
			if !c.DryRun && !c.ProcessContinuity {
				return fmt.Errorf("native live export requires process continuity and a shared host PID namespace")
			}
			if nativeVNIs[cfg.VNI] {
				return fmt.Errorf("native exporter requires unique VNI mapping")
			}
			nativeVNIs[cfg.VNI] = true
			for _, name := range []string{cfg.BridgeDevice, cfg.VXLANDevice, cfg.AdvertiseDevice} {
				if name == "" || name == "." || name == ".." || len(name) > 15 ||
					strings.ContainsAny(name, "/ \t\n") || devices[name] {
					return fmt.Errorf("native exporter requires distinct explicit Linux device names")
				}
				devices[name] = true
			}
		default:
			return fmt.Errorf("network %s: unsupported exporter source %q", cfg.NetworkID, cfg.ExporterSource)
		}
		if cfg.VNI == 0 || cfg.VNI > 16777215 {
			return fmt.Errorf("network %s: VNI outside 1..16777215", cfg.NetworkID)
		}
		vtep, err := netip.ParseAddr(cfg.LocalVTEP)
		if err != nil || !vtep.Is4() || vtep.IsUnspecified() || vtep.IsMulticast() || vtep.IsLoopback() {
			return fmt.Errorf("network %s: explicit unicast VTEP IPv4 required", cfg.NetworkID)
		}
		if _, err := bgp.ParseRouteDistinguisher(cfg.RouteDistinguisher); err != nil {
			return fmt.Errorf("network %s: invalid RD: %w", cfg.NetworkID, err)
		}
		if len(cfg.RouteTargets) == 0 {
			return fmt.Errorf("network %s: export RT required", cfg.NetworkID)
		}
		for _, rt := range cfg.RouteTargets {
			if _, err := bgp.ParseRouteTarget(rt); err != nil {
				return fmt.Errorf("network %s: invalid RT: %w", cfg.NetworkID, err)
			}
		}
	}
	return nil
}

type SpeakerReadiness struct {
	Client   api.GoBgpServiceClient
	ASN      uint32
	RouterID string
}

func (s SpeakerReadiness) Check(ctx context.Context) error {
	if s.Client == nil {
		return fmt.Errorf("speaker API is not configured")
	}
	res, err := s.Client.GetBgp(ctx, &api.GetBgpRequest{})
	if err != nil {
		return fmt.Errorf("speaker GetBgp: %w", err)
	}
	g := res.GetGlobal()
	if g == nil || s.ASN == 0 || g.Asn != s.ASN || g.RouterId != s.RouterID {
		return fmt.Errorf("speaker not ready: expected ASN=%d routerID=%s, got %v", s.ASN, s.RouterID, g)
	}
	for _, family := range g.Families {
		if family == uint32(oc.AFI_SAFI_TYPE_L2VPN_EVPN.ToInt()) {
			return nil
		}
	}
	return fmt.Errorf("speaker not ready: EVPN family missing")
}

type ReadinessGate interface{ Check(context.Context) error }

type ExportResult struct {
	Desired             []LocalEndpoint
	Applied             []OwnedType2Route
	DryRun              bool
	OwnershipUnverified bool
}

// EndpointExporter never manages speaker configuration. Transient recovery
// requires process continuity; ownership evidence loss permanently fences writes.
type EndpointExporter struct {
	mu          sync.Mutex
	Observer    EndpointObserver
	Gate        ReadinessGate
	Continuity  ReadinessGate
	Source      Type2RIBSource
	Applier     Type2RouteApplier
	DryRun      bool
	reconciler  *RouteReconciler
	initialized bool
	blocked     error
}

func (e *EndpointExporter) Reconcile(ctx context.Context) (result ExportResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reconcile(ctx, e.Observer)
}

type fixedLocalEndpoints []LocalEndpoint

func (p fixedLocalEndpoints) DesiredLocalEndpoints(context.Context) ([]LocalEndpoint, error) {
	return p, nil
}

// DrainNetworks relinquishes only this process's acknowledged receipts. It
// retains all other networks and still applies speaker/UUID ownership gates.
func (e *EndpointExporter) DrainNetworks(ctx context.Context, networks map[string]bool) (ExportResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keep fixedLocalEndpoints
	if e.reconciler != nil {
		for _, route := range e.reconciler.AppliedRoutes() {
			if !networks[route.Endpoint.NetworkID] {
				keep = append(keep, route.Endpoint)
			}
		}
	}
	return e.reconcile(ctx, keep)
}

func (e *EndpointExporter) reconcile(ctx context.Context, observer EndpointObserver) (result ExportResult, err error) {
	result = ExportResult{DryRun: e.DryRun}
	defer func() { result.OwnershipUnverified = e.blocked != nil || err != nil }()
	if e.reconciler == nil {
		e.reconciler = NewRouteReconciler()
	}
	result.Applied = e.reconciler.AppliedRoutes()
	if e.blocked != nil {
		return result, e.blocked
	}
	if observer == nil {
		return result, fmt.Errorf("endpoint observer required")
	}
	desired, err := observer.DesiredLocalEndpoints(ctx)
	if err != nil {
		return result, fmt.Errorf("endpoint observation failed; retaining applied routes: %w", err)
	}
	result.Desired = desired
	for _, ep := range desired {
		if _, err := BuildType2Route(ep); err != nil {
			return result, fmt.Errorf("invalid observed endpoint: %w", err)
		}
	}
	if e.DryRun {
		return result, nil
	}
	if e.Gate == nil || e.Source == nil || e.Applier == nil {
		return result, fmt.Errorf("speaker readiness, RIB source and applier required")
	}
	if e.Continuity != nil && e.initialized {
		if err := e.Continuity.Check(ctx); err != nil {
			e.blocked = fmt.Errorf("speaker continuity lost; no writes: %w", err)
			return result, e.blocked
		}
	}
	if err := e.Gate.Check(ctx); err != nil {
		if len(result.Applied) != 0 && (e.Continuity == nil || e.Continuity.Check(ctx) != nil) {
			e.blocked = fmt.Errorf("speaker continuity lost; restart/resync required: %w", err)
			return result, e.blocked
		}
		return result, err
	}
	if e.Continuity != nil {
		if err := e.Continuity.Check(ctx); err != nil {
			return result, fmt.Errorf("speaker continuity unverified; no writes: %w", err)
		}
	}
	if !e.initialized {
		if err := e.reconciler.Resync(ctx, e.Source, nil); err != nil {
			return result, err
		}
		e.initialized = true
	}
	paths, err := e.Source.Snapshot(ctx)
	if err != nil {
		return result, fmt.Errorf("RIB unavailable; no writes: %w", err)
	}
	for _, owned := range result.Applied {
		found := false
		for _, path := range paths {
			if path.Local && ribPathMatchesEndpoint(path, owned.Endpoint) {
				found = true
			}
		}
		if !found {
			e.blocked = fmt.Errorf("%w: confirmed path disappeared; possible speaker restart, no writes", ErrRouteResyncRequired)
			return result, e.blocked
		}
	}
	deltas, err := e.reconciler.Reconcile(desired)
	if err != nil {
		return result, err
	}
	for _, delta := range deltas {
		if err := e.Gate.Check(ctx); err != nil {
			if e.Continuity == nil || e.Continuity.Check(ctx) != nil {
				e.blocked = fmt.Errorf("speaker readiness lost before mutation: %w", err)
				return result, e.blocked
			}
			return result, fmt.Errorf("speaker readiness uncertain; writes frozen: %w", err)
		}
		if e.Continuity != nil {
			if err := e.Continuity.Check(ctx); err != nil {
				e.blocked = fmt.Errorf("speaker continuity lost before mutation: %w", err)
				return result, e.blocked
			}
		}
		if err := e.reconciler.Apply(ctx, delta, e.Applier); err != nil {
			result.Applied = e.reconciler.AppliedRoutes()
			return result, err
		}
		result.Applied = e.reconciler.AppliedRoutes()
	}
	result.Applied = e.reconciler.AppliedRoutes()
	return result, nil
}
