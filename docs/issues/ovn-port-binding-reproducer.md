# OVN northd crash reproducer: dynamic-routing switch and `addresses=unknown`

Current classification and workaround status are tracked in the
[OVN/OVS known-issues registry](../known-issues/ovn-ovs-issues.md).

## Result

The pinned OVN 26.09.0 runtime independently reproduces a northd crash when a
Logical_Switch_Port with `addresses=unknown` is added to a switch configured
for dynamic routing and `fdb,ip` redistribution. No CNI request is needed to
trigger it. The exact invalid memory/object and source-level fault are not
proven, so the result supports an OVN defect or defect-triggering configuration
interaction; it is not a confirmed upstream diagnosis.

The earlier endpoint-replacement failure is consistent with this trigger:
after removing the `unknown` LSP, a transient Port_Binding deletion reference
error recovered during northd recomputation, and the replacement binding
appeared bound/up. This does not prove that the `unknown` LSP is the only
condition required for the earlier crash.

## Runtime identity and isolation

- OVN image: `evpn4ovn/ovn-ovs:kind-26.09`
- Image ID: `sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505`
- OVN: `26.09.0-21.13.0-84.16`; NB schema `7.19.0`; SB schema `21.13.0`
- OVS: `4.0.0`; OVSDB schema `8.8.0`
- Linux kernel: `6.8.0-147-generic`
- Run ID: `cni-r-20261030T133010Z-1a0f5ac5`
- Container network mode: `none`; no host mounts; not privileged. The OVN/OVS
  container had only `CAP_NET_ADMIN` and `CAP_SYS_ADMIN`; endpoint containers
  also used `network=none` and shared only the OVN container's PID namespace
  so the node-agent could enter their network namespaces.
- No host OVN/OVS socket, kind cluster, FRR container, GoBGP process, or
  stabilization repository was used for this trigger reproduction.

## Reproduction

Start from the isolated image above with `ovn-northd` healthy. Create the
dynamic-routing bridge, VXLAN, and advertise interfaces in that isolated
runtime, then configure the switch and sentinel as follows:

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

Observe the NB transaction result and northd log:

```sh
grep -E 'invalid UTF-8|SIGSEGV detected' /var/log/ovn/ovn-northd.log
```

In the recorded run, northd logged a Southbound transaction constraint
violation containing `invalid UTF-8 sequence 0xd5 0xf4`, then repeatedly
terminated with SIGSEGV. Offline symbolization of retained same-image stacks
places the setup/recompute path in advertised MAC/IP binding generation,
including `advertised_mac_binding_entry_find` and
`advertised_mac_binding_add_with_mac`; the return addresses do not identify
the actual faulting instruction or value.

Remove only the test LSP:

```sh
ovn-nbctl lsp-del interop-bootstrap
```

The recorded northd crash count stopped at 13 after removal. Subsequent
endpoint lifecycle work completed without another crash. A valid foreign LSP
(`02:00:00:00:fa:02`) on a separate switch without dynamic-routing options
translated to SB and left the crash count unchanged. That is a tested
test-harness workaround, not a northd source fix.

## Port_Binding lifecycle observations

When the `unknown` LSP was absent, deleting a test endpoint produced a
referential-integrity error stating that one reference remained on the
Port_Binding. Northd recomputed, and bounded exact-name SB queries then
confirmed that the old Port_Binding and Advertised_MAC_Binding were gone.
After replacement ADD, its exact Port_Binding appeared on the local chassis
with `up=true`. The error was transient in this run; the remaining reference
table and writer are still unidentified.

The CNI-specific checks in the same isolated run passed: two endpoint
attachments, bidirectional ICMP, CHECK, exact-identity DEL and repeated DEL,
failure rollback, same-identity re-ADD, and final owned-resource cleanup.
The CNI_CONTAINERID used for DEL had to match the original ADD incarnation;
a mismatched ID was treated as stale and did not delete the endpoint.

## Classification and limits

**Facts:** the configuration above was exercised in the pinned runtime; the
transaction error and SIGSEGV followed; removing the `unknown` LSP stopped
further crashes in the run; valid foreign state on a separate static switch
translated normally.

**Hypothesis:** northd mishandles the combination of dynamic-routing
advertised-MAC recomputation and the special `unknown` address representation.
The invalid UTF-8 symptom is consistent with corrupted/uninitialized data,
but no retained core proves that interpretation.

This evidence is sufficient for an unpublished upstream report draft, not to
claim a confirmed upstream issue or root cause. No upstream issue was created
or published. Raw run observations and historical replacement-binding and
offline-symbolization reports are private development records, not distributed
with public source or claimed as new-candidate validation.
