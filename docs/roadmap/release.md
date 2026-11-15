# Experimental release gates

> For current capability status, see the [capability matrix](../project/capability-matrix.md);
> remaining release work is tracked in the [backlog](backlog.md).

The current release remains an experimental Go EVPN Framework, **not
production-qualified**. The modern Go implementation and historical Python
prototype have independent histories; see [project provenance](../archive/README.md).
Previous preparation results apply only to their reviewed inputs and do not
establish readiness of a new release candidate.
PASS requires executed evidence; FAIL is an unmet requirement; NOT_RUN is not
a pass; NOT_APPLICABLE identifies deliberately excluded scope.

## Experimental deployment boundary

The shared Framework uses GoBGP and OVN/OVS with alternative Netlink-Programmed
and OVSDB-Programmed integrations. Kubernetes uses k8gobgp-managed gobgpd.
Optional CNI/Multus consumes reusable Node Agent endpoint/network services.
EvpnCore is manifest-apply scaffolding, not k8gobgp's controller or a complete
installer. CMS is an optional test/demo emulator, not Framework core.
Containerlab provisions test environments, not production networking.

Historical scoped tests are indexed in the
[validation record](../archive/validation-record.md); they are not clean-candidate
release runs. Framework gRPC TLS was not enabled in those historical lab
acceptances. The [capability matrix](../project/capability-matrix.md)
describes current boundaries.

Previous source, licensing and safety reviews are summarized in the
[licensing review](../release/licensing.md) and [historical record](../archive/README.md).
They do not substitute for reviewing the exact release candidate. An unrun
gate is not a pass merely because a different historical suite passed.

## Release-candidate gates

| Gate | Result | Acceptance |
|---|---|---|
| Source scope and history review | NOT_RUN | Review release source, dependencies, attribution, and preserved project history. |
| Release-tree security/privacy screening | NOT_RUN | Review the complete candidate and reachable history for credentials, private records, and unsafe inherited content. |
| Clean-candidate build, static checks, and documentation links | NOT_RUN | Run documented safe checks against the candidate and verify links and generated artifacts. |
| Clean live reproduction or approved alternative | NOT_RUN | Reproduce the Netlink/OVSDB/CNI contracts on disposable infrastructure, or document an explicitly approved alternative. Historical private results are not fresh evidence. |
| Maintainer review and publication approval | NOT_RUN | Review exact candidate contents, scope, residual risks, and validation before publication. |

Remaining release work is tracked in the
[canonical backlog](backlog.md). Historical branch preservation and bounded
scenario results do not establish that the current candidate meets these gates.

## Work outside the bounded experimental release

Type-5/IP-VRF, shared-VTEP, general two-chassis, multi-VTEP, RR, multihoming,
DCI, multi-cluster, dynamic IPAM, future CRDs, production deployment,
OVN-Kubernetes GoBGP integration, Neutron, broader standalone failure recovery,
remote BGP security, broad recovery/performance and API
modernization remain [roadmap tasks](backlog.md). Their acceptance is NOT_RUN
where not established; production installation/future CRDs are NOT_APPLICABLE
to the bounded experimental first-release promise.

Credential disclosure, unsafe redistribution and private-history contamination
are always blockers. Functional roadmap incompleteness alone is not.
