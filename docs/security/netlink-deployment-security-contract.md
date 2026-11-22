# Netlink deployment security contract

The supported Netlink profile is experimental. Service mesh is neither
required nor a substitute for Unix peer authentication, endpoint authorization
or exact resource ownership.

## Kubernetes Operator profile

- The real OVN/OVS plus Linux-veth Node-Agent requires root:root Unix
  `SO_PEERCRED` and kernel `SO_PEERPIDFD` for its connected CNI peer,
  independently of optional continuity configuration.
- CNI continuity adds stricter caller/process, CRI workload and attachment
  identity checks. Kubernetes RBAC does not authenticate arbitrary gRPC
  clients.
- The Node-Agent is privileged and has host networking/PID and CNI/OVN access.
  The Operator also has broad permissions. Limit configuration authority and
  use only trusted, isolated nodes.
- The supported Operator Netlink profile excludes legacy `evpn-mgmt`,
  `ovsdb-mgmt` and `bgp_ad_handler` services.
- The legacy Kind compatibility manifest remains unauthenticated to permitted
  in-cluster callers. ClusterIP-only exposure is not authentication or tenant
  isolation.
- Only current authority and exact receipts permit mutation or cleanup.
  Ambiguous observations fail closed and foreign state is preserved.

## Standalone profile

Standalone export requires protected administrator policy, durable revision
fencing, OVN endpoint identity checks and actual GoBGP speaker attestation.
Remote import validates complete stable route snapshots and manages only
receipt-owned Netlink state. Unavailable or inconsistent authority does not
authorize speculative mutation or withdrawal.

## Bypass and operational limits

The Netlink provenance bypass is benchmark-only and excluded from supported
secure profiles and acceptance runs. Never enable it for operational or
release use. It does not replace the required peer authentication, endpoint
authorization, speaker attestation or ownership checks.

Protect BGP reachability independently. TCP MD5 is not encryption. Keep
management APIs and OVN databases on trusted networks; the legacy RPCs have no
transport authentication or authorization. Do not treat network placement,
ClusterIP or a service mesh as a substitute for application-level controls.

See [SECURITY.md](../../SECURITY.md), [CNI continuity](../architecture/cni-endpoint-continuity.md),
[standalone authorization](../architecture/standalone-endpoint-authorization.md),
and [known limitations](../known-issues/README.md).
