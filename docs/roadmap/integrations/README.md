# Future application and topology integrations

| Design | Current status | Canonical work |
|---|---|---|
| [OpenStack Neutron](openstack-neutron.md) | Proposed consumer; no adapter implemented | INT-01 |
| [DCI / multi-cluster](dci-multicluster.md) | Proposed routed/multi-site design; no acceptance | INT-02 |
| [OVN-Kubernetes](../upstream/ovn-kubernetes.md) | Upstream integration investigation, not a drop-in replacement | INT-03 |
| Standalone OVN platform integration | Design target; direct Go managers, fixtures and `evpn-ovsdb` exist, but no supported platform adapter/installer | DEP-01 plus framework lifecycle contracts |

All task definitions are in the [backlog](../backlog.md). The Framework's
Go route and endpoint services can be reused without CNI,
Multus or Kubernetes. This is not a claim that the full Node-Agent recovery
path is platform-neutral: its current authority uses Kubernetes/containerd.
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

The **standalone BGP provider** is a separate future configuration/deployment
owner for GoBGP; it is not the standalone OVN adapter. EvpnCore defaults to
`bgp.provider: k8gobgp` and rejects `standalone`. Any future provider must
preserve the separation of BGP identity/peer management from receipt-owned
EVPN route programming; no competing configuration writer is planned.
