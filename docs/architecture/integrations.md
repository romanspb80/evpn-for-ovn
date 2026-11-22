# Network-state programming

EVPN for OVN uses GoBGP for BGP/EVPN control-plane state and OVN/OVS for
workload forwarding. The Netlink integration reconciles Linux state consumed
by OVN/OVS; it does not replace OVN/OVS as the workload dataplane. A Linux
bridge is not an independent workload forwarding path.

## Netlink-Programmed — selected experimental release integration

The framework reads complete GoBGP Type-2 snapshots, validates network/VNI/RT
and next-hop policy, and reconciles only owned Linux FDB/neighbor entries.
Local export observes authorized OVN endpoint state and advertises exact
Type-2 paths through GoBGP. Ownership receipts and stable observations gate
mutation and withdrawal. OVN controllers consume the integration state and
program the OVS forwarding path.

The real OVN/OVS plus Linux-veth CNI mode requires root:root
`SO_PEERCRED` and kernel `SO_PEERPIDFD` on the connected Unix peer. This
requirement is independent of continuity configuration. Standalone export
uses protected authorization policy and actual speaker attestation.

## OVSDB-Programmed — maintenance and tests only

The direct OVSDB code is retained for basic maintenance and isolated regression
tests. It is not the selected candidate forwarding mode and does not have
release-level restart/withdrawal acceptance; it is outside selected release
deployment scope. No equivalence or performance comparison with the Netlink
path is claimed.

## Boundaries

- BGP routes are control-plane state; packet forwarding must be independently
  verified through OVN/OVS and packet evidence.
- OVN Geneve provides connectivity within a shared OVN domain; this does not
  depend on EVPN Type-2.
- FRR is an external peer in bounded interoperability fixtures, not a
  replacement project control plane.
- `bgp_ad_handler` and legacy management RPC services are not part of the
  supported Operator Netlink profile.
- Benchmark-only provenance bypass options are excluded from supported secure
  profiles and acceptance runs.
- These programming choices are separate from deployment lifecycle,
  CNI/Multus endpoint integration, platform and provider choice. The current
  platform is project-owned OVN/OVS; OVN-Kubernetes and Neutron / ML2-OVN are
  planned. The Operator currently accepts only the `k8gobgp` provider.

See [component responsibilities](framework-architecture.md),
[network topologies](network-topologies.md), and the [candidate record](../release/v0.1.0.md).
