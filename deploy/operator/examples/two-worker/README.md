# Two-worker inventory-gated Type-2: configuration templates

**Status: configuration template, not a verified deployment.** Do not apply
angle-bracket values. The supported attachment is OVN native dynamic routing:
`ovn-controller` reads Linux EVPN synchronization state and programs an OVS
VXLAN port and flows on `br-int`. There is deliberately **no packet-carrying
link** from `br-int` to `evpnbr1000`; a Linux bridge or an imported FDB entry
alone does not prove forwarding. The repository has single-worker forwarding
evidence, but this shared-switch/two-chassis combination has not been
validated. Live BGP and VXLAN for this example remain **BLOCKED**. Historical
two-chassis standalone forwarding (15/15) and lifecycle (23/23), and a
single-chassis Operator/FRR regression (18/18), passed in different
topologies; see the [evidence record](../../../../docs/archive/validation-record.md#standalone-multi-chassis-g7b5b2).

The [live validation runbook](VALIDATION.md) defines per-worker gates,
evidence collection and bounded recovery checks for an authorized disposable
environment.

The intent is one shared OVN NB/SB domain, one CNI endpoint per worker, two
independent host GoBGP processes, and one FRR peer with a real remote endpoint.
OVN Geneve carries local worker-to-worker traffic; no BGP session is configured
between workers. Documentation addresses below are examples; allocate real,
reachable, non-overlapping addresses before deployment and replace **every**
occurrence consistently:

| Purpose | Example |
|---|---|
| Worker A BGP/router ID and VTEP | 192.0.2.11 |
| Worker B BGP/router ID and VTEP | 192.0.2.12 |
| FRR BGP/router ID and VTEP | 192.0.2.30 |
| BGP ASN / RT / VNI | 64512 / 64512:1000 / 1000 |
| Logical switch / network ID | tenant-a / tenant-a |
| Workload A / B | 10.244.240.11 / .12, MAC ...:11 / ...:12 |
| FRR remote workload / bridge | 10.244.240.30 / .1 |

`evpncore.yaml` and `rbac.yaml` describe cluster resources. `export.json`,
`import.json`, `cni.json`, `provider.json`, and `gobgpd.yaml` are
**per-node templates**: instantiate separate files for A and B; never copy
identities or journals from one node to the other. The Operator creates the
inventory ConfigMap and DaemonSet; administrators create all other resources
and install node files, speakers, CNI/Multus, OVN and FRR. The `k8gobgp`
value in EvpnCore is the only supported API value and does **not** deploy or
manage either speaker. Here the existing Node-Agent standalone *speaker
attestation mode* authenticates two independently administered GoBGP services;
this is not the separate standalone Node-Agent runtime.

## Trust and placeholder inventory

Use an isolated cluster and dedicated underlay. Commands changing Kubernetes,
OVN, Linux networking, systemd or FRR require an authorized administrator;
the Node-Agent and speaker are privileged. Secure Kubernetes API and OVN
connections according to your environment; the sample `tcp:` OVN URLs assume
a trusted network and do not provide TLS. Restrict resource and host writes
to trusted administrators and Operator credentials.

| Placeholder | How to obtain or provision it |
|---|---|
| `NODE_A`, `NODE_B` | Existing distinct Kubernetes Node names; `kubectl get nodes` |
| `NODE_A_UID`, `NODE_B_UID`, `NODE_UID` | `kubectl get node NAME -o jsonpath='{.metadata.uid}'`; must match the node hosting each file |
| `CHASSIS_A`, `CHASSIS_B`, `CHASSIS` | On each host: `sudo ovs-vsctl --bare get Open_vSwitch . external_ids:system-id`; verify its unique SB Chassis row |
| `CLUSTER_UID` | Generate a stable UUID once (`uuidgen`), retain it with the reservation backup |
| `INSTALLATION_UID` | Create the disabled CR first, then `kubectl -n evpn-system get evpncore evpn-core -o jsonpath='{.metadata.uid}'` |
| `NODE_AGENT_IMAGE`, `CLIENT_IMAGE` | Administrator-selected runnable trusted images; use an immutable digest; the agent image must include this inventory-gate code |
| `OVN_NB_HOST`, `OVN_SB_HOST` | Reachable shared OVN NB/SB service DNS names, without scheme or port |
| `NODE`, `VTEP` | The current worker's exact Node name and allocated local IPv4 address |
| `BOOT_ID`, `HOST_NETNS` | On that host: `cat /proc/sys/kernel/random/boot_id` and `readlink /proc/self/ns/net`; changes require recovery review |
| `SERVICE_UID` | Independently generate and persist a distinct UUID for each speaker service |
| `SPEAKER_CGROUP` | After starting the service: `systemctl show -p ControlGroup --value gobgpd`; check the actual speaker PID's `/proc/PID/cgroup` |
| `GOBGPD_SHA256`, `GOBGP_CONFIG_SHA256` | On that host, SHA-256 of `/usr/local/bin/gobgpd` and the rendered `/etc/evpn/gobgpd.yaml` |
| `CTR_SHA256`, `CNI_SHA256`, `MULTUS_SHA256`, `RUNTIME_SHA256` | On that host, SHA-256 of `/opt/evpn/bin/ctr`, installed `evpn-cni`, Multus executable and actual containerd executable respectively |

`providerCTRHostPath` is mounted into the agent as
`/usr/local/libexec/evpn-ctr`; `CTRPath` and its hash refer to that mounted
file. CNI executable pins must describe the **actual process chain**. If the
installation uses a shell wrapper, add the three implemented wrapper pins
described in [CNI creation authority](../../../../docs/architecture/cni-creation-authority.md);
do not hash a wrapper as if it were the CNI binary.

## Staging and activation

1. Provision the shared NB/SB, one local OVS and ovn-controller on each worker,
   containerd, the primary CNI, Multus, and the project's `evpn-cni`. Verify
   worker identity and assign the distinct VTEPs to the hosts. Confirm the
   chosen underlay can carry TCP/179 and UDP/4789.
2. Render `gobgpd.yaml` for each host and install it as
   `/etc/evpn/gobgpd.yaml`; install `gobgpd.service` and the same trusted GoBGP
   executable as `/usr/local/bin/gobgpd`. Use root-owned, non-writable,
   non-symlink path components and files. Start each service with
   `sudo systemctl enable --now gobgpd`; confirm the socket is
   `/run/gobgp/gobgp.sock`. Record its exact command line, cgroup and hashes
   for that node's `provider.json`. No other process may own the socket.
3. Configure the external FRR system separately using `frr.conf`, with zebra
   and bgpd enabled. The peer must have 192.0.2.30 on its underlay. In a
   dedicated FRR network namespace or host, an administrator can create a
   remote endpoint, for example:

   ```sh
   ip link add br1000 type bridge
   ip addr add 10.244.240.1/24 dev br1000
   ip link set br1000 up
   ip link add vxlan1000 type vxlan id 1000 local 192.0.2.30 dstport 4789 nolearning
   ip link set vxlan1000 master br1000
   ip link set vxlan1000 up
   ip netns add remote-workload
   ip link add frr-host0 type veth peer name frr-work0
   ip link set frr-host0 master br1000
   ip link set frr-host0 up
   ip link set frr-work0 netns remote-workload
   ip -n remote-workload link set lo up
   ip -n remote-workload link set frr-work0 address 02:00:00:00:00:30
   ip -n remote-workload addr add 10.244.240.30/24 dev frr-work0
   ip -n remote-workload link set frr-work0 up
   ip netns exec remote-workload ping -c 2 10.244.240.1
   ```

   Names must be absent before creation. The endpoint must generate traffic
   for bridge/FDB and neighbor learning; confirm FRR advertises its MAC/IP.
   The fragment is a forwarding-capable **remote** topology, not proof of
   local forwarding. Permit TCP/179 and UDP/4789 in both directions and
   verify the underlay route and effective MTU; for an IPv4 underlay MTU of
   1500 use an endpoint MTU no greater than 1450, and lower it if the actual
   Geneve encapsulation or additional headers require it.
4. On each worker provision an exclusive Linux bridge `evpnbr1000`, VXLAN
   device `evpnvx1000`, and advertisement device `evpnadv1000`; verify
   the names do not already belong to anything else. The project's existing
   single-worker interop fixture uses a separate UDP 60100 for the Linux
   synchronization VXLAN and UDP 4789 for OVN EVPN forwarding:

   ```sh
   # Set VTEP to this worker's locally assigned address before running.
   sudo ip link add evpnbr1000 type bridge
   sudo ip link set evpnbr1000 up
   sudo ip link add evpnvx1000 type vxlan id 1000 local "$VTEP" dstport 60100 nolearning
   sudo ip link set evpnvx1000 master evpnbr1000
   sudo ip link set evpnvx1000 up
   sudo ip link add evpnadv1000 type dummy
   sudo ip link set evpnadv1000 master evpnbr1000
   sudo ip link set evpnadv1000 up
   sudo ovs-vsctl set Open_vSwitch . "external_ids:ovn-evpn-local-ip=$VTEP" external_ids:ovn-evpn-vxlan-ports=4789
   ```

   Both workers must use these same device **names** in the network namespace
   observed by their local `ovn-controller`, but each uses its own `$VTEP`,
   its own devices and its own journal. For this example `$VTEP` is
   `192.0.2.11` on A and `192.0.2.12` on B. `ovn-controller` must use the
   same local OVS database as the Node-Agent's OVS endpoint. Do not attach
   either Linux bridge to `br-int` with a veth, patch port or localnet port.

   On the shared NB server, create `tenant-a` if it is absent and dedicated
   to this example; never overwrite another owner's switch or settings:

   ```sh
   ovn-nbctl ls-add tenant-a
   ovn-nbctl set Logical_Switch tenant-a other_config:dynamic-routing-vni=1000 \
     other_config:dynamic-routing-bridge-ifname=evpnbr1000 \
     other_config:dynamic-routing-vxlan-ifname=evpnvx1000 \
     other_config:dynamic-routing-advertise-ifname=evpnadv1000 \
     other_config:dynamic-routing-redistribute=fdb,ip
   ```

   Skip `ls-add` only after independently establishing ownership of an
   existing dedicated switch. These settings follow
   [`interop_setup_config`](../../../../tools/lib/cni-evpn-frr-interop-test.sh);
   the verified fixture uses OVN 26.09.0 and OVS 4.0.0. Use a compatible
   version and confirm the controller actually realizes the mapping on each
   worker as described below. Do not add an `addresses=unknown` LSP to this
   switch: the pinned northd has a
   [known crash](../../../../docs/known-issues/ovn-northd-dynamic-routing-unknown-crash.md).
5. Install the Operator and create the app namespace. Render the CR and RBAC
   templates with the discovered values; review every substitution. Create the
   reservation bootstrap **once**; never replace an existing reservation
   ConfigMap with an empty one.

   ```sh
   kubectl apply -f deploy/operator/manifests/operator.yaml
   kubectl create namespace evpn-system
   kubectl create -f rendered-reservations.yaml
   kubectl apply -f rendered-rbac.yaml
   ```

   Grant `get pods` to the service account in every other namespace whose
   workloads use this CNI. Restrict admission and NAD writes to administrators:
   the root-owned CNI continuity mapping and pinned process chain authorize
   endpoint creation, while Kubernetes Pod identity is checked independently.
   `workloads.yaml` uses fixed unique addresses and is for one Pod per NAD only.
6. Render `evpncore.yaml` with `enabled: false`, create it with
   `kubectl create -f rendered-evpncore.yaml`, and obtain its
   installation UID. Then render the four JSON templates separately on each
   node. Install export/import/cni files at their `/run/evpn-node-agent/*.json`
   paths and provider file at `/etc/evpn/provider-attestation/provider.json`.
   Set root ownership and mode 0600; root-owned non-writable directory
   components are required. Provision the existing persistent host directory
   `/var/lib/evpn-worker` mode 0700. For a new empty deployment create the
   import state parent `/var/lib/evpn-worker/import` mode 0700; the importer
   creates its `receipts-v1` journal beneath that parent. Do **not** precreate
   `inventory`, `export`, `cni`, or `import/receipts-v1` journal directories;
   missing metadata in an existing journal is intentionally blocked. The corresponding container
   paths are under `/var/lib/evpn-node-agent`. Never reuse a journal from
   another node or installation.
7. Label exactly the two prepared Nodes `evpn.example.com/worker=true`.
   Enable the CR only after verifying all files and identities:

   ```sh
   kubectl -n evpn-system patch evpncore evpn-core --type merge -p '{"spec":{"enabled":true}}'
   kubectl -n evpn-system get configmap evpn-worker-inventory -o json
   kubectl -n evpn-system get daemonset evpn-node-agent -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
   kubectl -n evpn-system rollout status daemonset/evpn-node-agent
   kubectl -n evpn-system get evpncore evpn-core -o jsonpath='{.status.conditions}{"\n"}'
   ```

   Confirm ConfigMap UID matches `--worker-inventory-uid`, and the namespace,
   installation, cluster and journal flags match. `InventoryAccepted` and
   `RuntimeReady` remain Unknown; Pod readiness is not proof of acceptance.
8. Render and apply `workloads.yaml` only after the above trust and network
   prerequisites are met. Preserve each NAD's exclusive IP/MAC allocation.

## Native forwarding attachment and ownership

The worker's packet path is `Pod net1 -> host veth -> br-int -> OVN logical
switch pipeline -> controller-owned OVS VXLAN/4789 -> remote VTEP`. The
reverse packet enters that OVS VXLAN port, is mapped to VNI 1000 and the
logical switch, and exits through the owning endpoint's OVS port and veth.
Local A/B traffic stays on OVN's Geneve path. `evpnvx1000` on UDP 60100 is
**not** the OVS VXLAN port and is not in this packet path.

| Forwarding edge / state | Owner and provisioning | Verification |
|---|---|---|
| Pod net1 to local `br-int` | CNI Node-Agent creates the veth; [`EnsureEndpoint`](../../../../pkg/endpoint/manager.go) creates the NB LSP and [`EnsureAttachmentWithIdentity`](../../../../pkg/attachment/ovs.go) attaches its OVS interface with `iface-id` | Match the Pod's `net1`, LSP name, and OVS interface `external_ids:iface-id`; verify its SB binding is up on the expected chassis |
| NB switch and LSP to SB binding | Administrator creates/configures `tenant-a`; Node-Agent owns its endpoint LSP; northd and local ovn-controller create/bind SB state | Inspect NB switch/LSP and SB `Port_Binding`; compare its chassis UUID to the worker's `Chassis` UUID |
| Linux synchronization state to OVS | Administrator creates bridge, VXLAN and dummy devices. Importer writes receipt-owned remote-MAC FDB on `evpnvx1000` and IP neighbor on `evpnbr1000`; ovn-controller consumes them by Netlink | Inspect exact remote MAC, IP and VTEP in both Linux tables, then independently inspect controller-created OVS VXLAN port and remote-MAC flows |
| Outbound/inbound external packets | ovn-controller owns the flow-based OVS VXLAN interface and `br-int` flows; OVS forwards on UDP 4789, VNI 1000. Remote FRR/zebra and its administrator own the remote bridge, VXLAN and endpoint | Check OVS port type/options/ofport, flow and counters; capture both underlays and inner traffic. A receipt is not a packet test |
| Local advertisement / return-path FDB | Node-Agent CNI observer exports only locally authorized endpoints via its attested speaker; FRR imports their Type-2 routes and programs the remote FDB | Compare each endpoint's MAC/IP, RT, VNI and next hop in both BGP RIBs and the remote `vxlan1000` FDB |

This path uses neither an administrator-managed OVS flow nor a Linux
bridge-to-OVS forwarding port. OVN's `dynamic-routing-*` settings associate
the logical switch with the *control-state* devices; they do not enslave a
workload port to `evpnbr1000`. The implementation and evidence are in
[`runtime_wiring.go`](../../../../pkg/nodeagent/runtime_wiring.go),
[`owned_netlink.go`](../../../../pkg/evpn/owned_netlink.go),
[`interop_capture_ovs_snapshot`](../../../../tools/lib/cni-evpn-frr-interop-test.sh)
and the [external fabric evidence](../../../../docs/testing/external-evpn-fabric.md).
The upstream controller's
[`neighbor.c`](https://github.com/ovn-org/ovn/blob/2cf772348fc572c68e9bd70652f3b0ece267a9cd/controller/neighbor.c)
names the monitored dynamic-routing devices, and
[`create_evpn_tunnels`](https://github.com/ovn-org/ovn/blob/2cf772348fc572c68e9bd70652f3b0ece267a9cd/controller/encaps.c)
creates EVPN tunnels from chassis configuration. These upstream references
describe the mechanism, not the exact source revision of the fixture image.
The OVS flow and packet observations in those tests do not establish this
example's shared-switch behavior on both chassis.

After both agents have imported the remote route, run these **read-only**
commands separately in each worker's OVN/OVS network namespace and against
its local OVSDB, substituting the actual endpoint LSP for `$LSP`:

```sh
ovn-nbctl get Logical_Switch tenant-a _uuid other_config
ovn-nbctl get Logical_Switch_Port "$LSP" _uuid addresses
ovn-sbctl --columns=logical_port,chassis,up find Port_Binding "logical_port=$LSP"
ovs-vsctl --bare get Open_vSwitch . external_ids:system-id
ovn-sbctl --columns=_uuid,name find Chassis "name=$CHASSIS"
ovs-vsctl --columns=name,ofport,external_ids find Interface "external_ids:iface-id=$LSP"
ip -d link show evpnvx1000
bridge link show
bridge -d fdb show dev evpnvx1000
ip neigh show dev evpnbr1000
ovs-vsctl --columns=name,type,options,ofport find Interface type=vxlan options:dst_port=4789
ovs-vsctl list-ports br-int
ovs-ofctl dump-flows br-int
ovs-ofctl dump-ports br-int
```

Set `$CHASSIS` to the independently verified local system ID. Compare the
binding's chassis UUID and `up` state, device VNI/local IP/master and
`dstport`, the remote `02:00:00:00:00:30` FDB destination `192.0.2.30`,
and its IP neighbor `10.244.240.30`. The OVS VXLAN port must be on `br-int`,
have a positive ofport and flow-based tunnel options; inspect the flows for
the remote MAC, VNI and VTEP, not merely the port's existence. On the remote
host check `bridge -d fdb show dev vxlan1000` for **both** local MACs with
their respective VTEPs. If any check fails, stop rather than create manual
flows or additional attachments.

### BUM, ARP and safe cleanup

This example's bounded target is **IPv4 known-unicast**, not complete
Ethernet segment extension. The Type-2 importer does not implement Type-3
membership reconciliation or provide a general BUM replication guarantee.
OVN may create BUM groups from discovered VTEPs, but the example does not
rely on their behavior; the FRR-side zero-MAC flood entry used by the
single-worker fixture must not be blindly duplicated for two workers. For an
isolated unicast probe an administrator can install static *workload*
neighbors: in A and B's Pod network namespaces map `10.244.240.30` to
`02:00:00:00:00:30`; in `remote-workload` map `.11` and `.12` to their
respective `02:00:00:00:00:11` and `:12` MACs. For example, on the remote:

```sh
ip -n remote-workload neigh replace 10.244.240.11 lladdr 02:00:00:00:00:11 nud permanent dev frr-work0
ip -n remote-workload neigh replace 10.244.240.12 lladdr 02:00:00:00:00:12 nud permanent dev frr-work0
```

On each worker run `ip neigh replace 10.244.240.30 lladdr
02:00:00:00:00:30 nud permanent dev net1` **inside the appropriate Pod network
namespace**, using an administrator-authorized namespace entry method.
Verify the namespace and Pod UID first; the sample Pod does not grant
`NET_ADMIN`. These are not FDB entries and must not be used to fake
route-driven forwarding. Static neighbors bypass ARP bootstrap; they do not
prove ARP suppression, ND, unknown-unicast or multicast support. Do not rely
on this example for services needing BUM, IPv6/ND or multi-homing until
separately implemented and validated.

Node-Agent deletes only endpoint objects and imported entries covered by its
ownership receipts; it withdraws only its acknowledged BGP UUIDs.
ovn-controller owns its tunnel and flows. Administrators own the three Linux
devices, switch configuration and remote devices: after controlled endpoint
and route withdrawal, stop the relevant agents, verify exact ownership and
absence of dependencies, then remove only those specifically provisioned
resources. Preserve journals, foreign entries, unrelated OVS state and
existing OVN configuration; do not flush a bridge or delete `br-int`.

## Verify and recover

On **each host**, as root, inspect
`/var/lib/evpn-worker/inventory/journal.json` and compare
`Record.State.installationUID`, `nodeUID`, `nodeName`, `revision`, and
`digest` to the CR, Node and published ConfigMap (`data.sha256`).
`Record.Pending` must be null. This local inspection is an administrative
observation, not a remotely authenticated Operator acknowledgement; a root
writer can alter it. The journal's envelope checksum and the running agent's
normal recovery checks must also succeed. Never edit the journal to clear an
intent. Read the host's `/run/evpn-node-agent/export-status.json` and
`import-status.json` for errors and recovery state; an old status file alone
is not proof of present health.

For example, obtain `data.sha256` and `data.revision` from the published
ConfigMap, then run this on each host with `INSTALLATION_UID`, `NODE_UID`,
`NODE_NAME`, `CLUSTER_UID`, `REVISION`, and `INVENTORY_SHA256` set to the
independently observed values. Run as root; do not copy credentials or journal
contents to an untrusted machine:

```sh
sudo env INSTALLATION_UID="$INSTALLATION_UID" NODE_UID="$NODE_UID" NODE_NAME="$NODE_NAME" \
  CLUSTER_UID="$CLUSTER_UID" REVISION="$REVISION" INVENTORY_SHA256="$INVENTORY_SHA256" \
  python3 - <<'PY'
import hashlib, json, os
p = "/var/lib/evpn-worker/inventory/journal.json"
with open(p, encoding="utf-8") as f:
    text = f.read()
prefix = '{"Record":'
if not text.startswith(prefix):
    raise SystemExit("unexpected journal envelope")
record, end = json.JSONDecoder().raw_decode(text, len(prefix))
envelope = json.loads(text)
digest = hashlib.sha256(text[len(prefix):end].encode()).hexdigest()
if digest != envelope["SHA256"] or record["Pending"] is not None:
    raise SystemExit("invalid checksum or unresolved intent")
s = record["State"]
expected = {"installationUID": os.environ["INSTALLATION_UID"], "nodeUID": os.environ["NODE_UID"],
            "nodeName": os.environ["NODE_NAME"], "clusterUID": os.environ["CLUSTER_UID"],
            "revision": int(os.environ["REVISION"]), "digest": os.environ["INVENTORY_SHA256"]}
if any(s.get(k) != v for k, v in expected.items()):
    raise SystemExit("accepted fence differs from current published assignment")
print("local committed inventory fence matches publication")
PY
```

This verifies the committed fence snapshot and checksum, not current runtime
health or the independent integrity of a compromised host.

On both speakers run `gobgp --target unix:///run/gobgp/gobgp.sock neighbor`
and `gobgp --target unix:///run/gobgp/gobgp.sock global rib -a evpn -j`.
On FRR use `vtysh -c 'show bgp l2vpn evpn summary'` and
`vtysh -c 'show bgp l2vpn evpn route json'`. Check both local endpoint routes
and the remote MAC/IP, RT, VNI and next-hop/VTEP, not merely BGP Established.
Inspect `bridge -d fdb show dev evpnvx1000` and
`ip neigh show dev evpnbr1000` on each worker and equivalent remote entries.
After independently verifying the native OVN attachment on each worker,
test bidirectional endpoint traffic and capture UDP/4789 on both underlays.

For withdrawal in a disposable deployment, delete only a test Pod, inspect
its exact route and owned receipts until absent, and ensure the other endpoint
and unrelated routes remain. For restart, preserve journals, restart one agent
only after recording exact receipts and current observations, then inspect
recovery status and matching RIB/FDB/neighbor state. An ambiguous AddPath or
pending intent is **blocked**, not permission to clear state. Legacy
migration and reservation loss require the separately documented manual
ownership procedure in [Operator lifecycle](../../../../docs/architecture/evpn-core-operator.md).
