// Remote2 composes the existing Type-2 primitives for a disposable Linux lab.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"google.golang.org/protobuf/proto"
)

type config struct {
	Socket, Attestation, State, EndpointNamespace, EndpointDevice string
	InstallationUID, NodeUID                                      string
	Endpoint                                                      evpn.LocalEndpoint
	Import                                                        evpn.RemoteImportConfig
	Netlink                                                       evpn.RemoteNetlinkNetwork
}

type endpointSource struct {
	config    config
	lifecycle *lifecycle
}

type projectorEvidence struct {
	PID        int
	StartTicks string
}

func currentProjector() (projectorEvidence, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return projectorEvidence{}, err
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if len(fields) < 20 {
		return projectorEvidence{}, fmt.Errorf("incomplete projector process identity")
	}
	return projectorEvidence{PID: os.Getpid(), StartTicks: fields[19]}, nil
}

type fixtureRIBSource struct {
	source  evpn.RemoteRIBSource
	control string
}

func (s fixtureRIBSource) ReadRemoteRIB(ctx context.Context) (evpn.RemoteRIBSnapshot, error) {
	raw, err := s.source.ReadRemoteRIB(ctx)
	if err != nil {
		return raw, err
	}
	mode, err := os.ReadFile(s.control)
	if os.IsNotExist(err) {
		return raw, nil
	}
	if err != nil {
		return evpn.RemoteRIBSnapshot{}, err
	}
	switch string(mode) {
	case "incomplete\n":
		raw.Complete = false
		return raw, nil
	case "conflict\n":
		for _, path := range raw.Paths {
			if path.NeighborIp != "198.18.20.2" {
				continue
			}
			conflict := proto.Clone(path).(*api.Path)
			attrs, err := apiutil.GetNativePathAttributes(conflict)
			if err != nil {
				return evpn.RemoteRIBSnapshot{}, err
			}
			conflict.Pattrs, conflict.PattrsBinary = nil, nil
			changed := false
			for _, attr := range attrs {
				if reach, ok := attr.(*bgp.PathAttributeMpReachNLRI); ok {
					reach.Nexthop = netip.MustParseAddr("198.18.20.99")
					changed = true
				}
				data, err := attr.Serialize()
				if err != nil {
					return evpn.RemoteRIBSnapshot{}, err
				}
				conflict.PattrsBinary = append(conflict.PattrsBinary, data)
			}
			if !changed {
				return evpn.RemoteRIBSnapshot{}, fmt.Errorf("conflict seam lacks MP_REACH")
			}
			raw.Paths = append(raw.Paths, conflict)
			return raw, nil
		}
		return evpn.RemoteRIBSnapshot{}, fmt.Errorf("conflict seam lacks actual received route")
	default:
		return evpn.RemoteRIBSnapshot{}, fmt.Errorf("invalid observation seam")
	}
}

func (s endpointSource) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	return s.lifecycle.desired(ctx, s.config, observeEndpoint)
}

func observeEndpoint(ctx context.Context, c config) ([]evpn.LocalEndpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ns, err := netns.GetFromName(c.EndpointNamespace)
	if err != nil {
		return nil, fmt.Errorf("endpoint observation uncertain: %w", err)
	}
	defer ns.Close()
	handle, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	link, err := handle.LinkByName(c.EndpointDevice)
	if err != nil {
		return nil, err
	}
	ep := c.Endpoint
	if link.Attrs().HardwareAddr.String() != ep.MACAddress || link.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("endpoint identity/readiness differs")
	}
	addresses, err := handle.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	found := false
	for _, address := range addresses {
		found = found || address.IP.String() == ep.IPAddresses[0]
	}
	if !found {
		return nil, fmt.Errorf("endpoint IP differs")
	}
	return []evpn.LocalEndpoint{ep}, nil
}

func write(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".next", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".next", path)
}

func openImportRoot(path string) error {
	if err := os.Mkdir(path, 0700); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != 0 {
		return fmt.Errorf("unsafe existing importer root")
	}
	for _, name := range []string{"remote-netlink-owner", "receipts-v1/journal.json"} {
		if _, err := os.Lstat(filepath.Join(path, name)); err != nil {
			return fmt.Errorf("existing importer root requires original durable metadata: %w", err)
		}
	}
	return nil
}

func run(path string) (runErr error) {
	data, err := protectedRead(path)
	if err != nil {
		return err
	}
	var c config
	if err := decode(data, &c); err != nil {
		return err
	}
	if c.Endpoint.NetworkID != "ext2" || c.Endpoint.VNI != 2000 ||
		c.Endpoint.LocalVTEP != "198.18.20.4" || c.Endpoint.RouteDistinguisher != "198.18.20.3:2000" ||
		c.Endpoint.MACAddress != "02:00:00:02:00:66" || len(c.Endpoint.IPAddresses) != 1 ||
		c.Endpoint.IPAddresses[0] != "10.100.2.101" || !c.Endpoint.Advertise ||
		c.EndpointNamespace != "remote2-work" || c.EndpointDevice != "r2-work" ||
		c.Netlink.LogicalSwitch != "linux-bridge:br2000" || !filepath.IsAbs(c.State) {
		return fmt.Errorf("fixture endpoint/kernel ownership contract differs")
	}
	if _, err := evpn.BuildType2Route(c.Endpoint); err != nil {
		return err
	}
	lifetime, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	projector, err := currentProjector()
	if err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	ns, err := netns.Get()
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, ns.Close()) }()
	digest := sha256.Sum256(data)
	domain := ownership.Domain{InstallationUID: c.InstallationUID, NodeUID: c.NodeUID,
		BootID: string(boot[:len(boot)-1]), NetworkNamespace: ns.UniqueId(),
		ConfigDigest: hex.EncodeToString(digest[:])}
	ledger, err := openLifecycle(c, domain)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, ledger.journal.Close()) }()
	ctx, cancel := context.WithTimeout(lifetime, 10*time.Second)
	initial, err := connect(ctx, c)
	cancel()
	if err != nil {
		return err
	}
	binding := &speakerBinding{current: initial, observer: endpointSource{c, ledger},
		control: filepath.Join(c.State, "observation")}
	defer func() { runErr = errors.Join(runErr, binding.Close()) }()
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{
		StateDirectory: filepath.Join(c.State, "export"), Domain: domain}, initial.adapter, initial.applier, initial.guard)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, recovery.Close()) }()
	binding.recovery = recovery
	binding.exporter = binding.exporterFor(initial)
	reader, err := evpn.NewReadOnlyType2Importer(fixtureRIBSource{initial.source, binding.control}, c.Import)
	if err != nil {
		return err
	}
	binding.reader = reader
	importRoot := filepath.Join(c.State, "import")
	if err := openImportRoot(importRoot); err != nil {
		return err
	}
	backend, err := evpn.NewOwnedOVNNetlinkBackend(evpn.RemoteNetlinkConfig{
		Enabled: true, ExclusiveWriter: true, StateDirectory: importRoot,
		Networks: []evpn.RemoteNetlinkNetwork{c.Netlink},
		Recovery: &evpn.RemoteNetlinkRecovery{Domain: domain}})
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, backend.Close()) }()
	importer, err := evpn.NewValidatedRemoteType2Importer(reader, backend)
	if err != nil {
		return err
	}
	if err := ledger.bootstrap(); err != nil {
		return err
	}
	for {
		ctx, cancel := context.WithTimeout(lifetime, 5*time.Second)
		stepErr := immutableConfig(path, data)
		if stepErr == nil {
			stepErr = binding.ensure(ctx, c, connect)
		}
		export := evpn.ExportResult{OwnershipUnverified: true, RecoveryState: "RECOVERY_BLOCKED"}
		if stepErr == nil {
			var exportErr error
			export, exportErr = binding.exporter.Reconcile(ctx)
			stepErr = errors.Join(exportErr, ledger.acknowledge(export, exportErr), importer.ReconcileOnce(ctx))
		}
		cancel()
		status := struct {
			Time          time.Time
			Export        evpn.ExportResult
			Snapshot      evpn.RemoteImportSnapshot
			Receipts      []evpn.RemoteNetlinkReceipt
			Speaker       any
			Projector     projectorEvidence
			Lifecycle     lifecycleState
			RecoveryState string
			Quarantines   []evpn.RemoteSlotQuarantine
			Error         string
		}{Time: time.Now().UTC(), Export: export, Snapshot: importer.LastSnapshot(),
			Receipts: backend.Receipts(), Lifecycle: ledger.state, Projector: projector,
			RecoveryState: backend.RecoveryState(), Quarantines: backend.Quarantines()}
		if binding.current != nil {
			status.Speaker = binding.current.guard.Evidence()
		}
		if stepErr != nil {
			status.Snapshot.Authoritative = false
			status.Error = stepErr.Error()
			log.Print(stepErr)
		}
		if err := write(filepath.Join(c.State, "status.json"), status); err != nil {
			return err
		}
		select {
		case <-lifetime.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func main() {
	path := flag.String("config", "", "protected fixture configuration")
	flag.Parse()
	if err := run(*path); err != nil {
		log.Fatal(err)
	}
}
