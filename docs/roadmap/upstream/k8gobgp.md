# k8gobgp ownership and contribution opportunities

**Current integration:** digest-pinned v0.2.8 manager plus managed gobgpd in
one DaemonSet image. Accepted suites use the local Unix gRPC endpoint.
The [runtime dependency audit](../../architecture/runtime-dependencies.md)
records exact image, release-manifest checksum, source revision, schema and
the configuration writer matrix. The historical daemon version is
`PureLB-fork:01.03.08`, commit `6b99046`, not the Framework client version.

`BGPConfiguration → k8gobgp reconciliation → managed gobgpd` owns identity,
peers, EVPN AF and VRFs. The schema provides RD/import/export RT for VRFs but
not a per-endpoint declarative Type-2 MAC/IP/VNI path collection. The
Framework therefore uses GoBGP AddPath and exact-UUID DeletePath separately.
Upstream connected-route Netlink import/kernel route export exists; it is
disabled in acceptance to avoid competing writers, not because Netlink is
generally unsupported.

| Opportunity | Classification and required evidence |
|---|---|
| Explain CRD versus external path writer ownership and Unix-socket lifecycle | Proposed upstream documentation improvement; map real manager reconciliation and test controller/daemon restart behavior first ([FW-02/FW-04](../backlog.md#p1-reliability-and-contract-clarification)). |
| Declarative EVPN endpoint intent | Proposed API improvement, not a confirmed missing GoBGP capability. Decide whether endpoints belong in k8gobgp at all; avoid duplicate Framework ownership and require withdrawal/recovery tests. |
| EVPN-aware Netlink reconciliation | Research question for k8gobgp versus purelb/gobgp-netlink. Audit exact selected dependency and FDB/neighbor/VNI/VTEP semantics before proposing changes. |
| Type-5 / RR | No Framework acceptance. A provider schema field or daemon route-family capability does not establish this integration's routed dataplane or RR behavior. |
| Remote BGP authentication | `authPasswordSecretRef` already implemented in v0.2.8 for neighbors/peer groups; do not propose adding it. Exact-key lookup and Secret-only rotation/reconciliation gaps are source-backed; live interoperability remains [SEC-01](../backlog.md#sec-01). |

EvpnCore does not manage BGPConfiguration, upstream CRDs or the DaemonSet.
Its `gobgpImage` field is currently inert. Do not remove the accepted
CRD-managed setup or configure peers concurrently through evpn-mgmt.
The [peer-security plan](bgp-peer-security.md) links immutable v0.2.8 types,
resolver and watcher/comparison behavior, plus MD5/AO/GTSM/ACL/policy acceptance.
The [unsubmitted rotation/documentation proposal](../../issues/k8gobgp-peer-auth-rotation-proposal.md)
requests investigation of a source-supported gap, not duplicate Secret support.
No matching public issue/PR is verified; rotation duplicate review and live
reproduction remain pending. No contribution was submitted.
