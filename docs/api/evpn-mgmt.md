# EVPN management API

Service: `evpn.EvpnService`. Contract:
[`proto/evpn.proto`](../../proto/evpn.proto); generated descriptors:
[`evpn_grpc.pb.go`](../../gen/evpn_mgmt/evpn_grpc.pb.go);
implementation: [`services/evpn_mgmt/main.go`](../../services/evpn_mgmt/main.go).
All message types below are in package `evpn`.

This is a compatibility management surface, not the accepted Framework
exporter's route-ownership API. No maintained non-generated Go client of this
service was found; external/manual clients may exist. Its AddNetwork handler
internally calls ProvisionVni. Standard deployment exposes it, but managed
k8gobgp acceptance does not use it to configure the speaker or neighbors.

| RPC | Request -> response | Implementation, side effects and dependencies |
|---|---|---|
| AddSpeaker | `Speaker` -> `Status` | Concrete: StartBgp using ASN/router ID and environment fallback, persists process environment, waits for speaker readiness. Requires GoBGP; potentially conflicts with k8gobgp. |
| GetSpeaker | `SpeakerRequest` -> `SpeakerResponse` | Concrete: current GetBgp or stopped/environment fallback; request id is not a multi-speaker selector. |
| DelSpeaker | `SpeakerRequest` -> `Status` | Concrete: StopBgp; ignores id. GetBgp failure can be reported as already stopped, not authoritative absence. |
| AddNeighbor | `Neighbor` -> `Status` | Concrete: AddPeer with EVPN family and local-address heuristics. Fields `neighborAddress`, `peerAsn`; requires running speaker. Not the accepted BGPConfiguration writer. |
| GetNeighbors | `Empty` -> `NeighborsList` | Concrete: ListPeer stream; initial failure produces empty response, stream error can produce partial results. |
| DelNeighbor | `NeighborRequest` -> `Status` | **Unimplemented:** generated fallback, gRPC codes.Unimplemented. |
| AddNetwork | `Network` -> `Status` | Concrete: positive `vni` delegates to ProvisionVni using inferred LS; nonpositive VNI returns successful no-op. No OVN/network creation. |
| GetNetworks | `Empty` -> `NetworksList` | **Unimplemented:** generated fallback. |
| DelNetwork | `NetworkRequest` -> `Status` | **Unimplemented:** generated fallback. Not ovsdb-mgmt DelNetwork. |
| ProvisionVni | `VniConfig` -> `Status` | Concrete: requires positive `vni`, defaults blank `logical_switch`, records VNI -> LS in a mutex-protected in-memory map. `enabled` is logged only. No Linux, OVSDB or domain CreateVNI operation. |
| DeleteVni | `VniRequest` -> `Status` | Concrete: positive VNI removes map entry idempotently; no integration object deletion. |
| GetVnis | `Empty` -> `VnisList` | Concrete: unsorted process-local VNI keys; startup seeds VNI 1000. Does not discover actual OVN/kernel state. |
| AddClient | `Client` -> `Status` | Concrete: constructs Type-2 MAC/IP from `mac`, `ip`, `label`, `rd`, calls AddPath. No returned/persisted UUID receipt, configured RT set or accepted exporter next-hop/ownership model. |
| DelClient | `ClientRequest` -> `Status` | Concrete: scans global EVPN RIB for id matching MAC or IP; deletes matching full paths, logs individual failures and reports a success count. Not exact-UUID receipt withdrawal. |
| GetClients | `Empty` -> `ClientsList` | Concrete: ListPath EVPN and extract MAC/IP/label/IP-based RD; failure can be empty or partial, not complete route identity/provenance. |

Concrete status-returning methods mostly use `Status{success:false,...}` and
nil RPC error; there is no consistent typed gRPC error contract or uniform
nil-client protection. Get methods do not necessarily establish backend
health. See [transport/error limitations](README.md#transport-and-errors).

Keep the public `ProvisionVni` contract and planned domain
`CreateVNI(LS,VNI)` distinct from today's map-only implementation. Do not
claim provisioning Linux/OVN state from a successful ProvisionVni response.
Canonical work: [FW-01/FW-02/API-01](../roadmap/backlog.md).

Accepted route advertisement instead uses
[`pkg/evpn/type2_gobgp.go`](../../pkg/evpn/type2_gobgp.go):
complete RIB observation, validated attributes, AddPath UUID receipt and exact
UUID DeletePath. It can be called without evpn-mgmt and without CNI.
