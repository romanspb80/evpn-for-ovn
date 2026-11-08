# GoBGP API and compatibility

**Verified local usage:** Framework client
`github.com/osrg/gobgp/v4 v4.5.0`; managed runtime is the separately pinned
PureLB fork, not a stock daemon with the same version. Exact identities are
in the [dependency inventory](../../architecture/runtime-dependencies.md).

[type2_gobgp.go](../../../pkg/evpn/type2_gobgp.go) uses complete global EVPN
ListPath snapshots, AddPath returning UUID and DeletePath with the trusted
UUID. ListPath in the pinned API omits UUIDs; the adapter retains the
AddPath receipt, validates matching payload and refuses NLRI-based deletion
fallback. MAC-only routes use binary NLRI serialization because the pinned
apiutil typed path encodes an absent IP as `invalid IP`.
These are **confirmed compatibility constraints**, not proof of a current
upstream bug in another release.

The compatibility evpn-mgmt AddClient/DelClient implementation is different:
it does not retain those ownership receipts. See the
[API reference](../../api/evpn-mgmt.md). Do not use it as evidence that the
accepted Framework exporter lacks exact withdrawal.

| Area | Classification |
|---|---|
| Client/runtime version and protobuf reproducibility | Local Framework work, [API-01](../backlog.md#p1-reliability-and-contract-clarification). Pin and test actual wire behavior before changing versions. |
| UUID readback and absent-IP encoding | Candidate upstream clarification/improvement only after reproducing on the intended upstream release and searching existing issues/PRs. No contribution URL verified here. |
| gobgp-netlink EVPN FDB/neighbor lifecycle | Research question; inspect purelb/gobgp-netlink selected by k8gobgp, rather than attributing all behavior to osrg/gobgp. General kernel route import/export already exists. |
| Type-5, RR and multihoming | Local planned/research work, not a demonstrated missing GoBGP protocol feature. Framework tests and ownership design remain required. |

Upstream project references:
[osrg/gobgp](https://github.com/osrg/gobgp),
[purelb/gobgp-netlink](https://github.com/purelb/gobgp-netlink).
No existing project contribution or matching public issue/PR was verified.

Canonical compatibility triage is [UP-02](../backlog.md#up-02); the
[registry](../../known-issues/control-plane-issues.md) separates expected
selected-API UUID omission from absent-IP encoding investigation.
The [unsubmitted proposal](../../issues/gobgp-type2-api-proposal.md) specifies
stock/fork comparisons, evidence and pending duplicate review. Preliminary
issue searches did not establish a matching defect or fix.
Remote peer security, including current-source TCP-AO versus pinned-runtime
support, is separate [SEC-01 work](bgp-peer-security.md), not local attestation.
