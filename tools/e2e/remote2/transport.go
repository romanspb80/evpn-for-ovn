package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type speakerGuard interface {
	evpn.VerifiedSpeaker
	Evidence() nodeagent.SpeakerProcessEvidence
	Close() error
}

type attestedGate struct {
	readiness evpn.ReadinessGate
	guard     speakerGuard
}

func (g attestedGate) Check(ctx context.Context) error {
	if err := g.guard.Check(ctx); err != nil {
		return err
	}
	if err := g.readiness.Check(ctx); err != nil {
		return err
	}
	return g.guard.Check(ctx)
}

type transport struct {
	conn    io.Closer
	guard   speakerGuard
	gate    evpn.ReadinessGate
	adapter evpn.Type2RIBSource
	applier evpn.Type2RouteApplier
	source  evpn.RemoteRIBSource
}

func (t *transport) Close() error {
	return errors.Join(t.conn.Close(), t.guard.Close())
}

func connect(ctx context.Context, c config) (_ *transport, runErr error) {
	guard, err := nodeagent.NewAttestedSpeakerContinuity(c.Attestation)
	if err != nil {
		return nil, err
	}
	var conn *grpc.ClientConn
	defer func() {
		if runErr != nil {
			if conn != nil {
				runErr = errors.Join(runErr, conn.Close())
			}
			runErr = errors.Join(runErr, guard.Close())
		}
	}()
	conn, err = grpc.NewClient(c.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			return guard.DialContext(ctx, address)
		}))
	if err != nil {
		return nil, err
	}
	client := api.NewGoBgpServiceClient(conn)
	readiness := evpn.SpeakerReadiness{Client: client, ASN: 64513, RouterID: "198.18.20.3"}
	// The first read-only RPC dials and attests the actual Unix peer.
	if err := readiness.Check(ctx); err != nil {
		return nil, err
	}
	gate := attestedGate{readiness, guard}
	if err := gate.Check(ctx); err != nil {
		return nil, err
	}
	evidence := guard.Evidence()
	if evidence.Standalone == nil || evidence.Standalone.InstallationUID != c.InstallationUID ||
		evidence.Standalone.NodeUID != c.NodeUID {
		return nil, fmt.Errorf("actual speaker differs from recovery installation/node identity")
	}
	adapter := &evpn.GoBGPType2Adapter{Client: client, Gate: gate}
	return &transport{conn, guard, gate, adapter, adapter, &evpn.GoBGPRemoteRIBSource{Client: client, Gate: gate}}, nil
}

type speakerBinding struct {
	current  *transport
	recovery *evpn.ExportRecovery
	reader   *evpn.ReadOnlyType2Importer
	exporter *evpn.EndpointExporter
	observer evpn.EndpointObserver
	control  string
}

func (b *speakerBinding) exporterFor(t *transport) *evpn.EndpointExporter {
	return &evpn.EndpointExporter{Observer: b.observer, Gate: t.gate, Continuity: t.guard,
		Source: t.adapter, Applier: b.recovery, Recovery: b.recovery}
}

func (b *speakerBinding) ensure(ctx context.Context, c config, dial func(context.Context, config) (*transport, error)) error {
	if b.current != nil {
		if err := b.current.guard.Check(ctx); err == nil {
			return nil
		}
		old := b.current
		b.current = nil
		if err := old.Close(); err != nil {
			return fmt.Errorf("close stale speaker transport: %w", err)
		}
	}
	next, err := dial(ctx, c)
	if err != nil {
		return fmt.Errorf("new actual speaker attestation required: %w", err)
	}
	err = b.recovery.Rebind(next.adapter, next.applier, next.guard)
	if err == nil {
		err = b.reader.RebindSource(fixtureRIBSource{next.source, b.control})
	}
	if err != nil {
		return errors.Join(err, next.Close())
	}
	b.current, b.exporter = next, b.exporterFor(next)
	return nil
}

func (b *speakerBinding) Close() error {
	if b.current == nil {
		return nil
	}
	current := b.current
	b.current = nil
	return current.Close()
}
