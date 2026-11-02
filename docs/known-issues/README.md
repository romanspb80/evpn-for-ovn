# Known OVN/OVS issues

This registry tracks evidence-backed runtime behavior, integration limits, and
workarounds relevant to the current Framework and CNI application. It is not an
upstream issue tracker; classifications describe the available evidence and
do not establish an upstream root cause.

- [Issue registry](ovn-ovs-issues.md) — current findings, status, workarounds,
  evidence, and test coverage.
- [OVN northd crash with dynamic routing and `addresses=unknown`](ovn-northd-dynamic-routing-unknown-crash.md)
  — reproduction and test-configuration workaround for the pinned runtime.
- [Unpublished upstream issue draft](../issues/ovn-port-binding-upstream-draft.md)
  — local draft only; no upstream issue has been filed.

The current acceptance state is summarized in the root
[README](../../README.md) and [Testing Guide](../../TESTING_GUIDE.md).
Archived attempts remain records of their specific source, runtime, and
environment; later evidence may supersede their status.
