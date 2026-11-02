package evpn

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func d22dSVIPath(t *testing.T, address string) *api.Path {
	t.Helper()
	ip := netip.MustParseAddr(address)
	return importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.MacAddress = net.HardwareAddr{0x72, 0x81, 0x60, 0x13, 0xa0, 0x6d}
		route.IPAddress = ip
		if ip.Is4() {
			route.IPAddressLength = 32
		} else {
			route.IPAddressLength = 128
		}
	})
}

func TestD22dUnsupportedSVIIPv6OverlapFreezesImport(t *testing.T) {
	i, backend, kernel, source := remoteOwnedFixture(t)
	workload := importPath(t, nil)
	sviIPv4 := d22dSVIPath(t, "10.244.240.1")
	sviIPv6 := d22dSVIPath(t, "fe80::9c52:7eff:fe00:188b")
	source.snap = RemoteRIBSnapshot{
		Paths:    []*api.Path{workload, sviIPv4, sviIPv6},
		Complete: true,
	}

	if err := i.reconcileValidated(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "quarantined route conflicts with validated endpoint") {
		t.Fatalf("expected overlapping unsupported SVI route to freeze import, got %v", err)
	}
	snapshot := i.LastSnapshot()
	if snapshot.Authoritative || len(snapshot.Endpoints) != 0 || len(snapshot.Issues) != 1 ||
		snapshot.Issues[0].Reason != "unsupported unicast MAC/optional IPv4" {
		t.Fatalf("unexpected frozen importer snapshot: %+v", snapshot)
	}
	if kernel.mutations != 0 || len(backend.Receipts()) != 0 {
		t.Fatalf("non-authoritative observation changed owned forwarding state: mutations=%d receipts=%+v",
			kernel.mutations, backend.Receipts())
	}
}

func TestD22dNonOverlappingUnsupportedRoutePreservesWorkloadImport(t *testing.T) {
	macIP := importPath(t, nil)
	macOnly := importPath(t, func(route *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		route.IPAddressLength = 0
		route.IPAddress = netip.Addr{}
	})
	sviIPv6 := d22dSVIPath(t, "fe80::9c52:7eff:fe00:188b")
	source := &importTestSource{snap: RemoteRIBSnapshot{
		Paths:    []*api.Path{macIP, macOnly, sviIPv6},
		Complete: true,
	}}
	importer, err := NewReadOnlyType2Importer(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := confirmedImport(t, importer)
	if len(snapshot.Endpoints) != 1 || snapshot.Endpoints[0].IP != "10.251.1.11" ||
		len(snapshot.Endpoints[0].Routes) != 2 || len(snapshot.Issues) != 1 ||
		snapshot.Issues[0].Reason != "unsupported unicast MAC/optional IPv4" {
		t.Fatalf("non-overlapping quarantine changed compatible workload import: %+v", snapshot)
	}
}
