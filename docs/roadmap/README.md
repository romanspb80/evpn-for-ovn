# Development roadmap

This directory is the canonical current plan. The
[backlog](backlog.md) defines each task once, with stable IDs, evidence,
dependencies and acceptance criteria. Topic pages explain architecture and
link to those tasks; they are not competing TODO lists.

| Topic | Read here |
|---|---|
| First public release and remaining P0 gates | [Release readiness](release.md) |
| Implemented Framework and capability boundaries | [Framework](framework.md) |
| EvpnCore, proposed CRDs and CNI | [Kubernetes integration](kubernetes-integration.md) |
| OVN-Kubernetes, OVN/OVS, k8gobgp, GoBGP, Cilium | [Upstream index](upstream/README.md) |
| Neutron, DCI and multi-cluster | [Integration index](integrations/README.md) |
| Service contracts, implementations and callers | [API reference](../api/README.md) |

Priorities: **P0** blocks the first public release; **P1** is near-term
post-publication work; **P2** is planned functionality/integration; **P3** is
research. Status vocabulary: `Proposed`, `Investigating`, `Blocked`,
`In Progress`, `Verified`, `Done`. A proposal or source implementation alone
is not end-to-end acceptance.

The [Testing Guide](../../TESTING_GUIDE.md) remains authoritative for test
contracts and historical results. C4.4f.2q.2 established one-chassis Type-2
Framework and CNI scopes, not shared-VTEP, two-chassis, RR, Type-5 or DCI.
Publication requires the separate [release gates](release.md). A documentation
or source-preparation baseline is not permission to publish.

Legacy navigation paths [implementation TODO](../architecture/implementation-todo.md),
[upstream plan](upstream.md) and [integration goals](integration-goals.md)
are retained as pointers. Archived reports retain their original terminology
and historical status; they do not define today's plan.
