# Two-worker Type-2 example

This directory contains an experimental two-worker configuration template and
validation procedure. It is not a verified deployment profile or a production
installation recipe. Do not apply files containing placeholders.

The intended topology uses one shared OVN NB/SB domain, independent node-local
GoBGP speakers and Node-Agents, and an external FRR peer. OVN/OVS forwards
workload traffic. Shared-domain Geneve traffic is distinct from external
EVPN/VXLAN traffic. BGP route presence alone is not forwarding evidence.

The template is not a complete installer or a generalized multi-worker
recovery guarantee. See the [release scope](../../../../docs/release/v0.1.0.md) and
[validation procedure](VALIDATION.md).

Before considering this template in an authorized disposable environment:

1. Review [security requirements](../../../../SECURITY.md) and the
   [Operator configuration guide](../../../../docs/architecture/evpn-core-operator.md).
2. Replace every sample address, node name, identity and resource name with
   values from the dedicated test environment. Verify route reachability and
   non-overlap before applying anything.
3. Establish unique per-node speaker, VTEP and journal identities; protect
   speaker credentials, authorization state and host-level files.
4. Follow [VALIDATION.md](VALIDATION.md) and stop on any identity mismatch,
   pending intent, ownership conflict or cleanup failure.

This configuration is for testing only. General multi-worker lifecycle,
restart, withdrawal, recovery and production operations are not established.
