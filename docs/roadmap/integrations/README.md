# Future application and topology integrations

| Design | Current status | Canonical work |
|---|---|---|
| [OpenStack Neutron](openstack-neutron.md) | Proposed consumer; no adapter implemented | INT-01 |
| [DCI / multi-cluster](dci-multicluster.md) | Proposed routed/multi-site design; no acceptance | INT-02 |
| [OVN-Kubernetes](../upstream/ovn-kubernetes.md) | Upstream integration investigation, not a drop-in replacement | INT-03 |
| Standalone OVN platform integration | G.6/G.7 Node-Agent import/authorized export and single-chassis smoke implemented; complete lifecycle adapter/installer and multi-chassis migration unvalidated | [G.7b.5b](../backlog.md#g7b5b), [DEP-01](../backlog.md#dep-01) |

All task definitions are in the [backlog](../backlog.md). The Framework's
Go route and endpoint services can be reused without CNI,
Multus or Kubernetes. This is not a claim that the full Node-Agent recovery
path is platform-neutral: its CNI authority uses Kubernetes/containerd; explicit
standalone modes use independently provisioned speaker and authorization identity.
Kubernetes CRDs and reconciliation remain integration surfaces.
See the [verified architecture boundaries](../../architecture/framework-first.md).

## Standalone OVN design target

An adapter must supply administrator-owned network/endpoint identity, OVN
provenance, configuration and lifecycle without Pod/NAD/Multus assumptions.
It needs explicit ownership of existing versus newly created OVN objects,
exclusive route writers, independently verified speaker identity, durable
recovery domains, and install/update/delete/forwarding acceptance. Existing
direct service tests and the one-network/one-VTEP OVSDB importer are useful
starting points, not proof that this contract is implemented.

G.7b now implements protected endpoint policy, authoritative NB/SB intersection,
revision/generation fencing and lifecycle tombstones, consumed by the opt-in
standalone exporter. The [live smoke](../../../TESTING_GUIDE.md#standalone-export-smoke-g7b5a)
verifies authorization, policy revocation and agent restart without CNI on one
reused chassis. It does not implement a platform policy producer, supported
installer or live migration/forwarding acceptance.

The **EvpnCore standalone BGP provider deployment** is a separate future
configuration/installation owner; standalone process attestation already exists.
EvpnCore defaults to
`bgp.provider: k8gobgp` and rejects `standalone`. Any future provider must
preserve the separation of BGP identity/peer management from receipt-owned
EVPN route programming; no competing configuration writer is planned.
