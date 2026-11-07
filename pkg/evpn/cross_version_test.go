package evpn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// External observations are disposable models, not live GoBGP or kernel evidence.
type crossVersionRIB struct {
	owned []OwnedType2Route
	adds  int
	dels  int
}

func (a *crossVersionRIB) Snapshot(context.Context) ([]Type2RIBPath, error) {
	var paths []Type2RIBPath
	for _, owned := range a.owned {
		route, err := BuildType2Route(owned.Endpoint)
		if err != nil {
			return nil, err
		}
		paths = append(paths, Type2RIBPath{PathUUID: owned.PathUUID, Local: true,
			NLRI: route.NLRI, PathAttributes: route.PathAttributes})
	}
	return paths, nil
}

func (a *crossVersionRIB) Add(_ context.Context, route Type2Route) (string, error) {
	a.adds++
	id := "b313b954-c7de-43ad-ae2f-cc3a97962ecf"
	a.owned = append(a.owned, OwnedType2Route{Endpoint: route.Endpoint, PathUUID: id})
	return id, nil
}

func (a *crossVersionRIB) Delete(_ context.Context, owned OwnedType2Route) error {
	for i, p := range a.owned {
		if p.PathUUID == owned.PathUUID {
			a.dels++
			a.owned = append(a.owned[:i], a.owned[i+1:]...)
			return nil
		}
	}
	return errors.New("UUID not present")
}

func crossVersionWrite(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func crossVersionRead(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func TestCrossVersionEVPN(t *testing.T) {
	mode, dir, scenario := os.Getenv("EVPN_CROSS_MODE"), os.Getenv("EVPN_CROSS_DIR"), os.Getenv("EVPN_CROSS_CASE")
	if mode == "" {
		t.Skip("explicit cross-version coordinator required")
	}
	if os.Geteuid() != 0 || !filepath.IsAbs(dir) {
		t.Fatal("root and an absolute disposable fixture path required")
	}
	if mode != "write" && mode != "read" {
		t.Fatal("unknown helper mode")
	}
	fmt.Printf("COMPILER %s MODE %s CASE %s\n", runtime.Version(), mode, scenario)
	if len(scenario) >= 7 && scenario[:7] == "export-" {
		crossVersionExport(t, mode, dir, scenario[7:])
	} else if len(scenario) >= 7 && scenario[:7] == "import-" {
		crossVersionImport(t, mode, dir, scenario[7:])
	} else {
		t.Fatal("unknown scenario")
	}
}

func crossVersionExport(t *testing.T, mode, dir, scenario string) {
	ctx := context.Background()
	cfg := ExportRecoveryConfig{StateDirectory: filepath.Join(dir, "export"), Domain: recoveryDomain()}
	api := &crossVersionRIB{}
	peer := &testVerifiedSpeaker{id: "independently-modeled-speaker"}
	external := filepath.Join(dir, "external-rib.json")
	if mode == "read" {
		crossVersionRead(t, external, &api.owned)
		if scenario == "incarnation" {
			peer.id = "replacement-speaker"
		}
		if scenario == "domain" {
			cfg.Domain.NodeUID = cfg.Domain.InstallationUID
		}
	}
	r, err := OpenExportRecovery(cfg, api, api, peer)
	if mode == "read" && scenario == "domain" {
		if r != nil {
			r.Close()
		}
		if !errors.Is(err, ownership.ErrBlocked) {
			t.Fatal("foreign export domain accepted", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if mode == "write" {
		if _, err := r.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		route := mustProviderRoute(t, sampleEndpoint())
		if scenario == "ambiguous" || scenario == "crash-ack" {
			r.fault = func(boundary string) error {
				if scenario == "ambiguous" && boundary == "add-executed" ||
					scenario == "crash-ack" && boundary == "add-ack" {
					crossVersionWrite(t, external, api.owned)
					os.Exit(42)
				}
				return nil
			}
		}
		if scenario == "foreign" {
			api.owned = []OwnedType2Route{{Endpoint: route.Endpoint, PathUUID: "fad1d0e0-456a-4a59-8b18-6d5b3f077023"}}
		} else if _, err := r.Add(ctx, route); err != nil {
			t.Fatal(err)
		}
		crossVersionWrite(t, external, api.owned)
		return
	}
	before := append([]OwnedType2Route(nil), api.owned...)
	receipts, err := r.Recover(ctx)
	if scenario == "ambiguous" || scenario == "incarnation" {
		if !errors.Is(err, ownership.ErrBlocked) {
			t.Fatalf("ambiguous/incarnation recovery accepted: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	} else if scenario == "foreign" {
		if len(receipts) != 0 {
			t.Fatal("foreign UUID adopted")
		}
		if _, err := r.Add(ctx, mustProviderRoute(t, sampleEndpoint())); !errors.Is(err, ownership.ErrBlocked) {
			t.Fatal("matching foreign route replaced")
		}
	} else {
		if !reflect.DeepEqual(receipts, before) {
			t.Fatalf("durable export receipts changed: %+v %+v", receipts, before)
		}
		if _, err := r.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if api.adds != 0 || api.dels != 0 || !reflect.DeepEqual(api.owned, before) {
		t.Fatal("recovery mutated/adopted modeled RIB")
	}
	if scenario == "ack" || scenario == "crash-ack" {
		foreign := sampleEndpoint()
		foreign.MACAddress = "02:00:00:00:00:99"
		foreignUUID := "fad1d0e0-456a-4a59-8b18-6d5b3f077023"
		api.owned = append(api.owned, OwnedType2Route{Endpoint: foreign, PathUUID: foreignUUID})
		if err := r.Delete(ctx, receipts[0]); err != nil || api.dels != 1 || len(api.owned) != 1 || api.owned[0].PathUUID != foreignUUID {
			t.Fatalf("UUID-scoped recovered withdrawal failed: %v", err)
		}
	}
}

func crossVersionImport(t *testing.T, mode, dir, scenario string) {
	i, b, k, _ := remoteOwnedFixture(t)
	d := recoveryDomain()
	b.owned.controller, b.owned.target = d.NodeUID, d.BootID+"/"+d.NetworkNamespace
	b.owned.config.StateDirectory = filepath.Join(dir, "import")
	b.owned.config.Recovery = &RemoteNetlinkRecovery{Domain: d}
	external := filepath.Join(dir, "external-kernel.json")
	expected := filepath.Join(dir, "expected-receipts.json")
	if mode == "write" {
		if err := os.Mkdir(b.owned.config.StateDirectory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := b.owned.openRecovery(d.BootID, d.NetworkNamespace); err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		if err := b.owned.checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := acquireRemoteMarker(b.owned.config.StateDirectory, b.owned.controller, b.owned.target); err != nil {
			t.Fatal(err)
		}
		b.owned.markerPath = filepath.Join(b.owned.config.StateDirectory, "remote-netlink-owner")
		if scenario == "crash-mutation" {
			b.owned.fault = func(boundary string) {
				if boundary == "after-add" {
					crossVersionWrite(t, external, k.entries)
					os.Exit(42)
				}
			}
		}
		applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
		switch scenario {
		case "ambiguous":
			r := b.owned.receipts[b.Receipts()[0].Endpoint.Key]
			r.FDB.Status, r.FDB.ACK, r.FDB.Observed = "unattempted", false, RemoteKernelEntry{}
			if err := b.owned.checkpoint(); err != nil {
				t.Fatal(err)
			}
		case "quarantine":
			route := i.active[b.Receipts()[0].Endpoint.Key]
			b.owned.updates <- netlink.LinkUpdate{Link: k.links["test-vx"], Header: unix.NlMsghdr{Type: unix.RTM_DELLINK}}
			if err := b.Apply(context.Background(), route); err == nil {
				t.Fatal("device loss did not persist quarantine")
			}
		}
		crossVersionWrite(t, external, k.entries)
		crossVersionWrite(t, expected, b.Receipts())
		if scenario == "crash-ack" {
			os.Exit(42)
		}
		return
	}
	crossVersionRead(t, external, &k.entries)
	if scenario == "foreign" {
		k.entries[0].IP = []byte{192, 0, 2, 199}
	}
	if scenario == "domain" {
		b.owned.config.Recovery.Domain.NodeUID = d.InstallationUID
	}
	before := append([]netlink.Neigh(nil), k.entries...)
	i, b, err := reopenMemory(t, b, k)
	negative := scenario == "ambiguous" || scenario == "quarantine" || scenario == "foreign" || scenario == "domain" || scenario == "crash-mutation"
	if negative {
		if !errors.Is(err, ownership.ErrBlocked) {
			t.Fatalf("unsafe importer recovery did not fail closed: %v", err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		var receipts []RemoteNetlinkReceipt
		crossVersionRead(t, expected, &receipts)
		if !reflect.DeepEqual(receipts, b.Receipts()) {
			t.Fatal("import receipts changed across compiler versions")
		}
		applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	}
	if k.mutations != 0 || !reflect.DeepEqual(k.entries, before) {
		t.Fatal("recovery mutated modeled foreign/kernel objects")
	}
	if !negative {
		applyOwned(t, i, ownedSnapshot(t, i))
		if len(k.entries) != 0 || len(b.Receipts()) != 0 || k.mutations != 2 {
			t.Fatal("acknowledged importer withdrawal failed")
		}
	}
}
