# Operator-managed EVPN Type-2 acceptance

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
No multi-node EvpnCore behavior is introduced. There is one control-plane
Node and one worker; both operator-managed OVN/OVS and Node-Agent are pinned
to that worker through `spec.dataplaneNode`.

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

The compatibility `evpn-mgmt`/advertisement applications also run from the
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

Default evidence root: `/tmp/evpn-operator-type2-e2e/attempt-<UTC>-<random>`.
Override only the root with `EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT`.

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

### Current Phase D validation boundary

The SO_PEERPIDFD ownership correction passes both complete real suites:

| Run | Evidence directory | Result |
|---|---|---|
| Normal agent rollout and speaker replacement | `/tmp/evpn-operator-type2-e2e/attempt-20261106T095441Z-4497c78a` | PASS, testExit=0, cleanupExit=0 |
| Additional forced agent SIGKILL and speaker replacement | `/tmp/evpn-operator-type2-e2e/attempt-20261106T101056Z-77e1d7c7` | PASS, testExit=0, cleanupExit=0 |
| Independent operator lifecycle regression | `/tmp/evpn-operator-e2e/attempt-20261106T092145Z-7fd6e2ca` | PASS, test-exit=0, cleanup-exit=0 |

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
The payload-free causal trace is
`/tmp/evpn-rpc-boundary-20261106T0712/replacement-fd.strace`:
SO_PEERPIDFD on incoming CNI fd 41 failed with EINVAL at 07:31:06.110027;
CNI cleanup incorrectly closed the active GoBGP fd 0 at 07:31:06.143988.
No provider, RPC deadline, reconnect policy or GoBGP API change is part of
this correction.

Focused evidence is in `/tmp/evpn-pidfd-fix-20261106/`:
`race.log`, `vet-all.log`, `root.log`, `root-continuity.log`,
`receipts-root.log`, `importer-root.log`, `journal-root.log`,
`operator-lifecycle.log`, `type2-acceptance.log` and
`type2-crash-acceptance.log`. Subprocess regressions cover error-zero ownership,
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
interop race regressions pass. Evidence is in
`/tmp/evpn-phase-d-stabilization-20261106/`: `interop-nonroot.log`,
`interop-root.log`, `nodeagent-root.log`, `race.jsonl`, `all-tests.jsonl`,
`vet.log`, `evpn-root.log` and `ownership-root.log`.
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
Node-Agent suite. The full changed-file inventory relative to main HEAD is
`/tmp/evpn-phase-d-stabilization-20261106/changed-files.txt` (87 files,
including preserved earlier uncommitted changes).

Retained unacknowledged AddPath, invalid provider identity, foreign ownership,
missing/corrupted journals and fabricated creation requests remain fail-closed.
These results establish the bounded accepted recovery scenarios, not universal
automatic recovery or production readiness.

Commands used for this correction:

```text
go test -race -count=1 ./pkg/nodeagent ./pkg/evpn ./pkg/ownership ./deploy/operator/... ./cmd/evpn-cni ./cmd/evpn-node-agent
go vet ./...
go test -race -c ./pkg/nodeagent -o /tmp/evpn-pidfd-nodeagent-continuity-20261106.test
(cd pkg/nodeagent && sudo /tmp/evpn-pidfd-nodeagent-continuity-20261106.test -test.run 'TestCNI|TestAgentAuthority|TestProvider|TestSpeaker|TestExporterAuthority' -test.v)
go test -race -c ./pkg/evpn -o /tmp/evpn-pidfd-evpn-root-20261106.test
sudo /tmp/evpn-pidfd-evpn-root-20261106.test -test.run 'TestExportRecovery|TestRemoteRecovery' -test.v
sudo /tmp/evpn-pidfd-evpn-root-20261106.test -test.run 'TestImporterRecovery' -test.v
go test -race -c ./pkg/ownership -o /tmp/evpn-pidfd-journal-root-20261106.test
sudo /tmp/evpn-pidfd-journal-root-20261106.test -test.v
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

The earlier retained-implementation attempt
`/tmp/evpn-operator-type2-e2e/attempt-20261105T225857Z-578a9f97`
passed real CNI identity activation, exported/imported Type-2, MP-BGP,
bidirectional VXLAN, operator/importer restarts and live-endpoint Node-Agent
restart without CNI replay. Replacement exporter evidence independently shows
RUNNING with four fresh UUIDs and a changed attested speaker incarnation.
Importer observation after replacement did not regain authoritative RIB
convergence before the descriptor correction. Final withdrawal, later
CNI lifecycle/outage lanes and final foreign-resource assertions were not
reached. The subsequent attempt
`/tmp/evpn-operator-type2-e2e/attempt-20261105T230718Z-2eb2c4c1`
also failed speaker recovery; its unsuccessful transport-retry experiment was
reverted. See the exact [creation trust model and task inventory](cni-creation-authority.md).

Historically, the attempt `/tmp/evpn-operator-type2-e2e/attempt-20261105T213913Z-a7308ffe`
passed installation ownership and independently attested importer restart,
but failed live CNI Pod creation. The containerd v2.0.3 creation-time
inspector reported that the new sandbox ID was not yet present in the
`k8s.io` container store. The agent correctly refused unverified endpoint
creation. Cleanup passed. Local exporter, live-endpoint Node-Agent restart,
speaker restart and post-recovery packet/withdrawal assertions were **not
tested** in this attempt; this evidence is not full Phase D acceptance.

The earlier operator lifecycle suite passed at
`/tmp/evpn-operator-e2e/attempt-20261105T231712Z-44d0ab3c`.
Focused exporter crash/race, provider attestation and CNI continuity tests
did not substitute for the subsequently completed combined acceptance above.

### Phase D file inventory

The following paths were changed for this recovery phase; other dirty paths
belong to preserved earlier operator/Phase B/Phase C work.

```text
TESTING_GUIDE.md
go.mod
go.sum
cmd/evpn-node-agent/main.go
deploy/operator/api/v1alpha1/evpncore_types.go
deploy/operator/controllers/manifest.go
deploy/operator/controllers/provider_attestation_test.go
deploy/operator/core-resources.yaml
deploy/operator/manifests/operator.yaml
deploy/operator/manifests/provider-attestation.yaml
docs/architecture/cni-endpoint-continuity.md
docs/architecture/operator-type2-acceptance.md
docs/architecture/type2-restart-ownership.md
pkg/evpn/export_recovery.go
pkg/evpn/export_recovery_test.go
pkg/evpn/exporter.go
pkg/evpn/remote_type2.go
pkg/evpn/type2_gobgp.go
pkg/evpn/type2_gobgp_test.go
pkg/evpn/type2_lifecycle.go
pkg/nodeagent/cni_continuity.go
pkg/nodeagent/cni_continuity_attestation_test.go
pkg/nodeagent/cni_continuity_cri_linux.go
pkg/nodeagent/cni_continuity_cri_linux_test.go
pkg/nodeagent/cni_continuity_delete.go
pkg/nodeagent/cni_continuity_export.go
pkg/nodeagent/cni_continuity_multus_test.go
pkg/nodeagent/cni_continuity_runtime.go
pkg/nodeagent/cni_continuity_sandbox.go
pkg/nodeagent/cni_continuity_sandbox_test.go
pkg/nodeagent/cni_continuity_test.go
pkg/nodeagent/export_continuity.go
pkg/nodeagent/export_runtime.go
pkg/nodeagent/network_backend.go
pkg/nodeagent/provider_attestation_linux.go
pkg/nodeagent/provider_attestation_linux_test.go
pkg/nodeagent/provider_containerd_linux.go
pkg/nodeagent/provider_transport.go
pkg/nodeagent/remote_runtime.go
pkg/nodeagent/server.go
pkg/nodeagent/speaker_continuity_integration_test.go
pkg/nodeagent/speaker_continuity_linux.go
pkg/nodeagent/speaker_continuity_linux_test.go
tools/run-operator-type2-e2e.sh
```

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
The [Phase D API investigation](type2-restart-ownership.md#phase-d-api-investigation-unresolved-addpath)
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
multi-node operator or remote OVN acceptance is claimed.
