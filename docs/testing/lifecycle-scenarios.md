# Live scenario diagrams and qualification boundaries

Start with the [Testing Guide scenario table](../../TESTING_GUIDE.md#live-test-scenarios-and-expected-behavior)
and [topology boundaries](../architecture/network-topologies.md). These
diagrams explain contracts; none establishes current-release live acceptance.
All infrastructure procedures remain restricted to an authorized disposable
guest. No combined topology or generalized recovery guarantee is implied.

For the networking goals behind these tests, see the
[user-facing use cases](../use-cases/README.md). User goals and verification
scenarios are separate; not every goal has complete live acceptance.

## Secondary CNI and external Type-2

The [shared-OVN topology](../uml/multinode-secondary-external-domains.svg)
explains secondary CNI traffic only along its solid Geneve path. Dashed
external paths are separate fixtures, not a combined test. The
[CNI sequence](../uml/evpn-cni-lifecycle-sequence.svg) illustrates endpoint
ADD/CHECK/DEL plus an optional external Type-2 lifecycle; its external VXLAN
packets do not describe the ordinary Geneve-only secondary-network scenario.
Use `tools/run-cni-isolated-secondary-e2e.sh` and its
[implementation](../../tools/e2e/isolated_secondary_cni.py) for that scenario.

The [Netlink diagram](../uml/netlink-programmed-type2.svg) distinguishes
GoBGP route exchange, supporting Linux state and OVN/OVS external packets.
The [Framework sequence](../uml/evpn-framework-netlink-e2e-sequence.svg)
is a non-CNI variant; it is not the Operator CNI fixture.
For the latter, use the Testing Guide's `uc02` profile of
[the baseline Operator Type-2 runner](../../tools/run-operator-type2-e2e.sh).
The [bare-speaker fixture](../../tests/e2e/type2-fixture/README.md)
is control-plane-only and cannot replace forwarding tests.

## Agent and speaker restart

[Agent restart](../uml/node-agent-restart-sequence.svg) and
[speaker restart](../uml/gobgp-speaker-restart-sequence.svg) depict
`restart_agent_with_endpoints` and `restart_speaker_with_endpoints` in the
[Operator runner](../../tools/run-operator-type2-e2e.sh), with traffic,
withdrawal and survivor checks from
[the interop helper](../../tools/lib/cni-evpn-frr-interop-test.sh).
Use the exact `uc06-agent-restart` and `uc06-speaker-restart` selectors in
the [Testing Guide](../../TESTING_GUIDE.md#prepare-and-execute-the-scoped-scenarios).

Agent-only replacement retains the attested speaker and exact four export
receipts, restores verified endpoint state, compares captured ADD calls to
reject replay, checks one imported route and runs CHECK and packet probes.
Speaker-only replacement requires a changed attested process, four fresh
UUIDs disjoint from the old set and unchanged provider configuration
UID/spec/generation. Subsequent lifecycle assertions verify exact withdrawal,
survivors and owned cleanup. Neither diagram promises uninterrupted traffic,
bounded convergence time, combined restart or power-loss recovery.
The [ownership contract](../architecture/type2-restart-ownership.md) defines
identity and uncertainty gates.

## Foreign-state witness

There is no dedicated empty-switch sentinel diagram or built-in sentinel
extension in the secondary-CNI runner. The [Testing Guide specification](../../TESTING_GUIDE.md#live-test-scenarios-and-expected-behavior)
requires an independently owned empty OVN switch, its own cleanup receipt and
UUID/name/external-IDs/ports comparisons through ADD/CHECK/DEL. Failure or
ownership uncertainty must stop cleanup, not delete by a matching name.
Existing primary-CNI preservation checks are not that witness.
Historical external-fabric witnesses cover different resources and scenarios;
they do not fill this operational gap.

## Root CNI transport

No dedicated transport diagram is needed to replace the precise
[creation-authority contract](../architecture/cni-creation-authority.md).
Root:root `SO_PEERCRED` and kernel `SO_PEERPIDFD` bind the actual connected
Unix peer; peer exit invalidates process authority. These checks apply even
with continuity disabled. Continuity-enabled creation additionally verifies
the pinned invocation chain, held namespace and direct Kubernetes identity.

Use the exact isolated-guest root test command in the
[Testing Guide](../../TESTING_GUIDE.md#prepare-and-execute-the-scoped-scenarios).
Its entry points are `TestCNICallerActualGRPCTransport` and
`TestCNICallerPeerExit` in
[the transport tests](../../pkg/nodeagent/cni_caller_linux_test.go).
Non-root skips leave this boundary unchecked; neither a socket probe nor
the existing CNI lifecycle diagram proves caller authentication.

## Multi-worker and multi-VTEP

The [Attempt 8 diagram](../uml/operator-inventory-type2-two-worker.svg)
is historical, shared-OVN and inventory-gated. Its cited forwarding does
not assert worker-to-worker packets, restart, withdrawal or recovery.
The [external-fabric topology](../uml/external-evpn-fabric.svg) is proposed;
it is not acceptance of that combined topology. Use the
[two-worker validation procedure](../../deploy/operator/examples/two-worker/VALIDATION.md),
[historical external-fabric record](../internal/validation-history/external-evpn-fabric.md), and separate
[Remote2 restart procedure](../../tools/e2e/remote2/README.md#bounded-live-r3-runner)
without merging their scopes. Independent OVN domains and generalized
multi-VTEP lifecycle remain unqualified.

## Orphan and API-unavailability

The [orphan/API sequence](../uml/cni-orphan-api-recovery-sequence.svg)
is grounded in `cleanupOrphanLocked` / `rescanCNIReceiptsLocked` in
[continuity code](../../pkg/nodeagent/cni_continuity.go) and
[orphan regression tests](../../pkg/nodeagent/cni_orphan_recovery_test.go),
plus API-error negatives in
[continuity tests](../../pkg/nodeagent/cni_continuity_test.go).
The [detailed contract](../architecture/cni-endpoint-continuity.md#recovery-and-acknowledgements)
specifies exact DB cleanup and the orphan path's host-veth no-delete rule.

**Operational fixture gap:** there is no dedicated missed-DEL/Kubernetes
API-outage case in the scoped Operator profiles. Their NBDB/OVSDB pauses
are database-observation outages, not Kubernetes API failure. Separate
failure injections, independently confirmed original Pod retirement,
exact active receipts, journal comparisons and foreign-survivor observations
are required by the Testing Guide. API errors never authorize withdrawal
or cleanup. This diagram documents implemented behavior and incomplete
qualification, not a recovered live result or a new ready-to-run procedure.

## Direct OVSDB maintenance

Reuse the [programming-mode comparison](../uml/evpn-dataplane-comparison.svg)
only for architecture scope. There is no dedicated direct-OVSDB lifecycle
sequence. Use the isolated-guest
[acceptance-gap procedure](vm-e2e-lab.md#acceptance-gap-overlays) for
`tools/run-framework-ovsdb-e2e.sh` and its
[gap helper](../../tools/lib/framework-ovsdb-gap-cases.sh).
These maintenance tests concern receipt-owned OVSDB/OVN resources, snapshot
outages, restart, withdrawal and foreign conflicts; they are not the
selected Netlink deployment profile. Advanced operational qualification
remains deferred.
