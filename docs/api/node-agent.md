# NodeAgentCNI application API

Service: `node_agent_cni.NodeAgentCNI`. Contract:
[`proto/node_agent_cni.proto`](../../proto/node_agent_cni.proto); descriptors:
[`node_agent_cni_grpc.pb.go`](../../gen/node_agent_cni/node_agent_cni_grpc.pb.go);
handlers: [`pkg/nodeagent/server.go`](../../pkg/nodeagent/server.go).

Main caller is [`cmd/evpn-cni`](../../cmd/evpn-cni/main.go), with direct
unit/live test clients. All three methods have concrete handlers. This is
the application-facing CNI service, not a universal Framework endpoint API.
Reusable endpoint/network managers can also be called directly without CNI.

| RPC | Request -> response | Side effects, dependencies and error behavior |
|---|---|---|
| Add | `CNIAddRequest` -> `CNIAddResponse` | Validate/map identity, ensure netns/veth, insert or compare endpoint record, ensure owned NB LSP and OVS attachment. Equivalent replay returns AddOutcome.idempotent_replay; conflicting state uses AlreadyExists. Validation uses InvalidArgument; network/endpoint failure uses FailedPrecondition. On failure it deletes newly created network state and failed record, but post-LSP compensation is incomplete. |
| Del | `CNIDelRequest` -> `CNIDelResponse` | Resolve exact identity, delete identity-owned OVS attachment then NB LSP through EndpointManager, then network attachment. Missing store can recover state from request when real checking is enabled. Structured deleted/already_absent/stale_operation outcomes; stale delete does not delete replacement. InvalidArgument, FailedPrecondition or unexpected Internal failures. |
| Check | `CNICheckRequest` -> `CNICheckResponse` | Read-only identity, NB port, network and OVS attachment checks; can reconstruct request-backed record after store loss. Returns structured CheckDetails. InvalidArgument, NotFound and FailedPrecondition on malformed/missing/mismatched state. Does not recreate resources or prove traffic/EVPN convergence. |

All types above are in `node_agent_cni`. `EndpointIdentity` includes
`endpoint_id`, `network_id`, `attachment_id`, `workload_uid`, `incarnation`.
Runtime includes CNI version, container ID, netns path and interface name;
attachment data includes logical switch and static MAC/IP/CIDR configuration.
Kubernetes metadata is application identity, not a dependency of the Go
Framework managers. `request_id` is tracing metadata, not the ownership key.

`StartUnixServer` restricts the configured socket to mode `0600`. Real
runtime requires configured OVN NBDB/OVSDB and privileged namespace/network
access; dry-run/test backends are not live forwarding evidence. The
in-memory record store is not a durable database, but request-backed CHECK/
DEL recovery exists. Broader restart guarantees remain
[FW-04](../roadmap/backlog.md), not an assertion of no recovery at all.

Type-2 export/import runs in separate asynchronous reconcilers when enabled.
ADD/DEL do **not** synchronously advertise/withdraw BGP routes before returning.
The [CNI sequence](../uml/evpn-cni-lifecycle-sequence.puml) distinguishes the
application response from later observation and exact-UUID route lifecycle.
There is no NodeAgentCNI DeleteNetwork RPC; network teardown uses the
guarded runner-side `ovn-nbctl ls-del` of the test-owned switch after endpoint
and route cleanup, not through an invented node-agent network RPC.
