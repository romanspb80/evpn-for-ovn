# Upstream integration and contribution boundaries

| Topic | Current evidence class |
|---|---|
| [OVN-Kubernetes](ovn-kubernetes.md) | Research question / proposed integration; upstream APIs are FRR-specific, not interchangeable with k8gobgp. |
| [OVN/OVS](ovn-ovs.md) | Reproduced local crash, suspected transient error, expected reconciliation and local FDB limitation; not root-cause-confirmed upstream fixes. |
| [k8gobgp](k8gobgp.md) | Verified configuration ownership; proposed documentation/API/recovery improvements. |
| [GoBGP / gobgp-netlink](gobgp.md) | Verified Type-2 API usage and compatibility constraints; no demonstrated general EVPN absence. |
| [Remote BGP peer security](bgp-peer-security.md) | Existing MD5 Secret references; AO/GTSM/ACL/policy/rotation interoperability remains unvalidated. Separate from speaker attestation and endpoint authorization. |
| [Cilium](cilium.md) | No concrete current integration or contribution found. |

Use the shared five [evidence classifications](../../known-issues/ovn-ovs-issues.md#classification).
An existing issue/PR is a reference only after its relevance is verified.
No existing matching public issue/PR was verified in this milestone; no
submission, status change or upstream modification was performed.
Source/documentation links are references, not issue evidence.

Before any submission, search the target project's issues and PRs, minimize
and sanitize the reproducer, establish exact source/runtime versions and
single-writer ownership, and separate a desired feature from an actual bug.
No issue search performed here establishes that no related upstream issue
exists. G.8 performed preliminary OVN/GoBGP issue searches, not complete
duplicate review. All prepared drafts and readiness limits are indexed in
[docs/issues](../../issues/README.md).
Canonical work is [UP-01, UP-02, SEC-01, FW-02/FW-05 and INT-03](../backlog.md).
