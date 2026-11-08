package ovnexport

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
)

const externalIDAttachmentGeneration = "evpn.framework/attachment-generation"

type AuthorizationState string

const (
	AuthorizationEligible   AuthorizationState = "eligible"
	AuthorizationIneligible AuthorizationState = "ineligible"
	AuthorizationUncertain  AuthorizationState = "uncertain"
)

type AuthorizationDecision struct {
	NetworkID  string
	EndpointID string
	State      AuthorizationState
	Reason     string
	Endpoint   *evpn.LocalEndpoint
}

type AuthorizationIntersection struct {
	PolicyVersion endpointauth.PolicyVersion
	Decisions     []AuthorizationDecision
}

type TrustedEndpointIdentity struct {
	InstallationUID string
	NodeUID         string
	NodeName        string
	LocalChassis    string
}

// OVNEndpointSnapshot must represent complete NB and SB reads from one
// observation cycle. LogicalSwitch is the LSP membership resolved from the
// NB Logical_Switch.ports relation; LSP UUIDs and Port_Binding UUIDs must come
// from their actual rows, not external_ids.
type OVNEndpointSnapshot struct {
	NBComplete bool
	SBComplete bool
	NBError    error
	SBError    error
	LSPs       []OVNLogicalSwitchPort
	Bindings   []OVNPortBinding
}

type authorizationIdentity struct {
	networkID  string
	endpointID string
}

// IntersectEndpointAuthorization evaluates one immutable policy snapshot
// against one complete NB/SB observation. It returns decisions only; it never
// mutates route or ownership state.
func IntersectEndpointAuthorization(
	accepted *endpointauth.AcceptedSnapshot,
	local TrustedEndpointIdentity,
	networks map[string]evpn.EVPNNetworkConfig,
	observed OVNEndpointSnapshot,
) AuthorizationIntersection {
	result := AuthorizationIntersection{}
	policy, current := accepted.Snapshot()
	if !current {
		result.Decisions = []AuthorizationDecision{{
			State: AuthorizationUncertain, Reason: "durably accepted authorization policy is unavailable or stale",
		}}
		return result
	}
	result.PolicyVersion = accepted.PolicyVersion()

	records := policy.Endpoints()
	recordByIdentity := make(map[authorizationIdentity]endpointauth.EndpointRecord, len(records))
	tombstones := policy.Tombstones()
	tombstoneByIdentity := make(map[authorizationIdentity]endpointauth.EndpointTombstone, len(tombstones))
	identities := make(map[authorizationIdentity]bool, len(records)+len(tombstones)+len(observed.LSPs))
	for _, record := range records {
		key := authorizationIdentity{networkID: record.NetworkID, endpointID: record.EndpointID}
		recordByIdentity[key] = record
		identities[key] = true
	}
	for _, tombstone := range tombstones {
		key := authorizationIdentity{
			networkID:  tombstone.Record.NetworkID,
			endpointID: tombstone.Record.EndpointID,
		}
		tombstoneByIdentity[key] = tombstone
		identities[key] = true
	}
	for _, lsp := range observed.LSPs {
		ids := lsp.ExternalIDs
		if networkID, endpointID := ids["evpn.framework/network-id"], ids["evpn.framework/endpoint-id"]; networkID != "" && endpointID != "" {
			identities[authorizationIdentity{networkID: networkID, endpointID: endpointID}] = true
		}
	}
	keys := make([]authorizationIdentity, 0, len(identities))
	for key := range identities {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].networkID != keys[j].networkID {
			return keys[i].networkID < keys[j].networkID
		}
		return keys[i].endpointID < keys[j].endpointID
	})
	globalReason := ""
	switch {
	case policy.Version() < endpointauth.CurrentVersion || policy.Version() > endpointauth.LifecycleVersion ||
		policy.Revision() == 0 || policy.Digest() == "":
		globalReason = "authorization snapshot is not a validated policy"
	case observed.NBError != nil || observed.SBError != nil:
		globalReason = observationFailure(observed)
	case !observed.NBComplete || !observed.SBComplete:
		globalReason = "NB and SB observations must both be complete"
	case strings.TrimSpace(local.InstallationUID) == "" || strings.TrimSpace(local.NodeUID) == "" ||
		strings.TrimSpace(local.NodeName) == "" || strings.TrimSpace(local.LocalChassis) == "":
		globalReason = "trusted local installation, node, node name and chassis identity are required"
	case policy.InstallationUID() != local.InstallationUID || policy.NodeUID() != local.NodeUID ||
		policy.LocalChassis() != local.LocalChassis:
		globalReason = "authorization policy scope differs from independently established local identity"
	}
	if len(keys) == 0 && globalReason != "" {
		result.Decisions = append(result.Decisions, uncertainDecision("", "", globalReason))
		return result
	}

	for _, key := range keys {
		if globalReason != "" {
			result.Decisions = append(result.Decisions, uncertainDecision(key.networkID, key.endpointID, globalReason))
			continue
		}
		record, authorized := recordByIdentity[key]
		tombstone, hasTombstone := tombstoneByIdentity[key]
		if !authorized {
			if hasTombstone {
				state, reason := evaluateTombstone(tombstone, nil, local, observed)
				result.Decisions = append(result.Decisions, AuthorizationDecision{
					NetworkID: key.networkID, EndpointID: key.endpointID,
					State: state, Reason: reason,
				})
				continue
			}
			result.Decisions = append(result.Decisions, AuthorizationDecision{
				NetworkID: key.networkID, EndpointID: key.endpointID,
				State: AuthorizationIneligible, Reason: "endpoint is absent from the trusted policy",
			})
			continue
		}
		if hasTombstone {
			state, reason := evaluateTombstone(tombstone, &record, local, observed)
			if state != AuthorizationEligible {
				result.Decisions = append(result.Decisions, AuthorizationDecision{
					NetworkID: key.networkID, EndpointID: key.endpointID,
					State: state, Reason: reason,
				})
				continue
			}
		}
		result.Decisions = append(result.Decisions, evaluateAuthorizedEndpoint(record, local, networks, observed))
	}
	if !accepted.IsCurrent() {
		for i := range result.Decisions {
			result.Decisions[i].State = AuthorizationUncertain
			result.Decisions[i].Reason = "durable authorization policy changed during intersection"
			result.Decisions[i].Endpoint = nil
		}
	}
	return result
}

func evaluateTombstone(
	tombstone endpointauth.EndpointTombstone,
	replacement *endpointauth.EndpointRecord,
	local TrustedEndpointIdentity,
	observed OVNEndpointSnapshot,
) (AuthorizationState, string) {
	record := tombstone.Record
	var sameNameReplacement *endpointauth.EndpointRecord
	if tombstone.Reason == endpointauth.TombstoneReplaced {
		if replacement == nil || replacement.NetworkID != record.NetworkID ||
			replacement.EndpointID != record.EndpointID ||
			replacement.AttachmentGeneration <= record.AttachmentGeneration {
			return AuthorizationUncertain, "replacement tombstone has no newer authorized attachment"
		}
		sameNameReplacement = replacement
	}

	var oldPortVisible bool
	for _, lsp := range observed.LSPs {
		if lsp.UUID == record.LogicalSwitchPortUUID {
			if tombstone.Reason == endpointauth.TombstoneMigrated {
				if lsp.Name != record.LogicalSwitchPort {
					return AuthorizationUncertain, "migration tombstone LSP name differs from the retired attachment"
				}
				bindings := bindingsForPort(observed.Bindings, lsp.Name)
				if len(bindings) != 1 || !canonicalUUID(bindings[0].UUID) ||
					bindings[0].Up == nil || !*bindings[0].Up ||
					bindings[0].Chassis == "" || bindings[0].Chassis == local.LocalChassis {
					return AuthorizationUncertain, "migration tombstone lacks a unique ready remote SB binding"
				}
				return AuthorizationIneligible, "trusted migration tombstone matches authoritative remote SB binding"
			}
			return AuthorizationUncertain, "retired LSP UUID remains present after lifecycle tombstone"
		}
		if lsp.Name == record.LogicalSwitchPort {
			oldPortVisible = true
			if sameNameReplacement == nil || lsp.UUID != sameNameReplacement.LogicalSwitchPortUUID ||
				lsp.Name != sameNameReplacement.LogicalSwitchPort {
				return AuthorizationUncertain, "retired LSP name remains present without an exact authorized replacement"
			}
		}
	}

	for _, binding := range observed.Bindings {
		if binding.LogicalPort != record.LogicalSwitchPort {
			continue
		}
		if sameNameReplacement == nil || !oldPortVisible ||
			binding.LogicalPort != sameNameReplacement.LogicalSwitchPort {
			return AuthorizationUncertain, "retired logical port still has an SB binding"
		}
	}

	switch tombstone.Reason {
	case endpointauth.TombstoneDeleted:
		return AuthorizationIneligible, "trusted deletion tombstone and complete NB/SB absence confirm endpoint removal"
	case endpointauth.TombstoneReplaced:
		return AuthorizationEligible, "trusted replacement tombstone confirms the old attachment is absent"
	case endpointauth.TombstoneMigrated:
		return AuthorizationUncertain, "migration tombstone lacks an authoritative remote binding"
	default:
		return AuthorizationUncertain, "unsupported endpoint lifecycle tombstone"
	}
}

func bindingsForPort(bindings []OVNPortBinding, logicalPort string) []OVNPortBinding {
	var matches []OVNPortBinding
	for _, binding := range bindings {
		if binding.LogicalPort == logicalPort {
			matches = append(matches, binding)
		}
	}
	return matches
}

func evaluateAuthorizedEndpoint(
	record endpointauth.EndpointRecord,
	local TrustedEndpointIdentity,
	networks map[string]evpn.EVPNNetworkConfig,
	observed OVNEndpointSnapshot,
) AuthorizationDecision {
	decision := AuthorizationDecision{
		NetworkID: record.NetworkID, EndpointID: record.EndpointID,
	}
	cfg, ok := networks[record.NetworkID]
	if !ok {
		decision.State, decision.Reason = AuthorizationUncertain, "trusted network-to-switch mapping is unavailable"
		return decision
	}
	if cfg.NetworkID != record.NetworkID || cfg.LogicalSwitch == "" {
		decision.State, decision.Reason = AuthorizationUncertain, "trusted network-to-switch mapping is incomplete or contradictory"
		return decision
	}
	if !cfg.Enabled {
		decision.State, decision.Reason = AuthorizationIneligible, "trusted network mapping is disabled"
		return decision
	}

	matches := make([]OVNLogicalSwitchPort, 0, 1)
	for _, lsp := range observed.LSPs {
		if lsp.UUID == record.LogicalSwitchPortUUID || lsp.Name == record.LogicalSwitchPort ||
			lsp.ExternalIDs["evpn.framework/network-id"] == record.NetworkID &&
				lsp.ExternalIDs["evpn.framework/endpoint-id"] == record.EndpointID {
			matches = append(matches, lsp)
		}
	}
	if len(matches) == 0 {
		for _, binding := range observed.Bindings {
			if binding.LogicalPort == record.LogicalSwitchPort {
				decision.State, decision.Reason = AuthorizationUncertain,
					"SB Port_Binding remains while its NB LSP is absent; deletion is not established"
				return decision
			}
		}
		decision.State, decision.Reason = AuthorizationUncertain,
			"endpoint is absent from NB and SB observations without authoritative deletion evidence"
		return decision
	}
	if len(matches) != 1 {
		decision.State, decision.Reason = AuthorizationUncertain, "multiple NB LSP rows match the authorized endpoint"
		return decision
	}
	lsp := matches[0]
	if !canonicalUUID(lsp.UUID) {
		decision.State, decision.Reason = AuthorizationUncertain, "NB LSP row has an invalid UUID"
		return decision
	}

	bindings := make([]OVNPortBinding, 0, 1)
	for _, binding := range observed.Bindings {
		if binding.LogicalPort == lsp.Name {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) == 0 {
		decision.State, decision.Reason = AuthorizationUncertain, "NB LSP has no SB Port_Binding in the complete observation"
		return decision
	}
	if len(bindings) != 1 {
		decision.State, decision.Reason = AuthorizationUncertain, "multiple SB Port_Binding rows match the NB LSP"
		return decision
	}
	binding := bindings[0]
	if !canonicalUUID(binding.UUID) || strings.TrimSpace(binding.LogicalPort) == "" {
		decision.State, decision.Reason = AuthorizationUncertain, "SB Port_Binding identity is incomplete or invalid"
		return decision
	}
	for _, other := range observed.Bindings {
		if other.UUID == binding.UUID && other.LogicalPort != binding.LogicalPort {
			decision.State, decision.Reason = AuthorizationUncertain, "SB Port_Binding UUID is duplicated"
			return decision
		}
	}
	if binding.Up == nil {
		decision.State, decision.Reason = AuthorizationUncertain, "SB Port_Binding readiness is unknown"
		return decision
	}
	if !*binding.Up {
		decision.State, decision.Reason = AuthorizationIneligible, "SB Port_Binding is authoritatively down"
		return decision
	}
	if strings.TrimSpace(binding.Chassis) == "" {
		decision.State, decision.Reason = AuthorizationUncertain, "ready SB Port_Binding has no authoritative chassis"
		return decision
	}
	if binding.Chassis != local.LocalChassis {
		decision.State, decision.Reason = AuthorizationIneligible, "authoritative SB binding is on another chassis"
		return decision
	}

	ids := lsp.ExternalIDs
	for _, field := range []string{"evpn.framework/network-id", "evpn.framework/endpoint-id", "evpn.framework/attachment-id"} {
		if strings.TrimSpace(ids[field]) == "" {
			decision.State, decision.Reason = AuthorizationUncertain, "NB LSP correlation identity is incomplete"
			return decision
		}
	}
	if ids["evpn.framework/network-id"] != record.NetworkID ||
		ids["evpn.framework/endpoint-id"] != record.EndpointID ||
		ids["evpn.framework/attachment-id"] != record.AttachmentID {
		decision.State, decision.Reason = AuthorizationIneligible, "NB LSP correlation identity differs from authorization"
		return decision
	}
	generationText := ids[externalIDAttachmentGeneration]
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != generationText {
		decision.State, decision.Reason = AuthorizationUncertain, "NB LSP attachment generation is unavailable or invalid"
		return decision
	}
	if generation != record.AttachmentGeneration {
		decision.State, decision.Reason = AuthorizationIneligible, "NB LSP attachment generation differs from authorization"
		return decision
	}

	if lsp.UUID != record.LogicalSwitchPortUUID {
		decision.State, decision.Reason = AuthorizationIneligible, "authoritative NB row establishes LSP replacement"
		return decision
	}
	if lsp.Name != record.LogicalSwitchPort {
		decision.State, decision.Reason = AuthorizationIneligible, "NB LSP name differs from authorization"
		return decision
	}
	if lsp.LogicalSwitch == "" {
		decision.State, decision.Reason = AuthorizationUncertain, "authoritative NB logical-switch membership is unavailable"
		return decision
	}
	if lsp.LogicalSwitch != cfg.LogicalSwitch {
		decision.State, decision.Reason = AuthorizationIneligible, "NB LSP belongs to a different logical switch"
		return decision
	}
	mac, ip, ok := exactAddressPair(lsp.Addresses)
	if !ok {
		decision.State, decision.Reason = AuthorizationUncertain, "NB LSP does not expose exactly one canonical MAC/IP binding"
		return decision
	}
	if mac != record.MAC || ip != record.IP {
		decision.State, decision.Reason = AuthorizationIneligible, "authoritative NB MAC/IP differs from authorization"
		return decision
	}

	incarnation := "attachment-generation:" + strconv.FormatUint(record.AttachmentGeneration, 10)
	decision.Endpoint = &evpn.LocalEndpoint{
		NetworkID: record.NetworkID, EndpointID: record.EndpointID, Incarnation: incarnation,
		MACAddress: record.MAC, IPAddresses: []string{record.IP}, VNI: cfg.VNI,
		RouteDistinguisher: cfg.RouteDistinguisher, RouteTargets: append([]string(nil), cfg.RouteTargets...),
		LocalVTEP: cfg.LocalVTEP, NodeName: local.NodeName, Chassis: local.LocalChassis, Advertise: true,
	}
	decision.State, decision.Reason = AuthorizationEligible, "trusted policy exactly matches authoritative ready local OVN state"
	return decision
}

func exactAddressPair(addresses []string) (string, string, bool) {
	if len(addresses) != 1 {
		return "", "", false
	}
	fields := strings.Fields(addresses[0])
	if len(fields) != 2 {
		return "", "", false
	}
	mac, err := net.ParseMAC(fields[0])
	if err != nil || len(mac) != 6 || mac.String() != fields[0] {
		return "", "", false
	}
	ip, err := netip.ParseAddr(fields[1])
	if err != nil {
		prefix, prefixErr := netip.ParsePrefix(fields[1])
		if prefixErr != nil {
			return "", "", false
		}
		ip = prefix.Addr()
		if prefix.String() != fields[1] {
			return "", "", false
		}
	} else if ip.String() != fields[1] {
		return "", "", false
	}
	if ip.Zone() != "" {
		return "", "", false
	}
	return mac.String(), ip.String(), true
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func observationFailure(observed OVNEndpointSnapshot) string {
	switch {
	case observed.NBError != nil && observed.SBError != nil:
		return fmt.Sprintf("NB and SB observation failed: %v; %v", observed.NBError, observed.SBError)
	case observed.NBError != nil:
		return fmt.Sprintf("NB observation failed: %v", observed.NBError)
	case observed.SBError != nil:
		return fmt.Sprintf("SB observation failed: %v", observed.SBError)
	default:
		return "NB and SB observations are incomplete"
	}
}

func uncertainDecision(networkID, endpointID, reason string) AuthorizationDecision {
	return AuthorizationDecision{
		NetworkID: networkID, EndpointID: endpointID,
		State: AuthorizationUncertain, Reason: reason,
	}
}
