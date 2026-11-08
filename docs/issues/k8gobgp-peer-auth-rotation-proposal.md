# Proposal draft: documented Secret-backed BGP key rotation and reconciliation

**Unsubmitted.** Upstream: [purelb/k8gobgp](https://github.com/purelb/k8gobgp).
Canonical work: [SEC-01](../roadmap/backlog.md#sec-01) /
[FW-02](../roadmap/backlog.md#fw-02).
Classification: **Research or enhancement proposal**. Source-supported
reconciliation limitation; no live rotation failure or confirmed upstream
defect is claimed.

## Version / existing functionality

k8gobgp v0.2.8 source
[`71b26cb8ea5d48762c00f764e0204ce03e9b6e46`](https://github.com/purelb/k8gobgp/tree/71b26cb8ea5d48762c00f764e0204ce03e9b6e46),
selected `purelb/gobgp-netlink/v4 v4.900.8`, managed daemon
`PureLB-fork:01.03.08`, commit `6b99046`; Linux GoBGP/FRR fixture.

`authPasswordSecretRef` already exists for neighbors and peer groups and is
resolved in the BGPConfiguration namespace into GoBGP auth fields. This proposal
does **not** request that feature again or add an EVPN-for-OVN neighbor writer.

## Source evidence and requested behavior

The pinned
[controller](https://github.com/purelb/k8gobgp/blob/71b26cb8ea5d48762c00f764e0204ce03e9b6e46/controllers/bgpconfiguration_controller.go)
shows:

- `resolveAuthPassword` reads exactly `Secret.Data[selector.key]`; named missing
  Secret/key returns an error. Sample address/default-key wording is not an
  automatic fallback.
- `SetupWithManager` watches BGPConfiguration generations and Node labels,
  not referenced Secrets; successful Reconcile has no periodic requeue.
- `peerConfigEqual` intentionally ignores redacted password values;
  `authPasswordDrifted` detects only set/unset state.

Expected enhancement: explicitly document exact-key semantics and support a
bounded referenced-Secret update mechanism with a safe non-secret change
indicator and deliberate peer reconfiguration/rotation semantics. Avoid
comparing redacted readback passwords or leaking keys/hashes into events/logs.
Consider peer groups separately and preserve inheritance and sole-writer
ownership.

Actual source consequence: a nonempty-to-nonempty Secret-only change has no
guaranteed event or value-change detection for a stable neighbor. Merely
touching an unrelated field is not a proven rotation procedure.

## Proposed minimal live reproduction (not run in G.8)

1. Establish an authenticated EVPN GoBGP/FRR session using a Secret reference
   with an explicit key and record versions, non-secret config and state.
2. Change only that referenced Secret's nonempty key value and coordinate the
   FRR key change. Observe reconcile events, peer reconfiguration and reconnect.
3. Repeat with a BGPConfiguration spec event, peer-group inheritance, manager
   restart and daemon restart; include empty/missing key and wrong-key controls.
4. Check no unauthenticated fallback, no unrelated peer/path deletion, no
   continuously churned UpdatePeer and no secret values in retained evidence.

Workaround: no validated hot-rotation procedure is claimed. Use an explicitly
planned maintenance/reconfiguration window only after independent testing;
do not bypass k8gobgp with direct framework peer mutation.

## Duplicate check / readiness

Existing Secret-reference support is verified, preventing a duplicate feature
request. Targeted issue/PR duplicate checks for rotation and exact-key wording
remain **pending**. Runtime behavior, peer-group handling and maintainer contract
must be established before bug attribution or submission. Source links are the
evidence; no sanitized live rotation bundle exists yet.
