package evpn

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// GoBGPType2Adapter must be pinned to one speaker, not a load-balanced service.
// GoBGP has no compare-and-swap AddPath: callers must ensure exclusive local
// writer ownership of each wire route across controllers.
type GoBGPType2Adapter struct {
	Client api.GoBgpServiceClient
	mu     sync.Mutex
}

func (a *GoBGPType2Adapter) Snapshot(ctx context.Context) ([]Type2RIBPath, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("GoBGP client is required")
	}
	stream, err := a.Client.ListPath(ctx, &api.ListPathRequest{
		TableType:        api.TableType_TABLE_TYPE_GLOBAL,
		Family:           &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
		EnableOnlyBinary: true,
	})
	if err != nil {
		return nil, err
	}
	var paths []Type2RIBPath
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return paths, nil
		}
		if err != nil {
			return nil, err
		}
		if response == nil || response.Destination == nil {
			return nil, fmt.Errorf("incomplete GoBGP RIB response")
		}
		for _, p := range response.Destination.Paths {
			if p == nil {
				return nil, fmt.Errorf("nil GoBGP RIB path")
			}
			nlri, err := apiutil.GetNativeNlri(p)
			if err != nil {
				return nil, err
			}
			evpn, ok := nlri.(*bgp.EVPNNLRI)
			if !ok || evpn.RouteType != bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT {
				continue
			}
			attrs, err := apiutil.GetNativePathAttributes(p)
			if err != nil {
				return nil, err
			}
			id := ""
			if len(p.Uuid) != 0 {
				value, err := uuid.FromBytes(p.Uuid)
				if err != nil || value == uuid.Nil {
					return nil, fmt.Errorf("invalid GoBGP RIB UUID")
				}
				id = value.String()
			}
			paths = append(paths, Type2RIBPath{
				PathUUID: id, Local: id != "" || (p.NeighborIp == "" && !p.IsFromExternal),
				NLRI: evpn, PathAttributes: attrs,
			})
		}
	}
}

func (a *GoBGPType2Adapter) Add(ctx context.Context, route Type2Route) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	want, err := BuildType2Route(route.Endpoint)
	if err != nil {
		return "", err
	}
	paths, err := a.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if path.Local && path.NLRI.String() == want.NLRI.String() {
			return "", fmt.Errorf("%w: local wire route already exists", ErrRouteOwnership)
		}
	}
	path, err := apiutil.NewPath(bgp.RF_EVPN, want.NLRI, false, want.PathAttributes, time.Now())
	if err != nil {
		return "", err
	}
	if want.Endpoint.MACOnly {
		// Pinned apiutil marshals an absent netip.Addr as "invalid IP".
		// The supported binary NLRI API preserves the standard zero IP length.
		path.NlriBinary, err = want.NLRI.Serialize()
		if err != nil {
			return "", err
		}
		path.Nlri = nil
	}
	response, err := a.Client.AddPath(ctx, &api.AddPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path,
	})
	if err != nil {
		return "", err
	}
	id, err := uuid.FromBytes(response.GetUuid())
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("%w: AddPath returned no valid UUID", ErrRouteOwnership)
	}
	return id.String(), nil
}

func (a *GoBGPType2Adapter) Delete(ctx context.Context, owned OwnedType2Route) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, err := uuid.Parse(owned.PathUUID)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf("%w: exact UUID required for DeletePath", ErrRouteOwnership)
	}
	paths, err := a.Snapshot(ctx)
	if err != nil {
		return err
	}
	want, err := BuildType2Route(owned.Endpoint)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !path.Local || path.NLRI.String() != want.NLRI.String() {
			continue
		}
		if path.PathUUID != "" && path.PathUUID != id.String() {
			return fmt.Errorf("%w: wire route has another UUID", ErrRouteOwnership)
		}
		if !ribPathMatchesEndpoint(path, owned.Endpoint) {
			return fmt.Errorf("%w: UUID payload changed", ErrRouteOwnership)
		}
		// GoBGP v4.5.0 ListPath omits UUIDs. The trusted AddPath receipt is
		// still an exact deletion handle; never fall back to deleting NLRI.
		_, err = a.Client.DeletePath(ctx, &api.DeletePathRequest{
			TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: id[:],
		})
		return err
	}
	// A complete successful RIB read proves the entire wire route is absent.
	return nil
}
