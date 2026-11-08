package ovnexport

import (
	"errors"
	"testing"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

type authorizationTestFenceStore struct {
	data       []byte
	persistErr error
}

func (s *authorizationTestFenceStore) Load() ([]byte, error) {
	if s.data == nil {
		return nil, endpointauth.ErrFenceStateNotFound
	}
	return append([]byte(nil), s.data...), nil
}

func (s *authorizationTestFenceStore) Persist(data []byte) error {
	if s.persistErr != nil {
		return s.persistErr
	}
	s.data = append([]byte(nil), data...)
	return nil
}

func (*authorizationTestFenceStore) Close() error { return nil }

const (
	testLSPUUID     = "00000000-0000-4000-8000-000000000123"
	testBindingUUID = "00000000-0000-4000-8000-000000000456"
)

func authorizationFixture(t *testing.T) (*endpointauth.Snapshot, TrustedEndpointIdentity, map[string]evpn.EVPNNetworkConfig, OVNEndpointSnapshot) {
	t.Helper()
	document := endpointauth.Document{
		Version: 1, Revision: 12, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []endpointauth.EndpointRecord{{
			NetworkID: "net-a", EndpointID: "endpoint-1", AttachmentID: "attachment-1",
			AttachmentGeneration: 4, LogicalSwitchPort: "lsp-1", LogicalSwitchPortUUID: testLSPUUID,
			MAC: "02:00:00:00:00:01", IP: "192.0.2.10",
		}},
	}
	policy, err := endpointauth.NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	local := TrustedEndpointIdentity{
		InstallationUID: "installation-1", NodeUID: "node-1",
		NodeName: "node-a", LocalChassis: "chassis-1",
	}
	networks := map[string]evpn.EVPNNetworkConfig{
		"net-a": {
			NetworkID: "net-a", LogicalSwitch: "ls-net-a", Enabled: true,
			VNI: 100, RouteDistinguisher: "65000:100", RouteTargets: []string{"65000:100"},
			LocalVTEP: "192.0.2.1",
		},
	}
	observed := OVNEndpointSnapshot{
		NBComplete: true,
		SBComplete: true,
		LSPs: []OVNLogicalSwitchPort{{
			UUID: testLSPUUID, Name: "lsp-1", LogicalSwitch: "ls-net-a",
			Addresses: []string{"02:00:00:00:00:01 192.0.2.10"},
			ExternalIDs: map[string]string{
				"evpn.framework/network-id":    "net-a",
				"evpn.framework/endpoint-id":   "endpoint-1",
				"evpn.framework/attachment-id": "attachment-1",
				externalIDAttachmentGeneration: "4",
			},
		}},
		Bindings: []OVNPortBinding{{
			UUID: testBindingUUID, LogicalPort: "lsp-1", Chassis: "chassis-1", Up: boolPointer(true),
		}},
	}
	return policy, local, networks, observed
}

func intersectFixture(
	t *testing.T,
	policy *endpointauth.Snapshot,
	local TrustedEndpointIdentity,
	networks map[string]evpn.EVPNNetworkConfig,
	observed OVNEndpointSnapshot,
) AuthorizationIntersection {
	t.Helper()
	if policy == nil {
		return IntersectEndpointAuthorization(nil, local, networks, observed)
	}
	store := &authorizationTestFenceStore{}
	fencer, err := endpointauth.OpenFencer(store, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		return IntersectEndpointAuthorization(nil, local, networks, observed)
	}
	t.Cleanup(func() {
		if err := fencer.Close(); err != nil {
			t.Error(err)
		}
	})
	accepted, err := fencer.Accept(policy)
	if err != nil {
		return IntersectEndpointAuthorization(nil, local, networks, observed)
	}
	return IntersectEndpointAuthorization(accepted, local, networks, observed)
}

func intersectLifecycleFixture(
	t *testing.T,
	policies []*endpointauth.Snapshot,
	local TrustedEndpointIdentity,
	networks map[string]evpn.EVPNNetworkConfig,
	observed OVNEndpointSnapshot,
) AuthorizationIntersection {
	t.Helper()
	if len(policies) == 0 {
		t.Fatal("at least one accepted policy is required")
	}
	first := policies[0]
	fencer, err := endpointauth.OpenFencer(&authorizationTestFenceStore{}, first.InstallationUID(), first.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fencer.Close(); err != nil {
			t.Error(err)
		}
	})
	var accepted *endpointauth.AcceptedSnapshot
	for _, policy := range policies {
		accepted, err = fencer.Accept(policy)
		if err != nil {
			t.Fatal(err)
		}
	}
	return IntersectEndpointAuthorization(accepted, local, networks, observed)
}

func lifecyclePolicy(
	t *testing.T,
	revision uint64,
	endpoints []endpointauth.EndpointRecord,
	tombstones []endpointauth.EndpointTombstone,
) *endpointauth.Snapshot {
	t.Helper()
	if endpoints == nil {
		endpoints = []endpointauth.EndpointRecord{}
	}
	if tombstones == nil {
		tombstones = []endpointauth.EndpointTombstone{}
	}
	policy, err := endpointauth.NewSnapshot(endpointauth.Document{
		Version: endpointauth.LifecycleVersion, Revision: revision,
		InstallationUID: "installation-1", NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: endpoints, Tombstones: tombstones,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func decisionFor(t *testing.T, result AuthorizationIntersection, networkID, endpointID string) AuthorizationDecision {
	t.Helper()
	for _, decision := range result.Decisions {
		if decision.NetworkID == networkID && decision.EndpointID == endpointID {
			return decision
		}
	}
	t.Fatalf("no decision for %s/%s in %#v", networkID, endpointID, result.Decisions)
	return AuthorizationDecision{}
}

func expectState(t *testing.T, result AuthorizationIntersection, state AuthorizationState) AuthorizationDecision {
	t.Helper()
	decision := decisionFor(t, result, "net-a", "endpoint-1")
	if decision.State != state {
		t.Fatalf("state=%s, want %s: %s", decision.State, state, decision.Reason)
	}
	return decision
}

func TestAuthorizationIntersectionExactMatchIsEligible(t *testing.T) {
	policy, local, networks, observed := authorizationFixture(t)
	observed.LSPs[0].Addresses[0] = "02:00:00:00:00:01 192.0.2.10/24"
	result := intersectFixture(t, policy, local, networks, observed)
	decision := expectState(t, result, AuthorizationEligible)
	if decision.Endpoint == nil || !decision.Endpoint.Advertise ||
		decision.Endpoint.Incarnation != "attachment-generation:4" || decision.Endpoint.Chassis != local.LocalChassis {
		t.Fatalf("eligible decision did not produce the exact Type-2 endpoint: %#v", decision)
	}
	if result.PolicyVersion != policy.PolicyVersion() {
		t.Fatalf("result policy version=%#v, want %#v", result.PolicyVersion, policy.PolicyVersion())
	}
}

func TestAuthorizationIntersectionRejectsUnlistedEndpoint(t *testing.T) {
	_, local, networks, observed := authorizationFixture(t)
	document := endpointauth.Document{
		Version: 1, Revision: 13, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1", Endpoints: []endpointauth.EndpointRecord{},
	}
	revoked, err := endpointauth.NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	result := intersectFixture(t, revoked, local, networks, observed)
	decision := decisionFor(t, result, "net-a", "endpoint-1")
	if decision.State != AuthorizationIneligible || decision.Endpoint != nil {
		t.Fatalf("revoked endpoint state=%s, want ineligible: %#v", decision.State, decision)
	}
}

func TestAuthorizationIntersectionClassifiesAuthoritativeMismatchesIneligible(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*endpointauth.Snapshot, *TrustedEndpointIdentity, *map[string]evpn.EVPNNetworkConfig, *OVNEndpointSnapshot)
	}{
		{
			name: "network identity mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].ExternalIDs["evpn.framework/network-id"] = "net-other"
			},
		},
		{
			name: "endpoint identity mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].ExternalIDs["evpn.framework/endpoint-id"] = "endpoint-replaced"
			},
		},
		{
			name: "attachment identity mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].ExternalIDs["evpn.framework/attachment-id"] = "attachment-replaced"
			},
		},
		{
			name: "attachment generation mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].ExternalIDs[externalIDAttachmentGeneration] = "5"
			},
		},
		{
			name: "same LSP name but replaced UUID",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].UUID = "00000000-0000-4000-8000-000000000999"
			},
		},
		{
			name: "authoritative endpoint replacement",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].UUID = "00000000-0000-4000-8000-000000000999"
				observed.LSPs[0].ExternalIDs["evpn.framework/endpoint-id"] = "endpoint-replacement"
				observed.LSPs[0].ExternalIDs["evpn.framework/attachment-id"] = "attachment-replacement"
				observed.LSPs[0].ExternalIDs[externalIDAttachmentGeneration] = "5"
			},
		},
		{
			name: "wrong switch membership",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].LogicalSwitch = "ls-other"
			},
		},
		{
			name: "MAC mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].Addresses[0] = "02:00:00:00:00:02 192.0.2.10"
			},
		},
		{
			name: "IP mismatch",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].Addresses[0] = "02:00:00:00:00:01 192.0.2.11"
			},
		},
		{
			name: "authoritative chassis migration",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.Bindings[0].Chassis = "chassis-2"
			},
		},
		{
			name: "confirmed down",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.Bindings[0].Up = boolPointer(false)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, local, networks, observed := authorizationFixture(t)
			test.mutate(policy, &local, &networks, &observed)
			expectState(t, intersectFixture(t, policy, local, networks, observed), AuthorizationIneligible)
		})
	}
}

func TestAuthorizationIntersectionClassifiesUncertainObservations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*endpointauth.Snapshot, *TrustedEndpointIdentity, *map[string]evpn.EVPNNetworkConfig, *OVNEndpointSnapshot)
	}{
		{
			name: "unknown readiness",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.Bindings[0].Up = nil
			},
		},
		{
			name: "NB read failure",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.NBError = errors.New("NB unavailable")
			},
		},
		{
			name: "SB read failure",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.SBError = errors.New("SB unavailable")
			},
		},
		{
			name: "SB binding missing",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.Bindings = nil
			},
		},
		{
			name: "NB LSP deleted while SB binding remains",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs = nil
			},
		},
		{
			name: "NB and SB absent without deletion proof",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs, observed.Bindings = nil, nil
			},
		},
		{
			name: "duplicate NB identity",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs = append(observed.LSPs, observed.LSPs[0])
			},
		},
		{
			name: "duplicate SB identity",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.Bindings = append(observed.Bindings, observed.Bindings[0])
			},
		},
		{
			name: "multiple address bindings",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].Addresses = append(observed.LSPs[0].Addresses, "02:00:00:00:00:02 192.0.2.12")
			},
		},
		{
			name: "missing correlation identity",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				delete(observed.LSPs[0].ExternalIDs, "evpn.framework/attachment-id")
			},
		},
		{
			name: "noncanonical attachment generation",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.LSPs[0].ExternalIDs[externalIDAttachmentGeneration] = "04"
			},
		},
		{
			name: "policy scope mismatch",
			mutate: func(_ *endpointauth.Snapshot, local *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, _ *OVNEndpointSnapshot) {
				local.LocalChassis = "chassis-other"
			},
		},
		{
			name: "installation identity mismatch",
			mutate: func(_ *endpointauth.Snapshot, local *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, _ *OVNEndpointSnapshot) {
				local.InstallationUID = "installation-other"
			},
		},
		{
			name: "node identity mismatch",
			mutate: func(_ *endpointauth.Snapshot, local *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, _ *OVNEndpointSnapshot) {
				local.NodeUID = "node-other"
			},
		},
		{
			name: "network mapping absent",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, networks *map[string]evpn.EVPNNetworkConfig, _ *OVNEndpointSnapshot) {
				delete(*networks, "net-a")
			},
		},
		{
			name: "NB observation incomplete",
			mutate: func(_ *endpointauth.Snapshot, _ *TrustedEndpointIdentity, _ *map[string]evpn.EVPNNetworkConfig, observed *OVNEndpointSnapshot) {
				observed.NBComplete = false
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, local, networks, observed := authorizationFixture(t)
			test.mutate(policy, &local, &networks, &observed)
			result := intersectFixture(t, policy, local, networks, observed)
			expectState(t, result, AuthorizationUncertain)
		})
	}
}

func TestAuthorizationIntersectionRejectsUnavailableOrInvalidPolicyAsUncertain(t *testing.T) {
	_, local, networks, observed := authorizationFixture(t)
	for name, policy := range map[string]*endpointauth.Snapshot{
		"unavailable":        nil,
		"invalid zero value": {},
	} {
		t.Run(name, func(t *testing.T) {
			result := intersectFixture(t, policy, local, networks, observed)
			if len(result.Decisions) != 1 || result.Decisions[0].State != AuthorizationUncertain {
				t.Fatalf("policy failure did not produce explicit uncertainty: %#v", result)
			}
		})
	}
}

func TestAuthorizationIntersectionDoesNotTreatInvalidEmptySnapshotAsEmptyPolicy(t *testing.T) {
	observed := OVNEndpointSnapshot{NBComplete: true, SBComplete: true}
	result := IntersectEndpointAuthorization(
		nil, TrustedEndpointIdentity{}, nil, observed,
	)
	if len(result.Decisions) != 1 || result.Decisions[0].State != AuthorizationUncertain {
		t.Fatalf("invalid empty policy was treated as an empty authorization list: %#v", result)
	}
}

func TestAuthorizationIntersectionRequiresAuthoritativeTombstoneAndCompleteAbsence(t *testing.T) {
	initial, local, networks, observed := authorizationFixture(t)
	old := initial.Endpoints()[0]
	deleted := lifecyclePolicy(t, 13, []endpointauth.EndpointRecord{}, []endpointauth.EndpointTombstone{{
		Reason: endpointauth.TombstoneDeleted, Record: old,
	}})
	observed.LSPs, observed.Bindings = nil, nil
	result := intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, deleted}, local, networks, observed)
	decision := expectState(t, result, AuthorizationIneligible)
	if decision.Reason != "trusted deletion tombstone and complete NB/SB absence confirm endpoint removal" {
		t.Fatalf("deletion lacked explicit authoritative reason: %#v", decision)
	}

	incomplete := observed
	incomplete.NBComplete = false
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, deleted}, local, networks, incomplete)
	expectState(t, result, AuthorizationUncertain)

	stillPresent := authorizationFixtureObservation(t)
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, deleted}, local, networks, stillPresent)
	expectState(t, result, AuthorizationUncertain)
}

func TestAuthorizationIntersectionTombstonesConfirmReplacementAndMigration(t *testing.T) {
	initial, local, networks, observed := authorizationFixture(t)
	old := initial.Endpoints()[0]
	newer := old
	newer.AttachmentID, newer.AttachmentGeneration = "attachment-2", 5
	newer.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000789"
	newer.MAC, newer.IP = "02:00:00:00:00:02", "192.0.2.11"
	policy := lifecyclePolicy(t, 13, []endpointauth.EndpointRecord{newer}, []endpointauth.EndpointTombstone{{
		Reason: endpointauth.TombstoneReplaced, Record: old,
	}})
	observed.LSPs[0].UUID = newer.LogicalSwitchPortUUID
	observed.LSPs[0].Addresses = []string{newer.MAC + " " + newer.IP}
	observed.LSPs[0].ExternalIDs["evpn.framework/attachment-id"] = newer.AttachmentID
	observed.LSPs[0].ExternalIDs[externalIDAttachmentGeneration] = "5"
	result := intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, policy}, local, networks, observed)
	decision := expectState(t, result, AuthorizationEligible)
	if decision.Endpoint == nil || decision.Endpoint.Incarnation != "attachment-generation:5" {
		t.Fatalf("replacement did not authorize the newer incarnation: %#v", decision)
	}

	migrated := lifecyclePolicy(t, 13, []endpointauth.EndpointRecord{}, []endpointauth.EndpointTombstone{{
		Reason: endpointauth.TombstoneMigrated, Record: old,
	}})
	observed = authorizationFixtureObservation(t)
	observed.Bindings[0].Chassis = "chassis-remote"
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, migrated}, local, networks, observed)
	decision = expectState(t, result, AuthorizationIneligible)
	if decision.Reason != "trusted migration tombstone matches authoritative remote SB binding" {
		t.Fatalf("migration evidence was not explicit: %#v", decision)
	}

	observed.Bindings[0].Up = nil
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, migrated}, local, networks, observed)
	expectState(t, result, AuthorizationUncertain)

	observed = authorizationFixtureObservation(t)
	observed.Bindings[0].Chassis = "chassis-remote"
	observed.Bindings[0].UUID = "not-a-uuid"
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, migrated}, local, networks, observed)
	expectState(t, result, AuthorizationUncertain)

	observed = authorizationFixtureObservation(t)
	observed.LSPs[0].Name = "unexpected-lsp-name"
	observed.Bindings[0].LogicalPort = "unexpected-lsp-name"
	observed.Bindings[0].Chassis = "chassis-remote"
	result = intersectLifecycleFixture(t, []*endpointauth.Snapshot{initial, migrated}, local, networks, observed)
	expectState(t, result, AuthorizationUncertain)
}

func authorizationFixtureObservation(t *testing.T) OVNEndpointSnapshot {
	t.Helper()
	_, _, _, observed := authorizationFixture(t)
	return observed
}

func TestAuthorizationIntersectionCannotUseUnpersistedPolicy(t *testing.T) {
	policy, local, networks, observed := authorizationFixture(t)
	fencer, err := endpointauth.OpenFencer(
		&authorizationTestFenceStore{persistErr: errors.New("injected durable write failure")},
		policy.InstallationUID(), policy.NodeUID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fencer.Close(); err != nil {
			t.Error(err)
		}
	})
	if accepted, err := fencer.Accept(policy); err == nil || accepted != nil {
		t.Fatal("policy with failed durable acceptance produced an accepted snapshot")
	}
	result := IntersectEndpointAuthorization(nil, local, networks, observed)
	if len(result.Decisions) != 1 || result.Decisions[0].State != AuthorizationUncertain ||
		result.Decisions[0].Endpoint != nil {
		t.Fatalf("unpersisted policy did not fail closed: %#v", result.Decisions)
	}
}

func TestAuthorizationIntersectionKeepsOneImmutablePolicyVersionPerCycle(t *testing.T) {
	policy, local, networks, observed := authorizationFixture(t)
	version := policy.PolicyVersion()
	document := endpointauth.Document{
		Version: 1, Revision: 13, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1", Endpoints: []endpointauth.EndpointRecord{},
	}
	changed, err := endpointauth.NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	fencer, err := endpointauth.OpenFencer(&authorizationTestFenceStore{}, policy.InstallationUID(), policy.NodeUID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fencer.Close(); err != nil {
			t.Error(err)
		}
	})
	accepted, err := fencer.Accept(policy)
	if err != nil {
		t.Fatal(err)
	}
	result := IntersectEndpointAuthorization(accepted, local, networks, observed)
	if result.PolicyVersion != version || result.PolicyVersion == changed.PolicyVersion() {
		t.Fatalf("intersection mixed policy versions: result=%#v original=%#v changed=%#v",
			result.PolicyVersion, version, changed.PolicyVersion())
	}
	if len(result.Decisions) != 1 || result.Decisions[0].State != AuthorizationEligible {
		t.Fatalf("unexpected decision from immutable policy snapshot: %#v", result.Decisions)
	}
}
