package nodeagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

func TestCNIExportConfigLoading(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"disabled", `{"Enabled":false}`, true},
		{"unknown", `{"Enabled":false,"Enabeld":true}`, false},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"trailing", `{"Enabled":false} {}`, false},
		{"invalid-enabled", `{"Enabled":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "export.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadExporterConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestCNIExportLocalConfiguration(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-a")
	if err := validateLocalExportConfig(evpn.ExporterConfig{NodeName: "worker-b"}); err == nil {
		t.Fatal("wrong node identity accepted")
	}
	cfg := evpn.ExporterConfig{NodeName: "worker-a", Networks: []evpn.EVPNNetworkConfig{{NetworkID: "n", Enabled: true, LocalVTEP: "192.0.2.254"}}}
	if err := validateLocalExportConfig(cfg); err == nil {
		t.Fatal("nonlocal VTEP accepted")
	}
	cfg.Networks[0].Enabled = false
	if err := validateLocalExportConfig(cfg); err != nil {
		t.Fatal(err)
	}
}
