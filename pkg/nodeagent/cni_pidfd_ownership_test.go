package nodeagent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func pidfdOwnershipPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var result [2]*net.UnixConn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "pidfd ownership test")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[i] = c.(*net.UnixConn)
		t.Cleanup(func() { c.Close() })
	}
	return result[0], result[1]
}

func TestCNIPIDFDOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root Unix peer credentials required")
	}
	for _, scenario := range []string{"failure-zero", "success-zero", "concurrent-failures", "concurrent-rpc"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCNIPIDFDOwnershipHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "EVPN_PIDFD_OWNERSHIP_HELPER="+scenario)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func TestCNIPIDFDOwnershipHelper(t *testing.T) {
	scenario := os.Getenv("EVPN_PIDFD_OWNERSHIP_HELPER")
	if scenario == "" {
		return
	}
	// Only the isolated child alters descriptor zero.
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Dup3(int(f.Fd()), 0, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	c, peer := pidfdOwnershipPair(t)
	failed := func(int, int, int) (int, error) { return 0, unix.EINVAL }
	checkFailure := func() {
		t.Helper()
		_, fd, err := unixCNIPeerPIDFDWith(c, failed)
		if !errors.Is(err, unix.EINVAL) || fd != -1 {
			t.Errorf("failed acquisition returned fd=%d err=%v", fd, err)
		}
		if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); err != nil {
			t.Errorf("unrelated descriptor zero closed: %v", err)
		}
	}
	switch scenario {
	case "failure-zero":
		checkFailure()
	case "success-zero":
		get := func(int, int, int) (int, error) {
			fd, err := unix.PidfdOpen(os.Getpid(), 0)
			if err != nil {
				return 0, err
			}
			err = unix.Dup3(fd, 0, unix.O_CLOEXEC)
			unix.Close(fd)
			return 0, err
		}
		_, fd, err := unixCNIPeerPIDFDWith(c, get)
		if err != nil || fd != 0 {
			t.Fatalf("valid descriptor zero rejected: fd=%d err=%v", fd, err)
		}
		if err := checkCNIPIDFD(fd); err != nil {
			t.Fatal(err)
		}
		owned := &cniCallerConn{Conn: c, auth: &cniCallerAuthInfo{fd: fd}}
		if err := owned.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("owned descriptor not closed: %v", err)
		}
		if err := unix.Dup3(int(f.Fd()), 0, unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		if err := owned.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); err != nil {
			t.Fatalf("second close destroyed reused descriptor: %v", err)
		}
	case "concurrent-failures":
		provider, server := pidfdOwnershipPair(t)
		raw, err := provider.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var dupErr error
		if err := raw.Control(func(fd uintptr) { dupErr = unix.Dup3(int(fd), 0, unix.O_CLOEXEC) }); err != nil || dupErr != nil {
			t.Fatal(errors.Join(err, dupErr))
		}
		activeZero := os.NewFile(0, "independent Unix connection")
		defer activeZero.Close()
		if err := activeZero.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := server.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 128; i++ {
				checkFailure()
			}
		}()
		done := make(chan error, 1)
		go func() { _, err := io.Copy(server, server); done <- err }()
		for i := 0; i < 128; i++ {
			if _, err := activeZero.Write([]byte{byte(i)}); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if _, err := io.ReadFull(activeZero, b[:]); err != nil || b[0] != byte(i) {
				t.Fatalf("independent connection damaged: %v %v", b, err)
			}
		}
		wg.Wait()
		activeZero.Close()
		provider.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	case "concurrent-rpc":
		listener, err := net.Listen("unix", t.TempDir()+"/rpc.sock")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer()
		api.RegisterGoBgpServiceServer(server, pidfdTestSpeaker{})
		defer server.Stop()
		served := make(chan error, 1)
		go func() { served <- server.Serve(listener) }()
		client, err := grpc.NewClient("unix://"+listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 128; i++ {
				checkFailure()
			}
		}()
		for i := 0; i < 128; i++ {
			if _, err := api.NewGoBgpServiceClient(client).GetBgp(ctx, &api.GetBgpRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
		server.Stop()
		if err := <-served; err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal(scenario)
	}

	peer.Close()
}

type pidfdTestSpeaker struct {
	api.UnimplementedGoBgpServiceServer
}

func (pidfdTestSpeaker) GetBgp(context.Context, *api.GetBgpRequest) (*api.GetBgpResponse, error) {
	return &api.GetBgpResponse{Global: &api.Global{Asn: 64512, RouterId: "192.0.2.1"}}, nil
}
