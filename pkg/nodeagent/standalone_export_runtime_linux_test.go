//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent/ovnexport"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

type standaloneExportTestStore struct {
	data []byte
}

func (s *standaloneExportTestStore) Load() ([]byte, error) {
	if s.data == nil {
		return nil, endpointauth.ErrFenceStateNotFound
	}
	return append([]byte(nil), s.data...), nil
}

func (s *standaloneExportTestStore) Persist(data []byte) error {
	s.data = append([]byte(nil), data...)
	return nil
}

func (*standaloneExportTestStore) Close() error { return nil }

type standaloneExportTestSource struct {
	snapshot ovnexport.OVNEndpointSnapshot
	err      error
}

func (s standaloneExportTestSource) AuthorizationSnapshot(context.Context) (ovnexport.OVNEndpointSnapshot, error) {
	return s.snapshot, s.err
}

func TestStandaloneAuthorizationObserverRequiresDurableAcceptedSnapshot(t *testing.T) {
	observer := standaloneAuthorizationObserver{
		Source: standaloneExportTestSource{err: errors.New("must not read OVN")},
	}
	endpoints, err := observer.DesiredLocalEndpoints(context.Background())
	if err == nil || endpoints != nil {
		t.Fatalf("missing accepted policy must fail closed, got endpoints=%v err=%v", endpoints, err)
	}
}

func TestStandaloneAuthorizationObserverAdvertisesOnlyExactAuthorizedEndpoint(t *testing.T) {
	policy, local, networks, observed := standaloneExportAuthorizationFixture(t)
	fencer, err := endpointauth.OpenFencer(&standaloneExportTestStore{}, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	accepted, err := fencer.Accept(policy)
	if err != nil {
		t.Fatal(err)
	}
	observer := standaloneAuthorizationObserver{
		Source:   standaloneExportTestSource{snapshot: observed},
		Local:    local,
		Networks: networks,
		Accepted: accepted,
	}
	endpoints, err := observer.DesiredLocalEndpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 1 || endpoints[0].EndpointID != "endpoint-1" ||
		endpoints[0].Incarnation != "attachment-generation:4" {
		t.Fatalf("unexpected authorized endpoints: %#v", endpoints)
	}
	if len(observer.Observations) != 1 || observer.Observations[0].State != string(ovnexport.AuthorizationEligible) {
		t.Fatalf("unexpected authorization observation: %#v", observer.Observations)
	}
}

func TestStandaloneAuthorizationObserverRepresentsRevocationAndUncertainty(t *testing.T) {
	policy, local, networks, observed := standaloneExportAuthorizationFixture(t)
	store := &standaloneExportTestStore{}
	fencer, err := endpointauth.OpenFencer(store, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	if _, err := fencer.Accept(policy); err != nil {
		t.Fatal(err)
	}
	accepted, err := fencer.Accept(policy)
	if err != nil {
		t.Fatal(err)
	}
	observer := standaloneAuthorizationObserver{
		Source: standaloneExportTestSource{snapshot: observed},
		Local:  local, Networks: networks, Accepted: accepted,
	}
	unknown := observed
	unknown.Bindings = append([]ovnexport.OVNPortBinding(nil), observed.Bindings...)
	unknown.Bindings[0].Up = nil
	observer.Source = standaloneExportTestSource{snapshot: unknown}
	endpoints, err := observer.DesiredLocalEndpoints(context.Background())
	if err == nil || endpoints != nil {
		t.Fatalf("uncertain readiness must block reconciliation, got endpoints=%v err=%v", endpoints, err)
	}

	revoked, err := endpointauth.NewSnapshot(endpointauth.Document{
		Version: 1, Revision: 13, InstallationUID: policy.InstallationUID(),
		NodeUID: policy.NodeUID(), LocalChassis: policy.LocalChassis(),
		Endpoints: []endpointauth.EndpointRecord{},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = fencer.Accept(revoked)
	if err != nil {
		t.Fatal(err)
	}
	observer.Accepted = accepted
	observer.Source = standaloneExportTestSource{snapshot: observed}
	endpoints, err = observer.DesiredLocalEndpoints(context.Background())
	if err != nil || len(endpoints) != 0 {
		t.Fatalf("confirmed revocation must produce no desired endpoint: %#v, %v", endpoints, err)
	}
	if len(observer.Observations) != 1 || observer.Observations[0].State != string(ovnexport.AuthorizationIneligible) {
		t.Fatalf("revocation was not explicit: %#v", observer.Observations)
	}

}

func TestStandaloneAuthorizationObserverObservationFailureRetainsRouteSet(t *testing.T) {
	policy, local, networks, _ := standaloneExportAuthorizationFixture(t)
	fencer, err := endpointauth.OpenFencer(&standaloneExportTestStore{}, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	accepted, err := fencer.Accept(policy)
	if err != nil {
		t.Fatal(err)
	}
	observer := standaloneAuthorizationObserver{
		Source: standaloneExportTestSource{err: errors.New("SB read failed")},
		Local:  local, Networks: networks, Accepted: accepted,
	}
	endpoints, err := observer.DesiredLocalEndpoints(context.Background())
	if err == nil || endpoints != nil {
		t.Fatalf("incomplete OVN observation must preserve prior ownership through observer error, got %v, %v", endpoints, err)
	}
}

func TestStandaloneExportValidationRejectsPlatformAndCoexistentRuntimes(t *testing.T) {
	config := StandaloneExportRuntimeConfig{
		Enabled: true, InstallationUID: "installation-1", NodeUID: "node-1", LocalChassis: "chassis-1",
		ExporterConfigPath:          "/etc/evpn/export.json",
		AuthorizationPolicyPath:     "/etc/evpn/authorization.json",
		AuthorizationFenceDirectory: "/var/lib/evpn/authorization",
	}
	err := ValidateStandaloneExportRuntime(config, RuntimeOptions{Mode: RuntimeModeInMemory, NetworkBackend: "in-memory"}, "", "", "")
	if err == nil {
		t.Fatal("in-memory runtime must be rejected")
	}
	err = ValidateStandaloneExportRuntime(config, RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"}, "", "", "/etc/evpn/import.json")
	if err == nil {
		t.Fatal("coexisting remote importer must be rejected")
	}
}

func TestStandaloneImporterSelectionRequiresExplicitOptIn(t *testing.T) {
	for _, test := range []struct {
		name     string
		enable   bool
		importer string
		wantErr  bool
	}{
		{name: "export only", wantErr: false},
		{name: "path without opt in", importer: "/etc/evpn/import.json", wantErr: true},
		{name: "opt in without path", enable: true, wantErr: true},
		{name: "combined", enable: true, importer: "/etc/evpn/import.json", wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateStandaloneImporterSelection(
				StandaloneExportRuntimeConfig{EnableRemoteImport: test.enable},
				test.importer,
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("selection validation error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestRunStandaloneEndpointExporterRejectsMissingImporterSelection(t *testing.T) {
	config := StandaloneExportRuntimeConfig{Enabled: true, EnableRemoteImport: true}
	runtime := RuntimeOptions{
		Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth",
		OVNNBEndpoint: "unix:/unused-nb.sock", OVSDBEndpoint: "unix:/unused-ovs.sock",
	}
	err := RunStandaloneEndpointExporter(context.Background(), runtime, ExportRuntimeOptions{}, config, "", func(ExportStatus) {})
	if err == nil || !strings.Contains(err.Error(), "importer config path") {
		t.Fatalf("missing importer selection must fail before accessing files or journals: %v", err)
	}
}

func TestRunStandaloneEndpointExporterStartup(t *testing.T) {
	tests := []struct {
		name         string
		cohost       bool
		mutate       func(*RemoteRuntimeConfig)
		missing      bool
		exportOption bool
		wantError    string
	}{
		{name: "exporter only"},
		{name: "standalone config owns exporter path", exportOption: true},
		{name: "co-hosted", cohost: true},
		{name: "missing importer file", cohost: true, missing: true, wantError: "load standalone co-hosted importer config"},
		{name: "disabled importer", cohost: true, mutate: func(c *RemoteRuntimeConfig) { c.Enabled = false }, wantError: "enabled import"},
		{name: "installation mismatch", cohost: true, mutate: func(c *RemoteRuntimeConfig) {
			c.Netlink.Recovery.Domain.InstallationUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		}, wantError: "recovery identity differs"},
		{name: "network mismatch", cohost: true, mutate: func(c *RemoteRuntimeConfig) {
			c.Netlink.Networks[0].LogicalSwitch = "other-switch"
		}, wantError: "exporter mapping"},
		{name: "speaker mismatch", cohost: true, mutate: func(c *RemoteRuntimeConfig) {
			c.ASN++
		}, wantError: "same GoBGP"},
		{name: "shared journal", cohost: true, mutate: func(c *RemoteRuntimeConfig) {
			c.Netlink.StateDirectory = filepath.Join(filepath.Dir(c.AttestationConfigPath), "authorization")
		}, wantError: "separate directories"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, runtime, opts, importer, remotePath := standaloneStartupFixture(t)
			config.EnableRemoteImport = test.cohost
			if test.mutate != nil {
				test.mutate(&importer)
			}
			if test.cohost {
				if !test.missing {
					writeStandaloneStartupJSON(t, remotePath, importer)
				}
			} else {
				remotePath = ""
			}
			if test.exportOption {
				opts.ConfigPath = filepath.Join(filepath.Dir(config.ExporterConfigPath), "unused-export.json")
			}
			// A cancelled context and deliberately absent policy exercise startup
			// without opening a speaker/OVSDB socket or acquiring route authority.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var reports []ExportStatus
			err := RunStandaloneEndpointExporter(ctx, runtime, opts, config, remotePath, func(status ExportStatus) {
				reports = append(reports, status)
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || len(reports) != 0 {
					t.Fatalf("invalid startup was not rejected: reports=%v err=%v", reports, err)
				}
				if _, statErr := os.Stat(config.AuthorizationFenceDirectory); !os.IsNotExist(statErr) {
					t.Fatalf("invalid startup touched the authorization journal: %v", statErr)
				}
				return
			}
			if err != nil || len(reports) != 1 || reports[0].Error == "" ||
				len(reports[0].Result.Applied) != 0 || !reports[0].Result.OwnershipUnverified {
				t.Fatalf("valid selection did not reach the fail-closed exporter loop: reports=%v err=%v", reports, err)
			}
			fencer, err := OpenEndpointAuthorizationFencer(config.AuthorizationFenceDirectory, config.InstallationUID, config.NodeUID)
			if err != nil {
				t.Fatalf("startup did not release its durable fence: %v", err)
			}
			if err := fencer.Close(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"export", "import", "agent.sock"} {
				if _, err := os.Stat(filepath.Join(filepath.Dir(config.ExporterConfigPath), name)); !os.IsNotExist(err) {
					t.Fatalf("startup unexpectedly created route state or a CNI socket %s: %v", name, err)
				}
			}
		})
	}
}

func standaloneStartupFixture(t *testing.T) (StandaloneExportRuntimeConfig, RuntimeOptions, ExportRuntimeOptions, RemoteRuntimeConfig, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned startup configuration and durable authorization journal")
	}
	dir, err := os.MkdirTemp("/root", "evpn-standalone-startup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	config := StandaloneExportRuntimeConfig{
		Enabled: true, InstallationUID: "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID: "46b08606-9b26-4098-90d7-02c14098122b", LocalChassis: "chassis-1",
		ExporterConfigPath: filepath.Join(dir, "export.json"), AuthorizationPolicyPath: filepath.Join(dir, "missing-policy.json"),
		AuthorizationFenceDirectory: filepath.Join(dir, "authorization"),
	}
	importer := validRemoteRuntimeConfig()
	importer.AttestationConfigPath = filepath.Join(dir, "speaker.json")
	importer.Netlink.StateDirectory = filepath.Join(dir, "import")
	importer.Netlink.Recovery = &evpn.RemoteNetlinkRecovery{Domain: ownership.Domain{
		InstallationUID: config.InstallationUID, NodeUID: config.NodeUID,
	}}
	exporter := evpn.ExporterConfig{
		Enabled: true, ProcessContinuity: true, NodeName: "startup-test", LocalChassis: config.LocalChassis,
		GRPCEndpoint: importer.GRPCEndpoint, ASN: importer.ASN, RouterID: importer.RouterID,
		AttestationConfigPath: importer.AttestationConfigPath,
		Recovery: &evpn.ExportRecoveryConfig{StateDirectory: filepath.Join(dir, "export"),
			Domain: ownership.Domain{InstallationUID: config.InstallationUID, NodeUID: config.NodeUID}},
		Networks: []evpn.EVPNNetworkConfig{{
			Enabled: true, NetworkID: "n24", LogicalSwitch: "ls24b", VNI: 100, ExporterSource: "framework",
			LocalVTEP: importer.RouterID, RouteDistinguisher: "64512:100", RouteTargets: []string{"64512:100"},
		}},
	}
	uid := uint32(0)
	writeStandaloneStartupJSON(t, importer.AttestationConfigPath, ProviderAttestationConfig{
		Mode: "standalone", Standalone: &StandaloneSpeakerConfig{
			InstallationUID: config.InstallationUID, NodeUID: config.NodeUID,
			ServiceUID: "11111111-1111-4111-8111-111111111111", ExpectedUID: &uid,
			CgroupPath: "/system.slice/gobgpd.service", ExecutablePath: "/usr/bin/gobgpd",
			ExecutableSHA256: strings.Repeat("a", 64), GoBGPConfigPath: "/etc/gobgp/gobgp.conf",
			GoBGPConfigSHA256: strings.Repeat("b", 64),
			Arguments:         []string{"/usr/bin/gobgpd", "-f", "/etc/gobgp/gobgp.conf"},
			ASN:               importer.ASN, RouterID: importer.RouterID,
		},
	})
	writeStandaloneStartupJSON(t, config.ExporterConfigPath, exporter)
	runtime := RuntimeOptions{
		Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth",
		OVNNBEndpoint: "unix:" + filepath.Join(dir, "nb.sock"), OVSDBEndpoint: "unix:" + filepath.Join(dir, "ovs.sock"),
	}
	opts := ExportRuntimeOptions{Interval: time.Second, Timeout: time.Second, SBEndpoint: "unix:" + filepath.Join(dir, "sb.sock")}
	return config, runtime, opts, importer, filepath.Join(dir, "import.json")
}

func writeStandaloneStartupJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneExporterImporterPairRequiresSharedIdentityAndSeparateJournals(t *testing.T) {
	config := StandaloneExportRuntimeConfig{
		Enabled: true, EnableRemoteImport: true,
		InstallationUID: "installation-1", NodeUID: "node-1",
		AuthorizationFenceDirectory: "/var/lib/evpn/authorization",
	}
	exporter := evpn.ExporterConfig{
		Enabled: true, GRPCEndpoint: "unix:///run/gobgp/api.sock", ASN: 64512,
		RouterID: "192.0.2.1", AttestationConfigPath: "/etc/evpn/speaker.json",
		Recovery: &evpn.ExportRecoveryConfig{StateDirectory: "/var/lib/evpn/export"},
	}
	importer := RemoteRuntimeConfig{
		Enabled: true, GRPCEndpoint: exporter.GRPCEndpoint, ASN: exporter.ASN,
		RouterID: exporter.RouterID, AttestationConfigPath: exporter.AttestationConfigPath,
		Netlink: evpn.RemoteNetlinkConfig{
			Enabled: true, ExclusiveWriter: true, StateDirectory: "/var/lib/evpn/import",
			Recovery: &evpn.RemoteNetlinkRecovery{Domain: ownership.Domain{
				InstallationUID: config.InstallationUID, NodeUID: config.NodeUID,
			}},
		},
	}
	attestation := ProviderAttestationConfig{
		Mode: "standalone",
		Standalone: &StandaloneSpeakerConfig{
			InstallationUID: config.InstallationUID, NodeUID: config.NodeUID,
			ASN: exporter.ASN, RouterID: exporter.RouterID,
		},
	}
	if err := validateStandaloneExporterImporterPair(config, exporter, importer, attestation); err != nil {
		t.Fatalf("valid co-hosted standalone import/export rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*StandaloneExportRuntimeConfig, *evpn.ExporterConfig, *RemoteRuntimeConfig, *ProviderAttestationConfig)
	}{
		{
			name: "installation mismatch",
			mutate: func(_ *StandaloneExportRuntimeConfig, _ *evpn.ExporterConfig, remote *RemoteRuntimeConfig, _ *ProviderAttestationConfig) {
				remote.Netlink.Recovery.Domain.InstallationUID = "other-installation"
			},
		},
		{
			name: "shared journal",
			mutate: func(c *StandaloneExportRuntimeConfig, _ *evpn.ExporterConfig, remote *RemoteRuntimeConfig, _ *ProviderAttestationConfig) {
				remote.Netlink.StateDirectory = c.AuthorizationFenceDirectory
			},
		},
		{
			name: "speaker mismatch",
			mutate: func(_ *StandaloneExportRuntimeConfig, _ *evpn.ExporterConfig, _ *RemoteRuntimeConfig, pin *ProviderAttestationConfig) {
				pin.Standalone.ASN++
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, e, r, a := config, exporter, importer, attestation
			r.Netlink = importer.Netlink
			r.Netlink.Recovery = &evpn.RemoteNetlinkRecovery{Domain: importer.Netlink.Recovery.Domain}
			a.Standalone = &StandaloneSpeakerConfig{
				InstallationUID: attestation.Standalone.InstallationUID,
				NodeUID:         attestation.Standalone.NodeUID,
				ASN:             attestation.Standalone.ASN,
				RouterID:        attestation.Standalone.RouterID,
			}
			test.mutate(&c, &e, &r, &a)
			if err := validateStandaloneExporterImporterPair(c, e, r, a); err == nil {
				t.Fatal("invalid combined runtime was accepted")
			}
		})
	}
}

func TestLoadStandaloneExportRuntimeConfigRequiresProtectedStrictDocument(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a runtime config under a protected directory")
	}
	directory, err := os.MkdirTemp("/root", "evpn-standalone-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "standalone.json")
	content := `{"enabled":true,"installationUID":"installation-1","nodeUID":"node-1","localChassis":"chassis-1","exporterConfigPath":"/etc/evpn/export.json","authorizationPolicyPath":"/etc/evpn/endpoints.json","authorizationFenceDirectory":"/var/lib/evpn/auth"}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadStandaloneExportRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.AuthorizationPolicyPath != "/etc/evpn/endpoints.json" {
		t.Fatalf("unexpected standalone runtime config: %#v", config)
	}
	if err := os.WriteFile(path, []byte(content+` {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStandaloneExportRuntimeConfig(path); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func standaloneExportAuthorizationFixture(t *testing.T) (
	*endpointauth.Snapshot,
	ovnexport.TrustedEndpointIdentity,
	map[string]evpn.EVPNNetworkConfig,
	ovnexport.OVNEndpointSnapshot,
) {
	t.Helper()
	policy, err := endpointauth.NewSnapshot(endpointauth.Document{
		Version: 1, Revision: 12, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []endpointauth.EndpointRecord{{
			NetworkID: "net-a", EndpointID: "endpoint-1", AttachmentID: "attachment-1",
			AttachmentGeneration: 4, LogicalSwitchPort: "lsp-1",
			LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000123",
			MAC:                   "02:00:00:00:00:01", IP: "192.0.2.10",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	local := ovnexport.TrustedEndpointIdentity{
		InstallationUID: "installation-1", NodeUID: "node-1", NodeName: "node-a", LocalChassis: "chassis-1",
	}
	networks := map[string]evpn.EVPNNetworkConfig{
		"net-a": {
			NetworkID: "net-a", LogicalSwitch: "ls-net-a", Enabled: true,
			ExporterSource: "framework", VNI: 100, RouteDistinguisher: "65000:100",
			RouteTargets: []string{"65000:100"}, LocalVTEP: "192.0.2.1",
		},
	}
	up := true
	observed := ovnexport.OVNEndpointSnapshot{
		NBComplete: true, SBComplete: true,
		LSPs: []ovnexport.OVNLogicalSwitchPort{{
			UUID: "00000000-0000-4000-8000-000000000123", Name: "lsp-1",
			LogicalSwitch: "ls-net-a", Addresses: []string{"02:00:00:00:00:01 192.0.2.10"},
			ExternalIDs: map[string]string{
				"evpn.framework/network-id": "net-a", "evpn.framework/endpoint-id": "endpoint-1",
				"evpn.framework/attachment-id": "attachment-1", "evpn.framework/attachment-generation": "4",
			},
		}},
		Bindings: []ovnexport.OVNPortBinding{{
			UUID:        "00000000-0000-4000-8000-000000000456",
			LogicalPort: "lsp-1", Chassis: "chassis-1", Up: &up,
		}},
	}
	return policy, local, networks, observed
}
