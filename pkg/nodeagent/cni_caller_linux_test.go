package nodeagent

import (
	"bufio"
	"context"
	"debug/elf"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func cniCallerTestRequest() *pb.CNIAddRequest {
	return &pb.CNIAddRequest{
		Runtime: &pb.CNIRuntimeContext{ContainerId: "container", NetnsPath: "/run/netns/pod", IfName: "net1"},
		K8S:     &pb.K8SIdentity{PodUid: "uid", PodName: "pod", PodNamespace: "ns"},
	}
}

func cniCallerTestConfig() CNIAdmissionConfig {
	h := strings.Repeat("a", 64)
	return CNIAdmissionConfig{CNIExecutableSHA256: h, MultusExecutableSHA256: h, RuntimeExecutableSHA256: h}
}

func cniCallerTestEnvironment() []byte {
	return []byte("CNI_COMMAND=ADD\x00CNI_CONTAINERID=container\x00CNI_NETNS=/run/netns/pod\x00CNI_IFNAME=net1\x00CNI_ARGS=IgnoreUnknown=1;K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns\x00SECRET=never-evidence\x00")
}

func TestCNICallerConfig(t *testing.T) {
	c := cniCallerTestConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []CNIAdmissionConfig{
		{}, {CNIExecutableSHA256: strings.Repeat("a", 64)},
		{CNIExecutableSHA256: strings.Repeat("A", 64), MultusExecutableSHA256: strings.Repeat("b", 64), RuntimeExecutableSHA256: strings.Repeat("c", 64)},
	} {
		if c.validate() == nil {
			t.Fatal("missing or noncanonical executable pins accepted")
		}
	}
	for _, mutate := range []func(*CNIAdmissionConfig){
		func(c *CNIAdmissionConfig) { c.WrapperScriptPath = "/opt/cni/bin/evpn-cni" },
		func(c *CNIAdmissionConfig) { c.WrapperExecutableSHA256 = strings.Repeat("b", 64) },
		func(c *CNIAdmissionConfig) {
			c.WrapperExecutableSHA256 = strings.Repeat("b", 64)
			c.WrapperScriptSHA256 = strings.Repeat("c", 64)
			c.WrapperScriptPath = "/opt/cni/../evpn-cni"
		},
	} {
		c := cniCallerTestConfig()
		mutate(&c)
		if c.validate() == nil {
			t.Fatal("incomplete or unclean wrapper pins accepted")
		}
	}
	c.WrapperExecutableSHA256, c.WrapperScriptSHA256, c.WrapperScriptPath = strings.Repeat("b", 64), strings.Repeat("c", 64), "/opt/cni/bin/evpn-cni"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCNICallerEvidenceShape(t *testing.T) {
	e := CNIAdmissionEvidence{ContainerID: "container", PodUID: "uid", NetNSDevice: 1, NetNSInode: 2}
	for _, pid := range []int32{10, 9, 8} {
		e.Processes = append(e.Processes, CNIProcessEvidence{PID: pid, StartTicks: "100", BootID: "12345678-1234-1234-1234-123456789abc", ExecutableSHA256: strings.Repeat("a", 64)})
	}
	e.Caller = e.Processes[0]
	if err := e.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CNIAdmissionEvidence){
		func(e *CNIAdmissionEvidence) { e.Processes = nil },
		func(e *CNIAdmissionEvidence) { e.Caller.PID++ },
		func(e *CNIAdmissionEvidence) { e.Processes[1].PID = e.Caller.PID },
		func(e *CNIAdmissionEvidence) { e.Processes[1].BootID = "other" },
		func(e *CNIAdmissionEvidence) { e.Processes[1].StartTicks = "0" },
		func(e *CNIAdmissionEvidence) { e.Processes[1].ExecutableSHA256 = "" },
		func(e *CNIAdmissionEvidence) { e.NetNSInode = 0 },
		func(e *CNIAdmissionEvidence) { e.PodUID = "" },
		func(e *CNIAdmissionEvidence) { e.ContainerID = "" },
	} {
		bad := e
		bad.Processes = append([]CNIProcessEvidence(nil), e.Processes...)
		mutate(&bad)
		if bad.validate() == nil {
			t.Fatal("malformed persisted creation evidence accepted")
		}
	}
}

func TestCNICallerEnvironment(t *testing.T) {
	env, err := selectedCNIEnvironment(cniCallerTestEnvironment())
	if err != nil || matchCNIEnvironment(env, cniCallerTestRequest()) != nil {
		t.Fatalf("valid selected environment rejected: %v", err)
	}
	if len(env) != 5 || env["SECRET"] != "" {
		t.Fatal("unnecessary environment retained")
	}
	for _, data := range [][]byte{
		nil, []byte("CNI_COMMAND=ADD"), []byte(strings.Repeat("x", 64*1024+1)),
		append(cniCallerTestEnvironment(), []byte("CNI_COMMAND=ADD\x00")...),
		[]byte("CNI_COMMAND\x00"),
	} {
		if _, err := selectedCNIEnvironment(data); err == nil {
			t.Fatal("unbounded, truncated or duplicate environment accepted")
		}
	}
	for _, key := range []string{"CNI_COMMAND", "CNI_CONTAINERID", "CNI_NETNS", "CNI_IFNAME", "CNI_ARGS"} {
		t.Run(key, func(t *testing.T) {
			env, _ := selectedCNIEnvironment(cniCallerTestEnvironment())
			env[key] = "forged"
			if matchCNIEnvironment(env, cniCallerTestRequest()) == nil {
				t.Fatal("request/environment mismatch accepted")
			}
		})
	}
	for _, args := range []string{
		"K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns;K8S_POD_UID=other",
		"K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=other",
		"K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns",
		"K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns;",
	} {
		env, _ := selectedCNIEnvironment(cniCallerTestEnvironment())
		env["CNI_ARGS"] = args
		if matchCNIEnvironment(env, cniCallerTestRequest()) == nil {
			t.Fatal("ambiguous or missing Kubernetes caller identity accepted")
		}
	}
	for _, req := range []*pb.CNIAddRequest{nil, {}, {Runtime: &pb.CNIRuntimeContext{}}} {
		if matchCNIEnvironment(env, req) == nil {
			t.Fatal("incomplete request accepted")
		}
	}
}

func TestCNICallerMultusRepeatedIdentity(t *testing.T) {
	env, err := selectedCNIEnvironment(cniCallerTestEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	env["CNI_ARGS"] = "IgnoreUnknown=true;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod;K8S_POD_INFRA_CONTAINER_ID=container;K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_INFRA_CONTAINER_ID=container;K8S_POD_UID=uid;IgnoreUnknown=1;K8S_POD_NAMESPACE=ns"
	if err := matchCNIEnvironment(env, cniCallerTestRequest()); err != nil {
		t.Fatalf("identical Multus identity duplicates rejected: %v", err)
	}
	for _, key := range []string{"K8S_POD_UID", "K8S_POD_NAME", "K8S_POD_NAMESPACE", "K8S_POD_INFRA_CONTAINER_ID"} {
		t.Run(key, func(t *testing.T) {
			conflicting := make(map[string]string, len(env))
			for key, value := range env {
				conflicting[key] = value
			}
			conflicting["CNI_ARGS"] += ";" + key + "=conflicting"
			if matchCNIEnvironment(conflicting, cniCallerTestRequest()) == nil {
				t.Fatal("contradictory duplicate Kubernetes identity accepted")
			}
		})
	}
	env["CNI_ARGS"] = "K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns;K8S_POD_INFRA_CONTAINER_ID=other"
	if matchCNIEnvironment(env, cniCallerTestRequest()) == nil {
		t.Fatal("sandbox CNI_ARGS mismatch accepted")
	}
}

func TestCNICallerRequiresActualGRPCPeer(t *testing.T) {
	a := &cniCallerAuthority{config: cniCallerTestConfig(), runtimeSocket: "/run/containerd/containerd.sock"}
	for _, ctx := range []context.Context{
		context.Background(),
		metadata.NewIncomingContext(context.Background(), metadata.Pairs("pid", "1", "uid", "0", "cni-command", "ADD")),
		peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}}),
	} {
		if _, err := a.Admit(ctx, cniCallerTestRequest()); err == nil {
			t.Fatal("claims without actual Unix pidfd transport admitted")
		}
	}
	var absent *cniCallerAuthority
	if _, err := absent.Admit(context.Background(), nil); err == nil {
		t.Fatal("nil authority admitted")
	}
}

func TestCNICallerCredentialsServerOnly(t *testing.T) {
	c := newCNICallerCredentials()
	if _, _, err := c.ClientHandshake(context.Background(), "ignored", nil); err == nil {
		t.Fatal("client handshake accepted")
	}
	if c.OverrideServerName("ignored") == nil || c.Clone().Info() != c.Info() {
		t.Fatal("server-only credential semantics changed")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, _, err := c.ServerHandshake(a); err == nil {
		t.Fatal("non-Unix transport accepted")
	}
}

// Fixtures stay beneath the project, never testing.T.TempDir (which uses /tmp).
func cniCallerFixtureDir(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(".artifacts", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".artifacts", "cni-caller-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func cniCallerUnixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(cniCallerFixtureDir(t), "peer.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.AcceptUnix()
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return s, c
}

func TestCNICallerRealUnixCredentials(t *testing.T) {
	s, _ := cniCallerUnixPair(t)
	conn, info, err := newCNICallerCredentials().ServerHandshake(s)
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		if err == nil {
			conn.Close()
			t.Fatal("non-root Unix caller accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	auth := info.(*cniCallerAuthInfo)
	if auth.process.PID != int32(os.Getpid()) || auth.process.UID != 0 || checkCNIPIDFD(auth.fd) != nil {
		t.Fatal("handshake not bound to real Unix peer")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !auth.closed || checkCNIPIDFD(auth.fd) == nil {
		t.Fatal("held peer pidfd not closed with connection")
	}
	if err := conn.Close(); err != nil {
		t.Fatal("connection close not idempotent")
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: auth})
	a := &cniCallerAuthority{config: cniCallerTestConfig()}
	if _, err := a.Admit(ctx, cniCallerTestRequest()); err == nil {
		t.Fatal("closed transport admitted")
	}
}

type cniCallerTestServer struct {
	pb.UnimplementedNodeAgentCNIServer
	observed  chan *cniCallerAuthInfo
	authority *cniCallerAuthority
	result    chan error
}

func (s *cniCallerTestServer) Add(ctx context.Context, req *pb.CNIAddRequest) (*pb.CNIAddResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("gRPC peer missing")
	}
	if s.authority != nil {
		evidence, err := s.authority.Admit(ctx, req)
		if err == nil {
			err = evidence.validate()
		}
		s.result <- err
		return &pb.CNIAddResponse{}, err
	}
	a, ok := p.AuthInfo.(*cniCallerAuthInfo)
	if !ok {
		return nil, errors.New("gRPC transport authentication missing")
	}
	s.observed <- a
	return nil, errors.New("identity observed; lifecycle not wired")
}

// A private mount namespace and a project-local chroot give tests trusted
// /run ancestors without modifying ownership/mounts anywhere on the host.
func TestCNICallerCreationHelper(t *testing.T) {
	mode := os.Getenv("EVPN_CNI_CREATION_HELPER")
	if mode == "" {
		return
	}
	if mode == "multus" {
		cmd := exec.Command("/nodeagent.test", "-test.run=^TestCNICallerCreationHelper$")
		cmd.Env = []string{"EVPN_CNI_CREATION_HELPER=caller"}
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "CNI_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "caller" {
		ns, err := os.Open("/run/netns/pod")
		if err != nil {
			t.Fatal(err)
		}
		defer ns.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := grpc.DialContext(ctx, "passthrough:////run/agent.sock", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", "/run/agent.sock")
			}), grpc.WithBlock())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = pb.NewNodeAgentCNIClient(c).Add(ctx, cniCallerTestRequest())
		return
	}
	if mode == "isolate" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			t.Skipf("mount namespace capability unavailable: %v", err)
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			t.Fatal(err)
		}
		root := os.Getenv("EVPN_CNI_CREATION_ROOT")
		if root == "" || !filepath.IsAbs(root) {
			t.Fatal("fixture root missing")
		}
		if err := unix.Mount("/proc", filepath.Join(root, "proc"), "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount("/proc/self/ns/net", filepath.Join(root, "run/netns/pod"), "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount("/dev/null", filepath.Join(root, "dev/null"), "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/nodeagent.test", "-test.run=^TestCNICallerCreationHelper$", "-test.v")
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
		cmd.Dir = "/"
		cmd.Env = []string{"EVPN_CNI_CREATION_HELPER=runtime"}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	proc, err := readSpeakerProcess(int32(os.Getpid()), 0)
	if err != nil {
		t.Fatal(err)
	}
	config := CNIAdmissionConfig{CNIExecutableSHA256: proc.ExecutableSHA256, MultusExecutableSHA256: proc.ExecutableSHA256, RuntimeExecutableSHA256: proc.ExecutableSHA256}
	for _, scenario := range []string{"exact", "wrong-parent", "wrong-runtime", "wrong-env", "wrong-netns"} {
		t.Run(scenario, func(t *testing.T) {
			runtimeListener, err := net.Listen("unix", "/run/runtime.sock")
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeListener.Close()
			if err := os.Chmod("/run/runtime.sock", 0600); err != nil {
				t.Fatal(err)
			}
			agentListener, err := net.Listen("unix", "/run/agent.sock")
			if err != nil {
				t.Fatal(err)
			}
			defer agentListener.Close()
			s := grpc.NewServer(grpc.Creds(newCNICallerCredentials()))
			defer s.Stop()
			cfg := config
			if scenario == "wrong-runtime" {
				cfg.RuntimeExecutableSHA256 = strings.Repeat("f", 64)
			}
			handler := &cniCallerTestServer{authority: &cniCallerAuthority{config: cfg, runtimeSocket: "/run/runtime.sock"}, result: make(chan error, 1)}
			pb.RegisterNodeAgentCNIServer(s, handler)
			go s.Serve(agentListener)
			env := []string{
				"CNI_COMMAND=ADD", "CNI_CONTAINERID=container", "CNI_NETNS=/run/netns/pod", "CNI_IFNAME=net1",
				"CNI_ARGS=K8S_POD_UID=uid;K8S_POD_NAME=pod;K8S_POD_NAMESPACE=ns",
			}
			if scenario == "wrong-env" {
				env[1] = "CNI_CONTAINERID=forged"
			}
			if scenario == "wrong-netns" {
				env[2] = "CNI_NETNS=/proc/self/ns/net"
			}
			mode := "multus"
			if scenario == "wrong-parent" {
				mode = "caller"
			}
			cmd := exec.Command("/nodeagent.test", "-test.run=^TestCNICallerCreationHelper$")
			cmd.Env = append(env, "EVPN_CNI_CREATION_HELPER="+mode)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			select {
			case err := <-handler.result:
				if scenario == "exact" && err != nil {
					t.Fatalf("exact creation chain rejected: %v", err)
				}
				if scenario != "exact" && err == nil {
					t.Fatal("unrelated process, runtime pin, environment or namespace accepted")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("creation admission timed out")
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCNICallerCreationChain(t *testing.T) {
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Skip("root creation-time process chain test")
	}
	dir := cniCallerFixtureDir(t)
	for _, subdir := range []string{"proc", "run/netns", "dev"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "run/netns/pod"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev/null"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cniCallerCopyFixtureFile(t, exe, filepath.Join(dir, "nodeagent.test"))
	// An ordinary race-enabled test binary may use the dynamic ELF loader.
	// The fixture needs that loader and its already-loaded shared libraries,
	// regardless of the original binary's name, location or filesystem.
	binary, err := elf.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	for _, program := range binary.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(program.Open(), 4096))
		if err != nil {
			t.Fatal(err)
		}
		interpreter := strings.TrimRight(string(data), "\x00")
		if !filepath.IsAbs(interpreter) || filepath.Clean(interpreter) != interpreter {
			t.Fatal("invalid test binary ELF interpreter")
		}
		cniCallerCopyFixtureFile(t, interpreter, filepath.Join(dir, strings.TrimPrefix(interpreter, "/")))
		maps, err := os.ReadFile("/proc/self/maps")
		if err != nil {
			t.Fatal(err)
		}
		copied := map[string]bool{}
		for _, line := range strings.Split(string(maps), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 6 || !strings.HasPrefix(fields[5], "/") || !strings.Contains(fields[1], "x") {
				continue
			}
			path := fields[5]
			if path == exe || copied[path] {
				continue
			}
			copied[path] = true
			cniCallerCopyFixtureFile(t, path, filepath.Join(dir, strings.TrimPrefix(path, "/")))
		}
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-test.run=^TestCNICallerCreationHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "EVPN_CNI_CREATION_HELPER=isolate", "EVPN_CNI_CREATION_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("creation chain helper failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "mount namespace capability unavailable") {
		t.Skip(string(out))
	}
	if !strings.Contains(string(out), "--- PASS: TestCNICallerCreationHelper/exact") {
		t.Fatalf("exact chain was not executed: %s", out)
	}
}

func cniCallerCopyFixtureFile(t *testing.T, source, destination string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	if err := errors.Join(copyErr, output.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestCNICallerActualGRPCTransport(t *testing.T) {
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Skip("root:root transport test")
	}
	path, err := filepath.Abs(filepath.Join(cniCallerFixtureDir(t), "grpc.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(newCNICallerCredentials()))
	handler := &cniCallerTestServer{observed: make(chan *cniCallerAuthInfo, 1)}
	pb.RegisterNodeAgentCNIServer(s, handler)
	go s.Serve(l)
	defer s.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := grpc.DialContext(ctx, "passthrough:///"+path, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = pb.NewNodeAgentCNIClient(client).Add(ctx, cniCallerTestRequest())
	select {
	case auth := <-handler.observed:
		if auth.process.PID != int32(os.Getpid()) || checkCNIPIDFD(auth.fd) != nil {
			t.Fatal("RPC authentication is not actual Unix peer")
		}
	case <-ctx.Done():
		t.Fatal("no actual RPC transport authentication")
	}
}

func TestCNICallerNamespaceDescriptors(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned nsfs descriptor test")
	}
	ns, err := os.Open("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	st, err := cniNetNSStat(int(ns.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := heldCNINetNS(int32(os.Getpid()), st)
	if err != nil {
		t.Fatal(err)
	}
	held.Close()
	pidns, err := os.Open("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	defer pidns.Close()
	if _, err := cniNetNSStat(int(pidns.Fd())); err == nil {
		t.Fatal("PID namespace accepted as network namespace")
	}
	st.Ino++
	if f, _, err := heldCNINetNS(int32(os.Getpid()), st); err == nil {
		f.Close()
		t.Fatal("unheld namespace identity accepted")
	}
	if f, _, err := openCNINetNS("/proc/self/ns/net"); err == nil {
		f.Close()
		t.Fatal("untrusted proc symlink namespace path accepted")
	}
}

func TestCNICallerNetNSPathRejections(t *testing.T) {
	for _, path := range []string{"relative", "/", "/run/../run/netns/pod", "/run/netns//pod"} {
		if _, err := cniCanonicalNetNSPath(path); err == nil {
			t.Fatal("noncanonical namespace path accepted")
		}
	}
	for _, path := range []string{"/etc/passwd", "/dev/null", "/run/evpn-nonexistent-netns"} {
		if f, _, err := openCNINetNS(path); err == nil {
			f.Close()
			t.Fatal("non-nsfs namespace path accepted")
		}
	}
}

func TestCNICallerWrapperScript(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root wrapper script descriptor test")
	}
	dir := cniCallerFixtureDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "opt/cni/bin"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opt/cni/bin/evpn-cni")
	content := []byte("#!/bin/sh\nexec /opt/cni/bin/evpn-cni.real\n")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cfg := cniCallerTestConfig()
	cfg.WrapperScriptPath, cfg.WrapperScriptSHA256, cfg.WrapperExecutableSHA256 = "/opt/cni/bin/evpn-cni", hash, strings.Repeat("b", 64)
	script, _, err := openCNIWrapperScript(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	script.Close()
	// Exercise actual kernel shebang/interpreter argv, not a fabricated argv.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'wrapper-ready\\n'\nread answer\n"), 0755); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	shell := exec.Command("/bin/sh", absolute)
	input, err := shell.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := shell.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "wrapper-ready\n" {
		shell.Process.Kill()
		shell.Wait()
		t.Fatalf("wrapper helper unavailable: %q %v", ready, err)
	}
	command, err := readCNIWrapperCommand(int32(shell.Process.Pid), absolute)
	if err != nil || command == "" {
		shell.Process.Kill()
		shell.Wait()
		t.Fatalf("actual pinned shell script argv rejected: %v", err)
	}
	if _, err := readCNIWrapperCommand(int32(shell.Process.Pid), "/opt/cni/bin/other"); err == nil {
		shell.Process.Kill()
		shell.Wait()
		t.Fatal("different wrapper argv script accepted")
	}
	input.Close()
	shell.Wait()
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatal(err)
	}
	cfg.WrapperScriptSHA256 = strings.Repeat("f", 64)
	if f, _, err := openCNIWrapperScript(cfg, root); err == nil {
		f.Close()
		t.Fatal("wrong wrapper script hash accepted")
	}
	cfg.WrapperScriptSHA256 = hash
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openCNIWrapperScript(cfg, root); err == nil {
		f.Close()
		t.Fatal("writable wrapper script accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openCNIWrapperScript(cfg, root); err == nil {
		f.Close()
		t.Fatal("wrapper script symlink accepted")
	}
	heldRoot, _, err := openCNIHostRoot(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	heldRoot.Close()
	if f, _, err := openCNIHostRoot(-1); err == nil {
		f.Close()
		t.Fatal("unavailable wrapper process root accepted")
	}
	if _, err := readCNIWrapperCommand(int32(os.Getpid()), cfg.WrapperScriptPath); err == nil {
		t.Fatal("test executable cmdline accepted as wrapper script invocation")
	}
}

func TestCNICallerExecutableAndAncestry(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root process identity test")
	}
	process, err := readSpeakerProcess(int32(os.Getpid()), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The root test binary must be root-owned. Do not weaken ownership checks
	// merely because a development test binary was built by an ordinary user.
	if _, _, err := readCNIProcess(process.PID, strings.Repeat("f", 64)); err == nil {
		t.Fatal("wrong actual executable pin accepted")
	}
	got, parent, err := readCNIProcess(process.PID, process.ExecutableSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got != process || parent != int32(os.Getppid()) {
		t.Fatal("actual direct parent lost")
	}
	if _, err := readCNIParent(-1); err == nil {
		t.Fatal("unavailable parent accepted")
	}
}

func TestCNICallerPeerExit(t *testing.T) {
	if os.Getenv("EVPN_CNI_CALLER_HELPER") != "" {
		c, err := net.Dial("unix", os.Getenv("EVPN_CNI_CALLER_HELPER"))
		if err != nil {
			os.Exit(2)
		}
		defer c.Close()
		_, _ = c.Read(make([]byte, 1))
		return
	}
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Skip("root peer exit test")
	}
	path := filepath.Join(cniCallerFixtureDir(t), "exit.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.SetDeadline(time.Now().Add(5 * time.Second))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestCNICallerPeerExit$")
	child.Env = append(os.Environ(), "EVPN_CNI_CALLER_HELPER="+path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	socket, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	conn, info, err := newCNICallerCredentials().ServerHandshake(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := info.(*cniCallerAuthInfo)
	if auth.process.PID != int32(child.Process.Pid) {
		t.Fatal("bound to parent instead of actual CNI caller")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	if checkCNIPIDFD(auth.fd) == nil {
		t.Fatal("exited peer retained creation authority")
	}
}

var _ credentials.TransportCredentials = (*cniCallerCredentials)(nil)
