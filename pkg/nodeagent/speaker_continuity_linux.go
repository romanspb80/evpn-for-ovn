package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type SpeakerProcessEvidence struct {
	PID              int32
	StartTicks       string
	BootID           string
	Executable       string
	UID              uint32
	ExecutableSHA256 string
	PIDNamespace     string
	Workload         SpeakerWorkloadEvidence
	Standalone       *StandaloneSpeakerEvidence `json:"standalone,omitempty"`
}

type speakerRootIdentity struct {
	MountNamespace string
	Device         uint64
	Inode          uint64
}

func readSpeakerRootIdentity(pid int32) (speakerRootIdentity, error) {
	var identity speakerRootIdentity
	mountNamespace, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return identity, fmt.Errorf("read speaker mount namespace: %w", err)
	}
	var root unix.Stat_t
	if err := unix.Stat(fmt.Sprintf("/proc/%d/root", pid), &root); err != nil {
		return identity, fmt.Errorf("read speaker filesystem root identity: %w", err)
	}
	return speakerRootIdentity{MountNamespace: mountNamespace, Device: uint64(root.Dev), Inode: root.Ino}, nil
}

// unixSpeakerContinuity pins the peer of the actual gRPC transport, not a
// separately probed socket. The pidfd remains tied to that process after exit.
type unixSpeakerContinuity struct {
	mu       sync.Mutex
	fd       int
	bound    bool
	evidence SpeakerProcessEvidence
	attestor speakerAttestor
	closed   bool
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

func (s *unixSpeakerContinuity) Check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	if s.attestor == nil {
		return fmt.Errorf("provider attestation configuration required")
	}
	process, err := readSpeakerProcess(s.evidence.PID, s.evidence.UID)
	if err != nil {
		return err
	}
	process.Workload = s.evidence.Workload
	process.Standalone = s.evidence.Standalone
	if !sameSpeakerProcessEvidence(process, s.evidence) {
		return fmt.Errorf("speaker process identity changed")
	}
	identity, err := s.attestor.attest(ctx, process)
	if err != nil {
		return err
	}
	if identity.SpeakerWorkloadEvidence != s.evidence.Workload ||
		!standaloneEvidenceEqual(identity.Standalone, s.evidence.Standalone) {
		return fmt.Errorf("speaker workload identity changed")
	}
	after, err := readSpeakerProcess(s.evidence.PID, s.evidence.UID)
	if err != nil {
		return err
	}
	after.Workload = identity.SpeakerWorkloadEvidence
	after.Standalone = s.evidence.Standalone
	if !sameSpeakerProcessEvidence(after, s.evidence) {
		return fmt.Errorf("speaker changed during provider attestation")
	}
	return s.checkLocked()
}

func (s *unixSpeakerContinuity) Incarnation() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attestor == nil {
		return "", fmt.Errorf("provider attestation configuration required")
	}
	if err := s.checkLocked(); err != nil {
		return "", err
	}
	e := s.evidence
	if e.Standalone != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s/%d/%s/%s/%s/%s/%s/%s/%s/%d/%d/%s/%d/%s",
			e.BootID, e.PID, e.StartTicks, e.UID, e.ExecutableSHA256, e.Standalone.ArgumentsSHA256,
			e.Standalone.InstallationUID, e.Standalone.NodeUID, e.Standalone.CgroupPath, e.Standalone.ServiceUID,
			e.Standalone.RootMountNamespace, e.Standalone.RootDevice, e.Standalone.RootInode,
			e.Standalone.GoBGPConfigSHA256, e.Standalone.ASN, e.Standalone.RouterID)))
		return hex.EncodeToString(h[:]), nil
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s/%d/%s/%s/%s/%s/%s/%s/%s", e.BootID, e.PID, e.StartTicks,
		e.UID, e.ExecutableSHA256, e.Workload.NodeUID, e.Workload.DaemonSetUID, e.Workload.PodUID, e.Workload.ContainerID, e.Workload.ConfigUID, e.Workload.ConfigDigest)))
	return hex.EncodeToString(h[:]), nil
}

func standaloneEvidenceEqual(a StandaloneSpeakerEvidence, b *StandaloneSpeakerEvidence) bool {
	if a.ServiceUID == "" {
		return b == nil
	}
	return b != nil && a == *b
}

func sameStandaloneSpeakerEvidence(a, b *StandaloneSpeakerEvidence) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameSpeakerProcessEvidence(a, b SpeakerProcessEvidence) bool {
	aStandalone, bStandalone := a.Standalone, b.Standalone
	a.Standalone, b.Standalone = nil, nil
	return a == b && sameStandaloneSpeakerEvidence(aStandalone, bStandalone)
}

func (s *unixSpeakerContinuity) Evidence() SpeakerProcessEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence
}

func (s *unixSpeakerContinuity) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
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
	if s.closed {
		return nil, fmt.Errorf("speaker continuity closed; create a new attested transport")
	}
	if s.attestor == nil {
		return nil, fmt.Errorf("provider attestation configuration required")
	}
	if s.bound {
		if err := s.checkLocked(); err != nil {
			return nil, err
		}
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(target, "unix://"))
	if err != nil {
		return nil, err
	}
	if err := s.bind(ctx, c.(*net.UnixConn)); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (s *unixSpeakerContinuity) bind(ctx context.Context, c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	fd := -1
	var peerErr error
	if err := raw.Control(func(socket uintptr) {
		cred, fd, peerErr = speakerPeerCredentials(int(socket))
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
	evidence, err := readSpeakerProcess(cred.Pid, cred.Uid)
	if err != nil {
		return err
	}
	identity, err := s.attestor.attest(ctx, evidence)
	if err != nil {
		return err
	}
	evidence.Workload = identity.SpeakerWorkloadEvidence
	if identity.Standalone.ServiceUID != "" {
		standalone := identity.Standalone
		evidence.Standalone = &standalone
	}
	after, err := readSpeakerProcess(cred.Pid, cred.Uid)
	if err != nil {
		return err
	}
	after.Workload = identity.SpeakerWorkloadEvidence
	after.Standalone = evidence.Standalone
	if !sameSpeakerProcessEvidence(after, evidence) {
		return fmt.Errorf("speaker changed while binding provider attestation")
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if err := pollSpeakerPIDFD(fds, unix.Poll); err != nil || fds[0].Revents != 0 {
		return fmt.Errorf("speaker peer exited while binding evidence: %v", err)
	}
	if s.bound {
		if !sameSpeakerProcessEvidence(evidence, s.evidence) {
			return fmt.Errorf("speaker process evidence changed")
		}
		return s.checkLocked()
	}
	s.fd, s.bound, s.evidence = fd, true, evidence
	fd = -1
	return nil
}

func speakerPeerCredentials(socket int) (*unix.Ucred, int, error) {
	cred, err := unix.GetsockoptUcred(socket, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return nil, -1, fmt.Errorf("read kernel Unix peer credentials: %w", err)
	}
	if err := validateSpeakerPeerCredentials(cred); err != nil {
		return nil, -1, err
	}
	pidfd, err := unix.GetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	if err != nil {
		return nil, -1, fmt.Errorf("kernel Unix peer pidfd required: %w", err)
	}
	if pidfd < 0 {
		return nil, -1, fmt.Errorf("kernel Unix peer pidfd is invalid")
	}
	return cred, pidfd, nil
}

func validateSpeakerPeerCredentials(cred *unix.Ucred) error {
	if cred == nil || cred.Pid <= 0 {
		return fmt.Errorf("kernel Unix peer credentials are unavailable or ambiguous")
	}
	return nil
}

func readSpeakerProcess(pid int32, uid uint32) (SpeakerProcessEvidence, error) {
	var none SpeakerProcessEvidence
	proc := fmt.Sprintf("/proc/%d", pid)
	exe, err := os.Readlink(proc + "/exe")
	if err != nil {
		return none, fmt.Errorf("read actual speaker executable: %w", err)
	}
	f, err := os.Open(proc + "/exe")
	if err != nil {
		return none, err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return none, err
	}
	pidNamespace, err := os.Readlink(proc + "/ns/pid")
	if err != nil {
		return none, err
	}
	for _, ns := range []string{"pid", "net"} {
		actual, err := os.Readlink(proc + "/ns/" + ns)
		if err != nil {
			return none, err
		}
		host, err := os.Readlink("/proc/1/ns/" + ns)
		if err != nil || (ns == "net" && actual != host) {
			return none, fmt.Errorf("speaker requires shared host %s namespace", ns)
		}
		agent, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil || agent != host {
			return none, fmt.Errorf("attesting agent requires shared host %s namespace", ns)
		}
	}
	stat, err := os.ReadFile(proc + "/stat")
	if err != nil {
		return none, err
	}
	// comm may contain spaces or parentheses; fields after its last ')' start at 3.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return none, fmt.Errorf("invalid speaker process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return none, fmt.Errorf("incomplete speaker process stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return none, fmt.Errorf("invalid speaker start time: %w", err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return none, err
	}
	// Re-read all process UIDs: SO_PEERCRED is a connection-time snapshot,
	// whereas setuid/exec transitions after the connection must fail closed.
	status, err := os.ReadFile(proc + "/status")
	if err != nil {
		return none, err
	}
	found := false
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		uids := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(uids) != 4 {
			return none, fmt.Errorf("invalid process UID evidence")
		}
		for _, value := range uids {
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil || uint32(n) != uid {
				return none, fmt.Errorf("process UID changed from Unix peer credential")
			}
		}
		found = true
	}
	if !found || strings.TrimSpace(string(boot)) == "" {
		return none, fmt.Errorf("incomplete process evidence")
	}
	return SpeakerProcessEvidence{
		PID: pid, StartTicks: fields[19], BootID: strings.TrimSpace(string(boot)),
		Executable: exe, UID: uid, PIDNamespace: pidNamespace,
		ExecutableSHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func readSpeakerArguments(pid int32) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, fmt.Errorf("read speaker process arguments: %w", err)
	}
	if len(data) == 0 || len(data) > 64*1024 || data[len(data)-1] != 0 {
		return nil, fmt.Errorf("invalid or oversized speaker process arguments")
	}
	arguments := strings.Split(string(data[:len(data)-1]), "\x00")
	if len(arguments) == 0 {
		return nil, fmt.Errorf("speaker process arguments are empty")
	}
	return arguments, nil
}

func readSpeakerCgroupPath(pid int32) (string, error) {
	hostNamespace, err := os.Readlink("/proc/1/ns/cgroup")
	if err != nil {
		return "", fmt.Errorf("read host cgroup namespace: %w", err)
	}
	for _, path := range []string{fmt.Sprintf("/proc/%d/ns/cgroup", pid), "/proc/self/ns/cgroup"} {
		namespace, err := os.Readlink(path)
		if err != nil || namespace != hostNamespace {
			return "", fmt.Errorf("standalone speaker and attesting agent require the host cgroup namespace")
		}
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", fmt.Errorf("read standalone service cgroup: %w", err)
	}
	var path string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		if path != "" {
			return "", fmt.Errorf("ambiguous standalone service cgroup evidence")
		}
		path = strings.TrimPrefix(line, "0::")
	}
	if !strings.HasPrefix(path, "/") || filepath.Clean(path) != path || path == "/" {
		return "", fmt.Errorf("unavailable or invalid unified standalone service cgroup")
	}
	return path, nil
}

func equalSpeakerArguments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func speakerArgumentsSHA256(arguments []string) string {
	h := sha256.New()
	for _, arg := range arguments {
		_, _ = io.WriteString(h, arg)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
