# First experimental release gates

The first release is a reusable Go EVPN Framework, **not production-qualified**.
Preparation, local clean-history migration and public push are distinct decisions.
PASS requires executed evidence; FAIL is an unmet requirement; NOT_RUN is not
a pass; NOT_APPLICABLE identifies deliberately excluded scope.

## Frozen scope

The shared Framework uses GoBGP and OVN/OVS with alternative Netlink-Programmed
and OVSDB-Programmed integrations. Kubernetes uses k8gobgp-managed gobgpd.
Optional CNI/Multus consumes reusable Node Agent endpoint/network services.
EvpnCore is manifest-apply scaffolding, not k8gobgp's controller or a complete
installer. CMS is an optional test/demo emulator, not Framework core.
Containerlab provisions test environments, not production networking.

Historical private q2 acceptance reports two PASS runs each for one-chassis
Netlink Type-2, one-network/one-remote-VTEP OVSDB Type-2, and single-chassis CNI
lifecycle. The broader CNI aggregate remains PARTIAL/shared-VTEP NOT_RUN.
See the [Testing Guide](../../TESTING_GUIDE.md). These are not new-candidate runs.
**Framework gRPC TLS was not enabled in the C4.4f.2q.2 acceptance runs.**

## A. Mandatory before migration

| Gate | Result | Evidence / limit |
|---|---|---|
| Approved Apache-2.0 and required attribution | PASS | Official root LICENSE byte-match; required NOTICE retained; all 90 Linux/amd64 build/test modules licensed; 40 OVN model files reproduce from Apache-2.0 upstream schema. [Source-only review limits](../release/licensing.md). |
| Exact approved source manifest and exclusions | PASS | Private versioned contract enumerates 264 INCLUDE / 249 EXCLUDE / zero REVIEW_REQUIRED, with validated content/mode receipts and traversal/symlink refusal. The initial local export matched exactly, but migration stopped before main because candidate tests failed. |
| Candidate-wide source security/privacy screening | PASS | All 264 proposed INCLUDE paths screened, including generated code/models and notices; no credential/private-key/personal-path exposure found. Actual exported tree and objects must be checked again before push. |
| Deployment exposure review | PASS | HIGH unauthenticated NodePorts accepted explicitly by maintainer under [dedicated-lab/firewall containment](../../SECURITY.md); MEDIUM broad Secret RBAC disclosed. No runtime hardening or production claim. |
| Public docs, API and six diagram consistency | PASS | Approved-only relative paths/anchors checked; 35 RPCs/28 concrete handlers/seven Unimplemented and exact generated descriptors verified; all six SVGs reproduce without diagram changes. |
| Safe source validation | PASS | Preparation: `go test ./...`, seven-target build, Python helpers (215 total, three skips), fixture tests (84), 16 shell syntax checks, 12 YAML parses. No live acceptance run. |
| Deterministic independent-history migration procedure | PASS | Private migration contract specifies exact-file export, parentless commit, destination conflict refusal, object/ref audit and no push. No main created. |

The stopped export exposed a transport-description-sensitive CNI deadline test
and two offline Python tests dependent on excluded historical evidence.
The correction checks the exact gRPC deadline status and uses synthetic packet,
receipt and ownership fixtures without changing runtime behavior or acceptance
assertions. See [offline fixture provenance](../../TESTING_GUIDE.md#offline-fixture-provenance).
Subsequent migration must use the new approved correction checkpoint and its
reviewed manifest, not the superseded preparation snapshot. Creating public
main and all pre-push gates remain separate work.

Final preparation status is recorded by the private milestone checkpoint; this
public page deliberately does not link private audit material. No unrun gate
may be promoted to PASS merely because another gate or historical suite passed.

## B. Mandatory before public push

| Gate | Result | Acceptance |
|---|---|---|
| Independent main and approved candidate contents | NOT_RUN | Separately authorized migration; one root commit, zero parents, exact allowlisted tree; preserve historical master. No private commits, tags, refs, alternates or compromised objects. |
| Candidate files and reachable object/ref security audit | NOT_RUN | Scan actual exported contents and main's object closure, not just this private working tree. Verify required license/notices and no private artifacts. |
| Clean-candidate builds, static checks and public links | NOT_RUN | Run documented safe checks from candidate; check all links after exclusions and regenerate/compare required artifacts. |
| Clean live reproduction or approved alternative | NOT_RUN | Two clean runs each of existing Netlink/OVSDB/CNI contracts on disposable infrastructure, or explicit maintainer-approved alternative acceptance. Historical private results never count as fresh evidence. |
| Maintainer final review and explicit publication approval | NOT_RUN | Review exact main SHA, contents, residual risks and validation. Maintainer alone performs any explicit main-only push. |

## C. Post-release work, not new first-release blockers

Type-5/IP-VRF, shared-VTEP, general two-chassis, multi-VTEP, RR, multihoming,
DCI, multi-cluster, dynamic IPAM, future CRDs, production deployment,
OVN-Kubernetes GoBGP integration, Neutron, broad recovery/performance and API
modernization remain [roadmap tasks](backlog.md). Their acceptance is NOT_RUN
where not established; production installation/future CRDs are NOT_APPLICABLE
to the bounded experimental first-release promise.

Credential disclosure, unsafe redistribution and private-history contamination
are always blockers. Functional roadmap incompleteness alone is not.
