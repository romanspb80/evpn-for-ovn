# Implementation status

[README](README.md) | [Roadmap dashboard](docs/roadmap/overview.md) |
[Canonical backlog](docs/roadmap/backlog.md) | [Testing](TESTING_GUIDE.md)

Reviewed 2026-11-08; the preserved G.7b implementation and co-hosted startup
correction are committed locally at `750c478`. This is a status/navigation
page, not another task register.

| Surface | Implemented and bounded validation | Remaining boundary |
|---|---|---|
| Type-2 domain | Platform-independent `pkg/evpn` domain/reconciliation contracts; GoBGP control plane remains distinct from OVN/OVS/Netlink programming. | No accepted Type-3, Type-5, EVPN multihoming or generalized platform adapter. |
| Kubernetes / Multus | Existing Operator, CNI and Node-Agent lifecycle; 18/18 operator-managed Type-2 regression PASS. | Workload readiness is not BGP/dataplane readiness; production upgrade/storage/auth gates remain open. |
| G.6 speaker ownership | Standalone process/executable/config/socket attestation; durable exact AddPath UUID receipts, UUID-only withdrawal and ambiguous-add recovery blocking. | Not remote BGP peer authentication; no unsafe NLRI-delete fallback. |
| G.7a importer | Explicit `--standalone-evpn-import` with `--evpn-remote-config` runs existing importer without CNI/Kubernetes/CRI startup. | No production standalone installer or two-standalone-chassis forwarding acceptance. |
| G.7b.1-3 authorization | Protected trusted endpoint records, accepted-policy/authoritative NB-SB intersection, durable revision/digest and attachment-generation fence. | Policy producer integration/rotation and broad live failure coverage remain open. |
| G.7b.4 exporter | Explicit `--standalone-evpn-export-config`; real OVN observation and existing exporter; no CNI socket. Opt-in `enableRemoteImport` co-hosting uses matching identity and separate journals; actual startup-boundary and parent-cancellation regressions passed. | No separate daemon, second speaker writer or implicit co-hosting. Live co-hosted reconciliation/forwarding remains NOT TESTED. |
| G.7b.5 lifecycle | Version-2 tombstones distinguish deletion/replacement/migration; uncertain absence is fail-closed. | Unit coverage is not live tombstone migration/deletion acceptance. |
| G.7b.5a smoke | One existing OVN chassis: authenticated local speaker, advertisement, policy revocation, exact withdrawal, agent restart, foreign preservation, CNI absence. Pinned release cache works with downloads disabled. | Not fully air-gapped image/toolchain provisioning, multi-chassis forwarding, migration, power-loss recovery or production readiness. |

The live standalone and operator bundles in the
[Testing Guide](TESTING_GUIDE.md#standalone-export-smoke-g7b5a) were accessible
and independently checked during documentation consolidation. Full Go
build/unit/race/vet and privileged startup/authorization/ownership/crash-recovery
checks passed after the co-hosted startup correction; unchanged implementation
bytes were verified before the G.7b commit. These checks do not validate live
co-hosting or replace the release gates.

Future platform lifecycle work stays in the
[canonical backlog](docs/roadmap/backlog.md): Neutron/ML2-OVN and OVN-Kubernetes
are design/integration targets. Standalone runtime support does not mean that
the proposed EvpnCore standalone deployment provider or an installer exists.
