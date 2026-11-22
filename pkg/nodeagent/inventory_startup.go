package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

// WithdrawInventoryExports drains existing acknowledged ownership without
// starting CNI, observing desired endpoints, or granting inventory recovery.
// Inventory rejection must not prevent this withdrawal-only startup path.
func WithdrawInventoryExports(ctx context.Context, runtime RuntimeOptions, opts ExportRuntimeOptions, failure error) (result evpn.ExportResult, err error) {
	result.OwnershipUnverified = true
	result.RecoveryState = "RECOVERY_BLOCKED"
	defer func() {
		if err != nil {
			result.OwnershipUnverified = true
			result.RecoveryState = "RECOVERY_BLOCKED"
		}
	}()
	if runtime.Mode != RuntimeModeRealOVNOVS || runtime.NetworkBackend != "linux-veth" ||
		opts.InventoryGate == nil || !opts.RequireLocalChassis || failure == nil {
		return result, fmt.Errorf("inventory withdrawal requires the inventory-gated real-ovn-ovs/linux-veth runtime")
	}
	cfg, err := LoadExporterConfig(opts.ConfigPath)
	if err != nil {
		return result, err
	}
	if !cfg.Enabled || cfg.DryRun || cfg.Recovery == nil {
		return result, fmt.Errorf("inventory withdrawal requires live durable exporter configuration")
	}
	if node := os.Getenv("NODE_NAME"); node == "" || node != cfg.NodeName {
		return result, fmt.Errorf("inventory withdrawal requires the configured local Node identity")
	}
	// A missing journal is not evidence that an earlier speaker has no routes.
	info, err := os.Lstat(cfg.Recovery.StateDirectory)
	if err != nil {
		return result, fmt.Errorf("existing export ownership journal required for startup withdrawal: %w", err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("existing export ownership journal required for startup withdrawal: state path is not a directory")
	}
	authority, err := evpn.AcquireExporterAuthority()
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, authority.Close()) }()
	nb, err := newOVNNBClient(ctx, runtime.OVNNBEndpoint)
	if err != nil {
		return result, fmt.Errorf("startup withdrawal domain verification: %w", err)
	}
	defer nb.Close()
	if err := bindExporterRecovery(ctx, &cfg, nb); err != nil {
		return result, err
	}
	conn, continuity, client, err := newProviderTransport(cfg.AttestationConfigPath, cfg.GRPCEndpoint,
		cfg.ASN, cfg.RouterID, cfg.Recovery.Domain.InstallationUID, cfg.Recovery.Domain.NodeUID)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close(), continuity.Close()) }()
	gate := attestedReadiness{evpn.SpeakerReadiness{Client: client, ASN: cfg.ASN, RouterID: cfg.RouterID}, continuity}
	adapter := &evpn.GoBGPType2Adapter{Client: client, Gate: gate}
	recovery, err := evpn.OpenExportRecovery(*cfg.Recovery, adapter, withdrawalOnlyApplier{adapter}, continuity)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, recovery.Close()) }()
	if err := recovery.RecoverPending(ctx); err != nil {
		return result, fmt.Errorf("startup withdrawal pending ownership unresolved: %w", err)
	}
	var recordErr error
	if opts.InventoryRecovery != nil {
		// Preserve legacy-fence classification before schema conversion. A new
		// temporary denial must not become a legacy security incident.
		recordErr = opts.InventoryRecovery.RecordFailure(failure, recovery)
	}
	exporter := &evpn.EndpointExporter{Gate: gate, Continuity: continuity, Source: adapter,
		Applier: recovery, Recovery: recovery}
	result, err = withdrawFencedExport(ctx, exporter, recovery)
	return result, errors.Join(recordErr, err)
}

type withdrawalOnlyApplier struct{ evpn.Type2RouteApplier }

func (withdrawalOnlyApplier) Add(context.Context, evpn.Type2Route) (string, error) {
	return "", fmt.Errorf("startup inventory denial permits withdrawal only")
}
