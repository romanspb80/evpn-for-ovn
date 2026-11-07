package nodeagent

import (
	"context"
	"fmt"

	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type continuityExportObserver struct {
	server *CNIServer
	source evpn.EndpointObserver
}

func (o continuityExportObserver) DesiredLocalEndpoints(ctx context.Context) ([]evpn.LocalEndpoint, error) {
	o.server.operations.Lock()
	defer o.server.operations.Unlock()
	if o.server.continuity == nil {
		return nil, fmt.Errorf("RECOVERY_BLOCKED: durable CNI continuity is required for recovered exports")
	}
	endpoints, err := o.source.DesiredLocalEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	if err := o.server.validateRecoveredExportEndpointsLocked(ctx, endpoints); err != nil {
		return nil, err
	}
	return endpoints, nil
}

type continuityRouteApplier struct {
	server  *CNIServer
	applier evpn.Type2RouteApplier
}

func (a continuityRouteApplier) Add(ctx context.Context, route evpn.Type2Route) (string, error) {
	a.server.operations.Lock()
	defer a.server.operations.Unlock()
	if a.server.continuity == nil {
		return "", fmt.Errorf("RECOVERY_BLOCKED: CNI continuity unavailable")
	}
	if err := a.server.validateRecoveredExportEndpointsLocked(ctx, []evpn.LocalEndpoint{route.Endpoint}); err != nil {
		return "", err
	}
	return a.applier.Add(ctx, route)
}

func (a continuityRouteApplier) Delete(ctx context.Context, route evpn.OwnedType2Route) error {
	// A deleted Pod does not invalidate an acknowledged UUID withdrawal handle.
	return a.applier.Delete(ctx, route)
}
