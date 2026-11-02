# OVN / OVSDB management API

Service: `ovsdb_mgmt.OvsdbService`. Contract:
[`proto/ovsdb.proto`](../../proto/ovsdb.proto); generated descriptors:
[`ovsdb_grpc.pb.go`](../../gen/ovsdb_mgmt/ovsdb_grpc.pb.go);
implementation: [`services/ovsdb_mgmt/main.go`](../../services/ovsdb_mgmt/main.go).
All types below are in package `ovsdb_mgmt`.

Caller keys: **H** = separate
[`services/bgp_ad_handler`](../../services/bgp_ad_handler/main.go) route
handler; **F** = direct Framework E2E network setup/teardown in
[`framework_netlink_e2e_test.go`](../../pkg/nodeagent/framework_netlink_e2e_test.go)
and dedicated runners; **M** = exposed compatibility/manual method, no
maintained non-generated Go caller found. These are observed callers, not
access-control roles. H is **not** `cmd/evpn-ovsdb`, whose programmer uses
NBDB/OVSDB directly. Node-agent endpoints also use domain backends directly.

| RPC | Request -> response | Caller | Implementation, side effects and dependencies |
|---|---|---|---|
| AddNetwork | `AddNetworkRequest` -> `Status` | H/F | Concrete: creates/reuses NB LS, truncates name to 13 characters, sets dynamic-routing VNI/interface and redistribution options from configuration/defaults; updates local cache. Not full kernel device provisioning. |
| GetNetwork | `GetNetworkRequest` -> `GetNetworkResponse` | H | Concrete: cache/NB lookup and cache update; empty network on read failure/absence. |
| GetNetworks | `Empty` -> `GetNetworksResponse` | F | Concrete: lists NB switches and updates cache; nil collection on failure. |
| DelNetwork | `DelNetworkRequest` -> `Status` | F | Concrete: deletes named NB LS and cache entry; absent may return unsuccessful status. Controller cleanup is asynchronous. |
| AddVtep | `AddVtepRequest` -> `Status` | H | Concrete: `ls_name`, `lsp_name`, `vxlan_name`, `remote_ip`, `key`, `dst_port`; helper creates NB port and OVS VXLAN objects. Requires NB and OVS. Not the standalone provider-bridge programmer. |
| DelVtep | `DelVtepRequest` -> `Status` | H | Concrete: named LS/LSP/VXLAN removal through helper; no universal cross-database atomicity. |
| AddClient | `AddClientRequest` -> `Status` | H | Concrete: append `mac ip` to named LSP addresses. Requires NB; does not advertise BGP routes. |
| DelClient | `DelClientRequest` -> `Status` | H | Concrete: remove exact `mac ip` string from LSP addresses. Not generic endpoint or BGP deletion. |
| GetVtep | `GetVtepRequest` -> `GetVtepResponse` | M | Concrete: NB LSP and associated OVS VXLAN lookup; **can write OVS Interface external_ids iface-id** as an auto-fix. Not read-only; write failures may only be logged. |
| GetVteps | `Empty` -> `GetVtepsResponse` | M | Concrete: helper enumerates VTEP LSPs; empty/nil collection on failure. |
| AddLogicalSwitch | `LogicalSwitch` -> `Status` | M | **Unimplemented:** generated codes.Unimplemented fallback. |
| GetLogicalSwitch | `LogicalSwitchRequest` -> `LogicalSwitchResponse` | M | **Unimplemented.** |
| DelLogicalSwitch | `LogicalSwitchRequest` -> `Status` | M | **Unimplemented.** |
| GetLogicalSwitches | `Empty` -> `LogicalSwitchesList` | M | **Unimplemented.** |
| AddLogicalSwitchPort | `LogicalSwitchPort` -> `Status` | H | Concrete: NB LSP addresses/LS association and OVS br-int attachment; depends on configured local identity/interface conventions. Not the ownership-aware EndpointManager API. |
| GetLogicalSwitchPort | `LogicalSwitchPortRequest` -> `LogicalSwitchPortResponse` | H | Concrete: NB lookup by name; nil port on missing/backend error. |
| DelLogicalSwitchPort | `LogicalSwitchPortRequest` -> `Status` | H | Concrete: deletes NB row and attempts OVS port deletion; some transaction errors are ignored. Success does not prove complete cleanup. |

Default NB endpoint is `tcp:127.0.0.1:6641`; default OVSDB is
`unix:/var/run/openvswitch/db.sock`. NB connection is required at startup;
OVS connection or NB monitor failure can be logged without preventing the
server from serving. Methods that need the OVS client are not thereby safe
or ready when that client is unavailable.

Most failures use `Status.success/message` with gRPC OK, while read methods
may return empty data after backend failure. There is no cross-OVN/OVS/kernel
transaction or general endpoint identity/idempotency guarantee across this
surface. Do not use empty reads as proof of route absence or delete foreign
objects to compensate. See [API-01](../roadmap/backlog.md) and the separate
[owned integration contract](../architecture/integrations.md).
