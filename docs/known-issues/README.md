# Known issues and upstream candidates

This registry tracks evidence-backed runtime behavior, integration limits, and
workarounds relevant to the current Framework and CNI application. It is not an
upstream issue tracker; classifications describe the available evidence and
do not establish an upstream root cause.

- [Issue registry](ovn-ovs-issues.md) — current findings, status, workarounds,
  evidence, and test coverage.
- [Control-plane compatibility](control-plane-issues.md) — GoBGP UUID/MAC-only
  constraints, k8gobgp authentication reconciliation and OVN-Kubernetes proposal.
- [OVN northd crash with dynamic routing and `addresses=unknown`](ovn-northd-dynamic-routing-unknown-crash.md)
  — reproduction and test-configuration workaround for the pinned runtime.
- [Unpublished upstream issue draft](../issues/ovn-port-binding-upstream-draft.md)
  — local draft only; no upstream issue has been filed.
- [All unsubmitted drafts](../issues/README.md) and
  [remote BGP security plan](../roadmap/upstream/bgp-peer-security.md).
- [Canonical backlog](../roadmap/backlog.md) and
  [roadmap dashboard](../roadmap/overview.md).

OVN-001/002 require RCA; OVN-003 is expected asynchronous behavior; OVS-001 is
a framework limitation. Selected GoBGP UUID omission is an API behavior,
MAC-only encoding and k8gobgp rotation need compatibility/implementation
investigation, and OVN-Kubernetes is an enhancement proposal. No current record
meets the confirmed-upstream-defect bar. Duplicate checks are partial/pending,
not proof that no related issue exists.

The current acceptance state is summarized in the root
[README](../../README.md) and [Testing Guide](../../TESTING_GUIDE.md).
Archived attempts remain records of their specific source, runtime, and
environment; later evidence may supersede their status.
