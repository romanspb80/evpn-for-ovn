package evpn

import (
	"context"
	"fmt"
)

type legacyRemoteType2RIBSource struct {
	source RemoteRIBSource
}

func (s legacyRemoteType2RIBSource) ReadRemoteType2RIB(ctx context.Context) (RemoteType2Snapshot, error) {
	raw, err := s.source.ReadRemoteRIB(ctx)
	if err != nil {
		return RemoteType2Snapshot{}, err
	}
	if !raw.Complete {
		return RemoteType2Snapshot{Complete: false}, nil
	}
	return decodeGoBGPRemoteSnapshot(raw)
}

// ReadRemoteType2RIB converts the GoBGP wire snapshot into provider-neutral
// observations before the importer sees it.
func (s *GoBGPRemoteRIBSource) ReadRemoteType2RIB(ctx context.Context) (RemoteType2Snapshot, error) {
	raw, err := s.ReadRemoteRIB(ctx)
	if err != nil {
		return RemoteType2Snapshot{}, err
	}
	if !raw.Complete {
		return RemoteType2Snapshot{Complete: false}, nil
	}
	return decodeGoBGPRemoteSnapshot(raw)
}

func decodeGoBGPRemoteSnapshot(raw RemoteRIBSnapshot) (RemoteType2Snapshot, error) {
	if !raw.Complete {
		return RemoteType2Snapshot{}, fmt.Errorf("incomplete GoBGP RIB snapshot")
	}
	result := RemoteType2Snapshot{
		Paths:    make([]RemoteType2PathObservation, 0, len(raw.Paths)),
		Complete: true,
	}
	for _, path := range raw.Paths {
		route, err := DecodeRemoteType2(path)
		result.Paths = append(result.Paths, RemoteType2PathObservation{
			Route: route, DecodeError: err,
		})
	}
	return result, nil
}
