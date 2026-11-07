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

## Creation-task acceptance and subsequent descriptor correction

Historical creation-task evidence: `/tmp/evpn-operator-type2-e2e/attempt-20261105T225857Z-578a9f97`
and `/tmp/evpn-operator-type2-e2e/attempt-20261105T230718Z-2eb2c4c1`.
The latter included a transport-retry experiment that was reverted after it
failed to resolve speaker recovery; it is not evidence for a retained fix.

| Scenario | Outcome |
|---|---|
| Real Pod creation and authenticated pending-to-active transition | PASS |
| Actual exported/imported Type-2, GoBGP-FRR session and bidirectional VXLAN | PASS |
| Live-endpoint Node-Agent restart, retained acknowledged UUIDs, no CNI replay | PASS |
| Independent importer receipt restart and operator restart | PASS |
| Replacement provider attestation and exporter recovery | PASS in retained-implementation attempt; four fresh UUIDs, RUNNING |
| Importer authoritative observation after speaker replacement | Failed in these historical attempts |
| Complete combined speaker-restart acceptance | Incomplete in these historical attempts |
| Retained unacknowledged AddPath ownership adoption | BLOCKED by design |
| Final withdrawal, later CNI lifecycle/outage lanes and final foreign sentinel assertions | NOT TESTED after speaker failure |

The historical `GetBgp: DeadlineExceeded` was subsequently traced to CNI
`SO_PEERPIDFD` error cleanup closing an unrelated GoBGP connection on fd zero.
The helper now transfers ownership only after successful acquisition, retaining
`-1` on error and accepting a successfully acquired descriptor zero. Handshake
identity failures remain fail-closed. No provider, deadline or reconnection
policy change was needed. See [descriptor ownership](type2-restart-ownership.md#cni-peer-descriptor-ownership)
and the [current combined acceptance results](operator-type2-acceptance.md#current-phase-d-validation-boundary).
Neither replacement identity nor an unavailable RIB proves old routes absent;
universal automatic recovery is not claimed.

Final validation commands:

The CLI interop fixture creates an isolated anonymous network namespace.
Root execution needs `CAP_SYS_ADMIN`; non-root execution needs permission for
unprivileged user and network namespaces. Failure to create the namespace is
an explicit test failure, not a skip. See the [testing guide](../../TESTING_GUIDE.md)
and the [final stabilization results](operator-type2-acceptance.md#current-phase-d-validation-boundary).

```text
go test -race -count=1 ./pkg/nodeagent ./pkg/evpn ./pkg/ownership ./deploy/operator/... ./cmd/evpn-cni ./cmd/evpn-node-agent
go vet ./pkg/nodeagent ./pkg/evpn ./pkg/ownership ./deploy/operator/... ./cmd/evpn-cni ./cmd/evpn-node-agent
go test -race -c ./pkg/nodeagent -o /tmp/evpn-cni-creation.test
sudo /tmp/evpn-cni-creation.test -test.run 'TestCNICaller|TestCNITwoStage|TestCNI.*Pending|TestSpeaker|TestProvider' -test.v
bash tools/run-operator-e2e.sh
bash tools/run-operator-type2-e2e.sh
```

The compiled test binary was installed root:root mode 0755 for authority
tests and removed afterward. Race, vet, root identity tests and IDE build
passed; optional live-speaker-only tests without their environment were
explicitly skipped, not counted as passes. Logs are in
`/tmp/evpn-cni-creation-evidence/final-{race,vet,root}.log`.
The final operator lifecycle suite passed, including cleanup, at
`/tmp/evpn-operator-e2e/attempt-20261105T231712Z-44d0ab3c`
with log `/tmp/evpn-cni-creation-evidence/final-operator-lifecycle.log`.
Type-2 logs are `operator-type2-tenth.log` and
`operator-type2-eleventh.log` in that evidence directory.
Evidence remains private and is not committed.

## Creation-task file inventory

Earlier dirty operator/Phase B/Phase C/Phase D files are preserved separately.
This task adds or updates:

```text
cmd/evpn-cni/main.go
cmd/evpn-cni/main_test.go
pkg/nodeagent/cni_caller_linux.go
pkg/nodeagent/cni_caller_linux_test.go
pkg/nodeagent/cni_activation.go
pkg/nodeagent/cni_activation_test.go
pkg/nodeagent/cni_continuity.go
pkg/nodeagent/cni_continuity_runtime.go
pkg/nodeagent/cni_continuity_export.go
pkg/nodeagent/server.go
pkg/nodeagent/server_test.go
pkg/nodeagent/export_runtime.go
tools/lib/operator-cni-test.sh
tools/run-operator-type2-e2e.sh
docs/architecture/cni-creation-authority.md
docs/architecture/cni-endpoint-continuity.md
docs/architecture/type2-restart-ownership.md
docs/architecture/operator-type2-acceptance.md
```
