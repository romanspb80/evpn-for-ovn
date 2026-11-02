package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"

	"github.com/osrg/gobgp/v4/api"
	"github.com/ovn-kubernetes/libovsdb/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type RemoteRuntimeOptions struct {
	ConfigPath string
	StatusPath string
	Interval   time.Duration
	Timeout    time.Duration
}

type RemoteRuntimeConfig struct {
	Enabled      bool                     `json:"Enabled"`
	GRPCEndpoint string                   `json:"GRPCEndpoint"`
	ASN          uint32                   `json:"ASN"`
	RouterID     string                   `json:"RouterID"`
	Import       evpn.RemoteImportConfig  `json:"Import"`
	Netlink      evpn.RemoteNetlinkConfig `json:"Netlink"`
}

// RemoteRuntimeStatus is one launch-bound importer reconciliation observation.
type RemoteRuntimeStatus struct {
	Time        time.Time                   `json:"Time"`
	Disabled    bool                        `json:"Disabled,omitempty"`
	Error       string                      `json:"Error,omitempty"`
	Snapshot    evpn.RemoteImportSnapshot   `json:"Snapshot"`
	Receipts    []evpn.RemoteNetlinkReceipt `json:"Receipts"`
	Withdrawals []evpn.RemoteNetlinkReceipt `json:"Withdrawals"`
}

func LoadRemoteRuntimeConfig(path string) (RemoteRuntimeConfig, error) {
	var cfg *RemoteRuntimeConfig
	f, err := os.Open(path)
	if err != nil {
		return RemoteRuntimeConfig{}, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return RemoteRuntimeConfig{}, err
	}
	if cfg == nil {
		return RemoteRuntimeConfig{}, fmt.Errorf("remote import config must be a JSON object")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return RemoteRuntimeConfig{}, fmt.Errorf("remote import config must contain exactly one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return RemoteRuntimeConfig{}, err
	}
	return *cfg, nil
}

func (c RemoteRuntimeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !isNodeLocalUnixEndpoint(c.GRPCEndpoint) {
		return fmt.Errorf("enabled remote import requires an explicit node-local unix gRPC endpoint")
	}
	if c.ASN == 0 || net.ParseIP(c.RouterID).To4() == nil {
		return fmt.Errorf("enabled remote import requires an explicit ASN and IPv4 router ID")
	}
	if err := c.Import.Validate(); err != nil {
		return fmt.Errorf("invalid remote import policy: %w", err)
	}
	if !c.Netlink.Enabled || !c.Netlink.ExclusiveWriter {
		return fmt.Errorf("enabled remote import requires netlink enabled with exclusive writer ownership")
	}
	if err := c.Netlink.Validate(); err != nil {
		return fmt.Errorf("invalid remote netlink config: %w", err)
	}
	return nil
}

func isNodeLocalUnixEndpoint(endpoint string) bool {
	if strings.TrimSpace(endpoint) != endpoint || endpoint == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "unix" && u.Host == "" && u.Opaque == "" &&
		u.RawQuery == "" && u.Fragment == "" && filepath.IsAbs(u.Path) && u.Path != "/"
}

func validateRemoteRuntimeOptions(runtime RuntimeOptions, opts RemoteRuntimeOptions, cfg RemoteRuntimeConfig, report func(RemoteRuntimeStatus)) error {
	if opts.ConfigPath == "" || report == nil {
		return fmt.Errorf("remote import config path and reporter required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	if runtime.Mode != RuntimeModeRealOVNOVS || runtime.NetworkBackend != "linux-veth" {
		return fmt.Errorf("remote EVPN import requires real-ovn-ovs/linux-veth")
	}
	if opts.Interval <= 0 || opts.Timeout <= 0 {
		return fmt.Errorf("remote import interval and timeout must be positive")
	}
	if strings.TrimSpace(runtime.OVNNBEndpoint) == "" {
		return fmt.Errorf("remote import requires an OVN NBDB endpoint")
	}
	return nil
}

type remoteNetworkMappingGate struct {
	uuids   map[string]string
	blocked error
}

func (g *remoteNetworkMappingGate) verify(networks []evpn.RemoteNetlinkNetwork, switches []nbdb.LogicalSwitch) error {
	if g.blocked != nil {
		return g.blocked
	}
	next := make(map[string]string, len(networks))
	for _, network := range networks {
		var matches []nbdb.LogicalSwitch
		for _, ls := range switches {
			if ls.Name == network.LogicalSwitch {
				matches = append(matches, ls)
			}
		}
		var err error
		switch {
		case len(matches) != 1:
			err = fmt.Errorf("network %s requires exactly one logical switch %q; observed %d", network.NetworkID, network.LogicalSwitch, len(matches))
		case matches[0].UUID == "":
			err = fmt.Errorf("logical switch %q has no UUID", network.LogicalSwitch)
		case matches[0].OtherConfig["dynamic-routing-vni"] != strconv.FormatUint(uint64(network.VNI), 10) ||
			matches[0].OtherConfig["dynamic-routing-bridge-ifname"] != network.BridgeDevice ||
			matches[0].OtherConfig["dynamic-routing-vxlan-ifname"] != network.VXLANDevice:
			err = fmt.Errorf("logical switch %q native dynamic-routing mapping differs from configured VNI/devices", network.LogicalSwitch)
		case g.uuids[network.LogicalSwitch] != "" && g.uuids[network.LogicalSwitch] != matches[0].UUID:
			err = fmt.Errorf("logical switch %q UUID changed; restart required", network.LogicalSwitch)
		}
		if err != nil {
			if len(g.uuids) != 0 {
				g.blocked = err
			}
			return err
		}
		next[network.LogicalSwitch] = matches[0].UUID
	}
	g.uuids = next
	return nil
}

// RunRemoteType2Importer uses one config snapshot for its lifetime and never
// polls the speaker separately from the importer's single reconciliation call.
func (s *CNIServer) RunRemoteType2Importer(ctx context.Context, runtime RuntimeOptions, opts RemoteRuntimeOptions, report func(RemoteRuntimeStatus)) (runErr error) {
	if opts.ConfigPath == "" {
		return nil
	}
	if report == nil {
		return fmt.Errorf("remote import reporter required")
	}
	cfg, err := LoadRemoteRuntimeConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	if err := validateRemoteRuntimeOptions(runtime, opts, cfg, report); err != nil {
		return err
	}
	if !cfg.Enabled {
		status := RemoteRuntimeStatus{Time: time.Now().UTC(), Disabled: true}
		if err := writeRemoteRuntimeStatus(opts.StatusPath, &status); err != nil {
			status.Error = err.Error()
			report(status)
			return err
		}
		report(status)
		return nil
	}

	conn, err := grpc.NewClient(cfg.GRPCEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect to GoBGP gRPC endpoint: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, conn.Close()) }()
	apiClient := api.NewGoBgpServiceClient(conn)
	gate := evpn.SpeakerReadiness{Client: apiClient, ASN: cfg.ASN, RouterID: cfg.RouterID}
	readOnly, err := evpn.NewReadOnlyType2Importer(&evpn.GoBGPRemoteRIBSource{Client: apiClient, Gate: gate}, cfg.Import)
	if err != nil {
		return fmt.Errorf("create read-only remote Type-2 importer: %w", err)
	}
	backend, err := evpn.NewOwnedOVNNetlinkBackend(cfg.Netlink)
	if err != nil {
		return fmt.Errorf("create owned OVN netlink backend: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, backend.Close()) }()
	importer, err := evpn.NewValidatedRemoteType2Importer(readOnly, backend)
	if err != nil {
		return fmt.Errorf("create validated remote Type-2 importer: %w", err)
	}

	var nb client.Client
	defer func() {
		if nb != nil {
			nb.Close()
		}
	}()
	mappingGate := &remoteNetworkMappingGate{}
	checkMapping := func(ctx context.Context) error {
		if mappingGate.blocked != nil {
			return mappingGate.blocked
		}
		if nb == nil {
			var err error
			nb, err = newOVNNBClient(ctx, runtime.OVNNBEndpoint)
			if err != nil {
				return fmt.Errorf("read native remote network mappings: %w", err)
			}
		}
		if !nb.Connected() {
			return fmt.Errorf("native remote network mapping NBDB connection unavailable")
		}
		var switches []nbdb.LogicalSwitch
		if err := nb.List(ctx, &switches); err != nil {
			return fmt.Errorf("read native remote network mappings: %w", err)
		}
		return mappingGate.verify(cfg.Netlink.Networks, switches)
	}
	tick := func() error {
		stepCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		mappingErr := checkMapping(stepCtx)
		err := mappingErr
		if err == nil {
			err = importer.ReconcileOnce(stepCtx)
		}
		cancel()
		status := RemoteRuntimeStatus{
			Time:        time.Now().UTC(),
			Snapshot:    importer.LastSnapshot(),
			Receipts:    backend.Receipts(),
			Withdrawals: backend.Withdrawals(),
		}
		if mappingErr != nil {
			status.Snapshot.Authoritative = false
		}
		if err != nil {
			status.Error = err.Error()
		}
		if writeErr := writeRemoteRuntimeStatus(opts.StatusPath, &status); writeErr != nil {
			err = errors.Join(err, writeErr)
			status.Error = err.Error()
			report(status)
			return writeErr
		}
		report(status)
		return nil
	}

	if err := tick(); err != nil {
		return err
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := tick(); err != nil {
				return err
			}
		}
	}
}

func writeRemoteRuntimeStatus(path string, status *RemoteRuntimeStatus) error {
	if path == "" {
		return nil
	}
	value, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode remote import status: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, value, 0600); err != nil {
		return fmt.Errorf("write remote import status %s: %w", filepath.Clean(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write remote import status %s: %w", filepath.Clean(path), err)
	}
	return nil
}
