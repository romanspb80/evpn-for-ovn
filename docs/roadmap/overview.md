# Roadmap overview

The [backlog](backlog.md) is the authoritative source for task definitions,
dependencies and acceptance criteria. The
[capability matrix](../project/capability-matrix.md) is the current index of
implemented behavior and validation scope. Neither historical test results nor
this summary establish production readiness.

## Near-term priorities

- **Release and security:** review source and distribution provenance,
  reproducible inputs, management API security and the exact release snapshot.
- **Reliability:** strengthen ownership-aware recovery, endpoint rollback,
  speaker configuration ownership, and Operator lifecycle and upgrades.
- **Networking:** extend shared-VTEP and multi-VTEP lifecycle, and validate
  broader two-chassis failure cases for each integration mode.
- **Integration:** investigate OVN-Kubernetes and Neutron/ML2-OVN adapters
  without introducing competing configuration writers.
- **Research:** explore routed EVPN, multihoming, route reflection and
  measurable convergence and scale.

The full external-fabric acceptance status is **unresolved** pending
verification of final manifests and source/build identities. Separate bounded
scenarios are described in the [capability matrix](../project/capability-matrix.md).
The [history index](../archive/README.md) records significant transitions.

[Roadmap index](README.md) · [Backlog](backlog.md) ·
[Known issues](../known-issues/README.md) · [Release gates](release.md)
