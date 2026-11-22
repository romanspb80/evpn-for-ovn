package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	pb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type cniJournal interface {
	Snapshot() (json.RawMessage, *ownership.Intent, error)
	Begin(string, interface{}) (uint64, error)
	Commit(uint64, interface{}) error
}

// CNIWorkloadAttestor reads independently authoritative workload identity.
// A true deleted result means a definitive Kubernetes NotFound, never a timeout.
type CNIWorkloadAttestor interface {
	Verify(context.Context, endpoint.WorkloadRef, string, string) (deleted bool, err error)
}

type cniReceipt struct {
	SwitchUUID, LSPUUID, InterfaceUUID, PortUUID, BridgeUUID string
	HostIfIndex                                              int
	NetNSDevice, NetNSInode                                  uint64
}

type cniResourceObserver interface {
	Observe(context.Context, *endpointRecord) (cniReceipt, bool, error)
	NetworkAbsent(context.Context, string) error
	DeleteExact(context.Context, cniEndpointACK) error
	DeleteOrphanExact(context.Context, cniEndpointACK) error
	OrphanAbsent(context.Context, cniEndpointACK) (bool, error)
	OrphanHostAbsent(context.Context, cniEndpointACK) error
}

type cniEndpointACK struct {
	Identity   *pb.EndpointIdentity
	Runtime    *pb.CNIRuntimeContext
	Attachment *pb.AttachmentRequest
	Result     *pb.CNIResult
	Spec       endpoint.EndpointSpec
	Receipt    cniReceipt
	State      string
	Admission  *CNIAdmissionEvidence `json:",omitempty"`
	CreatedAt  time.Time             `json:",omitempty"`
	VerifyBy   time.Time             `json:",omitempty"`
	VerifiedAt time.Time             `json:",omitempty"`
	Reason     string                `json:",omitempty"`
}

func (a cniEndpointACK) record() *endpointRecord {
	return &endpointRecord{identity: a.Identity, runtime: a.Runtime, attachment: a.Attachment, result: a.Result, endpointSpec: a.Spec}
}

type cniACKState struct {
	Version   int
	Endpoints map[string]cniEndpointACK
}

type cniContinuity struct {
	nodeName   string
	domain     ownership.Domain
	journal    cniJournal
	workload   CNIWorkloadAttestor
	observer   cniResourceObserver
	state      cniACKState
	recovering bool
	scanAfter  string
	authority  interface {
		Admit(context.Context, *pb.CNIAddRequest) (CNIAdmissionEvidence, error)
	}
}

func decodeCNIState(raw []byte, dst interface{}) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(interface{})) != io.EOF {
		return errors.New("extra continuity JSON value")
	}
	return nil
}

func (s *CNIServer) recoverCNI(ctx context.Context, c *cniContinuity) error {
	return s.initializeCNI(ctx, c, false)
}

func (s *CNIServer) initializeCNI(ctx context.Context, c *cniContinuity, retry bool) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	if c.journal == nil || c.workload == nil || c.observer == nil || s.continuity != nil {
		return fmt.Errorf("%w: incomplete/already enabled CNI continuity", ownership.ErrBlocked)
	}
	raw, pending, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	c.state = cniACKState{Version: 1, Endpoints: map[string]cniEndpointACK{}}
	if string(raw) != "null" {
		c.state = cniACKState{}
		if err := decodeCNIState(raw, &c.state); err != nil {
			return fmt.Errorf("%w: CNI metadata: %v", ownership.ErrBlocked, err)
		}
	}
	if c.state.Version != 1 || c.state.Endpoints == nil {
		return fmt.Errorf("%w: incompatible CNI metadata", ownership.ErrBlocked)
	}
	// An ADD without a durably acknowledged resource UUID cannot be adopted.
	// Orphan DEL intents can be replayed only against the matching active ACK.
	if pending != nil {
		if pending.Operation == "cni-orphan-del" {
			var candidate cniEndpointACK
			if err := decodeCNIState(pending.Payload, &candidate); err != nil {
				return fmt.Errorf("%w: invalid orphan cleanup intent", ownership.ErrBlocked)
			}
			previous, ok := c.state.Endpoints[endpointKey(candidate.Identity)]
			if !ok || previous.State != "active" || !reflect.DeepEqual(candidate, previous) {
				return fmt.Errorf("%w: orphan cleanup intent does not match active ACK", ownership.ErrBlocked)
			}
		} else if pending.Operation != "cni-identity" {
			return fmt.Errorf("%w: unresolved CNI %s sequence %d", ownership.ErrBlocked, pending.Operation, pending.Sequence)
		} else {
			var candidate cniEndpointACK
			if err := decodeCNIState(pending.Payload, &candidate); err != nil {
				return fmt.Errorf("%w: invalid identity checkpoint", ownership.ErrBlocked)
			}
			previous, ok := c.state.Endpoints[endpointKey(candidate.Identity)]
			candidate.State, candidate.Reason = previous.State, previous.Reason
			candidate.VerifiedAt = previous.VerifiedAt
			if !ok || previous.State != "pending_creation" || !reflect.DeepEqual(candidate, previous) {
				return fmt.Errorf("%w: identity checkpoint cannot introduce ownership", ownership.ErrBlocked)
			}
			if err := c.journal.Commit(pending.Sequence, c.state); err != nil {
				return err
			}
		}
	}
	for key, ack := range c.state.Endpoints {
		if err := validateCNIACK(key, ack); err != nil {
			return fmt.Errorf("%w: %v", ownership.ErrBlocked, err)
		}
	}
	s.store.mu.RLock()
	occupied := len(s.store.records) != 0
	s.store.mu.RUnlock()
	if occupied {
		return fmt.Errorf("%w: recovery requires empty endpoint store", ownership.ErrBlocked)
	}
	c.recovering = true
	if !retry {
		if err := s.retryPendingOrphanLocked(ctx, c); err != nil {
			return fmt.Errorf("%w: %v", ownership.ErrBlocked, err)
		}
		if err := s.rescanCNIReceiptsLocked(ctx, c); err != nil {
			for key := range c.state.Endpoints {
				s.store.delete(key)
			}
			return fmt.Errorf("%w: %v", ownership.ErrBlocked, err)
		}
	}
	s.continuity = c
	return nil
}

const cniOrphanHostUnresolved = "orphan_host_unresolved"

// Replay must finish before scanning other ACKs: the journal has one intent slot.
func (s *CNIServer) rescanCNIReceiptsLocked(ctx context.Context, c *cniContinuity) error {
	keys := make([]string, 0, len(c.state.Endpoints))
	for key, ack := range c.state.Endpoints {
		if ack.State == "active" || c.recovering && ack.State == "pending_creation" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > c.scanAfter })
	var scanErr error
	for i := range keys {
		if err := ctx.Err(); err != nil {
			return errors.Join(scanErr, err)
		}
		key := keys[(start+i)%len(keys)]
		c.scanAfter = key
		ack := c.state.Endpoints[key]
		_, restored := s.store.get(key)
		if c.recovering && restored {
			continue
		}
		if ack.State == "active" {
			observe := cniWorkloadRetired
			if !c.recovering {
				observe = cniPodRetired
			}
			retired, err := observe(ctx, c.workload, ack)
			if err != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("observe Pod for endpoint %q: %w", key, err))
				continue
			}
			if retired {
				if err := s.cleanupOrphanLocked(ctx, c, key, ack, 0); err != nil {
					return errors.Join(scanErr, fmt.Errorf("orphan cleanup for %q pending retry: %w", key, err))
				}
				continue
			}
		}
		if c.recovering {
			if err := c.verifyResources(ctx, s, ack); err != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("recover resource identity for %q: %w", key, err))
				continue
			}
			s.store.putIfAbsentOrEqual(key, ack.record())
		}
	}
	if scanErr == nil {
		c.recovering = false
	}
	return scanErr
}

func validateCNIACK(key string, a cniEndpointACK) error {
	if a.Identity == nil || a.Runtime == nil || a.Attachment == nil || a.Result == nil ||
		key != endpointKey(a.Identity) || (a.State != "active" && a.State != "deleted" && a.State != "pending_creation" && a.State != "quarantined" && a.State != cniOrphanHostUnresolved) {
		return errors.New("invalid endpoint ACK metadata")
	}
	if a.State == cniOrphanHostUnresolved && a.Reason == "" {
		return errors.New("unresolved orphan host cleanup has no reason")
	}
	if a.State == "pending_creation" && (a.Admission == nil || a.CreatedAt.IsZero() || !a.VerifyBy.After(a.CreatedAt) ||
		a.VerifyBy.Sub(a.CreatedAt) > 2*time.Minute) {
		return errors.New("pending endpoint has no bounded durable creation authority")
	}
	if a.Admission != nil && (a.Admission.ContainerID != a.Runtime.GetContainerId() || a.Admission.PodUID != a.Identity.GetWorkloadUid() ||
		a.Admission.NetNSDevice == 0 || a.Admission.NetNSInode == 0 ||
		a.Admission.NetNSDevice != a.Receipt.NetNSDevice || a.Admission.NetNSInode != a.Receipt.NetNSInode) {
		return errors.New("durable admission identity differs from endpoint receipt")
	}
	if a.Admission != nil {
		if err := a.Admission.validate(); err != nil {
			return fmt.Errorf("invalid durable creation authority: %w", err)
		}
		if a.State == "active" && (a.VerifiedAt.IsZero() || a.VerifiedAt.Before(a.CreatedAt) || a.VerifiedAt.After(a.VerifyBy)) {
			return errors.New("active endpoint has no bounded activation acknowledgement")
		}
	}
	if _, err := validateIdentity(a.Identity); err != nil {
		return err
	}
	if a.Spec.Workload.Kind != "pod" || a.Spec.Workload.UID == "" || a.Spec.Workload.Name == "" ||
		a.Spec.Workload.Namespace == "" || a.Spec.Workload.NodeName == "" ||
		a.Identity.GetWorkloadUid() != a.Spec.Workload.UID ||
		a.Identity.GetIncarnation() != a.Runtime.GetContainerId() ||
		a.Spec.Incarnation != a.Identity.GetIncarnation() ||
		string(a.Spec.ID) != a.Identity.GetEndpointId() || a.Spec.NetworkID != a.Identity.GetNetworkId() ||
		a.Spec.AttachmentID != a.Identity.GetAttachmentId() ||
		string(a.Spec.ID) != a.Spec.NetworkID+"|"+a.Spec.AttachmentID+"|"+a.Spec.Workload.UID ||
		a.Receipt.SwitchUUID == "" || a.Receipt.LSPUUID == "" || a.Receipt.InterfaceUUID == "" || a.Receipt.PortUUID == "" || a.Receipt.BridgeUUID == "" {
		return errors.New("incomplete or conflicting durable identity/UUID")
	}
	if a.Runtime.GetNetnsPath() == "" || a.Runtime.GetIfName() == "" || a.Attachment.GetStaticMac() == "" || a.Attachment.GetStaticIpCidr() == "" {
		return errors.New("incomplete durable runtime/static configuration")
	}
	expected, err := mapCNIAddRequestToEndpointSpec(&pb.CNIAddRequest{
		Identity: a.Identity, Runtime: a.Runtime, Attachment: a.Attachment,
		K8S: &pb.K8SIdentity{PodUid: a.Spec.Workload.UID, PodName: a.Spec.Workload.Name, PodNamespace: a.Spec.Workload.Namespace, NodeName: a.Spec.Workload.NodeName},
	})
	if err != nil {
		return err
	}
	expected.OVSInterfaceName = hostVethNameForKey(key)
	if normalizeEndpointSpec(expected) != normalizeEndpointSpec(a.Spec) ||
		normalizeResult(a.Result) != normalizeResult(buildResultFromStaticConfig(a.Runtime, a.Attachment)) {
		return errors.New("durable spec/result differ from acknowledged CNI configuration")
	}
	return nil
}

func (c *cniContinuity) ready() error {
	if c.recovering {
		return fmt.Errorf("%w: CNI startup recovery pending", ownership.ErrBlocked)
	}
	_, pending, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	if pending != nil {
		return fmt.Errorf("%w: uncertain CNI %s outcome", ownership.ErrBlocked, pending.Operation)
	}
	return nil
}

func (c *cniContinuity) verifyResources(ctx context.Context, s *CNIServer, ack cniEndpointACK) error {
	receipt, absent, err := c.observer.Observe(ctx, ack.record())
	if err != nil {
		return err
	}
	if absent || receipt != ack.Receipt {
		return errors.New("durable endpoint resource UUID changed or absent")
	}
	_, err = s.network.Check(ctx, endpointKey(ack.Identity), ack.record())
	return err
}

func cniWorkloadRetired(ctx context.Context, workload CNIWorkloadAttestor, ack cniEndpointACK) (bool, error) {
	deleted, err := workload.Verify(ctx, ack.Spec.Workload, ack.Runtime.GetContainerId(), ack.Runtime.GetNetnsPath())
	if err == nil || !errors.Is(err, errCNIIdentityConflict) {
		return deleted, err
	}
	if retired, ok := workload.(interface {
		VerifyRetired(context.Context, endpoint.WorkloadRef) (bool, error)
	}); ok {
		confirmed, retirementErr := retired.VerifyRetired(ctx, ack.Spec.Workload)
		if retirementErr != nil {
			return false, retirementErr
		}
		if confirmed {
			return true, nil
		}
	}
	return deleted, err
}

func cniPodRetired(ctx context.Context, workload CNIWorkloadAttestor, ack cniEndpointACK) (bool, error) {
	if verifier, ok := workload.(interface {
		VerifyRetired(context.Context, endpoint.WorkloadRef) (bool, error)
	}); ok {
		return verifier.VerifyRetired(ctx, ack.Spec.Workload)
	}
	return cniWorkloadRetired(ctx, workload, ack)
}

func (s *CNIServer) cleanupOrphanLocked(ctx context.Context, c *cniContinuity, key string, ack cniEndpointACK, sequence uint64) error {
	if ack.State != "active" || key != endpointKey(ack.Identity) {
		return errors.New("orphan cleanup requires the matching active CNI ACK")
	}
	retired, err := cniPodRetired(ctx, c.workload, ack)
	if err != nil {
		return fmt.Errorf("revalidate retired Pod identity: %w", err)
	}
	if !retired {
		return errors.New("original Pod UID is not authoritatively retired")
	}
	if sequence == 0 {
		fmt.Fprintf(os.Stderr, "CNI orphan candidate endpoint=%q pod_retirement=confirmed\n", key)
		sequence, err = c.journal.Begin("cni-orphan-del", ack)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "CNI orphan intent persisted endpoint=%q sequence=%d\n", key, sequence)
	}
	// Recheck Kubernetes immediately before the receipt-fenced mutations. A new
	// Pod UID is retired relative to this ACK, but can never match its UUIDs.
	retired, err = cniPodRetired(ctx, c.workload, ack)
	if err != nil {
		return fmt.Errorf("revalidate retired Pod before deletion: %w", err)
	}
	if !retired {
		return errors.New("original Pod UID became active before orphan deletion")
	}
	if err := c.observer.DeleteOrphanExact(ctx, ack); err != nil {
		return err
	}
	absent, err := c.observer.OrphanAbsent(ctx, ack)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("orphan OVN/OVS resource absence not verified")
	}
	ack.State = "deleted"
	// RTM_DELLINK does not condition deletion on the observed identity.
	// An absence lookup is evidence only; never attempt orphan link mutation.
	if err := c.observer.OrphanHostAbsent(ctx, ack); err != nil {
		ack.State = cniOrphanHostUnresolved
		ack.Reason = "host veth absence unverified; deletion disabled because link identity cannot be fenced"
	}
	next := cniACKState{Version: c.state.Version, Endpoints: make(map[string]cniEndpointACK, len(c.state.Endpoints))}
	for endpointID, current := range c.state.Endpoints {
		next.Endpoints[endpointID] = current
	}
	next.Endpoints[key] = ack
	if err := c.journal.Commit(sequence, next); err != nil {
		return err
	}
	c.state = next
	s.store.delete(key)
	fmt.Fprintf(os.Stderr, "CNI orphan cleanup endpoint=%q ovs=complete ovn=complete state=%s reason=%q\n", key, ack.State, ack.Reason)
	return nil
}

func (c *cniContinuity) check(ctx context.Context, s *CNIServer, req *pb.CNICheckRequest) error {
	if err := c.ready(); err != nil {
		return err
	}
	ack, ok := c.state.Endpoints[endpointKey(req.GetIdentity())]
	if !ok || ack.State != "active" || normalizeIdentity(ack.Identity) != normalizeIdentity(req.GetIdentity()) {
		return errors.New("no matching durable active endpoint ACK")
	}
	deleted, err := c.workload.Verify(ctx, ack.Spec.Workload, ack.Runtime.GetContainerId(), ack.Runtime.GetNetnsPath())
	if err != nil {
		return err
	}
	if deleted {
		return errors.New("Kubernetes Pod deleted")
	}
	return c.verifyResources(ctx, s, ack)
}

func (c *cniContinuity) add(ctx context.Context, s *CNIServer, req *pb.CNIAddRequest) (*pb.CNIAddResponse, error) {
	fail := func(err error) (*pb.CNIAddResponse, error) {
		return nil, status.Errorf(codes.FailedPrecondition, "CNI continuity: %v", err)
	}
	id, runtime, attach, err := validateAddRequest(req)
	if err != nil {
		return nil, err
	}
	if err := c.ready(); err != nil {
		return fail(err)
	}
	// Multus does not necessarily supply K8S_NODE_NAME. The local trusted scope
	// supplies only the missing claim; direct Kubernetes GET still attests it.
	req = proto.Clone(req).(*pb.CNIAddRequest)
	if req.K8S == nil {
		req.K8S = &pb.K8SIdentity{}
	}
	if strings.TrimSpace(req.K8S.NodeName) == "" {
		req.K8S.NodeName = c.nodeName
	} else if req.K8S.NodeName != c.nodeName {
		return fail(errors.New("CNI node claim differs from trusted local node scope"))
	}
	spec, err := mapCNIAddRequestToEndpointSpec(req)
	if err != nil {
		return fail(err)
	}
	rec := &endpointRecord{identity: cloneIdentity(id), runtime: cloneRuntime(runtime), attachment: cloneAttachment(attach), result: buildResultFromStaticConfig(runtime, attach), endpointSpec: spec}
	rec.endpointSpec.OVSInterfaceName = hostVethNameForKey(endpointKey(id))
	key := endpointKey(id)
	proposed := cniEndpointACK{Identity: rec.identity, Runtime: rec.runtime, Attachment: rec.attachment, Result: rec.result, Spec: rec.endpointSpec, Receipt: cniReceipt{SwitchUUID: "unobserved", LSPUUID: "unobserved", InterfaceUUID: "unobserved", PortUUID: "unobserved", BridgeUUID: "unobserved"}, State: "active"}
	if err := validateCNIACK(key, proposed); err != nil {
		return fail(err)
	}
	var admission *CNIAdmissionEvidence
	pendingCreation := false
	var deleted bool
	if c.authority != nil {
		if existing, ok := c.state.Endpoints[key]; !ok || existing.State == "deleted" || existing.State == "pending_creation" {
			evidence, admitErr := c.authority.Admit(ctx, req)
			if admitErr != nil {
				return fail(admitErr)
			}
			admission = &evidence
			pendingCreation = true
		}
		verifier, ok := c.workload.(interface {
			VerifyPod(context.Context, endpoint.WorkloadRef) (bool, error)
		})
		if !ok {
			return fail(errors.New("creation authority requires independent Pod verifier"))
		}
		deleted, err = verifier.VerifyPod(ctx, spec.Workload)
	} else {
		deleted, err = c.verifyAddWorkload(ctx, spec.Workload, runtime.GetContainerId(), runtime.GetNetnsPath())
	}
	if err != nil {
		return fail(err)
	}
	if deleted {
		return fail(errors.New("Kubernetes Pod deleted; ADD refused"))
	}
	if ack, ok := c.state.Endpoints[key]; ok && ack.State != "deleted" {
		if ack.State == cniOrphanHostUnresolved {
			return fail(errors.New(ack.Reason))
		}
		if ack.State == "quarantined" {
			return fail(errors.New("endpoint quarantined; no ADD adoption"))
		}
		if !equivalentRecord(ack.record(), rec) {
			return nil, status.Error(codes.AlreadyExists, "conflicting durable ADD")
		}
		if ack.State == "active" {
			deleted, err := c.workload.Verify(ctx, ack.Spec.Workload, runtime.GetContainerId(), runtime.GetNetnsPath())
			if err != nil || deleted {
				return fail(fmt.Errorf("duplicate ADD runtime identity unverified: deleted=%v error=%v", deleted, err))
			}
		} else if admission == nil || admission.NetNSDevice != ack.Receipt.NetNSDevice || admission.NetNSInode != ack.Receipt.NetNSInode {
			return fail(errors.New("pending ADD replay lacks matching creation authority"))
		}
		if err := c.verifyResources(ctx, s, ack); err != nil {
			return fail(err)
		}
		s.store.putIfAbsentOrEqual(key, ack.record())
		return &pb.CNIAddResponse{Success: true, Result: cloneResult(ack.Result), Outcome: &pb.AddOutcome{Identity: cloneIdentity(id), IdempotentReplay: true}}, nil
	}
	preflight, absent, err := c.observer.Observe(ctx, rec)
	if err != nil || !absent {
		return fail(fmt.Errorf("fresh ADD requires independently absent resources: absent=%v error=%v", absent, err))
	}
	if previous, ok := c.state.Endpoints[key]; ok && previous.Receipt.SwitchUUID != preflight.SwitchUUID {
		return fail(errors.New("durable network switch UUID changed"))
	}
	if err := c.observer.NetworkAbsent(ctx, key); err != nil {
		return fail(err)
	}
	seq, err := c.journal.Begin("cni-add", cniEndpointACK{Identity: rec.identity, Runtime: rec.runtime, Attachment: rec.attachment, Spec: rec.endpointSpec, Admission: admission})
	if err != nil {
		return fail(err)
	}
	netState, _, err := s.network.Ensure(ctx, key, rec)
	if err != nil {
		return fail(err)
	}
	rec.endpointSpec.OVSInterfaceName = netState.HostIfName
	rec.result = buildResultFromNetworkState(runtime, attach, netState)
	if _, err := s.endpoint.EnsureEndpoint(ctx, rec.endpointSpec); err != nil {
		return fail(err)
	}
	receipt, absent, err := c.observeCreated(ctx, rec)
	if err != nil {
		return fail(err)
	}
	if absent {
		return fail(errors.New("ADD resource observation absent"))
	}
	ack := cniEndpointACK{Identity: rec.identity, Runtime: rec.runtime, Attachment: rec.attachment, Result: rec.result, Spec: rec.endpointSpec, Receipt: receipt, State: "active"}
	if pendingCreation {
		ack.State, ack.Admission = "pending_creation", admission
		ack.CreatedAt = time.Now().UTC()
		ack.VerifyBy = ack.CreatedAt.Add(2 * time.Minute)
		if admission.NetNSDevice != receipt.NetNSDevice || admission.NetNSInode != receipt.NetNSInode {
			return fail(errors.New("creation namespace changed during ADD"))
		}
	}
	if err := validateCNIACK(key, ack); err != nil {
		return fail(err)
	}
	if err := c.verifyResources(ctx, s, ack); err != nil {
		return fail(err)
	}
	if c.authority != nil {
		verifier := c.workload.(interface {
			VerifyPod(context.Context, endpoint.WorkloadRef) (bool, error)
		})
		deleted, err = verifier.VerifyPod(ctx, spec.Workload)
		if err == nil && !deleted && pendingCreation {
			second, checkErr := c.authority.Admit(ctx, req)
			if checkErr != nil || second.NetNSDevice != admission.NetNSDevice || second.NetNSInode != admission.NetNSInode {
				return fail(fmt.Errorf("creation authority changed during ADD: %v", checkErr))
			}
		}
	} else {
		deleted, err = c.verifyAddWorkload(ctx, spec.Workload, runtime.GetContainerId(), runtime.GetNetnsPath())
	}
	if err != nil || deleted {
		return fail(fmt.Errorf("ADD workload no longer verified: deleted=%v error=%v", deleted, err))
	}

	c.state.Endpoints[key] = ack
	if err := c.journal.Commit(seq, c.state); err != nil {
		return fail(err)
	}
	s.store.putIfAbsentOrEqual(key, rec)
	if pendingCreation {
		fmt.Fprintf(os.Stderr, "CNI identity state=pending_creation endpoint=%q export=prohibited\n", key)
	}
	return &pb.CNIAddResponse{Success: true, Result: cloneResult(rec.result), Outcome: &pb.AddOutcome{Identity: cloneIdentity(id)}}, nil
}

func (c *cniContinuity) verifyAddWorkload(ctx context.Context, ref endpoint.WorkloadRef, container, netns string) (bool, error) {
	if verifier, ok := c.workload.(interface {
		VerifyCreating(context.Context, endpoint.WorkloadRef, string, string) (bool, error)
	}); ok {
		return verifier.VerifyCreating(ctx, ref, container, netns)
	}
	return c.workload.Verify(ctx, ref, container, netns)
}

func (c *cniContinuity) del(ctx context.Context, s *CNIServer, req *pb.CNIDelRequest) (*pb.CNIDelResponse, error) {
	id, err := validateIdentity(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pb.CNIDelResponse, error) {
		return nil, status.Errorf(codes.FailedPrecondition, "CNI continuity DEL: %v", err)
	}
	if err := c.ready(); err != nil {
		return fail(err)
	}
	key := endpointKey(id)
	ack, ok := c.state.Endpoints[key]
	outcome := pb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT
	if ok && normalizeIdentity(ack.Identity) != normalizeIdentity(id) {
		outcome = pb.DelOutcome_DEL_OUTCOME_STALE_OPERATION
	} else if ok && ack.State == cniOrphanHostUnresolved {
		return fail(errors.New(ack.Reason))
	} else if ok && (ack.State == "active" || ack.State == "pending_creation" || ack.State == "quarantined") {
		if err := c.verifyResources(ctx, s, ack); err != nil {
			return fail(err)
		}
		seq, err := c.journal.Begin("cni-del", ack)
		if err != nil {
			return fail(err)
		}
		if err := c.observer.DeleteExact(ctx, ack); err != nil {
			return fail(err)
		}
		if exact, ok := s.network.(interface {
			DeleteAcknowledged(context.Context, string, *endpointRecord, int) error
		}); ok {
			if err := exact.DeleteAcknowledged(ctx, key, ack.record(), ack.Receipt.HostIfIndex); err != nil {
				return fail(err)
			}
		} else if err := s.network.Delete(ctx, key, ack.record()); err != nil {
			return fail(err)
		}
		_, absent, err := c.observer.Observe(ctx, ack.record())
		if err != nil {
			return fail(err)
		}
		if !absent {
			return fail(errors.New("DEL resource absence not verified"))
		}
		if err := c.observer.NetworkAbsent(ctx, key); err != nil {
			return fail(err)
		}
		ack.State = "deleted"
		c.state.Endpoints[key] = ack
		if err := c.journal.Commit(seq, c.state); err != nil {
			return fail(err)
		}
		s.store.delete(key)
		outcome = pb.DelOutcome_DEL_OUTCOME_DELETED
	}
	return &pb.CNIDelResponse{Success: true, Identity: cloneIdentity(id), Outcome: outcome}, nil
}
