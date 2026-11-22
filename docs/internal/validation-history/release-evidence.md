# Historical validation and engineering provenance

For researchers and evidence reviewers, not installation guidance.
Historical results establish only their recorded fixture, inputs and checks;
they do not certify the current v0.1.0 tree. Use the
[Testing Guide](../../../TESTING_GUIDE.md) for current procedures and the
[capability matrix](../../project/capability-matrix.md) for release maturity.

## Release acceptance evolution

The recorded original inventory has 89 canonical acceptance IDs within a
166-row matrix. Earlier evidence includes 75 PASS / 14 NOT TESTED and a later
87/89 gap-closure outcome with H053 and H054 failing. A separate 89/89 report
counted 77 retained cases plus 12 newly exercised cases; it did not report 89
fresh executions and is not the final release outcome.

The original inventory and 75/14 report are not tied to an exact tested Git
revision in the records available here. The gap-closure report identifies
`ae07988afdd08b860eb45221ed50c01e1995769f` as its production revision. The
89/89 report describes its run and evidence-manifest handling, but its tested
source revision and a portable final bundle checksum were not located.

The later 89-case review matrix recorded 87 PASS, H030 FAIL and H032b
NOT_ESTABLISHED. REVIEW040 recorded H030 and H032b as deferred pending items.
The available material does not identify which tests were removed from the
final revised inventory or their individual rationale. In particular, it does
not establish that either deferred case was retired or replaced.

The maintainer reports that obsolete or inapplicable tests were removed before
publication and that all cases in the final revised inventory passed. The
revised inventory, retirement list, exact tested source revision and final
evidence manifest/checksum were not located in the available repository and
session records. The statement is retained as maintainer-provided context; no
numeric final count or commit attribution is asserted here. It must not be
conflated with any of the intermediate outcomes above.

The later release-preparation records also distinguish the final-publication
candidate's offline test results from live acceptance: REVIEW041's local Go,
Python acceptance-harness, build and publication checks passed, while its
recommendation explicitly did not close deferred live acceptance areas. Its
candidate commit is `4c7a47bad63d297d4c8e14957ae46bddffdbac01`, based on
`8952fb561856debff7f806160d3a63adce7bd89e`; candidate archive SHA-256 is
`382efe8eb438ebff07a4dc4ec48434bdf99258b32329fb1e3f9a45d1fb95839c`; this is
not a final revised acceptance evidence bundle.

REVIEW040 used baseline `8952fb561856debff7f806160d3a63adce7bd89e` and the
pre-curation source archive SHA-256
`6fbd14bf0b9ff23b842ba9a4fe11715208b85b59599890da9027ebf53d8f22ec`. Its
defined Operator lifecycle profiles passed after exact image inputs were
verified, but H030 and H032b were still listed as deferred. REVIEW041's
publication checks similarly did not close those or other pending live
acceptance areas. These are not a record of the final revised inventory.

## Retained evidence

The [external-fabric record](external-evpn-fabric.md) is archived beside this
file. It retains historical scenario identifiers, per-attempt results,
failed/blocked transitions, topology details, source-input requirements and
the unresolved final full-fabric acceptance question.
Its proposed topology is not independent OVN-domain acceptance.

Other detailed records remain accessible at the immutable historical revision
[`ae07988afdd08b860eb45221ed50c01e1995769f`](https://github.com/romanspb80/evpn-for-ovn/tree/ae07988afdd08b860eb45221ed50c01e1995769f):

| Record | What is preserved |
|---|---|
| [Validation archive](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/validation-record.md) | Separate Framework Netlink/OVSDB, CNI, standalone, exporter and recovery reports; original counts, source identities, local bundle paths and limitations. |
| [Operator Type-2 record](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/architecture/operator-type2-acceptance.md) | One-chassis GoBGP/FRR, descriptor-ownership diagnostics and inventory-gated two-worker Attempt 8. The latter is not generalized mobility or lifecycle acceptance. |
| [History/provenance index](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/README.md) | Original project history, modern transitions, evidence availability and historical identifier interpretation. Current Project History remains in the [README](../../../README.md#project-history). |
| [OVN reproduction record](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/ovn-port-binding-reproducer.md) | Exact isolated OVN 26.09.0 configuration, crash/log observations, transient reference error and separate-switch test workaround. No proven source-level root cause. |

These files are retained in local Git history as well as linked by pinned
URLs. To inspect without touching historical branches:

```sh
git show ae07988afdd08b860eb45221ed50c01e1995769f:docs/archive/validation-record.md
git show ae07988afdd08b860eb45221ed50c01e1995769f:docs/architecture/operator-type2-acceptance.md
git show ae07988afdd08b860eb45221ed50c01e1995769f:docs/issues/ovn-port-binding-reproducer.md
```

Historical documents can contain superseded navigation, task names and
commands. Preserve those as provenance, not public current scenario names;
do not run their recipes against shared infrastructure.

## Earlier release-snapshot provenance

The earlier release commit
[`7d54a4f0f2b23912d51a0ec3085e60606f797a53`](https://github.com/romanspb80/evpn-for-ovn/tree/7d54a4f0f2b23912d51a0ec3085e60606f797a53)
preserves additional records removed from current-facing pages during
documentation consolidation. The reconciliation candidate retains this commit
in its ancestry; these links identify immutable records, not new test results.

| Immutable record | Historical evidence retained |
|---|---|
| [Release record](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/docs/release/v0.1.0.md) | Pre-curation source archive SHA-256, bounded CNI and external Type-2 acceptance summaries, separate agent/speaker recovery, synthetic foreign-state witness, forty VXLAN captures, containerd/`ctr` revision and binary identity, and failed image-transfer attempt followed by exact-digest verification. |
| [Testing scope](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/TESTING_GUIDE.md#live-test-scope) | Original per-scenario results and boundaries, including four additional Geneve captures and root transport tests without skips. |
| [Containerlab observations](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/deploy/containerlab/README.md) | Historical host tools, VM resources, guest/kernel versions and secondary-CNI fixture scope; not platform minimums or general compatibility. |
| [VM input record](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/docs/testing/vm-e2e-lab.md#host-prerequisites) | Historical base-box archive SHA-256 and cached disk fingerprint, with trust and reacquisition requirements. |
| [Model and licensing record](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/docs/release/licensing.md) | Official OVN schema/license hashes, all 40 generated outputs matching, version-specific license mappings, package-graph results and historical cache inventories; not a new distribution-specific licensing audit. |
| [Provider input](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/docs/architecture/runtime-dependencies.md#pinned-provider-input) / [FRR input and observations](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/frr/README.md) | Provider manifest digest and image-config identity, maintained FRR image-pin references and recorded runtime version. |
| [Bare-speaker fixture](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/tests/e2e/type2-fixture/README.md) | Historical FRR 8.4 privilege observations and control-plane-only scope, distinct from later Framework/CNI inputs. |

The release record identifies pre-curation source archive SHA-256
`6fbd14bf0b9ff23b842ba9a4fe11715208b85b59599890da9027ebf53d8f22ec`.
That archive identity is not the Git tree or commit identity of this candidate.
The linked reports do not establish fresh forwarding, lifecycle acceptance,
portable availability of private PCAPs, or equivalence of later harnesses.
Inspect any record locally with `git show 7d54a4f0f2b23912d51a0ec3085e60606f797a53:<path>`;
do not execute historical recipes merely to recover evidence.

## Bounded standalone two-chassis EVPN Type-2

A separate standalone run dated 2026-11-15 records base source commit
[`ae07988afdd08b860eb45221ed50c01e1995769f`](https://github.com/romanspb80/evpn-for-ovn/tree/ae07988afdd08b860eb45221ed50c01e1995769f),
tree `017ce27efa6eba344f519964184b450845b36b63`, with a test-only overlay.
The topology was two OVN/OVS namespaces sharing NB/SB, distinct switches and
VTEPs, and directly peered GoBGP speakers. The report lists five bounded
multi-chassis cases as PASS: Type-2 control-plane exchange, actual
bidirectional VXLAN forwarding, reconciliation stability, Agent-A restart
with both speakers preserved, and owned cleanup. Optional non-CNI endpoint
uncertainty, deletion, replacement, replay rejection, ordered migration and
recovery cases also passed in that attempt.

The local evidence bundle is `live-results.tar` (2,242,058,240 bytes; SHA-256
`0016964c2fc1a25d7fdebe4efa1196aa1350ff4fb8eb0a5507583d89b22df486`). It
identifies test-only changes to `tools/e2e/operator_secondary_cni.py` and
`tools/e2e/test_operator_secondary_cni.py`, including workload-capture setup
and status handling and provisioning capture/ping tools in the run-owned FRR
container. The available provenance does not establish exact overlay
applicability to every standalone assertion. The archive is not distributed
with this source tree; its digest identifies the retained local evidence, not
a public artifact link.

A separate retained H053/H054 gap-closure report at the same base source
snapshot records 87/89 canonical cases passing, with H053 and H054 failing.
H053 exercised GoBGP process replacement and observed fail-closed behavior
rather than full owned-route recovery. H054 modified an already trusted
active shared publication; both workers' routes were withdrawn while foreign
routes survived. This was not an invalid-candidate publication rejected
before acceptance. The report does not establish every requested recovery,
fencing or restoration behavior. H053's process replacement and H054's
active-publication corruption are different stimuli and must not be collapsed
into a generic “inventory rejection” result.

The pinned [validation record at `ae07988`](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/validation-record.md)
also documents separate November 8 standalone attempts reported as 15/15 and
23/23 PASS. That record explicitly says the run source/build identity is
unknown, identifies non-CNI fixtures rather than an inventory-gated Operator
DaemonSet, records a forwarding gap during source-down migration, and does not
establish speaker replacement or generalized recovery. Those counts must not
be attributed to the `ae07988` commit merely because the record is stored
there, nor to the selected `7d54a4f0` baseline.

The selected reconstruction baseline is commit
[`7d54a4f0f2b23912d51a0ec3085e60606f797a53`](https://github.com/romanspb80/evpn-for-ovn/tree/7d54a4f0f2b23912d51a0ec3085e60606f797a53),
tree `ca03e64703b8cc3d1bc1011dd34c93e84a137e4a`. It is a later implementation
snapshot with material recovery/trust changes relative to `ae07988`; therefore
the earlier `ae07988` passes and H053/H054 failures are not individually
assigned to it. The maintainer separately reports a passing final revised
release inventory, but its source revision and case mapping are unavailable.
The `590f23612cd7b8402328990ac93634a24f7c5b87` release-preparation commit is a
sibling of `7d54a4f0`, not its ancestor; its runtime/test paths inspected here
match those of `7d54a4f0`, but its source archive SHA-256
`9266fe24ee786ec6550099b628f4ca916b835118637af55fb047f00356d8be5d` is not a
final acceptance report. None of these provenance comparisons identifies the
missing revised inventory or its evidence bundle. Independent OVN-domain
interconnection, generalized multi-VTEP lifecycle, zero-loss mobility and
production support remain outside the established claims.

## Source and artifact qualifications

The Attempt 8 record's source archive digest is
`0d05c1ec554f9f6f4fb5e1f9f8c5558b371589df8a7c4b66cd9b98ba25504cc7`.
The one-chassis archive identifies run
`attempt-20261108T190549Z-46ea1d2e`. Neither record identifies the source
Git commit of its run: the pinned document revision above must not be
substituted for that missing provenance.

Most original bundles, PCAPs and private development logs are not distributed
with the source. Local artifact paths and build hashes alone do not establish
portable evidence or equivalence to the current checkout. Failed attempts
remain failures; supporting checks do not substitute for final acceptance
manifests or pre-destruction owned-route drainage.

The [recovered engineering findings](../../known-issues/engineering-findings.md)
link the original detailed registries and unsubmitted drafts. Accepted
[contributions](../../project/upstream-contributions.md) have a separate home;
neither engineering findings nor contributions are local validation records.

See the [canonical archive index](README.md) for the campaign inventory and
current links to release, two-chassis, standalone and Operator records.
