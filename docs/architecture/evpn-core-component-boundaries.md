# EVPN Core component boundaries

EVPN Core is a logical set of route-processing, reconciliation, ownership and
recovery responsibilities inside the project runtimes. It is not a separate
service or a promise of platform independence.

- **GoBGP** handles BGP sessions and EVPN RIB state.
- **Node-Agent** hosts the node-local Framework runtime and the optional CNI
  service.
- **OVN/OVS** owns OVN network state and the workload forwarding dataplane.
- **Netlink** manages selected integration state consumed by OVN/OVS; Linux
  bridges are not a separate workload dataplane.
- **Kubernetes/Operator** supply the current experimental platform and
  configuration lifecycle.

The Netlink-programmed path is the selected candidate. Direct OVSDB code is
maintenance/test-only. Neither this component boundary nor a passing route
check proves packet forwarding, general recovery or production readiness.
See [Framework architecture](framework-architecture.md) and
[integration modes](integrations.md).
