# Canonical task register

Task definitions live only here. Priorities and statuses follow the
[roadmap conventions](README.md). `None` means no known prerequisite, not
permission to implement. Acceptance criteria are gates, not implementation claims.
Reviewed 2026-11-08; preserved G.7b implementation and co-hosted startup
regressions are committed locally at `750c478`. There are 39 records, including
eight bounded G.6/G.7 milestones.
The [overview](overview.md) is a navigation dashboard derived from this register.
Priority headings supply priority; the action index below supplies component,
next action and upstream ownership for the original records. Together these
tables define each record without duplicating its acceptance criteria.
Verified means the stated bounded checks passed, not production readiness.

## P0: first public release

| ID | Status | Scope and current evidence | Dependencies | Acceptance criteria | Relevant sources |
|---|---|---|---|---|---|
| REL-01 | Done | Approved Apache-2.0 installed; NOTICE preserves attribution. All 90 Linux/amd64 build/test dependency modules have version-specific license evidence; 40 OVN generated models reproduce from licensed upstream schema. | None | Source-only license/attribution gates met; binary/image or other-platform redistribution requires its own dependency review. | [Licensing](../release/licensing.md), `go.mod`, `go.sum`, `gen/`, `services/ovsdb_mgmt/ovnnb/` |
| REL-02 | In Progress | The modern `main` exists with independent parentless root `38f0821d053b814be2016113d354b80262fafe40` (2026-11-02). Historical Python `master` was inspected read-only on GitHub; it is not a local ref. Root existence completes only the history subcriterion, not the original migration hygiene scope. | REL-01 | Reconcile exact approved migrated source/exclusions and preserve historical branch; no inherited private history/objects. Current release tree/object security screening remains REL-03. Do not complete this broad task from root existence alone. | [History](../archive/README.md), [release gates](release.md) |
| REL-03 | Investigating | Earlier source review and lab-only risk acceptance exist; G.6/G.7 additions and locally committed G.7b files require review of the next exact release snapshot. No fresh candidate object/ref audit is claimed. | REL-02 | Scan actual release files and reachable objects/refs; exclude credentials, private records and history; retain accepted lab risk disclosure. | [Security boundaries](../../SECURITY.md), [API transport](../api/README.md#transport-and-errors), `services/`, `deploy/` |
| REL-04 | Blocked | G.7b.5a standalone smoke and operator regression passed locally on 2026-11-08; these are not two clean public-candidate runs or distribution provenance. | REL-01, REL-02, REL-03 | Publicly obtainable licensed runtime inputs and clean candidate builds/helper/static/docs checks; two disposable runs each of unchanged Netlink/OVSDB/CNI acceptance, or explicit maintainer-approved alternative acceptance before publication. | [Testing](../../TESTING_GUIDE.md), `tools/run-*-e2e.sh`, `tools/e2e/containerlab.py` |
| REL-05 | Blocked | Next-snapshot publication approval is not established by local implementation or E2E results. | REL-04, maintainer approval | Maintainer reviews exact snapshot, scope, residual risks and evidence and explicitly authorizes publication; maintainer alone pushes. No automatic publication or history rewrite. | [Release gates](release.md) |

## P1: reliability and contract clarification

| ID | Status | Scope and current evidence | Dependencies | Acceptance criteria | Relevant sources |
|---|---|---|---|---|---|
| FW-01 | Investigating | `ProvisionVni` is a public RPC but only stores an in-memory VNI/LS map. No `CreateVNI` implementation was found. | None | Preserve `ProvisionVni` and domain `CreateVNI(LS,VNI)` contract; define ownership before implementing per-VNI Linux/OVN and idempotent per-chassis OVS configuration; test restart and deletion without foreign-state loss. | `proto/evpn.proto`, `services/evpn_mgmt/main.go`, [EVPN API](../api/evpn-mgmt.md) |
| FW-02 | Investigating | k8gobgp owns speaker configuration in acceptance; compatibility `StartBgp`/`AddPeer` methods can compete with it. | None | Enforce/document one writer per field; verify manager reconciliation and controller/daemon restarts; preserve accepted CRD configuration; decide local versus upstream changes from evidence. | `services/evpn_mgmt/main.go`, [Ownership matrix](../architecture/runtime-dependencies.md#bgpconfiguration-versus-direct-grpc) |
| FW-03 | Proposed | Post-LSP ADD compensation is absent; accepted failure injection occurs before LSP creation. | None | Inject attachment failure after LSP creation; remove only newly created owned state; preserve pre-existing endpoint, survivor and retry behavior. | `pkg/endpoint/manager.go`, `pkg/nodeagent/server.go` |
| FW-04 | Investigating | G.6/G.7 and operator acceptance cover bounded attestation, exact receipts, revision fencing and agent restart. Generalized restart/adoption, power loss, migration and both-adapter guarantees remain open. | FW-02 | Separate controller, daemon, node-agent and device-lifetime failures; test stale/partial observations, persisted receipts, adoption/refusal, reconnection and no foreign deletion for both adapters. | `pkg/evpn/exporter.go`, `export_authority.go`, `owned_netlink.go`, `remote_netlink_import.go`, [milestones](#completed-g6g7-milestones) |
| FW-05 | Investigating | OVS-001: OVSDB importer does not explicitly invalidate a withdrawn MAC's learned provider-bridge FDB entry while another route retains the tunnel. | None | Define supported per-MAC invalidation/aging contract; prove partial-withdrawal timing, survivor traffic, final cleanup and controller ownership independently of API row deletion. | `pkg/evpn/ovsdb_programmer.go`, `ovsdb_provider_bridge.go`, [OVS-001](../known-issues/ovn-ovs-issues.md) |
| K8S-01 | In Progress | EvpnCore has effective image/namespace/Node-Agent configuration, singleton Lease, workload conditions and cross-namespace finalizer cleanup; disable is not uninstall. Operator Type-2 regression passed 18/18 on 2026-11-08; production gates remain unmet. | FW-02 | Validate production storage/recovery, privilege boundaries, authenticated dependencies, multi-node scheduling and disruption/upgrade behavior; workload readiness is not BGP/dataplane readiness. | [Operator lifecycle](../architecture/evpn-core-operator.md), `deploy/operator/`, `tools/run-operator-e2e.sh` |
| API-01 | Proposed | Seven declared RPCs inherit Unimplemented; other management methods mix status payloads, nil/partial lists and best-effort errors. | FW-01, FW-02 | Versioned compatibility/error/idempotency policy; conformance tests for all declared methods, truthful reads and no hidden writes; reproducible protobuf generation. Do not remove working APIs implicitly. | `proto/`, `gen/`, [API reference](../api/README.md) |
| API-02 | Proposed | Framework management servers have no configured TLS/authentication; node-local CNI uses filesystem access control. | None | Threat model and deployable authenticated/authorized transports or enforceable local isolation; tests and migration plan; do not confuse lab management PKI with gRPC TLS. | `services/evpn_mgmt/main.go`, `services/ovsdb_mgmt/main.go`, `pkg/nodeagent/server.go` |
| UP-01 | Investigating | OVN-001 crash reproduced; OVN-002 transient reference error suspected; neither has a root-cause-confirmed upstream defect or verified fix. | None | Separate minimal reproducers, symbols/core and exact schema/version; search upstream before sanitized draft; verify any proposed fix independently. | [Issue registry](../known-issues/ovn-ovs-issues.md), [OVN plan](upstream/ovn-ovs.md) |
| SEC-01 | Proposed | Remote BGP security is separate from G.6 local speaker attestation and G.7 endpoint authorization. Pinned k8gobgp v0.2.8 already resolves Secret references for TCP MD5; no authenticated GoBGP/FRR acceptance is claimed. | FW-02 | Matching/mismatched/missing MD5 key tests; TCP-AO capability and kernel/FRR interoperability matrix; GTSM and ACL denial tests; EVPN import/export policy; protected secrets and coordinated rotation/reconnect without competing peer writers or secret leakage. | [Peer security](upstream/bgp-peer-security.md), [rotation proposal](../issues/k8gobgp-peer-auth-rotation-proposal.md) |
| UP-02 | Investigating | Pinned GoBGP API has UUID-readback and MAC-only encoding constraints; binary NLRI plus durable AddPath receipts preserve safety. No root-cause-confirmed upstream defect or applicable issue/fix is established. | None | Minimal stock-versus-fork tests for UUID visibility and absent-IP typed/binary roundtrip; retain UUID-only withdrawal; complete duplicate checks before submitting clarification or fix. | [Control-plane registry](../known-issues/control-plane-issues.md), [compatibility draft](../issues/gobgp-type2-api-proposal.md) |
| G.7b.5b | Proposed | Standalone multi-chassis forwarding, replacement, migration and broader restart/failure coverage remain unexecuted. Single-chassis G.7b.5a is not this acceptance. | G.7b.5a, FW-07, FW-04 | Two independently attested standalone nodes; real bidirectional OVN/OVS/VXLAN traffic; version-2 deletion/replacement/migration tombstones, stale/partial NB/SB reads, rollback/corrupt policy, agent/speaker restart, exact receipts, foreign preservation and run-owned cleanup. Reuse fixtures; isolate host OVN. | [Authorization/runtime](../architecture/standalone-endpoint-authorization.md), [testing](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a) |

## P2: planned capabilities and integrations

| ID | Status | Scope and current evidence | Dependencies | Acceptance criteria | Relevant sources |
|---|---|---|---|---|---|
| FW-06 | Proposed | Shared-VTEP lifecycle: CNI aggregate explicitly NOT_RUN for this scope. Two endpoints in accepted tests do not establish this contract. | FW-04, FW-05 | Independent Netlink and OVSDB tests for resource sharing, withdrawal of one owner, survivor forwarding and last-owner cleanup. | `pkg/evpn/`, `tools/run-framework-netlink-e2e.sh`, `tools/run-framework-ovsdb-e2e.sh`, `tools/run-cni-full-lifecycle-e2e.sh` |
| FW-07 | Proposed | Two-chassis GoBGP-to-GoBGP has no accepted test. | FW-04 | Identity isolation, bidirectional Type-2 and OVN/OVS traffic/captures, withdrawal, survivor preservation and cleanup on both chassis. | `pkg/evpn/`, `tools/e2e/` |
| FW-08 | Proposed | Multi-VTEP: opt-in OVSDB path is explicitly constrained to one network/remote VTEP. | FW-06, FW-07 | Network/VNI/VTEP ownership, next-hop change, independent route sets and lifecycle acceptance for both adapters. | `cmd/evpn-ovsdb/main.go`, `pkg/evpn/route_importer.go`, `ovsdb_programmer.go` |
| FW-09 | Proposed | Route Reflector scenarios are outside acceptance, regardless of provider schema fields. | FW-07 | RR/client policy, loop prevention, path selection and restart/failover tests with real traffic and route provenance. | `pkg/evpn/type2_gobgp.go`, [k8gobgp plan](upstream/k8gobgp.md) |
| FW-10 | Proposed | Type-5/IP-VRF, routed tenants and L3-VNI are not implemented Framework acceptance scopes. | FW-02 | Prefix/VRF ownership and APIs, RD/RT policy, next-hop/IRB contract and independent import/export/forwarding/withdrawal tests without Type-2 regression. | `pkg/evpn/type2.go`, `route_importer.go`, [DCI design](integrations/dci-multicluster.md) |
| FW-11 | Proposed | Single VXLAN Device and VLAN-to-VNI segmentation are not established by per-network device tests. | FW-06 | Device/VLAN/VNI allocation, limits, conflict and last-owner lifecycle; verify OVN-version compatibility and both local and remote forwarding. | `pkg/evpn/local_netlink.go`, `owned_netlink.go`, [OVN-Kubernetes comparison](upstream/ovn-kubernetes.md) |
| FW-12 | Proposed | Dynamic IPAM is absent; accepted CNI examples use static addresses. | FW-03 | Allocation/release, collision and stale-incarnation contracts; failure/retry/restart tests; keep independently reusable endpoint APIs. | `proto/node_agent_cni.proto`, `cmd/evpn-cni/`, `pkg/nodeagent/` |
| K8S-02 | Proposed | EvpnGateway/EvpnVTEP/EvpnNetwork are design proposals only, not project-owned CRDs. | FW-01, FW-02, K8S-01 | Review identity/topology/ownership schemas and translation to upstream config before implementation; test status, conflicts, upgrades and deletion; no duplicate BGP writers. | [Proposed CRDs](kubernetes-integration.md#proposed-crds-not-implemented), `deploy/operator/` |
| INT-01 | Proposed | No OpenStack Neutron adapter exists. | FW-01, API-01, API-02 | Explicit ML2/OVN network/port ownership and authentication; lifecycle/recovery/traffic tests without Multus/CNI/Kubernetes runtime dependency. | [Neutron design](integrations/openstack-neutron.md), `pkg/endpoint/`, `pkg/evpn/` |
| INT-02 | Proposed | DCI/multi-cluster has no accepted implementation. | FW-08, FW-09, FW-10 | Cross-site identity and RT policy; routed-tenant, isolation, overlap/MTU and failure-convergence interoperability tests; scoped cleanup per domain. | [DCI design](integrations/dci-multicluster.md), `pkg/evpn/` |
| INT-03 | Investigating | OVN-Kubernetes uses FRR/frr-k8s-specific APIs; GoBGP is not a drop-in replacement. | FW-02, FW-10, FW-11 | Compare CUDN/VTEP/RouteAdvertisements and FRRConfiguration contracts; prove an external adapter first or propose a reviewed upstream seam with no competing kernel writers. | [OVN-Kubernetes plan](upstream/ovn-kubernetes.md), `pkg/evpn/` |
| DEP-01 | Proposed | Production installation is not provided; maintained runners generate disposable lab manifests. | FW-02, FW-04, API-02 | Supported standalone and optional Kubernetes installation with reproducible images, per-node identities, privilege/mount/RBAC boundaries, configuration/addressing, observability and upgrade/recovery acceptance independent of E2E-generated manifests. | `deploy/kind/`, `deploy/operator/`, `cmd/evpn-node-agent/`, `cmd/evpn-ovsdb/`, [Deployment limits](../architecture/deployment.md) |

## P3: research

| ID | Status | Scope and current evidence | Dependencies | Acceptance criteria | Relevant sources |
|---|---|---|---|---|---|
| RS-01 | Proposed | EVPN multihoming is research-only; no ESI/DF election/failover acceptance. | FW-07, FW-08 | Choose single/all-active design; define ESI, DF, split-horizon, mass withdrawal and failure tests before implementation claims. | `pkg/evpn/type2.go`, `type2_gobgp.go` |
| RS-02 | Proposed | Failure-convergence/performance numbers are not established by functional pings. | FW-04, FW-07 | Reproducible route/workload scale, resource limits, clocks and packet-loss/convergence metrics; compare integrations without changing traffic or failure models. | `tools/e2e/`, `pkg/evpn/`, `TESTING_GUIDE.md` |

## Completed G.6/G.7 milestones

These P1 milestones are complete only within their stated scope. G.7b.2 through
G.7b.5a are committed locally at `750c478`; Verified is not a release gate or
supported installer. Full Go and privileged startup/authorization/ownership
tests passed after the co-hosted startup correction; unchanged implementation
bytes were verified before committing. Live co-hosting remains NOT TESTED.
The accessible G.7b.5a live bundle and 18/18 operator bundle were independently
checked; exact paths and assertion scope are recorded in the
[Testing Guide](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a).

| ID | Component | Status | Scope / acceptance achieved | Dependencies | Evidence / reference | Next action | Upstream |
|---|---|---|---|---|---|---|---|
| <a id="g6"></a>G.6 | Speaker ownership | Verified | Standalone service identity, process/executable/config pins, attested Unix API, durable exact UUID ownership and fail-closed ambiguous AddPath recovery implemented; no remote-peer authentication implied. | None | `ee818b0`, `pkg/nodeagent/provider_attestation_linux.go`, `pkg/nodeagent/standalone_attestation_linux_test.go`, `pkg/evpn/export_recovery.go`, [ownership](../architecture/type2-restart-ownership.md) | Retain guarantees in FW-04 / SEC-01. | GoBGP (reused) |
| <a id="g7a"></a>G.7a | Standalone importer | Verified | Existing Type-2 importer runs without CNI/Kubernetes/CRI startup, with independently configured identity, attestation and owned Netlink recovery; bounded tests, not standalone multi-chassis acceptance. | G.6 | `7b88e01`, `cmd/evpn-node-agent/main.go`, `pkg/nodeagent/remote_runtime.go`, [implementation](../../IMPLEMENTATION_README.md) | Extend live coverage under G.7b.5b. | None |
| <a id="g7b1"></a>G.7b.1 | Endpoint authorization | Verified | Protected, scoped trusted endpoint contract implemented independently of OVN metadata; exact endpoint/attachment/LSP/MAC/IP identity validation. | G.6 | `52c22fa`, `pkg/endpointauth/authorization.go`, `authorization_test.go`, [contract](../architecture/standalone-endpoint-authorization.md) | Keep current-policy validation mandatory. | None |
| <a id="g7b2"></a>G.7b.2 | OVN observation | Verified | Accepted authorization intersects actual NB/SB UUIDs, switch membership, canonical binding and ready local chassis; missing/conflicting evidence remains uncertain. | G.7b.1 | `pkg/nodeagent/ovnexport/authorization.go`, `authorization_test.go`, `pkg/nodeagent/export_observer.go` | Add multi-chassis uncertainty scenarios. | OVN (reused) |
| <a id="g7b3"></a>G.7b.3 | Durable authorization | Verified | Revision/digest and generation high-water persistence precedes accepted snapshot; interrupted intent recovery and route-journal isolation tests exist. No power-loss certification. | G.7b.1 | `pkg/endpointauth/fence.go`, `fence_test.go`, `pkg/nodeagent/endpoint_authorization_fence_linux_test.go` | Extend live interrupted-write/replay coverage. | None |
| <a id="g7b4"></a>G.7b.4 | Standalone exporter | Verified | Explicit protected runtime config reuses exporter and speaker attestation without CNI; optional importer co-hosting requires shared identity and distinct journals. | G.6, G.7b.2, G.7b.3 | `pkg/nodeagent/standalone_export_runtime_linux.go`, `standalone_export_runtime_linux_test.go`, `cmd/evpn-node-agent/main.go` | Preserve startup order and co-hosting opt-in. | None |
| <a id="g7b5"></a>G.7b.5 | Lifecycle withdrawal | Verified | Version-2 exact lifecycle tombstones and generation fencing implemented/tested; deletion needs trusted signal plus complete NB/SB absence, migration ready remote binding. Live revocation passed; live deletion/replacement/migration remain G.7b.5b. | G.7b.2, G.7b.3, G.7b.4 | `pkg/nodeagent/ovnexport/authorization_test.go`, `pkg/endpointauth/fence_test.go`, [tombstones](../architecture/standalone-endpoint-authorization.md#g7b5-deletion-and-replacement-evidence) | Run tombstone lifecycle E2E, not just revocation. | None |
| <a id="g7b5a"></a>G.7b.5a | E2E smoke / cache | Verified | Reused one OVN chassis: speaker/API/FRR, attestation, durable revision 1 advertisement, revision 2 exact UUID withdrawal, restart without resurrection, foreign preservation, no CNI socket, restoration and cleanup PASS; operator regression 18/18 PASS. | G.7b.4, G.7b.5 | `tools/lib/e2e-artifact-cache.sh`, `tools/lib/standalone-export-smoke-test.sh`, [verified evidence](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a) | Proceed only to separately authorized G.7b.5b. | None |

## Task action and ownership index

Each anchor resolves to the same canonical task defined above; no acceptance
criteria are maintained in the dashboard or topic pages.

| ID | Component | Priority | Next action | Upstream / issue |
|---|---|---|---|---|
| <a id="rel-01"></a>REL-01 | Release licensing | P0 | Preserve source notices; review any new distribution. | None |
| <a id="rel-02"></a>REL-02 | Project history | P0 | Reconcile exact migrated scope; preserve main/master histories. | None |
| <a id="rel-03"></a>REL-03 | Release screening | P0 | Review exact next snapshot and object/ref closure. | None |
| <a id="rel-04"></a>REL-04 | Reproducibility | P0 | Obtain reproducible inputs; execute clean snapshot gates. | None |
| <a id="rel-05"></a>REL-05 | Publication | P0 | Await maintainer review and explicit approval. | None |
| <a id="fw-01"></a>FW-01 | Network lifecycle | P1 | Specify per-VNI ownership and restart/deletion contract. | None |
| <a id="fw-02"></a>FW-02 | BGP configuration | P1 | Test sole-writer reconciliation and compatibility RPC boundaries. | [k8gobgp](upstream/k8gobgp.md); no issue verified |
| <a id="fw-03"></a>FW-03 | Endpoint rollback | P1 | Inject attachment failure after LSP creation. | None |
| <a id="fw-04"></a>FW-04 | Recovery | P1 | Extend failure matrix without broad adoption claims. | None |
| <a id="fw-05"></a>FW-05 | Provider FDB | P1 | Measure partial withdrawal and supported per-MAC invalidation. | OVS research; [OVS-001](../known-issues/ovn-ovs-issues.md#ovs-001-learned-fdb-state-after-partial-route-withdrawal) |
| <a id="k8s-01"></a>K8S-01 | Operator hardening | P1 | Validate storage, privileges, upgrades and multi-node placement. | None |
| <a id="api-01"></a>API-01 | Management API | P1 | Define versioned error/idempotency conformance. | None |
| <a id="api-02"></a>API-02 | Management security | P1 | Define authenticated transport and migration/containment tests. | None |
| <a id="up-01"></a>UP-01 | OVN triage | P1 | Minimize crash and identify SB reference owner; finish duplicate checks. | [OVN draft](../issues/ovn-port-binding-upstream-draft.md) |
| <a id="sec-01"></a>SEC-01 | Remote BGP security | P1 | Test existing MD5 Secret reference and rotation; audit AO/GTSM/ACL/policy. | [k8gobgp rotation proposal](../issues/k8gobgp-peer-auth-rotation-proposal.md) |
| <a id="up-02"></a>UP-02 | GoBGP API compatibility | P1 | Reproduce on stock/fork pins and complete duplicate review. | [GoBGP proposal](../issues/gobgp-type2-api-proposal.md) |
| <a id="g7b5b"></a>G.7b.5b | Standalone integration | P1 | Reuse fixtures for two-chassis forwarding and tombstone migration. | None |
| <a id="fw-06"></a>FW-06 | Shared-VTEP lifecycle | P2 | Validate partial withdrawal and last-owner cleanup per adapter. | None |
| <a id="fw-07"></a>FW-07 | Two-chassis EVPN | P2 | Define two-GoBGP/OVN traffic and identity acceptance. | None |
| <a id="fw-08"></a>FW-08 | Multi-VTEP | P2 | Extend scoped network/VTEP lifecycle after sharing tests. | None |
| <a id="fw-09"></a>FW-09 | Route reflection | P2 | Define RR policy, provenance and failover scenarios. | None |
| <a id="fw-10"></a>FW-10 | Type-5 / IP-VRF | P2 | Define prefix/VRF ownership and routed acceptance. | None |
| <a id="fw-11"></a>FW-11 | SVD / VLAN-VNI | P2 | Compare upstream device sharing and allocation ownership. | OVN-Kubernetes research |
| <a id="fw-12"></a>FW-12 | Dynamic IPAM | P2 | Define allocation/release and stale-incarnation safety. | None |
| <a id="k8s-02"></a>K8S-02 | Proposed network CRDs | P2 | Review intent translation before adding schemas. | None |
| <a id="int-01"></a>INT-01 | Neutron / ML2-OVN | P2 | Define authenticated network/port lifecycle adapter. | OpenStack research; no issue verified |
| <a id="int-02"></a>INT-02 | DCI / multi-cluster | P2 | Define cross-site identity and RT/MTU isolation. | None |
| <a id="int-03"></a>INT-03 | OVN-Kubernetes | P2 | Review external integration and provider seam with upstream. | [Architecture proposal](../issues/ovn-kubernetes-gobgp-provider-proposal.md) |
| <a id="dep-01"></a>DEP-01 | Production installation | P2 | Define supported distributions, identities and upgrade boundaries. | None |
| <a id="rs-01"></a>RS-01 | Multihoming | P3 | Choose ESI/DF/failover design before implementation. | None |
| <a id="rs-02"></a>RS-02 | Performance / convergence | P3 | Define reproducible scale and packet-loss measurements. | None |

No matching upstream issue/PR is assigned as a verified duplicate or fix.
Preliminary OVN/GoBGP issue searches were performed in G.8; duplicate review,
version comparison and submission readiness remain pending in the drafts.
Local diagnostic IDs are not GitHub issue numbers. Cilium has no concrete
implementation task; see its [scope decision](upstream/cilium.md).
