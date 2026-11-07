package nodeagent

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The supported runtime is containerd 1.7/2.x with its matching static ctr
// binary. Only these three read-only commands are executed; no general-purpose
// command or runtime endpoint is exposed through the node-agent service.
func (s *kubernetesContainerdSource) ctr(ctx context.Context, args ...string) ([]byte, error) {
	f, err := openAdministratorFile(s.config.CTRPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	binary, err := elf.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("runtime inspector must be a static Linux ELF executable: %w", err)
	}
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			return nil, fmt.Errorf("runtime inspector must be statically linked")
		}
	}
	hash, err := fileSHA256(f)
	if err != nil {
		return nil, err
	}
	if hash != s.config.CTRSHA256 {
		return nil, fmt.Errorf("runtime inspector executable SHA256 changed")
	}
	// Socket contents cannot be made read-only by a Kubernetes volume mount.
	// Verify its administrator-controlled path, but require a privileged trust
	// boundary: possession of this socket grants containerd administrator access.
	parent, err := openAdministratorDirectory(filepath.Dir(s.config.RuntimeSocket))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(s.config.RuntimeSocket), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != 0 || st.Mode&0022 != 0 {
		return nil, fmt.Errorf("containerd socket must be root-owned and not writable by group/other")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := append([]string{"--address", s.config.RuntimeSocket, "--namespace", "k8s.io"}, args...)
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", argv...)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return nil, containerdInspectionError(args, err)
	}
	return out, nil
}

func containerdInspectionError(args []string, err error) error {
	detail := ""
	if exit, ok := err.(*exec.ExitError); ok {
		stderr := exit.Stderr
		if len(stderr) > 1024 {
			stderr = stderr[:1024]
		}
		detail = strings.TrimSpace(string(stderr))
	}
	return fmt.Errorf("read-only containerd inspection %q: %w; stderr=%q", args, err, detail)
}

func openAdministratorDirectory(path string) (*os.File, error) {
	// The /proc/self/fd executable handle above is deliberately NOT accepted by
	// the administrator path loader.
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(fd) }()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid administrator directory")
	}
	if path != "/" {
		for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			next, err := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			var st unix.Stat_t
			if err := unix.Fstat(next, &st); err != nil {
				unix.Close(next)
				return nil, err
			}
			if st.Uid != 0 || st.Mode&0022 != 0 {
				unix.Close(next)
				return nil, fmt.Errorf("runtime path requires root-owned non-writable directories")
			}
			unix.Close(fd)
			fd = next
		}
	}
	f := os.NewFile(uintptr(fd), path)
	fd = -1
	return f, nil
}

func (s *kubernetesContainerdSource) Container(ctx context.Context, id string) (providerRuntimeEvidence, error) {
	var result providerRuntimeEvidence
	if !validSHA256(id) {
		return result, fmt.Errorf("invalid container ID")
	}
	info, err := s.ctr(ctx, "containers", "info", id)
	if err != nil {
		return result, err
	}
	var container struct {
		ID     string            `json:"ID"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(info, &container); err != nil {
		return result, err
	}
	result = providerRuntimeEvidence{
		ID: container.ID, Image: container.Image,
		PodUID:        container.Labels["io.kubernetes.pod.uid"],
		Namespace:     container.Labels["io.kubernetes.pod.namespace"],
		PodName:       container.Labels["io.kubernetes.pod.name"],
		ContainerName: container.Labels["io.kubernetes.container.name"],
	}
	tasks, err := s.ctr(ctx, "tasks", "list")
	if err != nil {
		return result, err
	}
	result.Running, err = containerdTaskRunning(tasks, id)
	if err != nil || !result.Running {
		return result, fmt.Errorf("containerd task is not running: %v", err)
	}
	rootPID, err := containerdTaskRootPID(tasks, id)
	if err != nil {
		return result, err
	}
	result.PIDNamespace, err = os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", rootPID))
	if err != nil {
		return result, fmt.Errorf("runtime task PID namespace: %w", err)
	}
	ps, err := s.ctr(ctx, "tasks", "ps", id)
	if err != nil {
		return result, err
	}
	result.PIDs, err = containerdTaskPIDs(ps)
	return result, err
}

func containerdTaskRunning(output []byte, id string) (bool, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || strings.Join(strings.Fields(lines[0]), " ") != "TASK PID STATUS" {
		return false, fmt.Errorf("unsupported ctr tasks list format")
	}
	found := false
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) != 3 {
			return false, fmt.Errorf("invalid ctr task row")
		}
		if f[0] != id {
			continue
		}
		if found {
			return false, fmt.Errorf("duplicate runtime task")
		}
		pid, err := strconv.ParseInt(f[1], 10, 32)
		if err != nil || pid <= 0 {
			return false, fmt.Errorf("invalid runtime task PID")
		}
		if f[2] != "RUNNING" {
			return false, nil
		}
		found = true
	}
	return found, nil
}

func containerdTaskRootPID(output []byte, id string) (int32, error) {
	running, err := containerdTaskRunning(output, id)
	if err != nil || !running {
		return 0, fmt.Errorf("runtime task root PID unavailable: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n")[1:] {
		fields := strings.Fields(line)
		if fields[0] == id {
			pid, err := strconv.ParseInt(fields[1], 10, 32)
			return int32(pid), err
		}
	}
	return 0, fmt.Errorf("runtime task not found")
}

func containerdTaskPIDs(output []byte) ([]int32, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || strings.Join(strings.Fields(lines[0]), " ") != "PID INFO" {
		return nil, fmt.Errorf("unsupported ctr tasks ps format")
	}
	var pids []int32
	seen := map[int32]bool{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) == 0 {
			return nil, fmt.Errorf("invalid runtime process row")
		}
		pid, err := strconv.ParseInt(f[0], 10, 32)
		if err != nil || pid <= 0 || seen[int32(pid)] {
			return nil, fmt.Errorf("invalid or duplicate runtime process PID")
		}
		seen[int32(pid)] = true
		pids = append(pids, int32(pid))
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("runtime task has no processes")
	}
	return pids, nil
}
