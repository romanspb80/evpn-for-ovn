package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	var socketPath string
	var runtimeMode string
	var networkBackend string
	var ovnNBEndpoint string
	var ovsDBEndpoint string
	var cniContinuityConfig string
	var standaloneImportOnly bool
	var standaloneExportConfigPath string
	var standaloneExportConfig nodeagent.StandaloneExportRuntimeConfig
	var exportOptions nodeagent.ExportRuntimeOptions
	var remoteOptions nodeagent.RemoteRuntimeOptions
	var inventoryOptions nodeagent.InventoryGateOptions
	flag.StringVar(&socketPath, "socket", "/run/evpn-node-agent/agent.sock", "Unix domain socket path for NodeAgentCNI gRPC server")
	flag.BoolVar(&standaloneImportOnly, "standalone-evpn-import", false, "Run only the standalone-attested Type-2 importer (requires real-ovn-ovs/linux-veth)")
	flag.StringVar(&standaloneExportConfigPath, "standalone-evpn-export-config", "", "Root-protected standalone EVPN export runtime config; does not start the CNI server")
	flag.StringVar(&runtimeMode, "mode", nodeagent.RuntimeModeInMemory, "Runtime mode: in-memory or real-ovn-ovs")
	flag.StringVar(&networkBackend, "network-backend", "in-memory", "Network backend: in-memory or linux-veth")
	flag.StringVar(&ovnNBEndpoint, "ovn-nb-endpoint", "unix:/var/run/ovn/ovnnb_db.sock", "OVN NBDB endpoint for real-ovn-ovs mode")
	flag.StringVar(&ovsDBEndpoint, "ovsdb-endpoint", "unix:/var/run/openvswitch/db.sock", "OVSDB endpoint for real-ovn-ovs mode")
	flag.StringVar(&cniContinuityConfig, "cni-continuity-config", "", "Root-owned CNI continuity JSON configuration; disabled by default")
	flag.StringVar(&exportOptions.ConfigPath, "evpn-export-config", "", "Optional node-local EVPN export JSON config; disabled by default")
	flag.StringVar(&exportOptions.StatusPath, "evpn-export-status", "", "Optional exporter status JSON file")
	flag.StringVar(&exportOptions.SBEndpoint, "ovn-sb-endpoint", "unix:/var/run/ovn/ovnsb_db.sock", "OVN SBDB endpoint for EVPN readiness reads")
	flag.BoolVar(&exportOptions.RequireLocalChassis, "evpn-export-verify-local-chassis", false, "Verify local OVS system-id against the configured OVN Southbound chassis before every export cycle")
	flag.BoolVar(&exportOptions.SkipNetlinkProvenance, "evpn-export-skip-netlink-provenance", false, "Trust native OVN Netlink export records without per-endpoint NB/SB verification (isolated benchmarks only, incompatible with recovery)")
	flag.DurationVar(&exportOptions.Interval, "evpn-export-interval", 2*time.Second, "EVPN observation interval")
	flag.DurationVar(&exportOptions.Timeout, "evpn-export-timeout", 5*time.Second, "Timeout for one EVPN observation/application cycle")
	flag.StringVar(&remoteOptions.ConfigPath, "evpn-remote-config", "", "Optional node-local remote EVPN import JSON config; disabled by default")
	flag.StringVar(&remoteOptions.StatusPath, "evpn-remote-status", "", "Optional remote importer status JSON file")
	flag.DurationVar(&remoteOptions.Interval, "evpn-remote-interval", 2*time.Second, "Remote EVPN import reconciliation interval")
	flag.DurationVar(&remoteOptions.Timeout, "evpn-remote-timeout", 5*time.Second, "Timeout for one remote EVPN import reconciliation")
	flag.StringVar(&inventoryOptions.Namespace, "worker-inventory-namespace", "", "Kubernetes namespace of the operator inventory; empty disables inventory mode")
	flag.StringVar(&inventoryOptions.ConfigMapUID, "worker-inventory-uid", "", "Pinned Kubernetes UID of the operator inventory ConfigMap")
	flag.StringVar(&inventoryOptions.ClusterUID, "worker-inventory-cluster-uid", "", "Trusted inventory cluster UID")
	flag.StringVar(&inventoryOptions.InstallationUID, "worker-inventory-installation-uid", "", "Trusted EvpnCore installation UID")
	flag.StringVar(&inventoryOptions.JournalDirectory, "worker-inventory-journal", "", "Dedicated durable node-local inventory fence directory")
	flag.StringVar(&inventoryOptions.RecoveryFile, "worker-inventory-recovery-file", "", "Opt in to versioned trust recovery; absolute protected host-administrator recovery request path")
	flag.Parse()

	var err error
	var exportConfig evpn.ExporterConfig
	if exportOptions.ConfigPath != "" {
		exportConfig, err = nodeagent.LoadExporterConfig(exportOptions.ConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid EVPN exporter configuration: %v\n", err)
			return 1
		}
	}
	if exportOptions.SkipNetlinkProvenance {
		if standaloneImportOnly || standaloneExportConfigPath != "" || cniContinuityConfig != "" || inventoryOptions.Namespace != "" {
			fmt.Fprintln(os.Stderr, "skipping Netlink export provenance is incompatible with standalone modes, CNI continuity or worker inventory")
			return 1
		}
		if err := nodeagent.ValidateNetlinkExportBenchmark(exportConfig); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	var remoteConfig nodeagent.RemoteRuntimeConfig
	if remoteOptions.ConfigPath != "" {
		remoteConfig, err = nodeagent.LoadRemoteRuntimeConfig(remoteOptions.ConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid EVPN remote importer configuration: %v\n", err)
			return 1
		}
	}
	if standaloneImportOnly {
		if err := validateStandaloneImportOnly(runtimeMode, networkBackend, cniContinuityConfig,
			exportOptions.ConfigPath, remoteOptions.ConfigPath, remoteConfig); err != nil {
			fmt.Fprintf(os.Stderr, "invalid standalone EVPN importer mode: %v\n", err)
			return 1
		}
		attestation, err := nodeagent.LoadProviderAttestationConfig(remoteConfig.AttestationConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid standalone speaker attestation: %v\n", err)
			return 1
		}
		if err := validateStandaloneSpeakerAttestation(attestation); err != nil {
			fmt.Fprintf(os.Stderr, "invalid standalone speaker attestation: %v\n", err)
			return 1
		}
	}
	if standaloneImportOnly && standaloneExportConfigPath != "" {
		fmt.Fprintln(os.Stderr, "standalone EVPN import and export modes are mutually exclusive")
		return 1
	}
	if standaloneExportConfigPath != "" {
		standaloneExportConfig, err = nodeagent.LoadStandaloneExportRuntimeConfig(standaloneExportConfigPath)
		if err == nil {
			err = nodeagent.ValidateStandaloneExportRuntime(
				standaloneExportConfig,
				nodeagent.RuntimeOptions{
					Mode: runtimeMode, NetworkBackend: networkBackend,
					OVNNBEndpoint: ovnNBEndpoint, OVSDBEndpoint: ovsDBEndpoint,
				},
				cniContinuityConfig, exportOptions.ConfigPath, remoteOptions.ConfigPath,
			)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid standalone EVPN exporter mode: %v\n", err)
			return 1
		}
	}
	if !standaloneImportOnly && standaloneExportConfigPath == "" && exportOptions.ConfigPath != "" && remoteOptions.ConfigPath != "" {
		if err := nodeagent.ValidateCohostedType2Config(exportConfig, remoteConfig); err != nil {
			fmt.Fprintf(os.Stderr, "invalid co-hosted EVPN configuration: %v\n", err)
			return 1
		}
		exportOptions.PeerImportConfig = &remoteConfig
		remoteOptions.ExpectedConfig = &remoteConfig
		remoteOptions.PeerExportConfigPath = exportOptions.ConfigPath
	}
	if standaloneExportConfigPath != "" && standaloneExportConfig.EnableRemoteImport {
		exportOptions.PeerImportConfig = &remoteConfig
		remoteOptions.ExpectedConfig = &remoteConfig
		remoteOptions.PeerExportConfigPath = standaloneExportConfig.ExporterConfigPath
	}

	authority, err := nodeagent.AcquireAgentAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to acquire evpn-node-agent authority: %v\n", err)
		return 1
	}
	defer authority.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	inventoryEnabled := inventoryOptions.Namespace != "" || inventoryOptions.ConfigMapUID != "" ||
		inventoryOptions.ClusterUID != "" || inventoryOptions.InstallationUID != "" || inventoryOptions.JournalDirectory != ""
	var inventoryGate *nodeagent.InventoryGate
	if inventoryEnabled {
		if standaloneImportOnly || standaloneExportConfigPath != "" || !exportOptions.RequireLocalChassis {
			fmt.Fprintln(os.Stderr, "inventory mode requires co-hosted CNI Type-2 runtimes and local chassis verification")
			return 1
		}
		inventoryOptions.ExportConfigPath, inventoryOptions.ImportConfigPath = exportOptions.ConfigPath, remoteOptions.ConfigPath
		inventoryOptions.ContinuityConfigPath = cniContinuityConfig
		inventoryOptions.OVSDBEndpoint, inventoryOptions.SBEndpoint = ovsDBEndpoint, exportOptions.SBEndpoint
		gate, err := nodeagent.OpenInventoryGate(inventoryOptions)
		if err != nil {
			fmt.Fprintf(os.Stderr, "inventory gate initialization failed: %v\n", err)
			return 1
		}
		defer func() {
			if err := gate.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "inventory gate close failed: %v\n", err)
			}
		}()
		exportOptions.InventoryGate = gate.CheckExporter
		if inventoryOptions.RecoveryFile != "" {
			exportOptions.InventoryRecovery = gate
		}
		remoteOptions.InventoryGate = gate.CheckImporter
		inventoryGate = gate
		err = checkInventoryStartup(ctx,
			func(checkCtx context.Context) error {
				if err := gate.CheckExporter(checkCtx, exportConfig); err != nil {
					return err
				}
				return gate.CheckImporter(checkCtx, remoteConfig)
			},
			func(cleanupCtx context.Context, failure error) error {
				result, err := nodeagent.WithdrawInventoryExports(cleanupCtx, nodeagent.RuntimeOptions{
					Mode: runtimeMode, NetworkBackend: networkBackend, OVNNBEndpoint: ovnNBEndpoint, OVSDBEndpoint: ovsDBEndpoint,
				}, exportOptions, failure)
				fmt.Fprintf(os.Stderr, "inventory startup withdrawal: state=%s remaining=%d unverified=%t error=%v\n",
					result.RecoveryState, len(result.Applied), result.OwnershipUnverified, err)
				return err
			})
		if err != nil {
			fmt.Fprintf(os.Stderr, "inventory startup blocked: %v\n", err)
			return 1
		}
		// Later CNI/importer initialization can deny inventory after preflight.
		// Runtime callers cancel and join both loops before this defer runs.
		defer func() {
			if exitCode == 0 {
				return
			}
			cleanupCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer done()
			result, err := nodeagent.WithdrawInventoryExports(cleanupCtx, nodeagent.RuntimeOptions{
				Mode: runtimeMode, NetworkBackend: networkBackend, OVNNBEndpoint: ovnNBEndpoint, OVSDBEndpoint: ovsDBEndpoint,
			}, exportOptions, fmt.Errorf("inventory runtime terminated before safe completion"))
			fmt.Fprintf(os.Stderr, "inventory fatal-exit withdrawal: state=%s remaining=%d unverified=%t error=%v\n",
				result.RecoveryState, len(result.Applied), result.OwnershipUnverified, err)
		}()
	}

	options := nodeagent.RuntimeOptions{
		Mode:           runtimeMode,
		NetworkBackend: networkBackend,
		OVNNBEndpoint:  ovnNBEndpoint,
		OVSDBEndpoint:  ovsDBEndpoint,
	}
	if standaloneImportOnly {
		err := nodeagent.RunRemoteType2Importer(ctx, options, remoteOptions, func(status nodeagent.RemoteRuntimeStatus) {
			value, err := json.Marshal(status)
			if err != nil {
				fmt.Fprintf(os.Stderr, "EVPN remote status encoding failed: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "evpn-remote-import %s\n", value)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "standalone EVPN remote importer stopped: %v\n", err)
			return 1
		}
		return 0
	}
	if standaloneExportConfigPath != "" {
		err := runStandaloneEVPN(ctx, options, exportOptions, standaloneExportConfig, remoteOptions,
			standaloneRunners{nodeagent.RunStandaloneEndpointExporter, nodeagent.RunRemoteType2Importer})
		if err != nil {
			fmt.Fprintf(os.Stderr, "standalone EVPN runtime stopped: %v\n", err)
			return 1
		}
		return 0
	}
	var service *nodeagent.CNIServer
	var cleanup func()
	build := func(ctx context.Context) error {
		var err error
		service, cleanup, err = nodeagent.BuildServerForOptions(ctx, nodeagent.NewStore(), options, nil)
		return err
	}
	if cniContinuityConfig != "" {
		err = nodeagent.WaitForCNIBackends(ctx, func(ctx context.Context) error {
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return build(attempt)
		})
	} else {
		err = build(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure evpn-node-agent runtime: %v\n", err)
		return 1
	}
	if cleanup != nil {
		defer cleanup()
	}
	if cniContinuityConfig != "" {
		cfg, err := nodeagent.LoadCNIContinuityConfig(cniContinuityConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid CNI continuity configuration: %v\n", err)
			return 1
		}
		if inventoryEnabled {
			if err := inventoryGate.ValidateContinuity(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "inventory CNI startup blocked: %v\n", err)
				return 1
			}
		}
		var closeContinuity func() error
		err = nodeagent.WaitForCNIBackends(ctx, func(ctx context.Context) error {
			var err error
			closeContinuity, err = service.EnableCNIContinuity(ctx, cfg, options)
			return err
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "CNI recovery blocked: %v\n", err)
			return 1
		}
		defer closeContinuity()
	}
	server, err := nodeagent.StartUnixServer(ctx, socketPath, service)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start evpn-node-agent: %v\n", err)
		return 1
	}
	defer func() {
		if stopErr := server.Stop(context.Background()); stopErr != nil {
			fmt.Fprintf(os.Stderr, "failed to stop evpn-node-agent cleanly: %v\n", stopErr)
		}
	}()

	fmt.Fprintf(os.Stderr, "evpn-node-agent listening on unix socket: %s\n", socketPath)
	runtimeErrors := make(chan error, 2)
	var runtimes sync.WaitGroup
	if exportOptions.ConfigPath != "" {
		runtimes.Add(1)
		go func() {
			defer runtimes.Done()
			err := service.RunEndpointExporter(ctx, options, exportOptions, func(status nodeagent.ExportStatus) {
				value, err := json.Marshal(status)
				if err != nil {
					fmt.Fprintf(os.Stderr, "EVPN status encoding failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "evpn-export %s\n", value)
			})
			if err != nil {
				runtimeErrors <- fmt.Errorf("EVPN exporter stopped: %w", err)
			}
		}()
	}
	if remoteOptions.ConfigPath != "" {
		runtimes.Add(1)
		go func() {
			defer runtimes.Done()
			err := service.RunRemoteType2Importer(ctx, options, remoteOptions, func(status nodeagent.RemoteRuntimeStatus) {
				value, err := json.Marshal(status)
				if err != nil {
					fmt.Fprintf(os.Stderr, "EVPN remote status encoding failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "evpn-remote-import %s\n", value)
			})
			if err != nil {
				runtimeErrors <- fmt.Errorf("EVPN remote importer stopped: %w", err)
			}
		}()
	}
	select {
	case <-ctx.Done():
		runtimes.Wait()
		close(runtimeErrors)
		for err := range runtimeErrors {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		return 0
	case err := <-runtimeErrors:
		fmt.Fprintf(os.Stderr, "%v\n", err)
		cancel()
		runtimes.Wait()
		return 1
	}
}

func checkInventoryStartup(ctx context.Context, check func(context.Context) error, withdraw func(context.Context, error) error) error {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := check(checkCtx)
	cancel()
	if err == nil {
		return nil
	}
	// Give owned cleanup its own bounded budget, not the failed observation's
	// canceled/expired child context. Parent shutdown still cancels cleanup.
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cleanupErr := withdraw(cleanupCtx, err); cleanupErr != nil {
		return errors.Join(err, fmt.Errorf("owned startup withdrawal unresolved: %w", cleanupErr))
	}
	return err
}

type standaloneRunners struct {
	exporter func(context.Context, nodeagent.RuntimeOptions, nodeagent.ExportRuntimeOptions, nodeagent.StandaloneExportRuntimeConfig, string, func(nodeagent.ExportStatus)) error
	importer func(context.Context, nodeagent.RuntimeOptions, nodeagent.RemoteRuntimeOptions, func(nodeagent.RemoteRuntimeStatus)) error
}

func runStandaloneEVPN(
	ctx context.Context,
	options nodeagent.RuntimeOptions,
	exportOptions nodeagent.ExportRuntimeOptions,
	exportConfig nodeagent.StandaloneExportRuntimeConfig,
	remoteOptions nodeagent.RemoteRuntimeOptions,
	runners standaloneRunners,
) error {
	if exportConfig.EnableRemoteImport != (remoteOptions.ConfigPath != "") {
		return fmt.Errorf("standalone remote import requires both enableRemoteImport=true and an importer config path")
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	count := 1
	if remoteOptions.ConfigPath != "" {
		count++
	}
	results := make(chan error, count)
	go func() {
		results <- runners.exporter(runtimeCtx, options, exportOptions, exportConfig, remoteOptions.ConfigPath, func(status nodeagent.ExportStatus) {
			value, err := json.Marshal(status)
			if err != nil {
				fmt.Fprintf(os.Stderr, "standalone EVPN status encoding failed: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "standalone-evpn-export %s\n", value)
		})
	}()
	if remoteOptions.ConfigPath != "" {
		go func() {
			results <- runners.importer(runtimeCtx, options, remoteOptions, func(status nodeagent.RemoteRuntimeStatus) {
				value, err := json.Marshal(status)
				if err != nil {
					fmt.Fprintf(os.Stderr, "standalone EVPN import status encoding failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "standalone-evpn-import %s\n", value)
			})
		}()
	}

	var firstErr error
	for completed := 0; completed < count; completed++ {
		if err := <-results; err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		cancel()
	}
	return firstErr
}

func validateStandaloneImportOnly(runtimeMode, networkBackend, cniContinuityConfig, exportConfig, remoteConfigPath string, remote nodeagent.RemoteRuntimeConfig) error {
	if runtimeMode != nodeagent.RuntimeModeRealOVNOVS || networkBackend != "linux-veth" {
		return fmt.Errorf("requires --mode=real-ovn-ovs and --network-backend=linux-veth")
	}
	if cniContinuityConfig != "" || exportConfig != "" {
		return fmt.Errorf("cannot be combined with CNI continuity or EVPN export")
	}
	if remoteConfigPath == "" || !remote.Enabled || remote.AttestationConfigPath == "" {
		return fmt.Errorf("requires an enabled --evpn-remote-config with standalone speaker attestation")
	}
	return nil
}

func validateStandaloneSpeakerAttestation(config nodeagent.ProviderAttestationConfig) error {
	if config.Mode != "standalone" || config.Standalone == nil {
		return fmt.Errorf("standalone mode requires explicit standalone speaker identity")
	}
	return nil
}
