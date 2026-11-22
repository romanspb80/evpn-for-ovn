# Capabilities and integration maturity

Implemented functionality, test scenarios and operational support are
different concepts. This matrix describes the experimental v0.1.0 scope,
not a historical test scorecard or a production support guarantee. Its
architecture dimensions are independent concepts; this is not a promise that
every possible combination is implemented or supported.

| Architecture dimension | Current scope | Boundary |
|---|---|---|
| Network-state programming | Netlink is the selected experimental release integration | Direct OVSDB is retained for maintenance and testing only, outside release deployment scope |
| Deployment and orchestration | Standalone Node-Agent EVPN import/export runtime, or Kubernetes Operator lifecycle | Standalone paths do not require the Operator and do not start a CNI server; a CNI-serving Node-Agent may be independently started for secondary-network service; Kubernetes profiles are constrained, not a general deployment matrix |
| Workload integration | CNI/Multus secondary attachment and OVN endpoint lifecycle | Separate from the cluster primary CNI and from standalone non-CNI EVPN paths |
| Platform integration | Project-owned OVN/OVS is current | OVN-Kubernetes and Neutron / ML2-OVN are planned, not implemented platform adapters |
| BGP/EVPN control plane | GoBGP engine; optional `k8gobgp` Kubernetes speaker provider | Operator accepts only `k8gobgp`; no standalone Operator provider is implemented |
| Vendor EVPN/VXLAN interoperability | Planned Type-2 peer test program | FRR is the planned reference; Nokia SR Linux, Juniper cJunosEvolved, NVIDIA Cumulus VX and Arista cEOS await authorized image and lab readiness; no vendor pass is claimed |

| Area | Implementation / maturity | Boundary |
|---|---|---|
| Netlink-Programmed Type-2 | Implemented; experimental | GoBGP route processing and owned Linux integration state; OVN/OVS workload forwarding. |
| Standalone exporter/importer co-hosting | Implemented; experimental | Standalone route import/export paths are separate from Operator lifecycle and CNI serving. |
| Two-Chassis EVPN Type-2 | Implemented and validated in v0.1.0 | Two OVN/OVS chassis share NBDB/SBDB and exchange Type-2 routes through GoBGP, with VXLAN data-plane forwarding. This is not independent OVN-domain interconnection. |
| Operator inventory-gated multi-worker profile | Implemented; experimental | Bounded per-worker external forwarding; not generalized worker-to-worker traffic or lifecycle support. |
| Exact owned-route withdrawal / foreign-route preservation | Implemented; bounded validation | Route withdrawal is scoped to owned receipts; arbitrary foreign-state preservation is not a release guarantee. |
| Endpoint delete, replacement and controlled migration | Implemented; bounded lifecycle | Does not promise zero-loss mobility or generalized multi-VTEP support. |
| Kubernetes CNI / Multus | Implemented experimental secondary ADD/CHECK/DEL lifecycle when the CNI-serving Node-Agent is enabled | Project-owned shared OVN domain; separate from the primary CNI and standalone non-CNI runtime |
| Operator-managed Node-Agent | Experimental lifecycle and worker-inventory publication | Kubernetes readiness is not route convergence or node-local ownership acceptance |
| Standalone EVPN runtime | Opt-in import/export with protected configuration | Administrator authorization, OVN identity, speaker attestation and durable fencing required; no CNI server or standalone Operator provider |
| CNI peer transport | Mandatory root:root `SO_PEERCRED` and `SO_PEERPIDFD` | Independent of optional continuity; does not protect against host compromise |
| Agent / speaker recovery | Implemented; experimental | Durable journals, exact receipts and identity-gated restart paths; generalized recovery and availability are not guaranteed |
| Missed DEL / orphan recovery | Exact-receipt retirement path | Operational acceptance incomplete; API absence and ambiguous host state must fail closed |
| Kubernetes API-unavailability | Authority gating and inventory recovery paths | Operational acceptance incomplete; an unavailable API does not prove endpoint retirement |
| Multi-worker / multi-VTEP | Worker-local inventory and test templates | Generalized multi-VTEP lifecycle and arbitrary foreign-state preservation are not guaranteed |
| OVSDB-Programmed | Maintenance and isolated tests only | Outside selected v0.1.0 deployment scope; advanced regressions deferred |
| OVN-Kubernetes / Neutron ML2-OVN | Planned adapters | Not implemented platform integrations |
| EVPN Type-3 / Type-5 | Outside implemented Type-2 release scope | No full BUM membership or routed Type-5 service claim |
| Final v0.1.0 release acceptance | All tests in the final revised inventory are reported as passing | The revised inventory and final evidence bundle are not included here; no numeric count is asserted. See the [validation-history archive](../internal/validation-history/README.md). |
| Production operations | Not qualified | Privileges, upgrades, rollback, platform support and availability remain open |

The [two networking use cases](../use-cases/README.md) are validated two-chassis
EVPN Type-2 connectivity and experimental external EVPN/VXLAN integration.
Independent OVN-domain interconnection is planned in the
[roadmap](../roadmap/README.md#independent-ovn-domain-interconnection-planned).
CNI/Multus, standalone
OVN and Operator are deployment/integration mechanisms; multi-worker placement
is a topology, and ownership/recovery is a lifecycle requirement. This
presentation does not change any matrix entry or its acceptance boundary.
The [vendor interoperability plan](../roadmap/README.md#3-vendor-evpnvxlan-interoperability)
is future test planning, not release-snapshot acceptance or a production
interoperability guarantee.

See [deployment and operations](../guides/deployment-and-integration.md),
[topology scenarios](../guides/network-topologies.md),
[reliability and lifecycle](../guides/reliability-and-lifecycle.md),
[release notes](../release/v0.1.0.md),
[test scenarios](../../TESTING_GUIDE.md),
[roadmap](../roadmap/README.md),
[security contract](../security/netlink-deployment-security-contract.md),
[known limitations](../known-issues/README.md), and
[validation-history archive](../internal/validation-history/README.md).
