# Kubernetes CNI application

Kubernetes CNI/Multus is an application consuming the EVPN Framework. It
implements the CNI protocol and Kubernetes attachment lifecycle; it is not the
Framework and does not define either Framework integration mode.

## Responsibilities and flow

Multus invokes `evpn-cni` for a secondary Pod network. The plugin sends the
request to `evpn-node-agent` over the node-local gRPC Unix socket. The
node-agent validates the network, attachment, workload, and incarnation
identity, then coordinates the network namespace/interface backend with shared
Framework endpoint and attachment services. The
[NodeAgentCNI API reference](../../api/node-agent.md) records request identity,
outcomes, dependencies and gRPC errors.

The Framework creates or verifies the owned OVN logical switch port and local
OVS attachment. Optional Type-2 import/export is configured separately and is
disabled by default. It runs asynchronously from ADD/DEL; a successful CNI
response does not itself establish route advertisement/withdrawal.
In the kind test topology, GoBGP is provided by upstream
`k8gobgp`; FRR is the external peer. OVN/OVS owns workload forwarding.

## CNI operations

- **ADD** creates an attachment and owned endpoint state, returning the CNI
  result only after the required operations succeed.
- **CHECK** validates the exact endpoint identity and expected network and
  OVN/OVS attachment; it is read-only and does not recreate missing resources.
- **DEL** removes only matching owned state. Repeated DEL is idempotent and a
  stale workload incarnation must not delete a replacement endpoint.
- **Replacement** uses a new attachment/workload identity on the same network;
  the old endpoint's withdrawal and cleanup must not remove survivor state.

The accepted rollback scenario injects a missing network namespace before the
OVN LSP is created, verifies no endpoint resources remain, and then retries
successfully. It does not establish general compensation for failures after
LSP creation; post-LSP ADD compensation remains open work.

## Configuration and deployment

The example Multus NetworkAttachmentDefinition is
[`network-attachment-definition.example.yaml`](../../../deploy/kind/cni-multus-test/network-attachment-definition.example.yaml).
It includes the CNI version, network name/type, node-agent socket, network ID,
attachment ID, logical switch, and example static IPv4/MAC values. Kubernetes
calls must provide unambiguous workload identity. Dynamic IPAM is not
implemented.

The node-agent DaemonSet, CNI staging, Multus configuration, and privileged
host mounts used by live tests are generated for their disposable kind
clusters. They are not a production installation recipe. Production lifecycle,
privilege, mount, RBAC, addressing, and upgrade policies remain to be designed
and validated.

## Verification

The accepted application-level test is the [CNI Full Lifecycle E2E](../../../TESTING_GUIDE.md#cni-full-lifecycle-acceptance),
run with `tools/run-cni-full-lifecycle-e2e.sh`. It covers initial and
replacement ADD/CHECK/DEL, bidirectional traffic and five-point packet
verification, idempotency, negative CHECK, pre-LSP rollback, survivor
preservation, and guarded cleanup.

The no-replacement interoperability scenario is a separate, narrower
regression using `tools/run-cni-mvp-2.4b-multus.sh`. Neither CNI test replaces
the independent Framework Netlink or OVSDB acceptance suites. Shared-VTEP
lifecycle and broader multi-chassis behavior remain untested.
