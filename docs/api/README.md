# Framework API reference

The checked-in Protocol Buffer definitions and generated descriptors define
wire names and field numbers. These APIs are experimental; a declared or
implemented method is not a production-support or end-to-end acceptance claim.

| Service | Purpose | Security boundary |
|---|---|---|
| `node_agent_cni.NodeAgentCNI` | Node-local CNI endpoint lifecycle | Unix socket permissions plus mandatory root:root `SO_PEERCRED` and `SO_PEERPIDFD` in real OVN/OVS plus Linux-veth mode. Continuity adds further caller/workload checks. |
| `evpn.EvpnService` | Legacy EVPN management compatibility API | TCP gRPC without TLS, authentication or authorization. Not deployed in the supported Operator Netlink profile. |
| `ovsdb_mgmt.OvsdbService` | Legacy OVN/OVS maintenance API | TCP gRPC without TLS, authentication or authorization; some operations mutate OVN/OVS. Not deployed in the supported Operator Netlink profile. |

The legacy Kind compatibility manifest exposes its management services only
through ClusterIP Services. ClusterIP does not authenticate permitted
in-cluster callers. Do not deploy that profile on a shared or untrusted
cluster.

Framework route operations use upstream GoBGP APIs for Type-2 path
observation, advertisement and exact path withdrawal. Those are not methods of
the legacy management APIs. An external provider remains the sole authority
for managed speaker and peer configuration. Direct OVSDB programming is
maintenance/test-only for this candidate.

See the individual [EVPN](evpn-mgmt.md), [Node-Agent CNI](node-agent.md) and
[OVN/OVS maintenance](ovsdb-mgmt.md) references, plus the [security contract](../security/netlink-deployment-security-contract.md).
