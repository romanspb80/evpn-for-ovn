package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type attestedReadiness struct {
	readiness  evpn.ReadinessGate
	continuity *unixSpeakerContinuity
}

func (g attestedReadiness) Check(ctx context.Context) error {
	// The first RPC causes the actual transport dial and peer binding.
	if err := g.readiness.Check(ctx); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) && status.Code(err) == codes.Canceled {
			return &inventoryFailure{Cause: "observation-canceled", Err: err}
		}
		if status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Unavailable {
			// A transport status is not identity evidence. Re-attest the
			// actual bound peer independently of the expired RPC deadline.
			proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if proofErr := g.continuity.Check(proofCtx); proofErr == nil {
				return &inventoryFailure{Cause: "attested-speaker-rpc-unavailable", Err: err}
			} else {
				var classified *inventoryFailure
				if errors.As(proofErr, &classified) {
					return &inventoryFailure{Cause: classified.Cause, Security: classified.Security, Err: errors.Join(err, proofErr)}
				}
				g.continuity.mu.Lock()
				bound, dialErr := g.continuity.bound, g.continuity.dialFailure
				g.continuity.mu.Unlock()
				if !bound && errors.As(dialErr, &classified) && !classified.Security {
					return &inventoryFailure{Cause: classified.Cause, Err: errors.Join(err, proofErr, dialErr)}
				}
				return errors.Join(err, fmt.Errorf("speaker continuity after failed RPC: %w", proofErr))
			}
		}
		return err
	}
	return g.continuity.Check(ctx)
}

func newProviderTransport(path, target string, asn uint32, routerID, installationUID, nodeUID string) (*grpc.ClientConn, *unixSpeakerContinuity, api.GoBgpServiceClient, error) {
	if path == "" || !isNodeLocalUnixEndpoint(target) {
		return nil, nil, nil, fmt.Errorf("RECOVERY_BLOCKED: actual Unix peer provider attestation configuration required")
	}
	guard, err := newAttestedSpeakerContinuityForSpeaker(path, speakerRuntimeExpectation{
		ASN: asn, RouterID: routerID, InstallationUID: installationUID, NodeUID: nodeUID,
	})
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
