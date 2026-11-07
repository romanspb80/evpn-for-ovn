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
	nodeName  string
	journal   cniJournal
	workload  CNIWorkloadAttestor
	observer  cniResourceObserver
	state     cniACKState
	authority interface {
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
	// Partial DEL is retained for operator reconciliation, never inferred success.
	if pending != nil {
		if pending.Operation != "cni-identity" {
			return fmt.Errorf("%w: unresolved CNI %s sequence %d", ownership.ErrBlocked, pending.Operation, pending.Sequence)
		}
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
	restored := map[string]*endpointRecord{}
	for key, ack := range c.state.Endpoints {
		if err := validateCNIACK(key, ack); err != nil {
			return fmt.Errorf("%w: %v", ownership.ErrBlocked, err)
		}
		if ack.State == "deleted" || ack.State == "quarantined" {
			continue
		}
		if ack.State == "pending_creation" {
			if err := c.verifyResources(ctx, s, ack); err != nil {
				return fmt.Errorf("%w: pending endpoint resource identity: %v", ownership.ErrBlocked, err)
			}
			restored[key] = cloneRecord(ack.record())
			continue
		}
		deleted, err := c.workload.Verify(ctx, ack.Spec.Workload, ack.Runtime.GetContainerId(), ack.Runtime.GetNetnsPath())
		if err != nil {
			return fmt.Errorf("%w: workload observation: %v", ownership.ErrBlocked, err)
		}
		if deleted {
			continue
		}
		if err := c.verifyResources(ctx, s, ack); err != nil {
			return fmt.Errorf("%w: %v", ownership.ErrBlocked, err)
		}
		restored[key] = cloneRecord(ack.record())
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if len(s.store.records) != 0 {
		return fmt.Errorf("%w: recovery requires empty endpoint store", ownership.ErrBlocked)
	}
	s.store.records = restored
	s.continuity = c
	return nil
}

func validateCNIACK(key string, a cniEndpointACK) error {
	if a.Identity == nil || a.Runtime == nil || a.Attachment == nil || a.Result == nil ||
		key != endpointKey(a.Identity) || (a.State != "active" && a.State != "deleted" && a.State != "pending_creation" && a.State != "quarantined") {
		return errors.New("invalid endpoint ACK metadata")
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
