# Type-2 route ownership and recovery

The framework treats route advertisements, speaker identity and Linux
integration entries as owned state with explicit identities and receipts.
Recovery is bounded by the current authority and the exact resources recorded
by the runtime; matching names or tuples alone are not adoption evidence.

## Speaker identity

Before route mutation or recovery, the runtime verifies the configured GoBGP
Unix endpoint, actual peer process identity and the applicable provider
attestation. A replacement process is a new identity unless current evidence
proves otherwise. Stale state must not silently authorize route reuse.

## Exact route lifecycle

The exporter associates an advertisement with its exact GoBGP path UUID and
ownership data. Withdrawal targets that path, not an NLRI-wide route set.
The importer requires complete, stable RIB observations and validates the
network mapping, VNI, route target, VTEP next hop and supported address form
before changing Netlink state. FDB and neighbor updates are limited to verified
owned entries.

## Fail-closed recovery

Durable journal intents and identity fences govern restart recovery. An
unavailable Kubernetes API, incomplete RIB, mismatched installation or node
identity, unresolved mutation, or uncertain host link does not prove an
endpoint is retired. Preserve foreign state and retain unresolved host
resources for operator investigation; do not infer cleanup authority from
missing files or stale tuples.

Restart, missed-DEL/orphan behavior and Kubernetes API-unavailability require
distinct test scenarios. Orphan/API-outage operational acceptance remains
incomplete; generalized production recovery is not guaranteed. See the
[security contract](../security/netlink-deployment-security-contract.md),
[CNI lifecycle](cni-endpoint-continuity.md), and
[release scope](../release/v0.1.0.md).

The [agent restart](../uml/node-agent-restart-sequence.svg),
[speaker restart](../uml/gobgp-speaker-restart-sequence.svg), and
[CNI orphan/API-outage](../uml/cni-orphan-api-recovery-sequence.svg) sequences
illustrate these distinct boundaries. Their [scenario notes and
procedures](../testing/lifecycle-scenarios.md) identify implemented contracts
and remaining qualification gaps; diagrams are not execution evidence.
