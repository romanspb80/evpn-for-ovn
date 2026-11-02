# Standalone Linux Netlink Component Acceptance

This scenario is **Standalone Linux Netlink Component Acceptance**. It
exercises `services/bgp_ad_handler` with `BGP_AD_HANDLER_MODE=netlink`; it is
not full Netlink-Programmed OVN/OVS E2E acceptance. It does not use CNI,
node-agent or OVN Framework E2E runner. It requires the already-owned GoBGP/FRR Type-2 fixture from
`tests/e2e/type2-fixture/`; this suite does not duplicate that fixture's peer
configuration, route observation, or route-withdrawal operations.

For the intended OVN integration, see the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration).
Linux bridge, VXLAN, FDB, and neighbor state are kernel integration state for
GoBGP and `ovn-controller`, not a replacement workload forwarding dataplane.
Full E2E evidence requires real OVN logical ports, `ovn-controller`, OVS tunnel
ports, OpenFlow/BUM programming, and bidirectional workload forwarding. A
PASS from this standalone component suite must never be reported as an
OVN/OVS dataplane PASS.

## Proposed authorized live run

First start the shared fixture with immutable GoBGP and FRR image references,
then use its printed run ID for this command:

```sh
python3 tools/e2e/evpn-type2-fixture.py start \
  --gobgp-image 'registry.example/gobgp@sha256:<64-lowercase-hex-digest>' \
  --frr-image 'registry.example/frr@sha256:<64-lowercase-hex-digest>'

python3 tools/e2e/netlink-acceptance.py \
  --authorize-disposable I_OWN_THIS_DISPOSABLE_ENVIRONMENT \
  --run-id '<fixture-run-uuid>' \
  --go-build-image 'registry.example/golang@sha256:<64-lowercase-hex-digest>' \
  --runtime-image 'registry.example/debian@sha256:<64-lowercase-hex-digest>'
```

The acceptance command is the only supported way to start this scenario. It
builds the current handler and a test-only GoBGP route helper, then creates the
handler on the fixture's run-owned Docker-internal network. It uses no host
network, host namespace or shared deployment resource. The fixture environment
is provisioned only through the common Containerlab lifecycle; helper containers
are registered by exact ID and matching lab/run labels with that owner. Handler
network administration is limited to `NET_ADMIN` and `NET_RAW` inside that
disposable container. It uses run-specific interface names and rejects an
unowned fixture network or stale source commit.

The shared fixture provides two veth-backed FRR-originated endpoints,
`secondary` and `netlink-primary`, behind the same VTEP; the runner uses those
for remote Netlink FDB/neighbor, component forwarding, and shared-VTEP
lifecycle checks.
The existing GoBGP-originated `primary` remains a separate control-plane
interoperability route. The runner uses fixture actions to advertise, observe,
withdraw, and re-advertise remote Type-2 routes. The test-only route helper uses the existing
`evpn.GoBGPType2Adapter` for one local endpoint route. Its returned GoBGP UUID
is persisted and is required for exact withdrawal. Remote and local route
identities, route targets, VNI, and VTEPs are stored in the per-run report.
The local route MAC is allocated from a named run-specific endpoint role and
is checked against the fixture bridge, every fixture endpoint, and the local
Netlink bridge before route advertisement. The endpoint MAC/IP are configured
on a dedicated veth endpoint attached to the Netlink bridge; the bridge keeps
its own distinct locally administered unicast MAC.
The runner allocates separate local-VTEP and route-helper addresses from the
fixture subnet after excluding the GoBGP peer, FRR peer, remote VTEP, gateway,
and advertised endpoint addresses; it does not reuse the FRR remote-VTEP IP.
The runner also writes `netlink-ownership.json`, which records the fixture
resources it relies on, every handler/helper/image it creates, and the final
owner-label inventory. It refuses to overwrite a prior acceptance report or
evidence directory.

The supplied immutable runtime base image must already include `ip`, `bridge`,
`ping`, `tcpdump`, and `timeout`. The build does not use a package manager, so
the image identity and its diagnostic tools remain fixed by the supplied digest.

## Component gates and regression behavior

The mandatory gates require exact Type-2 route evidence (MAC, IP, VNI, RD, RT,
and next hop) at both peers, handler
route-consumption logs, VXLAN VNI/UDP port, per-MAC FDB and IP/MAC neighbor
entries, shared VTEP neighbor and flood FDB, traffic in both directions, and
simultaneous UDP/4789 plus inner endpoint packet captures. The first withdrawal must remove only that MAC/IP state while retaining the
second route's state and forwarding. The runner then re-advertises the first
route and requires both endpoints' route, kernel-state, and bidirectional
Linux-component forwarding checks to recover. A failure of shared-state
preservation is recorded as a component regression; the runner does not repair
the state. These gates do not establish that `ovn-controller` consumed the
state or that OVS forwarded workload traffic.
Finally, it withdraws the second route and records that the re-advertised first
route remains valid until ownership-verified cleanup.

Each forwarding probe writes two Ethernet PCAPs under the run's
`captures/pcap/` directory: the handler's `eth0` VXLAN capture and the managed
bridge capture. Captures use explicit BPF filters and stop after the bounded
ping probe or 256 captured packets. Per-probe JSON records the interface,
container network-namespace identity, filter, start/completion timestamps,
process and stop exit codes, packet count, file size, SHA-256, decoded ARP/ICMP
records, and expected-packet matches. The decoder checks both VXLAN directions,
UDP/4789, VNI, inner endpoint MAC/IP attribution, and ICMP request/reply
traffic. Capture startup, stop, decode, or assertion failures fail the
forwarding-evidence gate; a successful ping alone cannot make that gate pass.
ARP packets are retained and counted as evidence, but this test does not assert
ARP suppression.

`netlink-report.json` and evidence are retained in the fixture run directory.
`netlink-ownership.json` retains the exact created-resource IDs and cleanup
states. During the local endpoint route gate, `evidence/route-diagnostics/local-export-import.json`
records the exact GoBGP Local RIB and Adj-RIB-Out, FRR received/accepted-route
command results, exact FRR route JSON, and GoBGP/FRR logs. An unsupported FRR
pre-policy received-routes command is recorded as unavailable; the test does
not enable soft-reconfiguration or change FRR policy.
Each gate is `PASS`, `FAIL`, `SKIP`, or `NOT RUN`; the required gates are all
mandatory, so `SKIP`, `NOT RUN`, a timeout, or missing evidence makes the overall
result `FAIL`. Cleanup withdraws the local route by its receipt UUID, removes
the exact owned handler/helper containers, withdraws remaining remote routes,
stops the shared fixture, and verifies that the run's owner-label inventory is
empty. Evidence remains on disk after cleanup.
The forwarding gates require the captured outer UDP/4789 packet to contain both
the expected local and remote VTEP addresses, and the inner ICMP capture to
contain both endpoint IPs.

To find the first failure, inspect `netlink-report.json` and locate the first
gate whose status is `FAIL`; later mandatory gates will be `NOT RUN`. For local
route export/import failures, use the route-diagnostics JSON alongside the
gate evidence and `topology/local-endpoint-interface.json`.

## Offline validation and live blockers

Offline checks may validate Python syntax, helper assertions, Dockerfile
structure, documentation links, and whitespace only. They must not invoke this
runner or the shared fixture's Docker commands. No container, BGP session, or
network namespace is started during scaffold validation.

The first authorized live run must confirm that the pinned FRR release
originates both expected Type-2 routes, that its Linux bridge/veth endpoint
responds through the standalone Linux component path, and that GoBGP propagates the
test-originated local route back to FRR. The current Netlink handler removes
VTEP-wide neighbor/flood state on each individual Type-2 withdrawal; the
multi-MAC sibling-preservation gate is intentionally expected to expose this
behavior if it occurs. Image digests, Docker Engine/Containerlab availability, and
explicit disposable-environment authorization are also prerequisites.

This fixture does not include OVN logical ports or `ovn-controller`-programmed
OVS forwarding. Full Netlink-Programmed E2E requires a separate OVN-integrated
test with actual workload traffic and OVS forwarding evidence.
