# CNI creation-time authority and two-stage identity

## Evidence and threat model

Containerd v2.0.3 invokes network setup before creating the pause container
and task. Its later CRI READY status is not available while ADD is running.
Waiting for that status inside ADD deadlocks startup. Pod UID, sandbox ID,
namespace path and CNI_ARGS in a protobuf request are claims, not authority.

Creation admission therefore requires the kernel identity of the **actual**
Unix RPC caller, not a separate CNI socket probe or caller-supplied metadata.
The implemented authority verifies root credentials and pidfd-backed process
incarnations for the exact chain:

```text
authenticated containerd CRI Unix peer -> pinned Multus -> pinned evpn-cni
                                                              |
                                                     actual Node-Agent RPC
```

Each executable hash is administrator-pinned independently of its name.
The containerd ancestor must be the actual process serving the trusted,
root-private runtime socket, not another process with the same executable.
Caller environment must bind ADD, sandbox ID, interface, namespace and Pod
identity to the request. Selected environment fields are read from the
authenticated process; arbitrary RPC metadata is never accepted as evidence.
The network namespace must be a held Linux network-namespace descriptor,
with stable device/inode and a trusted root-owned path.

The trust boundary includes host root administrators, kernel, Kubernetes
API, kubelet, containerd and pinned CNI executables. It excludes arbitrary
RPC clients, forged request claims, unrelated executions of the same binary,
stale process IDs, namespace replacement and residual matching dataplane
tuples. It does not protect against compromised host root/kernel/runtime.
Root socket permissions alone do not establish a CNI invocation.

Direct Kubernetes GET must independently confirm Pod UID/name/namespace,
Node UID and placement before mutation and before returning ADD. A namespace
descriptor proves namespace identity, not workload ownership; the authenticated
runtime invocation supplies creation authority. Later READY sandbox identity
supplies activation authority. Runtime-specific verification remains behind
the existing workload-attestor interface.

An optional shell wrapper is supported only with an independently pinned
shell executable, exact script path and script SHA256. The script is opened
through the pinned process's kernel root directory, using root-owned,
non-writable, no-symlink paths. An arbitrary shell hop is never accepted.
This covers ordinary instrumented CNI installations without making evidence
capture a privileged bypass. The real CNI executable holds the namespace
descriptor throughout its RPC; the server correlates that actual descriptor
with the mounted namespace identity.
The opt-in CNI configuration field `creationIdentity: true` enables descriptor
retention. A continuity-enabled server independently requires that descriptor;
turning the client field off does not bypass admission. Legacy servers and
in-memory CNI fixtures retain their prior behavior.

The fixture explicitly installs the CNI binary, Multus and wrapper as
root-owned files. Copying user-built artifacts into a container does not
necessarily change their ownership. Matching hashes on UID-1000 executables
were correctly rejected in real acceptance; changing their installed owner
is provisioning, not an identity-check bypass.

## Durable lifecycle

| State | Authority and allowed actions |
|---|---|
| PENDING_CREATION | Authenticated creation invocation, direct Pod verification, durable mutation intent, exact acknowledged LSP/OVS/veth and namespace receipts. ADD may return; BGP export is prohibited. |
| VERIFIED_ACTIVE | Direct READY runtime sandbox identity agrees with the original sandbox/Pod, namespace and exact resource receipts. Export and CHECK may proceed. |
| DELETING | Durable exact-receipt deletion intent; serialized with ADD, verification and export. |
| REMOVED | Independently confirmed resource absence and durable deletion acknowledgement. |
| QUARANTINED | Publication deadline expired or conflicting identity evidence; retain ownership receipts, prohibit export and adoption. Exact owned cleanup remains possible. |

Existing acknowledged active/deleted journal records retain their meaning.
An unresolved external mutation intent remains blocked; a pending endpoint
with **acknowledged** resource receipts is different from an unacknowledged
mutation. Restart may resume runtime verification only for those receipts.
It never reruns Ensure to recreate resources.

Verification runs once per second, with a ten-second pass deadline and a
two-minute persisted sandbox-publication deadline. It runs under the same
operation mutex as ADD/CHECK/DEL and exporter gates.
Unavailable runtime observations are not empty state or successful verification.
No CHECK invocation is required to activate an endpoint. A deleted Pod is
never recreated. Duplicate ADD must match the original immutable request and
resource identities; DEL uses acknowledged UUIDs and device identity rather
than caller claims. Failed or uncertain external mutations preserve durable
intent and require investigation.

This design was recorded before implementation. Real operator-managed
acceptance subsequently demonstrated authenticated creation, durable
pending-to-active transitions, Type-2 exchange and bidirectional forwarding.
Focused tests independently verify that pending endpoints cannot export.
If the invocation chain cannot be attested, creation remains blocked rather
than substituting metadata-only authority.

## Configuration and operational limitations

Set `Admission` in the root-private CNI continuity JSON to independently
provisioned SHA256 pins for `CNIExecutableSHA256`,
`MultusExecutableSHA256` and `RuntimeExecutableSHA256`. When a shell wrapper
is present, also set `WrapperExecutableSHA256`, `WrapperScriptSHA256` and
`WrapperScriptPath`. All executables and wrapper paths must satisfy the
root-owned, non-writable identity checks. The CNI network configuration must
enable `creationIdentity: true`. Admission configuration participates in the
ownership-domain digest; changing pins requires an explicit domain migration,
not silent reuse of receipts.

Activation retains direct READY runtime verification behind
`CNIWorkloadAttestor`; containerd-specific inspection does not define the core
pending endpoint ownership model. Additional runtimes are not implemented.
No active endpoint is restored by rerunning CNI ADD or recreating resources.
An unresolved external mutation, missing journal, identity/domain conflict or
corrupted metadata blocks recovery. Quarantine preserves exact receipts for
explicit cleanup; it does not destroy potentially foreign state.
An expired pending endpoint does not automatically become active later.
Administrators must investigate and explicitly remove/quarantine-clean the
acknowledged endpoint before retrying workload creation. One pending or
quarantined endpoint currently conservatively blocks desired-export validation
for the installation, rather than exporting a partially verified set.

<a id="creation-task-acceptance-and-subsequent-descriptor-correction"></a>
## Historical recovery investigation

An earlier combined test reached real endpoint activation, forwarding and
agent recovery but failed to recover authoritative importer observation after
speaker replacement; subsequent lifecycle and cleanup assertions were not
reached. A transport-retry experiment did not resolve the problem and was
reverted. The later investigation traced `GetBgp: DeadlineExceeded` to CNI
`SO_PEERPIDFD` error cleanup closing an unrelated GoBGP descriptor. See
[descriptor ownership](type2-restart-ownership.md#cni-peer-descriptor-ownership)
and the [historical combined acceptance record](operator-type2-acceptance.md#combined-recovery-validation)
for the correction, subsequent reported results and their limits.
Neither replacement identity nor an unavailable RIB proves old routes absent;
ambiguous AddPath recovery remains blocked. Historical private artifacts are
not current-revision acceptance.

<a id="creation-task-file-inventory"></a>
