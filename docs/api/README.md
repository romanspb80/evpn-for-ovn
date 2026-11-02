# Framework and application APIs

This is a source-grounded reference, not a claim that every declared RPC is
implemented or production-supported. The checked-in `.proto` and generated
Go descriptors are authoritative for wire names and field numbers.

| Fully qualified service | Contract / implementation | Concrete handlers / declared RPCs |
|---|---|---|
| `evpn.EvpnService` | [EVPN management](evpn-mgmt.md), `proto/evpn.proto`, `services/evpn_mgmt` | 12 / 15 |
| `ovsdb_mgmt.OvsdbService` | [OVN/OVSDB management](ovsdb-mgmt.md), `proto/ovsdb.proto`, `services/ovsdb_mgmt` | 13 / 17 |
| `node_agent_cni.NodeAgentCNI` | [Node-local CNI](node-agent.md), `proto/node_agent_cni.proto`, `pkg/nodeagent` | 3 / 3 |

All 35 project RPCs are unary. Seven compatibility RPCs inherit the generated
`codes.Unimplemented` response. A concrete handler is not an end-to-end
acceptance claim. The references list callers found in maintained Go callers,
test runners and service code, excluding generated client wrappers.

## Domain operations are not all RPCs

Reusable `pkg/endpoint`, `pkg/attachment`, `pkg/evpn` and `pkg/nodeagent`
managers provide network/endpoint/route services without CNI.
`EnsureEndpoint`/`DeleteEndpoint` are Go operations, not invented public
Framework gRPC methods. `ProvisionVni` remains the public provisioning
contract; the domain `CreateVNI(LS,VNI)` contract is not implemented by a
method found in this checkout. See [FW-01](../roadmap/backlog.md).

The Framework Type-2 adapter calls upstream GoBGP
`apipb.GoBgpService` (`github.com/osrg/gobgp/v4/api`), notably GetBgp,
ListPath, AddPath and exact-UUID DeletePath. These are upstream APIs, not
methods of evpn-mgmt. k8gobgp owns speaker/peer/AF/VRF configuration in
accepted managed deployments. Direct compatibility StartBgp/AddPeer must
not race it; see the [writer matrix](../architecture/runtime-dependencies.md#bgpconfiguration-versus-direct-grpc).

## Transport and errors

| Endpoint | Current boundary |
|---|---|
| evpn-mgmt TCP `:50052` | `grpc.NewServer()` without configured TLS, authentication, authorization or reflection. GoBGP client uses insecure credentials and this service's k8gobgp compatibility mode requires TCP, not the accepted suites' Unix endpoint. |
| ovsdb-mgmt TCP `:50053` | Same server transport limitation. Depends on OVN NBDB and, for OVS operations, a working OVSDB client. Server may start despite OVS connection failure; readiness is not proof every method is usable. |
| NodeAgentCNI Unix socket | `StartUnixServer` sets mode `0600`; node-local filesystem permissions are the access boundary, not gRPC TLS. |
| Managed GoBGP in acceptance | Node-local Unix socket pinned to one speaker. Do not load-balance receipt-owned paths across speakers. |

**Framework gRPC TLS was not enabled in the C4.4f.2q.2 acceptance runs.**
Containerlab management CA/SSH/HTTPS verification is a separate boundary.
Do not expose unauthenticated management endpoints on untrusted networks.
Production transports and API semantics are future
[API-01/API-02](../roadmap/backlog.md#p1-reliability-and-contract-clarification)
work. Read the required [lab containment](../../SECURITY.md) before deployment;
candidate-specific exposure review remains a [release gate](../roadmap/release.md).

Compatibility services generally encode errors in `Status.success/message`
while returning gRPC OK. Some list/read methods return nil/empty/partial
results after backend failure. Callers must inspect both RPC errors and
payloads, and cannot infer authoritative absence from these reads.
NodeAgentCNI instead uses non-OK gRPC codes for request/conflict/precondition
failures, with structured idempotent/stale outcomes.

## Generation and inspection tooling

Generated code lives in [gen](../../gen/). Headers report protoc 3.21.12,
protoc-gen-go 1.36.11, and protoc-gen-go-grpc 1.4.0 (EVPN), 1.6.2 (OVSDB),
1.5.1 (CNI). No single pinned protobuf generation pipeline, Buf config,
generated HTML/Markdown API docs, registered gRPC reflection, gRPC-Gateway
or project OpenAPI/Swagger endpoint was found. Kubernetes CRD OpenAPI
schemas and indirect go-openapi dependencies are **not** a gRPC REST API.
OVN database model generation separately uses
[`tools/regen-ovn-nb-models.sh`](../../tools/regen-ovn-nb-models.sh).

The first release uses these Markdown references. If grpcurl is separately
installed, inspect local descriptors without reflection or a running server:

```bash
grpcurl -import-path proto -proto evpn.proto describe evpn.EvpnService
grpcurl -import-path proto -proto ovsdb.proto describe ovsdb_mgmt.OvsdbService
grpcurl -import-path proto -proto node_agent_cni.proto describe node_agent_cni.NodeAgentCNI
```

These are optional descriptor-only examples, not evidence that grpcurl is
bundled or that a live API call passed. Do not add a gateway merely to
document the API.
