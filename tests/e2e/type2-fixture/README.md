# Shared EVPN Type-2 route fixture

This disposable fixture provides a real GoBGP speaker and a dedicated FRR
EVPN peer on a Docker-internal network. It is a control-plane route fixture,
not a dataplane E2E suite. It has no Kubernetes, CNI, node-agent, OVN, OVS, or
host-network dataplane dependency. Containerlab is its mandatory laboratory
provisioner, as for all maintained E2E environments.

## Requirements and safety

The first live run requires Docker Engine, Containerlab 0.79.0 with approved
privileges, OpenSSL, Python 3.11+, and
immutable GoBGP and FRR container image references. The images must include
`gobgpd`/`gobgp` and FRR's `vtysh` and `ip` commands. The helper
rejects image tags without a SHA-256 digest.

**Run this fixture only when you intend to create and operate a disposable
Docker environment.** `start` creates a Docker network and GoBGP/FRR
containers, and configures a BGP EVPN peer. `advertise` and `withdraw` change
EVPN routes in the fixture; `stop` removes the containers and network owned by
that run. The FRR container receives additional `NET_ADMIN` and `SYS_ADMIN`
capabilities, and the GoBGP gRPC API is reachable from the fixture's internal
Docker network. Do not run it on a shared or production Docker host, or with
images/configuration you do not trust.

The fixture does not publish host ports or use the host network.
It refuses to start if the generated subnet overlaps any existing Docker
network or if any reserved Containerlab resource already exists. Cleanup is restricted
to the exact resource IDs recorded in that run's ownership manifest; it does
not use wildcard removal or Docker prune.

The GoBGP service overrides the k8gobgp image entrypoint and starts only
`/usr/local/bin/gobgpd` with the generated TOML configuration. Its gRPC API
listens on TCP port 50051 on the run-owned internal Docker network; remote API
access is explicitly enabled for that isolated fixture network. The k8gobgp
manager is not started. The FRR service mounts a run-owned `/etc/frr/daemons`
file with `bgpd=yes` and a run-owned `/etc/frr/vtysh.conf` with integrated
configuration enabled; both are read-only mounts. This avoids relying on the
repository's top-level `frr/` directory and prevents vtysh's missing-config
warning. Startup waits for the bgpd process separately from the
BGP peer becoming established. A failure at either readiness gate is recorded
as startup failure. FRR runs with only the additional `NET_ADMIN` and
`SYS_ADMIN` capabilities declared by this fixture. The pinned FRR 8.4 image's
zebra and bgpd request `CAP_SYS_ADMIN` during privilege initialization; the
fixture adds it because the default Docker capability set omits it. This is a
broad Linux capability, so it is limited to the run-owned FRR container on the
internal fixture network. The fixture does not enable privileged mode or
disable Docker seccomp/AppArmor profiles. `NET_RAW` is not added because it
was already available in the observed default container capability set and is
not the capability missing from the failed startup.

Startup readiness checks each speaker's detailed neighbor output against the
expected peer address and exact established state. It then separately requires
the negotiated L2VPN EVPN family to be reported as both advertised and
received by GoBGP and FRR. A configured family or Established BGP session alone
does not pass the EVPN-family gate, and negotiated-family evidence does not
prove that any EVPN routes were exchanged; route presence remains a separate
mandatory check after advertisement.

The Framework/CNI suites now select official FRR 10.7.1 and managed k8gobgp
v0.2.8; see the [dependency audit](../../../docs/architecture/runtime-dependencies.md)
for their verified image digests. This standalone fixture still takes explicit
caller-supplied digest references and bypasses the k8gobgp manager. Its
historical FRR 8.4 captures and privilege observations above are not relabeled
as 10.7.1 evidence. Use the independent suite results in the
[Testing Guide](../../../TESTING_GUIDE.md), not this fixture's old captures,
for the upgraded Framework/CNI acceptance status.

## Start

```sh
python3 tools/e2e/evpn-type2-fixture.py start \
  --gobgp-image 'registry.example/gobgp@sha256:<64-lowercase-hex-digest>' \
  --frr-image 'registry.example/frr@sha256:<64-lowercase-hex-digest>'
```

The helper prints a UUID `run_id`. Save it for each subsequent command. Useful
parameters are `--gobgp-asn`, `--frr-asn`, `--gobgp-address`,
`--remote-vtep`, `--gobgp-router-id`, `--frr-router-id`, `--rd`, `--rt`, and
`--vni`. The BGP peer address and remote VTEP are distinct configurable
identities in the generated subnet. Defaults are GoBGP `.3`, FRR peer `.2`,
and remote VTEP `.4` within the generated `/24`; the run-owned FRR container
configures the VTEP address on its underlay interface. The default RD is the
GoBGP peer address plus the VNI (for example `198.18.0.3:1000`), and the
default RT is `64512:1000`. Both ASN defaults are 64512, matching the
historical GoBGP/FRR test configuration. The default subnet is selected from
RFC 2544 benchmarking space and checked against existing Docker networks.
Peer addresses and router IDs default from that subnet.

`start` writes a run-specific GoBGP TOML config, FRR config, ownership
`manifest.json`, `report.json`, and evidence directory under
`~/.local/state/evpn-for-ovn/e2e-runs/<run_id>` (or
`EVPN_E2E_STATE_ROOT`). The manifest records source commit, image digests,
parameters, Containerlab identity, owned container/network IDs, route identities,
and per-action status. `lab.clab.yaml` and `lab.json` retain the exact common
topology and ownership receipts. Containerlab deploys the two labeled speakers;
the common coordinator reserves their Docker-internal bridge because this
Containerlab release cannot directly create an internal management bridge.
The [common lifecycle guide](../../../deploy/containerlab/README.md) documents
locking, persistent leases, management CA verification and scoped recovery.
`runtime-versions.json` records actual speaker versions and image identities.
This management PKI does not enable Framework gRPC TLS.

## Bidirectional route lifecycle

The FRR peer uses `advertise-all-vni` and `advertise-svi-ip`. The fixture
creates one isolated Linux bridge and VNI-1000 VXLAN interface inside the FRR
container. The bridge is the VNI's SVI and receives a run-specific gateway
address on an endpoint subnet distinct from the BGP underlay subnet. The
FRR-originated `secondary` and `netlink-primary` endpoints use `ep2`/`sw2` and
`ep3`/`sw3`, respectively, and share a run-specific process-owned
network namespace created with `unshare --net`; it does not require the denied
`/var/run/netns` mount used by `ip netns add`. The namespace process PID is
recorded inside the run-owned FRR container and `nsenter` runs the endpoint
commands in that network namespace. The `sw2` and `sw3` veth peers remain
attached to the bridge on the default untagged VLAN (VLAN 1). Each
FRR-originated endpoint owns its manifest MAC/IP and shares the SVI subnet.
Route identities are unique per run, and each endpoint records its originator
and expected receiver in the manifest.
`primary` is injected into GoBGP and received by FRR. To originate `secondary`,
the fixture captures bounded ARP traffic on its endpoint, bridge-facing veth,
and `br-e2e` when `tcpdump` is available, then sends one bounded ping from the
endpoint to the bridge SVI. The same operation applies to `netlink-primary`.
PCAP files and per-capture command/status metadata are retained in the run
evidence directory. This generates real ARP
traffic instead of installing a static neighbor entry. The intended learning
sequence is endpoint reachability, bridge FDB learning, ARP neighbor learning
on the SVI, Zebra's EVPN MAC/IP association, FRR Type-2 advertisement, then
GoBGP receipt. Each step is subject to live verification; a successful ping or
kernel entry alone does not pass route acceptance. `advertise-svi-ip` concerns
the SVI address and is not treated as an instruction to advertise arbitrary
endpoint addresses. The FRR bridge uses a separate run-derived MAC, so its
local interface identity cannot collide with the GoBGP-originated primary
route.

FRR-originated endpoint withdrawal removes that endpoint's IPv4 address, its
learned SVI neighbor entry, and the bridge-facing veth attachment. This gives Zebra the
kernel neighbor/FDB changes needed to withdraw the local route. Re-advertisement
restores the address and bridge attachment, then triggers fresh endpoint ARP.
The fixture checks the same strict Type-2 route identity after each operation.
Cleanup terminates the run-owned namespace process, which destroys its
network namespace, and removes fixture-owned `sw2`, `sw3`, and bridge
interfaces. Failed
topology setup attempts the same rollback before preserving startup evidence.

```sh
python3 tools/e2e/evpn-type2-fixture.py advertise <run_id> primary
python3 tools/e2e/evpn-type2-fixture.py advertise <run_id> secondary
python3 tools/e2e/evpn-type2-fixture.py advertise <run_id> netlink-primary
python3 tools/e2e/evpn-type2-fixture.py observe <run_id>
python3 tools/e2e/evpn-type2-fixture.py withdraw <run_id> primary
python3 tools/e2e/evpn-type2-fixture.py observe <run_id>
python3 tools/e2e/evpn-type2-fixture.py advertise <run_id> primary
python3 tools/e2e/evpn-type2-fixture.py observe <run_id>
```

GoBGP originates `primary` with the pinned CLI's `global rib add -a evpn
macadv <MAC> <IP> etag 0 label <VNI> rd <RD> rt <RT> encap vxlan`; withdrawal
uses `global rib del` with the same route identity and attributes. FRR
originates `secondary` and `netlink-primary` through endpoint-generated ARP
and kernel bridge/SVI learning. Route observation checks the GoBGP RIB for all
three routes and uses FRR's exact query
`show bgp l2vpn evpn route rd <RD> mac <MAC> ip <IP> json` for FRR attributes.
The exact-route JSON captured from the pinned FRR 8.4 image exposes route-level
`VNI` and `vni`, route identity at the top level, RT in
`extendedCommunity.string`, next hop in `nexthops`, and peer provenance in
`peer.peerId`/`peer.routerId`. The received-route and locally originated
captured JSON samples are retained under `fixtures/` and drive parser
regressions. Both VNI spellings are accepted only when they
match and agree with the expected VNI. The FRR table query is used only to
confirm route absence/presence while polling; its output alone cannot pass the
attribute gate.

The GoBGP-originated primary must be a remote path from the expected GoBGP
peer at FRR; a local FRR path or peer ID `0.0.0.0` is rejected. The
Each FRR-originated endpoint must be present in GoBGP from the expected FRR
peer, and its FRR exact-route output must identify the local FRR path. The
parser does not treat `aspath.string: "Local"` as origin proof. For the GoBGP
primary and each FRR-originated route, the receiver checks exact MAC/IP, RD,
RT, route-level VNI/label, next hop, and peer/local provenance. GoBGP's own
primary RIB entry is checked against its
locally originated representation (including next hop `0.0.0.0`); the received
FRR route is independently checked for the advertised next hop.

The two-route lifecycle example is primary + secondary advertisement,
verification at both speakers, primary withdrawal, confirmation that primary
is absent from both route tables while secondary remains present with unchanged
attributes, then primary re-advertisement with the same identity and renewed
verification. Every JSON command parses stdout only, retains stderr in
evidence, and rejects failed commands, empty output, malformed JSON, and
unexpected schemas. Missing or unsupported fields fail explicitly. This
parser alone does not establish a passing live lifecycle; use the recorded
run report and source/runtime identities.

These are control-plane fixture routes, not proof of actual dataplane
forwarding. The independent OVSDB-Programmed and Netlink-Programmed E2E suites
add their own endpoint topology, traffic, packet captures and dataplane-state
assertions.

## Cleanup and evidence

```sh
python3 tools/e2e/evpn-type2-fixture.py stop <run_id>
```

Cleanup inspects Containerlab/run labels and ownership receipts, captures
bounded logs, and destroys only the exact manifest-owned topology and network.
It does not use wildcard container names, prune operations, or
broad cleanup. Run reports and evidence remain on disk after successful
cleanup. If startup or cleanup fails, the manifest and evidence are retained
for diagnosis; do not remove resources manually unless their ownership has
been verified.

`report.json` contains the run UUID, source commit, image digests, topology,
and per-action `PASS`/`FAIL` results with evidence paths. An action that was
not executed has no result entry and must be treated as `NOT RUN`.

## First live-run prerequisites

- Obtain immutable GoBGP and FRR image digests and confirm the required binaries
  are present.
- Confirm Containerlab 0.79.0, Docker Engine, OpenSSL and Python are installed.
- Confirm that Docker container/network creation and removal are acceptable
  on the selected disposable host.
- Confirm the generated isolated network does not overlap another Docker
  network (the helper performs this check).
- Verify GoBGP and FRR BGP EVPN session formation and both route advertisements
  before using this fixture in a dataplane suite.
- Preserve the generated run ID and evidence directory for the E2E report.
