# EVPN Framework architecture

The Framework reconciles EVPN Type-2 state with OVN-based workloads. EVPN Core
is a logical set of route, ownership and recovery responsibilities embedded
in existing processes; it is not a separate service.

## Component responsibilities

- **GoBGP** owns the BGP sessions and EVPN RIB. The framework uses GoBGP APIs
  for Type-2 path observation, advertisement and exact-path withdrawal. It
  does not configure peers when an external provider owns speaker lifecycle.
- **Node-Agent** hosts the node-local Framework runtime. It can serve CNI
  requests, while standalone EVPN import/export paths can run without the CNI
  service. It observes authorized local endpoints, reconciles routes and
  manages the selected integration state.
- **OVN/OVS** owns logical network state and the workload forwarding
  dataplane. Controllers consume OVN and integration state and program OVS.
- **Linux Netlink** is the selected candidate integration for managing
  ownership-scoped EVPN FDB/neighbor state. Linux bridges and VXLAN devices
  are supporting integration state, not an independent workload dataplane.
- **Kubernetes and Operator** provide an experimental deployment lifecycle;
  project-owned OVN/OVS is the current workload platform. The CNI is a
  secondary network plugin invoked by Multus, not the cluster's primary CNI.

Network-state programming (Netlink versus maintenance/testing-only OVSDB),
deployment lifecycle (standalone Node-Agent versus Kubernetes Operator),
workload endpoint integration (CNI/Multus), platform (project-owned OVN/OVS
versus planned OVN-Kubernetes or Neutron), and BGP engine/provider (GoBGP
versus optional `k8gobgp`) are distinct dimensions. Their names do not define
an arbitrary supported combination matrix. The Operator currently supports
only the `k8gobgp` provider; standalone EVPN runtime flags do not start the
CNI server or configure a standalone BGP provider.

## Control plane and packet forwarding

BGP carries EVPN control-plane routes; it does not carry workload packets.
For the external integration path, OVN/OVS forwards workload traffic through its
VXLAN integration. Within one shared OVN domain, OVN Geneve carries workload
traffic independently of EVPN Type-2. Do not use a BGP session, route, FDB
entry or bridge presence alone as proof of packet forwarding.

## Network-state programming

The Netlink-programmed path is the selected experimental release integration. It consumes validated
GoBGP Type-2 snapshots, checks mapping and ownership, and reconciles exact
Linux integration state. OVN/OVS remains responsible for forwarding.

Direct OVSDB programming is retained only for maintenance and isolated tests;
it is outside selected release deployment scope and is not a supported
alternate release mode. No
benchmark establishes that either integration is faster or superior.

## Authorization and ownership

Route advertisement requires an authorized endpoint observation. Standalone
export additionally requires protected policy, durable fencing, OVN identity
checks and speaker attestation. The real OVN/OVS plus Linux-veth CNI transport
requires root:root Unix peer credentials and `SO_PEERPIDFD` regardless of
continuity configuration. Durable continuity adds stricter caller, workload
and attachment checks.

Mutation and cleanup are scoped to current authority and exact owned receipts.
Unavailable or ambiguous observations do not authorize speculative delete,
adoption or withdrawal. See [CNI continuity](cni-endpoint-continuity.md),
[standalone authorization](standalone-endpoint-authorization.md), and
[Type-2 recovery](type2-restart-ownership.md).

## Status and non-goals

The supported Operator Netlink profile excludes legacy management RPC
services. The legacy compatibility manifest is unauthenticated to permitted
in-cluster callers and is not a supported release profile. The project is
experimental: missed-DEL/orphan and API-outage behavior, generalized
multi-node lifecycle, production operations, and broad platform support are
not established. OVN-Kubernetes and OpenStack integrations remain targets,
not implemented adapters.
