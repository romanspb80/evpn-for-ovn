package nodeagent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var standalonePeerConfigArgument = flag.String("f", "", "GoBGP configuration path used by the standalone peer test process")

type standalonePeerService struct {
	api.UnimplementedGoBgpServiceServer
	asn      uint32
	routerID string
}

func (s standalonePeerService) GetBgp(context.Context, *api.GetBgpRequest) (*api.GetBgpResponse, error) {
	return &api.GetBgpResponse{Global: &api.Global{
		Asn: s.asn, RouterId: s.routerID,
		Families: []uint32{uint32(oc.AFI_SAFI_TYPE_L2VPN_EVPN.ToInt())},
	}}, nil
}

func TestStandaloneSpeakerPeerHelper(t *testing.T) {
	path := os.Getenv("EVPN_STANDALONE_PEER_SOCKET")
	if path == "" {
		return
	}
	config, err := os.ReadFile(*standalonePeerConfigArgument)
	if err != nil || len(config) == 0 {
		t.Fatalf("read GoBGP config selected by process arguments: %v", err)
	}
	asn, err := strconv.ParseUint(os.Getenv("EVPN_STANDALONE_PEER_ASN"), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	routerID := os.Getenv("EVPN_STANDALONE_PEER_ROUTER_ID")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	api.RegisterGoBgpServiceServer(server, standalonePeerService{asn: uint32(asn), routerID: routerID})
	go func() { _ = server.Serve(listener) }()
	fmt.Println("standalone-peer-listening")
	select {}
}

type standalonePeerFixture struct {
	configPath string
	config     ProviderAttestationConfig
	socketPath string
	args       []string
}

func newStandalonePeerFixture(t *testing.T) *standalonePeerFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("standalone attestation integration requires root-owned trust files and pidfd peer inspection")
	}
	if _, err := os.Readlink("/proc/1/ns/pid"); err != nil {
		t.Skip("standalone attestation integration requires visibility of the host PID namespace")
	}
	dir, err := os.MkdirTemp("/run", "evpn-standalone-attestation-")
	if err != nil {
		t.Fatalf("create protected standalone test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	goBGPConfigPath := filepath.Join(dir, "gobgpd.yaml")
	goBGPConfig := []byte("global:\n  config:\n    as: 64512\n    router-id: 192.0.2.1\n")
	if err := os.WriteFile(goBGPConfigPath, goBGPConfig, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executableDigest, err := fileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	configDigest := sha256.Sum256(goBGPConfig)
	cgroupPath, err := readSpeakerCgroupPath(int32(os.Getpid()))
	if err != nil {
		t.Skipf("standalone attestation integration requires unified cgroup evidence: %v", err)
	}
	args := []string{executable, "-test.run=^TestStandaloneSpeakerPeerHelper$", "-f", goBGPConfigPath}
	uid := uint32(os.Geteuid())
	config := ProviderAttestationConfig{
		Mode: "standalone",
		Standalone: &StandaloneSpeakerConfig{
			InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(), ServiceUID: uuid.NewString(),
			ExpectedUID: &uid, CgroupPath: cgroupPath, ExecutablePath: executable, ExecutableSHA256: executableDigest,
			Arguments: args, GoBGPConfigPath: goBGPConfigPath,
			GoBGPConfigSHA256: hex.EncodeToString(configDigest[:]), ASN: 64512, RouterID: "192.0.2.1",
		},
	}
	fixture := &standalonePeerFixture{
		configPath: filepath.Join(dir, "attestation.json"),
		config:     config,
		socketPath: filepath.Join(dir, "gobgp.sock"),
		args:       args,
	}
	fixture.writeConfig(t)
	return fixture
}

func (f *standalonePeerFixture) writeConfig(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func TestAdministratorConfigResolutionUsesSpeakerFilesystemRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("administrator-root file resolution test requires root-owned fixtures")
	}
	agentRoot, peerRoot := filepath.Join(t.TempDir(), "agent"), filepath.Join(t.TempDir(), "peer")
	for _, root := range []string{agentRoot, peerRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "etc", "gobgp"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	agentConfig := []byte("global:\n  config:\n    as: 64513\n")
	peerConfig := []byte("global:\n  config:\n    as: 64512\n")
	configPath := "/etc/gobgp/gobgpd.yaml"
	if err := os.WriteFile(filepath.Join(agentRoot, "etc", "gobgp", "gobgpd.yaml"), agentConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerRoot, "etc", "gobgp", "gobgpd.yaml"), peerConfig, 0600); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(peerRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	config, err := openAdministratorFileAt(rootFD, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	got, err := io.ReadAll(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(peerConfig) || string(got) == string(agentConfig) {
		t.Fatalf("configuration resolution did not use the speaker's filesystem root: %q", got)
	}
}

func (f *standalonePeerFixture) start(t *testing.T, asn uint32, routerID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(f.args[0], f.args[1:]...)
	cmd.Env = append(os.Environ(),
		"EVPN_STANDALONE_PEER_SOCKET="+f.socketPath,
		"EVPN_STANDALONE_PEER_ASN="+strconv.FormatUint(uint64(asn), 10),
		"EVPN_STANDALONE_PEER_ROUTER_ID="+routerID,
	)
	stdout, err := cmd.StdoutPipe()
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
			_, _ = cmd.Process.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "standalone-peer-listening" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("standalone peer helper exited before listening")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standalone peer helper startup timed out")
	}
	return cmd
}

func (f *standalonePeerFixture) transport(t *testing.T, expectedASN uint32, expectedRouterID string) (*grpc.ClientConn, *unixSpeakerContinuity, api.GoBgpServiceClient, error) {
	t.Helper()
	return newProviderTransport(
		f.configPath, "unix://"+f.socketPath, expectedASN, expectedRouterID,
		f.config.Standalone.InstallationUID, f.config.Standalone.NodeUID,
	)
}

func (f *standalonePeerFixture) connection(guard *unixSpeakerContinuity) (*grpc.ClientConn, api.GoBgpServiceClient, error) {
	target := "unix://" + f.socketPath
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return guard.DialContext(ctx, target)
		}))
	if err != nil {
		return nil, nil, err
	}
	return conn, api.NewGoBgpServiceClient(conn), nil
}

func (f *standalonePeerFixture) expectAPI(t *testing.T, client api.GoBgpServiceClient, continuity *unixSpeakerContinuity, asn uint32, routerID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return (attestedReadiness{
		readiness:  evpn.SpeakerReadiness{Client: client, ASN: asn, RouterID: routerID},
		continuity: continuity,
	}).Check(ctx)
}

func TestStandaloneSpeakerAttestationVerifiesActualUnixPeerAndGoBGPAPI(t *testing.T) {
	fixture := newStandalonePeerFixture(t)
	fixture.start(t, 64512, "192.0.2.1")
	conn, continuity, client, err := fixture.transport(t, 64512, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer continuity.Close()
	if err := fixture.expectAPI(t, client, continuity, 64512, "192.0.2.1"); err != nil {
		t.Fatalf("pinned process and expected GoBGP API identity were rejected: %v", err)
	}
	incarnation, err := continuity.Incarnation()
	if err != nil || incarnation == "" {
		t.Fatalf("standalone process incarnation unavailable: %q, %v", incarnation, err)
	}
	evidence := continuity.Evidence()
	if evidence.Standalone == nil || evidence.Standalone.ServiceUID != fixture.config.Standalone.ServiceUID ||
		evidence.PID == 0 || evidence.StartTicks == "" || evidence.Standalone.ArgumentsSHA256 == "" {
		t.Fatalf("actual peer process identity was not retained: %+v", evidence)
	}
}

func TestStandaloneSpeakerReconnectRetainsSameAuthenticatedPeer(t *testing.T) {
	fixture := newStandalonePeerFixture(t)
	fixture.start(t, 64512, "192.0.2.1")
	firstConn, guard, firstClient, err := fixture.transport(t, 64512, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.expectAPI(t, firstClient, guard, 64512, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := firstConn.Close(); err != nil {
		t.Fatal(err)
	}
	secondConn, secondClient, err := fixture.connection(guard)
	if err != nil {
		t.Fatal(err)
	}
	defer secondConn.Close()
	defer guard.Close()
	if err := fixture.expectAPI(t, secondClient, guard, 64512, "192.0.2.1"); err != nil {
		t.Fatalf("same authenticated speaker was rejected after transport reconnect: %v", err)
	}
}

func TestStandaloneSpeakerAttestationRejectsIdentityAndPinMismatches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*StandaloneSpeakerConfig)
		want   string
	}{
		{
			name: "wrong peer UID",
			change: func(config *StandaloneSpeakerConfig) {
				wrong := *config.ExpectedUID + 1
				config.ExpectedUID = &wrong
			},
			want: "standalone Unix peer UID",
		},
		{
			name: "wrong service cgroup",
			change: func(config *StandaloneSpeakerConfig) {
				config.CgroupPath = "/untrusted.service"
			},
			want: "service cgroup differs",
		},
		{
			name: "wrong executable pin",
			change: func(config *StandaloneSpeakerConfig) {
				config.ExecutableSHA256 = strings.Repeat("f", 64)
			},
			want: "standalone Unix peer UID or executable",
		},
		{
			name: "wrong command configuration",
			change: func(config *StandaloneSpeakerConfig) {
				config.Arguments = append([]string(nil), config.Arguments...)
				config.Arguments[3] = "/run/untrusted-gobgpd.yaml"
				config.GoBGPConfigPath = "/run/untrusted-gobgpd.yaml"
			},
			want: "standalone GoBGP process arguments",
		},
		{
			name: "changed configuration digest",
			change: func(config *StandaloneSpeakerConfig) {
				config.GoBGPConfigSHA256 = strings.Repeat("f", 64)
			},
			want: "configuration SHA256",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newStandalonePeerFixture(t)
			fixture.start(t, 64512, "192.0.2.1")
			tc.change(fixture.config.Standalone)
			fixture.writeConfig(t)
			conn, continuity, client, err := fixture.transport(t, 64512, "192.0.2.1")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			defer continuity.Close()
			err = fixture.expectAPI(t, client, continuity, 64512, "192.0.2.1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mismatched standalone identity was not rejected by the expected evidence: %v", err)
			}
		})
	}
}

func TestStandaloneSpeakerAttestationBindsRuntimeASNAndRouterID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		peerASN    uint32
		peerRouter string
		want       string
	}{
		{name: "wrong API ASN", peerASN: 64513, peerRouter: "192.0.2.1", want: "speaker not ready: expected ASN="},
		{name: "wrong API router ID", peerASN: 64512, peerRouter: "192.0.2.2", want: "speaker not ready: expected ASN="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newStandalonePeerFixture(t)
			fixture.start(t, tc.peerASN, tc.peerRouter)
			conn, continuity, client, err := fixture.transport(t, 64512, "192.0.2.1")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			defer continuity.Close()
			err = fixture.expectAPI(t, client, continuity, 64512, "192.0.2.1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GoBGP API identity mismatch was not rejected: %v", err)
			}
		})
	}

	fixture := newStandalonePeerFixture(t)
	if _, _, _, err := fixture.transport(t, 64513, "192.0.2.1"); err == nil ||
		!strings.Contains(err.Error(), "ASN or router ID differs") {
		t.Fatalf("runtime ASN mismatch was not rejected against the trusted pin: %v", err)
	}
	if _, _, _, err := fixture.transport(t, 64512, "192.0.2.2"); err == nil ||
		!strings.Contains(err.Error(), "ASN or router ID differs") {
		t.Fatalf("runtime router ID mismatch was not rejected against the trusted pin: %v", err)
	}
}

func TestStandaloneSpeakerRejectsDifferentRecoveryDomainIdentity(t *testing.T) {
	fixture := newStandalonePeerFixture(t)
	_, err := newAttestedSpeakerContinuityForSpeaker(fixture.configPath, speakerRuntimeExpectation{
		ASN:             64512,
		RouterID:        "192.0.2.1",
		InstallationUID: uuid.NewString(),
		NodeUID:         fixture.config.Standalone.NodeUID,
	})
	if err == nil || !strings.Contains(err.Error(), "recovery domain") {
		t.Fatalf("standalone speaker identity was accepted for another recovery domain: %v", err)
	}
}

func TestStandaloneSpeakerRestartInvalidatesOldPidfdAndIncarnation(t *testing.T) {
	fixture := newStandalonePeerFixture(t)
	firstProcess := fixture.start(t, 64512, "192.0.2.1")
	conn, firstGuard, client, err := fixture.transport(t, 64512, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.expectAPI(t, client, firstGuard, 64512, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	firstIncarnation, err := firstGuard.Incarnation()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := firstProcess.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = firstProcess.Wait()
	if err := firstGuard.Check(context.Background()); err == nil {
		t.Fatal("stale pidfd accepted an exited speaker")
	}

	fixture.socketPath = filepath.Join(filepath.Dir(fixture.socketPath), "replacement.sock")
	fixture.start(t, 64512, "192.0.2.1")
	if c, err := firstGuard.DialContext(context.Background(), "unix://"+fixture.socketPath); err == nil {
		_ = c.Close()
		t.Fatal("old standalone continuity guard rebound to a replacement speaker")
	}
	newConn, newGuard, newClient, err := fixture.transport(t, 64512, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer newConn.Close()
	defer newGuard.Close()
	if err := fixture.expectAPI(t, newClient, newGuard, 64512, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	nextIncarnation, err := newGuard.Incarnation()
	if err != nil || nextIncarnation == firstIncarnation {
		t.Fatalf("replacement process did not receive a new incarnation: first=%q next=%q err=%v",
			firstIncarnation, nextIncarnation, err)
	}
	_ = firstGuard.Close()
}

func TestStandaloneSpeakerAttestationRejectsUntrustedTransportAndNoFallback(t *testing.T) {
	fixture := newStandalonePeerFixture(t)
	if _, _, _, err := newProviderTransport(
		fixture.configPath, "tcp://127.0.0.1:50051", 64512, "192.0.2.1",
		fixture.config.Standalone.InstallationUID, fixture.config.Standalone.NodeUID,
	); err == nil || !strings.Contains(err.Error(), "actual Unix peer") {
		t.Fatalf("TCP transport was treated as equivalent to kernel Unix peer evidence: %v", err)
	}

	fixture.start(t, 64512, "192.0.2.1")
	wrong := *fixture.config.Standalone.ExpectedUID + 1
	fixture.config.Standalone.ExpectedUID = &wrong
	fixture.writeConfig(t)
	conn, continuity, client, err := fixture.transport(t, 64512, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer continuity.Close()
	err = fixture.expectAPI(t, client, continuity, 64512, "192.0.2.1")
	if err == nil || !strings.Contains(err.Error(), "standalone Unix peer UID") ||
		strings.Contains(err.Error(), "Kubernetes identity") {
		t.Fatalf("failed standalone attestation fell back to another trust mode: %v", err)
	}
}

func TestStandaloneSpeakerPeerCredentialsFailClosed(t *testing.T) {
	if _, _, err := speakerPeerCredentials(-1); err == nil {
		t.Fatal("unavailable kernel peer credentials were accepted")
	}
	for _, cred := range []*unix.Ucred{nil, {Pid: 0, Uid: 1000}} {
		if err := validateSpeakerPeerCredentials(cred); err == nil {
			t.Fatalf("ambiguous peer credentials accepted: %+v", cred)
		}
	}
}

func TestKubernetesAttestationConfigurationRemainsLegacyByDefault(t *testing.T) {
	attestor, _, _ := providerFixture(99, 0, strings.Repeat("d", 64))
	if attestor.config.Mode != "" || attestor.config.Standalone != nil {
		t.Fatalf("legacy Kubernetes configuration unexpectedly selected another mode: %+v", attestor.config)
	}
	if err := attestor.config.validate(); err != nil {
		t.Fatalf("existing Kubernetes attestation config no longer validates: %v", err)
	}
	uid := uint32(1000)
	standalone := StandaloneSpeakerConfig{
		InstallationUID: uuid.NewString(), NodeUID: uuid.NewString(), ServiceUID: uuid.NewString(),
		ExpectedUID: &uid, CgroupPath: "/system.slice/gobgpd.service",
		ExecutablePath: "/usr/bin/gobgpd", ExecutableSHA256: strings.Repeat("a", 64),
		Arguments:       []string{"/usr/bin/gobgpd", "-f", "/etc/gobgp/gobgpd.yaml"},
		GoBGPConfigPath: "/etc/gobgp/gobgpd.yaml", GoBGPConfigSHA256: strings.Repeat("b", 64),
		ASN: 64512, RouterID: "192.0.2.1",
	}
	config := ProviderAttestationConfig{Mode: "standalone", Standalone: &standalone}
	if err := config.validate(); err != nil {
		t.Fatalf("valid standalone pin configuration rejected: %v", err)
	}
	config.NodeUID = standalone.NodeUID
	if err := config.validate(); err == nil {
		t.Fatal("mixed Kubernetes and standalone identity pins were accepted")
	}
}
