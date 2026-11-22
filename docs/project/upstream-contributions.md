# Upstream Contributions

This page records accepted project-author work relevant to EVPN-for-OVN,
not upstream dependencies, proposed integrations or a personal achievements
inventory. Project compatibility observations and unsubmitted investigations
belong in [engineering findings](../known-issues/engineering-findings.md).
Upstream status was verified on **2026-11-21**.

## GoBGP–FRR EVPN interoperability example

Roman Ptitcyn (`romanspb80`) contributed
[osrg/gobgp#3891 — docs: add FRR EVPN DCI example](https://github.com/osrg/gobgp/pull/3891).
The maintainer accepted and incorporated the contribution into GoBGP
`master` through
[commit dd1d3659](https://github.com/osrg/gobgp/commit/dd1d3659ed1c08754a628b5d0303a7ecbcbb14f7).
The related
[osrg/gobgp#3890 — Add a direct GoBGP–FRR EVPN peering example for L2 DCI control-plane emulation](https://github.com/osrg/gobgp/issues/3890)
was closed as completed.

The example documents direct GoBGP/FRRouting iBGP EVPN peering, Type-2
MAC/IP advertisement, RD/RT/VNI/next-hop attributes and route-inspection
commands. It helps readers reproduce a control-plane interoperability
exercise relevant to the project's external-fabric goal.

The incorporated commit retains Roman's authorship and Signed-off-by;
FUJITA Tomonori committed the change, squashing the two submitted commits
and correcting the FRR service command to `systemctl restart frr`.
The [maintainer's confirmation](https://github.com/osrg/gobgp/pull/3891#issuecomment-6676878898)
and [author's commit reference](https://github.com/osrg/gobgp/pull/3891#issuecomment-6704691159)
provide additional provenance. The commit is present in upstream `master`
and its `docs/sources/evpn.md` history.

This is an accepted **documentation/interoperability contribution**, not a
new GoBGP protocol implementation. The PR reports control-plane checks using
GoBGP v4.11.0 and FRR 10.7.1; those are the contribution's reported inputs,
not this release's client/speaker identities or acceptance. VXLAN dataplane
configuration, MAC forwarding and end-to-end packets are explicitly outside
the example's scope. See the [local use-case guide](../use-cases/README.md#connect-ovn-workloads-to-an-external-evpnvxlan-fabric)
for the separate OVN/OVS forwarding and ownership requirements.

No other accepted project-author upstream contribution with an established
relationship to this release was recovered. OVN-Kubernetes EVPN proposals,
FRR reload reports and the local GoBGP/k8gobgp/OVN drafts are not presented
as accepted EVPN-for-OVN contributions.
