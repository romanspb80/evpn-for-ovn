package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeRealFactory struct {
	buildErr    error
	buildCalled bool
}

func (f *fakeRealFactory) Build(_ context.Context, store *Store, _ RuntimeOptions) (*CNIServer, func(), error) {
	f.buildCalled = true
	if f.buildErr != nil {
		return nil, nil, f.buildErr
	}
	return NewCNIServer(store), nil, nil
}

func TestBuildServerForOptionsInMemoryDefault(t *testing.T) {
	srv, cleanup, err := BuildServerForOptions(context.Background(), NewStore(), RuntimeOptions{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv == nil {
		t.Fatalf("expected server")
	}
	if cleanup != nil {
		t.Fatalf("in-memory mode should not require cleanup function")
	}
}

func TestBuildServerForOptionsRealModeRequiresLinuxVeth(t *testing.T) {
	_, _, err := BuildServerForOptions(context.Background(), NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "in-memory",
	}, &fakeRealFactory{})
	if err == nil || !strings.Contains(err.Error(), "--network-backend=linux-veth") {
		t.Fatalf("expected linux-veth requirement error, got %v", err)
	}
}

func TestBuildServerForOptionsRealModeNoSilentFallback(t *testing.T) {
	factory := &fakeRealFactory{buildErr: fmt.Errorf("dial failed")}
	_, _, err := BuildServerForOptions(context.Background(), NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth",
		OVNNBEndpoint:  "unix:/tmp/nb.sock",
		OVSDBEndpoint:  "unix:/tmp/ovs.sock",
	}, factory)
	if err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("expected real factory error without fallback, got %v", err)
	}
	if !factory.buildCalled {
		t.Fatalf("expected real factory build to be called")
	}
}

func TestBuildServerForOptionsRealModeFactorySuccess(t *testing.T) {
	factory := &fakeRealFactory{}
	srv, _, err := BuildServerForOptions(context.Background(), NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth",
		OVNNBEndpoint:  "unix:/tmp/nb.sock",
		OVSDBEndpoint:  "unix:/tmp/ovs.sock",
	}, factory)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv == nil {
		t.Fatalf("expected server")
	}
	if !factory.buildCalled {
		t.Fatalf("expected factory build to be called")
	}
}

func TestDefaultRealServerFactoryInvalidOVNEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _, err := (defaultRealServerFactory{}).Build(ctx, NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth",
		OVNNBEndpoint:  "unix:/tmp/definitely-missing-ovn-nb.sock",
		OVSDBEndpoint:  "unix:/tmp/definitely-missing-ovsdb.sock",
	})
	if err == nil {
		t.Fatalf("expected ovn endpoint connection error")
	}
	if !strings.Contains(err.Error(), "ovn nb endpoint") {
		t.Fatalf("expected ovn endpoint context in error, got %v", err)
	}
}

func TestDefaultRealServerFactoryRequiresOVSDBEndpoint(t *testing.T) {
	_, _, err := (defaultRealServerFactory{}).Build(context.Background(), NewStore(), RuntimeOptions{
		Mode:           RuntimeModeRealOVNOVS,
		NetworkBackend: "linux-veth",
		OVNNBEndpoint:  "unix:/tmp/ovn-nb.sock",
		OVSDBEndpoint:  "",
	})
	if err == nil {
		t.Fatalf("expected ovsdb endpoint validation error")
	}
	if !strings.Contains(err.Error(), "ovsdb endpoint is required") {
		t.Fatalf("expected ovsdb endpoint context in error, got %v", err)
	}
}
