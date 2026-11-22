# Engineering findings and unresolved upstream investigations

For maintainers and SDN engineers investigating compatibility, not a list
of confirmed upstream bugs. This restores discoverability of development
findings without promoting historical observations into current acceptance.
The original detailed registries and local **unsubmitted** drafts are retained
at revision `ae07988afdd08b860eb45221ed50c01e1995769f`.

This page preserves technical evidence relevant to project limitations,
not a general upstream dependency tracker. Current operational limits remain
in [troubleshooting](README.md).
The [historical validation index](../internal/validation-history/README.md)
explains evidence provenance and availability.

## GoBGP and k8gobgp boundaries

| Finding | Classification, impact and handling | Original evidence and next action |
|---|---|---|
| AddPath UUID versus ListPath readback | Selected API/runtime compatibility observation, not a current-stock defect attribution. Exact durable mutation receipts remain authoritative; matching RIB tuples cannot establish ownership after ambiguous AddPath or speaker replacement. | [API proposal](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/gobgp-type2-api-proposal.md); current [adapter](../../pkg/evpn/type2_gobgp.go) and [recovery contract](../architecture/type2-restart-ownership.md). Compare stock/fork APIs and restart behavior before filing. |
| MAC-only/absent-IP Type-2 representation | Historical typed apiutil serialization produced `invalid IP`; the current adapter retains binary NLRI with standard zero IP length. A valid MAC/IP control and full NLRI/RD/RT/VNI/next-hop roundtrip are separate checks. No synthetic IP or NLRI-wide deletion fallback is permitted. | Same [API proposal](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/gobgp-type2-api-proposal.md) and [adapter tests](../../pkg/evpn/type2_gobgp_test.go). Historical live client v4.5.0 is distinct from current v4.7.0 and the PureLB daemon; reproduce exact versions before attribution. |
| Declarative endpoint routes and provider lifecycle | Provider owns BGPConfiguration, peers, AFs/VRFs and its daemon; the Framework owns receipt-scoped Type-2 AddPath/DeletePath. VRF RD/RT fields do not constitute per-endpoint Type-2 intent or a supported ownership-transfer API. | [Historical k8gobgp investigation](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/roadmap/upstream/k8gobgp.md); [current runtime ownership](../architecture/runtime-dependencies.md). Research a single-writer API only if needed; no matching filed issue recovered. |
| Secret-backed peer authentication and rotation | Secret references already exist. Pinned-source exact-key lookup, missing Secret watches and redacted password comparison leave nonempty-to-nonempty Secret-only rotation without a validated application contract. No live rotation failure or hot-rotation workaround is claimed. | [Unsubmitted rotation proposal](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/k8gobgp-peer-auth-rotation-proposal.md). Test neighbor/peer-group updates, manager/daemon restart and wrong/missing-key controls before submission; never race the provider with direct peer mutation. |
| Remote security and fork Netlink behavior | Local attestation is not remote BGP authentication. General provider kernel route import/export is separate from EVPN FDB/neighbor ownership. Type-3/5, RR, MD5/AO/GTSM and rotation require exact runtime/schema and independent acceptance. | [Historical peer-security investigation](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/roadmap/upstream/bgp-peer-security.md) and [control-plane registry](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/known-issues/control-plane-issues.md). Keep competing Netlink writers disabled; investigate without expanding release scope. |

Pinned source citations were rechecked on **2026-11-21**:
the fork's [Path schema](https://github.com/purelb/gobgp-netlink/blob/6b99046fdd0970fac50bc43f588640840798f4f9/proto/api/gobgp.proto#L938-L941)
explicitly reserves the removed read-path `uuid` field, and its
[AddPath/DeletePath implementation](https://github.com/purelb/gobgp-netlink/blob/6b99046fdd0970fac50bc43f588640840798f4f9/pkg/server/server.go#L3083-L3193)
generates UUIDs on addition and uses an in-memory UUID map for withdrawal.
These explain the selected fork boundary, not a current-stock GoBGP bug.
The previously recorded upstream
[TCP-AO source](https://github.com/osrg/gobgp/blob/fb62e885293b8f8dcec13584e3a9156c25d31323/internal/pkg/netutils/tcp_ao.go)
contains key/configuration types and validation at that revision. Source
presence alone does not establish selected-fork, k8gobgp CRD, peer/kernel
compatibility, rotation support or local acceptance.

## Pinned-provider startup

The selected k8gobgp v0.2.8 input has an upstream-reported failure after an
unclean daemon stop with persistent Unix socket storage:
[purelb/k8gobgp#98](https://github.com/purelb/k8gobgp/issues/98).
The leftover socket blocks API binding while entrypoint file-existence
checks incorrectly report readiness. This is relevant to speaker lifecycle,
not a locally reproduced failure or a general loss-of-forwarding finding.

The later [purelb/k8gobgp#102](https://github.com/purelb/k8gobgp/pull/102)
was merged on 2026-11-07, adding stale-socket startup cleanup, live API
readiness and daemon-exit checks alongside dependency upgrades. It is **not
in the selected v0.2.8 input**. The upstream report distinguishes a manifest
wedge from crash-looping probes; Pod replacement with fresh emptyDir was
reported to recover it, but is not a validated workaround for this project's
host-mounted socket fixtures. Qualify exact storage, process identity,
attestation and recovery before adopting a new image. Do not unlink a
potentially live provider socket to force readiness.

## Future FRR-managed integration boundary

The reported empty-VRF reload problem in
[metallb/frr-k8s#417](https://github.com/metallb/frr-k8s/issues/417) links
[FRRouting/frr#21245](https://github.com/FRRouting/frr/issues/21245).
The former was closed as not planned, not fixed; the latter remained open
when verified on 2026-11-21. The proposed
[FRRouting/frr#23442](https://github.com/FRRouting/frr/pull/23442)
was closed without incorporation.

The report's [pinned reloader source](https://github.com/FRRouting/frr/blob/e119526f953c05ed4d54be3836b96cd5e5b412df/tools/frr-reload.py#L1808-L1810)
retains empty VRF stanzas. This concerns future FRR/frr-k8s-managed integration,
not a confirmed failure of the selected GoBGP runtime or the project's FRR
test fixture. Manual `no vrf` worked for the reporter, not as a project-tested
cleanup recipe; do not mutate external controller-owned VRFs.

## OVN/OVS findings

The [original OVN/OVS registry](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/known-issues/ovn-ovs-issues.md)
retains complete versions, evidence, expected/actual behavior, regression
references, hypotheses and dispositions:

| Finding | Classification and historical evidence | Current implication / next action |
|---|---|---|
| northd dynamic routing with `addresses=unknown` | OVN 26.09.0 repeated SIGSEGV and invalid-UTF-8 transaction error, reproduced without CNI. Root-cause analysis incomplete; not a confirmed source-level upstream defect. | Preserve the recorded test workaround: valid MAC-only sentinel on a separate non-dynamic switch. It is not an OVN fix or general compatibility guarantee. Obtain minimized build/core and cross-version evidence. |
| Transient SB Port_Binding deletion reference error | One remaining reference prevented a transaction; later recomputation removed the old binding and replacement converged. Referring row/table and independent trigger are unknown. | Do not conflate this with the crash or mutate northd-owned SB rows. Capture complete referring state and transaction sequence before filing. |
| Asynchronous localnet patch-port cleanup | Expected controller behavior: initial provider-bridge removal safely refused while patch attachment remained; later reconciliation succeeded. | Retain ownership checks and bounded retries. Do not force-delete controller patch ports or promise teardown latency. |
| Learned FDB after partial route withdrawal | Local OVSDB-Programmed ownership limitation, not an OVS defect: no validated per-MAC learned-entry invalidation; final tunnel removal does not certify partial-withdrawal timing or peer-cache cleanup. | Maintenance/testing-only path. Measure survivors and cache lifetime; research ownership-safe invalidation, not broad bridge/FDB flushes. |
| Superseded schema mismatch and interface-MAC failures | Historical harness findings: raw JSON formatting comparison falsely reported mismatch; correcting test-interface/LSP MAC agreement restored traffic. | Preserve dispositions; do not present them as active OVN bugs. Capture/AppArmor, CNI_ARGS and evidence-ordering failures likewise remain harness/environment findings. |

The [dedicated crash record](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/known-issues/ovn-northd-dynamic-routing-unknown-crash.md),
[OVN-only reproduction](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/ovn-port-binding-reproducer.md)
and [unsubmitted draft](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/ovn-port-binding-upstream-draft.md)
remain unchanged in history. No current reproduction, matching upstream
issue/fix or source-level fault was established by this documentation review.

## Integration research and unrecovered references

The [OVN-Kubernetes GoBGP seam proposal](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/issues/ovn-kubernetes-gobgp-provider-proposal.md)
is an unsubmitted architecture investigation, not a defect or accepted adapter.
It proposes external integration first, then a reviewed control-plane seam
only if necessary, without taking over OVN-Kubernetes's OVN/kernel/FRR state.
FRR/frr-k8s-specific APIs and SVD/VLAN topology are not interchangeable with
this project's per-network Netlink path.

No exact public issue was recovered for the historical UUID-readback,
absent-IP encoding, declarative Type-2 ownership, pinned Secret-rotation,
northd `unknown` crash or transient SB-reference investigations. Searches
are not proof that no related report exists. The GoBGP FRR documentation
contribution is unrelated to those API questions.

Independent OVN-domain DCI, generalized multi-chassis/multi-VTEP lifecycle,
mobility and missed CNI DEL/API-outage acceptance remain **local incomplete
capabilities**, not presumed upstream bugs. The
[roadmap](../roadmap/README.md) remains their future-work home.

The complete historical research inventories remain available:
[GoBGP](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/roadmap/upstream/gobgp.md),
[OVN-Kubernetes](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/roadmap/upstream/ovn-kubernetes.md)
and [OVN/OVS](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/roadmap/upstream/ovn-ovs.md),
alongside the k8gobgp and peer-security records linked above.
Their contents and direct source URLs are preserved at the pinned revision;
they are research/evidence, not duplicate current issue-status inventories.
