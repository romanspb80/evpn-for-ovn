# Containerlab FRR peer

The Containerlab FRR node mounts `daemons`, `frr.conf`, and `vtysh.conf`.
`frr.conf` supplies the base BGP configuration; runtime peer addresses and
EVPN state are configured by the lab.

The active peer image is official `quay.io/frrouting/frr:10.7.1`, pinned by
manifest digest in the topology and shared acceptance helper.
Both base and vtysh configuration must be
mounted explicitly; the release image does not supply these files.
The [shared acceptance helper](../tools/lib/cni-evpn-frr-interop-test.sh)
is the maintained source for exact image identities and read-only
`vtysh` version/EVPN inspection commands. See
[runtime dependencies](../docs/architecture/runtime-dependencies.md#frr-fixture-inputs)
for their role and usage boundaries.

The standalone Type-2 fixture and CNI/Framework acceptance runners create
run-scoped FRR configuration in their evidence/state directories. They do not
modify these shared Containerlab files.
