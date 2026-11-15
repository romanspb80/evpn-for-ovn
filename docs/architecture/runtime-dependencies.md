# Runtime dependencies and configuration ownership

The C4.4f.2q.1 dependency change targets FRR 10.7.1 and upstream k8gobgp
v0.2.8. Live acceptance status is recorded separately in the
[Testing Guide](../../TESTING_GUIDE.md). An image pull or version pin is not
acceptance evidence.

## Reproducibility boundary

The [development checks](../../TESTING_GUIDE.md#development-checks) build
original Go source from `go.mod` and `go.sum`; they require no private image or
acceptance bundle. This is the safe source-validation baseline.

The live runners additionally require local OVN and test-base images with
specific **image config IDs**, not just matching tags. These IDs establish what
was tested, not a public registry pull location or reproducible image recipe.
In particular, `deploy/kind/Dockerfile.ovn-ovs` uses mutable `debian:sid-slim`
and unversioned apt packages; rebuilding it today does not promise the accepted
OVN/OVS image ID. Test-base images likewise must be supplied and verified before
running the harness. Do not silently substitute images or remove its ID checks.

Before public push, clean-candidate reproduction must establish publicly
obtainable, licensed runtime inputs with verified provenance and either the
accepted immutable identities or explicitly approved new acceptance. That gate
is [REL-04](../roadmap/backlog.md), not a claim completed by private-source tests.
No image build/pull or live provisioning was performed during the final
source hardening review. The framework client dependencies were upgraded;
this does not refresh the historical live acceptance evidence.

## Inventory and immutable identities

| Component | Before this change | Current configuration and executable identity | Used by |
|---|---|---|---|
| FRR | Shared Containerlab topology used mutable `frrouting/frr:latest`; isolated suites pinned the Docker Hub 8.4 image and actually reported `8.4_git`. | Official Quay 10.7.1 image; executable reports `10.7.1_git`. | External Containerlab peer and all three isolated Framework/CNI suites. |
| k8gobgp | Shared bootstrap installed `releases/latest`; optional operator scaffolding named 0.2.5. Isolated suites already pinned the v0.2.8 release manifest and image. | Every active installation/example uses v0.2.8 and the verified digest below. | Kubernetes manager plus GoBGP speaker, including all three isolated suites. |
| GoBGP runtime | k8gobgp's PureLB fork, not a separately installed stock daemon. | `PureLB-fork:01.03.08`, commit `6b99046`, base `gobgp-4.9.0`. | Managed speaker's gRPC RIB and BGP sessions. |
| Framework GoBGP client | `github.com/osrg/gobgp/v4 v4.5.0`. | `v4.7.0`; module/API client version is not the daemon version. | Framework Type-2 import/export and management APIs. |
| OVN/OVS | `evpn4ovn/ovn-ovs:kind-26.09`: OVN 26.09.0, OVS 4.0.0. | Unchanged; NB schema 7.19.0, SB schema 21.13.0. | Workload forwarding and database ownership. |

FRR image:

```text
quay.io/frrouting/frr:10.7.1@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2
linux/amd64 manifest: sha256:9aeb1c4947207a3d405674c4de06451d8b7e0d00e6f7d1f8e61e7354273efc63
linux/amd64 config:   sha256:27be65e6d6382c092d5ec87105eb8d38bee9e257e21963289623b24ceb3c99cd
```

The [official Quay tag metadata](https://quay.io/api/v1/repository/frrouting/frr/tag/?specificTag=10.7.1)
and manifest list were checked. It includes Linux amd64, arm64, ppc64le,
s390x, arm/v7, arm/v6 and riscv64. Acceptance runners intentionally require
the inspected **linux/amd64** image config. Other architectures are not
accepted by those runners or validated by these tests. The official image
prints the `_git` suffix; this is retained in evidence, not normalized away.

k8gobgp image and installation:

```text
ghcr.io/purelb/k8gobgp@sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de
Previously recorded linux/amd64 manifest: sha256:85901e9ac4ef89508f712b3aec625d13e54005f570063c76c51848d3247afe92
linux/amd64 config:   sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0
install.yaml SHA256:  f28c6fe7e45b5e535524de09fd13986c6956e112903c960e14f5b1cdeae91008
OCI source revision: 71b26cb8ea5d48762c00f764e0204ce03e9b6e46
```

Bootstrap downloads the [v0.2.8 release manifest](https://github.com/purelb/k8gobgp/releases/download/v0.2.8/install.yaml),
checks its checksum, and substitutes the pinned image before applying it.
The release tag resolves to the same revision as the image's OCI label.
The final read-only registry check verified the pinned `55da7e8c...` digest
as a Linux/amd64 OCI image manifest, not a multi-platform index, and verified
its `5210e185...` config digest. Its labels identify k8gobgp 0.2.8,
source revision `71b26cb8...` and gobgpd revision `6b99046...`.
The previously recorded `85901e9a...` manifest above is retained as historical
inventory, not asserted to be a child of the current pin.
This is metadata/source correspondence, not signed provenance or verification
of the embedded binaries. No filesystem layers were downloaded.
The selected source declares gRPC 1.83.2 and x/net 0.58.0; the separate
gobgpd source declares `PureLB-fork:01.03.08 [base: gobgp-4.9.0]`.
Actual binary build information, image signatures, OS-package advisories and
live compatibility with the framework's v4.7.0 client remain acceptance gates.
There is **one DaemonSet image containing both manager and gobgpd**, not
separate operator and speaker images. Its entrypoint runs the manager and
daemon using a local Unix gRPC socket. The maintained suites use that managed
Unix socket directly. The Containerlab environment-only bootstrap does not
install Framework services or add a separate TCP/proxy deployment path.

The EVPNCore operator's image fields/environment defaults are aligned
with this pin, but its current controller does not install upstream k8gobgp
from those fields. They must not be presented as an effective installation
mechanism.

## BGPConfiguration versus direct gRPC

Audited upstream v0.2.8 sources:
[API types](https://github.com/purelb/k8gobgp/blob/v0.2.8/api/v1/types.go),
[controller](https://github.com/purelb/k8gobgp/blob/v0.2.8/controllers/bgpconfiguration_controller.go),
[entrypoint](https://github.com/purelb/k8gobgp/blob/v0.2.8/entrypoint.sh)
and [module dependencies](https://github.com/purelb/k8gobgp/blob/v0.2.8/go.mod).
The provider uses `gobgp-netlink/v4 v4.900.8`; this does not change the
Framework client dependency or make every EVPN Netlink feature available.

Both `configs.bgp.purelb.io` and `bgpnodestatuses.bgp.purelb.io` expose
served/storage `bgp.purelb.io/v1`. BGPConfiguration supports global identity,
neighbors, peer groups, dynamic neighbors, policies/defined sets, VRFs, and
Netlink import/export configuration. `l2vpn-evpn` is an allowed family.
VRF configuration includes RD/import RT/export RT, but is **not** a
declarative collection of per-endpoint EVPN Type-2 MAC/IP/VNI paths.

| State or operation | Authoritative writer in the accepted suites |
|---|---|
| Local ASN, router ID, BGP listener and neighbor configuration | k8gobgp manager reconciling one BGPConfiguration. |
| Global and neighbor EVPN AFI/SAFI | BGPConfiguration and manager; not a second Framework `AddPeer` writer. |
| Local Type-2 MAC/IP advertisement, path RD/RT/VNI/next hop, exact withdrawal | Framework exporter through GoBGP `AddPath`/UUID `DeletePath`. |
| Received RIB and route filtering/import | GoBGP receives routes; Framework uses read-only `ListPath` snapshots and validates network/RT/VNI/VTEP. |
| Network-to-VNI mapping and owned integration objects | Framework network configuration and the selected Netlink or OVSDB adapter. |
| Linux FDB/neighbor integration in Netlink mode | Ownership-aware Framework adapter; upstream `netlinkImport` and `netlinkExport` are not enabled. |
| Explicit provider bridge/localnet/OVS VXLAN state in OVSDB mode | Opt-in Framework OVSDB adapter, scoped by ownership. |
| OVN bindings and workload OVS forwarding | OVN northd/controller; not the route importer. |
| Remote validation routes | External FRR peer; never the primary Framework control plane. |

The manager reconciles neighbors and can delete peers absent from desired
configuration. Identity changes can require a pod restart; direct
`StartBgp`/`AddPeer` calls must not race that reconciler. The legacy
`evpn-mgmt` API exposes those operations, but the acceptance suites do not
use them to configure the managed speaker. Deployments must keep the same
single-writer boundary. No architecture redesign or API removal is implied.
The provider does have general connected-route Netlink import and kernel
route export; disabling those writers here is deliberate ownership isolation,
not a claim that upstream lacks Netlink support.
The framework's own `ovn-netlink` local Type-2 observer is separate from those
provider writers: it reads OVN-redistributed Linux FDB/neighbor state with
NB/SB provenance and ownership checks. See the
[detailed Netlink import/export paths](integrations.md#netlink-programmed-type-2-internals).

## Embedded exporter composition

The exporter-composition change affects code structure, not dependency versions
or deployment topology.
`CNIServer.RunEndpointExporter` supplies `cniExportBinding`;
`RunStandaloneEndpointExporter` supplies `standaloneExportBinding`. Both use
the same private `runEndpointExporter` and existing `evpn.EndpointObserver`
and `Type2RouteApplier` contracts.

```text
Node-Agent entry point
  -> CNI binding: CNIServer + CNI/Framework/native observers + continuity guards
  -> standalone binding: protected policy + durable fencer + NB/SB intersection
       -> shared exporter loop
            -> actual attested GoBGP Unix transport + Type-2 recovery
```

Bindings select observers, prepare platform authority, update cycle inputs and
expose status observations. Shared runtime owns the database/transport/native
observer lifetimes; standalone runtime separately owns its authorization fence.
The CNI check-and-Add critical section is unchanged. No caller-provided identity
replaces actual peer/process, workload/CRI or OVN verification.

Direct package edges remain `cmd/evpn-node-agent -> pkg/nodeagent`,
`pkg/nodeagent -> pkg/evpn`, `pkg/nodeagent -> Kubernetes/CRI`, and
`pkg/evpn -> GoBGP/libovsdb/Netlink/ownership/domain`. Only `pkg/evpn/domain`
is standard-library-only. Standalone runtime avoids Kubernetes initialization;
the shared binary is not Kubernetes-free at compile time. Physical adapter
package extraction and future platform integrations are deferred.

See the [composition boundary](framework-architecture.md#export-runtime-composition-boundary)
and [acceptance evidence](../archive/validation-record.md#framework-fixture-and-exporter-regressions).

## Verification and limits

Each suite records pod/DaemonSet readiness, image and runtime image ID, installed
CRD, BGPConfiguration, process list showing **manager and gobgpd**, daemon
version, startup logs, and a real `gobgp global` gRPC response.
`provider-runtime.json` is written only after these checks pass. Existing
route/forwarding/withdrawal/survivor assertions remain mandatory.
Kubernetes compatibility is bounded to the suites' kind v1.32.3 node image;
this is not an upstream support-policy claim for every Kubernetes release.

To inspect an existing deployment without changing configuration:

```bash
kubectl -n k8gobgp-system get ds k8gobgp -o wide
kubectl get crd configs.bgp.purelb.io bgpnodestatuses.bgp.purelb.io
kubectl -n k8gobgp-system get pods -l app.kubernetes.io/name=k8gobgp
# Substitute the observed pod/container names:
kubectl -n k8gobgp-system exec <pod> -c k8gobgp -- gobgpd --version
kubectl -n k8gobgp-system exec <pod> -c k8gobgp -- gobgp global
kubectl -n k8gobgp-system exec <pod> -c k8gobgp -- gobgp neighbor
kubectl -n k8gobgp-system exec <pod> -c k8gobgp -- gobgp global rib -a evpn
docker exec <owned-frr-container> vtysh -c 'show version'
docker exec <owned-frr-container> vtysh -c 'show bgp l2vpn evpn summary'
docker exec <owned-frr-container> vtysh -c 'show bgp l2vpn evpn route'
```

The FRR release image needs explicit `frr.conf` and `vtysh.conf`; the common
Containerlab lifecycle mounts run-scoped configuration for every suite.
Existing iBGP, EVPN AF, VNI/RD/RT and
advertisement commands are retained. Route-reflector behavior, Type-5,
two-chassis GoBGP-to-GoBGP and shared-VTEP are not established by these suites.
The standalone Type-2 fixture deliberately bypasses the k8gobgp manager;
its results must never be reported as managed k8gobgp acceptance.

FRR 10.7 changes detailed EVPN output from `Advertised to non peer-group
peers:` to `Advertised to peers:`. The CNI route-scope assertion recognizes
both headings while still requiring separate exact MAC-only and MAC/IP
prefixes, an available path and the expected advertised peer. Regression
tests reject a matching next hop without advertisement, a different peer and
a missing MAC-only route. The first upgraded CNI attempt demonstrated this
parser incompatibility; replay of its unchanged evidence passes the corrected
assertion. Final acceptance must use fresh complete runs after that fix.
