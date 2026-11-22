# EVPN management API

Service: `evpn.EvpnService`. Contract:
[`proto/evpn.proto`](../../proto/evpn.proto); generated descriptors:
[`evpn_grpc.pb.go`](../../gen/evpn_mgmt/evpn_grpc.pb.go);
implementation: [`services/evpn_mgmt/main.go`](../../services/evpn_mgmt/main.go).
All message types below are in package `evpn`.

This is a compatibility management surface, not the accepted Framework
exporter's route-ownership API. No maintained non-generated Go client of this
service was found; external/manual clients may exist. Its AddNetwork handler
internally calls ProvisionVni. The supported Operator Netlink core profile no
longer deploys it. The legacy Kind compatibility manifest still exposes it
without authentication; managed k8gobgp deployments do not use it to configure
the speaker or neighbors.

| RPC | Request -> response | Implementation, side effects and dependencies |
|---|---|---|
| AddSpeaker | `Speaker` -> `Status` | **Disabled:** returns gRPC `PermissionDenied`; use k8gobgp to manage the speaker. |
| GetSpeaker | `SpeakerRequest` -> `SpeakerResponse` | Concrete: current GetBgp or stopped/environment fallback; request id is not a multi-speaker selector. |
| DelSpeaker | `SpeakerRequest` -> `Status` | **Disabled:** returns gRPC `PermissionDenied`; use k8gobgp to manage the speaker. |
| AddNeighbor | `Neighbor` -> `Status` | **Disabled:** returns gRPC `PermissionDenied`; configure managed peers through k8gobgp BGPConfiguration instead. |
| GetNeighbors | `Empty` -> `NeighborsList` | Concrete: ListPeer stream; initial failure produces empty response, stream error can produce partial results. |
| DelNeighbor | `NeighborRequest` -> `Status` | **Unimplemented:** generated fallback, gRPC codes.Unimplemented. |
| AddNetwork | `Network` -> `Status` | Concrete: positive `vni` delegates to ProvisionVni using inferred LS; nonpositive VNI returns successful no-op. No OVN/network creation. |
| GetNetworks | `Empty` -> `NetworksList` | **Unimplemented:** generated fallback. |
| DelNetwork | `NetworkRequest` -> `Status` | **Unimplemented:** generated fallback. Not ovsdb-mgmt DelNetwork. |
| ProvisionVni | `VniConfig` -> `Status` | Concrete: requires positive `vni`, defaults blank `logical_switch`, records VNI -> LS in a mutex-protected in-memory map. `enabled` is logged only. No Linux, OVSDB or domain CreateVNI operation. |
| DeleteVni | `VniRequest` -> `Status` | Concrete: positive VNI removes map entry idempotently; no integration object deletion. |
| GetVnis | `Empty` -> `VnisList` | Concrete: unsorted process-local VNI keys; startup seeds VNI 1000. Does not discover actual OVN/kernel state. |
| AddClient | `Client` -> `Status` | **Disabled:** returns gRPC `PermissionDenied`; use an authorized exporter rather than injecting Type-2 routes without inventory checks or ownership receipts. |
| DelClient | `ClientRequest` -> `Status` | **Disabled:** returns gRPC `PermissionDenied`; withdrawals must be performed by the route owner using its exact receipt. |
| GetClients | `Empty` -> `ClientsList` | Concrete: ListPath EVPN and extract MAC/IP/label/IP-based RD; failure can be empty or partial, not complete route identity/provenance. |

Concrete status-returning methods mostly use `Status{success:false,...}` and
nil RPC error; there is no consistent typed gRPC error contract or uniform
nil-client protection. Get methods do not necessarily establish backend
health. See [transport/error limitations](../security/netlink-deployment-security-contract.md).

Keep the public `ProvisionVni` contract and planned domain
`CreateVNI(LS,VNI)` distinct from today's map-only implementation. Do not
claim provisioning Linux/OVN state from a successful ProvisionVni response.
See the [security boundary](../security/netlink-deployment-security-contract.md) and the [candidate scope](../release/v0.1.0.md).

Accepted route advertisement instead uses
[`pkg/evpn/type2_gobgp.go`](../../pkg/evpn/type2_gobgp.go):
complete RIB observation, validated attributes, AddPath UUID receipt and exact
UUID DeletePath. It can be called without evpn-mgmt and without CNI.
