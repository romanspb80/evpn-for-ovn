# Development and testing guide

This guide explains offline checks and live test profiles: their objectives,
required environments, execution, evidence interpretation and cleanup.
Offline checks cannot establish live BGP exchange or workload forwarding.

The [testing documentation index](docs/testing/README.md) distinguishes
current procedures, scenario mappings and historical experimental evidence.

## Safety prerequisites

Live tests create privileged disposable Containerlab/Kind, OVN/OVS, BGP peer
and workload resources. Use only a dedicated isolated Linux test host and an
exactly owned run directory. Review [security boundaries](SECURITY.md), the
[Containerlab lifecycle and cleanup rules](deploy/containerlab/README.md), and
[VM E2E preparation](docs/testing/vm-e2e-lab.md). Do not use shared or
production clusters. Stop on ownership conflict, incomplete evidence or
cleanup failure.

The supported Operator Netlink profile excludes legacy unauthenticated
management RPC services. Do not use the legacy Kind compatibility manifest as
a release deployment. The benchmark-only provenance bypass is not permitted
in supported secure profiles or acceptance runs.

## Offline development checks

From the repository root:

```bash
go test ./...
go build ./cmd/evpn-cni ./cmd/evpn-node-agent ./cmd/evpn-ovsdb \
  ./services/evpn_mgmt ./services/ovsdb_mgmt ./services/bgp_ad_handler \
  ./deploy/operator
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/lib -p 'test_*.py'
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/e2e -p 'test_*.py'
```

Run the Python tool-harness unit tests separately:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/e2e -p 'test_*.py'
```

Some root-only or host-kernel tests are expected to skip in an unprivileged
local run. A skip is not a pass; run those only in a separately authorized,
disposable root-capable environment. The test that exercises real root:root
CNI transport must run as root in the isolated lab, never directly on a
workstation.

## Live test scenarios and expected behavior

Each scenario checks a specific boundary. A route in a RIB, a Ready pod or an
offline check alone is not proof of workload forwarding or safe recovery.

| Scenario | Objective and expected behavior | Topology / sequence diagram |
|---|---|---|
| Secondary CNI ADD/CHECK/DEL over OVN Geneve | Check attachment lifecycle, bidirectional packets and preservation of the primary CNI. The runner has no independent foreign-state witness; testing that boundary requires a separately owned sentinel and explicit identity comparisons. | [Shared-OVN topology](docs/uml/multinode-secondary-external-domains.svg), solid Geneve path only; [CNI lifecycle](docs/uml/evpn-cni-lifecycle-sequence.svg) also illustrates a separate optional external Type-2 test. [Scope and procedure](docs/testing/lifecycle-scenarios.md#secondary-cni-and-external-type-2). |
| External GoBGP/FRR EVPN Type-2 with Netlink integration | Check single-chassis Operator route exchange, bidirectional OVN/OVS VXLAN traffic, exact owned-route withdrawal and survivor preservation. | [Netlink Type-2 path](docs/uml/netlink-programmed-type2.svg); [Framework sequence](docs/uml/evpn-framework-netlink-e2e-sequence.svg) is the non-CNI variant, not the Operator CNI fixture. [Scope and procedure](docs/testing/lifecycle-scenarios.md#secondary-cni-and-external-type-2). |
| Node-Agent restart and recovery | Restart the agent in an owned fixture; check durable state, endpoint/path ownership, subsequent packets and cleanup rather than readiness alone. | [Agent restart sequence](docs/uml/node-agent-restart-sequence.svg); [Netlink topology](docs/uml/netlink-programmed-type2.svg). [Procedure](docs/testing/lifecycle-scenarios.md#agent-and-speaker-restart). |
| GoBGP speaker restart and recovery | Restart the speaker; check speaker identity, route re-advertisement, subsequent packets, exact withdrawal and surviving routes. | [Speaker restart sequence](docs/uml/gobgp-speaker-restart-sequence.svg); [Netlink topology](docs/uml/netlink-programmed-type2.svg). [Procedure](docs/testing/lifecycle-scenarios.md#agent-and-speaker-restart). |
| Foreign-state ownership witness | An extension can create an independently owned empty OVN switch, compare UUID/name/external IDs/ports through ADD/CHECK/DEL, and explicitly remove only that sentinel afterward. This is not built into the secondary-CNI runner or a substitute for arbitrary foreign-state testing. | No dedicated sentinel diagram; [exact textual specification and fixture gap](docs/testing/lifecycle-scenarios.md#foreign-state-witness). |
| Root CNI transport | Check connected root:root caller authentication and peer exit handling using the actual Unix transport and required kernel facilities. Skipped root tests leave that boundary unchecked. | No dedicated transport diagram; [authentication contract and exact test procedure](docs/testing/lifecycle-scenarios.md#root-cni-transport). |
| Multi-worker / multi-VTEP lifecycle | Use the linked two-worker and external-fabric guides to inspect worker-local inventory, external domains, ownership, restart and route replacement. Keep each scenario's actual scope separate from generalized operational support. | [Historical Attempt 8 topology](docs/uml/operator-inventory-type2-two-worker.svg), not lifecycle acceptance; [proposed external fabric](docs/uml/external-evpn-fabric.svg), not executed acceptance. [Scope and procedures](docs/testing/lifecycle-scenarios.md#multi-worker-and-multi-vtep). |
| Orphan and API-unavailability recovery | Exercise the relevant recovery/failure-injection cases in the harnesses, compare durable ownership and state before/after interruption, and inspect cleanup. Operational acceptance remains incomplete; implemented paths are not production guarantees. | [Orphan / API-outage sequence](docs/uml/cni-orphan-api-recovery-sequence.svg), implemented contract only; [operational fixture gap](docs/testing/lifecycle-scenarios.md#orphan-and-api-unavailability). |
| Direct OVSDB restart/withdrawal | Retain these maintenance test scenarios separately from the selected Netlink deployment profile; advanced operational qualification is deferred. | [Programming-mode comparison](docs/uml/evpn-dataplane-comparison.svg); no dedicated OVSDB lifecycle sequence. [Maintenance procedure and gap](docs/testing/lifecycle-scenarios.md#direct-ovsdb-maintenance). |

Diagrams explain topology or lifecycle contracts, not validation evidence.
See the [scenario notes](docs/testing/lifecycle-scenarios.md) for source entry
points, diagram qualifications and the gaps that require separate fixtures.

Broad multi-node lifecycle, arbitrary foreign-state preservation, production
upgrade/rollback and service availability are outside the release guarantees.
Neither integration has a comparative performance or superiority claim.

For orphan and API-unavailability cases, keep distinct failure injections:
withhold DEL after independently confirming Pod retirement, and separately
make the Kubernetes API unavailable. Retirement must use the exact receipt;
API errors must not authorize withdrawal or cleanup. Compare journal intents,
owned UUIDs and foreign survivors before interruption and after recovery.
These scenarios require their own fixture setup and cannot be inferred from
the agent or speaker restart profiles below.

## Prepare and execute the scoped scenarios

Run these commands **inside an authorized disposable Linux guest**, after
completing the [VM prerequisites](docs/testing/vm-e2e-lab.md). Check the
documented kernel requirements, connected Unix peer-identity facilities,
loaded `openvswitch` module, exact image references, Go toolchain and
Containerlab/Kind inputs first. The Operator runner requires
a real clean Git checkout, not only an extracted source archive. It derives
the worker containerd version and builds `ctr` from checksum-verified module
sources; do not substitute a copied executable.

First run the connected Unix CNI transport tests as root:root in the guest:

```bash
sudo sh -c 'cd "$1"; CGO_ENABLED=0 go test -mod=readonly -count=1 -json \
  ./pkg/nodeagent -run "^(TestCNICallerActualGRPCTransport|TestCNICallerPeerExit)$"' \
  sh "$PWD"
```

Ensure root's `PATH` resolves the pinned Go toolchain. Both named tests must
pass without skips before running the dependent scenarios.

The baseline runner accepts the profile identifiers shown below. They are
compatibility selectors, not general deployment profiles. Run serially and
verify `result.json` and complete owned-fixture cleanup before continuing:

```bash
# Kubernetes secondary-network connectivity (runner has no extra witness).
bash tools/run-cni-isolated-secondary-e2e.sh

# Pod-to-external EVPN/VXLAN connectivity.
EVPN_OPERATOR_TYPE2_PROFILE=uc02 \
EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT="$HOME/evpn-evidence/uc02" \
bash tools/run-operator-type2-e2e.sh

# Node-Agent restart recovery.
EVPN_OPERATOR_TYPE2_PROFILE=uc06-agent-restart \
EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH=0 \
EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT="$HOME/evpn-evidence/agent-recovery" \
bash tools/run-operator-type2-e2e.sh

# GoBGP speaker restart recovery.
EVPN_OPERATOR_TYPE2_PROFILE=uc06-speaker-restart \
EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH=0 \
EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT="$HOME/evpn-evidence/speaker-recovery" \
bash tools/run-operator-type2-e2e.sh
```

The baseline runner recognizes `uc02`, `uc06-agent-restart` and
`uc06-speaker-restart`; it does not accept the later descriptive aliases or
`--help` option. Scoped profiles cannot combine with the alternate
smoke/Geneve modes. `EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH` is allowed only
for `uc06-agent-restart` or `full`.

Do not treat the default `full` profile as equivalent to the narrower scenarios.
Inspect a scoped profile without creating infrastructure by adding
`--list-cases`. Keep separate evidence roots; retain packet captures, RIB
snapshots and path UUIDs, OVN/OVS state and counters, ownership receipts,
attestation records and cleanup results. Never delete the VM or its storage
as part of routine fixture cleanup.

## Inspect results and clean up

For each run, inspect `result.json` and individual case statuses, not only the
runner's exit code. A skipped, blocked or unexecuted case leaves its objective
unchecked. Missing packet evidence or incomplete cleanup must not be treated
as successful acceptance.

Correlate workload packets with the intended Geneve or VXLAN tunnel and OVS
flow/counter changes. Inspect RIB snapshots and path UUIDs alongside OVN/OVS
and Linux state. For withdrawal or replacement, compare exact owned endpoint
and route identities while checking that surviving routes remain intact.
For restart scenarios, retain before/after durable state, caller/provider
identity and speaker attestation records; readiness alone is insufficient.

Run scenarios serially in separate evidence directories. Allow each runner's
owned cleanup to complete and inspect its cleanup case and retained receipts.
On ownership conflict or failed cleanup, preserve diagnostics and resources
for investigation; do not use global destroy/prune or remove a VM/storage
volume to hide a failed fixture teardown. Follow the
[Containerlab cleanup procedure](deploy/containerlab/README.md).

## Image transfer and exact digest verification

A Docker image archive preserves image layers and tags, but `docker save` /
`docker load` does not reliably restore the registry `RepoDigest` alias. When a
live fixture requires a pinned registry digest, transfer/load the archive,
then pull that exact `repository@sha256:...` reference from its registry and
verify both the exact RepoDigest and expected image config ID before running.
Do not substitute a tag, a different digest, or a matching config ID alone.
The VM lab helper pulls and verifies the approved pinned image used by its
fixture; it does not accept caller-supplied image pins. See [VM E2E
preparation](docs/testing/vm-e2e-lab.md).

## Useful test documentation

- [User-facing networking goals and their verification boundaries](docs/use-cases/README.md)
- [Containerlab profiles, interoperability and cleanup](deploy/containerlab/README.md)
- [Isolated VM lab](docs/testing/vm-e2e-lab.md)
- [External EVPN fabric historical record](docs/internal/validation-history/external-evpn-fabric.md)
- [Type-2 fixture](tests/e2e/type2-fixture/README.md)
- [Netlink acceptance fixture](tests/e2e/netlink-acceptance/README.md)
- [Two-worker Operator example](deploy/operator/examples/two-worker/README.md)
- [Remote Type-2 generation, replacement and recovery scenarios](tools/e2e/remote2/README.md)

The external-fabric document is explicitly a historical engineering record,
preserved unchanged alongside its scenario specifications. Historical outcomes
there are not current release qualifications; use each scenario's prerequisites
and collect independent evidence for the source and environment under test.

Only run live profiles after establishing ownership, isolation, pinned inputs
and scoped cleanup. Preserve the test's actual status and scope; do not infer a
pass from deployment readiness or a route alone.
