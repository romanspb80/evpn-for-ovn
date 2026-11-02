package nodeagent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	rejected := &unixSpeakerContinuity{expectedExecutable: "not-the-speaker"}
	if c, err := rejected.DialContext(context.Background(), "unix://"+path); err == nil {
		c.Close()
		t.Fatal("wrong peer executable accepted")
	}
	if err := rejected.Check(context.Background()); err == nil {
		t.Fatal("failed binding must not establish continuity")
	}
	guard := &unixSpeakerContinuity{expectedExecutable: filepath.Base(exe)}
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
	start()
	if c, err := guard.DialContext(context.Background(), "unix://"+path); err == nil {
		c.Close()
		t.Fatal("replacement peer accepted")
	}
}
