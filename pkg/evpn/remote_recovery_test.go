package evpn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func recoveryRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned production journals")
	}
}

func recoveryDomain() ownership.Domain {
	return ownership.Domain{InstallationUID: "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID: "46b08606-9b26-4098-90d7-02c14098122b", BootID: "8a63e552-b620-445f-9ab6-8d924321ccfe",
		NetworkNamespace: "test-netns", ConfigDigest: strings.Repeat("a", 64)}
}

func persistentFixture(t *testing.T) (*RemoteType2Importer, *OVNNetlinkBackend, *memoryOwnedKernel) {
	t.Helper()
	recoveryRoot(t)
	i, b, k, _ := remoteOwnedFixture(t)
	s := b.owned
	d := recoveryDomain()
	s.controller = d.NodeUID
	s.target = d.BootID + "/" + d.NetworkNamespace
	s.config.StateDirectory = t.TempDir()
	s.config.Recovery = &RemoteNetlinkRecovery{Domain: d}
	if err := s.openRecovery(d.BootID, d.NetworkNamespace); err != nil {
		t.Fatal(err)
	}
	if err := s.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := acquireRemoteMarker(s.config.StateDirectory, s.controller, s.target); err != nil {
		t.Fatal(err)
	}
	s.markerPath = filepath.Join(s.config.StateDirectory, "remote-netlink-owner")
	t.Cleanup(func() { b.Close() })
	return i, b, k
}

func reopenMemory(t *testing.T, old *OVNNetlinkBackend, k *memoryOwnedKernel) (*RemoteType2Importer, *OVNNetlinkBackend, error) {
	t.Helper()
	cfg := old.owned.config
	old.Close()
	s := &ownedNetlinkState{kernel: k, config: cfg, controller: old.owned.controller,
		target: old.owned.target, devices: map[string]RemoteDeviceIdentity{}, receipts: map[string]*RemoteNetlinkReceipt{},
		latest: map[string]uint64{}, updates: make(chan netlink.LinkUpdate), done: make(chan struct{})}
	b := &OVNNetlinkBackend{owned: s}
	fail := func(err error) (*RemoteType2Importer, *OVNNetlinkBackend, error) { b.Close(); return nil, b, err }
	d := cfg.Recovery.Domain
	if err := s.openRecovery(d.BootID, d.NetworkNamespace); err != nil {
		return fail(err)
	}
	for name, expected := range s.devices {
		id, err := s.device(name)
		if err != nil || id != expected {
			return fail(fmt.Errorf("device identity changed: %s", name))
		}
	}
	if err := s.verifyRecovered(context.Background()); err != nil {
		return fail(err)
	}
	reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
	if err != nil {
		return fail(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		return fail(err)
	}
	t.Cleanup(func() { b.Close() })
	return i, b, nil
}

func TestImporterRecoveryRepeatedRestart(t *testing.T) {
	i, b, k := persistentFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	original := b.Receipts()[0]
	mutations := k.mutations
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		i, b, err = reopenMemory(t, b, k)
		if err != nil {
			t.Fatal(err)
		}
		source := i.verified.source.(*importTestSource)
		source.snap = RemoteRIBSnapshot{Complete: true, Paths: []*api.Path{p}}
		if err := i.ReconcileOnce(context.Background()); err == nil || k.mutations != mutations {
			t.Fatal("recovered snapshot mutated before two fresh observations")
		}
		if err := i.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if k.mutations != mutations || !reflect.DeepEqual(original, b.Receipts()[0]) {
			t.Fatal("restart duplicated or changed owned receipts")
		}
	}
	applyOwned(t, i, ownedSnapshot(t, i))
	if len(k.entries) != 0 || len(b.Receipts()) != 0 {
		t.Fatal("obsolete recovered entries not withdrawn")
	}
}

func TestImporterRecoveryAbsenceForeignAndCorruption(t *testing.T) {
	for _, mode := range []string{"missing-owned", "foreign-matching-unattempted", "foreign-neighbor-unattempted", "foreign-changed", "dump-failure", "corrupt", "marker-only", "marker-changed", "domain", "device"} {
		t.Run(mode, func(t *testing.T) {
			i, b, k := persistentFixture(t)
			p := importPath(t, nil)
			applyOwned(t, i, ownedSnapshot(t, i, p))
			switch mode {
			case "missing-owned":
				k.entries = nil
			case "foreign-matching-unattempted", "foreign-neighbor-unattempted":
				// Exact tuple equality cannot turn an unattempted receipt into ownership.
				r := b.owned.receipts[b.Receipts()[0].Endpoint.Key]
				o := &r.FDB
				if mode == "foreign-neighbor-unattempted" {
					o = r.Neighbor
				}
				o.Status, o.ACK = "unattempted", false
				o.Observed = RemoteKernelEntry{}
				if err := b.owned.checkpoint(); err != nil {
					t.Fatal(err)
				}
			case "foreign-changed":
				k.entries[0].IP = net.ParseIP("192.0.2.199")
			case "dump-failure":
				k.listError = fmt.Errorf("unavailable kernel dump")
			case "corrupt":
				if err := os.WriteFile(filepath.Join(b.owned.config.StateDirectory, "receipts-v1", "journal.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker-only":
				if err := os.Rename(filepath.Join(b.owned.config.StateDirectory, "receipts-v1"), filepath.Join(b.owned.config.StateDirectory, "archived")); err != nil {
					t.Fatal(err)
				}
			case "marker-changed":
				if err := os.WriteFile(b.owned.markerPath, []byte("foreign\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "domain":
				b.owned.config.Recovery.Domain.NodeUID = b.owned.config.Recovery.Domain.InstallationUID
			case "device":
				b.owned.devices["test-vx"] = RemoteDeviceIdentity{Name: "other"}
				if err := b.owned.checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			mutations := k.mutations
			i, b, err := reopenMemory(t, b, k)
			if mode != "missing-owned" {
				if err == nil || k.mutations != mutations {
					t.Fatalf("unsafe recovery accepted or mutated: %v", err)
				}
				return
			}
			if err != nil || len(k.entries) != 0 {
				t.Fatal("verified absence could not recover without startup mutation", err)
			}
			applyOwned(t, i, ownedSnapshot(t, i, p))
			if len(k.entries) != 2 || k.mutations != mutations+2 {
				t.Fatal("missing owned state did not converge after fresh snapshot")
			}
			if b.Receipts()[0].Incarnation == 0 {
				t.Fatal("receipt incarnation lost")
			}
		})
	}
}

func TestImporterRecoveryRetiringRouteReappears(t *testing.T) {
	i, b, k := persistentFixture(t)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	old := b.Receipts()[0]
	r := b.owned.receipts[old.Endpoint.Key]
	r.Retiring, r.Status = true, "partially-withdrawn"
	if err := b.owned.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := b.owned.withdrawObject(context.Background(), r.Neighbor); err != nil {
		t.Fatal(err)
	}
	i, b, err := reopenMemory(t, b, k)
	if err != nil {
		t.Fatal(err)
	}
	applyOwned(t, i, ownedSnapshot(t, i, p))
	current := b.Receipts()[0]
	if current.Incarnation <= old.Incarnation || current.Retiring || current.Status != "applied" ||
		len(k.entries) != 2 || len(b.Withdrawals()) != 1 {
		t.Fatal("retiring receipt revived instead of withdrawn and replaced", current)
	}
	if err := b.Withdraw(context.Background(), receiptRoute(&old)); err == nil {
		t.Fatal("old incarnation deleted recovered replacement")
	}
}

func TestImporterRecoveryDurableQuarantine(t *testing.T) {
	i, b, k := persistentFixture(t)
	applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	route := i.active[b.Receipts()[0].Endpoint.Key]
	b.owned.updates <- netlink.LinkUpdate{Link: k.links["test-vx"], Header: unix.NlMsghdr{Type: unix.RTM_DELLINK}}
	if err := b.Apply(context.Background(), route); err == nil {
		t.Fatal("known device lifecycle loss not blocked")
	}
	// Device names, indices, tuples and all current observations match again.
	// A new process must still retain the previous owner's observed conflict.
	if _, _, err := reopenMemory(t, b, k); err == nil {
		t.Fatal("restart erased a durably observed ownership quarantine")
	}
}

func kernelRecoveryFixture(t *testing.T) (RemoteNetlinkConfig, netns.NsHandle, *netlink.Handle) {
	t.Helper()
	recoveryRoot(t)
	if os.Getenv("EVPN_OWNED_KERNEL") != "disposable" {
		t.Skip("requires disposable privileged runtime")
	}
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "recover-br"}}
	if err := h.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(br); err != nil {
		t.Fatal(err)
	}
	vx := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "recover-vx", MasterIndex: br.Index}, VxlanId: 100, Port: 60100}
	if err := h.LinkAdd(vx); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(vx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.Close()
		if err := netns.Set(original); err != nil {
			panic(err)
		}
		ns.Close()
		original.Close()
		runtime.UnlockOSThread()
	})
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	d := recoveryDomain()
	d.BootID, d.NetworkNamespace = strings.TrimSpace(string(boot)), ns.UniqueId()
	return RemoteNetlinkConfig{Enabled: true, ExclusiveWriter: true, StateDirectory: t.TempDir(),
		Recovery: &RemoteNetlinkRecovery{Domain: d},
		Networks: []RemoteNetlinkNetwork{{NetworkID: "remote-test", LogicalSwitch: "test-ls", VNI: 100,
			BridgeDevice: "recover-br", VXLANDevice: "recover-vx"}}}, ns, h
}

func kernelImporter(t *testing.T, cfg RemoteNetlinkConfig) (*RemoteType2Importer, *OVNNetlinkBackend) {
	t.Helper()
	b, err := NewOwnedOVNNetlinkBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	i, err := NewValidatedRemoteType2Importer(reader, b)
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	return i, b
}

func TestImporterRecoveryKernelRestart(t *testing.T) {
	cfg, _, h := kernelRecoveryFixture(t)
	i, b := kernelImporter(t, cfg)
	p := importPath(t, nil)
	applyOwned(t, i, ownedSnapshot(t, i, p))
	before := b.Receipts()[0]
	if _, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
		t.Fatal("overlapping journal writers accepted")
	}
	b.Close()
	i, b = kernelImporter(t, cfg)
	defer b.Close()
	applyOwned(t, i, ownedSnapshot(t, i, p))
	if !reflect.DeepEqual(before, b.Receipts()[0]) {
		t.Fatal("real kernel recovery changed receipts")
	}
	applyOwned(t, i, ownedSnapshot(t, i))
	for _, scope := range []struct{ index, family int }{{before.FDB.Request.Ifindex, unix.AF_BRIDGE}, {before.Neighbor.Request.Ifindex, unix.AF_INET}} {
		entries, err := h.NeighList(scope.index, scope.family)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range entries {
			if n.HardwareAddr.String() == before.Endpoint.MAC {
				t.Fatal("obsolete owned state remains")
			}
		}
	}
}

func TestImporterRecoveryKernelIdentityChange(t *testing.T) {
	for _, mode := range []string{"namespace", "device"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, h := kernelRecoveryFixture(t)
			i, b := kernelImporter(t, cfg)
			applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
			b.Close()
			if mode == "namespace" {
				cfg.Recovery.Domain.NetworkNamespace = "different-network-namespace"
			} else {
				link, err := h.LinkByName("recover-vx")
				if err != nil {
					t.Fatal(err)
				}
				if err := h.LinkDel(link); err != nil {
					t.Fatal(err)
				}
				bridge, err := h.LinkByName("recover-br")
				if err != nil {
					t.Fatal(err)
				}
				replacement := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "recover-vx",
					MasterIndex: bridge.Attrs().Index}, VxlanId: 100, Port: 60100}
				if err := h.LinkAdd(replacement); err != nil {
					t.Fatal(err)
				}
			}
			if b, err := NewOwnedOVNNetlinkBackend(cfg); err == nil {
				b.Close()
				t.Fatal("changed namespace/device identity accepted")
			}
		})
	}
}

func TestImporterRecoveryCrashHelper(t *testing.T) {
	path := os.Getenv("EVPN_IMPORT_CRASH_CONFIG")
	if path == "" {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(netns.NsHandle(3)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg RemoteNetlinkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	i, b := kernelImporter(t, cfg)
	defer b.Close()
	boundary, occurrence := os.Getenv("EVPN_IMPORT_CRASH_BOUNDARY"), os.Getenv("EVPN_IMPORT_CRASH_OCCURRENCE")
	count := 0
	b.owned.fault = func(name string) {
		if name == boundary {
			count++
			if fmt.Sprint(count) == occurrence {
				fmt.Println("mutation-boundary-ready")
				select {}
			}
		}
	}
	if os.Getenv("EVPN_IMPORT_CRASH_DELETE") == "1" {
		applyOwned(t, i, ownedSnapshot(t, i))
	} else {
		applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
	}
	t.Fatal("requested mutation boundary not reached")
}

func TestImporterRecoveryKernelSIGKILL(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		for _, boundary := range []string{"before-add", "after-add", "before-add-ack", "after-add-ack"} {
			if deletion {
				boundary = strings.ReplaceAll(boundary, "add", "delete")
			}
			for _, occurrence := range []string{"1", "2"} {
				t.Run(fmt.Sprintf("delete=%t/%s/%s", deletion, boundary, occurrence), func(t *testing.T) {
					cfg, ns, _ := kernelRecoveryFixture(t)
					i, b := kernelImporter(t, cfg)
					if deletion {
						applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
					}
					b.Close()
					data, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(t.TempDir(), "config.json")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command(os.Args[0], "-test.run=^TestImporterRecoveryCrashHelper$")
					cmd.Env = append(os.Environ(), "EVPN_IMPORT_CRASH_CONFIG="+path,
						"EVPN_IMPORT_CRASH_BOUNDARY="+boundary, "EVPN_IMPORT_CRASH_OCCURRENCE="+occurrence,
						fmt.Sprintf("EVPN_IMPORT_CRASH_DELETE=%d", map[bool]int{true: 1}[deletion]))
					fd, err := unix.Dup(int(ns))
					if err != nil {
						t.Fatal(err)
					}
					nsFile := os.NewFile(uintptr(fd), "netns")
					defer nsFile.Close()
					cmd.ExtraFiles = []*os.File{nsFile}
					out, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					cmd.Stderr = os.Stderr
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { cmd.Process.Kill() })
					ready := make(chan bool, 1)
					go func() {
						scanner := bufio.NewScanner(out)
						for scanner.Scan() {
							if scanner.Text() == "mutation-boundary-ready" {
								ready <- true
								return
							}
						}
						ready <- false
					}()
					select {
					case ok := <-ready:
						if !ok {
							t.Fatal("child did not reach actual Netlink boundary")
						}
					case <-time.After(15 * time.Second):
						t.Fatal("mutation child timed out")
					}
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					cmd.Wait()
					recovered, err := NewOwnedOVNNetlinkBackend(cfg)
					if boundary != "after-add-ack" && boundary != "after-delete-ack" {
						if err == nil {
							recovered.Close()
							t.Fatal("unacknowledged mutation recovered from tuple matching")
						}
						return
					}
					if err != nil {
						t.Fatal("durably acknowledged operation could not recover", err)
					}
					reader, err := NewReadOnlyType2Importer(&importTestSource{}, importConfig())
					if err != nil {
						t.Fatal(err)
					}
					i, err = NewValidatedRemoteType2Importer(reader, recovered)
					if err != nil {
						t.Fatal(err)
					}
					if deletion {
						applyOwned(t, i, ownedSnapshot(t, i))
						if len(recovered.Receipts()) != 0 {
							t.Fatal("retiring receipt not completed")
						}
					} else {
						applyOwned(t, i, ownedSnapshot(t, i, importPath(t, nil)))
						if recovered.Receipts()[0].Status != "applied" {
							t.Fatal("partial acknowledged add not completed")
						}
					}
					recovered.Close()
				})
			}
		}
	}
}
