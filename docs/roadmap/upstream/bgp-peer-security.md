# Remote BGP peer security

Canonical work: [SEC-01](../backlog.md#sec-01), configuration ownership:
[FW-02](../backlog.md#fw-02). This is a capability/acceptance plan, not an
exploitable-vulnerability review or a claim that authenticated E2E passed.
The three independent boundaries are described in [SECURITY](../../../SECURITY.md#three-independent-evpn-security-boundaries):
remote peer transport, local speaker attestation and endpoint authorization.

## Pinned capabilities and interoperability

Reviewed 2026-11-08. The framework client is osrg/gobgp v4.5.0; the managed
daemon is `PureLB-fork:01.03.08`, commit `6b99046`, from the pinned k8gobgp
v0.2.8 image. Its controller selects `purelb/gobgp-netlink/v4 v4.900.8`.
These versions must not be collapsed into one GoBGP version.

| Control | Source evidence | Acceptance / limitation |
|---|---|---|
| TCP MD5 | k8gobgp v0.2.8 neighbor and peer-group `authPasswordSecretRef` resolve into GoBGP auth fields; daemon [configuration](https://github.com/purelb/gobgp-netlink/blob/v4.900.8/docs/sources/configuration.md) documents `auth-password`. [FRR 10.7 BGP manual](https://docs.frrouting.org/en/stable-10.7/bgp.html) documents `neighbor PEER password PASSWORD` as TCP MD5. | Capability exists; matching/mismatching/missing key EVPN GoBGP-to-FRR tests remain unrun. Authentication/integrity, not encryption. |
| TCP-AO | Current osrg/gobgp source has [AO socket/key support](https://github.com/osrg/gobgp/blob/fb62e885293b8f8dcec13584e3a9156c25d31323/internal/pkg/netutils/tcp_ao.go). | Not proof of support in the selected daemon/fork/CRD or FRR fixture. Verify release, Linux kernel/config/socket options, key IDs/algorithms and both peers. No AO interoperability or safe fallback accepted. |
| GTSM | Pinned daemon configuration documents `ttl-security.config`, mutually exclusive with eBGP multihop. FRR documents `neighbor PEER ttl-security hops NUMBER`. | Validate selected k8gobgp schema/writer mapping and hop semantics first; no CRD GTSM support claim. Test expected TTL, off-path rejection and multihop incompatibility. Not a substitute for authentication. |
| ACL / firewall | Host/network restrictions can limit TCP/179 and management/API reachability independently of BGP software. | Allow only approved peer IPs/interfaces; verify IPv4/IPv6 denial and intended connection/reconnect. Operator readiness and a ClusterIP do not prove isolation. |
| Import/export policy | Existing provider policy/defined-set and AF configuration is owned by k8gobgp; framework independently filters Type-2 network/RT/VNI/next-hop scope. | Test deny-by-default route-family and RD/RT/next-hop policy, peer limits and no unintended connected-route redistribution. Remote policy cannot grant endpoint authorization. |
| Secrets / rotation | Existing Secret references avoid inline CR credentials; exact-key lookup is implemented. | Least-privilege access, protected standalone config, no logs/evidence containing keys; coordinate both peers, deliberate session interruption/reconnect, rollback/refusal and restart tests. Kubernetes Secret encoding is not encryption at rest. |

## Existing Secret reference, not a new feature

k8gobgp v0.2.8 resolves to source
[`71b26cb8ea5d48762c00f764e0204ce03e9b6e46`](https://github.com/purelb/k8gobgp/tree/71b26cb8ea5d48762c00f764e0204ce03e9b6e46).
[Types](https://github.com/purelb/k8gobgp/blob/71b26cb8ea5d48762c00f764e0204ce03e9b6e46/api/v1/types.go)
already expose `AuthPasswordSecretRef` on neighbors and peer groups.
[Controller](https://github.com/purelb/k8gobgp/blob/71b26cb8ea5d48762c00f764e0204ce03e9b6e46/controllers/bgpconfiguration_controller.go)
`resolveAuthPassword` fetches the Secret **in the BGPConfiguration namespace**
and reads exactly `Secret.Data[selector.key]`. A named missing Secret/key
returns an error, not an inline-password fallback. An address key or `password`
must be explicitly selected; sample wording does not implement automatic
address/default-key fallback. Resolved values are forwarded into neighbor
`PeerConf.AuthPassword` / peer-group `PeerGroupConf.AuthPassword`.

`SetupWithManager` watches BGPConfiguration generation and Node labels, not
Secrets. Successful reconciliation returns without `RequeueAfter`.
`peerConfigEqual` deliberately does not compare the redacted password value;
`authPasswordDrifted` compares only whether authentication is set. Thus a
Secret-only nonempty-key change is not guaranteed to trigger or apply rotation,
even if another event causes reconciliation. An unrelated edit is not a
documented rotation workaround. Peer-group and restart behavior require their
own live checks.

The source-supported gap is recorded in the
[unsubmitted rotation/documentation proposal](../../issues/k8gobgp-peer-auth-rotation-proposal.md);
runtime reproduction and duplicate checks remain pending. Do not propose adding
`authPasswordSecretRef` again.

## Ownership and acceptance rules

Kubernetes configuration remains
`BGPConfiguration -> k8gobgp -> gobgpd`; the framework owns only scoped route
reconciliation and exact receipts. Do not write neighbors through evpn-mgmt
in parallel with the controller. A standalone administrator owns protected
speaker config and secrets; Node-Agent attests it, not reconciles remote peers.

Reuse existing FRR/GoBGP fixtures for SEC-01 tests, retaining local attestation,
endpoint authorization, UUID-only withdrawal and foreign-route checks during
reconnect/rotation. Never downgrade from failed authentication to unauthenticated
peering to make a test pass. G.7b.5a's Established session alone is not peer
security acceptance.
