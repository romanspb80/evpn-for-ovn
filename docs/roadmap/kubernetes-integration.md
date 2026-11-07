# Kubernetes integration and Operator boundaries

## Current EvpnCore implementation

The only project-owned CRD is namespaced
`evpn.romanspb80.github.io/v1alpha1`, kind `EvpnCore`
(`evpncores`, short name `ecore`). Sources:
[types](../../deploy/operator/api/v1alpha1/evpncore_types.go),
[controller](../../deploy/operator/controllers/evpncore_controller.go),
[CRD/RBAC](../../deploy/operator/manifests/operator.yaml) and
[applied core manifest](../../deploy/operator/core-resources.yaml).

| Surface | Actual behavior |
|---|---|
| Reconciliation | YAML-driven rendering and three-way patches, workload watches and 30-second resync; unchanged resources/status skip writes. |
| Managed resources | Core Services/Deployments plus optional Node-Agent DaemonSet configured through spec; OVN/OVS is still single-replica. |
| k8gobgp | Independent CRDs, speaker, Service and BGPConfiguration; default provider `k8gobgp`, unsupported/standalone values rejected. |
| Image fields | `appImage`, `ovnImage`, `nodeAgent.image` are effective; deprecated `gobgpImage` remains unused by design. |
| Namespace fields | App/OVN resources and their management service dependencies relocate with spec. |
| Existing deployment env | Matching environment overrides remain a compatibility escape hatch; explicit namespace relocation takes precedence. |
| Readiness | Current-generation Deployment/DaemonSet rollout counts gate Ready; conditions distinguish reconciling, degraded, invalid configuration and deletion. No BGP/traffic health claim. |
| Lifecycle | Singleton installation Lease, UID-scoped child journal and cross-namespace finalizer cleanup; disable pauses rather than uninstalls. Namespaces/network state are retained. |
| Process/RBAC | Leader election enabled; health/metrics listeners remain disabled. Unneeded Gateway/Secret/StatefulSet permissions removed, but cross-namespace host privileges still need hardening. |

See the [operator architecture/API/lifecycle guide](../architecture/evpn-core-operator.md)
for the singleton/admission distinction, Node-Agent configuration, breaking API
group change, cleanup semantics and installation limits. Operator lifecycle has
its own E2E bootstrap; specialized dataplane suites retain their independent
evidence-specific runtime deployments. This is still **experimental**, not
production or multi-chassis acceptance.

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
