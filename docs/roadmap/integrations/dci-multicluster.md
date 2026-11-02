# DCI and multi-cluster design direction

**Planned, not accepted.** Canonical task:
[INT-02](../backlog.md#p2-planned-capabilities-and-integrations).

Current one-chassis Type-2 tests do not establish routed DCI, cross-cluster
reachability, RR or Type-5 support. Design must first establish:

| Area | Future contract |
|---|---|
| Type-5 / IP-VRF | Prefix ownership, VRF/L3-VNI, gateway/IRB and import/export/withdrawal semantics distinct from Type-2 endpoint routes. |
| Tenant policy | Unique site/router identities and RD allocation, RT import/export and controlled route leaking; overlapping addresses must not escape tenant isolation. |
| VTEPs and peers | Per-site next-hop reachability, MTU/encapsulation and managed/unmanaged VTEP identity; external peers and RR topology with no shared configuration writers. |
| Cross-cluster lifecycle | Network creation/deletion, partial site loss and recovery; never garbage-collect another site's objects or infer global absence from failed observations. |
| Failure handling | Link/peer/RR/controller/daemon failure, partition healing, stale route handling, survivor forwarding and measured convergence. |
| Interoperability | At least two independent sites, explicit third-party peers, route/policy snapshots, packet-path evidence and scoped cleanup. |

Prerequisites are the separately defined multi-VTEP, RR and Type-5 work
(FW-08/FW-09/FW-10). OVN-Kubernetes CUDN/VTEP APIs are potential
[integration inputs](../upstream/ovn-kubernetes.md), not already implemented
Framework contracts. EVPN multihoming and performance evaluation remain
research topics, not first-public-release requirements.
