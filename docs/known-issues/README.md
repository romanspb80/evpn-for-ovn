# Known limitations and troubleshooting

This page is a practical summary of the experimental candidate's known limits,
not an upstream issue tracker or a promise of production support.

Use [engineering findings](engineering-findings.md) for verified compatibility
boundaries and unresolved local investigations, and
[Upstream Contributions](../project/upstream-contributions.md)
for accepted project-author work. Historical evidence has a separate
[validation-history archive](../internal/validation-history/README.md).

## Known limitations

- **Management RPCs:** legacy `evpn-mgmt` and `ovsdb-mgmt` lack transport
  authentication and authorization. The legacy Kind profile uses ClusterIP,
  which does not authenticate permitted in-cluster callers. The supported
  Operator Netlink profile excludes these services.
- **CNI recovery:** root:root peer authentication is mandatory in real
  OVN/OVS plus Linux-veth mode. Continuity provides additional identity and
  lifecycle checks. Missed-DEL/orphan recovery and Kubernetes API outages
  have incomplete operational acceptance.
- **Foreign resources:** receipt-owned withdrawal must preserve unrelated
  state, but arbitrary pre-existing production resources are outside the
  release guarantees. The secondary-network runner has no independent
  foreign-state witness. A witness extension must establish separate
  ownership and compare its exact identity before and after CNI deletion.
- **OVSDB path:** direct OVSDB programming is maintenance/test-only for this
  candidate. Advanced restart and withdrawal scenarios are deferred.
- **Deployment:** the Operator and examples are experimental; production
  privileges, RBAC, upgrades, rollback and supported-platform policy are not
  established.
- **Pinned speaker startup:** upstream k8gobgp v0.2.8 has a reported stale
  Unix-socket failure after an unclean stop with persistent socket storage.
  The later upstream fix is not in the selected input. Pod replacement with
  fresh emptyDir is an upstream-reported recovery, not a general recipe for
  this project's host-mounted socket fixtures; see the
  [provider startup evidence](engineering-findings.md#pinned-provider-startup).
  Investigate exact storage/process
  ownership and reattestation rather than unlinking a potentially live socket.

## Safe troubleshooting

1. Confirm the selected integration, source/image identity and test scope.
2. Check Node-Agent/Operator status, GoBGP peer state, route snapshots, OVN
   bindings and OVS port/flow state without mutating them.
3. Distinguish control-plane evidence from packet-forwarding evidence. Use
   packet captures on the actual underlay to distinguish OVN Geneve from
   external EVPN VXLAN traffic.
4. On incomplete observations, authorization errors, pending journal intents
   or ownership conflicts, stop and preserve evidence. Do not delete routes,
   interfaces, journals or foreign database objects to force convergence.
5. For live tests, use the [isolated lab and cleanup procedure](../../deploy/containerlab/README.md).

See [release scope](../release/v0.1.0.md),
[security guidance](../../SECURITY.md), and [testing procedures](../../TESTING_GUIDE.md).
