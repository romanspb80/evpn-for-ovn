# Control-plane compatibility and integration candidates

Reviewed 2026-11-08. This evidence index is not a task register or an upstream
bug tracker. The five classifications are shared with the
[OVN/OVS registry](ovn-ovs-issues.md#classification). No item below is a
confirmed upstream defect.

| Local ID | Upstream / tested scope | Classification | Expected vs actual / evidence | Workaround | Next action / readiness |
|---|---|---|---|---|---|
| BGP-001 | osrg/gobgp client v4.5.0; selected PureLB-fork:01.03.08 / `6b99046` daemon | Expected upstream behavior (selected API contract; intent needs clarification) | Durable AddPath UUID needed for ownership; selected ListPath omits UUID. Adapter validates payload against retained UUID. `pkg/evpn/type2_gobgp.go`, tests and live receipt bundle support local behavior, not a stock-version defect. | Retain exact durable AddPath receipt; UUID-only withdrawal; ambiguous AddPath BLOCKED. | [UP-02](../roadmap/backlog.md#up-02), [API proposal](../issues/gobgp-type2-api-proposal.md); stock/fork comparison and duplicate review pending. |
| BGP-002 | osrg/gobgp client v4.5.0 apiutil; same selected live runtime | Research or enhancement proposal (confirmed client compatibility constraint) | Valid absent-IP Type-2 should have a supported encoding; typed apiutil path yields `invalid IP`, binary serialization works. `pkg/evpn/type2_gobgp.go` MAC-only branch/test evidence; no current-stock RCA. | Binary NLRI; do not invent an IP or change withdrawal authority. | [UP-02](../roadmap/backlog.md#up-02), same [proposal](../issues/gobgp-type2-api-proposal.md); minimized upstream comparison pending. |
| BGP-003 | purelb/k8gobgp v0.2.8 / `71b26cb8ea5d48762c00f764e0204ce03e9b6e46` | Research or enhancement proposal | Existing Secret auth works through exact selected key; no Secret watch, successful periodic requeue or nonempty password-value comparison in inspected neighbor path. Source-supported rotation gap, not live reproduced failure. | No validated hot-rotation workaround; retain sole controller writer. | [SEC-01 / FW-02](../roadmap/backlog.md), [rotation proposal](../issues/k8gobgp-peer-auth-rotation-proposal.md); live matrix/duplicate review pending. |
| INT-OVNK-001 | ovn-kubernetes/ovn-kubernetes moving master documentation; no tested runtime version | Research or enhancement proposal | Documented EVPN uses FRR/frr-k8s-specific contracts; proposed optional GoBGP seam needs design, not defect correction. | Retain upstream FRR behavior; external integration first. | [INT-03](../roadmap/backlog.md#int-03), [architecture proposal](../issues/ovn-kubernetes-gobgp-provider-proposal.md); source pin/experiment/duplicates pending. |

No existing upstream issue/PR or verified fix is assigned. Preliminary GoBGP
searches were not sufficient to establish duplicates or their absence.
Remote BGP MD5/AO/GTSM/ACL/policy and secret rotation are tracked under
[SEC-01](../roadmap/backlog.md#sec-01), separate from local attestation and
endpoint authorization. Current smoke is not authenticated-peer acceptance.
