package nodeagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type speakerReadinessError struct{ err error }

func (g speakerReadinessError) Check(context.Context) error { return g.err }

func TestAttestedReadinessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rpcCanceled := fmt.Errorf("speaker GetBgp: %w", status.Error(codes.Canceled, "caller canceled"))
	gate := attestedReadiness{readiness: speakerReadinessError{rpcCanceled}}
	if !inventoryObservationCanceled(gate.Check(ctx)) {
		t.Fatal("gRPC caller cancellation became a trust incident")
	}
	if inventoryObservationCanceled(gate.Check(context.Background())) {
		t.Fatal("remote cancellation without caller cancellation was trusted")
	}
	identityFailure := errors.New("speaker identity differs")
	gate.readiness = speakerReadinessError{identityFailure}
	if err := gate.Check(ctx); err != identityFailure {
		t.Fatal("caller cancellation masked a real identity failure")
	}
}

func TestAttestedReadinessUnboundTransportFailure(t *testing.T) {
	temporary := &inventoryFailure{Cause: "speaker-transport-unavailable", Err: unix.ENOENT}
	guard := &unixSpeakerContinuity{dialFailure: temporary}
	gate := attestedReadiness{readiness: speakerReadinessError{status.Error(codes.Unavailable, "dial failed")}, continuity: guard}
	if cause, security := inventoryFailureClass(gate.Check(context.Background())); cause != temporary.Cause || security {
		t.Fatal("actual unbound transport failure was not preserved")
	}
	gate.readiness = speakerReadinessError{errors.New("speaker ASN differs")}
	if _, security := inventoryFailureClass(gate.Check(context.Background())); !security {
		t.Fatal("stale dial failure masked an observed speaker identity mismatch")
	}
}

func TestSpeakerPIDFDPollInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		revents int16
		err     error
	}{
		{name: "alive"},
		{name: "exited", revents: unix.POLLIN},
		{name: "invalid", err: unix.EBADF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			fds := []unix.PollFd{{Fd: 42, Events: unix.POLLIN}}
			err := pollSpeakerPIDFD(fds, func(got []unix.PollFd, timeout int) (int, error) {
				calls++
				if len(got) != 1 || got[0].Fd != 42 || got[0].Events != unix.POLLIN || timeout != 0 {
					t.Fatal("pidfd or nonblocking check changed")
				}
				if calls <= 2 {
					return -1, unix.EINTR
				}
				if calls > 3 {
					t.Fatal("retried a completed check")
				}
				got[0].Revents = tc.revents
				return 0, tc.err
			})
			if err != tc.err || fds[0].Revents != tc.revents || calls != 3 {
				t.Fatalf("poll outcome lost: err=%v revents=%d calls=%d", err, fds[0].Revents, calls)
			}
		})
	}
}

func TestSpeakerPeerHelper(t *testing.T) {
	path := os.Getenv("EVPN_PEER_HELPER_SOCKET")
	if path == "" {
		return
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fmt.Println("peer-listening")
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func TestSpeakerProcessContinuity(t *testing.T) {
	if _, err := os.Readlink("/proc/1/ns/pid"); err != nil {
		t.Skip("requires root visibility of host PID namespace; run the race test binary as root")
	}
	path := filepath.Join(t.TempDir(), "peer.sock")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	start := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestSpeakerPeerHelper$")
		cmd.Env = append(os.Environ(), "EVPN_PEER_HELPER_SOCKET="+path)
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
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		ready := make(chan bool, 1)
		go func() {
			s := bufio.NewScanner(out)
			for s.Scan() {
				if s.Text() == "peer-listening" {
					ready <- true
					return
				}
			}
			ready <- false
		}()
		select {
		case ok := <-ready:
			if !ok {
				t.Fatal("helper did not listen")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("helper startup timeout")
		}
		return cmd
	}
	child := start()
	process, err := readSpeakerProcess(int32(child.Process.Pid), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	a, source, _ := providerFixture(int32(child.Process.Pid), uint32(os.Geteuid()), process.ExecutableSHA256)
	source.runtime.PIDNamespace = process.PIDNamespace
	rejectedAttestor := *a
	rejectedAttestor.config.ExecutableSHA256 = strings.Repeat("f", 64)
	rejected := &unixSpeakerContinuity{attestor: &rejectedAttestor}
	if c, err := rejected.DialContext(context.Background(), "unix://"+path); err == nil {
		c.Close()
		t.Fatal("wrong peer executable accepted")
	}
	if err := rejected.Check(context.Background()); err == nil {
		t.Fatal("failed binding must not establish continuity")
	}
	guard := &unixSpeakerContinuity{attestor: a}
	defer guard.Close()
	if err := guard.Check(context.Background()); err == nil {
		t.Fatal("unbound guard accepted")
	}
	for i := 0; i < 2; i++ {
		c, err := guard.DialContext(context.Background(), "unix://"+path)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if err := guard.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ev := guard.Evidence()
	first, err := guard.Incarnation()
	if err != nil {
		t.Fatal(err)
	}
	again, err := guard.Incarnation()
	if err != nil || first != again {
		t.Fatal("incarnation not stable")
	}
	rpcFailure := status.Error(codes.DeadlineExceeded, "paused speaker")
	readiness := attestedReadiness{readiness: speakerReadinessError{rpcFailure}, continuity: guard}
	if cause, security := inventoryFailureClass(readiness.Check(context.Background())); cause != "attested-speaker-rpc-unavailable" || security {
		t.Fatal("freshly attested same-peer RPC outage was not availability")
	}
	source.err = fmt.Errorf("API unavailable")
	guard.dialFailure = &inventoryFailure{Cause: "speaker-transport-unavailable", Err: unix.ECONNREFUSED}
	if _, security := inventoryFailureClass(readiness.Check(context.Background())); !security {
		t.Fatal("RPC outage masked independent attestation failure")
	}
	if cached, err := guard.Incarnation(); err != nil || cached != first {
		t.Fatal("incarnation accessor performed full attestation")
	}
	if err := guard.Check(context.Background()); err == nil {
		t.Fatal("Check ignored unavailable API")
	}
	missingProcess := &os.PathError{Op: "readlink", Path: fmt.Sprintf("/proc/%d/ns/pid", child.Process.Pid), Err: os.ErrNotExist}
	// I2/I4: disappearing proc evidence alone is not availability proof.
	if got := speakerProcessFailure(missingProcess, int32(child.Process.Pid), guard.fd); got != missingProcess {
		t.Fatal("live peer proc failure was downgraded")
	}
	source.err = nil
	if ev.PID != int32(child.Process.Pid) || ev.BootID == "" || ev.StartTicks == "" {
		t.Fatalf("bad evidence: %+v", ev)
	}
	exited := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		exited <- child.Process.Kill()
	}()
	if cause, security := inventoryFailureClass(speakerProcessFailure(missingProcess,
		int32(child.Process.Pid), guard.fd)); cause != "verified-speaker-exit" || security {
		t.Fatal("proc teardown preceding pidfd readiness lost verified exit")
	}
	if err := <-exited; err != nil {
		t.Fatal(err)
	}
	child.Wait()
	if cause, security := inventoryFailureClass(guard.Check(context.Background())); cause != "verified-speaker-exit" || security {
		t.Fatal("pidfd-confirmed exit was not classified as temporary")
	}
	if cause, security := inventoryFailureClass(speakerProcessFailure(fmt.Errorf("attestation: %w", missingProcess),
		int32(child.Process.Pid), guard.fd)); cause != "verified-speaker-exit" || security {
		t.Fatal("proc disappearance race lost verified process-exit classification")
	}
	// I2/I4: death cannot mask an independently observed integrity failure.
	for _, failure := range []error{
		&os.PathError{Op: "open", Path: "/etc/evpn/provider.json", Err: os.ErrNotExist},
		&os.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/ns/pid", child.Process.Pid+1), Err: os.ErrNotExist},
		&os.PathError{Op: "open", Path: missingProcess.Path, Err: os.ErrPermission},
		errors.New("speaker workload identity changed"),
		os.ErrNotExist,
	} {
		if got := speakerProcessFailure(failure, int32(child.Process.Pid), guard.fd); got != failure {
			t.Fatalf("unrelated integrity error downgraded after peer exit: %v", failure)
		}
	}
	for _, fd := range []int{0x7fffffff, -1} {
		if got := speakerProcessFailure(missingProcess, int32(child.Process.Pid), fd); got != missingProcess {
			t.Fatal("invalid pidfd supplied availability proof")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement := start()
	if c, err := guard.DialContext(context.Background(), "unix://"+path); err == nil {
		c.Close()
		t.Fatal("replacement peer accepted")
	}
	source.runtime.PIDs = []int32{int32(replacement.Process.Pid)}
	newGuard := &unixSpeakerContinuity{attestor: a}
	defer newGuard.Close()
	c, err := newGuard.DialContext(context.Background(), "unix://"+path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	next, err := newGuard.Incarnation()
	if err != nil || next == first {
		t.Fatal("new speaker process incarnation not distinguished")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err := guard.DialContext(context.Background(), "unix://"+path); err == nil {
		c.Close()
		t.Fatal("closed guard rebound")
	}
}
