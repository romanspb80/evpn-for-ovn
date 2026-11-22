# Testing documentation

For developers, authorized lab operators and evidence reviewers. Test
procedures, implementation contracts and historical results are different
kinds of documentation; a runnable command is not a passing release result.

| Document | Purpose and scope |
|---|---|
| [Development and testing guide](../../TESTING_GUIDE.md) | Primary current entry point: offline checks, all nine technical live scenarios, execution commands, evidence interpretation and exact cleanup. |
| [Disposable VM laboratory](vm-e2e-lab.md) | Environment preparation, immutable source/image/binary inputs, guest execution, collection and ownership-aware cleanup. Contains distinct workflow variants, not one combined validated deployment. |
| [Lifecycle scenario map](lifecycle-scenarios.md) | Maps the nine scenarios to implementation entry points, topology/recovery diagrams, procedures and fixture/acceptance gaps. Does not replace the Testing Guide or detailed architecture contracts. |
| [Containerlab guide](../../deploy/containerlab/README.md) | Fixture coordination, profiles, isolation, resource receipts and cleanup beneath the authorized environment. Containers alone do not replace guest-kernel isolation. |
| [Validation-history archive](../internal/validation-history/README.md) | Canonical maintainer index for historical outcomes, acceptance evolution and evidence provenance; not a current test procedure. |
| [External EVPN fabric evidence](../internal/validation-history/external-evpn-fabric.md) | Historical engineering report with per-attempt scope, failures and full-fabric acceptance limits. |

The [validated two-chassis and experimental external-fabric use cases](../use-cases/README.md) explain why a user
would exercise these paths; linked deployment, topology and lifecycle guides
describe supporting workflows. The [diagram index](../uml/README.md) distinguishes
current contracts, historical topologies and proposed designs.

## Current tests and acceptance gaps

Use the current Testing Guide and the exact source for the selected scenario.
The lifecycle map identifies missing independent foreign-state witnesses,
root-transport skips, incomplete missed-DEL/API-outage fixtures and
unqualified generalized multi-worker recovery. Direct OVSDB procedures remain
maintenance/testing-only, outside the selected Netlink deployment.
No historical PASS, successful ADD, route observation or Ready condition
fills those gaps. Keep scenario-specific source/build provenance, route and
packet evidence, exact cleanup and actual PASS/FAIL/BLOCKED/SKIP statuses.

Follow [security prerequisites](../../SECURITY.md) before any live workflow.
No test or infrastructure was executed merely to organize these documents.
