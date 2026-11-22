# Standalone Type-2 and external-peer campaigns

Historical standalone reports cover bounded GoBGP Type-2 export/import and
external-peer scenarios. The archived records distinguish route exchange from
actual packet forwarding and do not establish one interchangeable topology.

The pinned [`ae07988` validation record](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/validation-record.md)
contains November 8 standalone attempts reported as 15/15 and 23/23 PASS. It
explicitly does not identify their source/build revision, and records a
forwarding gap during source-down migration. These results must not be
attributed to `ae07988` merely because the report is stored there.

Separate bounded GoBGP/FRR and GoBGP/Linux records include route exchange,
selected forwarding, ownership and recovery assertions. The local reports do
not establish a complete external vendor fabric, general multi-VTEP support or
full release acceptance. See the [external-fabric evidence archive](external-evpn-fabric.md)
for per-scenario limitations and the
[release evidence](release-evidence.md) for acceptance evolution.

Source-specific reports remain available in immutable Git history:

- [Validation archive at `ae07988`](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/archive/validation-record.md)
- [Operator Type-2 record at `ae07988`](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/architecture/operator-type2-acceptance.md)
- [Release preparation record at `7d54a4f`](https://github.com/romanspb80/evpn-for-ovn/blob/7d54a4f0f2b23912d51a0ec3085e60606f797a53/docs/release/v0.1.0.md)

Private bundles and packet captures are not included here. A report's
availability in Git history is not proof that its referenced local artifacts
remain obtainable.
