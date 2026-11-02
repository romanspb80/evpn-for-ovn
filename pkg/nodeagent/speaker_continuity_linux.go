package nodeagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type SpeakerProcessEvidence struct {
	PID        int32
	StartTicks string
	BootID     string
	Executable string
}

// unixSpeakerContinuity pins the peer of the actual gRPC transport, not a
// separately probed socket. The pidfd remains tied to that process after exit.
type unixSpeakerContinuity struct {
	mu                 sync.Mutex
	fd                 int
	bound              bool
	evidence           SpeakerProcessEvidence
	expectedExecutable string
}

func pollSpeakerPIDFD(fds []unix.PollFd, poll func([]unix.PollFd, int) (int, error)) error {
	for {
		_, err := poll(fds, 0)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

func (s *unixSpeakerContinuity) checkLocked() error {
	if !s.bound {
		return fmt.Errorf("speaker process evidence unavailable")
	}
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	err := pollSpeakerPIDFD(fds, unix.Poll)
	if err != nil {
		return fmt.Errorf("speaker pidfd check: %w", err)
	}
	if fds[0].Revents != 0 {
		return fmt.Errorf("original speaker process exited (pid=%d start=%s); receipts invalid for a new speaker", s.evidence.PID, s.evidence.StartTicks)
	}
	return nil
}

func (s *unixSpeakerContinuity) Check(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked()
}

func (s *unixSpeakerContinuity) Evidence() SpeakerProcessEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence
}

func (s *unixSpeakerContinuity) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound {
		return nil
	}
	err := unix.Close(s.fd)
	s.bound = false
	return err
}

func (s *unixSpeakerContinuity) DialContext(ctx context.Context, target string) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bound {
		if err := s.checkLocked(); err != nil {
			return nil, err
		}
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(target, "unix://"))
	if err != nil {
		return nil, err
	}
	if err := s.bind(c.(*net.UnixConn)); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (s *unixSpeakerContinuity) bind(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	fd := -1
	var peerErr error
	if err := raw.Control(func(socket uintptr) {
		cred, peerErr = unix.GetsockoptUcred(int(socket), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if peerErr == nil {
			fd, peerErr = unix.GetsockoptInt(int(socket), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		}
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return fmt.Errorf("kernel Unix peer pidfd required: %w", peerErr)
	}
	defer func() {
		if fd >= 0 {
			unix.Close(fd)
		}
	}()
	unix.CloseOnExec(fd)
	if cred.Pid <= 0 {
		return fmt.Errorf("speaker peer process not visible; shared host PID namespace required")
	}
	if s.bound && cred.Pid != s.evidence.PID {
		return fmt.Errorf("gRPC peer changed from pinned speaker process")
	}
	proc := fmt.Sprintf("/proc/%d", cred.Pid)
	exe, err := os.Readlink(proc + "/exe")
	if err != nil {
		return fmt.Errorf("read actual speaker executable: %w", err)
	}
	expected := s.expectedExecutable
	if expected == "" {
		expected = "gobgpd"
	}
	if filepath.Base(exe) != expected {
		return fmt.Errorf("Unix peer executable %q is not %q", exe, expected)
	}
	stat, err := os.ReadFile(proc + "/stat")
	if err != nil {
		return err
	}
	// comm may contain spaces or parentheses; fields after its last ')' start at 3.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 20 {
		return fmt.Errorf("incomplete speaker process stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return fmt.Errorf("invalid speaker start time: %w", err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	evidence := SpeakerProcessEvidence{PID: cred.Pid, StartTicks: fields[19], BootID: strings.TrimSpace(string(boot)), Executable: exe}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if err := pollSpeakerPIDFD(fds, unix.Poll); err != nil || fds[0].Revents != 0 {
		return fmt.Errorf("speaker peer exited while binding evidence: %v", err)
	}
	if s.bound {
		if evidence != s.evidence {
			return fmt.Errorf("speaker process evidence changed")
		}
		return s.checkLocked()
	}
	s.fd, s.bound, s.evidence = fd, true, evidence
	fd = -1
	return nil
}
