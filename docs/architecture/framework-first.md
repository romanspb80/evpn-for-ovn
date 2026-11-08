# EVPN Framework architecture

EVPN-for-OVN is a modular SDN framework for developing and testing EVPN over
OVN across platform integrations. Kubernetes / Multus is implemented;
OpenStack / Neutron and standalone OVN adapters are planned. The framework
is not primarily a CNI and is not a production-ready cross-platform installer.

Start with the [platform overview](../uml/evpn-overview.svg) and
[modular architecture](../uml/evpn-modular-architecture.svg). Blue identifies
project-owned core responsibilities, amber optional project integration and
slate third-party solutions. Dashed arrows and PLANNED labels mark targets;
blue route-control arrows are distinct from green VXLAN packet paths.
The boxes are logical responsibilities, not newly introduced runtime services.
In particular, **EVPN Core is not an independent standalone runtime**;
platform-independent orchestration remains a design target. The project-owned
EVPN CNI Adapter is distinct from external Multus, which invokes it in
Kubernetes and is not developed or maintained by EVPN-for-OVN.

## Verified component ownership matrix

Ownership here means who implements the component. Mutation authority
(which resources it may change) is narrower and listed separately. The
source links ground the diagrams in this working tree, not in a proposed
package split.

| Component | Implementation owner / category | Current responsibility and source |
|---|---|---|
| Type-2 importer/exporter | Project core | Build/filter routes, complete RIB observation, receipt-owned mutation; [`exporter.go`](../../pkg/evpn/exporter.go), [`type2_gobgp.go`](../../pkg/evpn/type2_gobgp.go), [`remote_type2.go`](../../pkg/evpn/remote_type2.go). |
| Endpoint reconciliation | Project core | Identity-scoped NB logical ports and OVS attachments; [`pkg/endpoint`](../../pkg/endpoint/manager.go), [`pkg/attachment`](../../pkg/attachment/ovs.go). |
| OVN / Netlink integration | Project core | Program owned integration state, not workload packets; [`owned_netlink.go`](../../pkg/evpn/owned_netlink.go), [`ovsdb_programmer.go`](../../pkg/evpn/ovsdb_programmer.go), [`runtime_wiring.go`](../../pkg/nodeagent/runtime_wiring.go). |
| Ownership and recovery | Project core plus runtime wiring | Durable intent/receipts and fail-closed recovery; [`pkg/ownership`](../../pkg/ownership/journal.go), [`export_recovery.go`](../../pkg/evpn/export_recovery.go), [`remote_recovery.go`](../../pkg/evpn/remote_recovery.go); process/provider/CNI identity remains in Node-Agent. |
| Node-Agent | Project integration and orchestration | Hosts reusable services and import/export loops; CNI authority, namespace/veth handling, attested Unix provider transport; [`server.go`](../../pkg/nodeagent/server.go), [`provider_transport.go`](../../pkg/nodeagent/provider_transport.go), [`cni_continuity_runtime.go`](../../pkg/nodeagent/cni_continuity_runtime.go). Not a platform-neutral recovery daemon. |
| EVPN Operator | Project Kubernetes orchestration | YAML-driven singleton workload lifecycle, configuration, readiness, installation journal and finalizer; [`controllers`](../../deploy/operator/controllers/evpncore_controller.go). No endpoint RIB writer. |
| EVPN CNI | Project optional platform integration | CNI ADD/CHECK/DEL and attachment translation via Unix Node-Agent RPC; [`cmd/evpn-cni`](../../cmd/evpn-cni/main.go). Multus itself is reused, not project code. |
| Compatibility management applications | Project applications | [`evpn-mgmt`](../../services/evpn_mgmt/main.go), [`ovsdb-mgmt`](../../services/ovsdb_mgmt/main.go) and [`bgp_ad_handler`](../../services/bgp_ad_handler/main.go). Not a new unified framework service; distinct from the opt-in [`evpn-ovsdb`](../../cmd/evpn-ovsdb/main.go) importer. |
| k8gobgp | Third-party independent provider manager | Owns BGPConfiguration and managed speaker lifecycle; manager and gobgpd share one image. See the [audited upstream inventory](runtime-dependencies.md). Not installed or configured by EvpnCore. |
| GoBGP | Third-party BGP engine | BGP sessions, EVPN RIB and gRPC route operations. Framework client uses `api.GoBgpServiceClient`; the managed fork differs from the client module version. |
| OVN / OVS and Linux | Third-party SDN/runtime | NB/SB databases, northd/controller, OVS packet forwarding; Linux kernel and Netlink provide integration/device state. Project code uses libovsdb and Netlink clients, not a replacement BGP or forwarding engine. |
| Kubernetes / Multus / containerd | Third-party platform/runtime | API workload lifecycle, CNI invocation and independently queried sandbox/process identity in the current recovery path. Not mandatory for all Go domain operations. |
| FRR and remote VTEPs | Third-party external peers | EVPN announcements and peer-side VXLAN forwarding in acceptance. FRR is not the framework's control-plane engine. |
| Containerlab / kind | Third-party test infrastructure | Disposable lab provisioning, not framework runtime components; see [deployment](deployment.md). |
| Neutron / standalone OVN adapters | Planned project integrations | No implemented platform adapter or supported installer; reuse existing logic only after defining platform identity, ownership and recovery contracts. |

## Framework core and existing interfaces

The shared Framework provides:

- GoBGP EVPN Type-2 route construction, validation, observation, import/export,
  and reconciliation.
- Network and endpoint provisioning with ownership-aware cleanup.
- OVN/OVS management and endpoint attachment services.
- Netlink-Programmed and OVSDB-Programmed integration implementations.
- APIs that can be consumed without Kubernetes CNI or Multus.

`pkg/evpn` contains core EVPN and reconciliation logic. `pkg/attachment`,
`pkg/endpoint`, and reusable `pkg/nodeagent` services support direct Framework
tests as well as the CNI application. Logical ownership is separate from the
current Go package layout; packages are not moved merely to match diagrams.

Concrete extension seams already exist: `endpoint.OVNBackend`,
`endpoint.AttachmentManager` and `endpoint.EndpointManager`, the
`evpn.GoBGPType2Adapter` with `ReadinessGate`, and node-agent backend/factory
interfaces. These are not a universal platform plugin API. There is no
implemented interchangeable k8gobgp/standalone provider-deployment interface:
`EvpnCore.spec.bgp.provider` is a forward-compatible configuration selector,
and its validator currently permits only `k8gobgp`.

## Control plane and provider management

GoBGP receives/sends MP-BGP EVPN routes and holds the authoritative RIB.
Framework Type-2 logic validates network/RD/RT/VNI/VTEP scope, reads complete
snapshots, and programs only its owned local advertisements. Exporter deletion
uses exact durable path UUIDs; a failed observation is never an empty RIB.

k8gobgp separately reconciles BGPConfiguration for ASN/router ID, neighbors
and EVPN families. EvpnCore does not create a competing configuration writer
or own the provider's resources. Node-Agent's attested Unix connection verifies
the actual managed speaker and its incarnation; attestation is runtime
integration, not a new BGP engine. Legacy `evpn-mgmt` configuration RPCs exist,
but must not race BGPConfiguration ownership.

The future **standalone BGP provider** means an alternative deployment and
configuration owner for GoBGP, retaining route programming and attestation
boundaries. It is not implemented and `provider: standalone` is rejected.
This is different from the planned **standalone OVN platform adapter**, which
would supply network/endpoint lifecycle without Kubernetes.
See [provider configuration ownership](runtime-dependencies.md#bgpconfiguration-versus-direct-grpc)
and [operator provider selection](evpn-core-operator.md).

## Data plane and integration adapters

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

OpenStack Neutron and standalone OVN are future platform consumers, not
implemented adapters. The existing standalone OVSDB importer and direct
Framework fixtures are real implementations, but not an end-to-end standalone
platform lifecycle. See the [CNI application guide](../applications/cni/README.md)
for its responsibilities and tested lifecycle.

CNI endpoint creation and Type-2 route exchange are separate operations:
ADD does not wait for BGP convergence. In the authenticated creation path,
durable PENDING_CREATION requires verified Unix caller and creation authority;
later independent runtime/OVN/OVS checks permit VERIFIED_ACTIVE and export.
The reusable endpoint managers do not themselves implement Kubernetes
admission. See [creation authority](cni-creation-authority.md) and
[endpoint continuity](cni-endpoint-continuity.md).

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
an experimental YAML-driven lifecycle controller, not a production installer.
It manages the optional Node-Agent DaemonSet and core app/OVN workloads, with
reconciliation-level singleton enforcement, workload readiness and conservative
finalization. Workload Ready is not proof of route convergence or forwarding.
See [operator lifecycle](evpn-core-operator.md).

## Implemented boundaries versus design targets

| Boundary | Implemented now | Remaining design target / limit |
|---|---|---|
| EVPN logic versus platform adapters | Route processing under `pkg/evpn`; direct endpoint/attachment managers can be exercised without CNI. | Complete platform-neutral orchestration/identity API; Node-Agent CNI recovery still depends on Kubernetes/containerd. |
| BGP management versus route programming | Independent k8gobgp/BGPConfiguration owner; Framework owned Type-2 RPCs and separate attestation. | Standalone provider management, deployment and independently verifiable identity contract. |
| CNI endpoints versus route exchange | CNI Unix RPC and asynchronous Type-2 loops with separate activation evidence. | Neutron and standalone endpoint adapters; not a translation of Kubernetes identity assumptions. |
| Kubernetes orchestration versus networking | EvpnCore YAML workload lifecycle separated from domain managers and route reconciliation. | Supported multi-platform installers and general multi-node lifecycle. Current operator is single designated dataplane-node MVP. |
| Route control versus packet forwarding | Framework integration-state mutations; GoBGP route exchange; OVN/OVS/Linux forwarding. | Broader topology and feature acceptance, not a project-owned packet engine. |
| Core versus runtime ownership/recovery | Durable generic journal and resource-specific receipts; Node-Agent process/provider/runtime verification. | A reusable platform-neutral authority layer is a design target, not a separate implemented module. |
| Project code versus dependencies | Project owns its logic/adapters; reuses GoBGP, k8gobgp, OVN/OVS and platform APIs. | No fork-independent compatibility guarantee or universal supported runtime installation. |

The [platform roadmap](../roadmap/integrations/README.md) distinguishes
proposed integration from accepted code. These diagrams do not imply a package
move, a plugin registry, a Neutron agent, or a standalone controller.

## Ownership and bounded recovery

The [recovery contract](type2-restart-ownership.md) binds journals to immutable
installation/node/boot/netns/configuration identity, fences writers, persists
intent before external mutation and records independently verified receipts.
Diagnostics/status JSON is not ownership authority. Foreign matching resources
are not adopted or deleted.

Importer recovery restores only verified exact Netlink receipts and obtains
fresh authoritative RIB observations. Exporter recovery retains UUID ownership
for the attested speaker and obtains fresh receipts after verified replacement.
An AddPath accepted without a durably acknowledged UUID remains intentionally
BLOCKED if the route is retained: the pinned provider cannot recover that lost
UUID from ListPath/WatchEvent. Ambiguous kernel mutations also fail closed.
Administrator-assisted resolution must verify and resolve the exact conflict,
not delete journals blindly. Neither OVSDB-mode generalized restart recovery
nor arbitrary namespace/device/host migration is implied by Phase D.

## Verified scope

Independent one-chassis Framework E2E tests cover Netlink-Programmed and
OVSDB-Programmed behavior separately. Kubernetes CNI Full Lifecycle is a
separate application-level acceptance test. These results do not establish
shared-VTEP lifecycle, two-chassis GoBGP-to-GoBGP operation, multi-VTEP
lifecycle, Type-5, dynamic IPAM, or production installation readiness. The
[Testing Guide](../../TESTING_GUIDE.md) records accepted criteria and evidence;
the [active roadmap](../roadmap/README.md) lists unresolved work.

The [operator-managed Phase D record](operator-type2-acceptance.md) additionally
reports normal and forced-SIGKILL runs, 18/18 each: authenticated real CNI
creation, agent/importer/exporter recovery, speaker re-attestation, real MP-BGP
and post-replacement bidirectional VXLAN forwarding, final UUID withdrawal,
CHECK/DEL, foreign-resource preservation and cleanup. These are bounded,
historical private-lab results, not fresh validation performed for these diagrams
or proof of production readiness.

## Detailed documentation navigation

| Topic | Dedicated documentation |
|---|---|
| Control/data plane | [Integration modes](integrations.md), [mode comparison diagram](../uml/evpn-dataplane-comparison.svg) |
| Provider and dependencies | [Runtime/ownership audit](runtime-dependencies.md) |
| Operator and lab deployment | [EvpnCore lifecycle](evpn-core-operator.md), [kind deployment](deployment.md) |
| CNI lifecycle | [Application guide](../applications/cni/README.md), [creation authority](cni-creation-authority.md), [continuity](cni-endpoint-continuity.md) |
| Standalone endpoint authorization | [Protected local authorization snapshot](standalone-endpoint-authorization.md) |
| Ownership/recovery and acceptance | [Restart contract](type2-restart-ownership.md), [operator Type-2 acceptance](operator-type2-acceptance.md), [Testing Guide](../../TESTING_GUIDE.md) |
| Platform roadmap | [Integration targets](../roadmap/integrations/README.md), [framework capabilities](../roadmap/framework.md) |

The [API reference](../api/README.md) separates compatibility management RPCs
from reusable Go domain operations and accepted Type-2 ownership.
In particular, today's `ProvisionVni` handler only updates an in-memory
registry; it does not implement the domain `CreateVNI(LS,VNI)` contract.
