# Deployment overview

The Kubernetes Operator and CNI workflows are experimental lab components,
not a production installation procedure. The checked-in manifests and
Containerlab examples are intended for isolated, disposable environments.

Deployment is a mechanism for the [networking use cases](../use-cases/README.md),
not an additional use case. The [deployment and operations guide](../guides/deployment-and-integration.md)
collects CNI/Multus, standalone OVN and Operator workflows and their test
references; the configuration and authority boundaries below are unchanged.

## Kubernetes profile

The Operator reconciles an `EvpnCore` configuration into constrained core
resources. Its `managed-single-node` profile manages an OVN/OVS Service and
Deployment plus an optional Node-Agent DaemonSet; `external-ovn` manages only
the Node-Agent DaemonSet. The Operator does not deploy application workloads
or the legacy `evpn-mgmt`, `ovsdb-mgmt` or `bgp_ad_handler` services. The
Node-Agent is privileged and requires host access for CNI and OVN/OVS
integration; restrict who can create or modify its configuration. The
Operator's privileges are broad and must be isolated.

Kubernetes manages configuration and lifecycle; it is not the BGP speaker.
The optional upstream `k8gobgp` provider manages GoBGP speaker configuration.
Multus invokes the project CNI for secondary attachments when CNI service is
enabled. The cluster's primary CNI remains separate. Standalone Node-Agent
EVPN import/export paths are non-CNI runtime modes and do not require the
Operator.

## Legacy compatibility profile

`deploy/kind/k8s.yaml` is a separate legacy compatibility and test manifest,
not the current Operator resource set or supported Netlink profile. Its
management RPCs have no transport authentication or authorization. ClusterIP
limits network routing exposure but does not authenticate permitted
in-cluster callers. Do not use it in a shared cluster.

## Standalone mode

Standalone Framework paths do not require the Operator. Standalone export
requires protected authorization configuration, durable fencing, OVN identity
checks and speaker attestation. It is not a complete general-purpose platform
installer.

## Isolated lab setup

Use the [Containerlab lifecycle guide](../../deploy/containerlab/README.md)
for prerequisites, ownership receipts, isolation and exact cleanup. The
[VM E2E guide](../testing/vm-e2e-lab.md) describes the isolated VM workflow.
No example authorizes reuse of an existing cluster or broad resource cleanup.
