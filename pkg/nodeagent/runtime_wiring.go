package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"

	"github.com/cenkalti/backoff/v4"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	RuntimeModeInMemory   = "in-memory"
	RuntimeModeRealOVNOVS = "real-ovn-ovs"
)

type RuntimeOptions struct {
	Mode           string
	NetworkBackend string
	OVNNBEndpoint  string
	OVSDBEndpoint  string
}

type RealServerFactory interface {
	Build(ctx context.Context, store *Store, opts RuntimeOptions) (*CNIServer, func(), error)
}

type defaultRealServerFactory struct{}

func BuildServerForOptions(ctx context.Context, store *Store, opts RuntimeOptions, realFactory RealServerFactory) (*CNIServer, func(), error) {
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = RuntimeModeInMemory
	}
	networkBackend := strings.TrimSpace(opts.NetworkBackend)
	if networkBackend == "" {
		networkBackend = "in-memory"
	}
	if realFactory == nil {
		realFactory = defaultRealServerFactory{}
	}

	switch mode {
	case RuntimeModeInMemory:
		switch networkBackend {
		case "in-memory":
			return NewCNIServer(store), nil, nil
		case "linux-veth":
			return NewCNIServerWithBackends(store, nil, NewLinuxVethBackend()), nil, nil
		default:
			return nil, nil, fmt.Errorf("unsupported network backend %q for mode %q", networkBackend, mode)
		}
	case RuntimeModeRealOVNOVS:
		if networkBackend != "linux-veth" {
			return nil, nil, fmt.Errorf("mode %q requires --network-backend=linux-veth", mode)
		}
		return realFactory.Build(ctx, store, opts)
	default:
		return nil, nil, fmt.Errorf("unsupported runtime mode %q", mode)
	}
}

func (defaultRealServerFactory) Build(ctx context.Context, store *Store, opts RuntimeOptions) (*CNIServer, func(), error) {
	nbEndpoint := strings.TrimSpace(opts.OVNNBEndpoint)
	ovsEndpoint := strings.TrimSpace(opts.OVSDBEndpoint)
	if nbEndpoint == "" {
		return nil, nil, fmt.Errorf("ovn nb endpoint is required for %q mode", RuntimeModeRealOVNOVS)
	}
	if ovsEndpoint == "" {
		return nil, nil, fmt.Errorf("ovsdb endpoint is required for %q mode", RuntimeModeRealOVNOVS)
	}

	nbClient, err := newOVNNBClient(ctx, nbEndpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("connect ovn nb endpoint %q: %w", nbEndpoint, err)
	}
	ovsClient, err := newOVSClient(ctx, ovsEndpoint)
	if err != nil {
		nbClient.Close()
		return nil, nil, fmt.Errorf("connect ovsdb endpoint %q: %w", ovsEndpoint, err)
	}

	ovsAttachmentMgr := attachment.NewOVSAttachmentManager(attachment.NewLibovsdbOVSStore(ovsClient))
	endpointMgr := endpoint.NewManager(endpoint.NewLibovsdbOVNBackend(nbClient), ovsAttachmentMgr, true)
	server := NewCNIServerWithBackendsAndChecker(store, endpointMgr, NewLinuxVethBackend(), ovsAttachmentChecker{mgr: ovsAttachmentMgr})
	cleanup := func() {
		ovsClient.Close()
		nbClient.Close()
	}
	return server, cleanup, nil
}

type ovsAttachmentChecker struct {
	mgr *attachment.OVSAttachmentManager
}

func (c ovsAttachmentChecker) CheckAttachment(ctx context.Context, spec endpoint.EndpointSpec) error {
	if c.mgr == nil {
		return fmt.Errorf("ovs attachment checker is not configured")
	}
	identity := endpoint.AttachmentIdentity{
		EndpointID:   string(spec.ID),
		NetworkID:    strings.TrimSpace(spec.NetworkID),
		AttachmentID: strings.TrimSpace(spec.AttachmentID),
		WorkloadUID:  strings.TrimSpace(spec.Workload.UID),
		Incarnation:  strings.TrimSpace(spec.Incarnation),
	}
	_, err := c.mgr.CheckAttachmentWithIdentity(ctx, identity, spec.LogicalSwitchPort, spec.OVSInterfaceName)
	return err
}

func newOVNNBClient(ctx context.Context, endpointAddress string) (client.Client, error) {
	nbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	nbClient, err := client.NewOVSDBClient(
		nbModel,
		client.WithEndpoint(endpointAddress),
		client.WithReconnect(2*time.Second, backoff.NewConstantBackOff(100*time.Millisecond)),
	)
	if err != nil {
		return nil, err
	}
	if err := nbClient.Connect(ctx); err != nil {
		return nil, err
	}
	if _, err := nbClient.MonitorAll(ctx); err != nil {
		nbClient.Close()
		return nil, err
	}
	return nbClient, nil
}

func newOVSClient(ctx context.Context, endpointAddress string) (client.Client, error) {
	ovsModel, err := model.NewClientDBModel("Open_vSwitch",
		map[string]model.Model{
			"Bridge":    &vswitch.Bridge{},
			"Port":      &vswitch.Port{},
			"Interface": &vswitch.Interface{},
		})
	if err != nil {
		return nil, err
	}
	ovsClient, err := client.NewOVSDBClient(
		ovsModel,
		client.WithEndpoint(endpointAddress),
		client.WithReconnect(2*time.Second, backoff.NewConstantBackOff(100*time.Millisecond)),
	)
	if err != nil {
		return nil, err
	}
	if err := ovsClient.Connect(ctx); err != nil {
		return nil, err
	}
	if _, err := ovsClient.MonitorAll(ctx); err != nil {
		ovsClient.Close()
		return nil, err
	}
	return ovsClient, nil
}
