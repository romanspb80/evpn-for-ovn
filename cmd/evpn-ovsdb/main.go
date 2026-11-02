package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"sigs.k8s.io/yaml"
)

type standaloneFileConfig struct {
	GoBGP              goBGPConfig   `json:"gobgp"`
	LocalVTEP          string        `json:"local_vtep"`
	OVNNBEndpoint      string        `json:"ovn_nb_endpoint"`
	OVSDBEndpoint      string        `json:"ovsdb_endpoint"`
	PollInterval       string        `json:"poll_interval"`
	UDPPort            uint16        `json:"udp_port"`
	ExclusiveWriter    bool          `json:"exclusive_writer"`
	Network            networkConfig `json:"network"`
	ImportRouteTargets []string      `json:"import_route_targets"`
	AllowedRemoteVTEPs []string      `json:"allowed_remote_vteps"`
}

type goBGPConfig struct {
	Endpoint string `json:"endpoint"`
	ASN      uint32 `json:"asn"`
	RouterID string `json:"router_id"`
}

type networkConfig struct {
	ID              string `json:"id"`
	LogicalSwitch   string `json:"logical_switch"`
	VNI             uint32 `json:"vni"`
	PhysicalNetwork string `json:"physical_network"`
	ProviderBridge  string `json:"provider_bridge"`
}

type goBGPSpeakerReadiness struct {
	Client   api.GoBgpServiceClient
	ASN      uint32
	RouterID string
}

func (r goBGPSpeakerReadiness) Check(ctx context.Context) error {
	if r.Client == nil {
		return fmt.Errorf("GoBGP readiness client is required")
	}
	response, err := r.Client.GetBgp(ctx, &api.GetBgpRequest{})
	if err != nil {
		return fmt.Errorf("read GoBGP speaker identity: %w", err)
	}
	global := response.GetGlobal()
	if global == nil || global.Asn != r.ASN || global.RouterId != r.RouterID {
		return fmt.Errorf("GoBGP speaker identity mismatch: expected ASN=%d routerID=%s, got %v",
			r.ASN, r.RouterID, global)
	}
	return nil
}

func (c standaloneFileConfig) validate() (time.Duration, evpn.Model1Network, evpn.RemoteImportConfig, error) {
	required := map[string]string{
		"GoBGP endpoint": c.GoBGP.Endpoint, "OVN NBDB endpoint": c.OVNNBEndpoint,
		"OVSDB endpoint": c.OVSDBEndpoint, "GoBGP router ID": c.GoBGP.RouterID,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("%s is required", name)
		}
	}
	routerID := net.ParseIP(c.GoBGP.RouterID)
	if c.GoBGP.ASN == 0 || routerID == nil || routerID.To4() == nil {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("valid nonzero GoBGP ASN and IPv4 router ID are required")
	}
	if c.UDPPort == 0 {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("nonzero VXLAN UDP port is required")
	}
	if !c.ExclusiveWriter {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("exclusive_writer must explicitly acknowledge isolated writer ownership")
	}
	interval, err := time.ParseDuration(c.PollInterval)
	if err != nil || interval <= 0 {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("poll_interval must be a positive Go duration")
	}
	network := evpn.Model1Network{
		NetworkID: c.Network.ID, LogicalSwitch: c.Network.LogicalSwitch,
		PhysicalNetwork: c.Network.PhysicalNetwork, ProviderBridge: c.Network.ProviderBridge,
		VNI: c.Network.VNI,
	}
	modelConfig := evpn.Model1Config{
		Networks: []evpn.Model1Network{network}, UDPPort: c.UDPPort, ExclusiveWriter: true,
	}
	if err := modelConfig.Validate(); err != nil {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, err
	}
	if len(c.AllowedRemoteVTEPs) != 1 {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("exactly one allowed remote VTEP is required")
	}
	importConfig := evpn.RemoteImportConfig{
		LocalVTEP: c.LocalVTEP,
		Networks: []evpn.RemoteImportNetwork{{
			NetworkID: network.NetworkID, VNI: network.VNI,
			ImportRouteTargets: append([]string(nil), c.ImportRouteTargets...),
			AllowedVTEPs:       append([]string(nil), c.AllowedRemoteVTEPs...),
		}},
	}
	if err := importConfig.Validate(); err != nil {
		return 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, err
	}
	return interval, network, importConfig, nil
}

func loadConfig(path string) (standaloneFileConfig, time.Duration, evpn.Model1Network, evpn.RemoteImportConfig, error) {
	if strings.TrimSpace(path) == "" {
		return standaloneFileConfig{}, 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("-config is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return standaloneFileConfig{}, 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("read config: %w", err)
	}
	var config standaloneFileConfig
	if err := yaml.UnmarshalStrict(raw, &config); err != nil {
		return standaloneFileConfig{}, 0, evpn.Model1Network{}, evpn.RemoteImportConfig{}, fmt.Errorf("decode config: %w", err)
	}
	interval, network, importerConfig, err := config.validate()
	return config, interval, network, importerConfig, err
}

func main() {
	configPath := flag.String("config", "", "path to the isolated OVSDB-Programmed YAML configuration")
	flag.Parse()
	config, interval, network, importerConfig, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, interval, network, importerConfig); err != nil {
		log.Fatal(err)
	}
}

func run(
	ctx context.Context,
	config standaloneFileConfig,
	interval time.Duration,
	network evpn.Model1Network,
	importerConfig evpn.RemoteImportConfig,
) error {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	log.Printf("connecting OVN NBDB endpoint=%s", config.OVNNBEndpoint)
	nbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return fmt.Errorf("build OVN NBDB model: %w", err)
	}
	nbClient, err := client.NewOVSDBClient(nbModel, client.WithEndpoint(config.OVNNBEndpoint))
	if err != nil {
		return fmt.Errorf("create OVN NBDB client: %w", err)
	}
	if err := nbClient.Connect(connectCtx); err != nil {
		return fmt.Errorf("connect OVN NBDB: %w", err)
	}
	defer nbClient.Close()
	log.Printf("connected OVN NBDB; monitoring database")
	if _, err := nbClient.MonitorAll(connectCtx); err != nil {
		return fmt.Errorf("monitor OVN NBDB: %w", err)
	}

	log.Printf("connecting OVSDB endpoint=%s", config.OVSDBEndpoint)
	ovsModel, err := vswitchd.FullDatabaseModel()
	if err != nil {
		return fmt.Errorf("build OVSDB model: %w", err)
	}
	ovsClient, err := client.NewOVSDBClient(ovsModel, client.WithEndpoint(config.OVSDBEndpoint))
	if err != nil {
		return fmt.Errorf("create OVSDB client: %w", err)
	}
	if err := ovsClient.Connect(connectCtx); err != nil {
		return fmt.Errorf("connect OVSDB: %w", err)
	}
	defer ovsClient.Close()
	log.Printf("connected OVSDB; monitoring database")
	if _, err := ovsClient.MonitorAll(connectCtx); err != nil {
		return fmt.Errorf("monitor OVSDB: %w", err)
	}

	log.Printf("connecting GoBGP endpoint=%s", config.GoBGP.Endpoint)
	gobgpConn, err := grpc.NewClient(
		config.GoBGP.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("create GoBGP gRPC client: %w", err)
	}
	defer gobgpConn.Close()
	log.Printf("GoBGP gRPC client created; preparing Type-2 route observer")
	gobgpClient := api.NewGoBgpServiceClient(gobgpConn)
	ribSource := &evpn.GoBGPRemoteRIBSource{
		Client: gobgpClient,
		Gate: goBGPSpeakerReadiness{
			Client: gobgpClient, ASN: config.GoBGP.ASN, RouterID: config.GoBGP.RouterID,
		},
	}
	importer, err := evpn.NewReadOnlyType2Importer(ribSource, importerConfig)
	if err != nil {
		return fmt.Errorf("create validated Type-2 importer: %w", err)
	}
	programmer, err := evpn.NewLibovsdbModel1Programmer(nbClient, ovsClient)
	if err != nil {
		return fmt.Errorf("create OVSDB-Programmed programmer: %w", err)
	}
	handler, err := evpn.NewModel1Type2Handler(importer, programmer, evpn.Model1Config{
		Networks: []evpn.Model1Network{network}, UDPPort: config.UDPPort,
		ExclusiveWriter: config.ExclusiveWriter,
	})
	if err != nil {
		return fmt.Errorf("create OVSDB-Programmed Type-2 handler: %w", err)
	}

	log.Printf("isolated OVSDB-Programmed observer started: network=%s VNI=%d provider-bridge=%s remote-VTEP=%s",
		network.NetworkID, network.VNI, network.ProviderBridge, config.AllowedRemoteVTEPs[0])
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snapshot, pollErr := importer.Poll(ctx)
		if pollErr != nil {
			log.Printf("OVSDB-Programmed RIB snapshot rejected; no state change: %v", pollErr)
		} else if !snapshot.Authoritative {
			log.Printf("OVSDB-Programmed waiting for a second matching complete Type-2 snapshot")
		} else if handlerErr := handler.ReconcileSnapshot(ctx, snapshot); handlerErr != nil {
			log.Printf("OVSDB-Programmed reconciliation failed; retrying next poll: %v", handlerErr)
		} else {
			log.Printf("OVSDB-Programmed reconciled authoritative snapshot generation=%d endpoints=%d",
				snapshot.Generation, len(snapshot.Endpoints))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
