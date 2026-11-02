# OVN-Kubernetes: GoBGP integration investigation

**Classification:** research question / proposed upstream improvement,
tracked once as [INT-03](../backlog.md#p2-planned-capabilities-and-integrations).
No implemented adapter, compatible plugin contract or upstream issue/PR is
claimed.

The inspected upstream [EVPN feature documentation](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/bgp-integration/evpn.md)
describes P-CUDN EVPN transport, a cluster-scoped `VTEP`, MAC-VRF and IP-VRF
settings, `RouteAdvertisements`, and FRR/frr-k8s `FRRConfiguration`.
Its [route advertisements documentation](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/bgp-integration/route-advertisements.md)
also exposes FRR-specific selectors. These are moving upstream references,
not the API version supported by this Framework's accepted lab.

Upstream MAC-VRF stretches L2; IP-VRF routes L3, including Type-5 for
Layer-3 networks. Its Single VXLAN Device model shares a bridge/VXLAN per
VTEP and maps VLANs to VNIs. The inspected page labels managed VTEP mode
unimplemented there. None of these upstream statements establishes local
Framework Type-5, SVD, managed-VTEP or CUDN support.

| Approach | Required investigation |
|---|---|
| External Framework integration first | Observe selected network intent without assuming ownership of ovnkube-node's devices, OVN rows, routes or FRR configuration. Define node/VTEP, RD/RT and endpoint provenance mapping. |
| Proposed upstream control-plane seam | Identify FRR-specific reconciliation/API coupling; define a provider-neutral desired-state and capability contract before mapping to k8gobgp/GoBGP. This requires upstream design, not just changing an image. |
| Type-2 and Type-5 | Compare MAC-only/MAC-IP, FDB/neighbor, VRF/prefix and withdrawal semantics independently; keep one writer per kernel and BGP object. |
| DCI | Define tenant/RT isolation, route leaking, RR and failure domains; depend on separate Type-5 and multi-site acceptance. |

The Framework's per-network Netlink integration and optional provider-bridge
OVSDB path are not equivalent to upstream SVD/VLAN topology. Coordinate
[FW-10/FW-11](../backlog.md#p2-planned-capabilities-and-integrations) before
claiming compatibility. Keep FRR as the Framework's external validation peer;
do not reinterpret the upstream use of FRR as this project's chosen runtime.
