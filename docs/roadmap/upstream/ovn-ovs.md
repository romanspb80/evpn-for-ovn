# OVN / OVS integration triage

OVN NB/SB logical state, northd translation and controller-owned forwarding
must remain distinct from Framework-owned integration objects. Relevant
upstream references are the
[dynamic-routing architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html),
[NB schema](https://www.ovn.org/support/dist-docs/ovn-nb.5.html) and
[controller manual](https://www.ovn.org/support/dist-docs/ovn-controller.8.html).
These documents describe seams, not acceptance of every configuration.

| Local record | Classification | Evidence and contribution boundary |
|---|---|---|
| OVN-001 | Reproduced issue requiring root-cause analysis | northd 26.09.0 crash with dynamic routing and `addresses=unknown`; root cause not proven. Preserve separate non-dynamic sentinel workaround. Symbols/core, minimal OVN-only reproducer and version comparison required before a confirmed-defect/fix claim. |
| OVN-002 | Reproduced issue requiring root-cause analysis | Transient SB Port_Binding reference error; later recompute converged. Establish exact referring table/column and independent trigger; do not call it the OVN-001 root cause. |
| OVN-003 | Expected upstream behavior | Localnet patch cleanup is controller-managed. Wait/retry/refuse unsafe deletion; do not force-delete controller state or submit expected reconciliation as a bug. |
| OVS-001 | Framework limitation | Partial withdrawal does not explicitly invalidate a learned per-MAC provider FDB entry. Final tunnel cleanup is accepted; partial survivor timing is not. Investigate supported OVS invalidation before proposing an upstream API improvement. |

Details remain in the [diagnostic registry](../../known-issues/ovn-ovs-issues.md),
[reproducer](../../issues/ovn-port-binding-reproducer.md) and
[unsubmitted draft](../../issues/ovn-port-binding-upstream-draft.md).
Canonical work: [UP-01 and FW-05](../backlog.md#p1-reliability-and-contract-clarification).
There is no verified public issue/PR URL or root-cause-confirmed upstream fix.
G.8 preliminary OVN searches did not establish a duplicate; full issue/PR
review remains pending in the draft. No source reproduction was rerun here.

Netlink mode lets `ovn-controller` observe Linux EVPN integration state.
OVSDB mode instead creates an owned localnet/provider-bridge/static-VXLAN
path; it must retain NB/SB and OVS ownership receipts and respect asynchronous
patch teardown. Neither mode may delete controller-managed objects.
Shared-VTEP, multi-VTEP and MAC-level convergence need separate acceptance,
not weaker tests or an assumed upstream defect.
