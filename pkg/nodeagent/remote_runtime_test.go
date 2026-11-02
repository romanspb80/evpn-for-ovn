package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

func TestLoadRemoteRuntimeConfigStrictAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		valid   bool
	}{
		{name: "disabled", content: `{"Enabled":false}`, valid: true},
		{name: "unknown", content: `{"Enabled":false,"Enabeld":true}`},
		{name: "null", content: `null`},
		{name: "array", content: `[]`},
		{name: "trailing object", content: `{"Enabled":false} {}`},
		{name: "non-unix endpoint", content: `{"Enabled":true,"GRPCEndpoint":"127.0.0.1:50051"}`},
		{name: "enabled without policy", content: `{"Enabled":true,"GRPCEndpoint":"unix:///var/run/gobgp.sock","ASN":64512,"RouterID":"192.0.2.32"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "remote.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRemoteRuntimeConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestRemoteRuntimeDisabledDoesNotInitializeBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.json")
	if err := os.WriteFile(path, []byte(`{"Enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	var reported []RemoteRuntimeStatus
	err := (&CNIServer{}).RunRemoteType2Importer(context.Background(), RuntimeOptions{
		Mode: RuntimeModeInMemory, NetworkBackend: "in-memory",
	}, RemoteRuntimeOptions{ConfigPath: path}, func(status RemoteRuntimeStatus) {
		reported = append(reported, status)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || !reported[0].Disabled || reported[0].Error != "" {
		t.Fatalf("unexpected disabled status: %#v", reported)
	}
}

func validRemoteRuntimeConfig() RemoteRuntimeConfig {
	return RemoteRuntimeConfig{
		Enabled:      true,
		GRPCEndpoint: "unix:///var/run/gobgp/gobgp.sock",
		ASN:          64512,
		RouterID:     "192.0.2.32",
		Import: evpn.RemoteImportConfig{
			LocalVTEP: "192.0.2.32",
			Networks: []evpn.RemoteImportNetwork{{
				NetworkID:          "n24",
				VNI:                100,
				ImportRouteTargets: []string{"64512:100"},
				AllowedVTEPs:       []string{"198.51.100.32"},
			}},
		},
		Netlink: evpn.RemoteNetlinkConfig{
			Enabled:         true,
			ExclusiveWriter: true,
			StateDirectory:  "/var/lib/evpn-node-agent/remote-import",
			Networks: []evpn.RemoteNetlinkNetwork{{
				NetworkID: "n24", LogicalSwitch: "ls24b", VNI: 100,
				BridgeDevice: "replace-bridge", VXLANDevice: "replace-vxlan",
			}},
		},
	}
}

func TestValidateRemoteRuntimeOptions(t *testing.T) {
	report := func(RemoteRuntimeStatus) {}
	opts := RemoteRuntimeOptions{ConfigPath: "remote.json", Interval: time.Second, Timeout: time.Second}
	if err := validateRemoteRuntimeOptions(RuntimeOptions{Mode: RuntimeModeInMemory}, opts, RemoteRuntimeConfig{}, report); err != nil {
		t.Fatalf("disabled config should need no real runtime: %v", err)
	}
	cfg := validRemoteRuntimeConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test remote config invalid: %v", err)
	}
	if err := validateRemoteRuntimeOptions(RuntimeOptions{Mode: RuntimeModeInMemory}, opts, cfg, report); err == nil {
		t.Fatal("enabled importer accepted non-real runtime")
	}
	real := RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth", OVNNBEndpoint: "unix:/var/run/ovn/ovnnb_db.sock"}
	if err := validateRemoteRuntimeOptions(real, opts, cfg, report); err != nil {
		t.Fatalf("valid runtime rejected: %v", err)
	}
	opts.Timeout = 0
	if err := validateRemoteRuntimeOptions(real, opts, cfg, report); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if err := validateRemoteRuntimeOptions(real, RemoteRuntimeOptions{ConfigPath: "remote.json"}, cfg, report); err == nil {
		t.Fatal("zero interval/timeout accepted")
	}
	if err := validateRemoteRuntimeOptions(real, opts, cfg, nil); err == nil {
		t.Fatal("missing reporter accepted")
	}
}

func remoteMappingSwitch() nbdb.LogicalSwitch {
	return nbdb.LogicalSwitch{
		UUID: "original-switch-uuid",
		Name: "ls24b",
		OtherConfig: map[string]string{
			"dynamic-routing-vni":           "100",
			"dynamic-routing-bridge-ifname": "replace-bridge",
			"dynamic-routing-vxlan-ifname":  "replace-vxlan",
		},
	}
}

func TestRemoteNetworkMappingGate(t *testing.T) {
	networks := validRemoteRuntimeConfig().Netlink.Networks
	for _, tc := range []struct {
		name   string
		change func([]nbdb.LogicalSwitch) []nbdb.LogicalSwitch
	}{
		{"missing switch", func(_ []nbdb.LogicalSwitch) []nbdb.LogicalSwitch { return nil }},
		{"renamed switch", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].Name = "other-switch"
			return rows
		}},
		{"duplicate switch", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			return append(rows, rows[0])
		}},
		{"wrong VNI", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].OtherConfig["dynamic-routing-vni"] = "101"
			return rows
		}},
		{"wrong bridge", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].OtherConfig["dynamic-routing-bridge-ifname"] = "other-bridge"
			return rows
		}},
		{"wrong VXLAN", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].OtherConfig["dynamic-routing-vxlan-ifname"] = "other-vxlan"
			return rows
		}},
		{"missing mapping", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].OtherConfig = nil
			return rows
		}},
		{"UUID replacement", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].UUID = "replacement-switch-uuid"
			return rows
		}},
		{"missing UUID", func(rows []nbdb.LogicalSwitch) []nbdb.LogicalSwitch {
			rows[0].UUID = ""
			return rows
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &remoteNetworkMappingGate{}
			if err := gate.verify(networks, []nbdb.LogicalSwitch{remoteMappingSwitch()}); err != nil {
				t.Fatal(err)
			}
			if err := gate.verify(networks, []nbdb.LogicalSwitch{remoteMappingSwitch()}); err != nil {
				t.Fatalf("unchanged switch rejected: %v", err)
			}
			if err := gate.verify(networks, tc.change([]nbdb.LogicalSwitch{remoteMappingSwitch()})); err == nil {
				t.Fatal("invalid mapping accepted")
			}
			if gate.uuids["ls24b"] != "original-switch-uuid" {
				t.Fatal("pinned switch UUID changed")
			}
			if err := gate.verify(networks, []nbdb.LogicalSwitch{remoteMappingSwitch()}); err == nil {
				t.Fatal("mapping discontinuity did not remain fenced")
			}
		})
	}
}

func TestRemoteNetworkMappingGateInitialFailure(t *testing.T) {
	gate := &remoteNetworkMappingGate{}
	networks := validRemoteRuntimeConfig().Netlink.Networks
	if err := gate.verify(networks, nil); err == nil {
		t.Fatal("missing initial logical switch accepted")
	}
	if len(gate.uuids) != 0 {
		t.Fatal("invalid initial observation pinned")
	}
	if err := gate.verify(networks, []nbdb.LogicalSwitch{remoteMappingSwitch()}); err != nil {
		t.Fatalf("valid first mapping rejected: %v", err)
	}
}

func TestIsNodeLocalUnixEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		valid    bool
	}{
		{"unix:///var/run/gobgp.sock", true},
		{"unix:/var/run/gobgp.sock", true},
		{"tcp://127.0.0.1:50051", false},
		{"unix://remote/var/run/gobgp.sock", false},
		{"unix:///", false},
		{"unix://relative.sock", false},
		{" unix:///var/run/gobgp.sock", false},
	} {
		if got := isNodeLocalUnixEndpoint(tc.endpoint); got != tc.valid {
			t.Errorf("isNodeLocalUnixEndpoint(%q)=%v, want %v", tc.endpoint, got, tc.valid)
		}
	}
}
