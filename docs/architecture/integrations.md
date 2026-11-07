# Framework integration modes

The EVPN Framework provides shared GoBGP route processing, network and
endpoint services, ownership-aware reconciliation, and OVN/OVS integration.
Its two dataplane integrations are alternatives. OVN/OVS owns workload
forwarding in both. These are networking integration paths, not platform
adapters: neither the opt-in standalone importer nor these test fixtures
implements the planned standalone OVN platform lifecycle.
See the [framework boundary matrix](framework-first.md) and
[modular diagram](../uml/evpn-modular-architecture.svg).

## Netlink-Programmed

The optional Netlink integration consumes GoBGP EVPN Type-2 route state and
reconciles ownership-scoped Linux EVPN integration state with Netlink. Local
endpoint export is also available through the node-agent's Framework services.
The path uses per-VNI Linux state and shared per-chassis OVS EVPN configuration;
`ovn-controller` observes the integration state and owns OVS forwarding.

The Linux bridge, VXLAN, FDB, and neighbor state is not a separate workload
dataplane. This integration follows the boundary described by the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration),
without claiming support for unverified upstream features.

The optional importer/exporter is disabled by default. Independent Framework
E2E covers one OVN chassis with an external FRR peer and verifies Type-2
exchange, OVN/OVS workload traffic, withdrawal, survivor preservation, and
owned cleanup.

## OVSDB-Programmed

The opt-in standalone importer reads the GoBGP gRPC RIB without modifying it,
validates Type-2 routes, and passes accepted snapshots to the OVSDB handler.
The route-derived state includes an OVN localnet logical switch port, provider
bridge and bridge mapping, and an OVS VXLAN interface/port. `ovn-controller`
reconciles the OVN state into the workload forwarding path.

Route withdrawal removes only state owned by the integration. Cleanup respects
OVN controller ownership and preserves unrelated bridge ports and interfaces.
The importer is standalone and is not enabled by the standard kind deployment.

The verified independent Framework E2E covers one network and one remote
VTEP. It does not establish multi-VTEP lifecycle, shared-VTEP sibling
lifecycle, or two-chassis GoBGP operation.

## Ownership and limits

Each adapter owns only the state it creates and can identify. OVN owns
controller-generated bindings and patch ports; the integration must wait for
controller reconciliation rather than deleting controller-owned objects.
GoBGP owns the control-plane RIB, while FRR is an external peer used for
interoperability testing.

For managed k8gobgp, BGPConfiguration is the sole writer of global identity,
neighbors and EVPN AFI/SAFI. Framework gRPC writes are limited to owned
Type-2 paths; the adapters own only their integration state. See the
[release-specific capability and ownership audit](runtime-dependencies.md)
before enabling upstream Netlink writers or calling legacy management
`StartBgp`/`AddPeer` APIs.

The operator-managed Netlink path has bounded
[Phase D recovery acceptance](operator-type2-acceptance.md), including
agent/speaker replacement and post-recovery forwarding/withdrawal. That does
not confer equivalent restart guarantees on the OVSDB path or authorize
ambiguous AddPath/Netlink adoption. The integrations do not establish EVPN
Type-5, dynamic IPAM, generalized restart/adoption guarantees, or production
installation readiness. The current
acceptance evidence and exact test commands are in the
[Testing Guide](../../TESTING_GUIDE.md).
