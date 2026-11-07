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
	var exportOptions nodeagent.ExportRuntimeOptions
	var remoteOptions nodeagent.RemoteRuntimeOptions
	flag.StringVar(&socketPath, "socket", "/run/evpn-node-agent/agent.sock", "Unix domain socket path for NodeAgentCNI gRPC server")
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
	authority, err := nodeagent.AcquireAgentAuthority()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to acquire evpn-node-agent authority: %v\n", err)
		return 1
	}
	defer authority.Close()

	if exportOptions.ConfigPath != "" {
		if _, err := nodeagent.LoadExporterConfig(exportOptions.ConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "invalid EVPN exporter configuration: %v\n", err)
			return 1
		}
	}
	if remoteOptions.ConfigPath != "" {
		if _, err := nodeagent.LoadRemoteRuntimeConfig(remoteOptions.ConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "invalid EVPN remote importer configuration: %v\n", err)
			return 1
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	options := nodeagent.RuntimeOptions{
		Mode:           runtimeMode,
		NetworkBackend: networkBackend,
		OVNNBEndpoint:  ovnNBEndpoint,
		OVSDBEndpoint:  ovsDBEndpoint,
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
