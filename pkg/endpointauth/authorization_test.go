package endpointauth

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func validDocument() Document {
	return Document{
		Version: CurrentVersion, Revision: 12, InstallationUID: "installation-1",
		NodeUID: "node-1", LocalChassis: "chassis-1",
		Endpoints: []EndpointRecord{{
			NetworkID: "net-a", EndpointID: "endpoint-123", AttachmentID: "attachment-456",
			AttachmentGeneration: 4, LogicalSwitchPort: "lsp-123",
			LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000123",
			MAC:                   "02:00:00:00:00:01", IP: "192.0.2.10",
		}},
	}
}

func TestNewSnapshotAcceptsValidDocument(t *testing.T) {
	snapshot, err := NewSnapshot(validDocument())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version() != 1 || snapshot.Revision() != 12 ||
		snapshot.InstallationUID() != "installation-1" || snapshot.NodeUID() != "node-1" ||
		snapshot.LocalChassis() != "chassis-1" {
		t.Fatalf("snapshot scope/version mismatch: %#v", snapshot.PolicyVersion())
	}
	record, ok := snapshot.Lookup("net-a", "endpoint-123")
	if !ok || !snapshot.Matches(record) {
		t.Fatalf("authorized record lookup failed: record=%#v ok=%v", record, ok)
	}
	record.IP = "192.0.2.11"
	if snapshot.Matches(record) {
		t.Fatal("snapshot matched an altered attachment binding")
	}
}

func TestNewSnapshotAcceptsExplicitEmptyAllowlist(t *testing.T) {
	document := validDocument()
	document.Endpoints = []EndpointRecord{}
	snapshot, err := NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Endpoints(); len(got) != 0 {
		t.Fatalf("empty allowlist contains %d records", len(got))
	}
	if !bytes.Contains(snapshot.CanonicalJSON(), []byte(`"endpoints":[]`)) {
		t.Fatal("empty allowlist was not preserved as an explicit JSON array")
	}
}

func TestNewSnapshotRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Document)
	}{
		{"unsupported version", func(d *Document) { d.Version = 99 }},
		{"zero revision", func(d *Document) { d.Revision = 0 }},
		{"missing installation", func(d *Document) { d.InstallationUID = "" }},
		{"missing node", func(d *Document) { d.NodeUID = "" }},
		{"missing chassis", func(d *Document) { d.LocalChassis = "" }},
		{"missing endpoint array", func(d *Document) { d.Endpoints = nil }},
		{"missing network", func(d *Document) { d.Endpoints[0].NetworkID = "" }},
		{"missing endpoint", func(d *Document) { d.Endpoints[0].EndpointID = "" }},
		{"missing attachment", func(d *Document) { d.Endpoints[0].AttachmentID = "" }},
		{"zero generation", func(d *Document) { d.Endpoints[0].AttachmentGeneration = 0 }},
		{"missing LSP", func(d *Document) { d.Endpoints[0].LogicalSwitchPort = "" }},
		{"invalid UUID", func(d *Document) { d.Endpoints[0].LogicalSwitchPortUUID = "not-a-uuid" }},
		{"nil UUID", func(d *Document) { d.Endpoints[0].LogicalSwitchPortUUID = "00000000-0000-0000-0000-000000000000" }},
		{"noncanonical UUID", func(d *Document) { d.Endpoints[0].LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000ABC" }},
		{"invalid MAC", func(d *Document) { d.Endpoints[0].MAC = "not-a-mac" }},
		{"noncanonical MAC", func(d *Document) { d.Endpoints[0].MAC = "02:00:00:00:00:0A" }},
		{"invalid IP", func(d *Document) { d.Endpoints[0].IP = "192.0.2.999" }},
		{"noncanonical IP", func(d *Document) { d.Endpoints[0].IP = "192.000.2.10" }},
		{"surrounding identity whitespace", func(d *Document) { d.Endpoints[0].EndpointID = " endpoint-123" }},
		{"duplicate endpoint", func(d *Document) { d.Endpoints = append(d.Endpoints, d.Endpoints[0]) }},
		{"conflicting attachment generation", func(d *Document) {
			other := d.Endpoints[0]
			other.EndpointID = "endpoint-456"
			other.AttachmentGeneration++
			other.LogicalSwitchPort = "lsp-456"
			other.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000456"
			d.Endpoints = append(d.Endpoints, other)
		}},
		{"conflicting network association", func(d *Document) {
			other := d.Endpoints[0]
			other.NetworkID = "net-b"
			other.AttachmentID = "attachment-789"
			other.LogicalSwitchPort = "lsp-789"
			other.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000789"
			d.Endpoints = append(d.Endpoints, other)
		}},
		{"duplicate LSP UUID", func(d *Document) {
			other := d.Endpoints[0]
			other.EndpointID = "endpoint-456"
			other.AttachmentID = "attachment-456-b"
			other.LogicalSwitchPort = "lsp-456"
			d.Endpoints = append(d.Endpoints, other)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			test.change(&document)
			if _, err := NewSnapshot(document); err == nil {
				t.Fatal("invalid document was accepted")
			}
		})
	}
}

func TestLifecycleVersionRequiresAndCanonicalizesTombstones(t *testing.T) {
	old := validDocument().Endpoints[0]
	newer := old
	newer.AttachmentID = "attachment-789"
	newer.AttachmentGeneration = 5
	newer.LogicalSwitchPort = "lsp-789"
	newer.LogicalSwitchPortUUID = "00000000-0000-4000-8000-000000000789"
	newer.MAC = "02:00:00:00:00:02"
	newer.IP = "192.0.2.11"
	document := validDocument()
	document.Version = LifecycleVersion
	document.Endpoints = []EndpointRecord{newer}
	document.Tombstones = []EndpointTombstone{{Reason: TombstoneReplaced, Record: old}}
	snapshot, err := NewSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version() != LifecycleVersion || len(snapshot.Tombstones()) != 1 ||
		snapshot.Tombstones()[0] != document.Tombstones[0] {
		t.Fatalf("lifecycle policy lost its exact tombstone: %#v", snapshot.Tombstones())
	}
	decoded, err := Decode(snapshot.CanonicalJSON())
	if err != nil || decoded.Digest() != snapshot.Digest() {
		t.Fatalf("canonical lifecycle policy did not round-trip: %v", err)
	}

	document.Tombstones = nil
	if _, err := NewSnapshot(document); err == nil {
		t.Fatal("version-2 policy without explicit tombstone array was accepted")
	}
}

func TestLifecycleVersionRejectsInvalidTombstones(t *testing.T) {
	old := validDocument().Endpoints[0]
	document := validDocument()
	document.Version = LifecycleVersion
	document.Tombstones = []EndpointTombstone{{Reason: TombstoneDeleted, Record: old}}
	tests := []struct {
		name   string
		change func(*Document)
	}{
		{"unknown reason", func(d *Document) { d.Tombstones[0].Reason = "expired" }},
		{"delete conflicts with active attachment", func(d *Document) {}},
		{"replacement does not advance generation", func(d *Document) {
			d.Tombstones[0].Reason = TombstoneReplaced
			d.Endpoints[0].AttachmentGeneration = old.AttachmentGeneration
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := document
			candidate.Endpoints = append([]EndpointRecord{}, document.Endpoints...)
			candidate.Tombstones = append([]EndpointTombstone{}, document.Tombstones...)
			test.change(&candidate)
			if _, err := NewSnapshot(candidate); err == nil {
				t.Fatal("invalid lifecycle tombstone was accepted")
			}
		})
	}
}

func TestDecodeStrictJSON(t *testing.T) {
	valid, err := json.Marshal(validDocument())
	if err != nil {
		t.Fatal(err)
	}
	nullEndpointsDocument := validDocument()
	nullEndpointsDocument.Endpoints = nil
	nullEndpoints, err := json.Marshal(nullEndpointsDocument)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data string
	}{
		{"unknown field", strings.TrimSuffix(string(valid), "}") + `,"unexpected":true}`},
		{"case-mismatched top-level field", strings.Replace(string(valid), `"version":1`, `"Version":1`, 1)},
		{"trailing JSON", string(valid) + ` {}`},
		{"duplicate field", strings.Replace(string(valid), `"revision":12`, `"revision":12,"revision":13`, 1)},
		{"duplicate nested field", strings.Replace(string(valid), `"networkID":"net-a"`, `"networkID":"net-a","networkID":"net-b"`, 1)},
		{"case-mismatched endpoint field", strings.Replace(string(valid), `"networkID":"net-a"`, `"NetworkID":"net-a"`, 1)},
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"malformed", `{"version":`},
		{"null", `null`},
		{"array", `[]`},
		{"null endpoints", string(nullEndpoints)},
		{"unsupported schema", strings.Replace(string(valid), `"version":1`, `"version":99`, 1)},
		{"negative revision", strings.Replace(string(valid), `"revision":12`, `"revision":-1`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode([]byte(test.data)); err == nil {
				t.Fatal("invalid JSON document was accepted")
			}
		})
	}
}

func TestDecodeRejectsOversizedDocument(t *testing.T) {
	if _, err := Decode([]byte(strings.Repeat(" ", MaxDocumentSize+1))); err == nil {
		t.Fatal("oversized document was accepted")
	}
}

func TestSnapshotCanonicalDigestAndImmutableCopies(t *testing.T) {
	first := validDocument()
	secondEndpoint := EndpointRecord{
		NetworkID: "net-b", EndpointID: "endpoint-456", AttachmentID: "attachment-789",
		AttachmentGeneration: 1, LogicalSwitchPort: "lsp-456",
		LogicalSwitchPortUUID: "00000000-0000-4000-8000-000000000456",
		MAC:                   "02:00:00:00:00:02", IP: "2001:db8::2",
	}
	first.Endpoints = append(first.Endpoints, secondEndpoint)
	second := validDocument()
	second.Endpoints = append(second.Endpoints, secondEndpoint)
	second.Endpoints = []EndpointRecord{second.Endpoints[1], second.Endpoints[0]}

	a, err := NewSnapshot(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSnapshot(second)
	if err != nil {
		t.Fatal(err)
	}
	second.Endpoints[0].IP = "2001:db8::99"
	if a.Digest() != b.Digest() || !bytes.Equal(a.CanonicalJSON(), b.CanonicalJSON()) {
		t.Fatal("record order changed canonical document or digest")
	}
	if len(a.Digest()) != 64 || a.PolicyVersion() != (PolicyVersion{Revision: 12, Digest: a.Digest()}) {
		t.Fatalf("invalid policy version: %#v", a.PolicyVersion())
	}

	endpoints := a.Endpoints()
	endpoints[0].IP = "192.0.2.99"
	if a.Matches(endpoints[0]) {
		t.Fatal("mutating returned endpoint slice changed the snapshot")
	}
	canonical := a.CanonicalJSON()
	canonical[0] = ' '
	if bytes.Equal(canonical, a.CanonicalJSON()) {
		t.Fatal("mutating returned canonical bytes changed the snapshot")
	}
}
