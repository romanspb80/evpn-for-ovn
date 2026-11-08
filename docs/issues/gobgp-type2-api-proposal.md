# Proposal draft: clarify EVPN Type-2 UUID visibility and MAC-only encoding

**Unsubmitted.** Upstream target: [osrg/gobgp](https://github.com/osrg/gobgp);
compare the selected [purelb/gobgp-netlink](https://github.com/purelb/gobgp-netlink)
fork before choosing the responsible implementation.
Canonical work: [UP-02](../roadmap/backlog.md#up-02).
Classification: **Research or enhancement proposal**, with confirmed local
client/runtime compatibility constraints, not a root-cause-confirmed defect.

## Version and environment

Framework Go client `osrg/gobgp/v4 v4.5.0`; managed daemon
`PureLB-fork:01.03.08`, commit `6b99046`, selected by k8gobgp v0.2.8.
Linux single-chassis OVN/OVS/GoBGP/FRR fixture; Go 1.27.3.
See [dependency inventory](../architecture/runtime-dependencies.md).
No independent reproduction on current stock GoBGP is claimed.

## Local evidence / proposed minimal comparison

`pkg/evpn/type2_gobgp.go` and its tests exercise two distinct API questions:

1. Add one valid local EVPN Type-2 path with AddPath, retain its returned UUID,
   then ListPath the complete global EVPN RIB. Compare UUID fields with the
   exact AddPath receipt; withdraw only that UUID. Repeat on stock and fork
   versions, recording protobuf descriptor/version and complete payload.
2. Construct an otherwise identical MAC-only Type-2 NLRI with absent IP through
   the v4.5.0 typed apiutil path and binary BGP serialization. Record the typed
   `invalid IP` failure versus binary success, AddPath/ListPath roundtrip and
   peer visibility. Compare an explicit valid MAC/IP route as control.

The above is a minimized **test recipe for upstream comparison**, not a newly
executed cross-version result. Existing framework tests and bounded live receipt/
withdrawal evidence are linked from the
[implementation milestones](../roadmap/backlog.md#completed-g6g7-milestones) and
[smoke manifest](../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a).

## Expected versus actual / requested clarification

Expected: a documented distinction between mutation UUID receipts and readback
identity, and an explicit supported API representation for absent-IP Type-2.
A UUID readback enhancement would be useful only with clearly stated identity
and restart semantics; omission may be intentional API behavior.

Actual locally: ListPath omits UUIDs in the selected API/runtime behavior;
typed absent-IP encoding fails with `invalid IP`. These do not establish that
all current GoBGP versions or binary NLRI are defective.

Workaround: retain durable AddPath UUID receipts, validate authoritative
payload before UUID-only DeletePath, and use binary NLRI for MAC-only.
Never adopt a matching path after ambiguous AddPath, fabricate a UUID, or use
NLRI deletion as a fallback. No workaround is being removed by this proposal.

## Duplicate check / submission readiness

Preliminary semantic issue search returned related EVPN encoding work, but no
matching issue or fix was established. Issue/PR review, stock/fork version
comparison and a small independently runnable reproducer remain **pending**.
Split encoding and UUID discussion if upstream prefers separate scopes.
Not ready to publish as a confirmed bug; no issue number is assigned.
