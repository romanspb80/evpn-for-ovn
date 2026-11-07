package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
	"github.com/vishvananda/netns"

	"github.com/osrg/gobgp/v4/api"
	"github.com/ovn-kubernetes/libovsdb/client"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
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
	if err := cfg.Validate(); err != nil {
		return evpn.ExporterConfig{}, err
	}
	if cfg.Enabled && (cfg.Recovery != nil || cfg.AttestationConfigPath != "") {
		var stat unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
			return evpn.ExporterConfig{}, err
		}
		if stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || stat.Nlink != 1 {
			return evpn.ExporterConfig{}, fmt.Errorf("RECOVERY_BLOCKED: exporter recovery config must be root-owned, single-link and not group/world writable")
		}
	}
	return *cfg, nil
}

func bindExporterRecovery(ctx context.Context, cfg *evpn.ExporterConfig, nb client.Client) error {
	if cfg.Recovery == nil {
		return nil
	}
	recovery := *cfg.Recovery
	cfg.Recovery = &recovery
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	namespace, err := netns.Get()
	if err != nil {
		return err
	}
	identity := namespace.UniqueId()
	if err := namespace.Close(); err != nil {
		return err
	}
	var switches []*nbdb.LogicalSwitch
	if err := selectExportRows(ctx, nb, "Logical_Switch", []string{"_uuid", "name", "ports", "other_config"}, &switches); err != nil {
		return fmt.Errorf("RECOVERY_BLOCKED: exporter network identity: %w", err)
	}
	uuids := map[string]string{}
	for _, network := range cfg.Networks {
		for _, ls := range switches {
			if ls.Name == network.LogicalSwitch {
				if uuids[ls.Name] != "" || ls.UUID == "" {
					return fmt.Errorf("RECOVERY_BLOCKED: ambiguous exporter switch identity")
				}
				uuids[ls.Name] = ls.UUID
			}
		}
		if uuids[network.LogicalSwitch] == "" {
			return fmt.Errorf("RECOVERY_BLOCKED: missing exporter switch %s", network.LogicalSwitch)
		}
	}
	pin, err := os.ReadFile(cfg.AttestationConfigPath)
	if err != nil {
		return err
	}
	immutable := *cfg
	immutable.Enabled, immutable.DryRun, immutable.Recovery = false, false, nil
	data, err := json.Marshal(struct {
		Config      evpn.ExporterConfig
		Switches    map[string]string
		Attestation []byte
	}{immutable, uuids, pin})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	actual := recovery.Domain
	actual.BootID = strings.TrimSpace(string(boot))
	actual.NetworkNamespace = identity
	actual.ConfigDigest = hex.EncodeToString(hash[:])
	for _, pair := range [][2]string{
		{recovery.Domain.BootID, actual.BootID},
		{recovery.Domain.NetworkNamespace, actual.NetworkNamespace},
		{recovery.Domain.ConfigDigest, actual.ConfigDigest},
	} {
		if pair[0] != "" && pair[0] != pair[1] {
			return fmt.Errorf("RECOVERY_BLOCKED: exporter ownership domain changed")
		}
	}
	cfg.Recovery.Domain = actual
	return nil
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
	var exporterRecovery *evpn.ExportRecovery
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
		if exporterRecovery != nil {
			exporterRecovery.Close()
		}
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
					if cfg.Recovery != nil {
						exporter.Observer = continuityExportObserver{s, selected}
					}
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
			err = bindExporterRecovery(stepCtx, &cfg, nb)
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
		if err == nil && cfg.Enabled {
			if bound == nil {
				copy := cfg
				bound = &copy
			}
			observer.config = cfg
			exporter.DryRun = cfg.DryRun
			if !cfg.DryRun && conn != nil && cfg.Recovery != nil && continuity.Check(stepCtx) != nil {
				err = conn.Close()
				err = errors.Join(err, continuity.Close())
				conn = nil
			}
			if !cfg.DryRun && err == nil && conn == nil {
				var newConn *grpc.ClientConn
				var newGuard *unixSpeakerContinuity
				var apiClient api.GoBgpServiceClient
				newConn, newGuard, apiClient, err = newProviderTransport(cfg.AttestationConfigPath, cfg.GRPCEndpoint)
				if err == nil {
					conn, continuity = newConn, newGuard
					gate := attestedReadiness{evpn.SpeakerReadiness{Client: apiClient, ASN: cfg.ASN, RouterID: cfg.RouterID}, continuity}
					adapter := &evpn.GoBGPType2Adapter{Client: apiClient, Gate: gate}
					exporter.Gate, exporter.Continuity = gate, continuity
					exporter.Source, exporter.Applier = adapter, adapter
					if cfg.Recovery != nil {
						// Bind the actual authenticated transport with a read-only
						// RPC before reusing the initialized exporter's receipts.
						err = gate.Check(stepCtx)
						if err != nil {
							err = errors.Join(err, conn.Close(), continuity.Close())
							conn = nil
						}
					}
					if cfg.Recovery != nil && err == nil {
						applier := continuityRouteApplier{s, adapter}
						if exporterRecovery == nil {
							exporterRecovery, err = evpn.OpenExportRecovery(*cfg.Recovery, adapter, applier, continuity)
						} else {
							err = exporterRecovery.Rebind(adapter, applier, continuity)
						}
						if err == nil {
							exporter.Recovery, exporter.Applier = exporterRecovery, exporterRecovery
						}
					}
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
			if native != nil && len(last.Applied) != 0 && exporter.Recovery == nil {
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
