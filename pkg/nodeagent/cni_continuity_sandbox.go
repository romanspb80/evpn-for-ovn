package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
)

// containerd may publish the CRI sandbox only after CNI ADD has returned.
// This optional creation-only inspector reads the already existing sandbox
// container and running pause task; it never creates a container or sandbox.
type cniCreatingSandboxAttestor struct {
	source *kubernetesContainerdSource
}

func (a *cniCreatingSandboxAttestor) VerifySandbox(ctx context.Context, ref endpoint.WorkloadRef, id, netns string) error {
	if !validSHA256(id) {
		return errors.New("invalid sandbox container ID")
	}
	raw, err := a.source.ctr(ctx, "containers", "info", id)
	if err != nil {
		return err
	}
	tasks, err := a.source.ctr(ctx, "tasks", "list")
	if err != nil {
		return err
	}
	return verifyCreatingSandboxEvidence(raw, tasks, ref, id, netns)
}

func verifyCreatingSandboxEvidence(raw, tasks []byte, ref endpoint.WorkloadRef, id, netns string) error {
	var info struct {
		ID     string            `json:"ID"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if info.ID != id || info.Labels["io.cri-containerd.kind"] != "sandbox" ||
		info.Labels["io.kubernetes.pod.uid"] != ref.UID ||
		info.Labels["io.kubernetes.pod.namespace"] != ref.Namespace ||
		info.Labels["io.kubernetes.pod.name"] != ref.Name {
		return errors.New("creating sandbox container identity differs from independently read Pod")
	}
	if running, err := containerdTaskRunning(tasks, id); err != nil || !running {
		return fmt.Errorf("creating sandbox task not independently running: %v", err)
	}
	var taskPID int
	for _, line := range strings.Split(strings.TrimSpace(string(tasks)), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == id {
			var err error
			taskPID, err = strconv.Atoi(fields[1])
			if err != nil || taskPID <= 0 {
				return errors.New("invalid creating sandbox task PID")
			}
		}
	}
	taskNS, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", taskPID))
	if err != nil {
		return err
	}
	attachmentNS, err := os.Stat(netns)
	if err != nil {
		return err
	}
	if !os.SameFile(taskNS, attachmentNS) {
		return errors.New("creating sandbox pause task namespace differs from CNI netns")
	}
	return nil
}
