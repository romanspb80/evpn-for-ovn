# Integration strategy and research goals

The Framework uses GoBGP for EVPN and alternative Netlink-Programmed or
OVSDB-Programmed adapters; OVN/OVS owns workload forwarding. CNI/Multus is a
current application, not the Framework architecture. Reusable node-agent
endpoint services are also exercised independently of CNI.

Detailed future designs now live in the
[integration index](integrations/README.md):
[OpenStack Neutron](integrations/openstack-neutron.md) and
[DCI/multi-cluster](integrations/dci-multicluster.md).
[OVN-Kubernetes](upstream/ovn-kubernetes.md) is a separate upstream integration
investigation, not a drop-in GoBGP replacement for FRR/frr-k8s.

The k8gobgp CRD/gRPC [ownership audit](../architecture/runtime-dependencies.md)
is recorded; generalized recovery and broader topologies remain open.
FRR remains an external interoperability peer, not the chosen Framework
control plane. These designs do not claim production readiness, Type-5,
multi-cluster support or feature equivalence. Priorities and acceptance
criteria have one home in the [backlog](backlog.md).
