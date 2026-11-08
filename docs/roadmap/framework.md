# Framework capabilities and evolution

The Framework's Go route and endpoint services can be exercised independently
of Kubernetes, CNI and Multus. Node-Agent has explicit standalone-attested
import and authorized-export modes; CNI authority/recovery and EvpnCore
orchestration remain Kubernetes/runtime integrations. GoBGP provides
EVPN control-plane services; OVN/OVS owns workload forwarding in **both**
integration modes. This page describes capabilities, not a second task list.
All priorities and acceptance criteria are in the [backlog](backlog.md).

## Implemented and verified within a bounded scope

| Capability | Evidence and limit |
|---|---|
| Type-2 construction/validation and export | `pkg/evpn/type2.go`, `type2_gobgp.go`, `exporter.go`: RD/RT/VNI/MAC/optional IP/next hop, AddPath receipt UUID and exact UUID withdrawal. Exclusive local writer required; not generic AddClient semantics. |
| Read-only import and reconciliation | `route_importer.go`, `remote_netlink_import.go`: complete authoritative RIB snapshots, network/RT/VNI/VTEP validation; failed/partial reads do not authorize destructive reconciliation. |
| Netlink-Programmed integration | `local_netlink.go`, `owned_netlink.go`: local advertisements require native kernel evidence plus OVN provenance; remote FDB/neighbor state is receipt-owned. `ovn-controller` observes this integration state and owns forwarding. |
| OVSDB-Programmed integration | `ovsdb_programmer.go`, `ovsdb_provider_bridge.go`, `cmd/evpn-ovsdb`: opt-in standalone reader/programmer, dedicated provider bridge, localnet and static VXLAN. Accepted one network and one remote VTEP only. |
| Endpoint/attachment services | `pkg/endpoint`, `pkg/attachment`, reusable `pkg/nodeagent` managers: identity-owned NB LSP and local OVS attachment, usable without CNI. Independent Framework tests use direct managers. |
| CNI application | Separate `evpn-cni`/NodeAgentCNI ADD/CHECK/DEL; static addressing, replacement, idempotency, negative CHECK, pre-LSP failure rollback, survivor and cleanup acceptance. |
| Standalone fixtures | `tools/e2e/netlink/`: isolated Type-2 route/withdrawal fixtures; these bypass the k8gobgp manager and do not prove managed Kubernetes acceptance. |
| Operator-managed recovery | [Phase D record](../architecture/operator-type2-acceptance.md): normal and forced-SIGKILL 18/18 runs, including CNI authority, agent/speaker recovery, post-replacement packets, UUID withdrawal and cleanup. Bounded Netlink path, not generalized recovery or OVSDB-mode acceptance. |
| G.6/G.7 standalone runtime | [Milestones](backlog.md#completed-g6g7-milestones): speaker attestation/exact receipts, CNI-free importer, protected endpoint authorization, authoritative OVN intersection, durable fences, explicit exporter and version-2 lifecycle tombstones. |
| G.7b.5a live smoke | [Verified evidence](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a): one reused OVN chassis, real advertisement/revocation/exact withdrawal/restart, foreign preservation, no CNI socket and 18/18 operator regression. Not standalone forwarding/migration acceptance. |

C4.4f.2q.2 ran each independent Framework integration and the single-chassis
CNI lifecycle twice. This is historical private-source acceptance, not a fresh
public-candidate validation. See the [Testing Guide](../../TESTING_GUIDE.md).

## Implemented but not generally validated

Management gRPC services contain compatibility operations, not a universal
Framework orchestrator. `ProvisionVni` stores only a process-local VNI/LS
registry; `CreateVNI(LS,VNI)` remains a domain contract, **not an implemented
method found in this checkout**. Preserve both contracts while addressing
[FW-01](backlog.md#p1-reliability-and-contract-clarification).
The [API reference](../api/README.md) distinguishes concrete handlers from
generated Unimplemented methods.

Ownership receipts, conservative observation and recovery code exist, but
do not establish every controller/speaker/device-replacement scenario
([FW-04](backlog.md#p1-reliability-and-contract-clarification)).
Post-LSP compensation and partial provider-FDB invalidation remain open
([FW-03/FW-05](backlog.md#p1-reliability-and-contract-clarification)).

## Experimental deployment versus planned capabilities

The optional `EvpnCore` Operator has YAML-driven singleton lifecycle,
Node-Agent deployment, workload readiness and conservative cleanup; it is not a
production installer. Supported standalone/Kubernetes installation is
[DEP-01](backlog.md#p2-planned-capabilities-and-integrations), separate from
experimental first-release reproduction. Operator responsibilities and planned CRDs are in
[Kubernetes integration](kubernetes-integration.md). k8gobgp configuration
ownership is already [audited](../architecture/runtime-dependencies.md#bgpconfiguration-versus-direct-grpc);
generalized recovery remains open. FRR/k8gobgp upgrades and Containerlab
unification are completed historical q1/q2 work, not pending roadmap tasks.

Shared-VTEP, two-chassis, multi-VTEP, RR, Type-5/IP-VRF, Single VXLAN
Device/VLAN segmentation and dynamic IPAM are **planned, not accepted**
(FW-06 through FW-12). DCI and Neutron are future
[integrations](integrations/README.md). Standalone authorized OVN export and
import runtimes exist; supported lifecycle integration/installer and
[G.7b.5b](backlog.md#g7b5b) multi-chassis/tombstone migration remain open.
EvpnCore still rejects a standalone provider deployment selector; this is
distinct from Node-Agent's implemented attestation/runtime. See the
[implemented-versus-target boundary matrix](../architecture/framework-first.md#implemented-boundaries-versus-design-targets).
Multihoming and quantitative
convergence/performance are research-only (RS-01/RS-02).

Publication hygiene and licensing are [P0 release gates](release.md);
these post-publication feature tasks do not expand the first-release scope.
