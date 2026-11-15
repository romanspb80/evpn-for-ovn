# Framework capabilities and evolution

The Framework's Go route and endpoint services can be exercised independently
of Kubernetes, CNI and Multus. Node-Agent has explicit standalone-attested
import and authorized-export modes; CNI authority/recovery and EvpnCore
orchestration remain Kubernetes/runtime integrations. GoBGP provides
EVPN control-plane services; OVN/OVS owns workload forwarding in **both**
integration modes. This page describes capabilities, not a second task list.
All priorities and acceptance criteria are in the [backlog](backlog.md).

The [capability matrix](../project/capability-matrix.md) is authoritative for
validation status; the table below describes implementation boundaries.

## Implementation boundaries

| Capability | Evidence and limit |
|---|---|
| Platform-neutral Type-2 domain | `pkg/evpn/domain`: provider-neutral endpoint and route-prefix identities plus deterministic pure reconciliation planning; static golden cases are also exercised through the `pkg/evpn` compatibility and GoBGP wire path. The package is standard-library-only; it does not make the stateful runtime platform-neutral. |
| Type-2 construction/validation and export | `pkg/evpn/type2.go`, `type2_gobgp.go`, `type2_lifecycle.go`: provider-neutral route source/applier contracts; GoBGP owns wire conversion, readiness and exact AddPath UUID receipts/withdrawals. AddPath has no compare-and-swap; exclusive local writer and fail-closed ambiguous recovery remain required. |
| Read-only import and reconciliation | `remote_type2.go`, `route_importer.go`, `type2_gobgp_remote.go`, `type2_gobgp_snapshot.go`, `remote_netlink_import.go`: decoded complete-snapshot contracts isolate importer policy from provider wire values; GoBGP adapters own protobuf/packet decoding. Legacy GoBGP-shaped source types remain compatibility bridges. |
| Netlink-Programmed integration — selected target | `local_netlink.go`, `owned_netlink.go`, `remote_netlink_import.go`: the verified path requires authoritative route evidence and exact durable FDB/neighbor receipts; recovery quarantine and foreign-state preservation remain backend-owned. `ovn-controller` observes this integration state and owns forwarding. The older `DataplaneBackend` contract remains scoped to the simple per-route importer and carries Linux device mapping strings. |
| OVSDB-Programmed integration — experimental | `ovsdb_programmer.go`, `ovsdb_provider_bridge.go`, `cmd/evpn-ovsdb`: an opt-in standalone Type-2 route consumer, dedicated provider bridge, localnet and static VXLAN. Existing transaction/ownership behavior accepts one network and one remote VTEP only; it is not unified with Netlink recovery. Exported `Model1*` Go names are legacy API identifiers retained for source compatibility, not architecture terminology. The persisted owner value `model1-nbdb` and related OVS ownership metadata remain unchanged. No `Model2` identifiers were found in the repository audit. |
| Endpoint/attachment services | `pkg/endpoint`, `pkg/attachment`, reusable `pkg/nodeagent` managers: identity-owned NB LSP and local OVS attachment, usable without CNI. Independent Framework tests use direct managers. |
| Kubernetes CNI solution | Separate `evpn-cni`/NodeAgentCNI ADD/CHECK/DEL; static addressing, replacement, idempotency, negative CHECK, pre-LSP failure rollback, survivor and cleanup acceptance. |
| Standalone fixtures | `tools/e2e/netlink/`: isolated Type-2 route/withdrawal fixtures; these bypass the k8gobgp manager and do not prove managed Kubernetes acceptance. |
| Operator-managed recovery | [Historical record](../architecture/operator-type2-acceptance.md): bounded Netlink recovery, not generalized recovery or OVSDB-mode acceptance. |
| Standalone runtime | Speaker attestation/exact receipts, CNI-free importer, protected endpoint authorization, authoritative OVN intersection, durable fences, explicit exporter and version-2 lifecycle tombstones. See [runtime contract](../architecture/standalone-endpoint-authorization.md). |

## Implemented but not generally validated

Management gRPC services contain compatibility operations, not a universal
Framework orchestrator. `ProvisionVni` stores only a process-local VNI/LS
registry; `CreateVNI(LS,VNI)` remains a domain contract, **not an implemented
method found in this checkout**. Preserve both contracts while addressing
[FW-01](backlog.md#p1-reliability-and-contract-clarification).
The [API reference](../api/README.md) distinguishes concrete handlers from
generated Unimplemented methods.

The Type-2 domain extraction leaves GoBGP conversion and RIB operations,
stateful route acknowledgments, ownership journals, recovery, OVN/OVS, Netlink,
and Node-Agent lifecycle in their existing packages. Provider-neutral
`Type2RIBSource`, `Type2RouteApplier`, `RemoteType2RIBSource`,
`Type2RouteSnapshotSource`, and `ReadinessGate` contracts keep the active
reconciliation paths clear of GoBGP wire types. Compatibility entry points
`RemoteRIBSource` and `RouteSnapshotSource` still accept GoBGP values, and
`pkg/evpn` still depends on GoBGP packet parsers for RD/RT validation; the
adapter boundary is source-level within the package, not a separately
importable provider module. Golden tests pin endpoint and route identities,
serialized GoBGP NLRI/attributes, complete/uncertain snapshots, exact UUID
withdrawal and reconciliation ordering; they do not establish platform
integration or multi-node acceptance.

Ownership receipts, conservative observation and recovery code exist, but
do not establish every controller/speaker/device-replacement scenario
([FW-04](backlog.md#p1-reliability-and-contract-clarification)).
Post-LSP compensation and partial provider-FDB invalidation remain open
([FW-03/FW-05](backlog.md#p1-reliability-and-contract-clarification)).

The mode-specific dataplane boundaries exist without
introducing a common backend package: the verified Netlink state machine and
the OVSDB object-transaction path have different authority, receipts, and
recovery semantics. Tests pin deterministic per-route withdrawal-before-apply
behavior for the simple route backend and retain separate Netlink
recovery/foreign-state and OVSDB ownership/transaction coverage. `pkg/evpn`
remains coupled to both Linux and OVSDB libraries; extracting a shared API is
deferred until a typed, non-lossy authorization and receipt handoff can be
designed without changing those safety contracts.

The [historical dataplane evidence](../archive/validation-record.md#framework-dataplane-boundary-evidence)
records the earlier runner changes, inputs, regressions and failed attempts.

## Experimental deployment versus planned capabilities

The optional `EvpnCore` Operator has YAML-driven singleton lifecycle,
Node-Agent deployment, workload readiness and conservative cleanup; it is not a
production installer. Supported standalone/Kubernetes installation is
[DEP-01](backlog.md#p2-planned-capabilities-and-integrations), separate from
experimental first-release reproduction. Operator responsibilities and planned CRDs are in
[Kubernetes integration](kubernetes-integration.md). k8gobgp configuration
ownership is already [audited](../architecture/runtime-dependencies.md#bgpconfiguration-versus-direct-grpc);
generalized recovery remains open. Historical dependency and laboratory
transitions are described in the [validation record](../archive/validation-record.md#dependency-and-provisioning-transition).

Shared-VTEP, broader two-chassis recovery, multi-VTEP, RR, Type-5/IP-VRF,
Single VXLAN Device/VLAN segmentation and dynamic IPAM remain
**open roadmap gates** (FW-06 through FW-12). The
[capability matrix](../project/capability-matrix.md) describes the bounded
two-chassis results and their limits. DCI and Neutron are future
[integrations](integrations/README.md). Standalone authorized OVN export and
import runtimes exist; supported lifecycle integration/installer and
[standalone recovery work](backlog.md#standalone-recovery) full-feature acceptance and broader recovery remain open.
EvpnCore still rejects a standalone provider deployment selector; this is
distinct from Node-Agent's implemented attestation/runtime. See the
[implemented-versus-target boundary matrix](../architecture/framework-architecture.md#implemented-boundaries-versus-design-targets).
Multihoming and quantitative
convergence/performance are research-only (RS-01/RS-02).

Publication hygiene and licensing are [P0 release gates](release.md);
these post-publication feature tasks do not expand the first-release scope.

## Backend comparison roadmap

The Netlink-Programmed integration is the selected development target.
OVSDB-Programmed remains experimental for independent Framework regression,
architectural research, and a future evidence-based comparison. No performance
winner is assumed and no comparative benchmark has been run.

A future comparison should hold topology, route population, workload, peer
behavior, and failure injection equivalent. Measure route and withdrawal
convergence, route scale, CPU and memory use, restart and recovery/reconciliation,
forwarding correctness, and operational complexity. Attribute results
separately to GoBGP engine behavior, the selected backend's integration
behavior, shared Framework logic, and test-harness limitations.

## EVPN Type-2 vendor interoperability

Plan a reusable, Containerlab-oriented interoperability suite for the
Netlink-Programmed Framework and external EVPN speakers. FRRouting (FRR) is the
reference and regression baseline; Nokia SR Linux, Juniper cJunosEvolved,
NVIDIA Cumulus VX, and Arista cEOS are planned vendor peers. This is a
test-planning milestone only: none of these vendor targets is claimed to have
passed validation.

### Target and readiness matrix

| Peer platform | Initial status | Planned role | Readiness gate |
|---|---|---|---|
| FRRouting (FRR) | `PLANNED` | Reference behavior and regression baseline; reuse existing FRR interoperability infrastructure. | Confirm the fixture, image provenance, and test inputs for the reusable suite. |
| Nokia SR Linux | `IMAGE_AVAILABILITY_PENDING` | Vendor interoperability target. | Verify an authorized image, licensing, Type-2 support, Containerlab or VM suitability, and automation. |
| Juniper cJunosEvolved | `IMAGE_AVAILABILITY_PENDING` | Vendor interoperability target. | Verify an authorized image, licensing, Type-2 support, Containerlab or VM suitability, and automation. |
| NVIDIA Cumulus VX | `IMAGE_AVAILABILITY_PENDING` | Vendor interoperability target. | Verify an authorized image, licensing, Type-2 support, Containerlab or VM suitability, and automation. |
| Arista cEOS | `IMAGE_AVAILABILITY_PENDING` | Vendor interoperability target. | Verify an authorized image, licensing, Type-2 support, Containerlab or VM suitability, and automation. |

These are initial planning statuses, not test results. Use the following status
values as work advances:

| Status | Meaning |
|---|---|
| `PLANNED` | Test or target is defined but not yet prepared or executed. |
| `IMAGE_AVAILABILITY_PENDING` | Authorized, technically suitable image availability has not been confirmed. |
| `LAB_READY` | Image, license, resources, platform support, and repeatable lab setup have been verified. |
| `CONTROL_PLANE_PASS` | The defined control-plane checks passed with retained evidence. |
| `DATAPLANE_PASS` | The defined VXLAN forwarding checks passed with retained evidence. |
| `BLOCKED` | A documented prerequisite prevents the planned test. |
| `NOT_SUPPORTED` | The platform or release does not provide a required capability; record the exact limitation. |

Do not assign either PASS status without executing the corresponding checks and
recording versioned configuration, logs, route observations, and forwarding
evidence. Control-plane and dataplane status are independent.

### Planned interoperability coverage

| Area | Planned checks | Applicability |
|---|---|---|
| BGP / EVPN session | BGP session establishment and EVPN AFI/SAFI negotiation. | Per platform and tested software release. |
| Type-2 routes | Advertisement and import of MAC-only and MAC/IP routes where supported; RD/RT handling; VNI, VTEP, and next-hop interpretation. | Record unsupported route forms or configuration differences rather than assuming parity. |
| Withdrawal and recovery | Exact route withdrawal, re-advertisement after restart, and control-plane convergence. | Exercise peer and Framework restarts separately; retain route-level observations. |
| VXLAN dataplane | External endpoint reachability across the advertised VNI/VTEP and supported MAC/IP forms. | Track independently from control-plane success. |
| Framework ownership | Preserve foreign routes and enforce ownership isolation during import, export, and withdrawal. | Framework-side safety checks; do not infer them from peer interoperability alone. |

### Lab architecture and prerequisites

Use a reusable Containerlab topology with a Framework/GoBGP node, one external
peer under test, and Linux endpoints or traffic generators for forwarding
checks. Keep the FRR reference case in the same topology and harness. Use
vendor container images only where legally and technically available; use a
VM-based peer when an image is unavailable, unsuitable, or incompatible with
the host/Containerlab environment.

Before implementing a vendor case or acquiring images, verify image source and
availability, license terms and redistribution limits, host CPU/memory/storage
requirements, architecture and virtualization compatibility, EVPN Type-2
feature support, Containerlab integration, repeatable configuration/observation
automation, and whether CI execution is feasible. No image availability,
license, or CI suitability is assumed by this roadmap.

### Recommended implementation order

1. Reuse and stabilize the existing FRR interoperability path as the
   configuration, route-observation, withdrawal, and forwarding baseline.
2. Preflight all four vendor targets against the image, license, resource,
   Type-2 feature, Containerlab/VM, and automation gates above; do not download
   images or begin deployment until authorized access is established.
3. Implement the first vendor case that passes those gates and offers the
   broadest automatable Type-2 coverage. Then add the remaining eligible peers
   using the same evidence format. If no vendor image is suitable, use an
   authorized VM path or record the target as `BLOCKED`.

The vendor order after FRR is intentionally selected by verified feasibility,
not by an unverified claim about image availability or feature parity.
