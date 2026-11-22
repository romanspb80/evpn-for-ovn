# Runtime dependencies and identity checks

Runtime images and tools are inputs to an isolated test, not proof of
interoperability or security. Use immutable identities and verify them before
starting a workload.

## Roles and authority

| Component | Role | Boundary |
|---|---|---|
| GoBGP | BGP sessions and EVPN RIB | Framework uses route APIs; an external provider remains the sole writer of managed peer configuration. |
| k8gobgp | Optional Kubernetes manager for a GoBGP speaker | Its local speaker/API identity must be verified; it is not the Framework route importer. |
| OVN/OVS | Network state and workload forwarding | Controller and OVS own the workload dataplane. |
| FRR | External test peer | Used only by bounded interoperability fixtures; not a replacement project control plane. |
| containerd `ctr` | Test/inspection input where a scenario requires it | Build or retrieve from an identified source and verify the binary hash before use. |

## Pinned provider input

The actual upstream is [purelb/k8gobgp](https://github.com/purelb/k8gobgp).
The selected v0.2.8 source is
[71b26cb8](https://github.com/purelb/k8gobgp/commit/71b26cb8ea5d48762c00f764e0204ce03e9b6e46);
its [module replacement](https://github.com/purelb/k8gobgp/blob/71b26cb8ea5d48762c00f764e0204ce03e9b6e46/go.mod)
uses [purelb/gobgp-netlink](https://github.com/purelb/gobgp-netlink)
v4.900.8 (user-facing v1.3.8 / daemon `PureLB-fork:01.03.08`, commit
`6b99046fdd0970fac50bc43f588640840798f4f9`). The current Framework Go
client is `osrg/gobgp/v4 v4.7.0`; neither it nor upstream `main` identifies
the selected fork's runtime. See [provider startup limitations](../known-issues/engineering-findings.md#pinned-provider-startup),
including the later stale-socket fix not contained in this pin.

The candidate example pins the k8gobgp image to:

```text
ghcr.io/purelb/k8gobgp@sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de
```

The expected image config ID required by the linux/amd64 fixture is:

```text
sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0
```

After an image archive is loaded, pull the exact digest reference from its
registry and verify both that RepoDigest and the expected config ID. Docker
save/load does not reliably preserve repository digest aliases. Do not
substitute a mutable tag, another digest, or a matching config ID alone. The
[VM E2E helper](../testing/vm-e2e-lab.md) enforces this check after image
transfer.

The Framework GoBGP client module version and the daemon version are separate
inputs. Likewise, an image label or source revision is not a signed build
attestation. Record the actual runtime image ID, architecture, source/build
identity and test result for each supported scenario.

## FRR fixture inputs

The [shared FRR acceptance helper](../../tools/lib/cni-evpn-frr-interop-test.sh)
defines `FRR_IMAGE` (digest-qualified image) and `FRR_IMAGE_ID` (expected
linux/amd64 config ID), verifies them before use, and captures read-only
inspection with `vtysh`: `show version`, `show bgp l2vpn evpn summary`, and
`show bgp l2vpn evpn route detail`. The
[Containerlab coordinator](../../tools/e2e/containerlab.py)
defines the peer image used when rendering its run-owned lab topology.

These are fixture inputs and diagnostics, not current release acceptance
evidence. Inspect the helper for maintained pins and commands; do not source
or execute it merely to read the constants. It is intended only for an
explicitly authorized disposable live test.

## Single-writer boundaries

In managed deployments, the provider owns global speaker identity, neighbors
and EVPN address-family configuration. The Framework manages only its
receipt-owned Type-2 paths. OVN/OVS owns workload forwarding; the selected
Netlink adapter manages only integration state that OVN/OVS consumes. Do not
enable competing provider Netlink writers or race legacy peer-management RPCs.

For the candidate, direct OVSDB route programming is maintenance/test-only.
See [network-state programming](integrations.md), [security requirements](../security/netlink-deployment-security-contract.md),
and [testing scope](../../TESTING_GUIDE.md).
