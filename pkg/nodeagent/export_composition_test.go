package nodeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent/ovnexport"
)

func TestExporterCycleRetryStatusAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"Enabled":false}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reports []ExportStatus
	err := NewCNIServer(nil).RunEndpointExporter(ctx,
		RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"},
		ExportRuntimeOptions{ConfigPath: path, SBEndpoint: "unix:/unused", Interval: time.Millisecond, Timeout: time.Second},
		func(status ExportStatus) {
			reports = append(reports, status)
			switch len(reports) {
			case 1:
				write(`{"Enabled":false,"unknown":true}`)
			case 2:
				write(`{"Enabled":false}`)
			case 3:
				cancel()
			}
		})
	if err != nil || len(reports) != 3 {
		t.Fatalf("cycle errors must retry, cancellation must return nil: reports=%v err=%v", reports, err)
	}
	if reports[0].Error != "" || reports[0].Observations[0].State != "disabled" ||
		!reports[1].Result.OwnershipUnverified || !strings.Contains(reports[1].Error, "unknown field") ||
		reports[2].Error != "" || reports[2].Result.OwnershipUnverified {
		t.Fatalf("status/retry behavior changed: %+v", reports)
	}
}

func TestExporterStartupValidationBeforeReporting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte(`null`), 0600); err != nil {
		t.Fatal(err)
	}
	reports := 0
	err := NewCNIServer(nil).RunEndpointExporter(context.Background(),
		RuntimeOptions{Mode: RuntimeModeRealOVNOVS, NetworkBackend: "linux-veth"},
		ExportRuntimeOptions{ConfigPath: path, SBEndpoint: "unix:/unused", Interval: time.Second, Timeout: time.Second},
		func(ExportStatus) { reports++ })
	if err == nil || !strings.Contains(err.Error(), "JSON object") || reports != 0 {
		t.Fatalf("startup config failure must propagate before cycles: reports=%d err=%v", reports, err)
	}
}

type lockedExportWorkload struct {
	server *CNIServer
	checks int
	t      *testing.T
}

func (w *lockedExportWorkload) Verify(context.Context, endpoint.WorkloadRef, string, string) (bool, error) {
	w.t.Helper()
	if w.server.operations.TryLock() {
		w.server.operations.Unlock()
		w.t.Error("continuity validation ran outside operations mutex")
	}
	w.checks++
	return false, nil
}

type lockedExportApplier struct {
	server    *CNIServer
	workload  *lockedExportWorkload
	t         *testing.T
	deletions int
}

func (a *lockedExportApplier) Add(context.Context, evpn.Type2Route) (string, error) {
	if a.server.operations.TryLock() {
		a.server.operations.Unlock()
		a.t.Error("route mutation ran outside continuity critical section")
	}
	if a.workload.checks == 0 {
		a.t.Error("mutation preceded continuity validation")
	}
	return "exact-uuid", nil
}

func (a *lockedExportApplier) Delete(_ context.Context, route evpn.OwnedType2Route) error {
	if route.PathUUID != "exact-uuid" {
		return errors.New("wrong withdrawal receipt")
	}
	a.deletions++
	return nil
}

func TestExportContinuityValidationAndMutationShareOperationsMutex(t *testing.T) {
	s, c, _, _, o, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	workload := &lockedExportWorkload{server: s, t: t}
	c.workload = workload
	ep := evpn.LocalEndpoint{
		NetworkID: req.Identity.NetworkId, EndpointID: "ovn-netlink/" + o.receipt.LSPUUID,
		MACAddress: req.Attachment.StaticMac, IPAddresses: []string{"10.1.2.3"},
	}
	api := &lockedExportApplier{server: s, workload: workload, t: t}
	observer := continuityExportObserver{s, contractObserverFunc(func(context.Context) ([]evpn.LocalEndpoint, error) {
		if s.operations.TryLock() {
			s.operations.Unlock()
			t.Error("recovered endpoint observation ran outside operations mutex")
		}
		return []evpn.LocalEndpoint{ep}, nil
	})}
	if endpoints, err := observer.DesiredLocalEndpoints(context.Background()); err != nil || len(endpoints) != 1 {
		t.Fatalf("verified observation: endpoints=%v err=%v", endpoints, err)
	}
	workload.checks = 0
	applier := continuityRouteApplier{s, api}
	if uuid, err := applier.Add(context.Background(), evpn.Type2Route{Endpoint: ep}); err != nil || uuid != "exact-uuid" {
		t.Fatalf("verified mutation: uuid=%s err=%v", uuid, err)
	}
	s.operations.Lock()
	s.stopping = true
	s.operations.Unlock()
	if _, err := applier.Add(context.Background(), evpn.Type2Route{Endpoint: ep}); err == nil {
		t.Fatal("mutation after CNI drain accepted")
	}
	if err := applier.Delete(context.Background(), evpn.OwnedType2Route{PathUUID: "exact-uuid"}); err != nil || api.deletions != 1 {
		t.Fatalf("exact withdrawal must not require live workload: %v", err)
	}
}

type authorizationSourceFunc func(context.Context) (ovnexport.OVNEndpointSnapshot, error)

func (f authorizationSourceFunc) AuthorizationSnapshot(ctx context.Context) (ovnexport.OVNEndpointSnapshot, error) {
	return f(ctx)
}

func TestExportBindingObserverAndRecoverySelection(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		cfg := evpn.ExporterConfig{
			Networks: []evpn.EVPNNetworkConfig{
				{NetworkID: "framework", Enabled: true, ExporterSource: "framework"},
				{NetworkID: "disabled", ExporterSource: "framework"},
				{NetworkID: "cni", Enabled: true},
			},
		}
		if recovery {
			cfg.Recovery = &evpn.ExportRecoveryConfig{}
		}
		server := NewCNIServer(nil)
		binding := &cniExportBinding{server: server}
		observer, native, err := binding.buildObserver(cfg, liveExportState{})
		if err != nil || native != nil {
			t.Fatalf("non-native CNI composition: native=%v err=%v", native, err)
		}
		if recovery {
			wrapped, ok := observer.(continuityExportObserver)
			if !ok || wrapped.server != server {
				t.Fatal("durable CNI observer lost server continuity wrapper")
			}
			observer = wrapped.source
		}
		selected, ok := observer.(selectedEndpointObserver)
		if !ok || selected.cni != binding.observer || selected.native != nil || selected.framework == nil {
			t.Fatalf("wrong CNI/Framework observer selection: %#v", observer)
		}
		framework := selected.framework.(*ovnexport.FrameworkEndpointObserver)
		if len(framework.Networks) != 1 || framework.Networks["framework"].NetworkID != "framework" {
			t.Fatalf("disabled or CNI mapping entered Framework observer: %v", framework.Networks)
		}
		api := &contractRouteAPI{}
		wrapped, ok := binding.wrapRecoveryApplier(api).(continuityRouteApplier)
		if !ok || wrapped.server != server || wrapped.applier != api {
			t.Fatal("CNI recovery applier lost atomic continuity guard")
		}
		binding.observer.Observations = []ExportObservation{{State: "retained"}}
		binding.beginCycle()
		if binding.observations()[0].State != "retained" {
			t.Fatal("CNI status reset semantics changed")
		}
	}
}

type compositionFenceStore struct {
	standaloneExportTestStore
	persistErr error
	writes     int
}

func (s *compositionFenceStore) Persist(data []byte) error {
	s.writes++
	if s.persistErr != nil {
		return s.persistErr
	}
	return s.standaloneExportTestStore.Persist(data)
}

func TestStandaloneBindingPreparationBeforePublication(t *testing.T) {
	config, _, _, _, _ := standaloneStartupFixture(t)
	cfg, err := LoadExporterConfig(config.ExporterConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	store := &compositionFenceStore{persistErr: errors.New("durable acceptance failed")}
	fencer, err := endpointauth.OpenFencer(store, config.InstallationUID, config.NodeUID)
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	binding := &standaloneExportBinding{config: config, fencer: fencer}
	invalid := cfg
	invalid.DryRun = true
	if err := binding.prepare(invalid); err == nil || !strings.Contains(err.Error(), "live process continuity") || store.writes != 0 {
		t.Fatalf("export validation must precede policy read and acceptance: %v", err)
	}
	policy := endpointauth.Document{
		Version: 1, Revision: 1, InstallationUID: config.InstallationUID, NodeUID: config.NodeUID,
		LocalChassis: config.LocalChassis, Endpoints: []endpointauth.EndpointRecord{},
	}
	writeStandaloneStartupJSON(t, config.AuthorizationPolicyPath, policy)
	if err := binding.prepare(cfg); err == nil || !strings.Contains(err.Error(), "durable acceptance failed") ||
		binding.accepted != nil || store.writes != 1 {
		t.Fatalf("failed durable acceptance published authority: accepted=%v err=%v", binding.accepted, err)
	}
	store.persistErr = nil
	if err := binding.prepare(cfg); err == nil {
		t.Fatal("failed persistence did not latch until reopen")
	}
	if err := fencer.Close(); err != nil {
		t.Fatal(err)
	}
	fencer, err = endpointauth.OpenFencer(store, config.InstallationUID, config.NodeUID)
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	binding.fencer = fencer
	if err := binding.prepare(cfg); err != nil {
		t.Fatal(err)
	}
	if binding.accepted == nil || !binding.accepted.IsCurrent() || len(store.data) == 0 {
		t.Fatal("cycle preparation did not durably accept policy")
	}
	observer, native, err := binding.buildObserver(cfg, liveExportState{})
	if err != nil || native != nil || observer != binding.observer ||
		binding.observer.Accepted != binding.accepted {
		t.Fatalf("standalone observer selection lost accepted snapshot: %v", err)
	}
	api := &contractRouteAPI{}
	if binding.wrapRecoveryApplier(api) != api {
		t.Fatal("standalone inherited CNI continuity applier")
	}
	binding.observer.Observations = []ExportObservation{{State: "old-cycle"}}
	binding.beginCycle()
	if len(binding.observations()) != 0 {
		t.Fatal("standalone observations survived cycle reset")
	}
	policy.Revision++
	writeStandaloneStartupJSON(t, config.AuthorizationPolicyPath, policy)
	old := binding.observer.Accepted
	if err := binding.prepare(cfg); err != nil {
		t.Fatal(err)
	}
	if old.IsCurrent() {
		t.Fatal("prior snapshot remained current after durable policy update")
	}
	binding.update(cfg)
	if binding.observer.Accepted != binding.accepted || !binding.observer.Accepted.IsCurrent() {
		t.Fatal("observer did not receive current accepted policy")
	}
}
func TestStandaloneAcceptedSnapshotInvalidationDuringObservation(t *testing.T) {
	policy, local, networks, observed := standaloneExportAuthorizationFixture(t)
	fencer, err := endpointauth.OpenFencer(&standaloneExportTestStore{}, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	defer fencer.Close()
	accepted, err := fencer.Accept(policy)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	observer := standaloneAuthorizationObserver{
		Local: local, Networks: networks, Accepted: accepted,
		Source: authorizationSourceFunc(func(context.Context) (ovnexport.OVNEndpointSnapshot, error) {
			reads++
			next, err := endpointauth.NewSnapshot(endpointauth.Document{
				Version: 1, Revision: 13, InstallationUID: policy.InstallationUID(),
				NodeUID: policy.NodeUID(), LocalChassis: policy.LocalChassis(),
				Endpoints: []endpointauth.EndpointRecord{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fencer.Accept(next); err != nil {
				t.Fatal(err)
			}
			return observed, nil
		}),
	}
	if endpoints, err := observer.DesiredLocalEndpoints(context.Background()); err == nil ||
		endpoints != nil || !strings.Contains(err.Error(), "changed during OVN observation") {
		t.Fatalf("stale policy published endpoints=%v err=%v", endpoints, err)
	}
	if _, err := observer.DesiredLocalEndpoints(context.Background()); err == nil || reads != 1 {
		t.Fatalf("stale snapshot reached source again: reads=%d err=%v", reads, err)
	}
}
