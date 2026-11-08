// Package endpointauth defines platform-neutral authorization intent for EVPN
// endpoint attachments. A snapshot is not evidence that an endpoint is present
// or ready in the dataplane.
package endpointauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const (
	CurrentVersion   = 1
	LifecycleVersion = 2
	MaxDocumentSize  = 1 << 20
)

// Document is the versioned administrator-authorized endpoint policy.
type Document struct {
	Version         int                 `json:"version"`
	Revision        uint64              `json:"revision"`
	InstallationUID string              `json:"installationUID"`
	NodeUID         string              `json:"nodeUID"`
	LocalChassis    string              `json:"localChassis"`
	Endpoints       []EndpointRecord    `json:"endpoints"`
	Tombstones      []EndpointTombstone `json:"tombstones,omitempty"`
}

// EndpointRecord authorizes one MAC/IP attachment to one exact OVN LSP UUID.
type EndpointRecord struct {
	NetworkID             string `json:"networkID"`
	EndpointID            string `json:"endpointID"`
	AttachmentID          string `json:"attachmentID"`
	AttachmentGeneration  uint64 `json:"attachmentGeneration"`
	LogicalSwitchPort     string `json:"logicalSwitchPort"`
	LogicalSwitchPortUUID string `json:"logicalSwitchPortUUID"`
	MAC                   string `json:"mac"`
	IP                    string `json:"ip"`
}

type EndpointTombstoneReason string

const (
	TombstoneDeleted  EndpointTombstoneReason = "deleted"
	TombstoneReplaced EndpointTombstoneReason = "replaced"
	TombstoneMigrated EndpointTombstoneReason = "migrated"
)

// EndpointTombstone is trusted lifecycle evidence for one exact retired
// attachment. It does not by itself replace complete OVN NB/SB observations.
type EndpointTombstone struct {
	Reason EndpointTombstoneReason `json:"reason"`
	Record EndpointRecord          `json:"record"`
}

// PolicyVersion is the compact revision identity suitable for durable fencing.
// Comparing or persisting versions is intentionally outside this package.
type PolicyVersion struct {
	Revision uint64
	Digest   string
}

type endpointKey struct {
	networkID  string
	endpointID string
}

type attachmentKey struct {
	networkID    string
	attachmentID string
}

// Snapshot is an immutable, fully validated authorization document.
type Snapshot struct {
	version         int
	revision        uint64
	installationUID string
	nodeUID         string
	localChassis    string
	endpoints       []EndpointRecord
	tombstones      []EndpointTombstone
	byIdentity      map[endpointKey]EndpointRecord
	canonical       []byte
	digest          string
}

// NewSnapshot validates and canonicalizes a document, taking an independent
// copy of its endpoint records.
func NewSnapshot(document Document) (*Snapshot, error) {
	if document.Version != CurrentVersion && document.Version != LifecycleVersion {
		return nil, fmt.Errorf("unsupported endpoint authorization version %d", document.Version)
	}
	if document.Revision == 0 {
		return nil, fmt.Errorf("authorization revision must be positive")
	}
	for name, value := range map[string]string{
		"installationUID": document.InstallationUID,
		"nodeUID":         document.NodeUID,
		"localChassis":    document.LocalChassis,
	} {
		if err := validateIdentity(name, value); err != nil {
			return nil, err
		}
	}
	if document.Endpoints == nil {
		return nil, fmt.Errorf("endpoints must be an array, even when empty")
	}
	if document.Version == LifecycleVersion && document.Tombstones == nil {
		return nil, fmt.Errorf("tombstones must be an array in authorization version %d", LifecycleVersion)
	}
	if document.Version == CurrentVersion && len(document.Tombstones) != 0 {
		return nil, fmt.Errorf("authorization tombstones require version %d", LifecycleVersion)
	}

	endpoints := append([]EndpointRecord{}, document.Endpoints...)
	byIdentity := make(map[endpointKey]EndpointRecord, len(endpoints))
	seenAttachments := make(map[attachmentKey]uint64, len(endpoints))
	seenUUIDs := make(map[string]bool, len(endpoints))
	endpointNetworks := make(map[string]string, len(endpoints))
	attachmentNetworks := make(map[string]string, len(endpoints))

	for i := range endpoints {
		record := &endpoints[i]
		for name, value := range map[string]string{
			"networkID":         record.NetworkID,
			"endpointID":        record.EndpointID,
			"attachmentID":      record.AttachmentID,
			"logicalSwitchPort": record.LogicalSwitchPort,
		} {
			if err := validateIdentity(name, value); err != nil {
				return nil, fmt.Errorf("endpoint record %d: %w", i, err)
			}
		}
		if record.AttachmentGeneration == 0 {
			return nil, fmt.Errorf("endpoint record %d: attachment generation must be positive", i)
		}
		lspUUID, err := uuid.Parse(record.LogicalSwitchPortUUID)
		if err != nil || lspUUID == uuid.Nil || lspUUID.String() != record.LogicalSwitchPortUUID {
			return nil, fmt.Errorf("endpoint record %d: LSP UUID must be canonical", i)
		}
		mac, err := net.ParseMAC(record.MAC)
		if err != nil || len(mac) != 6 || mac.String() != record.MAC {
			return nil, fmt.Errorf("endpoint record %d: MAC must be a canonical six-byte address", i)
		}
		ip, err := netip.ParseAddr(record.IP)
		if err != nil || ip.Zone() != "" || ip.String() != record.IP {
			return nil, fmt.Errorf("endpoint record %d: IP must be a canonical address", i)
		}

		key := endpointKey{networkID: record.NetworkID, endpointID: record.EndpointID}
		if _, exists := byIdentity[key]; exists {
			return nil, fmt.Errorf("duplicate endpoint identity %q on network %q", record.EndpointID, record.NetworkID)
		}
		if network, exists := endpointNetworks[record.EndpointID]; exists && network != record.NetworkID {
			return nil, fmt.Errorf("endpoint %q is ambiguously associated with networks %q and %q", record.EndpointID, network, record.NetworkID)
		}
		endpointNetworks[record.EndpointID] = record.NetworkID

		attachment := attachmentKey{networkID: record.NetworkID, attachmentID: record.AttachmentID}
		if generation, exists := seenAttachments[attachment]; exists {
			return nil, fmt.Errorf("duplicate attachment identity %q on network %q (generations %d and %d)", record.AttachmentID, record.NetworkID, generation, record.AttachmentGeneration)
		}
		if network, exists := attachmentNetworks[record.AttachmentID]; exists && network != record.NetworkID {
			return nil, fmt.Errorf("attachment %q is ambiguously associated with networks %q and %q", record.AttachmentID, network, record.NetworkID)
		}
		if seenUUIDs[record.LogicalSwitchPortUUID] {
			return nil, fmt.Errorf("duplicate LSP UUID %q", record.LogicalSwitchPortUUID)
		}
		seenAttachments[attachment] = record.AttachmentGeneration
		attachmentNetworks[record.AttachmentID] = record.NetworkID
		seenUUIDs[record.LogicalSwitchPortUUID] = true
		byIdentity[key] = *record
	}

	tombstones := append([]EndpointTombstone{}, document.Tombstones...)
	seenTombstones := make(map[endpointKey]bool, len(tombstones))
	for i := range tombstones {
		tombstone := &tombstones[i]
		switch tombstone.Reason {
		case TombstoneDeleted, TombstoneReplaced, TombstoneMigrated:
		default:
			return nil, fmt.Errorf("endpoint tombstone %d: unsupported reason %q", i, tombstone.Reason)
		}
		validated, err := NewSnapshot(Document{
			Version: CurrentVersion, Revision: document.Revision,
			InstallationUID: document.InstallationUID, NodeUID: document.NodeUID,
			LocalChassis: document.LocalChassis,
			Endpoints:    []EndpointRecord{tombstone.Record},
		})
		if err != nil {
			return nil, fmt.Errorf("endpoint tombstone %d: %w", i, err)
		}
		tombstone.Record = validated.endpoints[0]
		key := endpointKey{networkID: tombstone.Record.NetworkID, endpointID: tombstone.Record.EndpointID}
		if network, exists := endpointNetworks[key.endpointID]; exists && network != key.networkID {
			return nil, fmt.Errorf("endpoint tombstone %q is ambiguously associated with networks %q and %q", key.endpointID, network, key.networkID)
		}
		endpointNetworks[key.endpointID] = key.networkID
		attachment := attachmentKey{networkID: tombstone.Record.NetworkID, attachmentID: tombstone.Record.AttachmentID}
		if network, exists := attachmentNetworks[attachment.attachmentID]; exists && network != attachment.networkID {
			return nil, fmt.Errorf("attachment tombstone %q is ambiguously associated with networks %q and %q", attachment.attachmentID, network, attachment.networkID)
		}
		attachmentNetworks[attachment.attachmentID] = attachment.networkID
		if seenUUIDs[tombstone.Record.LogicalSwitchPortUUID] {
			return nil, fmt.Errorf("duplicate LSP UUID %q in endpoint tombstones", tombstone.Record.LogicalSwitchPortUUID)
		}
		seenUUIDs[tombstone.Record.LogicalSwitchPortUUID] = true
		if seenTombstones[key] {
			return nil, fmt.Errorf("duplicate endpoint tombstone for %q on network %q", key.endpointID, key.networkID)
		}
		seenTombstones[key] = true
		if active, exists := byIdentity[key]; exists {
			if tombstone.Reason != TombstoneReplaced || active.AttachmentGeneration <= tombstone.Record.AttachmentGeneration {
				return nil, fmt.Errorf("endpoint tombstone for %q on network %q conflicts with active attachment", key.endpointID, key.networkID)
			}
		}
	}

	sort.Slice(endpoints, func(i, j int) bool {
		a, b := endpoints[i], endpoints[j]
		if a.NetworkID != b.NetworkID {
			return a.NetworkID < b.NetworkID
		}
		if a.EndpointID != b.EndpointID {
			return a.EndpointID < b.EndpointID
		}
		if a.AttachmentID != b.AttachmentID {
			return a.AttachmentID < b.AttachmentID
		}
		return a.AttachmentGeneration < b.AttachmentGeneration
	})
	sort.Slice(tombstones, func(i, j int) bool {
		a, b := tombstones[i].Record, tombstones[j].Record
		if a.NetworkID != b.NetworkID {
			return a.NetworkID < b.NetworkID
		}
		if a.EndpointID != b.EndpointID {
			return a.EndpointID < b.EndpointID
		}
		return a.AttachmentGeneration < b.AttachmentGeneration
	})
	var canonical []byte
	var err error
	if document.Version == CurrentVersion {
		canonical, err = json.Marshal(struct {
			Version         int              `json:"version"`
			Revision        uint64           `json:"revision"`
			InstallationUID string           `json:"installationUID"`
			NodeUID         string           `json:"nodeUID"`
			LocalChassis    string           `json:"localChassis"`
			Endpoints       []EndpointRecord `json:"endpoints"`
		}{document.Version, document.Revision, document.InstallationUID, document.NodeUID,
			document.LocalChassis, endpoints})
	} else {
		canonical, err = json.Marshal(struct {
			Version         int                 `json:"version"`
			Revision        uint64              `json:"revision"`
			InstallationUID string              `json:"installationUID"`
			NodeUID         string              `json:"nodeUID"`
			LocalChassis    string              `json:"localChassis"`
			Endpoints       []EndpointRecord    `json:"endpoints"`
			Tombstones      []EndpointTombstone `json:"tombstones"`
		}{document.Version, document.Revision, document.InstallationUID, document.NodeUID,
			document.LocalChassis, endpoints, tombstones})
	}
	if err != nil {
		return nil, fmt.Errorf("canonicalize authorization document: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return &Snapshot{
		version: document.Version, revision: document.Revision,
		installationUID: document.InstallationUID, nodeUID: document.NodeUID,
		localChassis: document.LocalChassis, endpoints: endpoints, tombstones: tombstones,
		byIdentity: byIdentity, canonical: canonical, digest: hex.EncodeToString(sum[:]),
	}, nil
}

// Decode validates exactly one JSON document, rejecting duplicate object keys,
// unknown fields, unsupported versions and trailing data.
func Decode(data []byte) (*Snapshot, error) {
	if len(data) > MaxDocumentSize {
		return nil, fmt.Errorf("authorization document exceeds %d bytes", MaxDocumentSize)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("authorization document is empty")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	if err := rejectUnknownJSONFields(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document *Document
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode authorization document: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("authorization document must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("authorization document must contain exactly one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read authorization document trailer: %w", err)
	}
	return NewSnapshot(*document)
}

func rejectUnknownJSONFields(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil
	}
	for key := range object {
		switch key {
		case "version", "revision", "installationUID", "nodeUID", "localChassis", "endpoints", "tombstones":
		default:
			return fmt.Errorf("unknown authorization field %q", key)
		}
	}
	rawEndpoints, ok := object["endpoints"]
	if !ok {
		return nil
	}
	var endpoints []json.RawMessage
	if err := json.Unmarshal(rawEndpoints, &endpoints); err != nil {
		return nil
	}
	for i, rawEndpoint := range endpoints {
		var endpoint map[string]json.RawMessage
		if err := json.Unmarshal(rawEndpoint, &endpoint); err != nil || endpoint == nil {
			continue
		}
		for key := range endpoint {
			switch key {
			case "networkID", "endpointID", "attachmentID", "attachmentGeneration",
				"logicalSwitchPort", "logicalSwitchPortUUID", "mac", "ip":
			default:
				return fmt.Errorf("endpoint record %d: unknown authorization field %q", i, key)
			}
		}
	}
	rawTombstones, ok := object["tombstones"]
	if !ok {
		return nil
	}
	var tombstones []json.RawMessage
	if err := json.Unmarshal(rawTombstones, &tombstones); err != nil {
		return nil
	}
	for i, rawTombstone := range tombstones {
		var tombstone map[string]json.RawMessage
		if err := json.Unmarshal(rawTombstone, &tombstone); err != nil || tombstone == nil {
			continue
		}
		for key := range tombstone {
			if key != "reason" && key != "record" {
				return fmt.Errorf("endpoint tombstone %d: unknown authorization field %q", i, key)
			}
		}
		var record map[string]json.RawMessage
		if rawRecord, ok := tombstone["record"]; ok {
			if err := json.Unmarshal(rawRecord, &record); err == nil && record != nil {
				for key := range record {
					switch key {
					case "networkID", "endpointID", "attachmentID", "attachmentGeneration",
						"logicalSwitchPort", "logicalSwitchPortUUID", "mac", "ip":
					default:
						return fmt.Errorf("endpoint tombstone %d record: unknown authorization field %q", i, key)
					}
				}
			}
		}
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid authorization JSON: %w", err)
	}
	if _, err := decoder.Token(); err == nil {
		return fmt.Errorf("authorization document must contain exactly one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid authorization JSON trailer: %w", err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func validateIdentity(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is required and must not have surrounding whitespace", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func (s *Snapshot) Version() int            { return s.version }
func (s *Snapshot) Revision() uint64        { return s.revision }
func (s *Snapshot) InstallationUID() string { return s.installationUID }
func (s *Snapshot) NodeUID() string         { return s.nodeUID }
func (s *Snapshot) LocalChassis() string    { return s.localChassis }
func (s *Snapshot) Digest() string          { return s.digest }
func (s *Snapshot) PolicyVersion() PolicyVersion {
	return PolicyVersion{Revision: s.revision, Digest: s.digest}
}

// Endpoints returns a copy. EndpointRecord contains only immutable scalar
// values, so callers cannot mutate the snapshot through the returned slice.
func (s *Snapshot) Endpoints() []EndpointRecord {
	return append([]EndpointRecord{}, s.endpoints...)
}

// Tombstones returns a copy of the exact trusted retired attachments.
func (s *Snapshot) Tombstones() []EndpointTombstone {
	return append([]EndpointTombstone{}, s.tombstones...)
}

// Lookup returns the authorized record for an exact network and endpoint
// identity. The caller must still compare the complete record with observation.
func (s *Snapshot) Lookup(networkID, endpointID string) (EndpointRecord, bool) {
	record, ok := s.byIdentity[endpointKey{networkID: networkID, endpointID: endpointID}]
	return record, ok
}

// Matches reports whether candidate exactly matches an authorized record,
// including its attachment generation and all OVN/address bindings.
func (s *Snapshot) Matches(candidate EndpointRecord) bool {
	record, ok := s.Lookup(candidate.NetworkID, candidate.EndpointID)
	return ok && record == candidate
}

// CanonicalJSON returns a copy of the validated deterministic representation.
func (s *Snapshot) CanonicalJSON() []byte {
	return append([]byte(nil), s.canonical...)
}
