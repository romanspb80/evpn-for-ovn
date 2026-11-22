# Kubernetes secondary-CNI solution

This experimental solution attaches secondary Pod networks to a project-owned
OVN domain. Multus invokes the project CNI; the Node-Agent authenticates and
processes the local request. It is separate from the cluster's primary CNI and
is not a production installation recipe.

## Responsibilities

- **Multus** invokes the secondary CNI plugin.
- **The project CNI and Node-Agent** coordinate ADD/CHECK/DEL and exact
  endpoint identity over a node-local Unix socket.
- **OVN/OVS** owns network state and forwards workload packets.
- **GoBGP**, when explicitly configured, handles BGP/EVPN Type-2 control-plane
  state. EVPN is optional for connectivity inside a shared OVN Geneve domain.

In real OVN/OVS plus Linux-veth mode, the connected CNI Unix peer must be
root:root and pass `SO_PEERCRED`/`SO_PEERPIDFD` validation even when optional
continuity is disabled. Continuity adds process, CRI workload and attachment
identity checks. See [CNI creation authority](../../architecture/cni-creation-authority.md)
and [endpoint continuity](../../architecture/cni-endpoint-continuity.md).

## Lifecycle and configuration

- ADD creates or verifies only the authorized attachment and owned state.
- CHECK validates current identities and resources; it does not recreate
  missing resources.
- DEL removes only exact owned state and must not remove a replacement or
  foreign resource.
- Missing or ambiguous authority fails closed. Missed-DEL/orphan cleanup and
  Kubernetes API-unavailability recovery have incomplete operational acceptance.

The sample NetworkAttachmentDefinition is in
[`deploy/kind/cni-multus-test`](../../../deploy/kind/cni-multus-test/).
Replace every placeholder and use only a dedicated disposable cluster. The
Operator and privileged Node-Agent require careful trust and RBAC containment;
production privileges, upgrades and rollback are not defined.

## Testing methodology

The [testing guide](../../../TESTING_GUIDE.md) describes CNI, external
EVPN and restart scenarios and their limitations. Shared-OVN Geneve evidence
is not EVPN/VXLAN evidence; BGP route state is not packet-forwarding evidence.
