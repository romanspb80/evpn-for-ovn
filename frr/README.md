# Containerlab FRR peer

The Containerlab FRR node mounts `daemons`, `frr.conf`, and `vtysh.conf`.
`frr.conf` supplies the base BGP configuration; runtime peer addresses and
EVPN state are configured by the lab.

The active peer image is official `quay.io/frrouting/frr:10.7.1`, pinned by
manifest digest in the topology and shared acceptance helper. It reports
`FRRouting 10.7.1_git` at runtime. Both base and vtysh configuration must be
mounted explicitly; the release image does not supply these files.
See the [dependency audit](../docs/architecture/runtime-dependencies.md)
for exact image identities and read-only version/EVPN inspection commands.

The standalone Type-2 fixture and CNI/Framework acceptance runners create
run-scoped FRR configuration in their evidence/state directories. They do not
modify these shared Containerlab files.
