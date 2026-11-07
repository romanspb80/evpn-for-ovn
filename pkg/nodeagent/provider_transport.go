package nodeagent

import (
	"context"
	"fmt"
	"net"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type attestedReadiness struct {
	readiness  evpn.ReadinessGate
	continuity *unixSpeakerContinuity
}

func (g attestedReadiness) Check(ctx context.Context) error {
	// The first RPC causes the actual transport dial and peer binding.
	if err := g.readiness.Check(ctx); err != nil {
		return err
	}
	return g.continuity.Check(ctx)
}

func newProviderTransport(path, target string) (*grpc.ClientConn, *unixSpeakerContinuity, api.GoBgpServiceClient, error) {
	if path == "" || !isNodeLocalUnixEndpoint(target) {
		return nil, nil, nil, fmt.Errorf("RECOVERY_BLOCKED: actual Unix peer provider attestation configuration required")
	}
	guard, err := NewAttestedSpeakerContinuity(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("RECOVERY_BLOCKED: provider attestation: %w", err)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return guard.DialContext(ctx, target)
		}))
	if err != nil {
		guard.Close()
		return nil, nil, nil, err
	}
	return conn, guard, api.NewGoBgpServiceClient(conn), nil
}
