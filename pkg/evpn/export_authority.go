package evpn

import (
	"fmt"
	"net"
)

// AcquireExporterAuthority fences cooperating exporters in the speaker's network
// namespace. An abstract socket is released by the kernel on process exit.
func AcquireExporterAuthority() (*net.UnixListener, error) {
	lock, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@evpn4ovn-local-export-authority", Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("local exporter already active or authority unavailable: %w", err)
	}
	return lock, nil
}
