# Future application and topology integrations

| Design | Current status | Canonical work |
|---|---|---|
| [OpenStack Neutron](openstack-neutron.md) | Proposed consumer; no adapter implemented | INT-01 |
| [DCI / multi-cluster](dci-multicluster.md) | Proposed routed/multi-site design; no acceptance | INT-02 |
| [OVN-Kubernetes](../upstream/ovn-kubernetes.md) | Upstream integration investigation, not a drop-in replacement | INT-03 |

All task definitions are in the [backlog](../backlog.md). The Framework's
GoBGP, endpoint and OVN/OVS ownership services are reusable without CNI,
Multus or Kubernetes. Kubernetes CRDs and reconciliation remain application
integration surfaces, not mandatory Framework dependencies.
