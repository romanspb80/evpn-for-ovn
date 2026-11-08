# Implementation TODO

The current task register has moved to the canonical
[roadmap backlog](../roadmap/backlog.md). Existing links to this page remain
valid; do not add a competing task table here.

Start with the [roadmap index](../roadmap/README.md),
[roadmap dashboard](../roadmap/overview.md),
[Framework capability assessment](../roadmap/framework.md),
[Operator/CRD boundaries](../roadmap/kubernetes-integration.md),
[API reference](../api/README.md), and
[release gates](../roadmap/release.md).

Post-LSP compensation, configuration/recovery, partial FDB invalidation and
the ProvisionVni/CreateVNI contract are FW-01 through FW-05. Shared-VTEP,
two-chassis, multi-VTEP, RR, Type-5, SVD/VLAN and IPAM are FW-06 through
FW-12. Proposed CRDs are K8S-02; Neutron, DCI and OVN-Kubernetes are
INT-01 through INT-03. None is made complete by a documentation change.

G.6/G.7 bounded milestones and the open G.7b.5b multi-chassis/migration scope
are recorded in the same backlog. SEC-01 separates remote peer protection from
local speaker attestation and endpoint authorization; UP-02 tracks selected
GoBGP API compatibility. See [implementation status](../../IMPLEMENTATION_README.md).

Verified acceptance remains in the [Testing Guide](../../TESTING_GUIDE.md).
Historical investigations are not distributed as public acceptance artifacts.
The [release gates](../roadmap/release.md) distinguish source preparation from
clean-candidate verification and publication approval.
