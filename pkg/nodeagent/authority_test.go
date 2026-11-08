package nodeagent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func serializeAgentAuthorityTest(t *testing.T) {
	t.Helper()
	path := filepath.Join(os.TempDir(), "evpn4ovn-nodeagent-authority-test.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatalf("open authority test lock: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		t.Fatalf("inspect authority test lock: %v", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) ||
		stat.Nlink != 1 || stat.Mode&0077 != 0 {
		unix.Close(fd)
		t.Fatalf("authority test lock must be private, single-link, regular, and owned by this user")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		t.Fatalf("lock authority test fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
			t.Errorf("unlock authority test fixture: %v", err)
		}
		if err := unix.Close(fd); err != nil {
			t.Errorf("close authority test fixture lock: %v", err)
		}
	})
}

type authorityTestService struct{ *CNIServer }

func (s authorityTestService) Add(ctx context.Context, _ *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error) {
	fmt.Println("operation-active")
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAgentAuthorityHelper(t *testing.T) {
	if os.Getenv("EVPN_AGENT_AUTHORITY_HELPER") != "1" {
		return
	}
	lock, err := AcquireAgentAuthority()
	if err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(23)
	}
	defer lock.Close()
	if path := os.Getenv("EVPN_AGENT_AUTHORITY_SOCKET"); path != "" {
		server, err := StartUnixServer(context.Background(), path, authorityTestService{NewCNIServer(nil)})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Stop(context.Background())
	}
	fmt.Println("authority-ready")
	// stdin EOF models orderly shutdown without a PID-based ownership decision.
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	fmt.Println("authority-stopping")
}

func startAuthorityOwner(t *testing.T, path string) (*exec.Cmd, func(), <-chan string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentAuthorityHelper$")
	cmd.Env = append(os.Environ(), "EVPN_AGENT_AUTHORITY_HELPER=1", "EVPN_AGENT_AUTHORITY_SOCKET="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 16)
	go func() {
		defer close(ready)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			ready <- scanner.Text()
		}
	}()
	awaitAuthorityEvent(t, ready, "authority-ready")
	return cmd, func() { _ = input.Close() }, ready
}

func awaitAuthorityEvent(t *testing.T, events <-chan string, expected string) {
	t.Helper()
	select {
	case line := <-events:
		if line != expected {
			t.Fatalf("helper event %q, want %q", line, expected)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", expected)
	}
}

func TestAgentAuthorityLifecycle(t *testing.T) {
	serializeAgentAuthorityTest(t)
	// Deliberately not parallel: production authority covers the entire netns.
	for _, mode := range []string{"graceful", "stopped", "killed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cni.sock")
			owner, stop, _ := startAuthorityOwner(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stopped" {
				if err := owner.Process.Signal(unix.SIGSTOP); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				contender := exec.Command(os.Args[0], "-test.run=^TestAgentAuthorityHelper$")
				contender.Env = append(os.Environ(), "EVPN_AGENT_AUTHORITY_HELPER=1", "EVPN_AGENT_AUTHORITY_SOCKET="+path)
				out, err := contender.CombinedOutput()
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 ||
					!strings.Contains(string(out), "RECOVERY_BLOCKED") {
					t.Fatalf("contender did not fail closed: %s %v", out, err)
				}
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("contender touched active CNI socket: %v", err)
				}
			}
			if mode == "stopped" {
				if err := owner.Process.Signal(unix.SIGCONT); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "killed" {
				if err := owner.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				stop()
			}
			err = owner.Wait()
			if mode != "killed" && err != nil {
				t.Fatal(err)
			}
			lock, err := AcquireAgentAuthority()
			if err != nil {
				t.Fatalf("kernel did not release departed owner's authority: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentAuthorityWaitsForActiveRPC(t *testing.T) {
	serializeAgentAuthorityTest(t)
	path := filepath.Join(t.TempDir(), "cni.sock")
	owner, stop, events := startAuthorityOwner(t, path)
	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := nodeagentpb.NewNodeAgentCNIClient(conn).Add(ctx, &nodeagentpb.CNIAddRequest{})
		done <- err
	}()
	awaitAuthorityEvent(t, events, "operation-active")
	stop()
	awaitAuthorityEvent(t, events, "authority-stopping")
	if lock, err := AcquireAgentAuthority(); err == nil {
		lock.Close()
		t.Fatal("authority released while old CNI operation was still running")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("test RPC did not block until cancellation")
	}
	if err := owner.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireAgentAuthority()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestAgentAuthoritySameProcessContender(t *testing.T) {
	serializeAgentAuthorityTest(t)
	lock, err := AcquireAgentAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	other, err := AcquireAgentAuthority()
	if err == nil {
		other.Close()
		t.Fatal("second authority in same process accepted")
	}
	// Closing an unrelated socket must not release this process's authority.
	unrelated, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@evpn-agent-test-unrelated", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	unrelated.Close()
	if other, err := AcquireAgentAuthority(); err == nil {
		other.Close()
		t.Fatal("unrelated Close released authority")
	}
}
