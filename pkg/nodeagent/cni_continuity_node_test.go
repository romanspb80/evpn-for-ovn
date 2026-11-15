package nodeagent

import (
	"context"
	"strings"
	"testing"
)

func TestCNIContinuityRejectsOtherNodeBeforeRecovery(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-a")
	server := &CNIServer{}
	_, err := server.EnableCNIContinuity(context.Background(), CNIContinuityConfig{
		NodeName: "worker-b", JournalDirectory: "/var/lib/evpn-node-agent/cni",
		Networks: map[string]string{"network": "switch"},
	}, RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"})
	if err == nil || !strings.Contains(err.Error(), "differs from NODE_NAME") {
		t.Fatalf("wrong-node configuration not rejected: %v", err)
	}
}
