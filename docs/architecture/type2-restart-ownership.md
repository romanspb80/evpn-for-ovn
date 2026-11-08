# Type-2 restart ownership: implementation status and safety boundary

Automatic importer recovery and narrowly scoped exporter/CNI recovery are
implemented as opt-in runtime modes. Provider transport requires independently
verified Unix-peer evidence plus either Kubernetes/containerd identity or
explicit standalone service pins. Retained ambiguous AddPath outcomes remain
blocked. This is not universal recovery or a
production-readiness claim; see the actual
[operator-managed acceptance results](operator-type2-acceptance.md).

**Bounded Phase D acceptance passes:** the complete normal and additional
forced-agent-crash suites each pass all 18 cases, including authenticated
[two-stage CNI creation](cni-creation-authority.md), speaker replacement,
authoritative importer recovery, post-restart forwarding, final withdrawal,
CHECK/DEL, foreign-resource preservation and cleanup. The GetBgp failure was
traced to CNI SO_PEERPIDFD error cleanup closing an unrelated fd zero; its
ownership fix does not change transport policy or deadlines. Unavailable
observations remain blocked, not empty, and retained ambiguous AddPath outcomes
remain intentionally blocked. Complete automatic recovery is not claimed.

## Process-wide agent fencing

### Provider re-attestation contract

Provider namespace workload creation, privileged host mounts and the Kubernetes
API/runtime are trusted-administrator surfaces. DaemonSet UID and Pod owner
references are checked alongside the actual executable and container/process
identity; owner references or labels alone are not proof of controller
authorship. This does not defend against an administrator permitted to launch
arbitrary provider workloads or replace trusted host state. Full Pod-template
equality is not an attestation requirement: controller rollouts, admission
mutation and API defaulting need an explicit policy before such a restriction
could be added safely.

`nodeagent.NewAttestedSpeakerContinuity(configPath)` returns a guard exposing
`DialContext`, `Check(context.Context)`, `Incarnation() (string, error)`,
`Evidence`, and `Close`. Its dialer must be installed on the **actual** Unix
gRPC transport. Both importer and exporter use the same standalone JSON path
through their `AttestationConfigPath` configuration field. Missing identity
configuration, API/runtime errors, unsupported runtime formats, changed pins,
or ambiguous process association block access.

The supported topology is a single designated node, with hostPID/hostNetwork
for the agent and hostNetwork for the provider (its own PID namespace is supported),
a Linux kernel supporting `SO_PEERPIDFD`, and
containerd 1.7/2.x with a matching **static** `ctr`. The guard obtains
`SO_PEERCRED` and `SO_PEERPIDFD` from the connected socket, hashes `/proc/PID/exe`,
checks all process UIDs, the agent's host namespaces and the peer's host network
namespace, and retains the pidfd. Direct
client-go reads verify the pinned Node UID and DaemonSet UID, Pod owner UID,
placement, immutable image, and running container ID. Independently queried
containerd container metadata and `tasks list`/`tasks ps` must associate that
exact host PID with the same running container, Pod UID/name/namespace and
image; `/proc/PID/ns/pid` must match the independently observed runtime task
root PID's namespace. A gobgpd child of the k8gobgp manager is supported; in
Kubernetes mode, cgroup names are not identity authority.

Read-only dynamic Kubernetes access to `bgp.purelb.io/v1` resource `configs`
also verifies the BGPConfiguration UID, deletion state and SHA256 of canonical
`json.Marshal(spec)`. Both `netlinkImport.enabled` and `netlinkExport.enabled`
must not be true even if a supplied digest pins such a competing configuration.
The configuration namespace, name, UID and digest are required administrator
pins. Normal Kubernetes JSON specs can be hashed with
`json.dumps(spec, sort_keys=True, separators=(',', ':'))` in Python.

The root-owned JSON file and all parent directories must be non-writable by
group/other and contain no symlinks; the file must be single-link and bounded.
Kubernetes-mode example (replace every identity/hash with independently
obtained actual values):

```json
{
  "nodeName": "evpn-lab-control-plane",
  "nodeUID": "NODE-UID",
  "namespace": "k8gobgp-system",
  "daemonSetName": "k8gobgp",
  "daemonSetUID": "DAEMONSET-UID",
  "containerName": "k8gobgp",
  "image": "ghcr.io/purelb/k8gobgp@sha256:IMAGE-HEX",
  "imageDigest": "sha256:IMAGE-HEX",
  "expectedUID": 0,
  "executableSHA256": "GOBGPD-HEX",
  "runtime": "containerd-ctr-v1",
  "runtimeSocket": "/run/containerd/containerd.sock",
  "ctrPath": "/usr/local/libexec/evpn-ctr",
  "ctrSHA256": "CTR-HEX",
  "configNamespace": "k8gobgp-system",
  "configName": "cni-export",
  "configUID": "CONFIG-UID",
  "configDigest": "SPEC-SHA256-HEX"
}
```

Enable `spec.nodeAgent.providerAttestation` in the YAML-driven operator, and
optionally set `providerCTRHostPath` (default `/usr/bin/ctr` on the host).
Administrator provisioning must create `/etc/evpn/provider-attestation` with
the config, and install `deploy/operator/manifests/provider-attestation.yaml`,
adjusting its namespaces, configuration `resourceNames` (default `cni-export`)
and node `resourceNames` to match the installation.
The operator mounts that directory, `/run/containerd`, and the ctr binary
read-only at the paths above, and uses the read-only
`evpn-provider-observer` ServiceAccount with its in-cluster token. This RBAC is
administrator-owned, outside EvpnCore's managed-resource lifecycle.

Standalone speaker attestation is selected explicitly with `mode: "standalone"`
and a `standalone` object; Kubernetes fields cannot be mixed into that mode and
there is no fallback to Kubernetes after an attestation failure. The protected
pin includes installation, node and service UUIDs, expected UID, executable
path and SHA256, exact argv, GoBGP config path and SHA256, ASN, router ID, and
the expected unified cgroup-v2 path (for example
`/system.slice/gobgpd.service`). The actual gRPC transport must still be a
node-local Unix socket: kernel `SO_PEERCRED` and `SO_PEERPIDFD` bind the peer to
the process being checked. Each attestation checks that process's cgroup,
UIDs, executable, arguments and selected root-protected config; the GoBGP API
ASN/router ID are checked only after peer attestation. Process start identity,
service pins and config digest contribute to the speaker incarnation, so a
replacement process cannot inherit old receipts based on its API response.
Standalone mode requires visible host PID/network/cgroup namespaces, unified
cgroup v2, and kernel `SO_PEERPIDFD`; it does not construct Kubernetes or
containerd clients. This is an attestation option for the existing Node-Agent
runtime, not a standalone daemon or deployment topology.

```json
{
  "mode": "standalone",
  "standalone": {
    "installationUID": "<INSTALLATION-UUID>",
    "nodeUID": "<NODE-UUID>",
    "serviceUID": "<SERVICE-UUID>",
    "expectedUID": 0,
    "cgroupPath": "/system.slice/gobgpd.service",
    "executablePath": "/usr/bin/gobgpd",
    "executableSHA256": "<64-lowercase-hex>",
    "arguments": ["/usr/bin/gobgpd", "-f", "/etc/gobgp/gobgpd.yaml"],
    "goBGPConfigPath": "/etc/gobgp/gobgpd.yaml",
    "goBGPConfigSHA256": "<64-lowercase-hex>",
    "asn": 64512,
    "routerID": "192.0.2.1"
  }
}
```

For Kubernetes mode, the containerd socket must be root-owned, mode 0600, with
trusted parent directories. **A read-only socket mount does not restrict runtime RPCs**:
possession of it is containerd administrator authority. Only the three
read-only inspection commands above are invoked locally; no runtime API or
arbitrary command is exposed through agent RPC. The static ctr executable is
root-owned, SHA256-pinned and executed through its already-verified descriptor.
Provisioning this host access is therefore restricted to trusted administrators,
not delegated to untrusted workload/RPC callers.

Call `Check` immediately before and after every authoritative RIB observation
or mutation. `Incarnation` is a mutex-protected, pidfd-checked accessor of the
last attested evidence, **not** a fresh Kubernetes/runtime attestation; callers
must use `Check` before/after their observation. It hashes boot ID, process PID/start ticks/UID/executable
hash, Node/DaemonSet/Pod UIDs, and runtime container ID. A restarted gobgpd in
the same container changes process evidence; a Pod/container replacement also
changes workload evidence. Stable DaemonSet UID permits independently verified
Pod rollouts; replacing the DaemonSet requires administrator repinning.
Close the old gRPC connection, then its old guard; construct a **fresh** guard
for the replacement actual transport. Closed guards cannot be rebound, and
there is deliberately no reset operation capable of blessing old connections
or receipts. Attestation establishes provider identity, never ownership of a
route or resolution of an uncertain AddPath result.

`cmd/evpn-node-agent/main.go` acquires `nodeagent.AcquireAgentAuthority`
immediately after flag parsing, before configuration loading, backend
initialization, CNI socket preparation or Type-2 runtime startup.
The fixed Linux abstract Unix address
`@evpn4ovn-node-agent-authority` is independent of configured socket paths,
installation names and PIDs. A second cooperating executable in the same
dataplane network namespace exits with `RECOVERY_BLOCKED` before touching
the first agent's sockets or backends.

Authority remains held through runtime joins, CNI `GracefulStop` (including
in-flight RPC handlers), backend cleanup and handle closure. A paused or hung
process continues to hold authority; it cannot be replaced merely because
its RPCs time out. SIGKILL releases the kernel-held socket after process
termination. This does not make that process's interrupted mutations safe
to adopt: their ownership evidence still needs independent verification.

The existing exporter abstract-socket authority remains unchanged. It
continues to fence independently started cooperating exporters; the new
agent fence also covers the CNI and importer startup window.

This boundary assumes the supported operator `hostNetwork`/single-node
placement contract. Abstract sockets are network-namespace scoped, not
cluster-wide. It does not fence an unrelated privileged writer, an older
agent that does not acquire this authority, or a program in a different
network namespace accessing the same mounted host state. The persistent
journal's file lock provides an additional exclusion boundary for cooperating
importers sharing the same journal. Kubernetes Pod replacement and Lease
ownership are not fencing.

## CNI peer descriptor ownership

`SO_PEERPIDFD` transfers a descriptor only when its syscall succeeds.
`GetsockoptInt` can return `(0, EINVAL)` without acquiring anything. The CNI
peer helper keeps its invalid sentinel `-1` on error; it never closes that
unowned zero value. A successfully acquired pidfd numbered zero is valid.
On successful handshake the connection wrapper owns the pidfd and closes it
exactly once; intermediate validation failures close only their acquired pidfd.

The earlier implementation overwrote its sentinel with the failed syscall's
zero value and closed an unrelated GoBGP Unix socket. A payload-free syscall
trace established the cross-connection close; gRPC's READY state did not
reflect the out-of-band raw close. This was not evidence of a GoBGP handler
deadlock or insufficient RPC timeout. Provider attestation, RPC deadlines,
transport reconnection policy and UUID withdrawal are unchanged by this fix.

Root-only subprocess regressions inject the failed syscall, preserve an active
Unix connection on fd zero, accept a real pidfd at zero, verify exactly-once
cleanup after descriptor reuse, and run independent GetBgp RPCs concurrently
with repeated failed acquisitions. The descriptor audit also checked speaker
binding, CNI process pidfds and trusted file walkers. Speaker binding returns
before cleanup on acquisition error; file walkers invalidate their raw
descriptor after transferring ownership to `os.File`. No additional equivalent
close-on-error or SCM_RIGHTS handling was found in the Node-Agent audit.

## Durable ownership journal primitive

`pkg/ownership` implements a bounded, versioned root-owned journal.
`pkg/evpn/remote_recovery.go` integrates it with the owned Netlink importer.
Exporter and CNI use separate private journals and resource schemas built on
the same primitive. Runtime status JSON is diagnostic output, never ownership
authority.

A journal domain binds installation UUID, Node UUID, boot UUID, network
namespace identity and a SHA-256 digest of the complete resource mapping.
Those are supplied by the caller; the primitive validates their format and
exact continuity, not Kubernetes provenance. Installation and Node UUIDs must
be supplied through trusted administrator-owned runtime configuration. Recovery
configuration must be a single-link root-owned regular file that is not
group/world writable; the runtime checks the descriptor it actually read. The
acceptance runner obtains them from the actual EvpnCore and Node API objects.
`remote_runtime.go` binds the actual boot/network namespace and hashes the
import policy, network/device mapping and freshly observed OVN switch UUIDs.
An unavailable NBDB observation cannot substitute for an empty mapping.

The directory is private mode 0700. Every path component is opened through
directory-relative descriptors with `O_NOFOLLOW`; non-root or unsafe writable
ancestors are rejected. Root-owned sticky ancestors such as `/tmp` are
allowed, but the final state directory cannot be shared or sticky.
Both lock and journal must be single-link, root-owned regular files mode
0600. The stable `owner.lock` inode uses nonblocking exclusive `flock`.
The lock file is never removed on normal close.

Initialization is permitted only in a newly created directory. An existing
directory with a missing lock or journal is a recovery conflict even if
initialization was requested. This prevents a missing journal from silently
becoming a fresh installation. Wrong domains/versions, corrupted or oversized
records, changed permissions, hardlinks, symlinks and observable lock inode
or directory inode replacement fail closed. Active operations re-open and
validate the directory path before using the held lock, so a renamed or
replaced state directory also permanently quarantines the old handle.

### Intent and acknowledgement protocol

1. `Begin(operation, payload)` writes a new monotonically sequenced intent
   together with the last acknowledged state.
2. Only after Begin succeeds may the caller execute its external operation.
3. After independently verifying that result, `Commit(sequence, state)`
   durably records the acknowledgement and clears the intent.

An outstanding intent prevents another Begin. Reopening retains that intent;
it never converts it to a receipt. Stale acknowledgements are rejected.
Snapshots are defensive copies, not mutable references to journal state.
The SHA-256 envelope detects corruption; it is **not** a signature against
a hostile root administrator.

Every write uses a fresh exclusive temporary file in the same directory,
file fsync, atomic rename, and directory fsync. Directory creation and initial
lock persistence are also synchronized. A persistence error permanently
fences that Journal instance. Reopening may see either the previous record
or the renamed record if interruption preceded directory fsync, but must
not infer external-operation success from that alone. Temporary files left
by process death are never promoted to ownership records.

Atomic rename, kernel-held locks and fsync do **not** provide a transaction
with Netlink or GoBGP. The resource-specific recovery layer must
retain `RECOVERY_BLOCKED` for ambiguous operation outcomes, notably AddPath
accepted before its UUID receipt is durable. Tuple, NLRI or attribute matching
alone is insufficient to resolve those outcomes.

## Importer schema and enablement

The existing remote JSON configuration accepts an optional field:

```json
{
  "Netlink": {
    "Recovery": {
      "Domain": {
        "InstallationUID": "<EvpnCore UID>",
        "NodeUID": "<Kubernetes Node UID>"
      }
    }
  }
}
```

This is a fragment, not a complete runtime configuration. Existing import
policy, explicit exclusive writer, networks, devices and StateDirectory are
still required. The runtime fills boot ID, network namespace and mapping
digest; optional explicitly supplied values must match observations.
No new operator lifecycle ownership, Lease or BGP writer is introduced.
Without `Recovery`, the previous no-restart-adoption behavior is retained.

The state directory contains the original `remote-netlink-owner` and a private
`receipts-v1/` directory holding `owner.lock` and `journal.json`. The generic
journal envelope is version 1; its importer `State.Version` is independently
versioned at 1. Importer state contains:

- Stable random controller UUID and boot/network-namespace target.
- Explicit networks and complete recorded bridge/VXLAN device identities
  (ifindex, kind, master, VNI, local address, UDP port, MAC and alias).
- Last validated desired snapshot, generation and route-source evidence.
- Active or retiring receipts, exact requested and acknowledged/read-back
  FDB/neighbor tuples, ACK/deletion ACK, status and per-route incarnation.
- Latest incarnation counters, including already withdrawn routes.
- Durable quarantine evidence for observed ownership/device continuity loss;
  restarting cannot erase a previously observed conflict.

Each `netlink-add` or `netlink-delete` intent contains the operation object and
the current importer state. The previous acknowledged state remains in the
envelope until Commit. State-only `importer-checkpoint` operations persist
snapshot changes, retirement and completion; they never execute Netlink.
An interrupted checkpoint is conservatively blocked too.

## Mutation and restart ordering

```text
process authority + journal inode lock
    -> VERIFYING_OWNERSHIP
    -> verify domain, original marker, device identities, durable receipts
    -> authoritative kernel dumps (errors are never empty state)
    -> RECONCILING_STATE
    -> two equivalent fresh complete remote RIB observations
    -> withdraw obsolete owned receipts, then apply desired routes
    -> RUNNING

any ambiguous evidence or pending intent -> RECOVERY_BLOCKED
```

Every creation checks that the slot is empty, durably records intent, issues
exclusive `NeighAdd`, requires ACK and exact readback, then durably acknowledges
the receipt. Every deletion requires the current receipt and exact readback
before recording intent and issuing `NeighDel`; ACK and confirmed absence
precede durable acknowledgement. Neighbors are withdrawn before FDB entries.
Authority and the journal lock outlive all operations and kernel handle cleanup.

Recovery never adopts a preexisting tuple from an intent or an unattempted
receipt. A confirmed durable receipt plus unchanged ownership domain/device
identity authorizes checking its exact readback. A missing previously owned
object is recorded as absent without a startup mutation; it may be recreated
only if a fresh authoritative remote snapshot still desires it. A retiring
receipt is completed before a new incarnation can reuse the endpoint slot.
Read-only importer generation, active route generations and maximum incarnation
are restored together; recovered snapshots do not count as fresh observations.
The same trusted controller/marker remains across normal process restarts.

Unknown mutation outcomes stay blocked even when a dump shows an identical
tuple or no tuple. This includes process death after a kernel ACK but before
durable receipt confirmation. A crash after the durable confirmation can
recover partial FDB/neighbor progress and finish reconciliation.

## Legacy compatibility and administrator intervention

A legacy marker without a valid matching journal is **not migrated
automatically**, ignored or deleted. A matching original marker and valid
journal are accepted even though the creating process is dead; a changed,
missing, unsafe or unrelated marker blocks recovery. Initialization writes
durable state before creating the original marker; interruption between those
steps also blocks for administrator review rather than silently replacing it.

For legacy migration, an administrator must stop/fence every writer, inventory
and positively establish ownership of old kernel state, drain it through its
original owner where possible, and verify the configured remote domain is empty
before provisioning a new private state directory and recovery configuration.
Do not delete unknown entries or reuse a marker from a different installation.
Keep old evidence archived outside the new active directory.

Pending intents, corrupted/missing metadata, boot/netns changes, OVN switch UUID
changes, device changes or foreign slot conflicts require investigation.
There is deliberately no command that clears an intent and claims success.

The trust boundary remains exclusive cooperating writers and trusted privileged
administration. Linux does not expose a durable neighbor/FDB ownership CAS or
an unforgeable persistent device incarnation token. Recorded device identity
and in-process link notifications detect observable changes, but cannot prove
that a privileged foreign writer did not replace an object with an identical
tuple during downtime. Such noncooperating privileged mutation is outside this
contract; if it is suspected, do not enable automatic recovery. Cross-boot and
device recreation recovery are unsupported.
The journal is bounded to 4 MiB and retains latest-incarnation tombstones.
There is no online compaction or cross-domain migration in this phase;
exhaustion fails closed rather than discarding ownership evidence.

## Exporter recovery integration

The exporter JSON configuration adds `AttestationConfigPath` and `Recovery`:

```json
{
  "AttestationConfigPath": "/etc/evpn/provider-attestation/provider.json",
  "Recovery": {
    "StateDirectory": "/run/evpn-node-agent/export-receipts-v1",
    "Domain": {
      "InstallationUID": "<EvpnCore UID>",
      "NodeUID": "<Node UID>"
    }
  }
}
```

This fragment supplements the existing explicit export config. Live recovery
requires process continuity and durable CNI continuity. Configuration is
root-owned and non-writable by group/other. The runtime binds actual boot/netns
and a digest of immutable exporter configuration, freshly read logical-switch
UUIDs and the administrator's provider pins. A different domain cannot inherit
receipts. No upgrade/migration of missing metadata is implicit.

The version-1 exporter state stores attested incarnation and acknowledged
receipts containing endpoint identity, exact serialized NLRI/attributes and
AddPath UUID. `export-add`/`export-delete` intents precede the actual RPC.
Successful UUID response, complete RIB readback and unchanged provider evidence
precede durable acknowledgement. Failed writes poison the generic journal;
pending operations remain authoritative uncertainty, never implicit receipts.

Recovery verifies provider identity before/after complete RIB observations.
Within the same verified incarnation, the durable acknowledgement is ownership
authority; exact payload matching is only readback. An exposed differing UUID,
multiple paths at the owned slot or changed payload blocks. The pinned provider
does not expose UUIDs, so a noncooperating privileged writer replacing a path
with identical attributes cannot be detected through a read-only RIB query.
Such a writer is outside the exclusive cooperating-writer contract. Exact
UUID deletion still cannot delete a foreign replacement: a stale UUID fails.
Suspected foreign mutation requires investigation, not adoption.

An absent owned wire route may be retired from the receipt set. A pending add
may be discharged only after complete authoritative absence; any retained local
path at that slot blocks, including a matching payload. A pending deletion can
finish through its original acknowledged UUID within the same incarnation, or
through complete confirmed absence. A replacement speaker must be independently
attested and show absence of every previously owned/intended local slot before
those receipts are retired. Its new process identity alone is insufficient.
Foreign unrelated paths remain outside the receipt set and are preserved;
unclaimed same-slot paths prevent AddPath.

Interrupted state-only exporter checkpoints cannot introduce receipts: replay
validates that every checkpoint claim came from the previous acknowledged state,
then derives the next state again from current authenticated observations.

```text
attested provider + valid journal
    -> complete local RIB observation
    -> restore same-incarnation acknowledged receipts / confirm absence
    -> rebuild desired state from verified existing CNI/OVN/kernel endpoints
    -> withdrawal before advertisement, durable intent/RPC/readback/ACK
    -> RUNNING

retained ambiguous add / invalid domain / missing evidence -> RECOVERY_BLOCKED
```

Graceful shutdown retains acknowledged export receipts when recovery is enabled;
it does not drain live endpoints merely to make restart succeed. The old gRPC
transport is closed before a fresh attestation guard is created. The journal
lock is held until exporter operations have stopped, within the existing
process-wide agent authority.

[CNI continuity](cni-endpoint-continuity.md) runs before CNI socket preparation,
restores only independently verified existing endpoints, and serializes
ADD/CHECK/DEL. Recovered native and CNI desired advertisements must map to
durable LSP/attachment receipts. Snapshot validation is serialized with CNI;
each advertisement revalidates the endpoint and holds that serialization
through AddPath. UUID withdrawal remains available after Pod deletion.

## Phase D API investigation: unresolved AddPath

The initial Phase D investigation stopped at this safety gate. Subsequent
implementation covers the safe lanes above without altering this verified
limitation or changing the provider API.

The repository uses `github.com/osrg/gobgp/v4 v4.5.0`, but the acceptance
provider is the pinned k8gobgp v0.2.8 image
`sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de`.
Its image label `io.purelb.gobgpd.revision` identifies
`purelb/gobgp-netlink` revision
`6b99046fdd0970fac50bc43f588640840798f4f9`. Both implementations were inspected;
the provider must not be assumed to have the library's API behavior.

At that exact provider revision:

- `pkg/server/server.go:3083-3132` installs the path through `addPathList`
  before generating its UUID and recording it in a private `uuidMap`.
- `pkg/server/server.go:3138-3193` provides UUID-scoped deletion but no
  read-only lookup of that map.
- `proto/api/gobgp.proto:938-941` explicitly reserves field 16 and the name
  `uuid` on Path: the field was removed because it was never set on reads.
  AddPathResponse still returns a UUID and DeletePathRequest accepts it.
- `pkg/server/server.go:5884-5918` converts paths for ListPath/WatchEvent
  without the UUID map. Identifiers, route attributes and timestamps are
  not AddPath ownership receipts.

Sources are pinned public upstream references:
[mutation implementation](https://github.com/purelb/gobgp-netlink/blob/6b99046fdd0970fac50bc43f588640840798f4f9/pkg/server/server.go#L3083-L3193)
and [Path schema](https://github.com/purelb/gobgp-netlink/blob/6b99046fdd0970fac50bc43f588640840798f4f9/proto/api/gobgp.proto#L938-L941).

### Unresolvable case under the existing API

```text
durable AddPath intent
    -> speaker accepts route and assigns private UUID
    -> reply is lost, or agent dies before UUID acknowledgement is durable
    -> restarted agent has intent but no trusted deletion handle
```

If the same speaker may retain that route, a complete RIB containing the
expected payload is not ownership evidence. ListPath/WatchEvent cannot recover
the missing UUID; the API cannot accept a caller-chosen idempotency key or
caller-owned AddPath UUID. Reissuing AddPath can replace an unclaimed local path
and generate a different UUID. Deleting by NLRI, family or all-local scope
would violate isolation. Deletion cannot serve as a non-mutating ownership
probe, and guessed UUIDs are not receipts.

Provider re-attestation establishes who the speaker is, not which path was
created by an interrupted operation. A new verified process incarnation alone
also cannot discharge the intent: it still needs complete authoritative RIB
observations and independently established disappearance of the old outcome.
An unavailable RIB is never evidence of absence.

Consequently this case must remain `RECOVERY_BLOCKED`. A missing durable UUID
cannot be reconstructed from matching MAC/IP, RD/RT, VNI or next-hop.
`TestType2GoBGPLostReplies/add` exercises a real local GoBGP server accepting
the request while the test transport discards its reply; the adapter refuses
the subsequent conflicting retry. It is an API regression, not operator
restart or real MP-BGP acceptance.

### What this does and does not prevent

Trusted durable UUID receipts support exact withdrawal within a verified speaker
incarnation. Re-attested replacement and complete old-slot absence permit
narrowly scoped replay. Neither lane resolves a retained ambiguous AddPath or
conflicting ownership evidence.

Resolving an ambiguous AddPath with a still-present matching path requires
additional independently trusted evidence or a separately reviewed provider
API design (for example, an atomic caller-scoped idempotency receipt).
Merely adding a UUID field to ListPath would not prove which unacknowledged
request created an otherwise unclaimed path. No upstream modification, route
tagging scheme, broad deletion or provider reset was introduced here.
Administrator investigation is required for the unresolved case; automatic
marker/journal deletion and CNI replay are not substitutes for ownership proof.

Do not delete the legacy importer marker,
discard a journal, clear a pending intent, reset a speaker or unlink an active
lock as an automatic recovery action. Preserve evidence. Production disposition of ambiguous state
requires administrator investigation.

## Validation commands

```bash
go test -race -count=1 ./pkg/nodeagent ./cmd/evpn-node-agent
go test -count=1 ./pkg/ownership
```

Non-root journal tests explicitly skip production root-owned filesystem
assertions. To execute those assertions, build the test binary as the ordinary
user and run that specific trusted test binary as root:

```bash
go test -race -c -o /tmp/evpn-ownership-phase-b.test ./pkg/ownership
sudo -n /tmp/evpn-ownership-phase-b.test -test.v
go test -race -c -o /tmp/evpn-importer.test ./pkg/evpn
sudo -n env EVPN_OWNED_KERNEL=disposable /tmp/evpn-importer.test \
  -test.run '^TestImporterRecovery' -test.v
```

The journal tests use their own temporary directories. They exercise exclusive
writers, concurrent intents, root ownership/permissions, missing/corrupt state,
foreign domains, changed lock inodes, each write/sync/rename boundary and
abrupt child-process exit. These are process-crash tests, not physical
power-loss or filesystem failure certification. Agent subprocess tests cover
graceful exit, SIGSTOP, SIGKILL, unchanged active CNI socket identity and
authority retention while an RPC handler is draining.
Importer tests additionally use newly allocated Linux namespaces, actual
Netlink FDB/neighbors and SIGKILL before/after creation, deletion and durable
acknowledgement of both objects. Confirmed partial progress recovers; unresolved
outcomes remain blocked. These tests do not alter the host's network namespace
or use simulated kernel entries for the SIGKILL cases.
