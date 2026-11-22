# Two-Chassis EVPN Type-2

## Recorded scenario

A bounded standalone run used two OVN/OVS namespaces with separate switches and
VTEPs. The chassis shared one NBDB/SBDB control plane and ran directly peered
GoBGP speakers. This is a multi-chassis topology, not interconnection of
independent OVN domains.

The report records PASS for EVPN Type-2 exchange, bidirectional VXLAN
forwarding, reconciliation stability, Agent-A restart while both speakers
were preserved, and owned cleanup. Optional non-CNI endpoint uncertainty,
deletion, replacement, replay rejection, ordered migration and recovery cases
also passed in that attempt. These are bounded outcomes for that fixture and
are not a claim of generalized mobility or Operator inventory recovery.

## Provenance and limits

- Base source: `ae07988afdd08b860eb45221ed50c01e1995769f`.
- Source tree: `017ce27efa6eba344f519964184b450845b36b63`.
- Test-only overlay: changes to `tools/e2e/operator_secondary_cni.py` and
  `tools/e2e/test_operator_secondary_cni.py`; the retained record notes capture
  setup, status handling and run-owned FRR capture/ping tools.
- Evidence: local `live-results.tar`, 2,242,058,240 bytes, SHA-256
  `0016964c2fc1a25d7fdebe4efa1196aa1350ff4fb8eb0a5507583d89b22df486`.
- Availability: the bundle is referenced in retained session evidence but is
  not distributed with this documentation tree. The available provenance does
  not establish that the overlay applied to every standalone assertion.

The historical result supports only the listed shared-control-plane scenario.
It does not prove separate NB/SB domains, arbitrary vendor interoperability,
an exact final release inventory, or the release baseline's full acceptance.
See [release acceptance evolution](release-evidence.md) and the
[independent-domain roadmap](../../roadmap/README.md#independent-ovn-domain-interconnection-planned).
