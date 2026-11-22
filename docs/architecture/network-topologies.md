# Network topologies and dataplane boundaries

See the [README use-case map](../../README.md#use-cases) for the problem,
or the [user-facing guides](../use-cases/README.md) for prerequisites,
implementation status, existing diagrams and test/detail entry points.

This document distinguishes three planes that are easy to conflate:

- **BGP/EVPN control plane:** GoBGP exchanges Type-2 MAC/IP reachability with
  peers. A learned or advertised route is not packet-forwarding evidence.
- **OVN/OVS workload dataplane:** OVN logical state and controllers program
  OVS forwarding. For external EVPN traffic, the bounded candidate path uses
  OVN/OVS VXLAN integration.
- **Shared-OVN local connectivity:** OVN Geneve connects workloads in one
  shared OVN domain without requiring EVPN Type-2.

[![Netlink integration topology](../uml/netlink-programmed-type2.svg)](../uml/netlink-programmed-type2.svg)

The [external-fabric topology](../uml/external-evpn-fabric.svg) and
[route/packet flow](../uml/external-evpn-type2-exchange.svg) separate BGP
EVPN Type-2 exchange from OVN/OVS VXLAN workload forwarding. The
[independent-domain topology](../uml/multidomain-gobgp-gobgp-type2.svg) and
[its route/packet flow](../uml/multidomain-type2-flow.svg) are planned
designs, not end-to-end validation.

Linux bridge, VXLAN, FDB and neighbor entries used by the Netlink integration
are supporting state consumed by OVN/OVS; they are not a separate workload
forwarding dataplane. A packet test must attribute traffic to the intended
OVN/OVS path and distinguish Geneve from VXLAN.

The release implementation reads NB, SB and OVSDB state for endpoint
observation and authorization ([Node-Agent observer](../../pkg/nodeagent/export_observer.go));
the [GoBGP adapter](../../pkg/evpn/type2_gobgp.go) adds and withdraws owned
Type-2 paths and reads the local RIB. Remote-route import reads the GoBGP RIB,
validates route attributes and source, then reconciles owned Netlink FDB and
neighbor state ([remote importer](../../pkg/evpn/remote_netlink_import.go)).
That backend does not write SBDB or OVS OpenFlow. Direct OVSDB tunnel
programming is maintenance/test-only, not the selected release import path.
Consequently, a route receipt or Netlink update must not be drawn as an
automatic OVS tunnel or workload-flow programming action.

The validated Two-Chassis EVPN Type-2 use case has two OVN/OVS chassis sharing
NB/SB, with distinct switches/VTEPs and GoBGP route exchange. Bidirectional
VXLAN forwarding is a separate workload dataplane from the BGP control plane.
This topology is distinct from ordinary shared-domain Geneve and independent
OVN-domain interconnection. See the [use-case description](../use-cases/README.md#two-chassis-evpn-type-2-connectivity)
and [validation archive](../internal/validation-history/two-chassis.md) for
scope and provenance.

Independent OVN domains, generalized multi-chassis/multi-VTEP lifecycle,
Type-5, multihoming and broad mobility are not established. Separate bounded
tests must not be combined into an untested topology. See [integration modes](integrations.md)
and the [release scope](../release/v0.1.0.md).

For topology-to-lifecycle navigation, use the [Testing Guide scenario
table](../../TESTING_GUIDE.md#live-test-scenarios-and-expected-behavior) and
[scenario diagrams and procedures](../testing/lifecycle-scenarios.md).
Historical and proposed diagrams are explanatory context, not current
release acceptance; restart sequences describe the bounded fixture contract.
