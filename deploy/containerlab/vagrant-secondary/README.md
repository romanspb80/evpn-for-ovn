# Disposable secondary-CNI VM template

This Vagrant/libvirt template is for an explicitly authorized disposable test
VM only. It has no default VM name, libvirt pool, NAT network, subnet or
gateway: all must be supplied by the isolated VM lab helper after checking
that the resources are new and unused. Do not run Vagrant directly against an
existing machine or a shared host network.

Read the [VM E2E guide](../../../docs/testing/vm-e2e-lab.md) for prerequisites,
resource ownership, host-key handling, pinned image transfer and scoped
cleanup. The helper creates the Vagrant environment with explicit values and
records its domain identity. Host repository sharing is disabled.

The VM has a minimum configuration of 4 vCPUs, 12 GiB RAM and a 128 GiB sparse
disk. Resource values can be overridden only through the dedicated helper.
Create a new empty directory pool and a new dedicated NAT network after
checking all host routes and libvirt networks for overlap. Preserve all
existing VMs, pools, networks and disks; never invoke global cleanup.

The retained guest applies pinned package checks and a Docker configuration
for the lab. After importing an image archive, the helper pulls and verifies
the exact required registry digest and config identity; Docker save/load alone
does not retain `RepoDigest` metadata. Transfer a clean committed source
archive, not a working tree.

The secondary-CNI test validates only the scenario documented by its runner.
It is not a production installation. Destroy or retain the VM only according
to the separately approved ownership and cleanup plan.
