package nodeagent

import (
	"fmt"
	"net"
)

// AcquireAgentAuthority fences cooperating agents in the dataplane network
// namespace. It must precede backend initialization and CNI socket preparation.
// The fixed address is intentionally independent of configuration and socket paths.
// Keep it open until all RPC handlers, runtimes and backend cleanup have stopped.
func AcquireAgentAuthority() (*net.UnixListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: "@evpn4ovn-node-agent-authority", Net: "unix",
	})
	if err != nil {
		return nil, fmt.Errorf("RECOVERY_BLOCKED: node-agent authority held or unavailable; do not remove CNI sockets or ownership files: %w", err)
	}
	return listener, nil
}
