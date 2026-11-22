# Reliability and lifecycle

Recovery and ownership are requirements of the [networking use cases](../use-cases/README.md),
not separate EVPN connectivity use cases or general availability guarantees.
The linked architecture contracts define authority; this guide explains
the workflow and its validation limits without changing those contracts.

## Recovery and ownership

**User and problem:** An SRE needs component restart or missed cleanup to
avoid stale owned routes and unsafe deletion of unrelated resources.

**Desired outcome:** Current identity and durable receipts govern permitted
reconciliation, with explicit blocked state on uncertainty; subsequent
packets, exact withdrawal and scoped survivor preservation are checked.

**Prerequisites:** Original protected journals, stable installation/node and
resource identities, current authority observations and an attested speaker.
Record before/after state; inject failures only in the dedicated fixture.

**Workflow and diagrams:** Treat these as distinct transitions:

| Transition | Contract diagram and observable boundary |
|---|---|
| Node-Agent restart | [Agent recovery](../uml/node-agent-restart-sequence.svg): verify current endpoint resources and same-speaker receipts; do not recreate resources by replaying ADD. |
| GoBGP speaker restart | [Speaker replacement](../uml/gobgp-speaker-restart-sequence.svg): attest the new process, reconcile eligibility and record fresh UUIDs; never transfer old UUID authority. |
| Missed DEL or API outage | [Orphan/API recovery](../uml/cni-orphan-api-recovery-sequence.svg): confirmed original Pod retirement and exact receipts differ from an unavailable API. |

**Maturity and limits:** Identity-gated restart paths and receipt-based orphan
logic are implemented, with bounded offline regression coverage. Diagrams
are contracts, not live evidence. Missed-DEL/API-outage operational acceptance,
including orphan-triggered Type-2 withdrawal, remains incomplete. The orphan
path never deletes host veth: unresolved host evidence is retained as
`orphan_host_unresolved`. API errors do not authorize withdrawal, adoption
or cleanup. Foreign-state protection is receipt-scoped, not a guarantee
for arbitrary state. No uninterrupted availability, convergence SLA or
generalized recovery is promised.

**Architecture and verification:** See [route ownership](../architecture/type2-restart-ownership.md)
and [CNI continuity](../architecture/cni-endpoint-continuity.md#recovery-and-acknowledgements).
The [restart procedures](../../TESTING_GUIDE.md#prepare-and-execute-the-scoped-scenarios),
[foreign-witness specification](../testing/lifecycle-scenarios.md#foreign-state-witness)
and [orphan/API fixture gap](../testing/lifecycle-scenarios.md#orphan-and-api-unavailability)
describe separate checks; restart profiles do not cover all missed events.
See [known limitations](../known-issues/README.md) for the pinned provider's
unclean-start risk; historical speaker replacement does not certify every
container restart or storage configuration.
