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

	"golang.org/x/sys/unix"
)

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
	source.err = fmt.Errorf("API unavailable")
	if cached, err := guard.Incarnation(); err != nil || cached != first {
		t.Fatal("incarnation accessor performed full attestation")
	}
	if err := guard.Check(context.Background()); err == nil {
		t.Fatal("Check ignored unavailable API")
	}
	source.err = nil
	if ev.PID != int32(child.Process.Pid) || ev.BootID == "" || ev.StartTicks == "" {
		t.Fatalf("bad evidence: %+v", ev)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	if err := guard.Check(context.Background()); err == nil {
		t.Fatal("exited process accepted")
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
