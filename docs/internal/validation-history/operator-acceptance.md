# Operator Type-2 acceptance records

The historical Operator material covers separate scopes that must not be
combined into one acceptance claim:

- One-chassis GoBGP/FRR scenarios and descriptor-ownership diagnostics.
- Inventory-gated two-worker Attempt 8, which reported each worker forwarding
  to its external peer.

Attempt 8 does not establish worker-to-worker traffic, restart recovery,
withdrawal, relocation, or independent OVN domains. Its source commit is not
identified. The recorded source archive SHA-256 is
`0d05c1ec554f9f6f4fb5e1f9f8c5558b371589df8a7c4b66cd9b98ba25504cc7`; the
archive is not distributed with this repository.

The detailed source record is available at immutable
[`ae07988`](https://github.com/romanspb80/evpn-for-ovn/blob/ae07988afdd08b860eb45221ed50c01e1995769f/docs/architecture/operator-type2-acceptance.md).
Its placement in Git history does not identify the source used for the run.
The [historical validation index](README.md) and
[release evidence](release-evidence.md) keep this bounded result separate from
the final release acceptance statement.
