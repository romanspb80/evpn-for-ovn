package evpn

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func mixedImportPaths(t *testing.T) (*api.Path, *api.Path) {
	t.Helper()
	return importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
		n.IPAddressLength = 0
		n.IPAddress = netip.Addr{}
	}), importPath(t, nil)
}

func TestReadOnlyType2MixedDuplicatesAndOrdering(t *testing.T) {
	macOnly, macIP := mixedImportPaths(t)
	second := importPath(t, nil)
	second.Identifier = 2
	source := &importTestSource{}
	reader, err := NewReadOnlyType2Importer(source, importConfig())
	if err != nil {
		t.Fatal(err)
	}
	source.snap = RemoteRIBSnapshot{Paths: []*api.Path{macOnly, macIP, second}, Complete: true}
	first := confirmedImport(t, reader)
	if len(first.Endpoints) != 1 || len(first.Endpoints[0].Routes) != 3 {
		t.Fatalf("mixed paths lost independent references: %+v", first)
	}
	for _, paths := range [][]*api.Path{
		{second, macIP, macOnly},
		{macIP, macOnly, second, macIP, macOnly, second},
		{macOnly, second, macIP},
	} {
		source.snap.Paths = paths
		if got := confirmedImport(t, reader); !reflect.DeepEqual(got, first) {
			t.Fatalf("order/duplicates changed confirmed identity or generation: %+v", got)
		}
	}
}

func TestOwnedNetlinkMixedWithdrawal(t *testing.T) {
	for _, remaining := range []string{"mac-only", "mac-ip"} {
		t.Run(remaining, func(t *testing.T) {
			i, b, k, source := remoteOwnedFixture(t)
			macOnly, macIP := mixedImportPaths(t)
			foreign := RemoteKernelEntry{Family: unix.AF_INET, Ifindex: 2, MAC: "02:00:00:00:ff:ff",
				IP: "10.251.1.99", State: netlink.NUD_NOARP}
			k.entries = append(k.entries, *foreign.neigh())
			mixed := ownedSnapshot(t, i, macOnly, macIP)
			applyOwned(t, i, mixed)
			old := i.active[mixed.Endpoints[0].Key]
			path, wantEntries, wantIP := macIP, 3, mixed.Endpoints[0].IP
			if remaining == "mac-only" {
				path, wantEntries, wantIP = macOnly, 2, ""
			}
			source.snap.Paths = []*api.Path{path}
			mutations := k.mutations
			if err := i.ReconcileOnce(context.Background()); err == nil || k.mutations != mutations {
				t.Fatal("unconfirmed disappearance mutated mixed state")
			}
			if err := i.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(k.entries) != wantEntries || len(b.Receipts()) != 1 ||
				b.Receipts()[0].Endpoint.IP != wantIP || len(b.Receipts()[0].Endpoint.Routes) != 1 {
				t.Fatalf("remaining route not represented: entries=%+v receipts=%+v", k.entries, b.Receipts())
			}
			if len(k.deletes) != 2 || k.deletes[0].Family != unix.AF_INET || k.deletes[1].Family != unix.AF_BRIDGE {
				t.Fatalf("mixed receipt not retired neighbor-first: %+v", k.deletes)
			}
			mutations = k.mutations
			if err := b.Withdraw(context.Background(), old); err == nil || k.mutations != mutations {
				t.Fatal("stale mixed receipt removed replacement")
			}
			if err := i.ReconcileOnce(context.Background()); err != nil || k.mutations != mutations {
				t.Fatal("remaining snapshot was not idempotent", err)
			}
			applyOwned(t, i, ownedSnapshot(t, i))
			if len(b.Receipts()) != 0 || len(i.active) != 0 || len(k.entries) != 1 || kernelEntry(k.entries[0]) != foreign {
				t.Fatal("final withdrawal leaked owned state or changed foreign neighbor")
			}
			mutations = k.mutations
			applyOwned(t, i, ownedSnapshot(t, i))
			if k.mutations != mutations {
				t.Fatal("repeated empty snapshot mutated")
			}
		})
	}
}

func TestReadOnlyType2MixedConflictsFreezeOwnedState(t *testing.T) {
	for _, conflict := range []string{"vtep", "second-ip", "same-ip-other-mac", "ipv6", "link-local"} {
		t.Run(conflict, func(t *testing.T) {
			i, b, k, source := remoteOwnedFixture(t)
			macOnly, macIP := mixedImportPaths(t)
			applyOwned(t, i, ownedSnapshot(t, i, macOnly, macIP))
			before, mutations := b.Receipts(), k.mutations
			other := importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, attrs *[]bgp.PathAttributeInterface) {
				switch conflict {
				case "vtep":
					(*attrs)[1], _ = bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.81"))
				case "second-ip":
					n.IPAddress = netip.MustParseAddr("10.251.1.12")
				case "same-ip-other-mac":
					n.MacAddress = net.HardwareAddr{2, 0, 0, 0, 0, 99}
				case "ipv6", "link-local":
					n.IPAddressLength = 128
					n.IPAddress = netip.MustParseAddr("2001:db8::80")
					if conflict == "link-local" {
						n.IPAddress = netip.MustParseAddr("fe80::80")
					}
				}
			})
			source.snap.Paths = []*api.Path{macOnly, macIP, other}
			for attempt := 0; attempt < 2; attempt++ {
				if err := i.ReconcileOnce(context.Background()); err == nil || i.LastSnapshot().Authoritative ||
					k.mutations != mutations || !reflect.DeepEqual(b.Receipts(), before) {
					t.Fatal("conflicting/unsupported mixed route changed owned state")
				}
			}
		})
	}
}

func TestRemoteType2AddressFamilyIdentity(t *testing.T) {
	macOnly, ipv4 := mixedImportPaths(t)
	var keys []string
	for _, path := range []*api.Path{macOnly, ipv4,
		importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.IPAddressLength, n.IPAddress = 128, netip.MustParseAddr("2001:db8::80")
		}),
		importPath(t, func(n *bgp.EVPNMacIPAdvertisementRoute, _ *[]bgp.PathAttributeInterface) {
			n.IPAddressLength, n.IPAddress = 128, netip.MustParseAddr("fe80::80")
		}),
	} {
		route, err := DecodeRemoteType2(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, prior := range keys {
			if route.Source.Key == prior {
				t.Fatal("MAC-only/IPv4/IPv6 route identities collided")
			}
		}
		keys = append(keys, route.Source.Key)
		_, err = importConfig().ValidateRoute(route)
		if (len(keys) <= 2 && err != nil) || (len(keys) > 2 && err == nil) {
			t.Fatalf("optional IPv4 policy changed for %q: %v", route.IP, err)
		}
	}
}

func TestOwnedNetlinkMixedRequiresIPSourceEvidence(t *testing.T) {
	i, b, k, _ := remoteOwnedFixture(t)
	macOnly, _ := mixedImportPaths(t)
	snapshot := ownedSnapshot(t, i, macOnly)
	ep := &snapshot.Endpoints[0]
	ep.IP = "10.251.1.11"
	ep.Key = remoteHash([]string{ep.NetworkID, ep.MAC, ep.IP})
	if err := i.applyValidatedSnapshot(context.Background(), snapshot); err == nil || k.mutations != 0 || len(b.Receipts()) != 0 {
		t.Fatal("MAC-only source authorized an unadvertised neighbor")
	}
}
