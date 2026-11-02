package evpn

import (
	"os"
	"path/filepath"
	"testing"
)

func installTopologyCommand(t *testing.T, directory, name, body string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFRRNeighborIPPrefersKindNetwork(t *testing.T) {
	directory := t.TempDir()
	installTopologyCommand(t, directory, "docker",
		`printf '%s\n' 'bridge 172.19.0.8' 'kind 198.18.0.3' 'default 172.17.0.2'`)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FRR_CONTAINER", "frr-fixture")
	t.Setenv("FRR_NEIGHBOR_IP", "")
	t.Setenv("GOBGP_NEIGHBOR_IP", "")

	if got := resolveFRRNeighborIP(t); got != "198.18.0.3" {
		t.Fatalf("resolveFRRNeighborIP() = %q, want preferred kind network address", got)
	}
}

func TestResolveGoBGPLocalIPUsesRouteSource(t *testing.T) {
	directory := t.TempDir()
	installTopologyCommand(t, directory, "ip", `
if [ "$1" != route ] || [ "$2" != get ] || [ "$3" != 198.51.100.10 ]; then
  exit 2
fi
printf '%s\n' '198.51.100.10 via 198.51.100.1 dev eth0 src 198.51.100.20 uid 1000'`)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOBGP_LOCAL_IP", "")

	if got := resolveGoBGPLocalIP(t, "198.51.100.10"); got != "198.51.100.20" {
		t.Fatalf("resolveGoBGPLocalIP() = %q, want route-selected source address", got)
	}
}
