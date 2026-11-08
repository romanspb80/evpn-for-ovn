package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
)

func main() {
	os.Exit(run())
}

func run() int {
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
	flag.DurationVar(&exportOptions.Interval, "evpn-export-interval", 2*time.Second, "EVPN observation interval")
	flag.DurationVar(&exportOptions.Timeout, "evpn-export-timeout", 5*time.Second, "Timeout for one EVPN observation/application cycle")
	flag.StringVar(&remoteOptions.ConfigPath, "evpn-remote-config", "", "Optional node-local remote EVPN import JSON config; disabled by default")
	flag.StringVar(&remoteOptions.StatusPath, "evpn-remote-status", "", "Optional remote importer status JSON file")
	flag.DurationVar(&remoteOptions.Interval, "evpn-remote-interval", 2*time.Second, "Remote EVPN import reconciliation interval")
	flag.DurationVar(&remoteOptions.Timeout, "evpn-remote-timeout", 5*time.Second, "Timeout for one remote EVPN import reconciliation")
	flag.Parse()

	var err error
	if exportOptions.ConfigPath != "" {
		if _, err := nodeagent.LoadExporterConfig(exportOptions.ConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "invalid EVPN exporter configuration: %v\n", err)
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

	authority, err := nodeagent.AcquireAgentAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to acquire evpn-node-agent authority: %v\n", err)
		return 1
	}
	defer authority.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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
	service, cleanup, err := nodeagent.BuildServerForOptions(ctx, nodeagent.NewStore(), options, nil)
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
		closeContinuity, err := service.EnableCNIContinuity(ctx, cfg, options)
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
