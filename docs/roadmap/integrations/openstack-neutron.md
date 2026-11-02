# OpenStack Neutron integration design

**Planned, not implemented.** Canonical task:
[INT-01](../backlog.md#p2-planned-capabilities-and-integrations).

A Neutron ML2/OVN integration could translate network/port lifecycle into
Framework domain operations while retaining Neutron's ownership of existing
OVN switches and ports. It must not import Kubernetes-specific NAD, Multus,
Pod identity or EvpnCore assumptions. Reusable starting points are
`pkg/endpoint`, `pkg/attachment`, `pkg/evpn` and the non-CNI services in
`pkg/nodeagent`; not every domain method is exposed as a network RPC today.

| Contract to design | Required boundary |
|---|---|
| Network / port identity | Neutron network/port and revision identity, tenancy, idempotent replay, stale delete protection and explicit adoption versus managed creation. |
| Management API | Authentication/authorization, versioning, errors, persistence and recovery. Current compatibility gRPC methods are not a complete supported Neutron service. |
| OVN resource ownership | Reuse existing NB objects by verified provenance; scope new integration objects; leave SB bindings, patch ports and OVS forwarding to OVN controllers. |
| GoBGP EVPN | One authoritative speaker/peer writer plus receipt-owned Type-2 paths; no dependency on k8gobgp when deployed outside Kubernetes. |
| Netlink versus OVSDB | Select one adapter for each network; retain separate Linux and provider-bridge ownership, VTEP lifecycle and withdrawal contracts. |

Acceptance must include create/update/delete, failed and repeated calls,
controller/speaker restart, tenant isolation, bidirectional OVN/OVS traffic,
withdrawal and last-owner cleanup on a disposable OpenStack/OVN test
environment. Current kind/FRR evidence does not satisfy this gate.
