# Remote2 fixture lifecycle contract

Remote2 is test infrastructure, not an EVPN Core runtime or authorization API.
These rules do not accept the external-fabric isolation/lifecycle matrix,
foreign-preservation cases, final teardown acceptance, or any live restart
scenario.

## Identity and ownership

The root-protected `remote2.json` is immutable for a recovery installation.
Its **entire original byte sequence**, including the initial endpoint identity
and incarnation, remains the recovery configuration digest. Installation/node
UUID, boot, network namespace, socket/attestation paths, endpoint template,
import policy and complete kernel mapping remain bound to that domain.
Changing any of them requires a separate, explicitly drained installation;
there is no migration or implicit journal reset.

Endpoint lifecycle changes use `endpoint.json` in the protected state directory,
not edits to `remote2.json`. This document contains `Generation` (starting at 1),
`Incarnation` (a canonical lowercase nonzero UUID), and `Retired`. The initial generation is seeded
from the immutable configuration. Later generations must increment by exactly
one, use a never-used incarnation, and follow a durably retired, fully drained
generation. The endpoint ID and all route/resource mappings remain fixed.

The helper holds an additional `ownership.Journal` at `lifecycle` for its entire
lifetime. Its payload records endpoint generation, used incarnations and phase:
`active`, `retiring`, `retired`. This is a new test-only journal consumer, not a
production journal schema change. Pending lifecycle intents block reopening.
Missing or corrupt metadata never initializes an existing journal. A bootstrap
record also prevents missing export/import journals from becoming fresh ownership
after a completed initialization. Old pre-contract installations are not migrated.

Explicit retirement is either `Retired:true` for the current generation or the
legacy exact `EndpointID/Incarnation\n` `retire` marker. The helper durably records
`retiring` **before** publishing authoritative empty desired endpoints, without
requiring the retired netns to exist. Only successful receipt-owned export
reconciliation with no applied routes acknowledges `retired`. Missing netns,
device or IP while active is uncertainty, never authorization to delete.
Removing a retirement request cannot resurrect that incarnation. Recreation is
validated against the durable generation ledger before actual endpoint observation.

Export ownership remains exclusively the production acknowledged UUID receipts
bound to the actual speaker incarnation. The lifecycle ledger cannot confer route
ownership. Importer receipts belong to the independently locked `import` journal,
not the local endpoint generation. Local endpoint retirement/recreation never
clears importer receipts, quarantines or desired remote B state. Imported state is
withdrawn only through fresh complete authoritative BGP observations and existing
receipt-owned Netlink reconciliation.

## Process changes

Projector-only restart retains the speaker, session, immutable configuration and
original lifecycle/export/import journals. Kernel device/netns identity is
revalidated by the production backend. Restored snapshots are non-authoritative
until fresh complete stable observations. Foreign slots remain quarantined;
global pending intents, corrupt metadata and identity changes remain blockers.

Speaker-only replacement retains the projector and all journal locks. The old
connection and guard are closed before a new transport is constructed. Production
Unix-peer attestation checks the actual new process, executable/arguments/cgroup/
configuration/identity pins and readiness. Only then are export recovery and
import observations rebound with existing APIs. No old PID/pidfd or UUID authority
is transferred. Export recovery still verifies authoritative absence/conflicts;
the importer requires two fresh complete stable observations after rebinding.
Failed attestation leaves mutations frozen and retryable; it is not an empty RIB.

Combined restart is a distinct startup case: stop the old projector (release its
locks), replace the speaker, then start a new projector on the original journals.
Both process identities must change. The new actual peer is attested before
recovery; old-incarnation receipts are handled only by production recovery.
Independent restart tests do not establish live combined acceptance.
Status includes the actual projector PID and `/proc/self/stat` start ticks,
separately from attested speaker evidence.

Shutdown closes transport, guards, backend and journals, including partial startup.
It is not a route drain. Later live acceptance must explicitly retire/drain and
verify exact RIB/receipt/kernel absence before speaker destruction.

The existing A2 fixture restore publishes the next generation only after matching
`Lifecycle.Phase=retired` status. It never rewrites the immutable configuration.
This keeps A2 compatible with irreversible retirement; it does not implement full
external F2 recreation, E1/E2, F4 or H1 acceptance.

## Offline validation boundaries

Ordinary package tests exercise observation and fixture contracts. Root-owned
offline test binaries additionally exercise real durable journals, flock fencing,
corruption/pending-intent rejection, exact export UUID recovery with modeled RIBs,
and real Unix-peer subprocess replacement with production standalone attestation.
The combined composition test separately reopens the original export journal
against a replacement speaker; it is not inferred from the independent cases.
A separate subprocess test kills both offline projector-ownership and Unix-peer
processes, then reopens the original lifecycle/export journals and attests the
replacement peer, checking both process identities change. This bounded harness
does not construct the kernel backend or claim full projector runtime acceptance.
Core `TestImporterRecoveryPreservesForeignSlotsAndRestoresOwnedSlots` and
`TestImporterRecoveryQuarantinesConflictAndRestoresIndependentRoute` exercise the
unchanged receipt-owned backend against its memory kernel. These tests are not
live Netlink or multi-node acceptance.

Run the required Go test/vet/race commands and existing Python fixture tests.
Because production ownership journals require UID 0, also compile the Remote2
test binary, install it root-owned into a private executable directory, and run
it as root without starting any topology. Real-kernel and multi-node validation
remain separate, explicitly authorized future gates.

## Bounded live R3 runner

With explicit privileged-lab authorization, run:

```
PYTHONDONTWRITEBYTECODE=1 GOPROXY=off python3 tools/e2e/remote2_restart.py \
  --directory /tmp/remote2-evidence --cluster remote2-evidence
```

The runner reuses the canonical three-domain topology, coordinator lease, pinned
images and packet-capture helpers. It reports projector-only, speaker-only and
combined restarts separately, with actual identities, immutable journal domains,
lock/marker continuity, exact UUID drainage, independent importer history,
foreign sentinels and bidirectional packet attribution.

For speaker-only replacement, after proving rejection of the exited Unix peer,
the still-alive projector is briefly SIGSTOPed to observe the new speaker's RIB
before re-origination, then SIGCONTed for actual automatic rebind/recovery.
This explicit observation pause is retained in the timing evidence; reported
times are test observations, not convergence SLAs.

Two complete clean-room passes with identical final source inputs are required.
The runner does not execute or accept E1/E2 or full F2/F4/H1.
