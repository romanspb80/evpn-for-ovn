# Documentation

EVPN for OVN is experimental. Read the [project overview](../README.md) and
[security guidance](../SECURITY.md) before using any live example.

## Start here

- [Validated Two-Chassis EVPN Type-2 use case](use-cases/README.md#two-chassis-evpn-type-2-connectivity)
- [Use cases and scenario boundaries](../README.md#use-cases)
- [Deployment, CNI/Multus, standalone integration and Operator operations](guides/deployment-and-integration.md)
- [Network topology scenarios and validation limits](guides/network-topologies.md)
- [Reliability, recovery and owned lifecycle](guides/reliability-and-lifecycle.md)
- [Kubernetes secondary-CNI solution](solutions/kubernetes-cni/README.md)
- [Component overview](architecture/component-overview.md)
- [Architecture and component responsibilities](architecture/framework-architecture.md)
- [Netlink and OVSDB integration scope](architecture/integrations.md)
- [Deployment and Operator configuration](architecture/deployment.md)
- [API reference](api/README.md)
- [Security contract](security/netlink-deployment-security-contract.md)
- [Known limitations and troubleshooting](known-issues/README.md)

External-fabric interoperability is experimental; independent OVN-domain
interconnection is planned in the [roadmap](roadmap/README.md).

## Architecture and operations

- [Core component boundaries](architecture/evpn-core-component-boundaries.md)
- [Operator API and lifecycle contracts](architecture/evpn-core-operator.md)
- [Network topology and dataplane boundaries](architecture/network-topologies.md)
- [CNI creation authority](architecture/cni-creation-authority.md)
- [CNI endpoint continuity and lifecycle](architecture/cni-endpoint-continuity.md)
- [Standalone endpoint authorization](architecture/standalone-endpoint-authorization.md)
- [Type-2 ownership and recovery](architecture/type2-restart-ownership.md)
- [Runtime dependencies and configuration ownership](architecture/runtime-dependencies.md)

Architecture diagrams are indexed in the [diagram guide](uml/README.md); editable
PlantUML sources are kept beside generated SVG previews.

## Testing

- [Testing document roles and current versus historical scope](testing/README.md)
- [Containerlab lab setup and cleanup](../deploy/containerlab/README.md)
- [Isolated VM E2E workflow](testing/vm-e2e-lab.md)
- [Development and testing guide](../TESTING_GUIDE.md)
- [Live scenario diagrams, source entry points and qualification gaps](testing/lifecycle-scenarios.md)

## Limitations, contributions and release direction

- [Known limitations and safe troubleshooting](known-issues/README.md)
- [Recovered engineering findings and unsubmitted investigations](known-issues/engineering-findings.md)
- [Upstream Contributions](project/upstream-contributions.md)
- [Capabilities and planned integrations](project/capability-matrix.md)
- [Roadmap and future work, including vendor EVPN/VXLAN interoperability](roadmap/README.md)
- [Release scope and limitations](release/v0.1.0.md)
- [License and third-party notices](release/licensing.md)

## Validation history

The [validation-history archive](internal/validation-history/README.md) is the
canonical index of historical campaigns and release acceptance evidence.
Current test reproduction instructions remain in the
[Testing Guide](../TESTING_GUIDE.md); archive reports are not procedures or a
current release scorecard.
