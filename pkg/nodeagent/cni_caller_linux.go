package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// CNIAdmissionConfig contains administrator pins, never caller claims.
type CNIAdmissionConfig struct {
	CNIExecutableSHA256     string `json:"cniExecutableSHA256"`
	MultusExecutableSHA256  string `json:"multusExecutableSHA256"`
	RuntimeExecutableSHA256 string `json:"runtimeExecutableSHA256"`
	WrapperExecutableSHA256 string `json:"wrapperExecutableSHA256,omitempty"`
	WrapperScriptSHA256     string `json:"wrapperScriptSHA256,omitempty"`
	WrapperScriptPath       string `json:"wrapperScriptPath,omitempty"`
}

func (c CNIAdmissionConfig) validate() error {
	if !validSHA256(c.CNIExecutableSHA256) || !validSHA256(c.MultusExecutableSHA256) || !validSHA256(c.RuntimeExecutableSHA256) {
		return errors.New("CNI admission requires all three administrator executable SHA256 pins")
	}
	if c.WrapperExecutableSHA256 != "" || c.WrapperScriptSHA256 != "" || c.WrapperScriptPath != "" {
		if !validSHA256(c.WrapperExecutableSHA256) || !validSHA256(c.WrapperScriptSHA256) ||
			!filepath.IsAbs(c.WrapperScriptPath) || filepath.Clean(c.WrapperScriptPath) != c.WrapperScriptPath || c.WrapperScriptPath == "/" {
			return errors.New("CNI wrapper requires pinned shell executable, script SHA256 and clean absolute administrator script path")
		}
	}
	return nil
}

// CNIProcessEvidence deliberately excludes environment, arguments and paths.
type CNIProcessEvidence struct {
	PID              int32
	StartTicks       string
	ExecutableSHA256 string
	BootID           string
}

// CNIAdmissionEvidence is a creation-time observation, not a reusable grant.
// Processes is ordered caller, optional pinned wrapper, Multus, actual CRI peer.
type CNIAdmissionEvidence struct {
	Caller                  CNIProcessEvidence
	Processes               []CNIProcessEvidence
	NetNSDevice, NetNSInode uint64
	ContainerID, PodUID     string
}

// validate checks persisted evidence shape only. It cannot establish current
// caller authority or authorize replay; only a fresh Admit can do that.
func (e CNIAdmissionEvidence) validate() error {
	if (len(e.Processes) != 3 && len(e.Processes) != 4) || e.Caller != e.Processes[0] ||
		e.NetNSDevice == 0 || e.NetNSInode == 0 || e.ContainerID == "" || len(e.ContainerID) > 256 ||
		e.PodUID == "" || len(e.PodUID) > 128 || strings.TrimSpace(e.ContainerID) != e.ContainerID || strings.TrimSpace(e.PodUID) != e.PodUID {
		return errors.New("incomplete creation-time CNI admission evidence")
	}
	seen := map[int32]bool{}
	for _, p := range e.Processes {
		ticks, err := strconv.ParseUint(p.StartTicks, 10, 64)
		_, bootErr := uuid.Parse(p.BootID)
		if p.PID <= 0 || seen[p.PID] || err != nil || ticks == 0 || bootErr != nil ||
			p.BootID != e.Caller.BootID || !validSHA256(p.ExecutableSHA256) {
			return errors.New("invalid creation-time CNI process evidence")
		}
		seen[p.PID] = true
	}
	return nil
}

type cniCallerAuthInfo struct {
	mu      sync.Mutex
	fd      int
	closed  bool
	process SpeakerProcessEvidence
}

func (*cniCallerAuthInfo) AuthType() string { return "evpn-cni-unix-pidfd" }

type cniCallerConn struct {
	net.Conn
	auth *cniCallerAuthInfo
}

func (c *cniCallerConn) Close() error {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if !c.auth.closed {
		c.auth.closed = true
		return errors.Join(unix.Close(c.auth.fd), c.Conn.Close())
	}
	return nil
}

type cniCallerCredentials struct{}

func newCNICallerCredentials() credentials.TransportCredentials { return &cniCallerCredentials{} }
func (*cniCallerCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("CNI caller credentials support server handshakes only")
}
func (*cniCallerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "evpn-cni-unix-pidfd", SecurityVersion: "1"}
}
func (*cniCallerCredentials) Clone() credentials.TransportCredentials {
	return newCNICallerCredentials()
}
func (*cniCallerCredentials) OverrideServerName(string) error {
	return errors.New("CNI Unix credentials do not support server names")
}

// unixCNIPeerPIDFD binds credentials to the actual transport's peer. There is
// intentionally no pidfd_open fallback for a possibly recycled SO_PEERCRED PID.
func unixCNIPeerPIDFD(c *net.UnixConn) (*unix.Ucred, int, error) {
	return unixCNIPeerPIDFDWith(c, unix.GetsockoptInt)
}

func unixCNIPeerPIDFDWith(c *net.UnixConn, getInt func(int, int, int) (int, error)) (*unix.Ucred, int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, -1, err
	}
	var cred *unix.Ucred
	fd := -1
	var controlErr error
	err = raw.Control(func(socket uintptr) {
		cred, controlErr = unix.GetsockoptUcred(int(socket), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if controlErr == nil {
			controlErr = validateCRIPeer(cred)
		}
		if controlErr == nil {
			var acquired int
			acquired, controlErr = getInt(int(socket), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
			if controlErr == nil {
				fd = acquired
			}
		}
	})
	if err = errors.Join(err, controlErr); err != nil {
		if fd >= 0 {
			unix.Close(fd)
		}
		return nil, -1, fmt.Errorf("root:root Unix peer and SO_PEERPIDFD required: %w", err)
	}
	unix.CloseOnExec(fd)
	return cred, fd, nil
}

func checkCNIPIDFD(fd int) error {
	if fd < 0 {
		return errors.New("CNI process pidfd unavailable")
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if err := pollSpeakerPIDFD(fds, unix.Poll); err != nil {
		return err
	}
	if fds[0].Revents != 0 {
		return errors.New("CNI authority process exited or pidfd unavailable")
	}
	return nil
}

func (*cniCallerCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, nil, errors.New("CNI authority requires actual Unix transport")
	}
	cred, fd, err := unixCNIPeerPIDFD(c)
	if err != nil {
		return nil, nil, err
	}
	executable, statErr := os.Stat(fmt.Sprintf("/proc/%d/exe", cred.Pid))
	if statErr != nil || executable.Size() <= 0 || executable.Size() > 256*1024*1024 {
		unix.Close(fd)
		return nil, nil, errors.New("bounded real CNI peer executable required")
	}
	process, err := readSpeakerProcess(cred.Pid, 0)
	if err = errors.Join(err, checkCNIPIDFD(fd)); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	auth := &cniCallerAuthInfo{fd: fd, process: process}
	return &cniCallerConn{Conn: conn, auth: auth}, auth, nil
}

type cniCallerAuthority struct {
	config        CNIAdmissionConfig
	runtimeSocket string
}

func cniPublicProcess(p SpeakerProcessEvidence) CNIProcessEvidence {
	return CNIProcessEvidence{PID: p.PID, StartTicks: p.StartTicks, ExecutableSHA256: p.ExecutableSHA256, BootID: p.BootID}
}

func readCNIProcess(pid int32, hash string) (SpeakerProcessEvidence, int32, error) {
	var none SpeakerProcessEvidence
	st, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || st.Size() <= 0 || st.Size() > 256*1024*1024 {
		return none, 0, errors.New("bounded CNI authority executable required")
	}
	p, err := readSpeakerProcess(pid, 0)
	if err != nil {
		return none, 0, err
	}
	if p.ExecutableSHA256 != hash {
		return none, 0, fmt.Errorf("CNI authority executable pin mismatch (pid=%d actualSHA256=%s expectedSHA256=%s)",
			pid, p.ExecutableSHA256, hash)
	}
	for _, ns := range []string{"pid", "net", "user"} {
		actual, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, ns))
		host, hostErr := os.Readlink("/proc/1/ns/" + ns)
		agent, agentErr := os.Readlink("/proc/self/ns/" + ns)
		if err != nil || hostErr != nil || agentErr != nil || actual != host || agent != host {
			return none, 0, errors.New("CNI authority requires shared host PID, network and user namespaces")
		}
	}
	// The held real executable's ownership and pinned bytes are authority;
	// pathname/argv[0] is not. A replaced pathname cannot replace this inode.
	actual, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return none, 0, err
	}
	defer actual.Close()
	var running unix.Stat_t
	if err := unix.Fstat(int(actual.Fd()), &running); err != nil {
		return none, 0, err
	}
	digest, err := fileSHA256(actual)
	if err != nil {
		return none, 0, fmt.Errorf("CNI authority executable hash read failed (pid=%d uid=%d mode=%#o): %w",
			pid, running.Uid, running.Mode, err)
	}
	if digest != hash || running.Uid != 0 || running.Mode&0022 != 0 || running.Mode&unix.S_IFMT != unix.S_IFREG {
		return none, 0, fmt.Errorf("CNI authority executable is not root-owned pinned bytes (pid=%d uid=%d mode=%#o regular=%t hashMismatch=%t actualSHA256=%s expectedSHA256=%s)",
			pid, running.Uid, running.Mode, running.Mode&unix.S_IFMT == unix.S_IFREG, digest != hash, digest, hash)
	}
	parent, err := readCNIParent(pid)
	return p, parent, err
}

func readCNIParent(pid int32) (int32, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, errors.New("invalid CNI process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, errors.New("incomplete CNI process stat")
	}
	n, err := strconv.ParseInt(fields[1], 10, 32)
	if err != nil || n <= 0 {
		return 0, errors.New("CNI process parent unavailable")
	}
	return int32(n), nil
}

func readCNIWrapperCommand(pid int32, script string) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) == 0 || len(data) > 4096 || data[len(data)-1] != 0 {
		return "", errors.New("bounded complete wrapper command required")
	}
	args := strings.Split(string(data[:len(data)-1]), "\x00")
	if len(args) != 2 || args[0] == "" || args[1] != script {
		return "", errors.New("wrapper must execute exactly the administrator-pinned script")
	}
	return string(data), nil
}

type cniRootIdentity struct{ Device, Inode, MountID uint64 }

func cniRootStat(f *os.File) (cniRootIdentity, error) {
	var st unix.Stat_t
	var sx unix.Statx_t
	err := errors.Join(unix.Fstat(int(f.Fd()), &st),
		unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &sx))
	if err != nil || st.Uid != 0 || st.Mode&0022 != 0 || st.Mode&unix.S_IFMT != unix.S_IFDIR ||
		sx.Mask&unix.STATX_MNT_ID == 0 || sx.Mnt_id == 0 {
		return cniRootIdentity{}, errors.New("root-owned trusted process root mount identity required")
	}
	return cniRootIdentity{uint64(st.Dev), st.Ino, sx.Mnt_id}, nil
}

// /proc/PID/root is a deliberate kernel reference, anchored by the held pidfd.
// It is not passed through the administrator pathname loader as a symlink.
func openCNIHostRoot(pid int32) (*os.File, cniRootIdentity, error) {
	var none cniRootIdentity
	fd, err := unix.Open(fmt.Sprintf("/proc/%d/root", pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, none, err
	}
	root := os.NewFile(uintptr(fd), "held CNI process root")
	identity, err := cniRootStat(root)
	if err != nil {
		root.Close()
		return nil, none, err
	}
	hostFD, err := unix.Open("/proc/1/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		root.Close()
		return nil, none, err
	}
	host := os.NewFile(uintptr(hostFD), "held host root")
	defer host.Close()
	hostIdentity, err := cniRootStat(host)
	if err != nil || identity != hostIdentity {
		root.Close()
		return nil, none, errors.New("CNI wrapper root is not the independently held host root mount")
	}
	return root, identity, nil
}

func openCNIWrapperScript(config CNIAdmissionConfig, root *os.File) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, st, err
	}
	unix.CloseOnExec(fd)
	defer func() { unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(config.WrapperScriptPath, "/"), "/")
	for i, part := range parts {
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i == len(parts)-1 {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		}
		next, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			return nil, st, err
		}
		err = unix.Fstat(next, &st)
		if err != nil || st.Uid != 0 || st.Mode&0022 != 0 || (i == len(parts)-1 &&
			(st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Size <= 0 || st.Size > 64*1024)) {
			unix.Close(next)
			return nil, st, errors.New("pinned bounded root-owned no-symlink wrapper script required")
		}
		unix.Close(fd)
		fd = next
	}
	f := os.NewFile(uintptr(fd), "held CNI wrapper script")
	fd = -1
	hash, err := fileSHA256(f)
	if err != nil || hash != config.WrapperScriptSHA256 {
		f.Close()
		return nil, st, errors.New("wrapper script pin mismatch")
	}
	return f, st, nil
}

func readCNIEnvironment(pid int32) (map[string]string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return nil, err
	}
	return selectedCNIEnvironment(data)
}

func selectedCNIEnvironment(data []byte) (map[string]string, error) {
	if len(data) == 0 || len(data) > 64*1024 || data[len(data)-1] != 0 {
		return nil, errors.New("bounded complete CNI environment required")
	}
	selected := map[string]string{}
	for _, field := range strings.Split(string(data[:len(data)-1]), "\x00") {
		key, value, ok := strings.Cut(field, "=")
		switch key {
		case "CNI_COMMAND", "CNI_CONTAINERID", "CNI_NETNS", "CNI_IFNAME", "CNI_ARGS":
			if !ok {
				return nil, errors.New("invalid selected CNI environment")
			}
			if _, duplicate := selected[key]; duplicate {
				return nil, errors.New("duplicate selected CNI environment")
			}
			selected[key] = value
		}
	}
	return selected, nil
}

func matchCNIEnvironment(env map[string]string, req *pb.CNIAddRequest) error {
	if req == nil || req.GetRuntime() == nil || req.GetK8S() == nil {
		return errors.New("CNI runtime and Kubernetes request identity required")
	}
	r, k := req.GetRuntime(), req.GetK8S()
	if env["CNI_COMMAND"] != "ADD" || r.GetContainerId() == "" || r.GetNetnsPath() == "" || r.GetIfName() == "" ||
		env["CNI_CONTAINERID"] != r.GetContainerId() || env["CNI_NETNS"] != r.GetNetnsPath() || env["CNI_IFNAME"] != r.GetIfName() {
		return errors.New("actual caller CNI environment does not match ADD request")
	}
	args := map[string]string{}
	for _, field := range strings.Split(env["CNI_ARGS"], ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			return errors.New("invalid caller CNI_ARGS")
		}
		switch key {
		case "K8S_POD_UID", "K8S_POD_NAME", "K8S_POD_NAMESPACE", "K8S_POD_INFRA_CONTAINER_ID":
			if previous, duplicate := args[key]; duplicate && previous != value {
				return errors.New("conflicting caller Kubernetes CNI_ARGS")
			}
			args[key] = value
		}
	}
	if k.GetPodUid() == "" || k.GetPodName() == "" || k.GetPodNamespace() == "" ||
		args["K8S_POD_UID"] != k.GetPodUid() || args["K8S_POD_NAME"] != k.GetPodName() || args["K8S_POD_NAMESPACE"] != k.GetPodNamespace() {
		return errors.New("caller Kubernetes CNI_ARGS do not match request")
	}
	if sandbox, present := args["K8S_POD_INFRA_CONTAINER_ID"]; present && sandbox != r.GetContainerId() {
		return errors.New("caller Kubernetes sandbox CNI_ARGS do not match request")
	}
	return nil
}

// Only the conventional root-owned /var/run -> /run alias is accepted. Every
// canonical ancestor is independently checked by the no-symlink admin walker.
func cniCanonicalNetNSPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", errors.New("clean absolute CNI network namespace path required")
	}
	if strings.HasPrefix(path, "/var/run/") {
		parent, err := openAdministratorDirectory("/var")
		if err != nil {
			return "", err
		}
		defer parent.Close()
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), "run", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return "", err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			target, err := os.Readlink("/var/run")
			if err != nil || st.Uid != 0 || target != "/run" {
				return "", errors.New("untrusted /var/run alias")
			}
			path = "/run/" + strings.TrimPrefix(path, "/var/run/")
		}
	}
	return path, nil
}

func openCNINetNS(path string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	canonical, err := cniCanonicalNetNSPath(path)
	if err != nil {
		return nil, st, err
	}
	parent, err := openAdministratorDirectory(filepath.Dir(canonical))
	if err != nil {
		return nil, st, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(canonical), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, st, err
	}
	f := os.NewFile(uintptr(fd), canonical)
	st, err = cniNetNSStat(fd)
	var original unix.Stat_t
	if err = errors.Join(err, unix.Stat(path, &original)); err != nil || original.Dev != st.Dev || original.Ino != st.Ino {
		f.Close()
		return nil, st, errors.New("CNI network namespace path identity unavailable or changed")
	}
	return f, st, nil
}

func cniNetNSStat(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := errors.Join(unix.Fstat(fd, &st), unix.Fstatfs(fd, &fs)); err != nil {
		return st, err
	}
	kind, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil || kind != unix.CLONE_NEWNET || fs.Type != unix.NSFS_MAGIC || st.Uid != 0 {
		return st, errors.New("root-owned nsfs CLONE_NEWNET descriptor required")
	}
	return st, nil
}

func heldCNINetNS(pid int32, expected unix.Stat_t) (*os.File, string, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	f, err := os.Open(dir)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	// Bound both work and procfs memory consumption. No arbitrary ancestor scan.
	names, err := f.Readdirnames(1025)
	if err != nil && err != io.EOF {
		return nil, "", err
	}
	if len(names) > 1024 {
		return nil, "", errors.New("CNI caller descriptor table exceeds admission bound")
	}
	for _, name := range names {
		if _, err := strconv.ParseUint(name, 10, 32); err != nil {
			continue
		}
		path := dir + "/" + name
		// Check identity before opening: never open unrelated pipes or devices.
		var st unix.Stat_t
		if unix.Stat(path, &st) != nil || st.Dev != expected.Dev || st.Ino != expected.Ino {
			continue
		}
		ns, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		actual, err := cniNetNSStat(int(ns.Fd()))
		if err == nil && actual.Dev == expected.Dev && actual.Ino == expected.Ino {
			return ns, path, nil
		}
		ns.Close()
	}
	return nil, "", errors.New("actual CNI caller holds no matching network namespace descriptor")
}

// Admit verifies evpn-cni -> optional pinned script shell -> Multus -> CRI peer.
// Kubernetes object ownership is intentionally a separate lifecycle check.
func (a *cniCallerAuthority) Admit(ctx context.Context, req *pb.CNIAddRequest) (CNIAdmissionEvidence, error) {
	var none CNIAdmissionEvidence
	ctx, cancelAdmission := context.WithTimeout(ctx, 5*time.Second)
	defer cancelAdmission()
	if a == nil {
		return none, errors.New("CNI caller authority unavailable")
	}
	if err := a.config.validate(); err != nil {
		return none, err
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return none, errors.New("actual gRPC CNI Unix peer required")
	}
	auth, ok := p.AuthInfo.(*cniCallerAuthInfo)
	if !ok || auth == nil {
		return none, errors.New("actual gRPC CNI pidfd authentication required")
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.closed {
		return none, errors.New("CNI transport closed")
	}
	if err := errors.Join(ctx.Err(), checkCNIPIDFD(auth.fd)); err != nil {
		return none, err
	}
	caller, parentPID, err := readCNIProcess(auth.process.PID, a.config.CNIExecutableSHA256)
	if err != nil {
		return none, err
	}
	if caller != auth.process {
		return none, errors.New("CNI caller changed since actual transport handshake")
	}
	env, err := readCNIEnvironment(caller.PID)
	if err != nil {
		return none, err
	}
	if err := matchCNIEnvironment(env, req); err != nil {
		return none, err
	}
	type processHandle struct {
		process SpeakerProcessEvidence
		parent  int32
		fd      int
	}
	handles := []processHandle{{caller, parentPID, auth.fd}}
	var script *os.File
	var scriptStat unix.Stat_t
	var wrapperRoot *os.File
	var wrapperRootIdentity cniRootIdentity
	var wrapperCommand string
	if a.config.WrapperScriptPath != "" {
		wrapperFD, err := unix.PidfdOpen(int(parentPID), 0)
		if err != nil {
			return none, fmt.Errorf("wrapper parent pidfd required: %w", err)
		}
		defer unix.Close(wrapperFD)
		wrapper, multusPID, err := readCNIProcess(parentPID, a.config.WrapperExecutableSHA256)
		if err != nil {
			return none, err
		}
		wrapperCommand, err = readCNIWrapperCommand(parentPID, a.config.WrapperScriptPath)
		if err != nil {
			return none, err
		}
		wrapperRoot, wrapperRootIdentity, err = openCNIHostRoot(parentPID)
		if err != nil {
			return none, err
		}
		defer wrapperRoot.Close()
		script, scriptStat, err = openCNIWrapperScript(a.config, wrapperRoot)
		if err != nil {
			return none, err
		}
		defer script.Close()
		handles = append(handles, processHandle{wrapper, multusPID, wrapperFD})
		parentPID = multusPID
	}
	parentFD, err := unix.PidfdOpen(int(parentPID), 0)
	if err != nil {
		return none, fmt.Errorf("Multus parent pidfd required: %w", err)
	}
	defer unix.Close(parentFD)
	multus, runtimePID, err := readCNIProcess(parentPID, a.config.MultusExecutableSHA256)
	if err != nil {
		return none, err
	}
	path := strings.TrimPrefix(a.runtimeSocket, "unix://")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return none, errors.New("clean absolute CNI runtime socket required")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	runtimeConn, err := dialTrustedCRIUnix(probeCtx, path)
	if err != nil {
		return none, err
	}
	defer runtimeConn.Close()
	socket, ok := runtimeConn.(*net.UnixConn)
	if !ok {
		return none, errors.New("actual CRI Unix transport unavailable")
	}
	cred, runtimeFD, err := unixCNIPeerPIDFD(socket)
	if err != nil {
		return none, err
	}
	defer unix.Close(runtimeFD)
	if cred.Pid != runtimePID {
		return none, errors.New("Multus direct parent is not the actual CRI peer")
	}
	runtime, runtimeParent, err := readCNIProcess(runtimePID, a.config.RuntimeExecutableSHA256)
	if err != nil {
		return none, err
	}
	handles = append(handles, processHandle{multus, runtimePID, parentFD}, processHandle{runtime, runtimeParent, runtimeFD})
	seen := map[int32]bool{}
	for _, item := range handles {
		if seen[item.process.PID] {
			return none, errors.New("CNI exact ancestry contains repeated processes")
		}
		seen[item.process.PID] = true
	}
	ns, nsStat, err := openCNINetNS(req.GetRuntime().GetNetnsPath())
	if err != nil {
		return none, err
	}
	defer ns.Close()
	callerNS, callerNSPath, err := heldCNINetNS(caller.PID, nsStat)
	if err != nil {
		return none, err
	}
	defer callerNS.Close()
	// Re-read identities and relationships while all incarnation handles and
	// both namespace references remain held. Reparenting/exec/exit fails closed.
	for _, item := range handles {
		current, parent, err := readCNIProcess(item.process.PID, item.process.ExecutableSHA256)
		if err != nil || current != item.process || parent != item.parent {
			return none, errors.New("CNI process or exact ancestry changed during admission")
		}
		if err := checkCNIPIDFD(item.fd); err != nil {
			return none, err
		}
	}
	if script != nil {
		currentCommand, err := readCNIWrapperCommand(handles[1].process.PID, a.config.WrapperScriptPath)
		if err != nil || currentCommand != wrapperCommand {
			return none, errors.New("CNI wrapper command changed during admission")
		}
		currentRoot, currentRootIdentity, err := openCNIHostRoot(handles[1].process.PID)
		if err != nil {
			return none, err
		}
		defer currentRoot.Close()
		if currentRootIdentity != wrapperRootIdentity {
			return none, errors.New("CNI wrapper process root mount changed during admission")
		}
		currentScript, currentStat, err := openCNIWrapperScript(a.config, currentRoot)
		if err != nil {
			return none, err
		}
		currentScript.Close()
		hash, err := fileSHA256(script)
		if err != nil || hash != a.config.WrapperScriptSHA256 || currentStat.Dev != scriptStat.Dev || currentStat.Ino != scriptStat.Ino {
			return none, errors.New("CNI wrapper script changed during admission")
		}
	}
	afterEnv, err := readCNIEnvironment(caller.PID)
	if err != nil || matchCNIEnvironment(afterEnv, req) != nil {
		return none, errors.New("selected caller environment changed during admission")
	}
	for key, value := range env {
		if afterEnv[key] != value {
			return none, errors.New("selected caller environment changed during admission")
		}
	}
	currentNS, currentStat, err := openCNINetNS(req.GetRuntime().GetNetnsPath())
	if err != nil {
		return none, err
	}
	currentNS.Close()
	var stillHeld unix.Stat_t
	if unix.Stat(callerNSPath, &stillHeld) != nil || stillHeld.Dev != nsStat.Dev || stillHeld.Ino != nsStat.Ino ||
		currentStat.Dev != nsStat.Dev || currentStat.Ino != nsStat.Ino {
		return none, errors.New("original CNI path or caller namespace descriptor replaced")
	}
	processes := make([]CNIProcessEvidence, 0, len(handles))
	for _, item := range handles {
		if err := errors.Join(ctx.Err(), checkCNIPIDFD(item.fd)); err != nil {
			return none, err
		}
		processes = append(processes, cniPublicProcess(item.process))
	}
	evidence := CNIAdmissionEvidence{
		Caller:      cniPublicProcess(caller),
		Processes:   processes,
		NetNSDevice: uint64(nsStat.Dev), NetNSInode: nsStat.Ino,
		ContainerID: req.GetRuntime().GetContainerId(), PodUID: req.GetK8S().GetPodUid(),
	}
	if err := evidence.validate(); err != nil {
		return none, err
	}
	return evidence, nil
}
