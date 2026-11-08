//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent/ovnexport"
	"golang.org/x/sys/unix"
)

type StandaloneExportRuntimeConfig struct {
	Enabled                     bool   `json:"enabled"`
	EnableRemoteImport          bool   `json:"enableRemoteImport,omitempty"`
	InstallationUID             string `json:"installationUID"`
	NodeUID                     string `json:"nodeUID"`
	LocalChassis                string `json:"localChassis"`
	ExporterConfigPath          string `json:"exporterConfigPath"`
	AuthorizationPolicyPath     string `json:"authorizationPolicyPath"`
	AuthorizationFenceDirectory string `json:"authorizationFenceDirectory"`
}

func LoadStandaloneExportRuntimeConfig(path string) (StandaloneExportRuntimeConfig, error) {
	dirFD, name, err := openProtectedParent(path)
	if err != nil {
		return StandaloneExportRuntimeConfig{}, err
	}
	defer unix.Close(dirFD)

	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("open standalone export config: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return StandaloneExportRuntimeConfig{}, err
	}
	if before.Uid != 0 || before.Mode&unix.S_IFMT != unix.S_IFREG ||
		before.Mode&0022 != 0 || before.Nlink != 1 ||
		before.Size <= 0 || before.Size > endpointauth.MaxDocumentSize {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("standalone export config must be a bounded, root-owned, single-link regular file not writable by group or others")
	}
	data, err := io.ReadAll(io.LimitReader(file, endpointauth.MaxDocumentSize+1))
	if err != nil {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("read standalone export config: %w", err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return StandaloneExportRuntimeConfig{}, err
	}
	if int64(len(data)) != before.Size || !sameFileSnapshot(before, after) {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("standalone export config changed while being read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var config *StandaloneExportRuntimeConfig
	if err := decoder.Decode(&config); err != nil {
		return StandaloneExportRuntimeConfig{}, err
	}
	if config == nil {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("standalone export config must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return StandaloneExportRuntimeConfig{}, fmt.Errorf("standalone export config must contain exactly one JSON object")
	}
	if err := validateStandaloneExportPaths(*config); err != nil {
		return StandaloneExportRuntimeConfig{}, err
	}
	return *config, nil
}

func validateStandaloneExportPaths(config StandaloneExportRuntimeConfig) error {
	for name, path := range map[string]string{
		"exporter config":               config.ExporterConfigPath,
		"authorization policy":          config.AuthorizationPolicyPath,
		"authorization fence directory": config.AuthorizationFenceDirectory,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("standalone %s must be a clean absolute path", name)
		}
	}
	return nil
}

func ValidateStandaloneExportRuntime(
	config StandaloneExportRuntimeConfig,
	runtime RuntimeOptions,
	cniContinuityPath, cniExportConfigPath, remoteImportConfigPath string,
) error {
	if !config.Enabled {
		return fmt.Errorf("standalone export requires explicit enabled=true")
	}
	if runtime.Mode != RuntimeModeRealOVNOVS || runtime.NetworkBackend != "linux-veth" {
		return fmt.Errorf("standalone export requires real-ovn-ovs/linux-veth")
	}
	if runtime.OVNNBEndpoint == "" || runtime.OVSDBEndpoint == "" {
		return fmt.Errorf("standalone export requires explicit OVN NBDB and OVSDB endpoints")
	}
	if cniContinuityPath != "" || cniExportConfigPath != "" {
		return fmt.Errorf("standalone export cannot be combined with CNI continuity or CNI export")
	}
	if err := validateStandaloneImporterSelection(config, remoteImportConfigPath); err != nil {
		return err
	}
	if err := validateStandaloneExportPaths(config); err != nil {
		return err
	}
	if config.InstallationUID == "" || config.NodeUID == "" || config.LocalChassis == "" {
		return fmt.Errorf("standalone export requires independent installation, node and chassis identity")
	}
	exporterConfig, err := LoadExporterConfig(config.ExporterConfigPath)
	if err != nil {
		return fmt.Errorf("load standalone exporter config: %w", err)
	}
	if err := validateStandaloneExporterConfig(config, exporterConfig); err != nil {
		return err
	}
	if config.EnableRemoteImport {
		importer, err := LoadRemoteRuntimeConfig(remoteImportConfigPath)
		if err != nil {
			return fmt.Errorf("load standalone co-hosted importer config: %w", err)
		}
		return ValidateStandaloneExporterImporter(config, importer)
	}
	return nil
}

func validateStandaloneImporterSelection(config StandaloneExportRuntimeConfig, remoteImportConfigPath string) error {
	if config.EnableRemoteImport != (remoteImportConfigPath != "") {
		return fmt.Errorf("standalone remote import requires both enableRemoteImport=true and an importer config path")
	}
	return nil
}

func ValidateStandaloneExporterImporter(
	config StandaloneExportRuntimeConfig,
	importer RemoteRuntimeConfig,
) error {
	if !config.EnableRemoteImport {
		return fmt.Errorf("standalone exporter/importer combination requires enableRemoteImport=true")
	}
	if !importer.Enabled || importer.Netlink.Recovery == nil {
		return fmt.Errorf("standalone co-hosted importer requires enabled import with durable Netlink recovery")
	}
	if err := importer.Validate(); err != nil {
		return fmt.Errorf("invalid standalone co-hosted importer: %w", err)
	}
	exporter, err := LoadExporterConfig(config.ExporterConfigPath)
	if err != nil {
		return fmt.Errorf("load standalone exporter config: %w", err)
	}
	if err := validateStandaloneExporterConfig(config, exporter); err != nil {
		return err
	}
	attestation, err := LoadProviderAttestationConfig(exporter.AttestationConfigPath)
	if err != nil {
		return fmt.Errorf("load standalone GoBGP speaker attestation: %w", err)
	}
	return validateStandaloneExporterImporterPair(config, exporter, importer, attestation)
}

func validateStandaloneExporterImporterPair(
	config StandaloneExportRuntimeConfig,
	exporter evpn.ExporterConfig,
	importer RemoteRuntimeConfig,
	attestation ProviderAttestationConfig,
) error {
	if exporter.Recovery == nil || importer.Netlink.Recovery == nil {
		return fmt.Errorf("standalone exporter and importer both require durable recovery")
	}
	if !exporter.Enabled || exporter.GRPCEndpoint != importer.GRPCEndpoint ||
		exporter.ASN != importer.ASN || exporter.RouterID != importer.RouterID ||
		filepath.Clean(exporter.AttestationConfigPath) != filepath.Clean(importer.AttestationConfigPath) {
		return fmt.Errorf("standalone exporter and importer must use the same GoBGP endpoint, identity and attestation")
	}
	if importer.Netlink.Recovery.Domain.InstallationUID != config.InstallationUID ||
		importer.Netlink.Recovery.Domain.NodeUID != config.NodeUID {
		return fmt.Errorf("standalone importer recovery identity differs from exporter installation/node identity")
	}
	exportNetworks := make(map[string]evpn.EVPNNetworkConfig, len(exporter.Networks))
	for _, network := range exporter.Networks {
		if network.Enabled {
			exportNetworks[network.NetworkID] = network
		}
	}
	for _, network := range importer.Netlink.Networks {
		exportNetwork, ok := exportNetworks[network.NetworkID]
		if !ok || exportNetwork.LogicalSwitch != network.LogicalSwitch || exportNetwork.VNI != network.VNI {
			return fmt.Errorf("standalone importer network %s differs from the enabled exporter mapping", network.NetworkID)
		}
	}
	importerJournal := filepath.Clean(importer.Netlink.StateDirectory)
	if importerJournal == filepath.Clean(config.AuthorizationFenceDirectory) ||
		importerJournal == filepath.Clean(exporter.Recovery.StateDirectory) {
		return fmt.Errorf("standalone authorization, exporter and importer journals must use separate directories")
	}
	if attestation.Mode != "standalone" || attestation.Standalone == nil ||
		attestation.Standalone.InstallationUID != config.InstallationUID ||
		attestation.Standalone.NodeUID != config.NodeUID ||
		attestation.Standalone.ASN != exporter.ASN ||
		attestation.Standalone.RouterID != exporter.RouterID {
		return fmt.Errorf("standalone importer speaker attestation differs from exporter identity")
	}
	return nil
}

func validateStandaloneExporterConfig(config StandaloneExportRuntimeConfig, exporter evpn.ExporterConfig) error {
	if !exporter.Enabled || exporter.DryRun || !exporter.ProcessContinuity ||
		exporter.Recovery == nil || exporter.AttestationConfigPath == "" {
		return fmt.Errorf("standalone export requires live process continuity, speaker attestation and durable route recovery")
	}
	if exporter.LocalChassis != config.LocalChassis ||
		exporter.Recovery.Domain.InstallationUID != config.InstallationUID ||
		exporter.Recovery.Domain.NodeUID != config.NodeUID {
		return fmt.Errorf("standalone export identity differs from exporter recovery or chassis configuration")
	}
	if filepath.Clean(exporter.Recovery.StateDirectory) == filepath.Clean(config.AuthorizationFenceDirectory) {
		return fmt.Errorf("authorization fence and route ownership journals must use separate directories")
	}
	frameworkNetworks := 0
	for _, network := range exporter.Networks {
		if !network.Enabled {
			continue
		}
		if network.ExportSource() != "framework" {
			return fmt.Errorf("standalone export supports only framework OVN endpoint observation")
		}
		frameworkNetworks++
	}
	if frameworkNetworks == 0 {
		return fmt.Errorf("standalone export requires at least one enabled trusted framework network mapping")
	}
	attestation, err := LoadProviderAttestationConfig(exporter.AttestationConfigPath)
	if err != nil {
		return fmt.Errorf("load standalone GoBGP speaker attestation: %w", err)
	}
	if attestation.Mode != "standalone" || attestation.Standalone == nil ||
		attestation.Standalone.InstallationUID != config.InstallationUID ||
		attestation.Standalone.NodeUID != config.NodeUID {
		return fmt.Errorf("standalone speaker attestation identity differs from standalone export configuration")
	}
	return nil
}

func RunStandaloneEndpointExporter(
	ctx context.Context,
	runtime RuntimeOptions,
	opts ExportRuntimeOptions,
	config StandaloneExportRuntimeConfig,
	remoteImportConfigPath string,
	report func(ExportStatus),
) error {
	if err := ValidateStandaloneExportRuntime(config, runtime, "", "", remoteImportConfigPath); err != nil {
		return err
	}
	if opts.Interval <= 0 || opts.Timeout <= 0 || opts.SBEndpoint == "" || report == nil {
		return fmt.Errorf("standalone export interval, timeout, SB endpoint and reporter required")
	}
	fencer, err := OpenEndpointAuthorizationFencer(
		config.AuthorizationFenceDirectory, config.InstallationUID, config.NodeUID,
	)
	if err != nil {
		return fmt.Errorf("open durable standalone authorization fence: %w", err)
	}
	opts.ConfigPath = config.ExporterConfigPath
	runErr := runEndpointExporter(ctx, runtime, opts, report, nil, &config, fencer)
	return errors.Join(runErr, fencer.Close())
}

type standaloneAuthorizationObserver struct {
	Source       standaloneAuthorizationSource
	Local        ovnexport.TrustedEndpointIdentity
	Networks     map[string]evpn.EVPNNetworkConfig
	Accepted     *endpointauth.AcceptedSnapshot
	Observations []ExportObservation
}

type standaloneAuthorizationSource interface {
	AuthorizationSnapshot(context.Context) (ovnexport.OVNEndpointSnapshot, error)
}

func (o *standaloneAuthorizationObserver) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	o.Observations = nil
	if o.Accepted == nil || !o.Accepted.IsCurrent() {
		return nil, fmt.Errorf("durably accepted endpoint authorization is unavailable")
	}
	observed, err := o.Source.AuthorizationSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	result := ovnexport.IntersectEndpointAuthorization(o.Accepted, o.Local, o.Networks, observed)
	if !o.Accepted.IsCurrent() {
		return nil, fmt.Errorf("durably accepted endpoint authorization changed during OVN observation")
	}
	endpoints := make([]evpn.LocalEndpoint, 0, len(result.Decisions))
	for _, decision := range result.Decisions {
		observation := ExportObservation{
			EndpointID: decision.EndpointID,
			State:      string(decision.State),
			Reason:     decision.Reason,
		}
		o.Observations = append(o.Observations, observation)
		switch decision.State {
		case ovnexport.AuthorizationEligible:
			if decision.Endpoint == nil {
				return nil, fmt.Errorf("eligible authorization decision has no endpoint")
			}
			endpoints = append(endpoints, *decision.Endpoint)
		case ovnexport.AuthorizationIneligible:
		case ovnexport.AuthorizationUncertain:
			return nil, fmt.Errorf("standalone OVN authorization is uncertain for %s/%s: %s",
				decision.NetworkID, decision.EndpointID, decision.Reason)
		default:
			return nil, fmt.Errorf("unknown standalone authorization state %q", decision.State)
		}
	}
	return endpoints, nil
}
