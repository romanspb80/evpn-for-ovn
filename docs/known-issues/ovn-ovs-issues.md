# OVN/OVS issue registry

**Last reviewed:** 2026-10-30
**Scope:** evidence for the current Framework integrations and CNI application.
This is not an upstream issue tracker. No issue in this registry has been
published upstream.

## Classification

1. **Confirmed upstream defect** — independent minimal reproduction plus
   sufficient root-cause evidence. No current item meets this bar.
2. **Reproducible upstream crash, root cause unknown** — the runtime crash is
   reproduced independently of project CNI code; source-level cause and
   upstream status are unconfirmed.
3. **Suspected upstream defect** — behavior is observed and plausibly belongs
   to an upstream component, but independent reproduction or causal evidence
   is insufficient.
4. **Integration/configuration problem** — topology, ownership, or
   asynchronous component behavior requires an integration constraint or
   bounded reconciliation; this is not evidence of an upstream defect.
5. **Test-harness problem** — the failure belongs to test setup, evidence
   collection, or fixture construction, not the product runtime.
6. **Resolved or superseded finding** — a later correction or evidence
   supersedes the original observed failure; historical evidence is retained.

## Active findings

| ID | Component | Classification | Status |
|---|---|---|---|
| [OVN-001](ovn-northd-dynamic-routing-unknown-crash.md) | OVN northd 26.09.0 | 2 — Reproducible upstream crash, root cause unknown | Active test-configuration workaround; not fixed in OVN |
| [OVN-002](#ovn-002-transient-sb-port_binding-reference-error) | OVN northd / SBDB | 3 — Suspected upstream defect | Transient referential-integrity error observed; later recompute converged |
| [OVN-003](#ovn-003-asynchronous-localnet-patch-port-cleanup) | OVN controller / OVSDB | 4 — Integration/configuration problem | Expected asynchronous detach; bounded retry succeeds |
| [OVS-001](#ovs-001-learned-fdb-state-after-partial-route-withdrawal) | OVSDB-Programmed provider-bridge path | 4 — Integration/configuration problem | Open limitation; individual learned entry is not explicitly invalidated |

## OVN-001 — northd crash with dynamic routing and `addresses=unknown`

See the [dedicated issue document](ovn-northd-dynamic-routing-unknown-crash.md)
for exact configuration, versions, commands, observations, and evidence.

- **First observed:** 2026-10-29, during endpoint delete/replacement
  diagnostics on OVN 26.09.0. An OVN-only reproduction on 2026-10-30
  independently triggered repeated northd SIGSEGVs.
- **Affected component/version:** `ovn-northd`
  `26.09.0-21.13.0-84.16`, image
  `sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505`;
  OVS `4.0.0`, SB schema `21.13.0`, NB schema `7.19.0`.
- **Expected:** northd translates valid NB logical switch and port updates
  into SB state and remains healthy.
- **Actual:** with VNI 1000 and `fdb,ip` dynamic routing on `ls24b`, adding an
  LSP whose `addresses` is `unknown` is followed by an SB transaction
  constraint error (`invalid UTF-8 sequence 0xd5 0xf4`) and repeated northd
  SIGSEGVs. The trigger does not require the CNI binary.
- **Minimal reproduction:** use the exact OVN-only commands in
  [the dedicated document](ovn-northd-dynamic-routing-unknown-crash.md).
  Remove the triggering LSP to stop additional crashes in the recorded run.
- **Evidence:** [reproducer and versions](../issues/ovn-port-binding-reproducer.md).
  Historical replacement diagnostics and offline analysis were reviewed privately;
  raw evidence is not distributed with the public source.
- **Root-cause hypothesis/confidence:** dynamic-routing advertised
  MAC/IP-binding recomputation mishandles the special `unknown` address
  representation. Confidence is high that the tested configuration triggers
  the crash; confidence is low on source-level mechanism. No core proves the
  faulting object/value.
- **Current workaround:** do not put a foreign sentinel on the
  VNI-enabled/dynamic-routing switch. The CNI-R runner creates a separate
  non-dynamic switch and a valid MAC-only sentinel there. This is a
  test-configuration workaround, not an OVN fix.
- **Limitations:** no version comparison, upstream issue search result,
  source-level fix, or complete symbolized core is available. The workaround
  is verified only for the recorded test topology/version.
- **Regression coverage:** `test_foreign_sentinel_uses_valid_mac_on_separate_non_dynamic_switch`
  in `tools/lib/test_cni_replacement_binding.py` checks the configuration and
  foreign-LSP preservation assertion offline. The successful CNI-R runtime
  also passed replacement binding, traffic, withdrawal, and cleanup.
- **Upstream readiness:** keep the [draft](../issues/ovn-port-binding-upstream-draft.md)
  unpublished. The configuration is reproducible without CNI and is suitable
  for upstream triage, but exact source fault and cross-version behavior are
  missing. Do not label this a confirmed upstream defect.

<a id="ovn-002-transient-sb-port_binding-reference-error"></a>

## OVN-002 — transient SB Port_Binding reference error

- **Affected component:** northd's Southbound database transaction while
  deleting an endpoint `Port_Binding`.
- **Classification/status:** 3 — suspected upstream defect; transient
  referential-integrity failure observed, but not established as a persistent
  OVN defect. The same run observed later recomputation remove the old row and
  replacement binding converge.
- **First observed:** 2026-10-29 during endpoint DEL; seen again in the
  isolated 2026-10-30 follow-up without the `addresses=unknown` sentinel.
- **Versions:** OVN `26.09.0-21.13.0-84.16`, OVS `4.0.0`, NB schema `7.19.0`,
  SB schema `21.13.0`, pinned image ID listed under OVN-001.
- **Expected:** deleting the NB logical port eventually removes its
  corresponding SB Port_Binding without a failed referential-integrity
  transaction.
- **Actual:** northd logged `cannot delete Port_Binding row ... because of 1
  remaining reference(s)` and `OVNSB commit failed, force recompute next time`.
  During the crash-free follow-up, the error was transient: bounded queries
  later found the old Port_Binding and Advertised_MAC_Binding absent and the
  replacement binding `up=true`.
- **Reproduction:** no standalone minimal reproduction is established. It
  occurred in historical endpoint DEL diagnostics and was also recorded in the
  [isolated reproducer](../issues/ovn-port-binding-reproducer.md).
  Do not reproduce by mutating SBDB directly.
- **Evidence:** the above reports, northd logs in the one-shot report, and
  uncommitted local direct-run logs; full SB table/reference rows are not
  retained.
- **Root-cause hypothesis/confidence:** some live SB reference temporarily
  prevents row deletion during recomputation. Confidence is low: its table,
  column, owner, and writer are unknown. Later convergence means the error
  alone did not block replacement in the crash-free run.
- **Current workaround:** none required in production based on current
  evidence. Tests should use exact-row, bounded state checks and record
  northd health; a bounded wait is diagnostic, not a fix. Do not write to
  northd-owned SB tables.
- **Regression coverage:** the CNI-R runtime recorded the transient error,
  then verified replacement `BOUND/up=true`; no deterministic offline
  regression for the upstream transaction race exists.
- **Upstream readiness:** insufficient for a standalone issue. Missing
  evidence includes an independent minimal trigger and the complete
  referenced SB row/table immediately before the failed transaction.

<a id="ovn-003-asynchronous-localnet-patch-port-cleanup"></a>

## OVN-003 — asynchronous localnet patch-port cleanup

- **Affected component:** OVN controller localnet patch-port lifecycle and
  OVSDB-Programmed provider-bridge cleanup.
- **Classification/status:** 4 — integration/configuration problem;
  verified expected asynchronous state transition, not classified as an
  upstream defect.
- **First observed:** isolated final-route withdrawal, 2026-10-30.
- **Versions:** OVN `26.09.0`, OVS `4.0.0`; current pins and topology are in
  the [dependency inventory](../architecture/runtime-dependencies.md).
- **Expected:** after localnet LSP removal, OVN controller asynchronously
  removes its patch-port attachment, then the owned provider bridge can be
  deleted.
- **Actual:** the first bridge deletion was safely refused while the patch
  port remained attached; a later reconciliation poll succeeded after OVN
  controller removed it.
- **Reproduction:** in the single-chassis M1.6 topology, withdraw the final
  remote Type-2 route and let the OVSDB-Programmed path remove its localnet
  LSP, mapping, and bridge. Observe the first refused bridge removal and
  later successful retry.
- **Evidence:** historical private runtime observation, not a distributed
  reproducer or a fresh public-candidate result.
- **Root-cause hypothesis/confidence:** OVN's patch-port teardown is
  asynchronous relative to OVSDB cleanup. Confidence is high for the
  observed sequence, not a defect attribution.
- **Current workaround:** keep ownership checks and retry cleanup through the
  existing reconciliation poll; do not use an arbitrary fixed sleep or
  forcibly delete OVN-owned patch ports.
- **Limitations:** only the recorded topology/version was tested. No
  deterministic cleanup latency is claimed.
- **Regression coverage:** OVSDB programmer tests verify cleanup ownership and
  refusal while foreign/extra bridge ports remain; they do not emulate
  controller timing. The runtime report records one successful delayed retry.
- **Upstream readiness:** not an upstream issue; the observed behavior is
  consistent with asynchronous lifecycle semantics.

<a id="ovs-001-learned-fdb-state-after-partial-route-withdrawal"></a>

## OVS-001 — learned FDB state after partial route withdrawal

- **Affected component:** OVS standalone bridge MAC learning in the
  OVSDB-Programmed provider-bridge path.
- **Classification/status:** 4 — integration/configuration problem and open
  project limitation, not an upstream defect finding.
- **First observed:** identified during provider-bridge testing; the isolated run
  verified learned MAC-to-port state and final tunnel cleanup, not individual
  partial-withdrawal invalidation.
- **Versions:** design/runtime evidence uses OVN `26.09.0`, OVS `4.0.0`.
- **Expected:** after withdrawing one remote Type-2 endpoint, forwarding
  state for that endpoint should be removed promptly without disturbing
  remaining routes.
- **Actual:** the current OVSDB-only programmer changes its route-set digest
  but does not explicitly invalidate a dynamically learned per-MAC FDB entry.
  Final tunnel deletion removes the local bridge path, but the peer Linux VXLAN
  FDB/neighbor state remained until the disposable peer container was removed.
- **Reproduction:** no live partial-withdrawal timing reproduction is
  available. M1.6 verified final-route withdrawal/cleanup and documented
  peer-side learned-state persistence in a historical private run.
- **Evidence:** OVSDB-Programmed runtime evidence, including
  historical final-route withdrawal and cleanup observations;
  partial MAC FDB invalidation remains untested.
- **Root-cause hypothesis/confidence:** OVS dynamic MAC learning and remote
  peer kernel caches are not owned by the Framework OVSDB reconciler.
  Confidence is high that explicit invalidation is absent from the described
  implementation; runtime partial-withdrawal behavior remains unverified.
- **Current workaround:** none is claimed for deterministic per-MAC
  withdrawal. Treat partial withdrawal as a limitation; do not promise
  deterministic traffic stop based on route removal alone. Final withdrawal
  removes the owned static tunnel path in the tested case.
- **Limitations:** no per-MAC invalidation mechanism has been validated; no
  latency claim. Remote-peer cache cleanup is outside Framework ownership.
- **Regression coverage:** offline OVSDB programmer tests verify owned row
  cleanup only; they do not test dynamic FDB aging or peer-side cache
  invalidation.
- **Upstream readiness:** not an upstream report. This is currently an
  implementation/ownership limitation, and live partial-withdrawal evidence
  is missing.

## Resolved or superseded findings

| ID | Finding | Classification/status | Evidence and disposition |
|---|---|---|---|
| TEST-OVN-001 | One-shot schema preflight claimed the deployed NB schema differed from the image schema | 5 — Test-harness problem; superseded | Historical image/live NB JSON objects matched version `7.19.0` and checksum `2631744256 45474`; raw-format comparison was incorrect. No SB comparison or northd reproduction ran in that attempt. Not evidence of schema incompatibility. |
| TEST-OVN-002 | Initial ping failed because the test-interface MAC differed from the local VIF LSP MAC | 5 — Test-harness problem; resolved for that run | Matching the test interface MAC to the LSP fixed ARP/ICMP in the historical run; bidirectional traffic passed 5/5 without static ARP. Not an OVN/OVS defect. |

## Related failures excluded from this OVN/OVS registry

The following failures are documented in the CNI runtime history but are not
classified as OVN/OVS issues: Multus `CNI_ARGS` receipt-parser incompatibility,
tcpdump/AppArmor evidence-directory denial, packet-capture drain/attribution
gaps, and test-runner evidence ordering. Their first failures occurred in
harness or host capture plumbing, not in OVN/OVS. The repaired runner moves
tcpdump staging to a private `/tmp` directory and the latest CNI-R run passes
its strict capture verifier. These historical diagnostics are not distributed
acceptance evidence for a new public snapshot.
