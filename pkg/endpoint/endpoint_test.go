package endpoint

import (
	"context"
	"errors"
	"testing"
)

type fakeOVN struct {
	ensureErr   error
	deleteErr   error
	ensureCount int
	deleteCount int
}

func (f *fakeOVN) EnsureLogicalSwitchPort(_ context.Context, spec OVNPortSpec) (OVNPortStatus, error) {
	if f.ensureErr != nil {
		return OVNPortStatus{}, f.ensureErr
	}
	f.ensureCount++
	return OVNPortStatus{LogicalSwitch: spec.LogicalSwitch, LogicalSwitchPort: spec.LogicalSwitchPort}, nil
}

func (f *fakeOVN) GetLogicalSwitchPort(_ context.Context, logicalSwitchPort string) (OVNPortStatus, error) {
	return OVNPortStatus{LogicalSwitch: "ls", LogicalSwitchPort: logicalSwitchPort}, nil
}

func (f *fakeOVN) DeleteLogicalSwitchPort(_ context.Context, _ OVNPortSpec) error {
	f.deleteCount++
	return f.deleteErr
}

type fakeAttach struct {
	ensureErr      error
	deleteErr      error
	ensureIdentity AttachmentIdentity
	deleteIdentity AttachmentIdentity
	deleteOutcome  AttachmentDeleteOutcome
}

func (f *fakeAttach) EnsureAttachment(_ context.Context, spec AttachmentSpec) (AttachmentStatus, error) {
	if f.ensureErr != nil {
		return AttachmentStatus{}, f.ensureErr
	}
	return AttachmentStatus{OVSInterfaceName: spec.OVSInterfaceName}, nil
}

func (f *fakeAttach) DeleteAttachment(_ context.Context, _ EndpointID, _ string) error {
	return f.deleteErr
}

func (f *fakeAttach) EnsureAttachmentWithIdentity(_ context.Context, identity AttachmentIdentity, spec AttachmentSpec) (AttachmentStatus, error) {
	f.ensureIdentity = identity
	return f.EnsureAttachment(context.Background(), spec)
}

func (f *fakeAttach) DeleteAttachmentWithIdentity(_ context.Context, identity AttachmentIdentity, _ string) (AttachmentDeleteOutcome, error) {
	f.deleteIdentity = identity
	if f.deleteErr != nil {
		return f.deleteOutcome, f.deleteErr
	}
	if f.deleteOutcome == "" {
		f.deleteOutcome = AttachmentDeleteOutcomeDeleted
	}
	return f.deleteOutcome, nil
}

func baseSpec() EndpointSpec {
	return EndpointSpec{
		ID:                "ep-1",
		NetworkID:         "net-a",
		AttachmentID:      "att-0",
		LogicalSwitch:     "ls-a",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
		MACAddress:        "aa:bb:cc:dd:ee:ff",
		IPAddresses:       []string{"10.0.0.2"},
	}
}

func TestEnsureEndpointIdempotentShape(t *testing.T) {
	m := NewManager(&fakeOVN{}, &fakeAttach{}, true)
	spec := baseSpec()

	s1, err := m.EnsureEndpoint(context.Background(), spec)
	if err != nil {
		t.Fatalf("first ensure failed: %v", err)
	}
	s2, err := m.EnsureEndpoint(context.Background(), spec)
	if err != nil {
		t.Fatalf("second ensure failed: %v", err)
	}
	if s1.LogicalSwitchPort != s2.LogicalSwitchPort {
		t.Fatalf("expected same endpoint identity across ensure calls")
	}
	if s2.OVNPortState != StateEnsured || s2.LocalAttachState != StateEnsured {
		t.Fatalf("unexpected states: ovn=%s attach=%s", s2.OVNPortState, s2.LocalAttachState)
	}
}

func TestEnsureEndpointValidationError(t *testing.T) {
	m := NewManager(&fakeOVN{}, &fakeAttach{}, true)
	spec := baseSpec()
	spec.MACAddress = "invalid"
	if _, err := m.EnsureEndpoint(context.Background(), spec); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestEnsureEndpointOwnershipConflictPropagates(t *testing.T) {
	m := NewManager(&fakeOVN{ensureErr: errors.New("ownership conflict")}, &fakeAttach{}, true)
	if _, err := m.EnsureEndpoint(context.Background(), baseSpec()); err == nil {
		t.Fatalf("expected conflict error")
	}
}

func TestDeleteEndpointMissingNoError(t *testing.T) {
	m := NewManager(&fakeOVN{}, &fakeAttach{}, true)
	if err := m.DeleteEndpoint(context.Background(), baseSpec()); err != nil {
		t.Fatalf("expected idempotent delete, got %v", err)
	}
}

type legacyOnlyAttach struct{}

func (legacyOnlyAttach) EnsureAttachment(_ context.Context, spec AttachmentSpec) (AttachmentStatus, error) {
	return AttachmentStatus{OVSInterfaceName: spec.OVSInterfaceName}, nil
}

func (legacyOnlyAttach) DeleteAttachment(_ context.Context, _ EndpointID, _ string) error {
	return nil
}

func TestEnsureEndpointRequiresIdentityAwareAttachmentManager(t *testing.T) {
	m := NewManager(&fakeOVN{}, legacyOnlyAttach{}, true)
	_, err := m.EnsureEndpoint(context.Background(), baseSpec())
	if err == nil {
		t.Fatalf("expected identity-aware attachment manager requirement")
	}
}

func TestEnsureEndpointCompatibilityWhenLocalAttachDisabled(t *testing.T) {
	m := NewManager(&fakeOVN{}, legacyOnlyAttach{}, false)
	if _, err := m.EnsureEndpoint(context.Background(), baseSpec()); err != nil {
		t.Fatalf("expected compatibility when local attach disabled, got %v", err)
	}
}

func TestIdentityAwareAttachmentGetsFullIdentity(t *testing.T) {
	attach := &fakeAttach{}
	m := NewManager(&fakeOVN{}, attach, true)
	spec := baseSpec()
	spec.Incarnation = "inc-1"
	spec.Workload.UID = "wl-1"
	if _, err := m.EnsureEndpoint(context.Background(), spec); err != nil {
		t.Fatalf("ensure failed: %v", err)
	}
	if err := m.DeleteEndpoint(context.Background(), spec); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if attach.ensureIdentity.EndpointID != string(spec.ID) ||
		attach.ensureIdentity.NetworkID != spec.NetworkID ||
		attach.ensureIdentity.AttachmentID != spec.AttachmentID ||
		attach.ensureIdentity.WorkloadUID != spec.Workload.UID ||
		attach.ensureIdentity.Incarnation != spec.Incarnation {
		t.Fatalf("unexpected ensure identity: %+v", attach.ensureIdentity)
	}
	if attach.deleteIdentity.EndpointID != string(spec.ID) ||
		attach.deleteIdentity.NetworkID != spec.NetworkID ||
		attach.deleteIdentity.AttachmentID != spec.AttachmentID ||
		attach.deleteIdentity.WorkloadUID != spec.Workload.UID ||
		attach.deleteIdentity.Incarnation != spec.Incarnation {
		t.Fatalf("unexpected delete identity: %+v", attach.deleteIdentity)
	}
}

func TestDeleteEndpointStaleAttachmentRejectionPropagates(t *testing.T) {
	attach := &fakeAttach{
		deleteErr:     errors.New("stale operation"),
		deleteOutcome: AttachmentDeleteOutcomeStale,
	}
	m := NewManager(&fakeOVN{}, attach, true)
	if err := m.DeleteEndpoint(context.Background(), baseSpec()); err == nil {
		t.Fatalf("expected stale delete error propagation")
	}
}

func TestEnsureEndpointAttachmentFailureDoesNotCompensateOVNPortYet(t *testing.T) {
	ovn := &fakeOVN{}
	attach := &fakeAttach{ensureErr: errors.New("attachment failed")}
	m := NewManager(ovn, attach, true)
	if _, err := m.EnsureEndpoint(context.Background(), baseSpec()); err == nil {
		t.Fatalf("expected attachment failure")
	}
	if ovn.ensureCount != 1 {
		t.Fatalf("expected OVN ensure to run once, got %d", ovn.ensureCount)
	}
	if ovn.deleteCount != 0 {
		t.Fatalf("expected no automatic OVN compensation delete, got %d", ovn.deleteCount)
	}
}
