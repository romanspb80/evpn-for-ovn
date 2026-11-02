package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"

	"github.com/osrg/gobgp/v4/api"
	"github.com/ovn-kubernetes/libovsdb/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ExportRuntimeOptions struct {
	ConfigPath string
	StatusPath string
	SBEndpoint string
	Interval   time.Duration
	Timeout    time.Duration
}

type ExportStatus struct {
	Time           time.Time
	Error          string
	Observations   []ExportObservation
	Result         evpn.ExportResult
	SpeakerProcess SpeakerProcessEvidence
}

func LoadExporterConfig(path string) (evpn.ExporterConfig, error) {
	var cfg *evpn.ExporterConfig
	f, err := os.Open(path)
	if err != nil {
		return evpn.ExporterConfig{}, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return evpn.ExporterConfig{}, err
	}
	if cfg == nil {
		return evpn.ExporterConfig{}, fmt.Errorf("export config must be a JSON object")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return evpn.ExporterConfig{}, fmt.Errorf("export config must contain exactly one JSON object")
	}
	return *cfg, cfg.Validate()
}

func validateLocalExportConfig(cfg evpn.ExporterConfig) error {
	if node := os.Getenv("NODE_NAME"); node != "" && node != cfg.NodeName {
		return fmt.Errorf("configured node %q differs from NODE_NAME %q", cfg.NodeName, node)
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, network := range cfg.Networks {
		if !network.Enabled {
			continue
		}
		found := false
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.String() == network.LocalVTEP {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("network %s VTEP %s is not assigned on this node", network.NetworkID, network.LocalVTEP)
		}
	}
	return nil
}

// RunEndpointExporter is an optional periodic reader/application loop alongside
// the existing CNI server. It never invokes CNI ADD/DEL or speaker session RPCs.
func (s *CNIServer) RunEndpointExporter(ctx context.Context, runtime RuntimeOptions, opts ExportRuntimeOptions, report func(ExportStatus)) error {
	if opts.ConfigPath == "" {
		return nil
	}
	if runtime.Mode != RuntimeModeRealOVNOVS || runtime.NetworkBackend != "linux-veth" {
		return fmt.Errorf("EVPN export requires real-ovn-ovs/linux-veth")
	}
	if opts.Interval <= 0 || opts.Timeout <= 0 || opts.SBEndpoint == "" || report == nil {
		return fmt.Errorf("export interval, timeout, SB endpoint and reporter required")
	}
	_, err := LoadExporterConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	var nb, ovs, sb client.Client
	var conn *grpc.ClientConn
	var authority *net.UnixListener
	var native *evpn.LocalOVNNetlinkObserver
	continuity := &unixSpeakerContinuity{}
	defer func() {
		for _, c := range []client.Client{nb, ovs, sb} {
			if c != nil {
				c.Close()
			}
		}
		if conn != nil {
			conn.Close()
		}
		continuity.Close()
		if native != nil {
			native.Close()
		}
		if authority != nil {
			authority.Close()
		}
	}()
	exporter := &evpn.EndpointExporter{}
	var observer *CNIEndpointObserver
	var bound *evpn.ExporterConfig
	var last evpn.ExportResult
	tick := func() {
		status := ExportStatus{Time: time.Now().UTC(), Result: evpn.ExportResult{Applied: last.Applied}}
		cfg, err := LoadExporterConfig(opts.ConfigPath)
		stepCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		if err == nil && cfg.Enabled {
			err = validateLocalExportConfig(cfg)
		}
		if err == nil && cfg.Enabled && bound != nil {
			err = validateExporterAuthorityReload(*bound, cfg)
			old, next := *bound, cfg
			old.Networks, next.Networks = nil, nil
			old.DryRun, next.DryRun = false, false
			if err == nil && !reflect.DeepEqual(old, next) {
				err = fmt.Errorf("speaker/node identity change requires explicit exporter restart; existing receipts are not transferable")
			}
		}
		if err == nil && cfg.Enabled && authority == nil {
			authority, err = evpn.AcquireExporterAuthority()
		}
		if err == nil && cfg.Enabled && observer == nil {
			nb, err = newOVNNBClient(stepCtx, runtime.OVNNBEndpoint)
			if err == nil {
				ovs, err = newOVSClient(stepCtx, runtime.OVSDBEndpoint)
			}
			if err == nil {
				sb, err = newExportSBClient(stepCtx, opts.SBEndpoint)
			}
			if err == nil {
				source := liveExportState{nb: nb, ovs: ovs, sb: sb}
				for _, network := range cfg.Networks {
					if network.Enabled && network.ExportSource() == "ovn-netlink" {
						native, err = evpn.NewLocalOVNNetlinkObserver(cfg, &nativeOVNPorts{source: source, config: cfg})
						break
					}
				}
				if err == nil {
					observer = &CNIEndpointObserver{server: s, source: source, config: cfg}
					selected := selectedEndpointObserver{cni: observer}
					frameworkNetworks := make(map[string]evpn.EVPNNetworkConfig)
					for _, network := range cfg.Networks {
						if network.Enabled && network.ExportSource() == "framework" {
							frameworkNetworks[network.NetworkID] = network
						}
					}
					if len(frameworkNetworks) != 0 {
						selected.framework = &evpn.OVNEndpointObserver{
							Source:    liveOVNExportState{nb: nb, sb: sb},
							LocalNode: cfg.NodeName, LocalChassis: cfg.LocalChassis,
							Networks: frameworkNetworks,
						}
					}
					if native != nil {
						selected.native = native
					}
					exporter.Observer = selected
				}
			}
			if err != nil {
				for _, c := range []client.Client{nb, ovs, sb} {
					if c != nil {
						c.Close()
					}
				}
				nb, ovs, sb = nil, nil, nil
			}
		}
		if err == nil && cfg.Enabled {
			if bound == nil {
				copy := cfg
				bound = &copy
			}
			observer.config = cfg
			exporter.DryRun = cfg.DryRun
			if !cfg.DryRun && conn == nil {
				dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
				if cfg.ProcessContinuity {
					dialOptions = append(dialOptions, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
						return continuity.DialContext(ctx, cfg.GRPCEndpoint)
					}))
					exporter.Continuity = continuity
				}
				conn, err = grpc.NewClient(cfg.GRPCEndpoint, dialOptions...)
				if err == nil {
					apiClient := api.NewGoBgpServiceClient(conn)
					adapter := &evpn.GoBGPType2Adapter{Client: apiClient}
					exporter.Gate = evpn.SpeakerReadiness{Client: apiClient, ASN: cfg.ASN, RouterID: cfg.RouterID}
					exporter.Source, exporter.Applier = adapter, adapter
				}
			}
			if err == nil {
				status.Result, err = exporter.Reconcile(stepCtx)
				last = status.Result
			}
			status.Observations = append([]ExportObservation(nil), observer.Observations...)
		}
		if err != nil {
			status.Error = err.Error()
			status.Result.OwnershipUnverified = true
		}
		status.SpeakerProcess = continuity.Evidence()
		if !cfg.Enabled && err == nil {
			status.Observations = []ExportObservation{{State: "disabled", Reason: "explicit export enablement is false"}}
		}
		if opts.StatusPath != "" {
			if writeErr := writeExportStatus(opts.StatusPath, status); writeErr != nil {
				status.Error = errors.Join(err, writeErr).Error()
			}
		}
		report(status)
	}
	tick()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if native != nil && len(last.Applied) != 0 {
				shutdown, cancel := context.WithTimeout(context.Background(), opts.Timeout)
				defer cancel()
				networks := map[string]bool{}
				for _, cfg := range bound.Networks {
					if cfg.ExportSource() == "ovn-netlink" {
						networks[cfg.NetworkID] = true
					}
				}
				result, err := exporter.DrainNetworks(shutdown, networks)
				status := ExportStatus{Time: time.Now().UTC(), Result: result, SpeakerProcess: continuity.Evidence()}
				if err != nil {
					status.Error = "native exporter shutdown did not confirm withdrawal: " + err.Error()
				}
				if opts.StatusPath != "" {
					err = errors.Join(err, writeExportStatus(opts.StatusPath, status))
					if err != nil {
						status.Error = err.Error()
					}
				}
				report(status)
				return err
			}
			return nil
		case <-ticker.C:
			tick()
		}
	}
}

func writeExportStatus(path string, status ExportStatus) error {
	value, err := json.Marshal(status)
	if err == nil {
		err = os.WriteFile(path+".tmp", value, 0600)
		if err == nil {
			err = os.Rename(path+".tmp", path)
		}
	}
	if err != nil {
		return fmt.Errorf("write export status %s: %w", filepath.Clean(path), err)
	}
	return nil
}

func validateExporterAuthorityReload(old, next evpn.ExporterConfig) error {
	prior, current := map[string]evpn.EVPNNetworkConfig{}, map[string]evpn.EVPNNetworkConfig{}
	for _, cfg := range old.Networks {
		prior[cfg.NetworkID] = cfg
	}
	for _, cfg := range next.Networks {
		current[cfg.NetworkID] = cfg
	}
	for _, networks := range []map[string]evpn.EVPNNetworkConfig{prior, current} {
		for id := range networks {
			a, b := prior[id], current[id]
			if a.ExportSource() != b.ExportSource() ||
				(a.ExportSource() == "ovn-netlink" && !reflect.DeepEqual(a, b)) {
				return fmt.Errorf("network %s exporter authority/topology change requires controlled shutdown and restart; no live handoff", id)
			}
		}
	}
	return nil
}
