# Historical validation archive

This maintainer-oriented index separates recorded test outcomes from current
procedures and release claims. The [Testing Guide](../../../TESTING_GUIDE.md)
remains the entry point for current reproduction instructions. Historical
results apply only to their documented source, topology, test inputs, and
evidence; they are not interchangeable release scorecards.

## Campaign inventory

| Scenario | Milestone / source | Test inventory and topology | Recorded result | Evidence and availability |
|---|---|---|---|---|
| Standalone Type-2 export/import | Early standalone campaigns; exact tested source is not identified | Reported suites of 15 and 23 cases; non-CNI fixtures | PASS as recorded, with source/build and forwarding limitations | [Immutable `ae07988` validation record](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/validation-record.md); source record is in Git history, private run artifacts are not in this tree. |
| GoBGP-to-GoBGP Type-2, two chassis | Base source `ae07988afdd08b860eb45221ed50c01e1995769f`, tree `017ce27efa6eba344f519964184b450845b36b63`, with a test-only overlay | Two OVN/OVS namespaces, shared NB/SB, separate switches/VTEPs and directly peered speakers; bounded multi-chassis cases, not a full inventory run | Five bounded route, bidirectional VXLAN, stability, restart and cleanup cases recorded PASS; optional non-CNI lifecycle checks also passed in that attempt | [Two-chassis record](two-chassis.md); local `live-results.tar`, SHA-256 `0016964c2fc1a25d7fdebe4efa1196aa1350ff4fb8eb0a5507583d89b22df486`, is referenced but not distributed here. |
| GoBGP-to-FRR Type-2 | One-chassis and Operator campaigns; source identity varies or is missing | External FRR peer and worker-local routes; bounded fixture-specific scenarios | Selected route/forwarding and lifecycle checks passed; this is not general vendor or full-fabric acceptance | [Standalone and operator records](standalone-type2.md) and [Operator acceptance](operator-acceptance.md); source reports are linked in pinned Git history, private bundles are unavailable in this tree. |
| External EVPN/VXLAN fabric | Historical engineering campaigns; exact final-input manifest and source/build identity were not established | Shared-OVN worker scenarios with external FRR/GoBGP peers and distinct external VXLAN dataplanes | Some bounded checks passed; full-fabric acceptance remains unresolved | [Detailed archived record](external-evpn-fabric.md); local artifact paths are provenance only. |
| Node-Agent lifecycle and recovery | Earlier H053/H054 gap closure at `ae07988`; later exact-candidate REVIEW040 profiles | Agent/speaker replacement, inventory recovery and scoped Operator profiles | H053/H054 failed in the earlier report; later targeted profiles passed within their stated scope. H030 and H032b remained deferred, not shown as passed or retired by that report. | [Release evidence and result evolution](release-evidence.md); REVIEW040 local `guest-evidence.tar` SHA-256 `05c9789350e7b12834b32296b5f510c5ee73143be52fd6ee1fd3057da02a6a09`, not included in this repository. |
| Operator acceptance | Attempt 8; exact tested source commit is not identified | Two workers, each forwarding to its external peer; not worker-to-worker traffic or a two-domain OVN setup | Bounded per-worker forwarding checks recorded PASS; restart, withdrawal and relocation were not established by that attempt | [Operator acceptance record](operator-acceptance.md); source archive SHA-256 `0d05c1ec554f9f6f4fb5e1f9f8c5558b371589df8a7c4b66cd9b98ba25504cc7`; the archive itself is not distributed here. |
| Final v0.1.0 release acceptance | Release baseline `7d54a4f0f2b23912d51a0ec3085e60606f797a53`; later publication candidate and offline checks are separately identified in the record | Maintainer reports that obsolete/inapplicable cases were removed and every case in the final revised inventory passed | Reported PASS by the maintainer; the revised case list, exact tested source, final count and final evidence bundle were not located. No numeric final result is asserted here. | [Release acceptance evolution and evidence gap](release-evidence.md); no final revised manifest or matching evidence bundle is available in this tree. |

## Release acceptance evolution

The recorded original acceptance set contains 89 canonical IDs (within a larger
166-row inventory). Early results included 75 PASS / 14 NOT TESTED. A subsequent
gap-closure report at `ae07988` recorded 87/89, with H053 and H054 FAIL. A later
89/89 report described 77 retained cases plus 12 newly exercised cases; it did
not describe 89 fresh executions and is not the final result.

The later 89-row review matrix recorded 87 PASS, H030 FAIL and H032b
NOT_ESTABLISHED. REVIEW040 carried H030 and H032b as deferred pending items. The
available records do not identify which cases were removed from the final
inventory, why each was removed, or whether either H030 or H032b was among
them. Removal must not be interpreted as a pass.

The maintainer has stated that a pre-publication inventory review retired
obsolete or inapplicable tests and that all remaining tests passed. That
statement is preserved as maintainer-provided context, not independently
reconstructed evidence: the revised inventory, retirement rationale, exact
tested source revision and final evidence manifest/checksum remain missing.
Consequently this archive does not publish a final numeric acceptance count or
map the reported result to a specific Git commit. See
[release-evidence.md](release-evidence.md) for the source-by-source record.

## Current documentation

- [Current test procedures](../../../TESTING_GUIDE.md)
- [Release scope](../../release/v0.1.0.md)
- [Implemented and planned use cases](../../use-cases/README.md)
- [Roadmap](../../roadmap/README.md)
