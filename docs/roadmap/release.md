# Experimental release gates

The release remains a reusable Go EVPN Framework, **not production-qualified**.
Modern parentless `main` already exists at root
`38f0821d053b814be2016113d354b80262fafe40` (2026-11-02); reviewed local HEAD is
`750c478`, including G.7b and its co-hosted startup correction. Historical Python `master` is deliberately
independent and was only read remotely; no local master ref exists.
See [project provenance](../archive/README.md).
Earlier preparation, the established independent root and approval of the next exact
release snapshot are distinct decisions. Local refs/commit titles do not prove
publication approval or current release acceptance.
PASS requires executed evidence; FAIL is an unmet requirement; NOT_RUN is not
a pass; NOT_APPLICABLE identifies deliberately excluded scope.

## Current bounded scope

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

G.6/G.7 add explicit standalone-attested import and authorized export without
CNI startup. G.7b.5a single-chassis advertisement/revocation/restart and the
operator regression have accessible PASS evidence from 2026-11-08. Implementation
and focused co-hosted startup regressions are committed locally at `750c478`;
live co-hosting remains NOT TESTED. Neither bundle establishes standalone
multi-chassis traffic/migration, general recovery or two clean release-snapshot
runs. See [implementation status](../../IMPLEMENTATION_README.md) and
[G.7b.5a evidence](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a).

## A. Historical preparation record

The following PASS labels describe the earlier preparation snapshot, not the
current G.7b release tree. Preserve them as history; re-review new code and
distribution inputs against the next exact snapshot under REL-03/REL-04.

| Gate | Result | Evidence / limit |
|---|---|---|
| Approved Apache-2.0 and required attribution | PASS | Official root LICENSE byte-match; required NOTICE retained; all 90 Linux/amd64 build/test modules licensed; 40 OVN model files reproduce from Apache-2.0 upstream schema. [Source-only review limits](../release/licensing.md). |
| Exact approved source manifest and exclusions | PASS | Earlier private contract enumerated 264 INCLUDE / 249 EXCLUDE / zero REVIEW_REQUIRED with receipts and traversal/symlink refusal. An initial export stopped on candidate-test failures; this is historical, not the current main/G.7b manifest. |
| Candidate-wide source security/privacy screening | PASS | All 264 proposed INCLUDE paths screened, including generated code/models and notices; no credential/private-key/personal-path exposure found. Actual exported tree and objects must be checked again before push. |
| Deployment exposure review | PASS | HIGH unauthenticated NodePorts accepted explicitly by maintainer under [dedicated-lab/firewall containment](../../SECURITY.md); MEDIUM broad Secret RBAC disclosed. No runtime hardening or production claim. |
| Public docs, API and six diagram consistency | PASS | Approved-only relative paths/anchors checked; 35 RPCs/28 concrete handlers/seven Unimplemented and exact generated descriptors verified; all six SVGs reproduce without diagram changes. |
| Safe source validation | PASS | Preparation: `go test ./...`, seven-target build, Python helpers (215 total, three skips), fixture tests (84), 16 shell syntax checks, 12 YAML parses. No live acceptance run. |
| Deterministic independent-history migration procedure | PASS | Earlier contract specified exact-file export, parentless commit, conflict refusal and object/ref audit. Modern parentless main now exists; current snapshot screening remains separate. |

The stopped export exposed a transport-description-sensitive CNI deadline test
and two offline Python tests dependent on excluded historical evidence.
The correction checks the exact gRPC deadline status and uses synthetic packet,
receipt and ownership fixtures without changing runtime behavior or acceptance
assertions. See [offline fixture provenance](../../TESTING_GUIDE.md#offline-fixture-provenance).
Those corrections preceded the modern independent root. Do not reuse that
superseded preparation manifest as approval of today's G.6/G.7 changes.

Final preparation status is recorded by the private milestone checkpoint; this
public page deliberately does not link private audit material. No unrun gate
may be promoted to PASS merely because another gate or historical suite passed.

## B. Next-snapshot release gates

| Gate | Result | Acceptance |
|---|---|---|
| Independent modern main root (REL-02) | PASS | Parentless root verified locally; preserve historical master. This narrow result does not certify exact candidate contents or publication. |
| Approved next-snapshot contents | NOT_RUN | Review exact SHA/tree including current G.7b work; preserve attribution/exclusions and refuse private artifacts or unsafe inherited refs/objects. |
| Candidate files and reachable object/ref security audit | NOT_RUN | Scan actual exported contents and main's object closure, not just this private working tree. Verify required license/notices and no private artifacts. |
| Clean-candidate builds, static checks and public links | NOT_RUN | Run documented safe checks from candidate; check all links after exclusions and regenerate/compare required artifacts. |
| Clean live reproduction or approved alternative | NOT_RUN | Two clean runs each of existing Netlink/OVSDB/CNI contracts on disposable infrastructure, or explicit maintainer-approved alternative acceptance. Historical private results never count as fresh evidence. |
| Maintainer final review and explicit publication approval | NOT_RUN | Review exact main SHA, contents, residual risks and validation. Maintainer alone performs any explicit main-only push. |

REL-02 remains In Progress: root existence alone does not complete the original
exact-source/exclusion/object migration scope. REL-03 is Investigating;
REL-04 and REL-05 remain Blocked in the
[canonical backlog](backlog.md). No release gate was executed or promoted by
G.8 documentation work, except verification of the already-existing independent
root. The user alone performs pushes; this phase does not authorize one.

## C. Roadmap work outside the bounded experimental release

Type-5/IP-VRF, shared-VTEP, general two-chassis, multi-VTEP, RR, multihoming,
DCI, multi-cluster, dynamic IPAM, future CRDs, production deployment,
OVN-Kubernetes GoBGP integration, Neutron, G.7b.5b standalone migration/traffic,
remote BGP security, broad recovery/performance and API
modernization remain [roadmap tasks](backlog.md). Their acceptance is NOT_RUN
where not established; production installation/future CRDs are NOT_APPLICABLE
to the bounded experimental first-release promise.

Credential disclosure, unsafe redistribution and private-history contamination
are always blockers. Functional roadmap incompleteness alone is not.
