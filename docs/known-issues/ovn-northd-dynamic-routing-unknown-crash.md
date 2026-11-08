# OVN northd SIGSEGV with dynamic routing and `addresses=unknown`

- **Local issue:** `OVN-001`
- **Classification:** reproduced issue requiring root-cause analysis
- **Status:** active test-configuration workaround; not fixed in OVN
- **Upstream:** draft only; no issue has been published

## Summary

In the pinned OVN `26.09.0-21.13.0-84.16` runtime, northd repeatedly SIGSEGVs
after a Logical_Switch_Port with `addresses=unknown` is added to a logical
switch configured for EVPN dynamic routing and `fdb,ip` redistribution. The
trigger was reproduced using OVN CLI/OVSDB operations without a CNI request.
The exact source-level fault and the invalid value's origin are unknown.

## Environment

- OVN image ID:
  `sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505`
- OVN: `26.09.0-21.13.0-84.16`
- NB schema: `7.19.0`; SB schema: `21.13.0`
- OVS: `4.0.0`; OVSDB schema: `8.8.0`
- Linux kernel: `6.8.0-147-generic`
- Run ID: `cni-r-20261030T133010Z-1a0f5ac5`
- Isolated OVN/OVS container used `--network=none`, no host mounts, and
  `Privileged=false`; only `CAP_NET_ADMIN` and `CAP_SYS_ADMIN` were granted.

## Reproduction

Start the pinned OVN/OVS image in a disposable isolated runtime, verify
`ovn-northd` is healthy, and create the bridge/VXLAN/advertisement devices
within that runtime. Then apply:

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

The transaction was followed by an `OVNSB commit failed` message with an
invalid UTF-8 constraint error (`0xd5 0xf4`) and repeated northd SIGSEGVs.
Inspect `/var/log/ovn/ovn-northd.log` and the northd process in the isolated
container; do not use a host OVN/OVS service or active cluster.

Remove only the triggering test LSP:

```sh
ovn-nbctl lsp-del interop-bootstrap
```

The recorded crash count stopped at 13 after removal. This is recovery
evidence for the isolated run, not proof that the daemon's internal state was
fully repaired or that every reproduction stops at the same count.

## Working CNI/EVPN configuration

The current runner creates a dynamic-routing switch for the local EVPN test
network and places foreign-state test data on a separate switch that has no
dynamic-routing options:

```sh
-- ls-add "${FOREIGN_SWITCH}" \
-- lsp-add "${FOREIGN_SWITCH}" "${FOREIGN_LSP}" \
-- lsp-set-addresses "${FOREIGN_LSP}" 02:00:00:00:fa:02
```

`FOREIGN_SWITCH` is run-unique (`interop-foreign-${KIND_CLUSTER}`); the
sentinel uses a valid MAC-only address. The runner records the sentinel LSP
UUID before lifecycle operations and verifies that the same UUID remains
afterward. The prior fixture placed `addresses=unknown` on `ls24b`, which
shares the dynamic-routing configuration and triggered the observed crash.

This workaround is restricted to test setup. It changes no CNI, node-agent, or
Framework production logic. A focused offline test protects the
switch separation, valid address, and foreign-LSP preservation assertion.
The complete source-built CNI-R runtime then passed replacement binding,
bidirectional traffic, Type-2 route withdrawal, capture verification, and
owned-resource cleanup.

## Evidence and limits

- [Prior reproducer record](../issues/ovn-port-binding-reproducer.md)
- [Unpublished upstream draft](../issues/ovn-port-binding-upstream-draft.md)
- Historical replacement-binding diagnostics and offline northd analysis are
  private development records, not included public acceptance artifacts.
- Raw direct-run logs and DB snapshots were local and were not committed; the
  container has been removed.

Same-image offline symbolization places recomputation in advertised-MAC/IP
binding generation, including `advertised_mac_binding_entry_find` and
`advertised_mac_binding_add_with_mac`, but does not identify the faulting
instruction or object. No core with the exact crashing locals is retained.
The separate transient Port_Binding reference error is tracked as `OVN-002`;
do not assume it is the cause of this crash.

The workaround is verified for the pinned runtime and test topology only. No
other OVN version was tested, no matching upstream issue/fix was verified, and
no source-level OVN change was made. Keep the upstream draft unpublished until
the missing source/core and version-comparison evidence is available.
