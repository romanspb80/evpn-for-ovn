# Kubernetes integration and Operator boundaries

## Current EvpnCore implementation

The only project-owned CRD is namespaced
`evpn.core.example.com/v1alpha1`, kind `EvpnCore`
(`evpncores`, short name `ecore`). Sources:
[types](../../deploy/operator/api/v1alpha1/evpncore_types.go),
[controller](../../deploy/operator/controllers/evpncore_controller.go),
[CRD/RBAC](../../deploy/operator/manifests/operator.yaml) and
[applied core manifest](../../deploy/operator/core-resources.yaml).

| Surface | Actual behavior |
|---|---|
| Reconciliation | Watches EvpnCore only, ensures app/OVN namespaces, reads the first available core manifest, creates/updates its objects and requeues after 30 seconds. |
| Managed resources | Core manifest has Namespaces, Services and Deployments for OVN/OVS, evpn-mgmt, ovsdb-mgmt and separate bgp-ad-handler; optional Netlink agent configuration is disabled by default. |
| k8gobgp | Manifest creates a Service selecting upstream pods. It does **not** install the upstream CRDs/DaemonSet or reconcile BGPConfiguration. |
| Image fields | `appImage`, `gobgpImage`, `ovnImage` exist in the CRD/type but are not consumed by the reconciler. Environment defaults in operator.yaml do not make those fields effective. |
| Namespace fields | Namespace creation uses spec/defaults, but explicit namespaces in YAML are preserved. Do not assume complete manifest relocation. |
| Existing deployment env | Matching environment overrides are preserved during apply; this is not a full declarative replacement of all runtime configuration. |
| Readiness | Sets `phase=Ready`, `ready=true`, reason `core resources reconciled` immediately after successful apply. No dependency/pod/BGP readiness gate, conditions or observedGeneration. |
| Lifecycle | No owner references, finalizer, child `Owns` watches or uninstall. `enabled=false` skips reconciliation; it does not delete the deployment. |
| Process/RBAC | Leader election and health/metrics listeners disabled in main. RBAC is broad; granted daemonset/Gateway permissions do not prove those resources are managed by the core manifest. |

This is **experimental scaffolding**, not the accepted suites' installer.
Effective image/configuration, readiness and deletion semantics are
[K8S-01](backlog.md#p1-reliability-and-contract-clarification).

## k8gobgp and application ownership

The inspected release manifest deploys upstream k8gobgp as a **DaemonSet**.
Its image runs both manager and managed `gobgpd`; the accepted suites use
the node-local Unix GoBGP gRPC socket. The relationship is:

`BGPConfiguration (configs.bgp.purelb.io) → k8gobgp reconciliation → managed gobgpd`.

The manager owns ASN/router ID/listener/peers/AF/VRFs. The Framework owns
its Type-2 AddPath/UUID DeletePath lifecycle and selected integration
objects; upstream Netlink import/export is disabled in these suites to
avoid competing writers. See the [ownership matrix](../architecture/runtime-dependencies.md).
`evpn-mgmt` compatibility speaker/peer operations must not race the manager
([FW-02](backlog.md#p1-reliability-and-contract-clarification)).

Multus invokes `evpn-cni`; the plugin translates ADD/CHECK/DEL and
Kubernetes attachment identity into the node-local
[NodeAgentCNI API](../api/node-agent.md). The node-agent also hosts reusable
Framework endpoint/attachment services and optional asynchronous Type-2
reconcilers. It is not wholly CNI-only. OVN northd/controller and OVS retain
logical-to-physical forwarding ownership. Neither CNI nor EvpnCore owns
upstream BGPConfiguration in the verified implementation.

## Proposed CRDs: not implemented

These are architectural proposals under
[K8S-02](backlog.md#p2-planned-capabilities-and-integrations), not new API
versions or accepted runtime features.

| Proposed kind | Candidate responsibilities | Boundary to resolve before implementation |
|---|---|---|
| EvpnGateway | Local ASN/router ID and BGP identity, peers, node association, RR roles, VTEP references, future multihoming | Translate to or reference k8gobgp-owned configuration; never install a second configuration writer. |
| EvpnVTEP | VTEP identity/addresses, node binding, managed/unmanaged modes, dataplane ownership and shared-VTEP references | Define address/device owner, shared reference lifecycle and conflict/deletion behavior per adapter. |
| EvpnNetwork | MAC-VRF/IP-VRF, VNI, RD/RT, L2/L3 topology, VTEP association and network lifecycle | Separate network intent from endpoint attachments, future Type-5 and ownership of existing OVN resources. |

Do not equate proposed project `EvpnVTEP` with OVN-Kubernetes's upstream
`VTEP` API. Kubernetes reconciliation is an integration layer, not a
dependency of the reusable Framework. Future
[Neutron](integrations/openstack-neutron.md) can consume domain services
without Multus, CNI or Kubernetes CRDs.
