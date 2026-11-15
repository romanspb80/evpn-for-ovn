# Disposable secondary-CNI VM

This VM is exclusively for the isolated secondary-CNI lab, not a production
host. It uses KVM, 4 vCPUs, 12 GiB RAM, a 128 GiB sparse virtual disk (the
box's minimum), a dedicated
libvirt pool `evpn-cni-e2e-709bc99`, and the dedicated NAT network
`evpn-cni-e2e-nat` (`192.168.249.0/24`). It does not mount the host repository.
Check the names and subnet against existing libvirt resources and host routes
before use. Vagrant and vagrant-libvirt must be installed. Create the dedicated
directory and pool only after confirming they do not already exist:

The guest routes DNS through its libvirt gateway and disables guest DNSSEC
validation because the box's static resolver configuration fails validation
on this network. Package signatures and pinned download checksums remain
required; this configuration is not applied to the host.
The guest Docker daemon uses the classic image store to preserve the pinned
image configuration IDs across `docker save`/`docker load`. Provisioning
refuses to replace an existing daemon configuration or restart Docker when
containers are present.

```sh
sudo mkdir -m 0755 /var/lib/libvirt/images/evpn-cni-e2e-709bc99
virsh -c qemu:///system pool-define-as evpn-cni-e2e-709bc99 dir --target /var/lib/libvirt/images/evpn-cni-e2e-709bc99
virsh -c qemu:///system pool-start evpn-cni-e2e-709bc99
cd deploy/containerlab/vagrant-secondary
vagrant up --provider=libvirt
vagrant ssh
vagrant halt
```

The pool must be started again after a host reboot if it is not running. The
VM is retained by default. Only when explicitly authorized, `vagrant destroy
evpn-cni-e2e-709bc99` destroys this VM and its Vagrant-managed disk; it does not
remove the retained dedicated network or storage pool.

Transfer a clean, committed source snapshot and the required pinned OVN image
from the host; do not share the host working tree. Run these commands from the
repository root after the VM is up:

```sh
git archive HEAD | (cd deploy/containerlab/vagrant-secondary && vagrant ssh -c 'mkdir -p ~/evpn-for-ovn && tar -x -C ~/evpn-for-ovn')
docker save evpn4ovn/ovn-ovs:kind-26.09 | (cd deploy/containerlab/vagrant-secondary && vagrant ssh -c 'docker load')
```

Inside the VM, verify the pinned image ID and the absence of foreign containers,
OVS/OVN processes and an active lab lease. Then run from `~/evpn-for-ovn`:

```sh
bash tools/run-cni-isolated-secondary-e2e.sh
```

The existing harness safety checks, ownership and cleanup rules remain
mandatory. This lab validates native secondary OVN forwarding, not EVPN Type-2.
