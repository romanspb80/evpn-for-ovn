# EVPN for OVN

EVPN for OVN helps network and platform engineers connect OVN workloads to
external EVPN/VXLAN networks using GoBGP Type-2 routes and ownership-aware
endpoint lifecycle. It is an experimental Go framework with bounded
standalone Node-Agent and Kubernetes paths, not a production fabric installer
or a guarantee of interchangeable deployment combinations.

EVPN Type-2 advertises endpoint MAC/IP reachability and the remote VTEP
location; the framework connects that control-plane information to OVN/OVS
VXLAN forwarding. GoBGP is the intentionally selected engine, not an
interchangeable backend.

OVN/OVS is the intentional workload networking foundation, and GoBGP is the
selected BGP/EVPN engine. The reusable framework core handles route processing,
advertisement and withdrawal, endpoint ownership, authorization, fencing and
durable recovery state. It is not an OVN-independent network platform or a
production installer.

[![EVPN for OVN component overview](docs/uml/component-overview.svg)](docs/uml/component-overview.svg)

## Architecture and components

- **EVPN Core:** Type-2 route reconciliation and owned endpoint lifecycle.
- **Node-Agent:** node-local integration state and recovery; it serves CNI
  requests only when started in the CNI-serving mode.
- **GoBGP:** BGP/EVPN control-plane operations; optional k8gobgp provider in
  Kubernetes environments.
- **OVN/OVS:** workload forwarding, including the external VXLAN dataplane.
- **Operator:** Kubernetes resource management for the Netlink core profile.

Netlink manages Linux integration state consumed by OVN/OVS; a Linux bridge
is not a separate workload-forwarding dataplane. BGP route exchange and
OVN/OVS-to-external-VTEP packet forwarding are distinct paths.

## Use Cases

The primary validated networking use case is EVPN Type-2 connectivity across
two OVN chassis, with GoBGP handling route exchange and OVN/OVS forwarding
workload traffic over VXLAN.

### 1. [Two-Chassis EVPN Type-2 Connectivity](docs/use-cases/README.md#two-chassis-evpn-type-2-connectivity)

Validation covers bidirectional forwarding, route reconciliation and
withdrawal, Node-Agent restart recovery, and cleanup. The two chassis share one
OVN NBDB/SBDB control plane; this is not independent-domain interconnection.

### 2. [Integrating OVN with an External EVPN/VXLAN Fabric](docs/use-cases/README.md#integrating-ovn-with-an-external-evpnvxlan-fabric)

Integrate OVN-managed workloads with an existing EVPN fabric, including
external VTEPs and BGP EVPN speakers such as FRR.

The framework handles authorized endpoint advertisement, remote route import,
and ownership-aware withdrawal.

**Status:** Experimental. General full-fabric and vendor interoperability are
not qualified; the validated two-chassis scenario above is a narrower scope.

[![EVPN Type-2 control and OVN/OVS external VXLAN forwarding](docs/uml/external-evpn-fabric.svg)](docs/use-cases/README.md#integrating-ovn-with-an-external-evpnvxlan-fabric)

The diagram separates route exchange, supporting Linux state and actual
OVN/OVS-to-external-VTEP packets; it is not fabric-wide acceptance evidence.

Independent OVN-domain interconnection remains planned; its design and
validation requirements are tracked in the [roadmap](docs/roadmap/README.md#independent-ovn-domain-interconnection-planned).

### Deployment and Integration

The framework provides experimental integration paths for standalone OVN
environments and Kubernetes deployments, including secondary CNI/Multus
networking and Kubernetes Operator management.

Deployment support varies by configuration. Refer to the
[capability matrix](docs/project/capability-matrix.md) and
[deployment guides](docs/architecture/deployment.md) for supported combinations
and validation boundaries. The [detailed use-case guide](docs/use-cases/README.md)
describes both networking goals and links supporting
[deployment and operations](docs/guides/deployment-and-integration.md),
[network topologies](docs/guides/network-topologies.md), and
[reliability and lifecycle](docs/guides/reliability-and-lifecycle.md) guidance.

## Implemented functionality

The experimental framework provides Type-2 import/export with policy checks,
exact path withdrawal and durable ownership receipts; CNI-serving Node-Agent
ADD/CHECK/DEL when configured; identity-gated recovery; and Operator
worker-inventory publication. Standalone EVPN import/export paths are
separate and do not launch that CNI service. These are implementation
features, not guarantees of generalized multi-VTEP recovery or production
availability.

## Architecture dimensions and release scope

These are separate architectural questions, not a promise that every
combination is implemented or supported. See the [capability
matrix](docs/project/capability-matrix.md) for boundaries.

| Dimension | v0.1.0 scope |
|---|---|
| Network-state programming | Netlink-Programmed is the selected experimental integration; OVSDB-Programmed is retained for maintenance and testing, outside release deployment scope. |
| Deployment and orchestration | Standalone Node-Agent EVPN import/export is a non-CNI runtime and does not require the Operator. A CNI-serving Node-Agent may be launched separately for secondary-network service; that is distinct from the standalone non-CNI runtime. In Operator profiles, Node-Agent is optional in `managed-single-node` and required in `external-ovn`. |
| Workload integration | Kubernetes CNI/Multus provides secondary attachments and OVN endpoint lifecycle; it is separate from the cluster's primary CNI and from standalone non-CNI EVPN paths. |
| Platform integration | Project-owned OVN/OVS is current. OVN-Kubernetes and Neutron / ML2-OVN are planned, not implemented platform integrations. |
| BGP/EVPN control plane | GoBGP is the BGP/EVPN engine. `k8gobgp` is an optional Kubernetes provider; the Operator currently accepts only `k8gobgp`, not a standalone-speaker provider. |

The Kubernetes secondary CNI is separate from the cluster's primary CNI.
Ordinary connectivity inside a shared OVN Geneve domain does not require
EVPN Type-2 routes. External Type-2 exchange supplies EVPN/VXLAN integration;
it does not replace OVN networking.

## Quick start: build and offline tests

Use the Go toolchain declared in `go.mod` and Python 3 for test harnesses.
From the repository root:

```bash
go test ./...
go build ./cmd/evpn-cni ./cmd/evpn-node-agent ./cmd/evpn-ovsdb \
  ./services/evpn_mgmt ./services/ovsdb_mgmt ./services/bgp_ad_handler \
  ./deploy/operator
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/lib -p 'test_*.py'
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/e2e -p 'test_*.py'
```

For configuration and usage examples, start with the
[deployment guide](docs/architecture/deployment.md), the
[Kubernetes CNI guide](docs/solutions/kubernetes-cni/README.md), or the
[two-worker Operator example](deploy/operator/examples/two-worker/README.md).
These require trusted disposable lab environments, not production clusters.
Read [SECURITY.md](SECURITY.md) before any live workflow. The
[testing guide](TESTING_GUIDE.md) explains preparation, scenarios, execution,
evidence inspection and run-owned cleanup.

## Project History

EVPN-for-OVN began in [**2018**](https://github.com/romanspb80/evpn-for-ovn/commit/63777a5b23cd66f9776cb9a7c9de735773e860ec)
as an experimental project for interconnecting
OVN-based networks using BGP EVPN and VXLAN. The original Python prototype
evolved with Ryu and ovsdbapp, with service communication through RabbitMQ and
oslo.messaging.

The modern Go implementation is a substantial architectural redesign using
GoBGP and libovsdb, rather than a direct translation of the prototype. It
introduces a modular framework, node-local ownership and lifecycle management,
and Kubernetes CNI/Operator integration while retaining OVN/OVS as its
networking foundation.

The historical [`master` branch](https://github.com/romanspb80/evpn-for-ovn/tree/master)
preserves the Python prototype and its development history.
[`main`](https://github.com/romanspb80/evpn-for-ovn/tree/main) contains the
modern Go framework. The prototype documents the project's origins; neither
its historical examples nor the current experimental framework imply
production readiness.

## Roadmap at a glance

Bounded Type-2/Netlink, standalone EVPN, the validated two-chassis Type-2
scenario, secondary CNI/Multus and constrained Operator paths are
implemented experimentally. Orphan/API-outage recovery and generalized
multi-worker lifecycle lack complete acceptance. Type-5 and
OVN-Kubernetes/Neutron integrations remain planned; Type-3 and multihoming
are exploratory topics outside the release scope. A separate Type-2
interoperability program names FRR and four vendor peers, all planned or
awaiting authorized image availability; none is claimed to have passed.
These are not delivery commitments. See the
[complete roadmap and vendor test gates](docs/roadmap/README.md) and
[known limitations](docs/known-issues/README.md).

## Documentation

- [Practical user-facing use cases](docs/use-cases/README.md)
- [Component overview](docs/architecture/component-overview.md)
- [Architecture and component roles](docs/architecture/framework-architecture.md)
- [Deployment and configuration](docs/architecture/deployment.md)
- [Security contract](docs/security/netlink-deployment-security-contract.md)
- [API reference](docs/api/README.md)
- [Known limitations and troubleshooting](docs/known-issues/README.md)
- [Development and testing](TESTING_GUIDE.md)
- [Release scope](docs/release/v0.1.0.md)
- [Capabilities and planned integrations](docs/project/capability-matrix.md)
- [Roadmap and future work](docs/roadmap/README.md)
- [Documentation index](docs/README.md)

## Experimental limitations and future work

Implemented functionality is not production qualification. Generalized
multi-VTEP lifecycle, arbitrary foreign-state preservation, production
upgrade/rollback and service availability are outside the release guarantees.
Missed CNI DEL/orphan recovery and Kubernetes API-unavailability recovery
remain incomplete operational acceptance areas. Direct OVSDB regressions are
deferred; Type-3 and Type-5 functionality are outside the implemented Type-2
release scope.

Secure CNI transport requires root:root Unix peer authentication using
`SO_PEERCRED` and kernel `SO_PEERPIDFD`, independently of optional continuity
checks. The supported Operator profile excludes legacy
unauthenticated management services. Privileged components still require
trusted hosts and administrators; ClusterIP is not authentication.

## Contributing and license

Use the [development and testing guide](TESTING_GUIDE.md) when extending
components or harnesses. Keep changes scoped, add appropriate tests, and
document configuration and security implications without claiming broader
operational support.

The project is licensed under [Apache 2.0](LICENSE). See [NOTICE](NOTICE) and
[licensing information](docs/release/licensing.md) for dependency and
distribution considerations.
