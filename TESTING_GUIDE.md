# Testing Guide

The safe baseline is the local Go test/build workflow in the
[Quick Start](QUICK_START.md). This guide covers additional validation paths
that require external infrastructure.
All live commands below require the [experimental lab security boundary](SECURITY.md).
Historical results are scoped private-source records, not fresh validation of
the future public snapshot. Raw private evidence is intentionally not shipped.
Live suites also require the [explicit runtime-image inputs](docs/architecture/runtime-dependencies.md#reproducibility-boundary);
historical local image IDs are not a promise that those images can be pulled
from a public registry or rebuilt byte-for-byte from a mutable Dockerfile.

## Offline fixture provenance

The Python unit suites run without private archive evidence:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/lib -p 'test_*.py'
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/e2e -p 'test_*.py'
```

The incomplete-capture regression generates Ethernet/ICMP and VXLAN packets
using the existing packet-test builders. It omits reverse sequence 2 at the
pod-facing, worker-underlay and FRR-underlay points, while retaining complete
FRR-VXLAN/remote-workload captures and an OVS flow-counter delta of nine.
Its original PARTIAL, missing-packet and counter assertions remain unchanged.
The receipt lifecycle regression generates successful ADD receipts, duplicated
Multus identity fields and matching OVSDB ownership snapshots using the existing
receipt-test builder; UID validation and lifecycle-dispatch assertions remain.
These are synthetic schema fixtures, not copied or sanitized historical bundles
and not evidence of live acceptance. Optional external capture replays and
privileged tests retain their explicit opt-in requirements.

The Go CNI deadline regression checks the gRPC `DeadlineExceeded` status and
empty stdout, not transport-dependent error-description text. `Canceled`,
`Unknown` and other statuses do not satisfy that assertion.

## Cross-version durable recovery helpers

`tools/e2e/cross-version-recovery.py` runs explicit writer and reader modes in
three package-private `_test.go` helpers. Compile `ownership.test`,
`nodeagent.test` and `evpn.test` independently under each compiler:

```bash
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off CGO_ENABLED=1 \
  GOCACHE=/path/to/isolated-cache /absolute/path/to/go \
  test -mod=readonly -race -c -o /path/to/binaries/ownership.test ./pkg/ownership
# Repeat for pkg/nodeagent and pkg/evpn, using separate directories per compiler.
sudo python3 tools/e2e/cross-version-recovery.py \
  --old /root-owned/go125-binaries --new /root-owned/go127-binaries \
  --evidence /path/to/new-evidence-directory
```

The coordinator currently requires Go 1.25.0 and Go 1.27.3. Build both sets of
binaries from the pre-migration harness commit (whose module directive is
Go 1.25.0); Go 1.25.0 cannot build the upgraded module. Install the binaries
as root-owned, non-writable executables; retain their compiler
metadata. It records SHA-256 and verifies each process's actual compiler
version. Its root-private `/tmp` fixtures must reside on an executable
filesystem (not a `noexec` mount). Both `TMPDIR` and `GOTMPDIR` point to the
short fixture root; journals and evidence are separate. Fixtures are removed
in `finally`, including failures; evidence remains.
When running the full privileged package suites, also set `PATH` explicitly
after `sudo` to the selected compiler's `bin` directory: CNI interop tests
invoke a child `go run`, and `sudo`'s default PATH can select another compiler.

Both directions cover real durable journals, checksums, strict decoding,
pending/ACK receipts, overlapping writer locks and abrupt process exits.
CNI admission, OVS/workload observations, Netlink state and GoBGP RIB are
test models. Importer/exporter use production recovery and real journal/wire
serializers, with a separate external-state model persisted between processes.
Ambiguous mutation, UUID replacement, ownership/incarnation mismatch and
quarantine must fail closed without external mutations. These results prove
cross-compiler durable-state behavior, not live kernel, pidfd authentication,
provider attestation, MP-BGP continuity or traffic acceptance. Run the existing
privileged and live suites separately; their skips are not passes.

## Required Containerlab provisioning

### Core Operator lifecycle

```bash
go test ./deploy/operator/... ./pkg/nodeagent ./pkg/evpn
go test -race ./deploy/operator/...
bash tools/run-operator-e2e.sh
```

The operator runner uses the shared `cni` Containerlab profile, deploys the
actual operator with its CRD/RBAC, and configures the existing Node-Agent
through `EvpnCore`. It checks real API-server defaulting/admission, stable
resource versions across resync, workload availability, singleton conflicts,
ordinary namespace-editor denial and explicit administrator authorization,
exact single-node OVN/agent placement, restart/missing-journal recovery,
namespace endpoint round trips, agent updates/disable, cross-namespace
deletion, retained host config and unchanged independent k8gobgp resources.
Go tests additionally inject journal/create/delete outages, generation and
deletion changes, UID replacements, concurrent inventory updates, partial
restores, cleanup conflicts and stale rollout failure conditions.
It requires the existing verified
test-base/OVN/app images and upstream provider release inputs. Evidence is
run-specific under `/tmp/evpn-operator-e2e` by default, with explicit test and
cleanup exit codes. It does not substitute for the Type-2 traffic suites.
The runner supplies a test-only TCP-to-Unix byte-stream proxy for the upstream
Unix-only speaker. This makes compatibility app gRPC dependencies reachable
without adding another GoBGP configuration writer; the proxy remains outside
EvpnCore ownership and is checked for preservation alongside the provider.

### Operator-managed Type-2 acceptance

```bash
bash tools/run-operator-type2-e2e.sh
```

This dedicated suite uses operator-managed OVN/OVS and Node-Agent, actual
Multus/CNI, the direct Unix GoBGP integration and an independent FRR/Linux
VXLAN endpoint. It checks real MP-BGP routes, native import/export, packet
captures, full CNI lifecycle, withdrawal and dependency failures.
See [topology, evidence and recovery limitations](docs/architecture/operator-type2-acceptance.md).
The suite now attempts agent restart with durable acknowledged export/CNI
receipts and provider replacement with independent re-attestation plus
authoritative absence of old local paths. It does not drain routes, clear
markers, reset provider configuration or replay CNI ADD to make recovery pass.
Exit 3 still means unsupported mandatory recovery, not full acceptance.
Existing specialized suites remain separate.
The [restart ownership foundations](docs/architecture/type2-restart-ownership.md)
document process fencing, root-owned journal faults, actual Netlink importer
restart/SIGKILL tests and real local GoBGP exporter SIGKILL tests. Run the
root-owned exporter and CNI assertions explicitly; ordinary non-root tests skip
root-only checks.

The CLI interop test (`TestInteropWithEVPNCNIClient`) uses a held subprocess in
an isolated network namespace, not an arbitrary host PID's namespace. Root
runs require `CAP_SYS_ADMIN` to create that namespace; non-root runs require
permitted unprivileged user and network namespaces (`CLONE_NEWUSER` and
`CLONE_NEWNET`). The fixture fails with an explicit prerequisite error if
namespace creation is denied; it is not silently skipped and does not use a
CNI namespace-validation override. Anonymous namespace lifetime is bounded
by the helper process, which is reaped during test cleanup.

```bash
go test -race -count=1 ./pkg/evpn ./pkg/nodeagent ./pkg/ownership
go test -race -c -o /tmp/evpn-exporter.test ./pkg/evpn
sudo -n /tmp/evpn-exporter.test -test.v -test.run '^TestExportRecovery'
go test -race -c -o /tmp/evpn-nodeagent.test ./pkg/nodeagent
sudo -n /tmp/evpn-nodeagent.test -test.v -test.run '^TestCNI|^TestProvider|^TestSpeaker'
```

A retained AddPath with no durable UUID, unverifiable provider identity and
foreign conflicts are expected fail-closed cases, reported separately from
successful safe recovery. Process-crash coverage is not physical power-loss
certification or production readiness.

**Containerlab is the standard and required environment provisioning mechanism
for all EVPN-for-OVN E2E tests.** It is not required for unit tests, static
analysis, library builds, ordinary Framework deployment or user-managed
Kubernetes. See the [Containerlab guide](deploy/containerlab/README.md) for
Linux/privilege prerequisites, the enforced **0.79.0** version, installation,
profile selection, bootstrap, evidence, scoped teardown and recovery commands.

The common coordinator renders one run-specific topology from shared native
kind configuration. Containerlab owns native kind creation/deletion and Linux
peer lifecycle. The coordinator handles external CA configuration, private
kubeconfig export, the owned worker's underlay attachment, and the standalone
fixture's explicitly owned internal bridge. Kubernetes workloads remain
suite-specific setup, not a second cluster owner.

### Maintained scenario inventory and ownership

| Scenario | Classification and profile | Common environment operations | Suite-specific setup/assertions |
|---|---|---|---|
| Framework Netlink | Maintained E2E; `framework-netlink` | Native two-node kind, FRR container/underlay, PKI, resource receipts, diagnostics and teardown | OVN/OVS and managed k8gobgp deployment; Framework endpoints, Type-2/RD/RT/VNI, Linux integration, traffic/captures, withdrawal and survivors |
| Framework OVSDB | Maintained E2E; `framework-ovsdb` | Same shared kind/FRR lifecycle | Managed RIB import, opt-in OVSDB programmer, localnet/provider bridge, OVS VXLAN, traffic, withdrawal and survivors |
| CNI full lifecycle | Maintained E2E; `cni` with FRR | Same shared kind/FRR lifecycle | Multus/node-agent, ADD/CHECK/DEL, repeats, replacement, negative CHECK, pre-LSP rollback, traffic, withdrawal and survivors |
| Core Operator lifecycle | Lifecycle E2E; `cni` without FRR | Same Containerlab native kind, locking and scoped teardown | Operator-managed Node-Agent, singleton conflicts, API defaults/rejection, spec updates, workload readiness, cross-namespace finalizer cleanup and independent k8gobgp preservation; not Type-2 traffic acceptance |
| Operator-managed Type-2 | Integration acceptance; `cni` with FRR | Same shared lock, native kind/FRR topology and scoped teardown | Real operator/Multus/CNI, direct Unix GoBGP, export/import, MP-BGP, VXLAN packets, full lifecycle and recovery; automatic restart convergence remains blocked |
| Other CNI runner modes | Maintained subsets/diagnostics; `cni`, optional FRR | All modes of `run-cni-mvp-2.4b-multus.sh` use the same common lifecycle | Initial/import/export, no-replacement, provider continuity and binding diagnostics; not substitutes for full lifecycle |
| Standalone Type-2 fixture | Maintained control-plane component fixture; `type2` | Containerlab Linux speakers, run-owned internal bridge, management PKI and teardown; no Compose alternative | Explicit immutable images, standalone gobgpd, FRR EVPN readiness, exact route lifecycle and process-owned endpoint namespace |
| Standalone Linux Netlink acceptance | Implemented component runner, not OVN/OVS E2E; consumes `type2` | Same fixture lifecycle; exact labeled helper-container IDs registered with the common owner | Handler/route-helper builds, Linux state and forwarding assertions; requires a diagnostic-tool-equipped immutable runtime image |
| FRR interoperability | Maintained within the above runners, not another environment owner | Shared FRR profile/fixture | EVPN negotiation and exact route/traffic assertions |
| Shared-VTEP OVN, two-chassis, RR, Type-5, DCI, multi-cluster | Planned or unestablished full-Framework acceptance; **NOT_RUN** | Future suites must use the common Containerlab lifecycle | No new feature or acceptance claim in this infrastructure milestone |
| Historical reproductions and archived reports | Legacy evidence, not maintained provisioning paths | None; do not replay against shared resources | Preserve historical results and terminology |

`tools/bgp-provider-test-supervisor.sh` is a test process inside an owned CNI
environment, not a separate provisioner. Runtime Kubernetes manifests and
Dockerfiles still serve ordinary deployment/builds; Containerlab is not added
to Framework dependencies. No active repository CI pipeline was found.
Temporary namespaces, interfaces, capture processes and route operations stay
inside suite-owned nodes/containers; the common lifecycle owns the laboratory
that contains them. No maintained runner may create an independent Docker,
Compose or kind laboratory outside this model.

All suites/actions serialize through an exclusive lock; a persistent lease
also rejects overlapping retained/interrupted labs. Parallel acceptance is
unsupported because benchmark subnets/VNIs and whole-host inventory checks
would conflict. Unknown IDs, labels, kind members or network attachments
block deletion. Never bypass these guards using global destroy/prune.

Run the infrastructure failure/isolation regression on the same disposable
host, using a fresh output directory:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 tools/e2e/containerlab-regression.py \
  --directory /tmp/evpn-containerlab-isolation-unique
```

It creates its own unrelated witness lab, injects a nonexistent FRR entrypoint
into a separate generated topology, requires actual partial-resource receipts,
then verifies scoped target cleanup, unchanged/responding witness resources,
and scoped witness cleanup. It never uses an existing user lab as a witness.
The output directory retains `report.json` and both exact topologies.

### C4.4f.2q.2 acceptance

The q1 results below predate migration and do not count toward this milestone.
The final q2 matrix must contain two clean Containerlab runs of each primary
suite, with exact topology identity, Containerlab version, runtime versions and
image digests, managed-provider/gRPC evidence, route/traffic/capture evidence,
withdrawal/survivor results, exit status and scoped cleanup. The commands in
the Containerlab guide run this matrix sequentially. Evidence stays outside
the repository; do not publish kubeconfigs, private keys or raw secret bundles.

The final matrix passed on **2026-11-02**, using implementation commit
`56ed56c` after common lifecycle commit `3f4da4a` and runner migration
`9abcaba`. Every cell is a fresh Containerlab environment with **exit 0,
owned cleanup PASS and unchanged before/after Docker/kind inventories**.
The q1 runs and earlier diagnostic attempts are not counted.

| Suite | Run 1 | Run 2 |
|---|---|---|
| Framework Netlink | **PASS** `attempt-20261102T192201Z-a57e1a8e` | **PASS** `attempt-20261102T193223Z-c734fd23` |
| Framework OVSDB | **PASS** `attempt-20261102T192444Z-83756d04` | **PASS** `attempt-20261102T193507Z-393a064f` |
| CNI Full Lifecycle, single chassis | **PASS** `attempt-20261102t192700z-34c3c6bc` | **PASS** `attempt-20261102t193725z-dc386432` |

Historical attempt bundles and console logs remain private. Fresh runs print
their own non-overwriting evidence directories; retain their topology, receipts,
status and captures locally rather than copying private bundles into source.

All six bundles were checked for actual Containerlab 0.79.0, FRR
`10.7.1_git`, managed k8gobgp v0.2.8 and its pinned image, GoBGP
`PureLB-fork:01.03.08 (commit: 6b99046) [base: gobgp-4.9.0]`, OVN 26.09.0
and OVS 4.0.0. Provider process/CRD/config/gRPC evidence and exact image
identities are present. Original Type-2, traffic/capture, withdrawal, survivor
and cleanup gates passed without weakening assertions.

**CNI scope distinction:** `interop-status.json` retains its existing broader
`overall_status=PARTIAL` because `shared_vtep_lifecycle_status=NOT_RUN`.
Its full single-chassis lifecycle, forwarding, packet verification and cleanup
fields all passed. This infrastructure milestone does not relabel that broader
aggregate or establish shared-VTEP acceptance.

Additional infrastructure checks passed:

| Check | Evidence |
|---|---|
| Partial bootstrap and unrelated disposable witness isolation, twice | Historical private reports: target cleanup, unchanged/responding witness and witness cleanup all PASS |
| CNI kind-only variant, native node readiness and Kubernetes v1.32.3 | Historical private report: cleanup PASS and management CA correctly `NOT_USED` |
| Standalone Type-2 full route lifecycle with a registered auxiliary test container | Historical private report: three route identities, withdrawal/survivors, re-advertisement and exact helper/topology cleanup PASS |
| Management PKI and host metadata | Every primary bundle verifies issued Linux-node leaf chains/SANs against the same intended persistent CA; no owned Docker/kind resources, netns symlinks, SSH config or host entries remain |
| Offline validation | Go tests/build, 215 helper tests (3 opt-in skips), 84 fixture/component tests, shell syntax, YAML parsing, all four Containerlab profiles, active Markdown paths, whitespace and targeted generated-artifact/private-key markers PASS |
| Opt-in capture regressions | The three default-skipped tests were also explicitly executed: private-file publication, real bounded tcpdump preflight, and replay of the new CNI PCAP evidence all PASS |

Containerlab management TLS does not enable Framework gRPC TLS. The standalone
Linux Netlink component's full forwarding/shared-state acceptance was **NOT_RUN**
in this milestone; its fixture and auxiliary ownership path were exercised,
not its Framework feature gates. Shared-VTEP OVN, two-chassis, RR, Type-5,
DCI and multi-cluster remain **NOT_RUN**. This baseline accepts E2E
infrastructure, not new Framework capabilities or production installation.

## Dependency upgrade acceptance

The C4.4f.2q.1 source pins official FRR 10.7.1 and upstream k8gobgp v0.2.8.
See the [dependency inventory and ownership audit](docs/architecture/runtime-dependencies.md)
for exact image digests, the release-manifest checksum, actual daemon identity
and CRD/gRPC boundaries. OVN/OVS and the Framework's GoBGP client module were
not upgraded.

**Historical evidence below is not automatically new-version evidence.**
The previously recorded C4.4f.2o and security-preparation regressions used
FRR `8.4_git`; their immutable image and original results remain valid only
for those runs. The isolated suites already used k8gobgp v0.2.8 before this
change; shared bootstrap's mutable installation and the optional 0.2.5
scaffolding references were brought into alignment.

All three current runners deploy the actual upstream k8gobgp manager and
gobgpd together, configure global identity/neighbors/EVPN AF through
BGPConfiguration, and exercise Framework path operations over gRPC. The
standalone Type-2 fixture, which overrides the image entrypoint, **does not**
exercise the manager and is not substitute evidence.

For each new run, retain `provider-runtime.json`, `provider-daemonset.json`,
`provider-pod.json`, `provider-crd.json`, `provider-configs.json`,
`provider-image.json`, `provider-processes.txt`, `speaker-version.txt`,
`speaker-global.txt`, `frr-version.txt`, `frr-image.json`, and the existing
route, packet, withdrawal, survivor and cleanup evidence. Framework runs
also record `dataplane-version.txt`, `dataplane-image.json` and Kubernetes
version; CNI records its existing `runtime.txt` and Kubernetes version.
Runtime output must say FRR `10.7.1_git` and the managed daemon
`PureLB-fork:01.03.08 (commit: 6b99046) [base: gobgp-4.9.0]`, not just match
an image tag. Provider identity checks fail for a standalone daemon,
wrong/unready image, wrong CRD, or competing enabled upstream Netlink writer.

### Verified C4.4f.2q.1 runs

The complete matrix passed on **2026-11-02**, after the final FRR-output
compatibility fix at implementation commit `7f37a5f`. Each cell below is an
independent clean environment with **exit 0 and cleanup PASS**. All six
bundles were checked for the exact FRR/k8gobgp image identities and actual
daemon versions above, Type-2 route evidence and successful original
acceptance gates. No assertion was weakened.

| Suite | Run 1 | Run 2 |
|---|---|---|
| Framework Netlink | **PASS** `attempt-20261102T172837Z-506f297d` | **PASS** `attempt-20261102T173901Z-97e306ac` |
| Framework OVSDB | **PASS** `attempt-20261102T173124Z-a1ab145f` | **PASS** `attempt-20261102T174146Z-b97a4c65` |
| CNI Full Lifecycle | **PASS** `attempt-20261102t172314z-a625f89c` | **PASS** `attempt-20261102t173340z-469ad012` |

Complete historical logs and attempt bundles remain private and are not
required inputs for reproducing the current runners.

FRR neighbor evidence reports **Established** and L2VPN EVPN advertised and
received. RIB evidence contains local `.10`/`.11` and remote `.12` workload
Type-2 routes, expected MACs, VNI 1000, RD, RT `64512:1000` and next hops.
The suites verified bidirectional workload traffic and packet-path evidence,
scoped withdrawal, survivor state/traffic and owned cleanup. CNI additionally
passed ADD/CHECK/DEL, repeated operations, negative CHECK, replacement and
injected pre-LSP rollback. The embedded managed-provider identity/gRPC check
passed in all six runs; no additional standalone-speaker result is counted
as operator coverage.

Earlier failed diagnostic attempts were retained separately and are not
counted. Compatibility fixes supply the release image's missing base/vtysh
configuration, handle snap kubectl output capture, and recognize FRR's changed
advertisement heading while retaining exact route/peer assertions.
The final offline checks also passed: Go tests/build, 202 Python helper tests
(3 skipped), 82 fixture tests, shell syntax, deployment YAML parsing, active
Markdown relative paths, whitespace and targeted tracked-file secret markers.
The marker scan is not a comprehensive security audit.

There are no remaining blockers for this **dependency-upgrade** scope.
Shared-VTEP is still `NOT_RUN`; two-chassis, route-reflector and Type-5
acceptance are not established. The then-destructive shared Containerlab
bootstrap and standalone Type-2 fixture were not live-revalidated by the q1
matrix; the q2 section above tracks their replacement and revalidation.
None of these results declare production or public-release readiness.

Reproduce on the isolated-test host, sequentially, with no global cleanup:

```bash
for run in 1 2; do
  bash tools/run-framework-netlink-e2e.sh || exit "$?"
  bash tools/run-framework-ovsdb-e2e.sh || exit "$?"
  bash tools/run-cni-full-lifecycle-e2e.sh || exit "$?"
done
```

Each runner must complete its original assertions and owned cleanup. A failed
attempt is retained, not counted as either clean successful run. Do not edit
a shell runner while its process is executing: Bash can read later blocks
from the modified file. Use a fresh process after the source is stable.
With snap-packaged kubectl, capture evidence through a pipe (`| tee file`)
rather than inheriting an open host regular-file descriptor.

## Offline validation

From the repository root, with Go 1.27.3 installed:

```bash
go test ./...
go build ./cmd/evpn-cni ./cmd/evpn-node-agent ./cmd/evpn-ovsdb \
  ./services/evpn_mgmt ./services/ovsdb_mgmt ./services/bgp_ad_handler \
  ./deploy/operator
```

Live integration tests are opt-in through test-specific environment
variables. They may change cluster, database, or Linux networking state; use
only the isolated environment required by that test. The default test run
does not opt into those live tests.

## Test inventory

| Test category | Entry point and dependencies | What it asserts | Not covered |
|---|---|---|---|
| Go unit and component tests | `go test ./...`; Go 1.27.3 and module dependencies. Live tests are opt-in. | Route validation/import/export, ownership, reconciliation, management APIs, CNI/node-agent services, and focused component behavior. | A package PASS does not establish live OVN/OVS forwarding or any E2E scope. |
| Python helper and offline integration tests | `python3 -m unittest discover -s tools/lib -p 'test_*.py'`; `python3 -m unittest discover -s tests/e2e -p 'test_*.py'`. | Stubbed runner behavior, evidence parsing, CNI helpers, and fixture configuration. | No live Kubernetes, OVN/OVS, GoBGP, FRR, or workload forwarding unless a separate live runner is explicitly invoked. |
| Offline Framework integration tests | Go integration/component tests under `pkg/evpn` and `pkg/nodeagent`, through `go test ./...`; test-specific environment variables control live scenarios. | In-process or fixture-backed GoBGP/OVN/Netlink/OVSDB interactions and ownership/withdrawal behavior. | Do not imply independent live Framework acceptance without the corresponding E2E runner and evidence. |
| Framework Netlink-Programmed E2E | `tools/run-framework-netlink-e2e.sh`; Containerlab-managed Docker/kind, OVN/OVS, upstream k8gobgp, and external FRR peer. | Independent Framework lifecycle, Type-2 exchange, OVN/OVS forwarding, bidirectional traffic, withdrawal, survivor preservation, and cleanup. | One chassis only; not shared-VTEP, two-chassis GoBGP-to-GoBGP, or Type-5 acceptance. |
| Framework OVSDB-Programmed E2E | `tools/run-framework-ovsdb-e2e.sh`; Containerlab-managed Docker/kind, OVN/OVS, upstream k8gobgp, and external FRR peer. | Independent Framework lifecycle through the opt-in route importer, OVN/OVS programming and forwarding, withdrawal, survivor preservation, and cleanup. | One network and one remote VTEP; not multi-VTEP, two-chassis, shared-VTEP, or production-default acceptance. |
| CNI Full Lifecycle E2E | `tools/run-cni-full-lifecycle-e2e.sh`; Containerlab-managed Docker/kind, Multus, OVN/OVS, upstream k8gobgp, and external FRR peer. | Real CNI ADD/CHECK/DEL and replacement, traffic and packet attribution, idempotency, negative CHECK, injected pre-LSP ADD rollback, survivor preservation, and cleanup. | One chassis; shared-VTEP and broader multi-chassis acceptance are not established. |
| CNI no-replacement interoperability | `tools/run-cni-mvp-2.4b-multus.sh` with `EVPN_FRR_INTEROP_TEST=1 EVPN_IMPORT_TEST=1 EVPN_FRR_INTEROP_NO_REPLACEMENT=1`; Containerlab-managed kind/Multus/OVN/OVS and external FRR. | Supported no-replacement one-chassis CNI/Framework subset and cleanup. | It does not substitute for the full lifecycle, replacement, shared-VTEP, or two-chassis acceptance. |

## E2E acceptance categories

Keep the following suites and their results separate:

| Category | Scope | Current evidence status |
|---|---|---|
| CNI no-replacement interoperability | CNI endpoint lifecycle, optional node-agent Type-2 import/export, controller-programmed forwarding, packet-path attribution, withdrawal, and cleanup with an external FRR peer. | The no-replacement regression passed on 2026-11-02. Full CNI lifecycle is tracked separately below; shared-VTEP acceptance is not established. |
| GoBGP Netlink EVPN Integration — independent Framework E2E | Direct Framework network/endpoint lifecycle, one OVN chassis, GoBGP↔FRR Type-2 exchange, OVN/OVS forwarding, packet attribution, withdrawal, survivor traffic, and owned cleanup. | Acceptance passed in two consecutive independent attempts; evidence is listed below. This does not establish two-chassis, shared-VTEP, replacement-ADD, or Type-5 acceptance. |
| Framework OVSDB-Programmed — independent E2E | Direct Framework lifecycle, standalone GoBGP RIB Type-2 importer, OVN/OVS provider bridge and VXLAN state, remote workload forwarding, withdrawal, survivor preservation, and cleanup. | Acceptance passed in two consecutive clean live runs; evidence paths are listed below. |
| GoBGP Netlink EVPN Integration — two-chassis OVN acceptance | GoBGP Type-2 import/export; kernel integration state observed by `ovn-controller`; OVS tunnels, OpenFlow/BUM, and traffic through real OVN logical ports; withdrawal, sibling preservation, and recovery. | Not established. A component or one-chassis PASS must not be reported as two-chassis/shared-VTEP acceptance. |
| Application — Kubernetes CNI | CNI ADD/CHECK/DEL, node-agent integration, endpoint and Type-2 lifecycle, forwarding, injected pre-LSP ADD rollback, and cleanup. | Full one-chassis lifecycle passed in two consecutive live runs on 2026-11-02; shared-VTEP acceptance remains unrun. |

Neither framework suite requires CNI. CNI's OVN/OVS endpoint attachment
operations do not demonstrate the standalone OVSDB-Programmed EVPN dataplane.
For the GoBGP Netlink EVPN Integration, Linux bridge, VXLAN, FDB, and neighbor state are
the kernel integration boundary, not the workload forwarding dataplane;
`ovn-controller` programs OVS forwarding according to the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration).
See the [Framework architecture](docs/architecture/framework-first.md),
[integration guide](docs/architecture/integrations.md), and
[CNI application guide](docs/applications/cni/README.md). Do not describe a
component test or archived report as full acceptance unless it meets the
corresponding contract and identifies its source/runtime.
The matrices below use stable acceptance descriptions rather than internal
runner gate numbers. `PASS` means executed and verified, `FAIL` means
executed and failed, `BLOCKED` means a demonstrated prerequisite prevents
execution, `NOT RUN` means not executed, and `PARTIAL` means only a documented
subset was verified. Status applies only to the referenced commit and scope.

## Independent Framework Netlink E2E

This CNI-free, Multus-free test exercises the current GoBGP Netlink EVPN
Integration through the Framework directly. Its test provisioner calls the
Framework OVSDB management API to create/delete the logical network and uses
the node-agent's reusable network and endpoint managers for two local
endpoints; it does not invoke the CNI plugin, CNI ADD/CHECK/DEL, a
NetworkAttachmentDefinition, or CNI-specific configuration. The test-specific
provisioner is in
[`pkg/nodeagent/framework_netlink_e2e_test.go`](pkg/nodeagent/framework_netlink_e2e_test.go),
and the isolated runner is
[`tools/run-framework-netlink-e2e.sh`](tools/run-framework-netlink-e2e.sh).

Run from the repository root on a disposable host with Docker, kind,
kubectl, Go, and the required pinned images available:

```bash
tools/run-framework-netlink-e2e.sh
```

The runner uses the dedicated `framework-netlink-e2e-c4k` kind cluster and
stores a new, non-overwriting attempt under
`/tmp/framework-netlink-e2e-c4k-evidence/attempt-<UTC timestamp>-<run id>/`.
It verifies resource ownership and before/after Docker and kind inventories,
and removes only run-owned resources. The topology is one OVN worker chassis,
the shared OVN NB/SB services, the upstream GoBGP speaker, the node-agent's
optional Type-2 importer/exporter, and one external FRR peer. Two Framework-
managed local endpoints and an FRR-side remote workload are attached to real
OVN logical ports. The sequence is shown in the
[Framework E2E sequence diagram](docs/uml/evpn-framework-netlink-e2e-sequence.svg).

The runner records its checks individually through their evidence files:
dedicated topology and cleanup; CNI/Multus independence; direct Framework
provisioning; OVN NB/SB and chassis binding; GoBGP↔FRR session and Type-2
exchange; exporter/importer receipts; ownership-scoped Netlink state;
`ovn-controller`-owned OVS attachment; bidirectional traffic; five-point
packet-path captures; endpoint withdrawal and survivor preservation; endpoint,
route, network, and Netlink cleanup; and repeatability. All listed criteria
passed in two consecutive runs:

- `attempt-20261101T221430Z-3c9b53c4`
- `attempt-20261101T221831Z-34582eeb`

Each attempt has `run-status.txt`, `cni-independence.txt`, Framework
provisioning receipts, OVN NB/SB and OVS snapshots, GoBGP/FRR route evidence,
Netlink state, both directions of initial and survivor ping output, and the
five `*.pcap` files with decoded `*.decoded.txt` companions. Cleanup
verification includes empty exporter state, withdrawal of remote-owned
Netlink state, absence of the deleted logical network, and restored cluster
and Docker inventories.

This is a one-chassis external-FRR interoperability acceptance only. It does
not claim two-chassis GoBGP↔GoBGP operation, shared-VTEP sibling lifecycle,
replacement ADD, Type-5, or production readiness. The current source also
does not validate a two-chassis OVN data path.

The required C4.4f.2m Netlink Framework regression also passed on 2026-11-02:
`/tmp/framework-netlink-e2e-c4k-evidence/attempt-20261102T110728Z-0a14d0b8/`.

### Netlink-Programmed Framework acceptance

| Acceptance criterion | Verification | Status | Evidence / test reference |
|---|---|---|---|
| Environment and isolation | Dedicated kind topology, before/after inventories, and ownership-scoped cleanup | PASS | Two consecutive attempts; `attempt-20261101T221430Z-3c9b53c4/` and `attempt-20261101T221831Z-34582eeb/`, `run-status.txt` |
| CNI independence | No CNI, Multus, NetworkAttachmentDefinition, or CNI-specific configuration | PASS | `cni-independence.txt`; `pkg/nodeagent/framework_netlink_e2e_test.go` |
| Framework provisioning | Direct network and two local endpoint lifecycle through Framework managers | PASS | Framework provisioning receipts in both attempt directories |
| OVN endpoint state | NB logical ports, SB Port_Binding records, and chassis binding | PASS | OVN NB/SB snapshots in both attempt directories |
| EVPN Type-2 exchange | GoBGP and FRR peer/session and local/remote route evidence | PASS | GoBGP/FRR route evidence in both attempt directories |
| Netlink integration | Ownership-aware route importer/exporter state, FDB, and neighbor state | PASS | Exporter/importer receipts and Netlink snapshots in both attempt directories |
| OVN/OVS forwarding | `ovn-controller`-owned OVS attachment and controller-programmed forwarding | PASS | OVS snapshots and forwarding assertions in both attempt directories |
| Traffic verification | Bidirectional workload traffic | PASS | Initial and survivor ping output in both attempt directories |
| Packet-path evidence | Five-point captures with decoded companions | PASS | `*.pcap` and `*.decoded.txt` in both attempt directories |
| Withdrawal and survivor | Endpoint/route withdrawal while survivor route and traffic remain | PASS | Withdrawal receipts and survivor pings in both attempt directories |
| Ownership and cleanup | Endpoint, route, network, Netlink state, and test-owned environment cleanup | PASS | Cleanup evidence and `run-status.txt` in both attempt directories |
| Reproducibility | Two consecutive independent live runs | PASS | Both runs above; latest verified 2026-11-01 at commit `86e23a529dc89aea12427cc2dcbf7e301ff548ae` |

Scope is limited to one OVN chassis and an external FRR interoperability
peer. Two-chassis GoBGP operation, shared-VTEP sibling lifecycle, replacement
ADD, Type-5, and production readiness are not established.

## Independent Framework OVSDB-Programmed E2E

Run this separately from the Netlink E2E:

```bash
tools/run-framework-ovsdb-e2e.sh
```

The runner owns a dedicated `framework-ovsdb-e2e-c4l` kind cluster and writes
each attempt to a new directory under
`/tmp/framework-ovsdb-e2e-c4l-evidence/`. It provisions the network through
the OVSDB management `AddNetwork` API and local endpoints through the existing
Framework network/endpoint managers in `pkg/nodeagent`; it does not invoke
`evpn-cni`, Multus, a NetworkAttachmentDefinition, or CNI ADD/CHECK/DEL.
The test starts the node-agent's Framework endpoint exporter and the standalone
`cmd/evpn-ovsdb` route importer as separate workloads. It does not configure the
Netlink remote importer or the legacy `bgp_ad_handler` writer for this network.

The route mapping exercised by this integration is: an imported Type-2 route's
network/VNI and next-hop VTEP identify the network and remote tunnel; MAC and
optional IP remain in route-validation and tunnel ownership metadata. The
OVSDB programmer creates an owned OVN `localnet` logical switch port using the
configured physnet, an owned provider bridge and bridge mapping, and an OVS
VXLAN interface/port configured with `remote_ip`, VNI key, and UDP destination
port. `ovn-controller` reconciles the NBDB localnet port into OVN/OVS state.
The route handler rejects multiple remote VTEPs for this single-network
integration. Remote withdrawal removes only the remote workload's bridge
attachment while keeping the FRR peer established; the runner verifies that
the remote Type-2 route and owned tunnel/localnet path disappear while the
remote Type-3 route, pre-existing unowned VXLAN interfaces, and surviving
Framework endpoint route and port remain. This is a one-route ownership check,
not shared-VTEP sibling-lifecycle acceptance.

The runner records each check in its attempt's `run-status.txt` and
`acceptance-gates.txt`: dedicated owned topology; CNI/Multus independence;
direct Framework provisioning; explicit OVSDB writer selection; NB/SB and
chassis attachment; GoBGP↔FRR EVPN Type-2 exchange; standalone importer
reconciliation; route-driven OVN/OVS objects; bidirectional remote workload
traffic; correlated endpoint/underlay/FRR packet captures; local withdrawal
with survivor preservation; remote withdrawal with owned-state removal;
and full environment cleanup. Reproducibility is established only when a
second consecutive attempt follows a fully passing, clean prior attempt. A successful
control-plane snapshot or local-only ping is not sufficient. The capture set
records endpoint veth, local underlay, FRR underlay, FRR VXLAN, and remote
workload traffic; it is specific to this one-chassis E2E topology, not a general
assertion that every integration uses the same packet path.

Two consecutive live attempts passed all listed checks and cleanup:

- `/tmp/framework-ovsdb-e2e-c4l-evidence/attempt-20261101T235036Z-d342b904/`
- `/tmp/framework-ovsdb-e2e-c4l-evidence/attempt-20261101T235300Z-25b006ad/`

The required C4.4f.2m OVSDB Framework regression also passed on 2026-11-02:
`/tmp/framework-ovsdb-e2e-c4l-evidence/attempt-20261102T111021Z-0cdd286e/`.

The live route-processing sequence and cleanup boundaries are shown in the
[OVSDB Framework E2E sequence diagram](docs/uml/evpn-framework-ovsdb-e2e-sequence.puml).
The OVSDB importer is an opt-in standalone path rather than the standard
deployment runtime. Its current provider-bridge integration supports one
network and one remote VTEP; it does not establish two-chassis GoBGP operation,
shared-VTEP sibling lifecycle, Type-5, or production readiness.

### OVSDB-Programmed Framework acceptance

| Acceptance criterion | Verification | Status | Evidence / test reference |
|---|---|---|---|
| Environment and isolation | Dedicated topology, ownership-safe cleanup, and a fresh attempt directory | PASS | Two consecutive attempts; `attempt-20261101T235036Z-d342b904/` and `attempt-20261101T235300Z-25b006ad/`, `run-status.txt` |
| CNI independence | Direct Framework lifecycle without CNI, Multus, or NetworkAttachmentDefinition | PASS | `acceptance-gates.txt`; Framework E2E test and `tools/run-framework-ovsdb-e2e.sh` |
| Integration selection | Explicit OVSDB writer and no conflicting Netlink writer for the test network | PASS | Attempt manifests and `run-status.txt` |
| Framework provisioning | Network and endpoint lifecycle through Framework APIs/managers | PASS | Framework provisioning receipts in both attempt directories |
| OVN endpoint state | NB/SB logical port and chassis attachment | PASS | OVN snapshots in both attempt directories |
| EVPN Type-2 exchange | GoBGP↔FRR Type-2 import/export | PASS | GoBGP/FRR route evidence in both attempt directories |
| Standalone route importer | GoBGP RIB-to-OVSDB Type-2 importer reconciles the route | PASS | Importer status and route-processing evidence in both attempt directories |
| OVSDB programming | Route-derived localnet LSP, provider bridge/mapping, and VXLAN tunnel | PASS | NBDB/OVSDB snapshots and ownership assertions in both attempt directories |
| OVN/OVS forwarding | Controller reconciliation and workload forwarding | PASS | Forwarding state and workload traffic assertions in both attempt directories |
| Traffic verification | Bidirectional local/remote workload traffic | PASS | Ping results in both attempt directories |
| Packet-path evidence | Correlated endpoint/underlay/FRR capture points | PASS | Capture manifest, PCAPs, and decoded evidence in both attempt directories |
| Local withdrawal and survivor | Local route withdrawal with surviving endpoint route/port preserved | PASS | Local withdrawal and survivor evidence in both attempt directories |
| Remote withdrawal | Owned remote path removed while BGP peer stays established | PASS | Remote withdrawal snapshots and peer-state evidence in both attempt directories |
| Ownership and cleanup | Framework, OVN/OVS, route-importer, and test-owned environment cleanup | PASS | Cleanup evidence and `run-status.txt` in both attempt directories |
| Reproducibility | Two consecutive independent live runs | PASS | Both runs above; latest verified 2026-11-01 at commit `86e23a529dc89aea12427cc2dcbf7e301ff548ae` |

The importer remains an opt-in standalone path, not production-default
deployment. This integration is limited to one network and one remote VTEP; it
does not establish shared-VTEP sibling lifecycle, two-chassis GoBGP operation,
Type-5, or production readiness.

## Kind deployment

The Containerlab topology and runtime components are described in the
[deployment guide](docs/architecture/deployment.md). Every dedicated Framework
and CNI E2E runner invokes the shared Containerlab lifecycle itself. Do not
pre-create a separate shared lab. The environment-only compatibility wrapper
uses the same exact-topology ownership guards and scoped teardown; it no longer
destroys other laboratories or installs the Framework runtime. Use a dedicated
disposable host for all live tests.

## Optional CNI / Type-2 interoperability test

This runner creates a uniquely named disposable kind cluster, exercises the
CNI binary and Multus, and performs guarded cleanup of resources it owns. It
requires Docker, kind, kubectl, network access for pinned runtime inputs, and
sufficient privileges. Read the
[CNI application guide](docs/applications/cni/README.md) and runner before
use; do not run it against a shared cluster.

```bash
EVPN_FRR_INTEROP_TEST=1 EVPN_IMPORT_TEST=1 \
  bash tools/run-cni-mvp-2.4b-multus.sh
```

The historical CNI-R diagnostics do not establish current-HEAD acceptance.
The maintained full-lifecycle contract is described below; no private report
is required to run it.

## CNI Full Lifecycle acceptance

The full-lifecycle entry point is `tools/run-cni-full-lifecycle-e2e.sh`. Run it
from the repository root on a disposable host with Docker, kind, kubectl,
network access for pinned runtime inputs, and sufficient privileges:

```bash
bash tools/run-cni-full-lifecycle-e2e.sh
```

The wrapper gives every run a unique cluster and non-overwriting attempt
directory under `/tmp/cni-full-lifecycle-c4m-evidence/`. It exercises actual
Multus/CNI operations through the installed CNI binary: initial ADD, CHECK,
bidirectional workload traffic, DEL and repeated DEL, replacement ADD on the
same network using a new attachment identity, replacement CHECK and traffic,
then final DEL and network cleanup. It also verifies repeated ADD, negative
CHECK without resource recreation, injected missing-netns ADD rollback followed
by a valid retry, and survivor endpoint state/traffic throughout A's lifecycle.
The CHECK assertions cover exact identity and the expected OVN/OVS attachment;
they do not substitute for traffic verification.

Each phase has its own packet manifest, verifier result, five capture points,
and decoded companion files. The initial files are unprefixed; replacement
files use the `replacement-` prefix. The verifier checks the initial and
replacement endpoint identities, correlated bidirectional ICMP, VNI 1000,
VTEPs `198.18.0.2` and `198.18.0.4`, and OVN/OVS forwarding evidence. A set of
PCAP files without successful packet-content verification is not a PASS.
The five points are the pod `net1` peer, worker underlay, FRR underlay, FRR
VXLAN, and remote workload peer.

Two consecutive full runs passed on 2026-11-02:

- `/tmp/cni-full-lifecycle-c4m-evidence/attempt-20261102t105149z-4fca0fb6/`
- `/tmp/cni-full-lifecycle-c4m-evidence/attempt-20261102t110030z-e4786a0a/`

Both runs completed all full-lifecycle operations and cleanup, and both
`packet-path-verification.json` and
`packet-path-replacement-verification.json` report `PASS`. The five-point
captures and decoded companions for each phase are in the corresponding
attempt directory. The one-chassis topology uses the Framework's Netlink
integration with OVN/OVS forwarding and an external FRR interoperability peer.

The existing no-replacement regression remains executable with its original
flags and meaning:

```bash
EVPN_FRR_INTEROP_TEST=1 EVPN_IMPORT_TEST=1 \
EVPN_FRR_INTEROP_NO_REPLACEMENT=1 \
  bash tools/run-cni-mvp-2.4b-multus.sh
```

The no-replacement regression passed on 2026-11-02; its attempt evidence is in
`/tmp/cni-24b-20261102111248-44cf7a57559d-evidence/`. It verifies the
supported one-chassis no-replacement CNI/Framework subset. It does not replace
the full lifecycle run or establish shared-VTEP acceptance.

| Acceptance criterion | Verification | Status | Evidence / test reference |
|---|---|---|---|
| Environment and isolation | Dedicated cluster, guarded ownership, attempt-specific evidence, and safe cleanup | PASS | Both full-lifecycle attempts above; cleanup and inventory restoration verified |
| Initial CNI ADD and endpoint state | Real Multus/CNI ADD; exact identity/ownership, NB/SB binding, OVS attachment, and Type-2 advertisement | PASS | Initial lifecycle receipts and endpoint/route snapshots in both attempt directories |
| Initial CNI CHECK | Installed CNI binary validates the actual attachment identity and resources | PASS | Initial CHECK result in both full-lifecycle attempt directories |
| Initial connectivity | Bidirectional local/remote workload forwarding | PASS | Initial forward and reverse traffic records in both attempts; zero packet loss |
| Initial packet-path evidence | Five-point captures, decoded companions, packet identity, VNI/VTEP and OVN/OVS forwarding | PASS | `packet-path-verification.json`, `packet-path-manifest.json`, PCAPs and `.decoded.txt` files in both attempts |
| Initial CNI DEL and withdrawal | Real DEL, Type-2 withdrawal, remote route removal, and owned OVN/OVS cleanup | PASS | Initial DEL, withdrawal and cleanup evidence in both attempts |
| Repeated DEL | Replayed DEL is safe and preserves unrelated state | PASS | Repeated DEL result in both full-lifecycle attempt directories |
| Replacement ADD and endpoint state | New attachment identity on the same network; fresh SB binding, OVS attachment, ownership, and Type-2 | PASS | Replacement lifecycle receipts and endpoint/route snapshots in both attempts |
| Replacement CHECK | Real CHECK validates the replacement attachment | PASS | Replacement CHECK result in both full-lifecycle attempt directories |
| Replacement connectivity | Bidirectional forwarding for replacement identity | PASS | Replacement forward and reverse traffic records in both attempts; zero packet loss |
| Replacement packet-path evidence | Five-point captures, decoded companions and packet attribution for replacement identity | PASS | `packet-path-replacement-verification.json`, `packet-path-replacement-manifest.json`, `replacement-*.pcap`, and decoded companions in both attempts |
| Final DEL and cleanup | Replacement route withdrawal, OVN/SB/OVS cleanup, ownership cleanup, then network deletion | PASS | Final lifecycle and guarded environment cleanup evidence in both attempts |
| Repeated ADD | Same attachment identity returns a consistent result without duplicates | PASS | Repeated ADD assertions in both full-lifecycle attempt directories |
| Negative CHECK | Missing or inconsistent attachment is rejected without resource recreation | PASS | Negative CHECK result in both full-lifecycle attempt directories |
| Injected pre-LSP ADD rollback | Missing network namespace fails before LSP creation and leaves no endpoint resources; a subsequent valid ADD succeeds | PASS | Rollback injection and subsequent valid retry evidence in both full-lifecycle attempt directories |
| Survivor preservation | Unrelated endpoint route, ownership, OVN/OVS attachment, and bidirectional traffic survive lifecycle operations | PASS | Survivor checks after lifecycle transitions in both attempts |
| No-replacement regression | Existing one-chassis no-replacement interoperability scenario remains executable | PASS | 2026-11-02 run at `/tmp/cni-24b-20261102111248-44cf7a57559d-evidence/`; its report remains `PARTIAL` because shared-VTEP is not run |
| Reproducibility | Two consecutive complete full-lifecycle runs | PASS | Both full-lifecycle attempts above; packet verifiers PASS for both phases in both attempts |
| Shared-VTEP lifecycle | Sibling endpoint lifecycle and forwarding across a shared VTEP | NOT RUN | Explicitly outside the verified one-chassis full-lifecycle scope |

The results establish the listed one-chassis CNI lifecycle only. They do not
establish shared-VTEP lifecycle or broader multi-chassis acceptance.

## Evidence and capture rules

Successful lab dataplane validation requires bidirectional traffic and packet
captures; gRPC success alone is insufficient. In kind mode, run OVN/OVS
captures through `kubectl exec` in the `ovn-ovs` pod. FRR-side captures must
run in the `frr` container namespace. Do not use static ARP pinning as a
substitute for validating the real dataplane path.

Check that traffic succeeds in both directions, ARP is learned dynamically
(not `PERMANENT`), and captures show ICMP on the OVS client veth and FRR
bridge `br1000`. The `ns-ovs-client` namespace is inside the `ovn-ovs` pod;
run namespace commands with `kubectl exec ... ip netns exec`. A missing FRR
namespace from the OVN pod indicates a capture-context error. A missing
`rt-*` tunnel at capture start is timing-sensitive diagnostic evidence, not
by itself a dataplane failure. Failed OVS-client ARP suggests a resolution or
interface-mapping problem.

The generated `rt-*` transit name is source-derived, not a stable literal:
`lspName := "rt-" + idPart`, where `idPart` comes from the source router ID
or neighbor IP, replaces `.` and `:` with `-`, and is truncated. Tunnel
capture is best-effort diagnostics and can be timing-sensitive.

Read-only spot checks from a lab host include:

```bash
kubectl -n ovn-system exec deploy/ovn-ovs -- ovn-nbctl show
kubectl -n ovn-system exec deploy/ovn-ovs -- ovs-vsctl show
kubectl -n ovn-system exec deploy/ovn-ovs -- ovs-vsctl list interface
kubectl -n k8gobgp-system get pods,svc
docker exec frr ip -br a
docker exec frr bridge fdb show dev vxlan1000
```

Current limitations and workarounds are tracked in the
[known-issues registry](docs/known-issues/README.md).
