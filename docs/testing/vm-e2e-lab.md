# Disposable VM E2E laboratory

This is an experimental workflow for the Operator-managed two-worker
shared-OVN CNI and inventory-gated Type-2 tests. Use a disposable VM: Docker
containers alone share the host kernel and are not an equivalent isolation
boundary. The existing runner creates privileged containers and networking.
Never use a production host, cluster, shared Docker daemon, pool or network.

The baseline uses legacy CNI mode. Continuity uses authenticated CNI, per-node
trust and journals, restarts one Node-Agent and checks recovered endpoint
state. Type-2 is disabled in this topology. Neither scenario demonstrates
independent OVN-domain interoperability, BGP/GoBGP route ownership, external
VXLAN forwarding, or automatic cleanup of unreachable nodes.

The separate `operator-inventory-type2-two-worker` scenario validates
Operator-published worker inventory, independently attested GoBGP speakers,
per-worker durable acceptance, Type-2 import/export, and external FRR/Linux
EVPN state. It is not interchangeable with `operator-managed-type2` or the
legacy/continuity paths. A GoBGP RIB observation is control-plane evidence,
not proof of Linux FDB programming or packet forwarding.

```text
Linux physical host: libvirt + QEMU/KVM
  dedicated NAT + dedicated storage pool
  +-- disposable Ubuntu VM (guest kernel, Docker)
      +-- kind control plane: Operator, shared OVN NB/SB
      +-- kind worker A: Multus, OVS, ovn-controller, Node-Agent, journal A
      +-- kind worker B: Multus, OVS, ovn-controller, Node-Agent, journal B
          Node-Agents are managed by the Operator DaemonSet
          secondary CNI net1 <---- Geneve ----> secondary CNI net1
```

## Host prerequisites

Use an x86-64 Linux host with KVM enabled and accessible `/dev/kvm`, QEMU,
libvirt system daemon and CLI, Vagrant and `vagrant-libvirt`, Python 3, Git,
tar and Docker to export the pinned OVN image. A working libvirt system
connection, permissions to manage its resources, and guest outbound access
to package and pinned tool sources are required. Guest is
`generic/ubuntu2204` version 4.3.12, libvirt provider. The helper defaults
to 4 vCPU, 12 GiB RAM and a 128 GiB sparse disk;
these are resource settings, **not measured platform minimums**.
Provide adequate free host disk, RAM and CPU in addition to the host's own
needs. The guest needs the
`openvswitch` kernel module, network namespaces and privileged containers.
Nested virtualization is not required merely to run Docker in this VM; it is
needed only if the physical host is itself a VM without exposed KVM.

After a retained guest is rebooted, verify that its installed `openvswitch`
kernel module is actually loaded before starting a lab. The pinned OVN image
does not contain the guest's kernel modules. Ask the laboratory administrator
to load the installed guest module when needed; restarting Docker alone does
not fix this prerequisite. A failed in-container OVS module insertion can
remove Linux bridges and break the guest's Docker/Kind networking.

Check without changing infrastructure:

```sh
test "$(uname -m)" = x86_64
test -r /dev/kvm && test -w /dev/kvm
virsh -c qemu:///system list --all
virsh -c qemu:///system pool-list --all
virsh -c qemu:///system net-list --all
ip -4 route
vagrant --version
vagrant plugin list
docker info
```

An administrator must obtain the exact box through a trusted channel, verify
its SHA256 against an independently trusted publisher/administrator pin, and
install that exact verified file as `generic/ubuntu2204` version `4.3.12`
for libvirt. Vagrant box metadata can specify a local `file://` URL and a
`sha256` checksum; verify the installed version with `vagrant box list`.
Do not treat a version label or HTTPS alone as an integrity pin. If no trusted
box hash is available, provisioning is **BLOCKED**. Guest package signatures
and checksums for Kind, kubectl, Go and Containerlab are enforced by the
existing `deploy/containerlab/vagrant-secondary/provision.sh`.

The cached `box.img` is pinned by the VM manager to
`65db5e4be7ca68a84fe29d3f356f2189bb4b8a43116c358d764a126d80f90ae4`.
The original `.box` archive is not retained in the Vagrant cache; if the cache
is missing or its disk fingerprint changes, reacquire the exact 4.3.12 libvirt
box and re-verify its publisher SHA256 before importing it.

For an already downloaded file and an independently verified pin, one way to
import it is to create local Vagrant metadata. Use absolute paths without
spaces for this example and replace both placeholders:

```sh
BOX="/absolute/path/to/verified-ubuntu2204-libvirt.box"
BOX_SHA256="ADMINISTRATOR_VERIFIED_64_DIGIT_SHA256"
printf '%s  %s\n' "$BOX_SHA256" "$BOX" | sha256sum -c || { echo "box hash mismatch"; exit 1; }
cat > /tmp/evpn-vagrant-box-metadata.json <<EOF
{"name":"generic/ubuntu2204","versions":[{"version":"4.3.12","providers":[{"name":"libvirt","url":"file://$BOX","checksum_type":"sha256","checksum":"$BOX_SHA256"}]}]}
EOF
vagrant box add /tmp/evpn-vagrant-box-metadata.json --provider libvirt
vagrant box list
```

The host must also have the exact local image
`evpn4ovn/ovn-ovs:kind-26.09`, image ID
`sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505`.
Obtain it through an administrator-approved trusted channel; this repository
does not supply a reproducible image build or a trusted public image download.
Do not substitute a tag with a different ID.

## Prepare isolated libvirt resources

An administrator must create a **new empty directory storage pool** and a
**new dedicated NAT network**, with DHCP and DNS, reachable from the guest.
Choose an unused private IPv4 /24 and gateway that do not overlap any host
route, network, VPN or libvirt network. The wrapper checks that the selected
network is active, NAT, private /24 and not attached to any existing VM, and
that the pool is active and empty; it cannot establish that the
administrator-selected network is safe for the surrounding infrastructure.
It refuses its historical lab defaults and ignores Vagrant environment
overrides that redirect its working directory, state directory or
Vagrantfile.
Review host routes and every network's `virsh net-dumpxml` before creation.
The wrapper does not create, delete or reconfigure either pool or network.

The following example creates resources, so run it only after an administrator
has checked names, directory, subnet and routing are unused and approved them.
Replace the example names and subnet if they conflict. The directory must be
dedicated and writable by libvirt/QEMU; host-specific MAC or filesystem
policies may require administrator configuration.

```sh
POOL=evpn-vm-lab-pool
NETWORK=evpn-vm-lab-nat
POOL_DIR="/var/lib/libvirt/images/$POOL"
if virsh -c qemu:///system pool-info "$POOL" >/dev/null 2>&1; then echo "pool exists: STOP"; exit 1; fi
if virsh -c qemu:///system net-info "$NETWORK" >/dev/null 2>&1; then echo "network exists: STOP"; exit 1; fi
# Stop here if this subnet overlaps anything.
sudo mkdir -m 0755 "$POOL_DIR"
virsh -c qemu:///system pool-define-as "$POOL" dir --target "$POOL_DIR"
virsh -c qemu:///system pool-start "$POOL"
cat > /tmp/evpn-vm-network.xml <<EOF
<network>
  <name>$NETWORK</name>
  <forward mode="nat"/>
  <ip address="192.168.248.1" netmask="255.255.255.0">
    <dhcp><range start="192.168.248.100" end="192.168.248.200"/></dhcp>
  </ip>
</network>
EOF
virsh -c qemu:///system net-define /tmp/evpn-vm-network.xml
virsh -c qemu:///system net-start "$NETWORK"
```

Creation of a new NAT network adds its own host bridge/firewall rules; it must
not change existing bridges. Do not proceed if that change is not authorized.

## Quick start

Run on the physical host from the repository root. Replace the three values
with your own dedicated resources and a **nonexistent** private state
directory whose parent exists:

```sh
export VAGRANT_HOME="$HOME/.local/share/evpn-vm-lab/vagrant-home"
LAB="$HOME/evpn-vm-lab-state"
POOL=evpn-vm-lab-pool
NETWORK=evpn-vm-lab-nat
python3 tools/vm-e2e-lab.py --directory "$LAB" create --pool "$POOL" --network "$NETWORK"
python3 tools/vm-e2e-lab.py --directory "$LAB" status
```

Install the independently verified Vagrant box into this dedicated
`VAGRANT_HOME` before creating the lab; the VM manager rejects a missing or
changed cached disk. Do not copy a maintainer's private Vagrant path.

`create` boots and provisions the VM. `start` later resumes a stopped VM; it
does not reprovision. Resource overrides are `--cpus`, `--memory-mb` and
`--disk-gb` on `create`. Never delete or edit the private state directory
while its VM exists: it contains the ownership receipt and Vagrant machine ID.
If creation fails before a receipt is completed, automatic destruction is
refused. Preserve the directory, inspect libvirt and Vagrant state, and ask an
administrator to resolve ownership; do not destroy by guessed names.

The inventory scenario executes an archive of one exact committed source tree.
The checkout must be clean (including untracked, non-ignored files); do not
include uncommitted runner changes. The VM does not mount the host checkout and
does not need `.git`: the inventory runner and its dependencies do not query
Git metadata in the guest. The current committed tree contains no submodules or
symlink entries; the source uploader rejects unsafe archive members.

```sh
test -z "$(git status --porcelain --untracked-files=all)"
SOURCE_COMMIT="$(git rev-parse HEAD)"
git archive --format=tar "$SOURCE_COMMIT" > /tmp/evpn-vm-source.tar
sha256sum /tmp/evpn-vm-source.tar
test "$(docker image inspect evpn4ovn/ovn-ovs:kind-26.09 --format '{{.Id}}')" = \
  'sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505'
docker save evpn4ovn/ovn-ovs:kind-26.09 -o /tmp/evpn-vm-ovn.tar
python3 tools/vm-e2e-lab.py --directory "$LAB" source --archive /tmp/evpn-vm-source.tar
python3 tools/vm-e2e-lab.py --directory "$LAB" image --archive /tmp/evpn-vm-ovn.tar
```

The `image` action loads the archive, then pulls and verifies the exact pinned
k8gobgp registry digest and expected config ID. Docker archive load alone does
not restore `RepoDigest` aliases. The helper refuses to continue when the exact
registry digest or expected image identity is unavailable; do not substitute a
tag or another digest.

`source` recomputes `git archive --format=tar "$SOURCE_COMMIT"` and refuses a
dirty host checkout or any archive whose SHA-256 differs from that committed
tree. It streams the verified tar archive to the guest, where the uploaded
archive is checksum-verified before extraction. The guest retains the archive
and a private provenance receipt outside the source tree; before inventory
execution it verifies the receipt against the provenance environment, hashes
the retained archive again, and checks that extracted paths and file contents
match it. The source commit and archive SHA are recorded outside the archive
(`lab.json` on the host and the guest receipt), avoiding a self-hash
dependency. `lab.json` records the exact source commit, archive SHA-256, tar
format, and command. Source can be installed only once in a VM; create a fresh
VM to update it. Protect and remove the temporary host archives when no longer
needed.

```sh
python3 tools/vm-e2e-lab.py --directory "$LAB" run baseline
```

For continuity, first perform the trusted input procedure below, then:

```sh
PIN="YOUR_EXPLICIT_64_DIGIT_LOWERCASE_SHA256"
python3 tools/vm-e2e-lab.py --directory "$LAB" ctr --file /path/to/verified/ctr --sha256 "$PIN"
python3 tools/vm-e2e-lab.py --directory "$LAB" run continuity --ctr-sha256 "$PIN"
python3 tools/vm-e2e-lab.py --directory "$LAB" collect --output "$HOME/evpn-vm-evidence.tar"
python3 tools/vm-e2e-lab.py --directory "$LAB" stop
python3 tools/vm-e2e-lab.py --directory "$LAB" start
python3 tools/vm-e2e-lab.py --directory "$LAB" status
python3 tools/vm-e2e-lab.py --directory "$LAB" destroy
```

`run both --ctr-sha256 "$PIN"` runs the stages sequentially and reports each
independently. The output file must not already exist. Collect before
destruction, including on failure. `ssh` opens an interactive guest session for
diagnosis; it is not itself proof of a safe test environment.

## Trusted runtime input and continuity

An administrator must select a trusted, Linux/amd64, executable `ctr` whose
version is compatible with the **kind workers' containerd**, verify its origin
and SHA256 independently, and supply both file and pin. The pinned kind node
image is
`kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c`.
The runner copies the binary independently to each worker and checks the hash
and CRI connectivity. Do not copy runtime observations or generated trust
configuration between nodes. The stable node identity is distinct from
boot/process identities, which are observed again after restart. A changed
binary or unexpected identity must fail closed; never silently repin.
For the legacy continuity scenario, there is no repository-provided source and
independently verified pin for this binary: that scenario is **BLOCKED** until
an administrator supplies one.
The existing runtime contract is documented in
[runtime dependencies](../architecture/runtime-dependencies.md).
The fixture derives other executable hashes from each newly created worker;
it does not compare them with administrator-approved pins across separate
cluster runs. Review those observations for every new run. The workflow does
not include a live negative attestation/identity-change test. That scenario
requires separate failure injection and exact process/route observations.

## Inventory-gated Type-2 scenario

The inventory workflow requires `ctr` matching the pinned Kind node image's
containerd and standalone `gobgpd` from the locally pinned GoBGP image. Do not
use an arbitrary host `ctr` or rebuild GoBGP from an unpinned source. Extract
each binary from the exact image and record its SHA256:

```sh
KIND_IMAGE='kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c'
GOBGP_IMAGE='ghcr.io/purelb/k8gobgp:0.2.8'
test "$(docker image inspect "$KIND_IMAGE" --format '{{.Id}}')" = \
  'sha256:2fd45711f98975a41fd31c0915b7772b482e5004e061f7086e80f1ae14d26c95'
test "$(docker image inspect "$GOBGP_IMAGE" --format '{{.Id}}')" = \
  'sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0'
KIND_CID="$(docker create --name evpn-ctr-extract --label evpn.e2e.owner=inventory-input --entrypoint /bin/true "$KIND_IMAGE")"
docker cp "$KIND_CID:/usr/local/bin/ctr" /tmp/evpn-inventory-ctr
test "$(docker inspect --format '{{index .Config.Labels "evpn.e2e.owner"}}' "$KIND_CID")" = inventory-input
docker rm "$KIND_CID"
GOBGP_CID="$(docker create --name evpn-gobgpd-extract --label evpn.e2e.owner=inventory-input --entrypoint /bin/true "$GOBGP_IMAGE")"
docker cp "$GOBGP_CID:/usr/local/bin/gobgpd" /tmp/evpn-inventory-gobgpd
test "$(docker inspect --format '{{index .Config.Labels "evpn.e2e.owner"}}' "$GOBGP_CID")" = inventory-input
docker rm "$GOBGP_CID"
chmod 0755 /tmp/evpn-inventory-ctr /tmp/evpn-inventory-gobgpd
sha256sum /tmp/evpn-inventory-ctr /tmp/evpn-inventory-gobgpd
```

Use unique temporary container names and remove only the exact container IDs
created by these commands. The extracted binaries' hashes are explicit run
inputs; the VM manager verifies each hash before transfer and again inside the
guest. The inventory run automatically installs and holds the signed Ubuntu
HWE kernel packages `6.8.0-149.149~22.04.1`, reboots if required, checks the
running release after reconnect (SSH availability alone is not sufficient),
and executes `getsockopt(SOL_SOCKET, SO_PEERPIDFD)` on a connected
`AF_UNIX/SOCK_STREAM` socket in the guest. Reconnect attempts are preserved in
`kernel-reconnect.log`; the raw probe output and extracted JSON result are
preserved separately. The inventory test image also executes the exact probe
inside the Node-Agent container under its normal seccomp policy.
`pidfd_open()` is not a substitute.

After creating a fresh dedicated pool/network and VM, transferring the source
archive and verified OVN image as in Quick start, run:

```sh
CTR_SHA256="$(sha256sum /tmp/evpn-inventory-ctr | cut -d' ' -f1)"
GOBGPD_SHA256="$(sha256sum /tmp/evpn-inventory-gobgpd | cut -d' ' -f1)"
python3 tools/vm-e2e-lab.py --directory "$LAB" ctr \
  --file /tmp/evpn-inventory-ctr --sha256 "$CTR_SHA256"
python3 tools/vm-e2e-lab.py --directory "$LAB" gobgpd \
  --file /tmp/evpn-inventory-gobgpd --sha256 "$GOBGPD_SHA256"
python3 tools/vm-e2e-lab.py --directory "$LAB" run operator-inventory-type2-two-worker \
  --ctr-sha256 "$CTR_SHA256" --gobgpd-sha256 "$GOBGPD_SHA256"
python3 tools/vm-e2e-lab.py --directory "$LAB" collect \
  --output "$HOME/evpn-inventory-evidence.tar"
```

The guest executes
`tools/run-operator-secondary-cni-e2e.sh --scenario operator-inventory-type2-two-worker`
with disposable-host confirmation and both pinned binaries. Do not substitute a
single-worker scenario. The harness requires time-correlated
workload-interface captures (`net1` and
`frr-work0`), worker Linux bridge/VXLAN snapshots, and an allowlisted
evidence manifest with source and binary provenance; ping success alone is not
sufficient for dataplane attribution. Worker Linux bridge/VXLAN counters are
observation-only integration state: identity and monotonic counter deltas are
recorded, but zero deltas are valid and do not gate forwarding. Workload
interfaces and OVS tunnel/Type-2 flow counters remain expected-forwarding
evidence and must advance. The manifest hashes a fixed artifact allowlist; a
separate `inventory-evidence-manifest.sha256` hashes the completed manifest
without creating a circular artifact hash.

## Evidence and interpretation

### Acceptance gap overlays

Keep acceptance-gap evidence separate from previous attempts. Record the exact
production commit, original archive hash and every test-only overlay; compare
production source bytes with that commit. Do not weaken the VM source uploader's
clean-tree/archive checks to admit an overlay. A directly invoked guest fixture
with recorded test overlays is a different run variant from the pristine archive
workflow above.

On the verified disposable guest, the Framework runners have separate opt-ins:

```sh
EVPN_NATIVE_GAP_CLOSURE=1 EVIDENCE_ROOT="$HOME/evpn-gap-evidence/native" \
  bash tools/run-framework-netlink-e2e.sh
FRAMEWORK_OVSDB_GAP_CLOSURE=1 EVIDENCE_ROOT="$HOME/evpn-gap-evidence/ovsdb" \
  bash tools/run-framework-ovsdb-e2e.sh
```

Run them serially. Preserve their fixed milestone cluster names; each evidence
root contains uniquely named attempts. Native gaps cover IPv4/IPv6 export and
withdrawal plus an actual SBDB process pause/recovery. OVSDB gaps separately
exercise importer/speaker restart safety, peer outage and foreign ownership
conflicts; native-backend evidence does not establish those assertions.

`TestRemoteType2GapNegatives_Live` requires the existing disposable read-only
importer fixture, `EVPN_IMPORT_LIVE=disposable`, `EVPN_GAP_CLOSURE=1`,
`EVPN_IMPORT_LOCAL_VTEP` matching its receiver, and an existing owned
`EVPN_GAP_EVIDENCE_DIR`. Run it after `TestRemoteType2Import_Live` has released
its test peer. It establishes a real EVPN peer and uses an isolated Linux network
namespace for owned FDB/neighbour assertions. That namespace retains its own
`192.0.2.32` local VTEP; `EVPN_IMPORT_LOCAL_VTEP` identifies the separate
receiver's readiness gate, not the namespace's VXLAN device. Evidence is copied
through the agent container, where the files actually reside. The receiver's
two local exports and foreign local control remain intact. Each execution creates a unique
`route-negatives-*` evidence directory. Unauthorized RT/VTEP/VNI input,
duplicate AddPaths and conflicting next hops are tested with valid and foreign
survivors. This is wire-to-kernel ownership evidence, not workload forwarding
or OVSDB backend parity.

For the separate importer gap run, enable `EVPN_EXPORT_TEST=1`,
`EVPN_IMPORT_TEST=1`, and `EVPN_GAP_CLOSURE=1` with
`EVPN_FRR_INTEROP_TEST=0` in `tools/run-cni-mvp-2.4b-multus.sh`.
This opt-in reuses the maintained provider-attestation setup without enabling
the FRR topology or a competing remote writer. It establishes two healthy
local exports and a foreign control before exercising the importer API outage
and route negatives, then exits through owned topology cleanup. It does not
repeat the unrelated legacy exporter fault sequence or bypass Unix-peer,
Kubernetes, executable, or runtime identity checks.

Retain failed attempts as well as successful ones. An opt-in test skipped before
fixture setup is not live coverage. Count canonical case IDs rather than command
invocations, and preserve each original result alongside its gap-closure result.

Each run saves scenario status, logs, kernel package/probe evidence where
applicable, and environment versions in a private guest `/tmp/evpn-vm-run-*`
directory. The existing
runner creates `/tmp/cni-sec-*-evidence`, including Kubernetes and Operator
diagnostics, node/OVN/OVS observations, CNI and Geneve evidence, and
continuity-stage and journal observations where reached. A status of PASS
means the invoked runner exited successfully; inspect its detailed evidence
for individual assertions and any `FOREIGN_WITNESS_NOT_AVAILABLE` result.
If an inventory workload capture fails to reach its listening state,
`inventory-capture-startup-failure.json` records the command, elapsed time,
captured startup/trailing stderr, and process exit status; it is setup evidence,
not evidence that forwarding passed or failed. The inventory fixture provisions
missing `tcpdump` only in its verified run-owned FRR container. It records tool
and Alpine versions in `inventory-frr-workload-tools.json` and provisions
`iputils-ping` in that same container when the available ping lacks the required
`-e` identifier option. The owned lab teardown removes these writable-layer
changes; no host packages or pinned image are changed.
FAIL means the invocation failed; BLOCKED means a prerequisite is unavailable;
SKIP means intentionally skipped; NOT RUN means no invocation occurred.
Preflight failures can occur before a status file exists and are not PASS.
Read `failure.json` and the scenario log for the first failure.

Collected archives and guest evidence can contain credentials, Pod data,
configuration and packet captures. They are private (output mode 0600), not
automatically redacted, and must be inspected and sanitized before sharing.
Collection includes all matching run directories in this disposable guest.

## Reruns, troubleshooting and cleanup

Rerun the same `run` command only after checking the previous runner's cleanup
and lease evidence. It refuses foreign Docker, OVN and cluster resources; do
not force cleanup to get a pass. If cleanup is uncertain, preserve the VM and
evidence and follow the ownership recovery guidance in the
[Containerlab guide](../../deploy/containerlab/README.md). Recreate the VM to
replace its source or trusted binary. No general reset command is supplied.

- KVM/libvirt unavailable: check `/dev/kvm`, daemon, permissions and host CPU
  virtualization. Do not fall back to a physical-host test.
- Resource, box or dependency failure: check host free space, memory, guest
  connectivity, package signatures and pinned hashes; do not change pins.
- Docker or cluster creation failure: inspect guest `docker info`, Kind and
  Containerlab logs and prior ownership leases.
- Missing workers, Operator install or DaemonSet placement: inspect runner
  logs, Kubernetes snapshots, events, CR status and ownership evidence.
- OVN/OVS sockets, Multus/CNI or Geneve failure: inspect per-worker socket,
  CNI invocation, OVN binding and packet-correlation evidence; primary-network
  connectivity is not evidence of secondary connectivity.
- ctr mismatch or attestation failure: verify the approved binary, exact pin,
  worker CRI version and per-worker runtime observations; do not bypass trust.
- Journal or restart failure: preserve receipts, locks and logs; readiness
  alone is not recovery proof.
- Incomplete cleanup: do not remove foreign resources or shared networks;
  inspect the exact ownership receipts and recover manually.

`destroy` validates the Vagrant machine ID, domain UUID, configuration, disk
paths, pool UUID and network UUID before asking Vagrant to destroy that VM
and its VM disk. It retains the private state directory, evidence archive,
dedicated network and pool. The administrator may separately remove an empty
pool/network **only after** confirming their exact identity and no remaining
users; this tool does not remove them. Compare `virsh list --all`,
`pool-list --all`, `net-list --all` and host routes with the pre-lab inventory
to confirm unrelated physical-host resources remain unchanged.
