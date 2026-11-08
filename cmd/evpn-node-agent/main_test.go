package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
)

func TestRunStandaloneEVPNSelectionAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name        string
		enable      bool
		path        string
		importFails bool
		wantError   string
	}{
		{name: "exporter only"},
		{name: "co-hosted", enable: true, path: "/etc/evpn/import.json"},
		{name: "missing importer selection", enable: true, wantError: "importer config path"},
		{name: "unrequested importer", path: "/etc/evpn/import.json", wantError: "importer config path"},
		{name: "importer startup failure", enable: true, path: "/etc/evpn/import.json", importFails: true, wantError: "no such file or directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.importFails {
				test.path = filepath.Join(t.TempDir(), "missing-import.json")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			exportStarted, importStarted := make(chan string, 1), make(chan string, 1)
			exportStopped := make(chan struct{})
			runners := standaloneRunners{
				exporter: func(ctx context.Context, runtime nodeagent.RuntimeOptions, opts nodeagent.ExportRuntimeOptions,
					config nodeagent.StandaloneExportRuntimeConfig, path string, report func(nodeagent.ExportStatus)) error {
					defer close(exportStopped)
					exportStarted <- path
					if path == "" {
						return nil
					}
					<-ctx.Done()
					return nil
				},
				importer: func(ctx context.Context, runtime nodeagent.RuntimeOptions, opts nodeagent.RemoteRuntimeOptions,
					report func(nodeagent.RemoteRuntimeStatus)) error {
					importStarted <- opts.ConfigPath
					if test.importFails {
						return nodeagent.RunRemoteType2Importer(ctx, runtime, opts, report)
					}
					return nil
				},
			}
			err := runStandaloneEVPN(ctx, nodeagent.RuntimeOptions{}, nodeagent.ExportRuntimeOptions{},
				nodeagent.StandaloneExportRuntimeConfig{EnableRemoteImport: test.enable},
				nodeagent.RemoteRuntimeOptions{ConfigPath: test.path}, runners)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatal("parent failed to cancel and join its runtimes")
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if test.enable != (test.path != "") {
				if len(exportStarted) != 0 || len(importStarted) != 0 {
					t.Fatal("invalid selection started a child runtime")
				}
			} else {
				if got := <-exportStarted; got != test.path {
					t.Fatalf("exporter received importer path %q, want %q", got, test.path)
				}
				<-exportStopped
				if test.enable {
					if got := <-importStarted; got != test.path {
						t.Fatalf("importer received path %q, want %q", got, test.path)
					}
				} else if len(importStarted) != 0 {
					t.Fatal("exporter-only selection started the importer")
				}
			}
		})
	}
}

func TestStandaloneCLIDoesNotStartCNIServer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-protected standalone startup files")
	}
	dir, err := os.MkdirTemp("/root", "evpn-standalone-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	installation, node := "7b2caa55-3f84-43ad-99e9-dcaad600a6ac", "46b08606-9b26-4098-90d7-02c14098122b"
	speaker := write("speaker.json", `{"mode":"standalone","standalone":{
		"installationUID":"`+installation+`","nodeUID":"`+node+`","serviceUID":"11111111-1111-4111-8111-111111111111",
		"expectedUID":0,"cgroupPath":"/system.slice/gobgpd.service","executablePath":"/usr/bin/gobgpd",
		"executableSHA256":"`+strings.Repeat("a", 64)+`","arguments":["/usr/bin/gobgpd","-f","/etc/gobgp/gobgp.conf"],
		"goBGPConfigPath":"/etc/gobgp/gobgp.conf","goBGPConfigSHA256":"`+strings.Repeat("b", 64)+`",
		"asn":64512,"routerID":"192.0.2.32"}}`)
	exporter := write("export.json", `{"Enabled":true,"ProcessContinuity":true,"NodeName":"startup-test",
		"LocalChassis":"chassis-1","GRPCEndpoint":"unix:///run/gobgp/api.sock","ASN":64512,"RouterID":"192.0.2.32",
		"AttestationConfigPath":"`+speaker+`","Recovery":{"StateDirectory":"`+filepath.Join(dir, "export")+`",
		"Domain":{"InstallationUID":"`+installation+`","NodeUID":"`+node+`"}},
		"Networks":[{"Enabled":true,"NetworkID":"n24","LogicalSwitch":"ls24b","VNI":100,"ExporterSource":"framework",
		"LocalVTEP":"192.0.2.32","RouteDistinguisher":"64512:100","RouteTargets":["64512:100"]}]}`)
	config := nodeagent.StandaloneExportRuntimeConfig{
		Enabled: true, InstallationUID: installation, NodeUID: node, LocalChassis: "chassis-1",
		ExporterConfigPath: exporter, AuthorizationPolicyPath: filepath.Join(dir, "missing-policy.json"),
		AuthorizationFenceDirectory: filepath.Join(dir, "authorization"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := write("standalone.json", string(data))
	socket, status := filepath.Join(dir, "agent.sock"), filepath.Join(dir, "status.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStandaloneCLIChild$", "--",
		"--standalone-evpn-export-config="+path, "--mode=real-ovn-ovs", "--network-backend=linux-veth",
		"--socket="+socket, "--evpn-export-status="+status)
	cmd.Env = append(os.Environ(), "EVPN_STANDALONE_CLI_CHILD=1", "NODE_NAME=startup-test")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(status); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standalone CLI did not enter the exporter loop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("standalone CLI started the CNI socket: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("standalone CLI shutdown failed: %v\n%s", err, output.String())
	}
	if strings.Contains(output.String(), "evpn-node-agent listening") {
		t.Fatalf("standalone CLI entered the CNI server branch: %s", output.String())
	}
}

func TestStandaloneCLIChild(t *testing.T) {
	if os.Getenv("EVPN_STANDALONE_CLI_CHILD") != "1" {
		t.Skip("standalone CLI subprocess only")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
			os.Exit(run())
		}
	}
	t.Fatal("missing CLI subprocess arguments")
}

func TestValidateStandaloneImportOnly(t *testing.T) {
	validRemote := nodeagent.RemoteRuntimeConfig{
		Enabled:               true,
		AttestationConfigPath: "/etc/evpn/go-bgp-attestation.json",
	}
	tests := []struct {
		name               string
		mode, backend, cni string
		export, remotePath string
		remote             nodeagent.RemoteRuntimeConfig
		wantErr            bool
	}{
		{
			name: "enabled standalone import only", mode: nodeagent.RuntimeModeRealOVNOVS, backend: "linux-veth",
			remotePath: "/etc/evpn/remote-import.json", remote: validRemote,
		},
		{
			name: "reject in-memory runtime", mode: nodeagent.RuntimeModeInMemory, backend: "in-memory",
			remotePath: "/etc/evpn/remote-import.json", remote: validRemote, wantErr: true,
		},
		{
			name: "reject CNI continuity", mode: nodeagent.RuntimeModeRealOVNOVS, backend: "linux-veth", cni: "/etc/evpn/cni.json",
			remotePath: "/etc/evpn/remote-import.json", remote: validRemote, wantErr: true,
		},
		{
			name: "reject local endpoint export", mode: nodeagent.RuntimeModeRealOVNOVS, backend: "linux-veth", export: "/etc/evpn/export.json",
			remotePath: "/etc/evpn/remote-import.json", remote: validRemote, wantErr: true,
		},
		{
			name: "require enabled attested remote importer", mode: nodeagent.RuntimeModeRealOVNOVS, backend: "linux-veth",
			remotePath: "/etc/evpn/remote-import.json", remote: nodeagent.RemoteRuntimeConfig{Enabled: true}, wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStandaloneImportOnly(test.mode, test.backend, test.cni, test.export, test.remotePath, test.remote)
			if (err != nil) != test.wantErr {
				t.Fatalf("wantErr=%v, got %v", test.wantErr, err)
			}
		})
	}
}

func TestValidateStandaloneSpeakerAttestation(t *testing.T) {
	tests := []struct {
		name    string
		config  nodeagent.ProviderAttestationConfig
		wantErr bool
	}{
		{name: "standalone identity required", config: nodeagent.ProviderAttestationConfig{Mode: "standalone", Standalone: &nodeagent.StandaloneSpeakerConfig{}}},
		{name: "reject default Kubernetes attestation", config: nodeagent.ProviderAttestationConfig{Mode: "kubernetes"}, wantErr: true},
		{name: "reject missing standalone identity", config: nodeagent.ProviderAttestationConfig{Mode: "standalone"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStandaloneSpeakerAttestation(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("wantErr=%v, got %v", test.wantErr, err)
			}
		})
	}
}
