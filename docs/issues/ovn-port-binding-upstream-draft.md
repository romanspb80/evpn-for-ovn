# Draft: OVN northd SIGSEGV with dynamic-routing switch and `addresses=unknown`

**Publication status: draft only. No upstream issue has been filed.**

**Proposed title:** OVN northd SIGSEGV with dynamic-routing switch and `addresses=unknown`.
**Upstream repository:** [ovn-org/ovn](https://github.com/ovn-org/ovn).
**Classification:** reproduced issue requiring root-cause analysis, not a
confirmed source-level upstream defect.
**Canonical task:** [UP-01](../roadmap/backlog.md#up-01).
The historical `ovn-port-binding-*` filename is preserved for existing links;
this draft concerns the northd crash, not the separate transient reference error.

Local evidence classification: [OVN-001 in the known-issues registry](../known-issues/ovn-ovs-issues.md).

## Summary

In the pinned OVN 26.09.0 runtime, northd repeatedly SIGSEGVs after a
Logical_Switch_Port with `addresses=unknown` is added to a
dynamic-routing-enabled Logical_Switch. The observed sequence includes a
Southbound transaction constraint violation reporting invalid UTF-8 bytes.
The same trigger was exercised with OVN CLI/OVSDB operations and does not
depend on the CNI binary.

## Environment

- Image: `evpn4ovn/ovn-ovs:kind-26.09`
- Image ID: `sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505`
- OVN: `26.09.0-21.13.0-84.16`
- OVN NB schema: `7.19.0`; SB schema: `21.13.0`
- OVS: `4.0.0`; OVSDB schema: `8.8.0`
- Kernel: `6.8.0-147-generic`
- Isolated disposable container, `network=none`; no host database sockets or
  mounts. Runtime setup granted `CAP_NET_ADMIN` and `CAP_SYS_ADMIN`, not
  `--privileged`.

## Steps

1. Start the pinned OVN/OVS image in a disposable isolated runtime and verify
   `ovn-northd` is healthy.
2. Create and bring up `evpnbr33`, `evpnvx33`, and `evpnad33` within that
   runtime.
3. Configure the dynamic-routing switch and add the LSP:

   ```sh
   ovn-nbctl -- ls-add ls24b \
     -- set Logical_Switch ls24b \
       other_config:dynamic-routing-vni=1000 \
       other_config:dynamic-routing-bridge-ifname=evpnbr33 \
       other_config:dynamic-routing-vxlan-ifname=evpnvx33 \
       other_config:dynamic-routing-advertise-ifname=evpnad33 \
       other_config:dynamic-routing-redistribute=fdb,ip \
     -- lsp-add ls24b interop-bootstrap \
     -- lsp-set-addresses interop-bootstrap unknown
   ```

4. Inspect `/var/log/ovn/ovn-northd.log` and the northd process.

## Expected and observed

Expected: northd accepts the logical-port update, translates it to
Southbound, and remains healthy.

Observed: the run logged an OVSDB transaction constraint violation with
`invalid UTF-8 sequence 0xd5 0xf4`, followed by repeated northd SIGSEGVs.
Retained same-image stack symbolization places the recompute path in
advertised MAC/IP binding generation. The exact faulting value and source
instruction are not established.

Removing only `interop-bootstrap` stopped additional crashes in the recorded
run. A valid MAC-only foreign LSP on a separate non-dynamic switch translated
to SB without increasing the crash count. This is a configuration workaround
for the test harness, not a verified upstream fix.

## Related endpoint replacement observation

In the same isolated run, CNI endpoint deletion could produce a transient
SB Port_Binding referential-integrity error for one remaining reference.
Northd recomputation removed the old Port_Binding and its
Advertised_MAC_Binding, and a replacement endpoint subsequently received a
bound/up Port_Binding. The reference owner/table remains unknown. The evidence
does not prove that the referential-integrity error alone causes the crash.

## Hypothesis and confidence

**Confirmed behavior:** this pinned runtime crashes after the described
OVN-only configuration; CNI is not required for the trigger.

**Hypothesis:** the dynamic-routing advertised-MAC recomputation path does
not safely handle the special `unknown` address representation and reaches an
invalid/corrupted value. Confidence is high that the configuration triggers
the crash and moderate that the fault is in northd; confidence is low on the
precise source-level mechanism until a core or reproducer on another OVN
build is available.

The observations are sufficient to request upstream triage, but no matching
issue, release fix, or version comparison has been verified. This draft must
be updated with a minimized standalone OVN-only reproducer and a core/symbol
trace before publication if those become available.

## Evidence and duplicate-check status

The [historical reproduction record](ovn-port-binding-reproducer.md) and
[dedicated configuration record](../known-issues/ovn-northd-dynamic-routing-unknown-crash.md)
contain the exact tested configuration/version, CLI sequence and log excerpts.
Retained stack symbolization mentions `advertised_mac_binding_entry_find` /
`advertised_mac_binding_add_with_mac`; no core proves a faulting value.
Raw local runtime logs are not a distributed public evidence bundle.

G.8 performed a preliminary OVN issue search; related northd reports were
returned but no exact duplicate was established. Full issue/PR review and
cross-version checks remain **pending**; no verified issue number or fix is
assigned. Before submission, retain sanitized logs/core and exact source/build
provenance, minimize setup interfaces and confirm reproducibility independently.
The existing recipe supports triage, not a root-cause-confirmed bug claim.
OVN-002 requires its own independent trigger/referring-row capture before a
separate issue draft; do not combine it into the crash's causal explanation.
