# Architecture proposal draft: optional GoBGP control-plane integration seam

**Unsubmitted enhancement, not a bug report.**
Upstream: [ovn-kubernetes/ovn-kubernetes](https://github.com/ovn-kubernetes/ovn-kubernetes).
Canonical work: [INT-03](../roadmap/backlog.md#int-03).
Classification: **Research or enhancement proposal**.

## Current context and version limits

Inspected upstream
[EVPN documentation](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/bgp-integration/evpn.md)
and [route advertisements](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/bgp-integration/route-advertisements.md)
describe CUDN/VTEP/MAC-VRF/IP-VRF and FRR/frr-k8s configuration. These moving
references are not a tested/pinned OVN-Kubernetes integration version.
No OVN-Kubernetes runtime reproduction, supported provider ABI or compatible
GoBGP adapter exists in EVPN-for-OVN.

## Proposed discussion

First evaluate an external integration consuming network/VTEP/endpoint intent
without modifying OVN-Kubernetes-owned OVN rows, devices, FDBs or routes.
Only if that is insufficient, identify a narrow upstream control-plane seam
for desired peer/VRF/policy/route state and explicit capabilities. A GoBGP
implementation would map to its existing provider/configuration owner rather
than replace FRR merely by changing a container image.

Expected enhancement: reviewed optional control-plane integration with one
writer per resource, immutable identity, durable exact ownership, conservative
withdrawal/recovery and separate packet-forwarding ownership. Existing FRR users
must retain behavior and deployment compatibility.

Actual current state: documented APIs/reconciliation couple to FRRConfiguration
and frr-k8s; EVPN-for-OVN's per-network Netlink/provider-bridge modes are not the
same as upstream SVD/VLAN topology. This is intentional current architecture,
not a confirmed defect or general lack of GoBGP protocol support.

## Validation and alternatives

Use a pinned upstream revision and minimal selected CUDN/VTEP configuration
before any compatibility claim. Compare Type-2 MAC-only/MAC-IP, scoped policy,
FDB/neighbor and withdrawal independently; Type-5/IP-VRF requires separate
implementation/acceptance. Verify no competing OVN/kernel/BGP writers.
FRR remains EVPN-for-OVN's external validation peer; this proposal does not
change its chosen GoBGP control plane.

Workaround/alternative: retain supported upstream FRR integration; keep the
framework external until lifecycle/device/identity contracts are proven.
There is no defect reproduction or workaround needed.

## Duplicate check / readiness

Targeted upstream issue/PR and roadmap duplicate review is **pending**.
Pin the inspected documentation/source, consult maintainers and produce a small
external-adapter experiment before proposing an ABI or implementation.
Not ready for a compatibility claim or bug submission; no issue number assigned.
