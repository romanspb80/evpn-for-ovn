# Historical validation record

These are **historical reports**, not test results for the current checkout.
The [Testing Guide](../../TESTING_GUIDE.md) contains current entry points and
prerequisites; the [capability matrix](../project/capability-matrix.md)
defines current status and its evidence limits. Most original result bundles,
captures and source manifests are not version-controlled. The three local
bundles identified below were accessible when reviewed; their local paths are
not portable. A historical result is not a substitute for a new run.

The [history index](README.md) explains the distinction between
reproducible procedures, historical observations and retained evidence.
All live tests require a dedicated disposable host and the
[lab ownership and safety rules](../../deploy/containerlab/README.md).

<a id="planned-external-evpn-fabric-acceptance-g7b5c"></a>
<a id="two-worker-ovn-geneve-baseline-g7b5c2"></a>
<a id="external-frr-type-2-interoperability-g7b5c3"></a>
<a id="external-gobgp-type-2-interoperability-g7b5c4"></a>
<a id="external-evpn-isolation-and-lifecycle-g7b5c5"></a>
## Shared OVN and external EVPN peers

The [external fabric record](../testing/external-evpn-fabric.md) is the
authoritative historical record for these distinct scenarios, their
topologies, assertions, source identities, failed attempts and limitations:

- Two workers with shared OVN demonstrated Geneve using fixture endpoint
  namespaces, without BGP; this is **not** Multus lifecycle acceptance.
- The FRR/Linux peer exchanged Type-2 with one local agent and exercised
  VXLAN traffic while separate local Geneve traffic continued.
- A second external GoBGP/Linux peer used a **test-only RIB projector** and
  Linux forwarding, not an independent OVN control plane. It was exercised
  alongside the FRR peer and local Geneve.
- Isolation, lifecycle and recovery were tested in bounded configurations.
  Earlier failed-closed restart observations and later reported successes
  describe different attempts; they do not prove final fabric acceptance.

The final full-fabric claim remains **unresolved** until two final-input
acceptance manifests and source/build identities are independently verified.
See the [acceptance prerequisites](../testing/external-evpn-fabric.md#remaining-acceptance-prerequisites).
These scenarios are separate from the `d253272` two-worker secondary-CNI
baseline, which tested Multus and Geneve but not EVPN Type-2.

<a id="standalone-type2-export-smoke"></a>
<a id="standalone-export-smoke-g7b5a"></a>
<a id="standalone-co-hosting-g7b5b1"></a>
<a id="standalone-multi-chassis-g7b5b2"></a>
<a id="standalone-endpoint-lifecycle-g7b5b3"></a>
## Standalone authorization and two-chassis lifecycle

The standalone runtime scenarios reused a Kubernetes laboratory, not a
Kubernetes-free provisioner. The single-chassis export smoke reported
attested GoBGP advertisement, exact UUID withdrawal following authorization
revocation, agent restart without resurrection, and preservation of an
unrelated FRR route. Co-hosted import/export additionally reported independent
journal recovery and remote FDB/neighbor installation after agent restart.

The two-chassis fixture used a Kind worker and a separate OVN/OVS container
sharing NB/SB, with disjoint logical switches, separate speakers and journals,
one VNI and native Netlink import. A historical 15-case run reported
bidirectional attributed VXLAN packets, an agent-only restart, exact receipt
preservation, withdrawal and scoped cleanup. Its source/build identity is not
established as `d253272`.

The lifecycle extension reported 23/23 cases on 2026-11-08, including the 15
baseline cases. Complete NB/SB absence without a trusted tombstone retained
the advertisement; an exact deletion tombstone permitted withdrawal.
Recreation and replacement used higher generations and distinct LSP/path
UUIDs; rollback, digest equivocation and stale-generation replay were
rejected. Migration drained the source and remote receipt before destination
attachment, then tested forwarding and agent restart. A source-down probe
observed a forwarding gap: no zero-downtime claim is made. Original endpoints
and a foreign FRR route were survivor controls. Source/build identity for
this historical run is not established as `d253272`.

The following 2026-11-08 bundles were inspected read-only. Each contains
`result.json`, `cases.tsv`, build hashes and scenario artifacts, including
packet captures. Both test and cleanup exits were zero. Build hashes alone
do not establish the corresponding source commit or equivalence to the
current checkout.

| Result | Local evidence directory | Scope |
|---|---|---|
| 15/15 PASS | `/tmp/evpn-operator-type2-e2e/attempt-20261108T172656Z-7f275ab1` | Standalone/co-hosted two-chassis fixture; not two Operator-managed workers |
| 23/23 PASS | `/tmp/evpn-operator-type2-e2e/attempt-20261108T185704Z-916507b7` | Same baseline plus standalone endpoint lifecycle |
| 18/18 PASS | `/tmp/evpn-operator-type2-e2e/attempt-20261108T190549Z-46ea1d2e` | One Operator-managed OVN chassis and independent FRR/Linux peer |

Despite the suite names, the two-chassis agents were not an
inventory-gated Operator DaemonSet. The second chassis was a separate
container, not a Kubernetes worker, and the standalone speakers peered
directly. VTEPs were `198.18.0.2` and `198.18.0.5`; captured forwarding
used VNI 1000 and UDP 4789. The lifecycle extension also used a non-CNI
endpoint. These reports do not validate the current shared-switch,
two-worker, external-FRR example.

Earlier fixture failures included container-network setup, unpinned cgroup
identity, buffered capture, mutable journal sequence assumptions and neighbor
quarantine when destination binding preceded source drain. These failures
were not counted as passes or used to relax ownership checks. The scenario
does not establish speaker replacement, stale database reads, interrupted
writes, power loss, both dataplane adapters or general production recovery.
See the [authorization and tombstone contract](../architecture/standalone-endpoint-authorization.md)
and [runner and flags](../../TESTING_GUIDE.md#standalone-authorization-and-recovery).

<a id="independent-framework-netlink-e2e"></a>
<a id="netlink-programmed-framework-acceptance"></a>
<a id="independent-framework-ovsdb-programmed-e2e"></a>
<a id="ovsdb-programmed-framework-acceptance"></a>
<a id="cni-full-lifecycle-acceptance"></a>
<a id="e2e-acceptance-categories"></a>
## One-chassis Framework and CNI scenarios

Historical reports describe two independent runs each for direct Framework
Netlink, opt-in OVSDB, and Multus CNI full lifecycle using a GoBGP/FRR peer.
The Netlink and OVSDB repeatability results dated 2026-11-01 identify source
commit `86e23a529dc89aea12427cc2dcbf7e301ff548ae`. A subsequent
2026-11-02 Containerlab matrix identifies implementation `56ed56c`, common
lifecycle `3f4da4a`, and runner migration `9abcaba`; its reported two runs
per scenario used Containerlab 0.79.0, OVN 26.09.0, OVS 4.0.0, FRR 10.7.1,
k8gobgp 0.2.8 and GoBGP fork `6b99046`. These identities do not establish the
current build identity; original reports and packet bundles are private.

The Framework tests did not invoke CNI or Multus. Netlink imported validated
routes into owned Linux FDB/neighbor state observed by `ovn-controller`;
OVS forwarded the workload packets. The OVSDB path instead used the opt-in
importer, localnet/provider bridge and owned VXLAN objects; it is limited to
one network and one remote VTEP. Both reported Type-2 route and attributed
packet checks, exact withdrawal, survivor preservation and owned cleanup.
The CNI scenario exercised real ADD/CHECK/DEL, replacement, repeated calls,
negative CHECK, pre-LSP rollback, forwarding and survivor cleanup. Its broader
shared-VTEP aggregate remained PARTIAL / NOT_RUN. Neither one-chassis scenario
proves general multi-chassis or shared-VTEP recovery.

The [scenario guide](../../TESTING_GUIDE.md#framework-netlink-and-ovsdb-integration)
links to the maintained runners and their acceptance criteria. The
[operator record](../architecture/operator-type2-acceptance.md) separately
documents one-chassis operator-managed GoBGP–FRR acceptance, including
forced agent crash and the descriptor-ownership failure investigation.

<a id="framework-dataplane-boundary-evidence"></a>
<a id="framework-fixture-and-exporter-regressions"></a>
<a id="g84b-dataplane-boundary-acceptance"></a>
<a id="g85b-export-composition-acceptance"></a>
## Framework fixture and exporter regressions

In the 2026-11-10 dataplane-boundary report, missing fixture provider
attestation caused importer validation to fail; the exporter's cancelled
database dial was secondary. The OVSDB fixture also used noncanonical
endpoint IDs, which the observer rejected correctly. Fixture wiring was
corrected without disabling attestation or changing production
reconciliation. OVSDB expected two acknowledged MAC/IP advertisements;
the Netlink fixture emitted four, MAC-only plus MAC/IP for two endpoints.
Source-manifest SHA-256 was reported as
`b7d4d10fffe6afbf644dcf19737138f68ab3829c3c612eaf38b73226234dfe34`;
the manifest itself is not a public artifact. The reported Netlink agent image
was `sha256:d11ead06b7fc1c6eaf72fbd24125451a83d68d7d16123bae9ab7794a55a8e266`.

Both this report and the subsequent exporter-composition report distinguish
the two OVSDB runs: the first passed operational checks but **repeatability
was not established**; the second reported the repeatability gate passing.
In the dataplane-boundary report the preceding attempt had failed. The
runner's actual `O14` assertion is the consecutive-clean-run gate; see
[`run-framework-ovsdb-e2e.sh`](../../tools/run-framework-ovsdb-e2e.sh).
One recovery attempt sampled a pending importer checkpoint and failed;
an unchanged rerun reported the bounded restart checks passing. Neither
result proves generalized recovery or shared-VTEP lifecycle.

The exporter-composition source manifest included untracked Go files and
reported SHA-256
`f6aaaaa8a42a8264ad4cea9aeb145cb276365d1856e9d8fb2d6bb79e92ef7810`;
its private manifest is unavailable here. The reported Netlink agent image
was `sha256:8555edef7e223f092f5dafd5490671930034fb4fcc1acf4cea3ef5b664965aa1`;
the provider image was `sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de`
and speaker executable SHA-256 was
`cb1db57e29555aef67a5d6c0bdfb7809243fffc7a8a201a55aeb93a57722a15f`.
The reported tests included
characterization of CNI operation-lock validation/AddPath ordering,
standalone policy fencing, exact withdrawal and independent Netlink/OVSDB,
operator, CNI and bounded external-peer regressions. See
[`export_composition_test.go`](../../pkg/nodeagent/export_composition_test.go) and
the [dataplane contracts](../architecture/framework-architecture.md#type-2-dataplane-boundaries).
These reports do not establish a `d253272` live run.

<a id="offline-fixture-provenance"></a>
<a id="offline-validation"></a>
<a id="test-inventory"></a>
## Offline fixtures and recovery models

Offline packet and receipt regressions generate synthetic data; they are not
sanitized copies of live bundles or proof of packet forwarding. Tests for
incomplete capture, duplicate CNI identity and CNI deadline status remain in
the [test sources](../../tools/lib/) and [test guide](../../TESTING_GUIDE.md#unit-and-offline-tests).
The CLI interop test needs permission to create an isolated network namespace;
failure to create it is a prerequisite failure, not a skipped success. Root
requires `CAP_SYS_ADMIN`; non-root needs permitted user and network namespaces.

<a id="cross-version-durable-recovery-helpers"></a>
### Cross-version recovery model

[`cross-version-recovery.py`](../../tools/e2e/cross-version-recovery.py) coordinates
writer and reader binaries compiled separately with Go 1.25.0 and 1.27.3
from the pre-migration harness revision. The current module directive cannot
be compiled by Go 1.25.0. Compile the `pkg/ownership`, `pkg/nodeagent` and
`pkg/evpn` test binaries with each compiler, install the binaries root-owned
and non-writable, and use distinct executable fixture and evidence
directories. Run the coordinator with `--old`, `--new` and `--evidence`;
it verifies compiler metadata and binary hashes. See the script's `--help`
and test helpers for the exact inputs.

The model exercises real journal and serialization code, locks, pending
intents, abrupt exits and refusal of ambiguous or mismatched ownership.
GoBGP, kernel and workload external state are modeled; it does not demonstrate
live BGP, peer authentication, real kernel forwarding or power-loss safety.

<a id="required-containerlab-provisioning"></a>
<a id="core-operator-lifecycle"></a>
<a id="operator-managed-type-2-acceptance"></a>
<a id="maintained-scenario-inventory-and-ownership"></a>
<a id="kind-deployment"></a>
<a id="optional-cni--type-2-interoperability-test"></a>
<a id="evidence-and-capture-rules"></a>
## Reproducibility and evidence policy

Use the [lab guide](../../deploy/containerlab/README.md) and
[Testing Guide](../../TESTING_GUIDE.md) for current commands, prerequisites,
profiles, failure handling and ownership-safe teardown. The route-only
[GoBGP/FRR fixture](../../tests/e2e/type2-fixture/README.md) and the
[Linux Netlink component fixture](../../tests/e2e/netlink-acceptance/README.md)
do not establish OVN workload forwarding. Packet-path acceptance requires
attributed bidirectional traffic, not merely a BGP session, ping or PCAP file.
Capture and route state in their actual process/network namespaces; an
unavailable or incomplete observation is not an authoritative empty result.

Record source and build identities, configuration, peer and dataplane
versions, assertion outcomes, route receipts, packet attribution and cleanup.
Keep credentials and raw private bundles out of source control. A historical
private path, absent manifest or summary table is not durable public evidence.

<a id="c44f2q2-acceptance"></a>
<a id="dependency-upgrade-acceptance"></a>
<a id="verified-c44f2q1-runs"></a>
## Dependency and provisioning transition

An earlier dependency report identified implementation `7f37a5f` for
FRR 10.7.1 and k8gobgp 0.2.8; the standalone route fixture bypasses the
k8gobgp manager and is not equivalent provider evidence. The later
Containerlab migration matrix above supersedes its repeated status table,
not the distinct failure investigations. Earlier compatibility issues
included missing FRR base configuration and changed FRR output formatting.
Containerlab management TLS does **not** secure Framework gRPC.
Exact current pins and provenance requirements are in
[runtime dependencies](../architecture/runtime-dependencies.md).
