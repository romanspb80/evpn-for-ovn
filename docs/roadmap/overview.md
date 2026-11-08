# Roadmap overview

[Roadmap index](README.md) | [Canonical backlog](backlog.md) |
[Implementation status](../../IMPLEMENTATION_README.md) |
[Known issues](../known-issues/README.md) | [Release gates](release.md)

Derived from the canonical register, reviewed 2026-11-08. This dashboard contains
no independent acceptance criteria: change the backlog first, then synchronize
the status, dependency and action cells here. Status vocabulary is
**Proposed**, **Investigating**, **Blocked**, **In Progress**, **Verified**, **Done**.
Verified is bounded validation; Done is completion of the stated task, not a
production-readiness label.

## Summary

39 records: 9 completed/bounded milestones (1 Done, 8 Verified), 30 open.
Open priorities: P0 **4**, P1 **12**, P2 **12**, P3 **2**.
REL-04 and REL-05 are Blocked; other tasks can have unmet dependencies without
being marked Blocked until their execution is actually blocked.

G.6 speaker ownership, G.7a importer and G.7b.1-5 authorization/fencing/export/
tombstone code are implemented. G.7b.5a single-chassis live smoke and the existing
operator regression passed with accessible evidence. G.7b implementation and
co-hosted startup regressions are committed locally at `750c478`; live co-hosting
is NOT TESTED. G.7b.5b two-chassis forwarding and live migration remain open.

Upstream collaboration affects FW-02 / SEC-01 (existing k8gobgp configuration),
UP-01 (OVN triage), UP-02 (GoBGP API), FW-05 (FDB investigation), FW-11 and INT-03
(OVN-Kubernetes design). These are research/triage dependencies, **not** verified
upstream fixes awaiting merge. No issue has been submitted by this consolidation.

Next release gates are exact-snapshot security/licensing/object review, publicly
reproducible inputs and clean runs, then explicit maintainer approval. A local
smoke PASS does not satisfy those gates or certify production readiness.

## Master dashboard

| ID | Component | Task | Priority | Status | Dependencies | Next Action | Upstream / Issue |
|---|---|---|---|---|---|---|---|
| [REL-01](backlog.md#rel-01) | Release licensing | Provenance audit | P0 | Done | None | Preserve notices; review new distribution. | None |
| [REL-02](backlog.md#rel-02) | Project history | Migration hygiene / independent root | P0 | In Progress | REL-01 | Reconcile migrated scope; preserve histories. | None |
| [REL-03](backlog.md#rel-03) | Release screening | Next-snapshot security/object review | P0 | Investigating | REL-02 | Review exact snapshot and object/ref closure. | None |
| [REL-04](backlog.md#rel-04) | Reproducibility | Clean release acceptance | P0 | Blocked | REL-01, REL-02, REL-03 | Obtain reproducible inputs and run gates. | None |
| [REL-05](backlog.md#rel-05) | Publication | Maintainer approval | P0 | Blocked | REL-04, maintainer approval | Await explicit approval. | None |
| [FW-01](backlog.md#fw-01) | Network lifecycle | Authoritative per-VNI cleanup | P1 | Investigating | None | Specify ownership and restart/deletion. | None |
| [FW-02](backlog.md#fw-02) | BGP configuration | Sole-writer reconciliation | P1 | Investigating | None | Test writer and compatibility boundaries. | [k8gobgp](upstream/k8gobgp.md) |
| [FW-03](backlog.md#fw-03) | Endpoint rollback | Failed attachment cleanup | P1 | Proposed | None | Inject failure after LSP creation. | None |
| [FW-04](backlog.md#fw-04) | Recovery | Generalized failure matrix | P1 | Investigating | FW-02 | Extend bounded recovery evidence. | None |
| [FW-05](backlog.md#fw-05) | Provider FDB | Partial-withdrawal timing | P1 | Investigating | None | Measure per-MAC invalidation. | [OVS-001](../known-issues/ovn-ovs-issues.md) |
| [K8S-01](backlog.md#k8s-01) | Operator hardening | Production lifecycle | P1 | In Progress | FW-02 | Validate storage, upgrades and placement. | None |
| [API-01](backlog.md#api-01) | Management API | Versioned conformance | P1 | Proposed | FW-01, FW-02 | Define errors and idempotency tests. | None |
| [API-02](backlog.md#api-02) | Management security | Authenticated transport | P1 | Proposed | None | Define secure migration/containment. | None |
| [UP-01](backlog.md#up-01) | OVN triage | Crash / SB reference analysis | P1 | Investigating | None | Minimize and complete duplicate checks. | [OVN draft](../issues/ovn-port-binding-upstream-draft.md) |
| [SEC-01](backlog.md#sec-01) | Remote BGP security | MD5/AO/GTSM/ACL/policy/rotation | P1 | Proposed | FW-02 | Test existing Secret auth and rotation. | [Security plan](upstream/bgp-peer-security.md) |
| [UP-02](backlog.md#up-02) | GoBGP API compatibility | UUID / MAC-only triage | P1 | Investigating | None | Compare stock/fork and duplicates. | [GoBGP draft](../issues/gobgp-type2-api-proposal.md) |
| [G.7b.5b](backlog.md#g7b5b) | Standalone integration | Two-chassis / live migration | P1 | Proposed | G.7b.5a, FW-07, FW-04 | Extend existing fixtures. | None |
| [FW-06](backlog.md#fw-06) | Shared-VTEP lifecycle | Last-owner cleanup | P2 | Proposed | FW-04, FW-05 | Test both adapters. | None |
| [FW-07](backlog.md#fw-07) | Two-chassis EVPN | Bidirectional traffic | P2 | Proposed | FW-04 | Define two-GoBGP/OVN acceptance. | None |
| [FW-08](backlog.md#fw-08) | Multi-VTEP | Scoped topology lifecycle | P2 | Proposed | FW-06, FW-07 | Extend after sharing tests. | None |
| [FW-09](backlog.md#fw-09) | Route reflection | RR policy / failover | P2 | Proposed | FW-07 | Define provenance scenarios. | None |
| [FW-10](backlog.md#fw-10) | Type-5 / IP-VRF | Prefix ownership | P2 | Proposed | FW-02 | Define routed acceptance. | None |
| [FW-11](backlog.md#fw-11) | SVD / VLAN-VNI | Device sharing design | P2 | Proposed | FW-06 | Compare allocation ownership. | OVN-Kubernetes research |
| [FW-12](backlog.md#fw-12) | Dynamic IPAM | Allocation lifecycle | P2 | Proposed | FW-03 | Define release/incarnation safety. | None |
| [K8S-02](backlog.md#k8s-02) | Proposed network CRDs | Intent translation | P2 | Proposed | FW-01, FW-02, K8S-01 | Review before adding schemas. | None |
| [INT-01](backlog.md#int-01) | Neutron / ML2-OVN | Platform adapter | P2 | Proposed | FW-01, API-01, API-02 | Define authenticated port lifecycle. | OpenStack research |
| [INT-02](backlog.md#int-02) | DCI / multi-cluster | Cross-site isolation | P2 | Proposed | FW-08, FW-09, FW-10 | Define identity/RT/MTU boundaries. | None |
| [INT-03](backlog.md#int-03) | OVN-Kubernetes | Provider seam proposal | P2 | Investigating | FW-02, FW-10, FW-11 | Review external integration with upstream. | [Proposal](../issues/ovn-kubernetes-gobgp-provider-proposal.md) |
| [DEP-01](backlog.md#dep-01) | Production installation | Supported deployment lifecycle | P2 | Proposed | FW-02, FW-04, API-02 | Define distributions and upgrades. | None |
| [RS-01](backlog.md#rs-01) | Multihoming | ESI/DF/failover design | P3 | Proposed | FW-07, FW-08 | Choose design before implementation. | None |
| [RS-02](backlog.md#rs-02) | Performance / convergence | Scale and packet loss | P3 | Proposed | FW-04, FW-07 | Define reproducible measurements. | None |
| [G.6](backlog.md#g6) | Speaker ownership | Standalone attestation / exact receipts | P1 | Verified | None | Retain guarantees in FW-04 / SEC-01. | GoBGP (reused) |
| [G.7a](backlog.md#g7a) | Standalone importer | CNI-free importer runtime | P1 | Verified | G.6 | Extend live coverage in G.7b.5b. | None |
| [G.7b.1](backlog.md#g7b1) | Endpoint authorization | Trusted endpoint contract | P1 | Verified | G.6 | Keep policy validation mandatory. | None |
| [G.7b.2](backlog.md#g7b2) | OVN observation | Authoritative NB/SB intersection | P1 | Verified | G.7b.1 | Add multi-chassis uncertainty tests. | OVN (reused) |
| [G.7b.3](backlog.md#g7b3) | Durable authorization | Revision / generation fence | P1 | Verified | G.7b.1 | Extend interrupted-write/replay E2E. | None |
| [G.7b.4](backlog.md#g7b4) | Standalone exporter | Opt-in runtime / co-hosting | P1 | Verified | G.6, G.7b.2, G.7b.3 | Preserve startup order and opt-in. | None |
| [G.7b.5](backlog.md#g7b5) | Lifecycle withdrawal | Exact lifecycle tombstones | P1 | Verified | G.7b.2, G.7b.3, G.7b.4 | Run live tombstone lifecycle E2E. | None |
| [G.7b.5a](backlog.md#g7b5a) | E2E smoke / cache | Single-chassis revocation / restart | P1 | Verified | G.7b.4, G.7b.5 | Await separately authorized G.7b.5b. | None |
