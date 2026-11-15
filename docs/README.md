# Documentation

EVPN-for-OVN is experimental. Start with the [project overview](../README.md)
and [security boundary](../SECURITY.md); use live environments only in an
isolated, disposable lab.

## Getting Started

- [Kubernetes CNI solution](solutions/kubernetes-cni/README.md): the current
  concrete networking solution and its lifecycle.
- [Containerlab prerequisites and secondary-CNI lab](../deploy/containerlab/README.md):
  an experimental, disposable Kind/OVN environment, not a production installer.
- [Vagrant secondary-CNI environment](../deploy/containerlab/vagrant-secondary/README.md).

## Concepts and Architecture

- [Component overview](concepts/component-overview.md): roles and interactions.
- [Network topologies](architecture/network-topologies.md): shared OVN Geneve
  versus independent EVPN domains.
- [EVPN Framework Architecture](architecture/framework-architecture.md):
  canonical core boundary, backend roles and integration limits.
- [Integration modes](architecture/integrations.md) and
  [diagram index](uml/README.md).
- [Endpoint authorization](architecture/standalone-endpoint-authorization.md)
  and [Type-2 ownership and recovery](architecture/type2-restart-ownership.md).

## Deployment and Operations

- [Operator lifecycle](architecture/evpn-core-operator.md) and
  [lab deployment](architecture/deployment.md).
- [Runtime dependencies and provider ownership](architecture/runtime-dependencies.md).
- [Known issues](known-issues/README.md) and [security](../SECURITY.md).

## Configuration and API Reference

- [API index](api/README.md): Node-Agent, EVPN and OVSDB management.
- [Operator API and configuration](architecture/evpn-core-operator.md).
- [Standalone authorization configuration](architecture/standalone-endpoint-authorization.md).

## Development and Testing

- [Development and Testing Guide](../TESTING_GUIDE.md): build and offline test
  commands, acceptance scopes, and E2E workflows.
- [Containerlab E2E laboratories](../deploy/containerlab/README.md).
- [Type-2 route fixture](../tests/e2e/type2-fixture/README.md) and
  [Netlink acceptance](../tests/e2e/netlink-acceptance/README.md).
- [FRR lab peer](../frr/README.md) and
  [test-only Remote2 fixture contract](../tools/e2e/remote2/README.md).

## Capability Status and Roadmap

- [Capability matrix](project/capability-matrix.md): status, evidence and limits.
- [Roadmap and canonical backlog](roadmap/README.md).
- [Integration targets](roadmap/integrations/README.md) and
  [release gates](roadmap/release.md).

## Historical Engineering Evidence

- [Operator Type-2 acceptance](architecture/operator-type2-acceptance.md).
- [External EVPN fabric evidence](testing/external-evpn-fabric.md).
- [Historical validation record](archive/validation-record.md) and
  [project history and evidence policy](archive/README.md).
- [Upstream issue drafts](issues/README.md).

Historical results describe their original fixtures and source identities;
they are not a fresh validation of the current deployment.
