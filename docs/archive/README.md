# Project history and evidence provenance

[Project history](../../README.md#project-history-and-evolution) |
[Current capabilities](../project/capability-matrix.md) |
[Release provenance](../roadmap/release.md)

This index separates significant project history from current capabilities.
It does not fabricate evidence or resurrect obsolete installation instructions.
The original implementation remains in the deliberately separate historical
[`master` branch](https://github.com/romanspb80/evpn-for-ovn/tree/master).
The modern default `main` is a redesigned Go implementation, not a rewrite of
that branch's commits.

## Independently checked historical records

| Record | What it establishes |
|---|---|
| [2018-12-20 README commit](https://github.com/romanspb80/evpn-for-ovn/commit/0ef54bd48547d7bd984124ef5c4d63540516f6b6) | Project activity in 2018; not proof of the exact earliest start date. |
| [2018-12-23 Ryu applications](https://github.com/romanspb80/evpn-for-ovn/commit/113bcc3bf332b802fa84509223348798d60c34a5) and [README at that commit](https://github.com/romanspb80/evpn-for-ovn/blob/113bcc3bf332b802fa84509223348798d60c34a5/README.md) | Python/Ryu EVPN-VXLAN prototype for OVN/OpenStack; REST/RPC separation and RabbitMQ/oslo.messaging. |
| [Historical README snapshot](https://github.com/romanspb80/evpn-for-ovn/blob/2db621f16cf676b39264350f186110d0d28ae996/README.md) and [OVSDB-agent dependencies](https://github.com/romanspb80/evpn-for-ovn/blob/2db621f16cf676b39264350f186110d0d28ae996/docker_files/ovsdb-agent/requirements.txt) | Later Python prototype used Ryu, ovsdbapp and messaging components; its future plans mention GoBGP/libovsdb and CNI. These are historical plans, not proof of modern capabilities. |
| Modern root `38f0821d053b814be2016113d354b80262fafe40` (2026-11-02) | Locally verified parentless Go-framework root; modern and historical implementation histories are deliberately independent. |

Historical dates establish a 2018 origin, not a precise initial-development day.

## Modern framework transitions

The modern Go framework grew from a Kubernetes/Multus integration into reusable
Type-2 domain and endpoint services. Subsequent work added local speaker
attestation and UUID-only route ownership (historical source `ee818b0`),
standalone import (`7b88e01`), protected endpoint authorization (`52c22fa`),
and explicit standalone export, durable revision/generation fencing and
tombstone-based lifecycle. The export smoke and startup correction were
recorded at `750c478`. These are historical source references, not proof of
current behavior or a release candidate.

The canonical [authorization contract](../architecture/standalone-endpoint-authorization.md),
[ownership and recovery contract](../architecture/type2-restart-ownership.md)
and [framework boundaries](../architecture/framework-architecture.md) retain the
technical rules. Current capability and validation status belongs only to the
[capability matrix](../project/capability-matrix.md); outstanding work
belongs to the [backlog](../roadmap/backlog.md).

## Retained validation and evidence policy

- [OVN crash/reference reproduction](../issues/ovn-port-binding-reproducer.md)
  retains the historical isolated-lab record and its limitations.
- [Historical validation record](validation-record.md) indexes distinct
  Framework, CNI and standalone scenarios and their source identities.
- [Operator acceptance](../architecture/operator-type2-acceptance.md) preserves
  the one-chassis FRR topology and descriptor failure investigation.
- [External fabric](../testing/external-evpn-fabric.md) preserves detailed
  topology, scenario criteria and the unresolved final acceptance question.

These are historical observations, not current validation. The versioned
test source and [Testing Guide](../../TESTING_GUIDE.md) provide reproducible
entry points subject to their lab prerequisites; they do not recreate the
original evidence. Most original result manifests and captures were private
and are not retained as public artifacts. Temporary local paths do not make
them durable. Before release or upstream submission, retain a sanitized,
accessible bundle with exact source/build and image provenance. Never execute
historical recipes against shared host OVN/OVS resources.

## Identifier inventory

Historical acceptance identifiers such as A1, C1-C3, D1-D3, E1-E4, F1-F5,
G1-G3, H1 and C0 identify specific assertions in the external fabric matrix;
R3-R6 identify its bounded or final acceptance suites. `O14` names the
OVSDB runner's repeatability gate. They remain to correlate scenarios with
test source and historical reports, not as evidence that an unverified run
passed. Roadmap IDs such as `FW-04` and `SEC-01` identify current tasks.
Older milestone labels survive in existing anchors and detailed records for
link compatibility and provenance; they do not define current status.
