# Kubernetes deployment and kind lab

The standard Kubernetes CNI architecture is an `EvpnCore` configuration
reconciled by the EVPN Operator into an Operator-managed Node-Agent DaemonSet.
The node-local agent hosts the EVPN Core runtime; the Operator does not process
routes or program endpoint state. The default mode selects one designated
node; experimental `external-ovn` mode selects multiple prepared nodes using
a Kubernetes label selector.
The Operator and installation are experimental, not a production installer.
In default mode the Operator also manages the OVN/OVS and compatibility
workloads; external-OVN mode manages only the agent. It does not install or
own upstream k8gobgp, Multus, or their CRs.
See [Operator ownership and configuration](evpn-core-operator.md).

Standalone framework operation outside Kubernetes does not require the
Operator. Future OVN-Kubernetes and Neutron integrations do not imply this
Operator or the current Node-Agent executable.

The checked-in kind deployment is a lab and integration environment, not a
production installer. Its source of truth is
[`deploy/kind/k8s.yaml`](../../deploy/kind/k8s.yaml), the Containerlab
topology, and the isolated E2E runners.

The [Kubernetes runtime diagram](../uml/evpn-runtime-architecture.svg) is a
logical view of the Operator, Node-Agent, optional Kubernetes integrations and
OVN/OVS datapaths. It is not a manifest inventory or acceptance topology. The
separate [attempt-8 diagram](../uml/operator-inventory-type2-two-worker.svg)
shows the exact scope of that two-worker Type-2 forwarding result.

The retained [runtime draft](../uml/drafts/kubernetes-runtime-draft.svg) is
superseded by the canonical runtime view; it is not acceptance evidence.

## Runtime components

| Component | Runtime responsibility |
|---|---|
| Upstream `k8gobgp` | Manager plus managed gobgpd in one upstream DaemonSet image; BGPConfiguration owns configuration. Accepted suites use the node-local Unix gRPC socket. |
| `evpn-mgmt` | EVPN management API in `evpn-system`; configured to use the GoBGP gRPC Service. |
| `ovsdb-mgmt` | OVN/OVSDB management API in `evpn-system`; connects to the `ovn-ovs` Northbound and OVSDB endpoints. |
| `bgp_ad_handler` | Separate route-handler service path in the checked-in manifest. It is not the standalone `cmd/evpn-ovsdb` Framework importer. |
| `ovn-ovs` | OVN databases, northd/controller, OVS, and the workload forwarding dataplane in `ovn-system`. |
| FRR | External EVPN peer in the Containerlab topology; used for route exchange and test validation. |

The manifest exposes management NodePorts `30052` and `30053`. Its k8gobgp
Service declares port `50051` with target `50054`, and compatibility service
clients are configured to use it. **That declaration does not install a
proxy or prove a working listener.** The upstream v0.2.8 managed runtime and
accepted runners use a Unix socket, not an assumed proxy sidecar. Applying
this manifest alone is not the accepted suite's runtime installation.

The standalone OVSDB route importer is an opt-in test/runtime component and is
not part of the default deployment. The Operator-managed Type-2 runner installs
the Operator and creates the agent via an `EvpnCore`; other disposable runners
create DaemonSets directly to isolate subsystems or exercise the multi-worker
Geneve baseline. The distinct inventory-gated two-worker Type-2 attempt-8
scenario uses the Operator; it does not establish general multi-node Operator
lifecycle or runtime health. Direct DaemonSet creation also serves development
and debugging, not a separate supported production installation mode. CNI,
Multus and the example NetworkAttachmentDefinitions are installed separately
by the fixtures; see
[`deploy/kind/cni-multus-test/`](../../deploy/kind/cni-multus-test/) and
the [management API limitations](../api/README.md).

The active external peer is pinned to official FRR 10.7.1 on Quay. Upstream
k8gobgp installation is pinned to the v0.2.8 release manifest, checksum and
image digest. OVN/OVS and the Framework GoBGP client module are unchanged.
See [runtime dependencies](runtime-dependencies.md) for immutable identities,
runtime verification commands, CRD/gRPC ownership and compatibility limits.

## Bootstrap and safety

All maintained E2E environments use the common
[Containerlab lifecycle](../../deploy/containerlab/README.md). Each runner
bootstraps its own profile, then deploys its suite-specific runtime and waits
for dependency readiness. The environment-only compatibility entrypoint is:

```bash
bash deploy/containerlab/deploy-kind-frr.sh
```

This wrapper no longer performs global cleanup or installs Kubernetes
resources. It refuses collisions and uses an exact topology/ownership receipt;
pass `down` to remove only that laboratory. Use a disposable Linux host.

For test-specific topology and cleanup boundaries, use the isolated
[Framework E2E runners](../../TESTING_GUIDE.md#framework-netlink-and-ovsdb-integration)
or [CNI lifecycle runner](../../TESTING_GUIDE.md#kubernetes-cni-lifecycle).
They invoke the same Containerlab coordinator themselves; do not pre-create
their environments. Containerlab provisions test laboratories, not Framework
runtime operations. User-managed Kubernetes and ordinary runtime deployment
remain independent of Containerlab.
