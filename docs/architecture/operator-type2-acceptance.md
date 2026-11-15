# Operator-managed EVPN Type-2 acceptance

> **Historical engineering record:** results apply to the stated fixtures
> and source inputs, not automatically to the current tree. Local artifact
> paths are provenance and may be unavailable. For current status and
> reproducible entry points, see the
> [capability matrix](../project/capability-matrix.md) and
> [Testing Guide](../../TESTING_GUIDE.md).

This is a disposable integration suite, not a production-readiness claim.
It uses the actual operator, CNI, Node-Agent, OVN/OVS and independently managed
k8gobgp speaker. It does not replace the specialized CNI, OVSDB, continuity
or Framework suites.

## Implementation and component boundaries

```text
kubelet -> Multus -> evpn-cni ADD/CHECK/DEL
                         |
                    Unix gRPC
                         |
              operator-managed Node-Agent
                         |
                OVN NB logical port + OVS attachment
                         |
          OVN native dynamic routing / Linux FDB and neighbors
                         |
          actual Node-Agent ovn-netlink endpoint observer
                         |
         real Type-2 AddPath / UUID-scoped withdrawal
                         |
              direct GoBGP Unix socket
                         |
           independent k8gobgp-managed gobgpd
                         |
                 MP-BGP EVPN session
                         |
           independent FRR/Linux VXLAN endpoint

Reverse control plane:
FRR -> GoBGP RIB -> actual Node-Agent importer
    -> receipt-owned Linux FDB/neighbors -> OVN native dynamic routing
    -> OVS OpenFlow/VXLAN -> CNI Pod
```

The remote dataplane is **FRR/Linux, not a second OVN installation**.
This historical one-chassis acceptance introduces no multi-node EvpnCore
behavior. It has one control-plane Node and one worker; both operator-managed
OVN/OVS and Node-Agent are pinned to that worker through `spec.dataplaneNode`.

| Responsibility | Implementation |
|---|---|
| Kubernetes lifecycle, journal, Lease, finalizer | `deploy/operator/controllers/evpncore_controller.go`, `lifecycle.go`, `journal.go`, `snapshot.go` |
| YAML and agent scheduling/configuration | `deploy/operator/core-resources.yaml`, `controllers/manifest.go`, `placement.go` |
| Real CNI ADD/DEL/CHECK and Unix RPC | `cmd/evpn-cni/main.go`, `pkg/nodeagent/server.go` |
| OVN/OVS endpoint creation and observation | `pkg/nodeagent/runtime_wiring.go`, `local_netlink_observer.go`, `export_observer.go` |
| Actual export and process identity checks | `pkg/nodeagent/export_runtime.go`, `speaker_continuity_linux.go`, `pkg/evpn/exporter.go`, `type2_gobgp.go` |
| Actual import, durable receipts and owned kernel state | `pkg/nodeagent/remote_runtime.go`, `pkg/evpn/remote_type2.go`, `owned_netlink.go`, `remote_recovery.go`, `pkg/ownership/journal.go` |
| Acceptance setup and assertions | `tools/run-operator-type2-e2e.sh`, `tools/lib/operator-cni-test.sh`, `cni-evpn-frr-interop-test.sh`, `pkg/evpn/operator_type2_e2e_test.go` |

The operator does not write BGPConfiguration or manage the speaker.
The suite installs pinned upstream k8gobgp independently and checks that its
BGPConfiguration UID, generation and spec remain unchanged across recovery.
The Node-Agent's exporter/importer use
`unix:///var/run/gobgp/gobgp.sock`, mounted from the worker's
`/run/gobgp-export-test`. Linux peer credentials, executable identity,
process start ticks and boot ID are recorded. The agent requires `hostPID: true` to validate the actual `gobgpd` process.
Its authenticated transport also checks administrator-pinned executable/image,
Node/DaemonSet/Pod/container identities and independently queried containerd
process membership. The provider keeps its independent lifecycle.

The compatibility `evpn-mgmt`/advertisement services also run from the
operator's YAML. A separately owned test-only TCP-to-Unix byte-stream proxy
provides their legacy gRPC endpoint. **It neither implements BGP nor writes
BGPConfiguration, and it is not the Node-Agent's export/import path.**

## Topology and mappings

| Item | Value |
|---|---|
| Isolated underlay | `198.18.0.0/24` |
| Local OVN VTEP / speaker | `198.18.0.2` |
| FRR BGP peer / remote RD | `198.18.0.3` / `198.18.0.3:1000` |
| FRR dataplane VTEP | `198.18.0.4` (deliberately different from BGP peer) |
| Local RD / RT / VNI / Ethernet Tag | `198.18.0.2:1000` / `64512:1000` / `1000` / `0` |
| Wire VXLAN / native synchronization UDP ports | `4789` / `60100` |
| Native synchronization devices | `evpnbr33`, `evpnvx33`, `evpnad33` |
| CNI endpoints A/B | `10.244.240.10`, `10.244.240.11` |
| Local MACs A/B | `0a:58:f0:00:00:0a`, `0a:58:f0:00:00:0b` |
| Remote endpoint / MAC | `10.244.240.12` / `0a:58:f0:01:00:0c` |

The fixture creates the **network-level** switch, native synchronization
devices, remote Linux namespace/VXLAN and BUM replication configuration.
It does not directly create local endpoint logical ports, final imported
unicast forwarding entries, or simulated GoBGP routes. Local endpoint
creation/removal uses actual Multus/kubelet CNI calls; explicit idempotency
and failure replays invoke the same real CNI binary with recorded identity.
The exporter emits MAC-only and MAC/IP paths for each local endpoint;
the importer coalesces the remote MAC-only and MAC/IP paths into one owned
endpoint.

## Reproduce

From the repository root, on an isolated Linux test host:

```bash
bash tools/run-operator-type2-e2e.sh
```

For an additional acknowledged-state forced-crash run:

```bash
EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH=1 bash tools/run-operator-type2-e2e.sh
```

This replaces only the live-endpoint agent restart lane with SIGKILL of the
exact running container task, after checking its container ID and executable.
Kubernetes must restart that container before the normal receipt, identity,
authoritative observation, CHECK and packet assertions can pass. It does not
inject an ambiguous AddPath mutation or bypass the journal. Ordinary runs
continue to use DaemonSet rollout replacement.

Prerequisites are the existing [Containerlab setup](../../deploy/containerlab/README.md)
and [runtime image inputs](runtime-dependencies.md#reproducibility-boundary):
Docker, kind, kubectl, Go, Python 3, curl, Containerlab 0.79.0, passwordless
sudo for scoped network captures, and the verified local OVN/app/CNI-base
images. The suite pins upstream FRR, k8gobgp and Multus inputs and verifies
their established identities/checksums. It builds the actual binaries from
the current working tree, records HEAD, dirty status and binary hashes, and
never commits or changes tags. Historical local image IDs are not a promise
of a public reproducible image build.

Provider and CNI attestation require a root-owned, hash-pinned **static**
`ctr` inspector. The fixture records the worker's containerd version and
builds its matching containerd v2 module with `CGO_ENABLED=0` and
`-mod=readonly`, recording module checksum provenance in `ctr-source.json`.
It does not copy the dynamically linked inspector from the kind image or
relax executable verification to accommodate that binary.

All infrastructure uses the shared Containerlab coordinator and its
exclusive lock, not a second kind provisioner. Run-specific cluster and
image names prevent collisions. Teardown uses the coordinator's resource
ownership records and removes only the test-owned image reference.
Raw evidence contains administrative kubeconfig/CNI credentials: the
run directory is mode 0700, must remain private, and must not be committed.

The runner writes each attempt to a private per-run evidence directory.
`EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT` overrides the base directory.

`result.json` and `cases.tsv` distinguish PASS, FAIL, BLOCKED and NOT TESTED.
Exit 0 requires all cases PASS and successful cleanup. An assertion/setup
failure exits nonzero; an otherwise completed suite with unsupported restart
convergence exits **3**, with overall **BLOCKED**. Blocked requirements are
not silently skipped or treated as full acceptance.

## Assertions and evidence

Database outage assertions accept either endpoint-observer failure or the
specific earlier recovery-domain Logical_Switch deadline. They require a
fresh failed-closed status, identical complete acknowledged receipts and
speaker identity, and an independent read-only wire snapshot with exact route
attributes and duplicate counts. A failed observation is never interpreted as
an empty RIB. Database processes are resumed even if the outage assertion fails.

The final lifecycle captures current UUID receipts immediately before deletion,
not the initial snapshot that may predate activation or speaker/Pod replacement.
Negative CHECK accepts the explicit missing-durable-active-ACK rejection and
still verifies that nothing was recreated. With creation authority enabled, the
legacy synthetic successful ADD for a nonexistent Pod is intentionally blocked:
the test requires an unchanged journal and no attachment or route. Successful
replacement ADD then uses a real kubelet/containerd/Multus-created Pod. This
does not claim that a fabricated sandbox is an authenticated rollback/retry.

<a id="combined-recovery-validation"></a>
### Historical combined recovery validation

The SO_PEERPIDFD ownership correction passes both complete real suites:

| Run | Evidence access | Result |
|---|---|---|
| Normal agent rollout and speaker replacement | Private historical bundle unavailable | PASS, testExit=0, cleanupExit=0 |
| Additional forced agent SIGKILL and speaker replacement | Private historical bundle unavailable | PASS, testExit=0, cleanupExit=0 |
| Independent operator lifecycle regression | Private historical bundle unavailable | PASS, test-exit=0, cleanup-exit=0 |

Both Type-2 runs pass all 18 reported cases, including authenticated real CNI
activation, actual GoBGP/FRR exchange, bidirectional packet captures, importer
and live-endpoint agent recovery, attested speaker replacement, fresh exporter
UUID receipts, post-replacement authoritative importer observations, BGP/NBDB/
OVSDB outages, withdrawal during failure, final CHECK/DEL, exact UUID withdrawal,
foreign-resource preservation and test-owned lab cleanup.
The forced run records the exact killed container/task and its Kubernetes
restart in `forced-agent-crash.txt` and `forced-agent-replacement.json`.

The confirmed defect and descriptor audit are described in
[CNI peer descriptor ownership](type2-restart-ownership.md#cni-peer-descriptor-ownership).
The historical payload-free causal trace, which is not retained publicly,
recorded:
SO_PEERPIDFD on incoming CNI fd 41 failed with EINVAL at 07:31:06.110027;
CNI cleanup incorrectly closed the active GoBGP fd 0 at 07:31:06.143988.
No provider, RPC deadline, reconnect policy or GoBGP API change is part of
this correction.

The private focused-test bundle is unavailable publicly. It included
subprocess regressions covering error-zero ownership,
an active unrelated Unix connection on fd zero, valid pidfd zero, exactly-once
cleanup and independent concurrent GetBgp RPCs. Root exporter SIGKILL tests
exercise all six add/delete intent/execution/acknowledgement boundaries,
including the deliberately blocked retained ambiguous AddPath.

The earlier expanded root-only Node-Agent failure was a test-fixture defect:
`TestInteropWithEVPNCNIClient` used an arbitrary `/proc/123/ns/net`, which could
refer to the plugin's own namespace. The CNI library correctly rejected it.
Final stabilization replaces that path with a held helper subprocess's new
network namespace, verifies its inode differs from the caller's namespace,
and uses it through ADD/CHECK/DEL. No CNI namespace override or production
validation bypass is used. Helper readiness and lifetime are bounded; cleanup
closes its input and waits for process exit, releasing the anonymous namespace.

**Host prerequisite:** root execution needs permission to create a network
namespace (`CAP_SYS_ADMIN`); non-root execution needs permitted unprivileged
user and network namespaces. Namespace creation failure fails the test with
an explicit prerequisite error, rather than silently skipping it. The test
still invokes the actual CNI CLI against the test server's fake backends;
it does not replace authenticated real-runtime acceptance.

Final stabilization's unfiltered root Node-Agent suite and both root/non-root
interop race regressions were reported passing. Their private evidence bundle
is unavailable publicly.
Environment-gated tests are listed separately in the corresponding
`*-skips.txt` files. Seven live/disposable-runtime Node-Agent tests and
disposable-runtime importer kernel crash tests remain skipped in host-focused
runs, not counted as passes. Actual agent crash/recovery and forwarding are
independently covered by the complete disposable suite above.

Final stabilization changes only the test fixture and related documentation;
production code and real acceptance semantics are unchanged. The two complete
real runs above are therefore retained evidence, not newly rerun E2Es.
The stabilization pass also runs `go test -race -count=1 ./...`,
`go test -count=1 ./...`, `go vet ./...` and the unfiltered compiled root
Node-Agent suite. The full changed-file inventory relative to main HEAD was recorded privately
and is unavailable for independent review.

Retained unacknowledged AddPath, invalid provider identity, foreign ownership,
missing/corrupted journals and fabricated creation requests remain fail-closed.
These results establish the bounded accepted recovery scenarios, not universal
automatic recovery or production readiness.

Commands used for this correction:

```text
go test -race -count=1 ./pkg/nodeagent ./pkg/evpn ./pkg/ownership ./deploy/operator/... ./cmd/evpn-cni ./cmd/evpn-node-agent
go vet ./...
go test -race -c ./pkg/nodeagent -o /tmp/evpn-nodeagent-continuity.test
(cd pkg/nodeagent && sudo /tmp/evpn-nodeagent-continuity.test -test.run 'TestCNI|TestAgentAuthority|TestProvider|TestSpeaker|TestExporterAuthority' -test.v)
go test -race -c ./pkg/evpn -o /tmp/evpn-evpn-root.test
sudo /tmp/evpn-evpn-root.test -test.run 'TestExportRecovery|TestRemoteRecovery' -test.v
sudo /tmp/evpn-evpn-root.test -test.run 'TestImporterRecovery' -test.v
go test -race -c ./pkg/ownership -o /tmp/evpn-journal-root.test
sudo /tmp/evpn-journal-root.test -test.v
bash tools/run-operator-e2e.sh
bash tools/run-operator-type2-e2e.sh
EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH=1 bash tools/run-operator-type2-e2e.sh
```

Compiled root test binaries were installed root:root mode 0755, their hashes
recorded in `test-binary-sha256.txt`, and the task-owned binaries removed.
Go remains 1.25.0. No dependency upgrade, commit, push, merge or tag was made.
The descriptor-correction task changes exactly these paths, preserving earlier
uncommitted work:

```text
pkg/nodeagent/cni_caller_linux.go
pkg/nodeagent/cni_pidfd_ownership_test.go
tools/run-operator-type2-e2e.sh
tools/lib/cni-evpn-frr-interop-test.sh
docs/architecture/type2-restart-ownership.md
docs/architecture/cni-creation-authority.md
docs/architecture/operator-type2-acceptance.md
```

### Historical pre-correction attempts

An earlier retained-implementation attempt passed real CNI identity activation,
exported/imported Type-2, MP-BGP,
bidirectional VXLAN, operator/importer restarts and live-endpoint Node-Agent
restart without CNI replay. Replacement exporter evidence independently shows
RUNNING with four fresh UUIDs and a changed attested speaker incarnation.
Importer observation after replacement did not regain authoritative RIB
convergence before the descriptor correction. Final withdrawal, later
CNI lifecycle/outage lanes and final foreign-resource assertions were not
reached. The subsequent attempt also failed speaker recovery; its unsuccessful
transport-retry experiment was
reverted. See the exact [creation trust model and task inventory](cni-creation-authority.md).

Another historical attempt passed installation ownership and independently
attested importer restart,
but failed live CNI Pod creation. The containerd v2.0.3 creation-time
inspector reported that the new sandbox ID was not yet present in the
`k8s.io` container store. The agent correctly refused unverified endpoint
creation. Cleanup passed. Local exporter, live-endpoint Node-Agent restart,
speaker restart and post-recovery packet/withdrawal assertions were **not
tested** in this attempt; this evidence is not full combined acceptance.

The earlier operator lifecycle suite was also reported passing.
Focused exporter crash/race, provider attestation and CNI continuity tests
did not substitute for the subsequently completed combined acceptance above.

<a id="acceptance-observations"></a>
### Acceptance observations

The suite independently records:

- Operator ownership, installation Lease, exact-node placement, actual
  runtime arguments, workload readiness and independent provider ownership.
- CNI ADD/CHECK/DEL, duplicate ADD, repeated DEL, negative CHECK,
  failed-ADD rollback, replacement endpoint and unaffected survivor.
- Real local/learned Type-2 NLRI, MAC/IP, RD, RT, VNI label, Ethernet Tag,
  next-hop, local origin attributes and duplicate counts, through a read-only
  GoBGP API validator. A successful AddPath alone is not acceptance.
  AddPath UUIDs come from exporter acknowledgements; GoBGP's global ListPath
  output need not include them. Withdrawal additionally checks those receipts.
- Actual importer snapshots, validation and receipt-owned FDB/neighbors.
- Independent importer restart before local CNI endpoints exist: the actual
  operator-managed Node-Agent is replaced while remote FDB/neighbor receipts
  remain live. Its original marker and exact durable receipts must survive,
  and it must obtain fresh authoritative observations and return to RUNNING.
  Subsequent real CNI/packet assertions exercise the recovered import path.
- Established FRR/GoBGP EVPN peering and route evidence in both RIBs.
- ARP/neighbor state, bidirectional ICMP, OVS flow/counter changes and matched
  packets at Pod-facing, worker-underlay, FRR-underlay, FRR-VXLAN and remote
  workload capture points. Wire VXLAN must use the imported VTEP, not merely
  the BGP peer address.
- Local/remote route withdrawal, UUID disappearance, kernel cleanup,
  NB/SB/OVS endpoint cleanup and preservation of foreign sentinel state.
- Operator restart, BGP session shutdown/reconnect, individual NBDB and
  OVSDB process pause/resume, and CNI DEL failure during an NBDB outage
  followed by retry, withdrawal, replacement ADD and packet convergence.

Evidence includes `run.log`, per-case JSON, actual runtime JSON configuration,
CNI invocation receipts, GoBGP/FRR RIB output, OVN/OVS/kernel snapshots,
pcaps and packet verdicts, operator/agent/provider logs, and cleanup results.
Earlier endpoint identity evidence is retained when the outage case replaces
Pod A; the subsequent lifecycle uses its newly captured UID and database
identity.

## Independent importer restart and remaining limitations

The runner enables [journal-backed importer recovery](type2-restart-ownership.md)
using installation and Node UUIDs read from this run's actual API objects.
`importer_restart` is independent of `node_agent_restart` and `speaker_restart`.
It restarts the operator-managed agent with live remote receipts **before**
creating local endpoints, leaving the independently managed provider unchanged.
There is no remote drain, marker deletion, provider reset or CNI replay for
this case. Exact controller/route/generation/device/kernel receipts, unchanged
marker, new-agent logs and a fresh authoritative RUNNING snapshot are checked.
The subsequent bidirectional packet case validates the recovered forwarding path.

The runner now enables separate exporter and CNI journals and independently
attested provider transports. `node_agent_restart` requires unchanged
acknowledged UUID receipts, unchanged speaker incarnation, verified existing
CNI endpoints, no additional ADD invocations, recovered remote imports and
bidirectional forwarding. `speaker_restart` deletes the actual independent
provider Pod and requires a newly attested process/container, authoritative
absence of old local paths, fresh UUIDs, real GoBGP/FRR exchange and forwarding.
Neither case uses a manual drain, marker change, configuration reset or CNI ADD
replay. Provider BGPConfiguration UID/spec/generation must remain unchanged.
Subsequent lifecycle assertions verify actual withdrawal and foreign survivors.

These are bounded safe scenarios, not universal restart recovery.
The [AddPath recovery limitation](type2-restart-ownership.md#addpath-ownership-limitation)
verified that the actual provider fork cannot recover an AddPath UUID lost
before durable acknowledgement. A retained ambiguous AddPath stays blocked.
Focused exporter SIGKILL tests verify both safe recovery and that expected
blocked outcome using a real local GoBGP server; they do not inject fake routes
into the live MP-BGP acceptance provider. Provider identity failures and
foreign-slot conflicts are tested separately and never counted as successful
route adoption.
The legacy marker-only mode still refuses restart adoption. A matching marker
is accepted only alongside valid durable importer receipts and domain evidence.
Ambiguous Netlink mutation outcomes remain blocked even with matching tuples.
See the ownership documentation for explicit legacy migration and administrator
investigation requirements.

Remaining work includes ambiguous-operation administrator tooling, dependency replacement and
long-duration churn testing, reproducible public runtime images, and
operational/security hardening. Privileged/root, host networking, host PID,
host sockets and admin-equivalent EvpnCore authoring remain trust boundaries.
No Type-5, multihoming, DCI, standalone provider, tenant-safe delegation,
general multi-node operator lifecycle or remote OVN acceptance is claimed by
this historical one-chassis record. The separate attempt-8 scope follows.

## Attempt 8: inventory-gated two-worker Type-2 forwarding

**Result: PASS within the exact scenario boundary.** This is a separate
acceptance from the one-chassis lifecycle and recovery runs above. It validates
the initial inventory-gated Type-2 export/import and workload-to-external-peer
forwarding path on two workers. It does not extend the earlier lifecycle
results to this topology.

### Provenance and source identity

| Item | Verified value |
|---|---|
| Scenario / attempt | `operator-inventory-type2-two-worker`, VM attempt 8 |
| Scenario result | `OPERATOR_INVENTORY_TWO_WORKER_PASS`; Type-2 `PASS`; recovery `NOT_TESTED` |
| Guest evidence archive SHA-256 | `69fb968cdf9ab62b712f554ecec68ccc437d0fd7168988f8fb7f8a4de694f7ef` |
| Source archive SHA-256 | `0d05c1ec554f9f6f4fb5e1f9f8c5558b371589df8a7c4b66cd9b98ba25504cc7` |
| Pinned OVN container SHA-256 | `304216c90562dc1c0e2a6a458cab62e13a90a7a2614a52bfa6ce171b689e54fa` |
| Pinned GoBGP executable SHA-256 | `cb1db57e29555aef67a5d6c0bdfb7809243fffc7a8a201a55aeb93a57722a15f` |

The complete guest archive checksum was verified locally. In the historical
pre-edit comparison, all 468 files in the source archive matched the checkout
as it existed at that time, byte-for-byte; the 44 then-current files absent
from the archive were ignored Vagrant runtime artifacts or Python bytecode
caches. That comparison describes only that historical checkout, not the
current integrated revision. The comparison record also
identified the local variable rename `modelConfig` to `ovsdbConfig` in
`cmd/evpn-ovsdb/main.go` as a subsequent non-behavioral source edit at that
time. The archive does not record a Git commit SHA.
The first attempt-8 invocation stopped before scenario execution because the
guest checkout had not been transferred. After transferring the source and
pinned inputs, the same scenario command completed with the PASS result above.

### Verified topology and Type-2 assertions

The two workers were `cni-sec-e9022d22-worker` and
`cni-sec-e9022d22-worker2`, with distinct Kubernetes Node UIDs and OVN chassis.
They shared the OVN NB/SB Service at `10.96.105.156` (ports 6641/6642).
Each worker had an independently attested GoBGP speaker: distinct process IDs
and PID namespaces, router IDs `198.18.30.2` and `198.18.30.3`, and executable
hashes matching the pinned GoBGP binary. Both Node-Agent container
`SO_PEERPIDFD` probes succeeded. Inventory revision 1 was durably accepted on
each worker at sequence 1, with no pending operation and
`LOCAL_FENCES_MATCH`.

Both workers exported their own local MAC/IP Type-2 route:

| Worker | RD | MAC / IP | VNI | Next hop |
|---|---|---|---:|---|
| `cni-sec-e9022d22-worker` | `198.18.30.2:1000` | `02:00:00:00:00:11` / `10.244.240.11` | 1000 | `198.18.30.2` |
| `cni-sec-e9022d22-worker2` | `198.18.30.3:1000` | `02:00:00:00:00:12` / `10.244.240.12` | 1000 | `198.18.30.3` |

Both exports carried route target `64512:1000`.
Each worker's authoritative import snapshot contained the external FRR
Type-2 route with RD `198.18.30.4:1000`, MAC `02:00:00:00:00:30`, IP
`10.244.240.30`, VNI 1000, route target `64512:1000`, and next hop
`198.18.30.4`. Captures and route/flow observations verified the expected
VTEPs and VNI 1000.

The workload on each worker and the external FRR/Linux endpoint passed IPv4
connectivity **3/3 in each direction**. Per-worker observed counter deltas
were:

| Counter | RX | TX |
|---|---:|---:|
| OVS tunnel packets | 6 | 8 |
| Linux VXLAN packets | 14 | 12 |
| Workload interface packets | 15 | 13 |

The remote-Type-2 flow delta was 6 on each worker; observed errors and drops
were zero. These counters and captures apply only to this run and topology.

### Verification boundary

The result demonstrates two distinct Kubernetes workers/chassis, independent
GoBGP speaker identities, durable inventory acceptance on both workers,
Type-2 local export and external route import, and bidirectional forwarding
between each worker workload and the external endpoint. It does **not** test
direct workload-A-to-workload-B forwarding, arbitrary multi-chassis
topologies, or complete Operator runtime health.

The following remain **NOT TESTED** by attempt 8: Node-Agent restart, GoBGP
restart, exact withdrawal lifecycle, adoption/recovery after restart, and
failure convergence. Foreign-resource preservation was not an assertion of
this scenario. Generalized multi-chassis support and production readiness remain
**UNCONFIRMED**. The archived scenario result does not itself establish cleanup
or the execution report's focused-test claims. During the earlier consolidation,
the then-current checkout reran the 68 focused Python tests, focused Go package
tests, Python syntax compilation, shell syntax validation and `git diff --check`;
these historical offline checks are not expanded E2E coverage or validation of
the current integrated revision. In particular, the prior report
states that attempt-8's VM was removed and its dedicated pool was empty while
the task-owned pool and network were retained; this cleanup state is not
independently verified here.
