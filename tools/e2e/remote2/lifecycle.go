package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

type endpointRequest struct {
	Generation  uint64
	Incarnation string
	Retired     bool
}

type lifecycleState struct {
	Generation   uint64
	Incarnation  string
	Phase        string
	Used         []string
	Bootstrapped bool
}

type lifecycle struct {
	journal *ownership.Journal
	state   lifecycleState
}

func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document")
	}
	return nil
}

func protectedRead(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 {
		return nil, fmt.Errorf("unsafe fixture document: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err == nil && len(data) > 1<<20 {
		err = fmt.Errorf("oversized fixture document: %s", path)
	}
	return data, err
}

func openLifecycle(c config, domain ownership.Domain) (*lifecycle, error) {
	path := filepath.Join(c.State, "lifecycle")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		for _, name := range []string{"export", "import"} {
			if _, err := os.Lstat(filepath.Join(c.State, name)); !os.IsNotExist(err) {
				return nil, fmt.Errorf("original lifecycle journal missing or pre-contract state present: %s", name)
			}
		}
	}
	j, err := ownership.Open(path, domain, true)
	if err != nil {
		return nil, err
	}
	l := &lifecycle{journal: j}
	if err := l.restore(c.Endpoint.Incarnation); err != nil {
		return nil, errors.Join(err, j.Close())
	}
	if l.state.Bootstrapped {
		for _, name := range []string{"export/journal.json", "import/remote-netlink-owner", "import/receipts-v1/journal.json"} {
			if _, err := os.Lstat(filepath.Join(c.State, name)); err != nil {
				return nil, errors.Join(fmt.Errorf("original recovery metadata required: %w", err), j.Close())
			}
		}
	}
	return l, nil
}

func (l *lifecycle) restore(initial string) error {
	data, pending, err := l.journal.Snapshot()
	if err != nil {
		return err
	}
	if pending != nil {
		return fmt.Errorf("%w: unresolved lifecycle intent %s", ownership.ErrBlocked, pending.Operation)
	}
	if string(data) == "null" {
		if id, err := uuid.Parse(initial); err != nil || id == uuid.Nil || id.String() != initial {
			return fmt.Errorf("initial endpoint incarnation must be a canonical nonzero UUID")
		}
		return l.persist(lifecycleState{Generation: 1, Incarnation: initial, Phase: "active", Used: []string{initial}})
	}
	if err := decode(data, &l.state); err != nil {
		return err
	}
	s := l.state
	if s.Generation == 0 || uint64(len(s.Used)) != s.Generation || s.Used[0] != initial ||
		s.Used[len(s.Used)-1] != s.Incarnation ||
		(s.Phase != "active" && s.Phase != "retiring" && s.Phase != "retired") {
		return fmt.Errorf("invalid lifecycle ledger")
	}
	seen := map[string]bool{}
	for _, value := range s.Used {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value || seen[value] {
			return fmt.Errorf("invalid or reused lifecycle incarnation")
		}
		seen[value] = true
	}
	return nil
}

func (l *lifecycle) persist(next lifecycleState) error {
	seq, err := l.journal.Begin("endpoint-lifecycle", next)
	if err == nil {
		err = l.journal.Commit(seq, next)
	}
	if err == nil {
		l.state = next
	}
	return err
}

func (l *lifecycle) accept(request endpointRequest) error {
	s := l.state
	if request.Generation < s.Generation || request.Generation == 0 {
		return fmt.Errorf("stale endpoint generation")
	}
	if request.Generation == s.Generation {
		if request.Incarnation != s.Incarnation {
			return fmt.Errorf("endpoint incarnation changed within generation")
		}
		if request.Retired && s.Phase == "active" {
			s.Phase = "retiring"
			return l.persist(s)
		}
		// Retirement is irreversible, even when the request is removed/replayed.
		return nil
	}
	if s.Generation == ^uint64(0) || request.Generation != s.Generation+1 || s.Phase != "retired" || request.Retired {
		return fmt.Errorf("recreation requires the next generation after acknowledged retirement")
	}
	id, err := uuid.Parse(request.Incarnation)
	if err != nil || id == uuid.Nil || id.String() != request.Incarnation {
		return fmt.Errorf("new endpoint incarnation must be a canonical nonzero UUID")
	}
	for _, used := range s.Used {
		if used == request.Incarnation {
			return fmt.Errorf("retired endpoint incarnation cannot reappear")
		}
	}
	s.Generation, s.Incarnation, s.Phase = request.Generation, request.Incarnation, "active"
	s.Used = append(append([]string(nil), s.Used...), request.Incarnation)
	return l.persist(s)
}

func (l *lifecycle) acknowledge(result evpn.ExportResult, reconcileErr error) error {
	if reconcileErr == nil && l.state.Phase == "retiring" {
		if result.OwnershipUnverified || len(result.Applied) != 0 || len(result.Desired) != 0 {
			return fmt.Errorf("retirement lacks verified export drainage")
		}
		next := l.state
		next.Phase = "retired"
		return l.persist(next)
	}
	return nil
}

func (l *lifecycle) bootstrap() error {
	if l.state.Bootstrapped {
		return nil
	}
	next := l.state
	next.Bootstrapped = true
	return l.persist(next)
}

func (l *lifecycle) desired(ctx context.Context, c config, observe func(context.Context, config) ([]evpn.LocalEndpoint, error)) ([]evpn.LocalEndpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := protectedRead(filepath.Join(c.State, "endpoint.json"))
	if err == nil {
		var request endpointRequest
		if err := decode(data, &request); err != nil {
			return nil, err
		}
		if err := l.accept(request); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	data, err = protectedRead(filepath.Join(c.State, "retire"))
	if err == nil {
		if string(data) != c.Endpoint.EndpointID+"/"+l.state.Incarnation+"\n" {
			return nil, fmt.Errorf("retirement identity differs")
		}
		if err := l.accept(endpointRequest{l.state.Generation, l.state.Incarnation, true}); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if l.state.Phase != "active" {
		return nil, nil
	}
	c.Endpoint.Incarnation = l.state.Incarnation
	return observe(ctx, c)
}

func immutableConfig(path string, original []byte) error {
	data, err := protectedRead(path)
	if err == nil && !reflect.DeepEqual(data, original) {
		err = fmt.Errorf("immutable recovery configuration changed")
	}
	return err
}
