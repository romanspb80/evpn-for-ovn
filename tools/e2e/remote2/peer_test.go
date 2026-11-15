package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
)

var peerConfig = flag.String("f", "", "offline peer's pinned GoBGP configuration")

type peerService struct {
	api.UnimplementedGoBgpServiceServer
}

func (peerService) GetBgp(context.Context, *api.GetBgpRequest) (*api.GetBgpResponse, error) {
	return &api.GetBgpResponse{Global: &api.Global{Asn: 64513, RouterId: "198.18.20.3",
		Families: []uint32{uint32(oc.AFI_SAFI_TYPE_L2VPN_EVPN.ToInt())}}}, nil
}

func (peerService) ListPath(*api.ListPathRequest, grpc.ServerStreamingServer[api.ListPathResponse]) error {
	return nil
}

type offlineProjectorStatus struct {
	Projector   projectorEvidence
	Speaker     nodeagent.SpeakerProcessEvidence
	Incarnation string
}

func TestOfflineProjectorOwnershipProcess(t *testing.T) {
	path := os.Getenv("EVPN_R2_OFFLINE_PROJECTOR")
	if path == "" {
		return
	}
	data, err := protectedRead(path)
	if err != nil {
		t.Fatal(err)
	}
	var c config
	if err := decode(data, &c); err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	digest := sha256.Sum256(data)
	domain := ownership.Domain{InstallationUID: c.InstallationUID, NodeUID: c.NodeUID,
		BootID: strings.TrimSpace(string(boot)), NetworkNamespace: ns.UniqueId(),
		ConfigDigest: hex.EncodeToString(digest[:])}
	ledger, err := openLifecycle(c, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.journal.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := connect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{
		StateDirectory: filepath.Join(c.State, "export"), Domain: domain},
		transport.adapter, transport.applier, transport.guard)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	if _, err := recovery.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	projector, err := currentProjector()
	if err != nil {
		t.Fatal(err)
	}
	incarnation, err := transport.guard.Incarnation()
	if err != nil {
		t.Fatal(err)
	}
	if err := write(filepath.Join(c.State, "offline-status.json"),
		offlineProjectorStatus{projector, transport.guard.Evidence(), incarnation}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("remote2-offline-projector-ready")
	select {}
}

func startOfflineProcess(t *testing.T, args []string, environment, readyMessage string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), environment)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if scanner.Text() == readyMessage {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("offline process failed to start")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offline process startup timeout")
	}
	return cmd
}

func TestOfflineActualPeerProcess(t *testing.T) {
	socket := os.Getenv("EVPN_R2_OFFLINE_SOCKET")
	if socket == "" {
		return
	}
	if _, err := os.ReadFile(*peerConfig); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	api.RegisterGoBgpServiceServer(server, peerService{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	fmt.Println("remote2-offline-peer-ready")
	select {}
}

func TestConnectAttestsActualReplacementAndRejectsChangedPins(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("actual Unix-peer attestation requires root-owned trust files and process visibility")
	}
	directory, err := os.MkdirTemp("/run", "evpn-r2-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exeBytes, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	exeDigest := sha256.Sum256(exeBytes)
	speakerPath := filepath.Join(directory, "gobgpd.yaml")
	speakerConfig := []byte("global:\n  config:\n    as: 64513\n    router-id: 198.18.20.3\n")
	if err := os.WriteFile(speakerPath, speakerConfig, 0600); err != nil {
		t.Fatal(err)
	}
	configDigest := sha256.Sum256(speakerConfig)
	socket := filepath.Join(directory, "peer.sock")
	args := []string{executable, "-test.run=^TestOfflineActualPeerProcess$", "-f", speakerPath}
	uid := uint32(0)
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	cgroupPath := ""
	for _, line := range strings.Split(string(cgroup), "\n") {
		if strings.HasPrefix(line, "0::") {
			cgroupPath = strings.TrimPrefix(line, "0::")
		}
	}
	if cgroupPath == "" || cgroupPath == "/" {
		t.Fatal("offline actual-peer test requires a specific unified cgroup; no identity bypass")
	}
	trust := nodeagent.ProviderAttestationConfig{Mode: "standalone", Standalone: &nodeagent.StandaloneSpeakerConfig{
		InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(), ServiceUID: uuid.NewString(),
		ExpectedUID: &uid, CgroupPath: cgroupPath, ExecutablePath: executable,
		ExecutableSHA256: hex.EncodeToString(exeDigest[:]), Arguments: args,
		GoBGPConfigPath: speakerPath, GoBGPConfigSHA256: hex.EncodeToString(configDigest[:]),
		ASN: 64513, RouterID: "198.18.20.3"}}
	c := config{Socket: "unix://" + socket, Attestation: filepath.Join(directory, "attestation.json"),
		InstallationUID: trust.Standalone.InstallationUID, NodeUID: trust.Standalone.NodeUID}
	publish(t, c.Attestation, trust)
	start := func() *exec.Cmd {
		t.Helper()
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return startOfflineProcess(t, args, "EVPN_R2_OFFLINE_SOCKET="+socket, "remote2-offline-peer-ready")
	}
	dial := func() (*transport, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return connect(ctx, c)
	}
	first := start()
	old, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldID, err := old.guard.Incarnation()
	if err != nil {
		t.Fatal(err)
	}
	oldEvidence := old.guard.Evidence()
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("peer process did not terminate")
	}
	if err := old.guard.Check(context.Background()); err == nil {
		t.Fatal("old pidfd remained trusted")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	second := start()
	replacement, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	newID, err := replacement.guard.Incarnation()
	if err != nil || newID == oldID || replacement.guard.Evidence().PID == oldEvidence.PID {
		t.Fatalf("new actual peer reused process incarnation: %s %s %v", oldID, newID, err)
	}
	for _, field := range []string{"executable", "arguments", "cgroup", "configuration", "installation"} {
		t.Run(field, func(t *testing.T) {
			data, err := json.Marshal(trust)
			if err != nil {
				t.Fatal(err)
			}
			var changed nodeagent.ProviderAttestationConfig
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "executable":
				changed.Standalone.ExecutableSHA256 = strings.Repeat("a", 64)
			case "arguments":
				changed.Standalone.Arguments = append(changed.Standalone.Arguments, "--untrusted")
			case "cgroup":
				changed.Standalone.CgroupPath = "/untrusted.service"
			case "configuration":
				changed.Standalone.GoBGPConfigSHA256 = strings.Repeat("b", 64)
			case "installation":
				changed.Standalone.InstallationUID = uuid.NewString()
			}
			publish(t, c.Attestation, changed)
			untrusted, err := dial()
			if untrusted != nil {
				untrusted.Close()
			}
			if err == nil {
				t.Fatalf("%s pin mismatch accepted", field)
			}
			publish(t, c.Attestation, trust)
			// Failed partial initialization must not poison a later valid dial.
			trusted, err := dial()
			if err != nil {
				t.Fatalf("valid attestation after rejected initialization: %v", err)
			}
			trusted.Close()
		})
	}
	t.Run("combined-projector-and-speaker-processes", func(t *testing.T) {
		state := filepath.Join(directory, "projector")
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		projectorConfig := c
		projectorConfig.State = state
		projectorConfig.Endpoint.Incarnation = uuid.NewString()
		path := filepath.Join(state, "remote2.json")
		publish(t, path, projectorConfig)
		projectorArgs := []string{executable, "-test.run=^TestOfflineProjectorOwnershipProcess$"}
		firstProjector := startOfflineProcess(t, projectorArgs, "EVPN_R2_OFFLINE_PROJECTOR="+path,
			"remote2-offline-projector-ready")
		readStatus := func() offlineProjectorStatus {
			t.Helper()
			data, err := protectedRead(filepath.Join(state, "offline-status.json"))
			if err != nil {
				t.Fatal(err)
			}
			var status offlineProjectorStatus
			if err := decode(data, &status); err != nil {
				t.Fatal(err)
			}
			return status
		}
		before := readStatus()
		for _, process := range []*exec.Cmd{firstProjector, second} {
			if err := process.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := process.Wait(); err == nil {
				t.Fatal("combined restart did not terminate both original processes")
			}
		}
		start()
		startOfflineProcess(t, projectorArgs, "EVPN_R2_OFFLINE_PROJECTOR="+path,
			"remote2-offline-projector-ready")
		after := readStatus()
		if after.Projector.PID == before.Projector.PID || after.Speaker.PID == before.Speaker.PID ||
			after.Incarnation == before.Incarnation || after.Projector.StartTicks == "" {
			t.Fatalf("combined restart did not replace both identities: before=%+v after=%+v", before, after)
		}
	})
}
