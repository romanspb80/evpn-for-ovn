# Standalone endpoint authorization snapshot

This page describes the standalone endpoint authorization contract and runtime.
Loading a policy establishes trusted management intent only. It does not prove
that an OVN endpoint exists or is ready; standalone Type-2 export is enabled
only by the explicitly configured G.7b.4 runtime and its full authorization,
OVN-observation, speaker-attestation and ownership-recovery chain.

## Trust boundaries

The versioned contract is in `pkg/endpointauth` and has no Kubernetes, CRI,
OVN, GoBGP, or CMS dependency. The Linux-only loader is in
`pkg/nodeagent`; it reads a root-protected file and exposes only a fully
validated immutable snapshot. The CMS emulator remains a testing/demo
component, not an authorization authority in the framework core.

Standalone advertisement intersects this snapshot with independent OVN NBDB
and SBDB observations through the explicitly configured G.7b.4 Node-Agent
runtime. In particular, LSP external IDs are not an authorization source.
Existing EVPN Type-2 reconciliation and UUID-owned GoBGP recovery remain the
only route mutation and ownership mechanisms.

## Version 1 schema

The JSON document contains:

| Field | Requirement |
|---|---|
| `version` | Exactly `1`. |
| `revision` | Positive integer identifying a policy revision. |
| `installationUID`, `nodeUID`, `localChassis` | Non-empty, trimmed identities defining the local ownership scope. |
| `endpoints` | Zero or more unique authorized attachment records. |

Each endpoint record contains `networkID`, `endpointID`, `attachmentID`,
positive `attachmentGeneration`, `logicalSwitchPort`,
canonical `logicalSwitchPortUUID`, canonical six-byte `mac`, and canonical
`ip`. Exactly one MAC/IP pair is supported. Endpoint identity is unique by
network and endpoint; attachment identity is unique by network and attachment.
The same endpoint or attachment ID may not be ambiguously associated with
multiple networks in one scope, and an LSP UUID may appear only once.

The document rejects unknown and duplicate JSON object keys, including
case-mismatched field names, unsupported versions, malformed or trailing JSON,
missing identities, noncanonical addresses, duplicate identities/generations,
and duplicate LSP UUIDs. The canonical representation sorts endpoint records
by network, endpoint, attachment and generation, then uses the fixed version-1
JSON field order.
The snapshot digest is the lowercase hexadecimal SHA-256 of those canonical
bytes.

## Version 2 lifecycle tombstones

Version 1 remains accepted unchanged. Version 2 adds a required `tombstones`
array; an empty version-2 array must be explicit. Each tombstone contains a
`reason` (`deleted`, `replaced`, or `migrated`) and the complete previous
`EndpointRecord`, pinning its network, endpoint, attachment ID and generation,
LSP name and exact UUID, MAC, and IP. A `replaced` tombstone must accompany a
newer authorized generation for that endpoint. `deleted` and `migrated` records
cannot coexist with an active record for the same endpoint. Policy omission
remains authorization revocation; it is not an OVN deletion.

The root-protected policy publisher is the lifecycle authority. A tombstone
alone is insufficient to withdraw: the cycle also requires complete NB and SB
observations. Deletion requires the old LSP UUID/name and its SB logical port to
be absent. Replacement requires the old UUID to be absent and the new
policy-authorized LSP and binding to match. Migration requires the old LSP to
remain visible with one ready SB binding on a chassis other than the
independently configured local chassis. Contradictions, incomplete reads, a
stale SB binding, or unknown readiness remain uncertain and retain routes.
OVN external IDs are correlation evidence only and cannot create or retire a
tombstone.

Version-1 canonical bytes and digest remain unchanged. Version 2 includes the
sorted tombstone array in its digest, so changing a reason or retired record
conflicts with the same-revision digest. The fencer checks a new tombstone
against the active record and durably retains a compact digest, reason and
retired generation. Existing version-1 fence state is migrated in memory; the
next accepted policy write persists version 2. Generation high-water marks
remain the replay defense after old tombstones are removed from later policies.

The snapshot exposes copies of the endpoint list and canonical bytes. Exact
lookup uses network and endpoint identity; `Matches` compares every record
field, including generation, LSP UUID, MAC and IP. Future observers must still
establish observed endpoint identity and readiness independently.
`Decode` enforces the 1 MiB serialized-document limit. `NewSnapshot` validates
an already-constructed Go value and does not apply a serialized-size limit;
callers handling untrusted bytes must use `Decode`.

## File ownership and update procedure

Configure an explicit absolute path. The file and every parent directory must
be root-owned; the file must be a regular file without multiple hard links,
and neither it nor any parent directory may be group- or world-writable. The
loader rejects symlinked components, non-regular files, absent/empty files,
files larger than 1 MiB, and any document that does not validate completely.
It opens each path component relative to an already-open directory descriptor
with no-follow semantics, then reads and verifies the opened file descriptor.
It does not reopen by pathname or fall back to ordinary path-based file access.

To update the policy, an authorized administrator should create a new file in
the same protected directory, write the complete JSON document, set root
ownership and restrictive permissions (for example `0600`), flush it, and
atomically rename it over the configured path. Do not edit the live file
in-place. A reader sees one opened inode: during atomic replacement it may
read the complete old or new version, never a partial rename. An already-open
old inode may have no directory link after the rename and remains safe to read
through its descriptor. Likewise, if a parent directory is renamed after the
loader opens it, descriptor-relative traversal stays pinned to that directory
inode and can read its complete policy; a later lookup from the configured
path rejects a symlink replacement rather than following it. This relies on
the documented root-owned protected-directory trust boundary and does not
protect against a privileged attacker with unrestricted root control.
The loader allows the opened inode's link count to fall from one to zero, with
the corresponding change in inode change time, when atomic rename removes its
directory link. Other detected metadata changes and invalid or incomplete
contents are rejected. Concurrent in-place mutation is outside the supported
update procedure.

## G.7b.3 durable revision and lifecycle fencing

`endpointauth.Fencer` accepts only a fully validated canonical snapshot whose
serialized representation is within the 1 MiB document limit. It is scoped to
the exact installation and node identities and persists the highest accepted
revision, its canonical SHA-256 digest, and endpoint lifecycle high-water
records before it returns an `AcceptedSnapshot`.

The acceptance rules are strict: a higher revision is accepted transactionally;
the same revision and digest is idempotent; the same revision with another
digest and every lower revision are rejected. Missing, invalid, oversized,
wrong-scope, corrupt, or inaccessible state fails closed. An empty policy is a
valid revocation only at a higher revision; it advances the policy high-water
mark and retires currently active endpoint generations without clearing their
tombstones. It is never a reset.

The Linux Node-Agent opens this fence through
`OpenEndpointAuthorizationFencer` in a dedicated root-owned `0700` directory.
The directory must be distinct from the existing exporter route-receipt
directory. It reuses `pkg/ownership.Journal` for exclusive process locking,
checksummed state, and atomic temp-file/rename/directory-sync persistence, but
uses a stable installation/node scope so the authorization fence survives a
host reboot. The policy digest is mutable authorization state; it is not added
to the exporter ownership-domain configuration digest, and this journal does
not contain or replace route ownership receipts.

For each `(installation, node, network, endpoint)` identity, durable state
retains the highest accepted attachment generation. Generations must increase
across attachment-ID changes for that endpoint. A generation becomes retired
when a strictly newer accepted policy omits the endpoint, or when that policy
replaces it with a higher generation. Version 2 requires an exact `replaced`
tombstone when replacing an active generation. Reappearance at the same or a
lower generation is rejected; a higher generation may be accepted, including
after restart. At the same generation, the complete endpoint record—including the
attachment ID, LSP name and exact LSP UUID, MAC and IP—must remain byte-for-byte
equivalent by its canonical record digest. An LSP UUID replacement therefore
requires a generation increase.

Only one high-water entry per endpoint identity is retained; individual
generation history and full revoked records are compacted into that maximum.
An entry cannot safely be discarded without allowing replay for that identity.
Durable state is capped at 2 MiB; if the irreducible identity set exceeds the
bound, acceptance fails closed rather than deleting tombstones.

The journal first durably records an acceptance intent, then atomically commits
the new high-water state. A snapshot is returned only after the commit succeeds.
A crash before the intent leaves the previous state authoritative. On restart,
a checksummed pending authorization intent is completed before state is
validated and exposed; it was created only after policy validation and before
any snapshot publication. A crash after commit but before publication leaves
the new revision durable, and reloading the same policy is idempotent. Corrupt
state, an incompatible pending operation, or an uncertain persistence error
blocks the live fencer; after a persistence error it must be reopened so the
atomic journal can establish which complete state survived. No eligible
snapshot is returned from a failed write.

`IntersectEndpointAuthorization` now accepts only an opaque
`endpointauth.AcceptedSnapshot`, not a raw policy snapshot. The intersection
returns uncertain for a missing, closed, superseded, or failed accepted
snapshot. A valid newer policy that revokes an endpoint remains an explicit
ineligible decision; this phase does not mutate routes. An accepted token is
invalidated when a newer revision is accepted by the same open fencer.

This authorization-only journal does not change UUID-scoped AddPath receipts,
withdrawal, or ambiguous AddPath recovery blocking. The existing Kubernetes
CNI path is unchanged. G.7b.4 now consumes the accepted fence snapshot in the
explicit standalone exporter runtime below; G.7b.5a verifies durable revision
acceptance and restart recovery in a bounded live smoke.

## G.7b.2 authorization intersection

The standalone-specific intersection in `pkg/nodeagent/ovnexport` treats the
policy snapshot and OVN state as separate evidence. It captures one
`PolicyVersion` per evaluation cycle and requires trusted local
installation/node/chassis identity plus a trusted enabled
network-to-logical-switch mapping. The caller must establish the local chassis
independently; repeating a chassis value in OVN metadata is not sufficient.

An eligible decision requires the exact policy network, endpoint and
attachment IDs and generation; the actual NB LSP row UUID and name; the
authoritative logical-switch membership; one canonical MAC/IP pair; and one
SB Port_Binding row with an actual UUID, matching logical-port name, `Up ==
true`, and the independently established local chassis. Endpoint and
attachment external IDs, including
`evpn.framework/attachment-generation`, are correlation data only. They are
checked against the authorization record but never grant authorization by
themselves. NB and SB row UUIDs must come from their actual database rows.

Decisions are `eligible`, `ineligible`, or `uncertain`. A complete observation
that confirms a policy revocation, replacement LSP UUID, binding on another
chassis, down binding, switch mismatch, or MAC/IP mismatch is ineligible.
Missing readiness (`Up == nil`), read failures, missing Port_Binding, multiple
or conflicting rows, incomplete membership/address evidence, and a missing
LSP without deletion proof are uncertain. In particular, an SB binding that
outlives its NB LSP is not treated as endpoint deletion. Version-2 lifecycle
tombstones provide trusted lifecycle evidence; absence without a matching
tombstone remains uncertain. A confirmed tombstone must still agree with
complete NB/SB observations.

The observation adapter must source NB and SB rows from complete successful
reads and flag partial/error results. NB and SB databases do not provide one
cross-database atomic transaction; if row relationships contradict one
another, the caller must mark the cycle incomplete/uncertain rather than
combining observations from different cycles. Database row identity fields
must come from actual rows, not be reconstructed from names or metadata.

A chassis migration is ineligible on the old local node only after a complete
SB observation shows a unique ready binding on the other chassis; unknown
readiness or binding identity remains uncertain. Version 1 accepts exactly one
canonical MAC/IP pair. Multiple or malformed OVN address bindings are
uncertain, never arbitrarily selected.

This intersection returns decisions and candidate `LocalEndpoint` values
only. In G.7b.2 it was not wired to the production exporter, GoBGP, route
ownership, or withdrawal; the G.7b.4 section below documents the later,
explicitly configured standalone runtime integration. The existing
Kubernetes CNI observer and its authorization semantics are unchanged.
Durable revision/generation high-water fencing in G.7b.3 is required by that
runtime.

## G.7b.4 standalone Type-2 export runtime

Standalone export has a separate opt-in entry point:
`--standalone-evpn-export-config=/etc/evpn/standalone-export.json`. Its
root-protected configuration identifies the installation, node and local
chassis and names the existing exporter configuration, authorization policy
file and authorization-fence directory. A representative runtime document is:

```json
{
  "enabled": true,
  "installationUID": "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
  "nodeUID": "46b08606-9b26-4098-90d7-02c14098122b",
  "localChassis": "compute-a",
  "exporterConfigPath": "/etc/evpn/export.json",
  "authorizationPolicyPath": "/etc/evpn/endpoints.json",
  "authorizationFenceDirectory": "/var/lib/evpn/authorization-fence"
}
```

The existing exporter config must contain at least one enabled `framework`
network mapping, the local GoBGP endpoint and ASN/router ID, standalone speaker
attestation, process continuity, and the existing durable exporter route
ownership recovery config. Its route journal directory must differ from the
authorization fence directory. All policy, runtime, attestation and recovery
configuration remains administrator-controlled and protected. Runtime mode
must be `real-ovn-ovs` with `linux-veth`; the CNI continuity path, ordinary
CNI exporter config cannot be combined with this mode. Export-only is the
default. A topology requiring remote EVPN dataplane programming may explicitly
set `"enableRemoteImport": true` in the protected standalone runtime document
and supply the existing `--evpn-remote-config`. The importer must use the same
GoBGP endpoint, ASN/router ID and standalone attestation as the exporter, match
the installation/node identity, and use its own durable Netlink journal,
separate from both the exporter route journal and authorization fence. Each
import network must match an enabled exporter network by network ID, logical
switch and VNI. This enables the existing importer and exporter in one
Node-Agent process; it does not add another BGP speaker, route reconciler, or
ownership journal. Without the explicit flag and importer config together,
co-hosting is rejected. Standalone export does not call `BuildServerForOptions` or
`StartUnixServer`, and therefore does not start the Kubernetes CNI gRPC
service or create its Unix socket. The importer-only mode remains separate.

The CLI owns the importer path through `--evpn-remote-config` and passes that
same path into `RunStandaloneEndpointExporter`. The exporter entry point
revalidates the selection and loads the importer configuration to check the
shared identity, attestation, network mappings and separate recovery journals
before opening the authorization fence. Programmatic callers cannot rely on
prior CLI validation or enable import without supplying a valid configuration.
Focused startup tests cover this boundary and parent cancellation on importer
failure without live networking. Live co-hosted reconciliation and forwarding
remain untested; the export-only smoke below does not establish that coverage.

Startup and each policy reload follow this order: read and validate the
protected policy; require its installation, node and chassis to match the
independently configured standalone identity; durably accept it through the
authorization fencer; establish the OVN observations; attest the existing
GoBGP speaker; then invoke the existing `EndpointExporter` reconciliation.
The fencer recovers a checksummed pending authorization write before accepting
the current file. A recovered revision is not treated as current-file
validation: the policy file is securely reopened and accepted on every cycle
before route reconciliation, including recovery of existing route receipts.
Failure to load, validate, accept or persist a policy skips reconciliation,
so no new advertisement is authorized and prior routes are retained. The next
cycle can accept a repaired higher revision; rollback and same-revision digest
conflict remain rejected. Policy digest/revision are not part of the immutable
exporter ownership-domain digest.

The live adapter now reads the actual NB LSP UUID and switch membership, and
the actual SB Port_Binding UUID, `up` value and chassis reference. It resolves
the chassis UUID to the SB Chassis row name and passes complete NB/SB evidence
to the existing G.7b.2 intersection. Attachment ID and the positive
`evpn.framework/attachment-generation` correlation must already be present on
the observed LSP and match the accepted policy; the runtime does not synthesize
a generation. Missing correlation, missing/unknown readiness, failed or
incomplete reads, contradictory rows, or uncertain accepted-snapshot state
returns an observation error to the existing exporter. The exporter thereby
retains its applied routes and blocks new route changes. A confirmed policy
revocation, down binding, local chassis migration, or authorized replacement
is represented as an ineligible old endpoint; the existing reconciler
withdraws only its exact UUID-owned receipt before applying any replacement.
Foreign GoBGP paths and unresolved AddPath operations retain their existing
protections.

An NB LSP or SB Port_Binding absence by itself still cannot prove endpoint
deletion. Version-2 policy tombstones provide the trusted lifecycle signal,
but complete NB/SB absence (or the specific ready remote-binding evidence for a
migration) is required. A stale SB binding, unknown readiness, or incomplete
table read retains the owned route. Likewise, an endpoint is eligible only when the correlation generation is
provided by the trusted attachment lifecycle and the exact single MAC/IP
binding and LSP UUID match. Type-3/Type-5 and standalone runtime handoff are not
enabled. The opt-in standalone Type-2 exporter runtime and its live smoke test
are described below. Importer/exporter co-hosting is an explicit opt-in
described above and remains limited to the existing validated remote Type-2
import and Linux FDB/neighbor reconciliation behavior.

The runtime remains intentionally opt-in and limited to framework-observed
Type-2 export. Its CNI, importer-only, speaker attestation, GoBGP route
ownership and recovery paths continue to use their existing implementations;
there is no second exporter or route journal. Operational deployment still
requires provisioned trusted identity, protected policy/config files, distinct
durable journals, the standalone GoBGP service, and an OVN endpoint lifecycle
that publishes generation correlation consistently.

## G.7b.5 deletion and replacement evidence

The contract uses two independent authorities: a higher, durably accepted
version-2 policy revision states the intended lifecycle transition; complete
NB/SB observations establish whether that exact incarnation disappeared, was
replaced, or migrated. A valid policy omission is confirmed authorization
revocation, not proof of OVN deletion. A missing, corrupt, stale, or rejected
policy never becomes an empty desired state.

The fencer persists before intersection. Tombstones must match the active
record's endpoint, attachment generation, attachment ID, and record digest. A
replacement must advance generation and include the exact old record. The
fence journal retains a compact tombstone digest/reason/generation across
restart; even after the policy stops listing a tombstone, the generation
high-water mark prevents replay. The mutable policy digest remains outside the
immutable exporter ownership domain. The existing reconciler remains the sole
route mutator: it withdraws exact owned receipts before replacement
advertisements, preserves foreign paths, and retains routes under unresolved
AddPath state or uncertain observations.

The existing `tools/run-operator-type2-e2e.sh` reuses a real Kubernetes/operator
OVN/OVS chassis, GoBGP/FRR peering, VXLAN forwarding and recovery topology. It is
now also hosts the opt-in G.7b.5a standalone-runtime smoke described below.
Authorization, fencing, revocation, restart and startup without CNI passed;
live deletion/replacement/migration tombstones and multi-chassis forwarding
remain [G.7b.5b](../roadmap/backlog.md#g7b5b).
The existing `tools/e2e/evpn-type2-fixture.py` exercises isolated GoBGP/FRR
routes, not the live OVN/OVS Node-Agent runtime.

## G.7b.5a offline-capable standalone export smoke

The existing operator Type-2 runner now accepts the explicit
`EVPN_STANDALONE_EXPORT_SMOKE=1` mode. It reuses the runner's disposable Kind
worker, operator-managed OVN/OVS chassis, k8gobgp GoBGP speaker, FRR peer and
isolated VXLAN underlay; it does not provision a second topology. The smoke
sequence creates one endpoint through the existing Multus/CNI path, then
disables the operator Node-Agent and runs the same Node-Agent binary inside the
existing `ovn-ovs` Pod container. The pinned GoBGP process is launched beside
it with the same visible process/cgroup context and accessible OVN/OVS sockets;
there is no temporary independent container or harness. It starts with
`--standalone-evpn-export-config`; it does not start the Kubernetes CNI server
or create its Unix socket. It uses the real OVN NB/SB observation,
root-protected policy, authorization fence, GoBGP process attestation and
separate exporter route journal. A runtime that cannot prove the configured
speaker identity fails closed; the runner does not relax those checks.

For strict standalone attestation, the runner temporarily removes the
disposable k8gobgp DaemonSet pod and runs the pinned `gobgpd` and `gobgp`
binaries beside Node-Agent in that existing `ovn-ovs` container, using a protected
explicit GoBGP config. It restores the original DaemonSet before returning to
the operator-managed CNI cleanup path. The regular operator-managed provider
setup and its attestation remain unchanged.

Speaker files and its API use canonical `/run/gobgp` paths and
`unix:///run/gobgp/gobgp.sock`, not a `/var/run` symlink or default TCP target.
Non-interactive `kubectl exec` has `/dev/null` stdin; intentional `exec -i`
transfers retain streaming. When the installed launcher is Snap, the runner
selects the native kubectl binary from the same package to preserve redirected
evidence output. Recorded process PIDs, exit status/stderr, speaker logs,
neighbor state and Node-Agent startup logs distinguish exec-stream failures
from actual speaker failures and are retained before cleanup.

Run the smoke test through the existing entry point:

```bash
EVPN_STANDALONE_EXPORT_SMOKE=1 \
EVPN_E2E_ARTIFACT_CACHE_DIR="$HOME/.cache/evpn-for-ovn/e2e-artifacts" \
EVPN_E2E_ALLOW_DOWNLOAD=0 \
tools/run-operator-type2-e2e.sh
```

By default, the runner downloads a missing pinned artifact over HTTPS and
verifies its SHA-256 before caching or use. To require a fully pre-populated
offline cache, add `EVPN_E2E_ALLOW_DOWNLOAD=0`. Cache entries are named
`<filename>.<sha256>`:

| Pinned artifact | Cache filename | SHA-256 |
|---|---|---|
| Multus CNI v4.3.1 Linux amd64 archive | `multus-cni_4.3.1_linux_amd64.tar.gz.88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e` | `88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e` |
| k8gobgp v0.2.8 install manifest | `k8gobgp-v0.2.8-install.yaml.f28c6fe7e45b5e535524de09fd13986c6956e112903c960e14f5b1cdeae91008` | `f28c6fe7e45b5e535524de09fd13986c6956e112903c960e14f5b1cdeae91008` |

Populate an offline cache by copying the exact release files under the listed
names, for example:

```bash
cache="$HOME/.cache/evpn-for-ovn/e2e-artifacts"
install -d -m 700 "$cache"
install -m 600 /path/to/multus-cni_4.3.1_linux_amd64.tar.gz \
  "$cache/multus-cni_4.3.1_linux_amd64.tar.gz.88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e"
install -m 600 /path/to/k8gobgp-v0.2.8-install.yaml \
  "$cache/k8gobgp-v0.2.8-install.yaml.f28c6fe7e45b5e535524de09fd13986c6956e112903c960e14f5b1cdeae91008"
```

An incorrect cached file is rejected rather than overwritten or silently
redownloaded. Missing entries with downloads disabled are reported as
unavailable; omit `EVPN_E2E_ALLOW_DOWNLOAD=0` to allow HTTPS downloads of
missing pinned artifacts, which are still SHA-256 verified. The pinned
k8gobgp container image and the existing OVN, FRR, Kind, and base application
images are separate image prerequisites.

The smoke scenario records the actual LSP UUID, SB bindings, logical switch
membership, GoBGP/FRR EVPN RIBs, exact `PathUUID` ownership receipt, authorization
fence revisions, exporter journal, and Node-Agent logs in the attempt evidence
directory printed by the runner. It checks revision-1 advertisement, revision-2
policy revocation, exact owned-route withdrawal while the existing remote FRR
route remains, and Node-Agent restart with no route resurrection. The runner
then restores the original CNI exporter configuration and operator-managed
agent before deleting the test pod through the ordinary CNI path. The whole
Containerlab lab is torn down using its run-owned directory; the smoke code
does not delete host resources outside that disposable lab.

This is a live smoke test, not a second full datapath suite: bidirectional
forwarding remains covered by the existing operator Type-2 E2E. No standalone
smoke PASS should be inferred from unit tests or setup completion. A failed
speaker attestation, missing cached dependency, unavailable image, OVN
observation, or route check remains a failed/blocked live scenario with its
failure evidence retained for diagnosis.

The 2026-11-08 smoke and operator regression bundles were accessible and
independently checked during G.8; both are PASS. See the
[evidence manifest](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a).
This does not certify remote peer authentication, multi-chassis forwarding,
live tombstone migration/deletion, generalized recovery or production use.
