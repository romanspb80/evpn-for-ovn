package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
)

type memoryCNIJournal struct {
	raw                 json.RawMessage
	pending             *ownership.Intent
	beginErr, commitErr error
	sequence            uint64
}

func (j *memoryCNIJournal) Snapshot() (json.RawMessage, *ownership.Intent, error) {
	return j.raw, j.pending, nil
}
func (j *memoryCNIJournal) Begin(op string, payload interface{}) (uint64, error) {
	if j.beginErr != nil {
		return 0, j.beginErr
	}
	if j.pending != nil {
		return 0, ownership.ErrBlocked
	}
	j.sequence++
	b, _ := json.Marshal(payload)
	j.pending = &ownership.Intent{Sequence: j.sequence, Operation: op, Payload: b}
	return j.sequence, nil
}
func (j *memoryCNIJournal) Commit(seq uint64, state interface{}) error {
	if j.commitErr != nil {
		return j.commitErr
	}
	if j.pending == nil || j.pending.Sequence != seq {
		return ownership.ErrBlocked
	}
	j.raw, _ = json.Marshal(state)
	j.pending = nil
	return nil
}

type memoryCNIWorkload struct {
	err           error
	deleted       bool
	replaced      bool
	wait, entered chan struct{}
	once          sync.Once
	nodeName      string
}

type conflictingActiveCNIWorkload struct{}

func (conflictingActiveCNIWorkload) Verify(context.Context, endpoint.WorkloadRef, string, string) (bool, error) {
	return false, errCNIIdentityConflict
}

func (conflictingActiveCNIWorkload) VerifyRetired(context.Context, endpoint.WorkloadRef) (bool, error) {
	return false, nil
}

func (w *memoryCNIWorkload) Verify(_ context.Context, ref endpoint.WorkloadRef, _, _ string) (bool, error) {
	if w.nodeName != "" && ref.NodeName != w.nodeName {
		return false, errors.New("Pod is not on independently observed node")
	}
	if w.err != nil {
		return false, w.err
	}
	if w.replaced {
		return false, errCNIIdentityConflict
	}
	if w.wait != nil {
		w.once.Do(func() { close(w.entered) })
		<-w.wait
	}
	return w.deleted, nil
}

func (w *memoryCNIWorkload) VerifyRetired(_ context.Context, ref endpoint.WorkloadRef) (bool, error) {
	if w.nodeName != "" && ref.NodeName != w.nodeName {
		return false, errors.New("Pod is not on independently observed node")
	}
	return w.deleted || w.replaced, w.err
}

type memoryCNIObserver struct {
	receipt               cniReceipt
	present               bool
	observeErr, deleteErr error
	observations, deletes int
	hostAbsentErr         error
}

func (o *memoryCNIObserver) Observe(context.Context, *endpointRecord) (cniReceipt, bool, error) {
	o.observations++
	return o.receipt, !o.present, o.observeErr
}
func (o *memoryCNIObserver) NetworkAbsent(context.Context, string) error {
	if o.present {
		return errors.New("network present")
	}
	return nil
}

func (o *memoryCNIObserver) OrphanHostAbsent(context.Context, cniEndpointACK) error {
	return o.hostAbsentErr
}
func (o *memoryCNIObserver) DeleteExact(context.Context, cniEndpointACK) error {
	o.deletes++
	if o.deleteErr != nil {
		return o.deleteErr
	}
	o.present = false
	return nil
}

func (o *memoryCNIObserver) DeleteOrphanExact(_ context.Context, _ cniEndpointACK) error {
	o.deletes++
	if o.deleteErr != nil {
		return o.deleteErr
	}
	o.present = false
	return nil
}

func (o *memoryCNIObserver) OrphanAbsent(context.Context, cniEndpointACK) (bool, error) {
	return !o.present, o.observeErr
}

func (f *fakeNetworkBackend) VerifyAcknowledgedHost(context.Context, *endpointRecord, int) error {
	return nil
}

func (f *fakeNetworkBackend) DeleteOrphanAcknowledged(ctx context.Context, key string, rec *endpointRecord, index int) error {
	if index <= 0 {
		return errors.New("missing acknowledged host index")
	}
	return f.Delete(ctx, key, rec)
}

type continuityManager struct {
	*fakeEndpointManager
	observer *memoryCNIObserver
}

func (m *continuityManager) EnsureEndpoint(ctx context.Context, spec endpoint.EndpointSpec) (endpoint.EndpointStatus, error) {
	out, err := m.fakeEndpointManager.EnsureEndpoint(ctx, spec)
	if err == nil {
		m.observer.present = true
	}
	return out, err
}

func continuityFixture(t *testing.T, active bool) (*CNIServer, *cniContinuity, *memoryCNIJournal, *memoryCNIWorkload, *memoryCNIObserver, *continuityManager, *pb.CNIAddRequest) {
	t.Helper()
	req := sampleAddRequest("net-h22|net1|uid-a", "container-a", "10.1.2.3/24")
	req.K8S = &pb.K8SIdentity{PodUid: "uid-a", PodName: "pod-a", PodNamespace: "ns-a", NodeName: "node-a"}
	spec, err := mapCNIAddRequestToEndpointSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	spec.OVSInterfaceName = hostVethNameForKey(endpointKey(req.Identity))
	rec := &endpointRecord{identity: req.Identity, runtime: req.Runtime, attachment: req.Attachment, endpointSpec: spec, result: buildResultFromStaticConfig(req.Runtime, req.Attachment)}
	o := &memoryCNIObserver{receipt: cniReceipt{SwitchUUID: "switch-uuid", LSPUUID: "lsp-uuid", InterfaceUUID: "iface-uuid", PortUUID: "port-uuid", BridgeUUID: "bridge-uuid", HostIfIndex: 42}, present: active}
	manager := &continuityManager{fakeEndpointManager: newFakeEndpointManager(), observer: o}
	network := &fakeNetworkBackend{state: networkState{HostIfName: spec.OVSInterfaceName, ContainerIfName: req.Runtime.IfName, MACAddress: req.Attachment.StaticMac, StaticIPCIDR: req.Attachment.StaticIpCidr}}
	server := NewCNIServerWithBackends(nil, manager, network)
	j := &memoryCNIJournal{raw: json.RawMessage("null")}
	w := &memoryCNIWorkload{}
	c := &cniContinuity{nodeName: "node-a", journal: j, workload: w, observer: o}
	if active {
		state := cniACKState{Version: 1, Endpoints: map[string]cniEndpointACK{endpointKey(req.Identity): {
			Identity: req.Identity, Runtime: req.Runtime, Attachment: req.Attachment, Result: rec.result,
			Spec: spec, Receipt: o.receipt, State: "active",
		}}}
		j.raw, _ = json.Marshal(state)
	}
	return server, c, j, w, o, manager, req
}

func TestCNIContinuityRestoreVerifiedACKWithoutEnsure(t *testing.T) {
	s, c, _, _, _, mgr, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, present := s.store.get(endpointKey(req.Identity)); !present {
		t.Fatal("verified ACK not restored")
	}
	if mgr.ensureCount != 0 {
		t.Fatal("recovery recreated endpoint")
	}
	response, err := s.Add(context.Background(), req)
	if err != nil || !response.Outcome.IdempotentReplay {
		t.Fatalf("replay: %v %+v", err, response)
	}
	if mgr.ensureCount != 0 {
		t.Fatal("replay mutated verified existing endpoint")
	}
}

func TestCNIContinuityRecoveryFailsClosed(t *testing.T) {
	for _, name := range []string{"pending-add", "pending-del", "corrupt", "missing-state", "uuid-replaced", "unavailable-kube", "unavailable-ovs", "network-unavailable", "identity-conflict"} {
		t.Run(name, func(t *testing.T) {
			s, c, j, w, o, mgr, req := continuityFixture(t, true)
			switch name {
			case "pending-add":
				j.pending = &ownership.Intent{Operation: "cni-add", Sequence: 1}
			case "pending-del":
				j.pending = &ownership.Intent{Operation: "cni-del", Sequence: 1}
			case "corrupt":
				j.raw = []byte(`{"Version":1,"unknown":true}`)
			case "missing-state":
				j.raw = []byte(`{"Version":1}`)
			case "uuid-replaced":
				o.receipt.LSPUUID = "replacement"
			case "unavailable-kube":
				w.err = errors.New("API offline")
			case "unavailable-ovs":
				o.observeErr = errors.New("OVS offline")
			case "network-unavailable":
				s.network.(*fakeNetworkBackend).checkErr = errors.New("netns absent")
			case "identity-conflict":
				var state cniACKState
				json.Unmarshal(j.raw, &state)
				ack := state.Endpoints[endpointKey(req.Identity)]
				ack.Spec.Workload.UID = "replacement-pod"
				state.Endpoints[endpointKey(req.Identity)] = ack
				j.raw, _ = json.Marshal(state)
			}
			if err := s.recoverCNI(context.Background(), c); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if len(s.store.records) != 0 || s.continuity != nil || mgr.ensureCount != 0 {
				t.Fatal("blocked recovery changed authority")
			}
		})
	}
}

func TestCNIContinuityDeletedPodOrphanCleanedWithoutNetNS(t *testing.T) {
	s, c, j, w, o, mgr, req := continuityFixture(t, true)
	w.deleted = true
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(s.store.records) != 0 || mgr.ensureCount != 0 || o.deletes != 1 || o.present {
		t.Fatal("deleted Pod was not receipt-fenced and cleaned up")
	}
	if c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" || j.pending != nil {
		t.Fatal("orphan cleanup was not durably acknowledged")
	}
	if _, err := s.Add(context.Background(), req); err == nil {
		t.Fatal("deleted Pod ADD accepted")
	}
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{{NetworkID: req.Identity.NetworkId, EndpointID: "ovn-netlink/" + o.receipt.LSPUUID}}); err == nil {
		t.Fatal("deleted Pod exported from residual resources")
	}
}

func TestCNIContinuityOrphanCleanupRetriesAfterRestart(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, true)
	w.deleted = true
	o.deleteErr = errors.New("OVN disconnected")
	if err := s.recoverCNI(context.Background(), c); err == nil || j.pending == nil || j.pending.Operation != "cni-orphan-del" {
		t.Fatal("transient cleanup failure did not retain a durable intent")
	}
	o.deleteErr = nil
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatalf("restart retry failed: %v", err)
	}
	if o.deletes != 2 || o.present || j.pending != nil || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatal("restart retry did not finish exact orphan cleanup")
	}
}

func TestCNIContinuityOrphanCleanupRetriesInProcess(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	w.deleted = true
	o.deleteErr = errors.New("OVN temporarily unavailable")
	if err := s.ValidateContinuity(context.Background()); err == nil || j.pending == nil || len(s.store.records) != 1 {
		t.Fatal("incomplete orphan cleanup was not retained and export-fenced")
	}
	o.deleteErr = nil
	if err := s.verifyPendingCNI(context.Background()); err != nil {
		t.Fatalf("in-process orphan retry failed: %v", err)
	}
	if j.pending != nil || len(s.store.records) != 0 || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatal("in-process retry did not durably finish orphan cleanup")
	}
}

func TestCNIContinuityOrphanCleanupFailsClosedOnKubernetesError(t *testing.T) {
	s, c, j, w, o, _, _ := continuityFixture(t, true)
	w.deleted = true
	w.err = errors.New("API offline")
	if err := s.recoverCNI(context.Background(), c); err == nil || o.deletes != 0 || j.pending != nil {
		t.Fatal("unavailable Kubernetes API authorized orphan cleanup")
	}
}

func TestCNIContinuityOrphanCleanupFencesRecreatedPodUID(t *testing.T) {
	s, c, j, w, o, _, req := continuityFixture(t, true)
	w.replaced = true
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if o.deletes != 1 || j.pending != nil || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatal("new same-name Pod UID prevented exact retirement of the old receipt")
	}
}

func TestCNIContinuityWorkloadRetirementPreservesUnconfirmedIdentityConflict(t *testing.T) {
	_, c, _, _, _, _, req := continuityFixture(t, true)
	ack := c.state.Endpoints[endpointKey(req.Identity)]
	retired, err := cniWorkloadRetired(context.Background(), conflictingActiveCNIWorkload{}, ack)
	if retired || !errors.Is(err, errCNIIdentityConflict) {
		t.Fatalf("unconfirmed active identity conflict was suppressed: retired=%v err=%v", retired, err)
	}
}

func TestCNIContinuityADDIntentAndACK(t *testing.T) {
	s, c, j, _, _, mgr, req := continuityFixture(t, false)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	result, err := s.Add(context.Background(), req)
	if err != nil || !result.Success || j.pending != nil {
		t.Fatalf("ADD failed: %v %+v", err, result)
	}
	if mgr.ensureCount != 1 || len(s.store.records) != 1 || c.state.Endpoints[endpointKey(req.Identity)].State != "active" {
		t.Fatal("ADD missing durable ACK")
	}
}

func TestCNIContinuityUncertainADDRemainsPending(t *testing.T) {
	for _, failure := range []string{"begin", "ensure", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, c, j, _, _, mgr, req := continuityFixture(t, false)
			if err := s.recoverCNI(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "begin":
				j.beginErr = errors.New("disk fault")
			case "ensure":
				mgr.ensureErr = errors.New("uncertain OVN transaction")
			case "commit":
				j.commitErr = errors.New("disk fault")
			}
			if _, err := s.Add(context.Background(), req); err == nil {
				t.Fatal("uncertain ADD acknowledged")
			}
			if len(s.store.records) != 0 {
				t.Fatal("unacknowledged ADD became export ownership")
			}
			if failure == "begin" && mgr.ensureCount != 0 {
				t.Fatal("mutation without durable intent")
			}
			if failure != "begin" && j.pending == nil {
				t.Fatal("uncertain intent discarded")
			}
			before := mgr.ensureCount
			if _, err := s.Add(context.Background(), req); err == nil {
				t.Fatal("uncertain replay succeeded")
			}
			if mgr.ensureCount != before {
				t.Fatal("uncertain replay mutated resources")
			}
		})
	}
}

func TestCNIContinuityNoTupleAdoption(t *testing.T) {
	s, c, _, _, o, mgr, req := continuityFixture(t, false)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	o.present = true
	if _, err := s.Add(context.Background(), req); err == nil {
		t.Fatal("foreign pre-existing tuple adopted")
	}
	if _, err := s.Check(context.Background(), &pb.CNICheckRequest{Identity: req.Identity, Runtime: req.Runtime, Attachment: req.Attachment}); err == nil {
		t.Fatal("request tuple recovered ownership")
	}
	out, err := s.Del(context.Background(), &pb.CNIDelRequest{Identity: req.Identity, Runtime: req.Runtime, Attachment: req.Attachment})
	if err != nil || out.Outcome != pb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT || o.deletes != 0 || mgr.ensureCount != 0 {
		t.Fatal("unknown DEL mutated tuple")
	}
}

func TestCNIContinuityDELIntentAndACK(t *testing.T) {
	for _, failure := range []bool{false, true} {
		s, c, j, _, o, _, req := continuityFixture(t, true)
		if err := s.recoverCNI(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if failure {
			o.deleteErr = errors.New("uncertain deletion")
		}
		out, err := s.Del(context.Background(), &pb.CNIDelRequest{Identity: req.Identity})
		if failure {
			if err == nil || j.pending == nil || len(s.store.records) != 1 {
				t.Fatal("uncertain DEL lost ownership evidence")
			}
			if err := s.ValidateContinuity(context.Background()); err == nil {
				t.Fatal("uncertain DEL allowed export")
			}
		} else if err != nil || out.Outcome != pb.DelOutcome_DEL_OUTCOME_DELETED || j.pending != nil ||
			len(s.store.records) != 0 || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
			t.Fatalf("DEL ACK failed: %v %+v", err, out)
		}
	}
}

func TestCNIContinuityPartialOrStaleDELDoesNotMutate(t *testing.T) {
	s, c, _, _, o, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, incarnation := range []string{"", "other-sandbox"} {
		id := cloneIdentity(req.Identity)
		id.Incarnation = incarnation
		out, err := s.Del(context.Background(), &pb.CNIDelRequest{Identity: id})
		if err != nil || out.Outcome != pb.DelOutcome_DEL_OUTCOME_STALE_OPERATION || o.deletes != 0 {
			t.Fatal("stale DEL mutated ACK")
		}
	}
}

func TestCNIContinuityRecoverySerializesWithADD(t *testing.T) {
	s, c, _, w, _, _, req := continuityFixture(t, true)
	w.wait, w.entered = make(chan struct{}), make(chan struct{})
	recovery := make(chan error, 1)
	go func() { recovery <- s.recoverCNI(context.Background(), c) }()
	<-w.entered
	add := make(chan error, 1)
	go func() { _, err := s.Add(context.Background(), req); add <- err }()
	select {
	case err := <-add:
		t.Fatalf("ADD escaped recovery fence: %v", err)
	default:
	}
	close(w.wait)
	if err := <-recovery; err != nil {
		t.Fatal(err)
	}
	if err := <-add; err != nil {
		t.Fatal(err)
	}
}

func TestCNIContinuityOrphanCleanupSerializesWithADD(t *testing.T) {
	s, c, j, w, o, mgr, req := continuityFixture(t, true)
	w.deleted = true
	w.wait, w.entered = make(chan struct{}), make(chan struct{})
	ensureCount := mgr.ensureCount
	recovery := make(chan error, 1)
	go func() { recovery <- s.recoverCNI(context.Background(), c) }()
	<-w.entered
	add := make(chan error, 1)
	go func() { _, err := s.Add(context.Background(), req); add <- err }()
	select {
	case err := <-add:
		t.Fatalf("ADD escaped orphan cleanup fence: %v", err)
	default:
	}
	close(w.wait)
	if err := <-recovery; err != nil {
		t.Fatal(err)
	}
	if err := <-add; err == nil {
		t.Fatal("ADD accepted after authoritative Pod retirement")
	}
	if o.deletes != 1 || j.pending != nil || mgr.ensureCount != ensureCount || c.state.Endpoints[endpointKey(req.Identity)].State != "deleted" {
		t.Fatal("orphan cleanup raced with ADD or failed to durably retire the ACK")
	}
}

func TestNativeCNIObserverPinsOwnershipAndUUIDs(t *testing.T) {
	exporter, source := readyExportObserver(t)
	source.data.Bridges[0].UUID = "bridge-uuid"
	req := sampleAddRequest("net-h22|net1|uid-a", "container-a", "10.1.2.3/24")
	rec, _ := exporter.server.store.get(endpointKey(req.Identity))
	observer := &nativeCNIObserver{source: source, networks: map[string]string{rec.endpointSpec.NetworkID: rec.endpointSpec.LogicalSwitch}}
	receipt, absent, err := observer.Observe(context.Background(), rec)
	if err != nil || absent || receipt.LSPUUID != "lsp-uuid" || receipt.InterfaceUUID != "i-uuid" {
		t.Fatalf("native receipt: %+v %v", receipt, err)
	}
	source.data.Interfaces[0].ExternalIDs = nil
	if _, _, err := observer.Observe(context.Background(), rec); err == nil {
		t.Fatal("foreign native attachment verified")
	}
}

func TestCNIContinuityNativeExportGate(t *testing.T) {
	s, c, _, _, o, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ep := evpn.LocalEndpoint{NetworkID: req.Identity.NetworkId, EndpointID: "ovn-netlink/" + o.receipt.LSPUUID, Incarnation: "sb-pb-uuid", MACAddress: req.Attachment.StaticMac, IPAddresses: []string{"10.1.2.3"}}
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{ep}); err != nil {
		t.Fatal(err)
	}
	macOnly := ep
	macOnly.MACOnly, macOnly.IPAddresses = true, nil
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{macOnly, ep}); err != nil {
		t.Fatalf("verified native MAC-only companion denied: %v", err)
	}
	macOnly.IPAddresses = []string{"10.1.2.3"}
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{macOnly}); err == nil {
		t.Fatal("MAC-only export with IP accepted")
	}
	macOnly.IPAddresses, macOnly.MACAddress = nil, "02:00:00:00:00:99"
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{macOnly}); err == nil {
		t.Fatal("foreign MAC-only export accepted")
	}
	ep.EndpointID = "ovn-netlink/foreign-uuid"
	if err := s.ValidateRecoveredExportEndpoints(context.Background(), []evpn.LocalEndpoint{ep}); err == nil {
		t.Fatal("unacknowledged native LSP adopted")
	}
}

func TestCNIOrphanInventoryRequiresExactReceiptAndOwnership(t *testing.T) {
	s, c, _, _, _, _, req := continuityFixture(t, true)
	if err := s.recoverCNI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ack := c.state.Endpoints[endpointKey(req.Identity)]
	ids := attachment.ExternalIDsForIdentity(attachment.Identity{
		EndpointID: string(ack.Spec.ID), NetworkID: ack.Spec.NetworkID, AttachmentID: ack.Spec.AttachmentID,
		WorkloadUID: ack.Spec.Workload.UID, Incarnation: ack.Spec.Incarnation,
	}, ack.Spec.LogicalSwitchPort, ack.Spec.MACAddress)
	port, ofport := ack.Receipt.PortUUID, 1
	source := &exportMemorySource{data: exportSnapshot{
		Switches: []*nbdb.LogicalSwitch{{UUID: ack.Receipt.SwitchUUID, Name: ack.Spec.LogicalSwitch, Ports: []string{ack.Receipt.LSPUUID}}},
		LSPs: []*nbdb.LogicalSwitchPort{{UUID: ack.Receipt.LSPUUID, Name: ack.Spec.LogicalSwitchPort, ExternalIDs: ids,
			Addresses: []string{ack.Spec.MACAddress + " " + ack.Spec.IPAddresses[0]}}},
		Interfaces: []*vswitch.Interface{{UUID: ack.Receipt.InterfaceUUID, Name: ack.Spec.OVSInterfaceName, ExternalIDs: ids, Ofport: &ofport}},
		Ports:      []*vswitch.Port{{UUID: ack.Receipt.PortUUID, Name: ack.Spec.OVSInterfaceName, Interfaces: []string{ack.Receipt.InterfaceUUID}}},
		Bridges:    []*vswitch.Bridge{{UUID: ack.Receipt.BridgeUUID, Name: "br-int", Ports: []string{port}}},
	}}
	networks := map[string]string{ack.Spec.NetworkID: ack.Spec.LogicalSwitch}
	resources, err := inspectCNIOrphanResources(context.Background(), source, networks, ack)
	if err != nil || resources.lsp == nil || resources.iface == nil || resources.port == nil || !resources.switchMember || !resources.bridgeMember {
		t.Fatalf("valid receipt rejected: resources=%+v err=%v", resources, err)
	}
	for _, mutate := range []struct {
		name string
		fn   func()
		undo func()
	}{
		{"replacement-lsp-uuid", func() { source.data.LSPs[0].UUID = "replacement-lsp" }, func() { source.data.LSPs[0].UUID = ack.Receipt.LSPUUID }},
		{"lsp-owner-mismatch", func() { source.data.LSPs[0].ExternalIDs = nil }, func() { source.data.LSPs[0].ExternalIDs = ids }},
		{"replacement-interface-uuid", func() { source.data.Interfaces[0].UUID = "replacement-interface" }, func() { source.data.Interfaces[0].UUID = ack.Receipt.InterfaceUUID }},
		{"foreign-port-interface", func() { source.data.Ports[0].Interfaces = []string{"foreign-interface"} }, func() { source.data.Ports[0].Interfaces = []string{ack.Receipt.InterfaceUUID} }},
		{"foreign-switch-membership", func() {
			source.data.Switches = append(source.data.Switches, &nbdb.LogicalSwitch{UUID: "other-switch", Name: "other", Ports: []string{ack.Receipt.LSPUUID}})
		}, func() { source.data.Switches = source.data.Switches[:1] }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			mutate.fn()
			defer mutate.undo()
			if _, err := inspectCNIOrphanResources(context.Background(), source, networks, ack); err == nil {
				t.Fatal("mismatched resource was authorized for deletion")
			}
		})
	}
	partial := &exportMemorySource{data: exportSnapshot{
		Switches: source.data.Switches,
		LSPs:     source.data.LSPs,
		Bridges:  []*vswitch.Bridge{{UUID: ack.Receipt.BridgeUUID, Name: "br-int"}},
	}}
	if _, err := inspectCNIOrphanResources(context.Background(), partial, networks, ack); err != nil {
		t.Fatalf("partially cleaned OVS resources were not retryable: %v", err)
	}
	missingReceipt := ack
	missingReceipt.Receipt.LSPUUID = ""
	if _, err := inspectCNIOrphanResources(context.Background(), source, networks, missingReceipt); err == nil {
		t.Fatal("missing LSP receipt authorized orphan cleanup")
	}
}
