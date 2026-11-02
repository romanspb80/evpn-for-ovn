package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
)

type readinessTestClient struct {
	api.GoBgpServiceClient
	global *api.Global
	err    error
}

func (c readinessTestClient) GetBgp(context.Context, *api.GetBgpRequest, ...grpc.CallOption) (*api.GetBgpResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &api.GetBgpResponse{Global: c.global}, nil
}

func TestGoBGPSpeakerReadinessUsesGoBGPIdentity(t *testing.T) {
	tests := []struct {
		name    string
		client  readinessTestClient
		wantErr bool
	}{
		{
			name: "ready-with-api-omitted-families",
			client: readinessTestClient{
				global: &api.Global{Asn: 64512, RouterId: "192.0.2.10"},
			},
		},
		{
			name: "wrong-asn",
			client: readinessTestClient{
				global: &api.Global{Asn: 64513, RouterId: "192.0.2.10"},
			},
			wantErr: true,
		},
		{
			name: "wrong-router-id",
			client: readinessTestClient{
				global: &api.Global{Asn: 64512, RouterId: "192.0.2.11"},
			},
			wantErr: true,
		},
		{
			name:    "api-error",
			client:  readinessTestClient{err: errors.New("unavailable")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (goBGPSpeakerReadiness{
				Client: tt.client, ASN: 64512, RouterID: "192.0.2.10",
			}).Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("readiness error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestStandaloneExampleConfigValidates(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "ovsdb-standalone.example.yaml")
	config, interval, network, importerConfig, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if interval != 2*time.Second || network.NetworkID != "m1-lab" || network.VNI != 1000 ||
		network.ProviderBridge != "br-m1" || len(importerConfig.Networks) != 1 ||
		len(config.AllowedRemoteVTEPs) != 1 || config.AllowedRemoteVTEPs[0] != "192.0.2.20" {
		t.Fatalf("unexpected standalone test configuration: config=%+v network=%+v importer=%+v",
			config, network, importerConfig)
	}
}

func TestStandaloneConfigRejectsUnsafeOrAmbiguousValues(t *testing.T) {
	config, _, _, _, err := loadConfig(filepath.Join("..", "..", "deploy", "ovsdb-standalone.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func()
	}{
		{"missing-go-bgp-endpoint", func() { config.GoBGP.Endpoint = "" }},
		{"zero-asn", func() { config.GoBGP.ASN = 0 }},
		{"invalid-poll-interval", func() { config.PollInterval = "0s" }},
		{"writer-ownership-not-acknowledged", func() { config.ExclusiveWriter = false }},
		{"multiple-remote-vteps", func() { config.AllowedRemoteVTEPs = append(config.AllowedRemoteVTEPs, "192.0.2.21") }},
		{"missing-route-target", func() { config.ImportRouteTargets = nil }},
		{"invalid-local-vtep", func() { config.LocalVTEP = "127.0.0.1" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original, _, _, _, err := loadConfig(filepath.Join("..", "..", "deploy", "ovsdb-standalone.example.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			config = original
			test.change()
			if _, _, _, err := config.validate(); err == nil {
				t.Fatal("invalid standalone configuration was accepted")
			}
		})
	}
}

func TestStandaloneConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
gobgp:
  endpoint: "127.0.0.1:50051"
  asn: 64512
  router_id: "192.0.2.10"
unsupported: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("unknown configuration field was accepted")
	}
}
