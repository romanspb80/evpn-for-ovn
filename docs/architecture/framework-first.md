# EVPN Framework architecture

EVPN for OVN is an experimental, reusable Go Framework for EVPN route
processing and OVN/OVS connectivity. This document describes the current
implementation boundaries; it does not claim production readiness.

## Framework core

The shared Framework provides:

- GoBGP EVPN Type-2 route construction, validation, observation, import/export,
  and reconciliation.
- Network and endpoint provisioning with ownership-aware cleanup.
- OVN/OVS management and endpoint attachment services.
- Adapter interfaces for Netlink-Programmed and OVSDB-Programmed integrations.
- APIs that can be consumed without Kubernetes CNI or Multus.

`pkg/evpn` contains core EVPN and reconciliation logic. `pkg/attachment`,
`pkg/endpoint`, and reusable `pkg/nodeagent` services support direct Framework
tests as well as the CNI application. Logical ownership is separate from the
current Go package layout; packages are not moved merely to match diagrams.

## Integration adapters

The Framework supports two alternative integration modes:

| Integration | Responsibility |
|---|---|
| Netlink-Programmed | Optional GoBGP Type-2 importer/exporter reconciles owned Linux EVPN integration state with Netlink. `ovn-controller` observes the state and programs OVN/OVS workload forwarding. |
| OVSDB-Programmed | Opt-in standalone route importer reads the GoBGP gRPC RIB, validates Type-2 routes, and reconciles route-derived OVN/OVSDB provider-bridge, localnet, and VXLAN state. |

OVN/OVS owns workload forwarding in both modes. Linux bridge, VXLAN, FDB,
and neighbor entries in the Netlink integration are not a separate workload
dataplane. The integration boundary follows the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration),
without claiming support for every upstream feature.

See [integration modes](integrations.md) for ownership, test scope, and limits.

## Applications and shared services

Kubernetes CNI/Multus is a current application consumer. `evpn-cni` handles
ADD/CHECK/DEL and CNI result semantics; node-agent orchestration resolves
Kubernetes identity and coordinates namespace/interface lifecycle with shared
Framework services. The whole node-agent is not CNI-only.

OpenStack Neutron and other orchestrators are possible future consumers, not
implemented applications. See the [CNI application guide](../applications/cni/README.md)
for its responsibilities and tested lifecycle.

## Runtime ownership

In the kind lab, upstream `k8gobgp` is the GoBGP speaker and FRR is an external
EVPN peer. `evpn-mgmt` exposes EVPN management operations; `ovsdb-mgmt` exposes
OVN/OVSDB operations. The checked-in `bgp_ad_handler` service path is distinct
from the opt-in standalone `cmd/evpn-ovsdb` Framework importer.

OVN NBDB describes logical network state; `ovn-northd` translates it to
Southbound logical flows and port bindings. `ovn-controller` owns local
binding and controller-managed OVS forwarding. Framework
reconcilers remove only resources they own and must not delete
controller-managed objects directly.

The kind deployment and its generated test components are described in the
[deployment guide](deployment.md). The optional `EvpnCore` operator is
experimental scaffolding, not a Framework API or production installer.

## Verified scope

Independent one-chassis Framework E2E tests cover Netlink-Programmed and
OVSDB-Programmed behavior separately. Kubernetes CNI Full Lifecycle is a
separate application-level acceptance test. These results do not establish
shared-VTEP lifecycle, two-chassis GoBGP-to-GoBGP operation, multi-VTEP
lifecycle, Type-5, dynamic IPAM, or production installation readiness. The
[Testing Guide](../../TESTING_GUIDE.md) records accepted criteria and evidence;
the [active roadmap](../roadmap/README.md) lists unresolved work.

The [API reference](../api/README.md) separates compatibility management RPCs
from reusable Go domain operations and accepted Type-2 ownership.
In particular, today's `ProvisionVni` handler only updates an in-memory
registry; it does not implement the domain `CreateVNI(LS,VNI)` contract.
