# Two-worker Type-2 live validation runbook

**Current inventory-gated two-worker result: BLOCKED.** Historical
two-chassis standalone and single-chassis Operator/FRR runs passed in
different topologies; see the [evidence record](../../../../docs/archive/validation-record.md#standalone-multi-chassis-g7b5b2).
This is a procedure, not evidence that the
template works. Execute it only on an explicitly authorized disposable
cluster, two dedicated workers and a dedicated FRR host. Do not use the
historical VM, a shared cluster, or production. Record ownership and obtain
permission before provisioning anything. A Kubernetes context or Docker
daemon alone is not evidence that an environment is disposable.

The scope is IPv4 known-unicast with static workload neighbors. It does not
validate BUM, ARP bootstrap, IPv6/ND or EVPN multihoming. The shared OVN
switch on two chassis and the example's `ExporterSource: cni` configuration
have not yet passed this test. Never add Linux-to-OVS patch ports or manual
OVN OpenFlow rules as a workaround.

## Preparation and result rules

1. Follow [README staging and activation](README.md#staging-and-activation)
   exactly, including allocating reachable addresses, rendering all templates,
   installing the independent speakers and FRR, trust, and creating the CR
   disabled before obtaining its UID. Apply `workloads.yaml` only after
   enabling the CR and verifying inventory publication. Follow its local
   journal checksum procedure on **each** host. The manifest contains
   placeholders, not ready-to-apply values.
2. Record software versions and immutable image/build identities, date, exact
   Kubernetes context and cluster identity, Node names/UIDs, chassis names
   and UUIDs, OVN NB/SB endpoints, host namespaces and distinct underlay
   interfaces. Verify the selected context is the authorized isolated cluster
   before **each** Kubernetes operation. These commands do not make a cluster
   disposable:

   ```sh
   kubectl config current-context
   kubectl get nodes -o wide
   kubectl -n evpn-system get evpncore evpn-core -o yaml
   kubectl -n evpn-system get configmap evpn-worker-inventory -o json
   kubectl -n evpn-system get daemonset evpn-node-agent -o yaml
   kubectl -n evpn-system get pods -o wide
   ```

3. Use a restricted evidence directory on each participating machine.
   Record command, timestamp, exit status, actual host and worker, full
   output, and input identities. Status files can be stale: capture two
   successive snapshots with increasing `Time` and check `Error`, recovery
   state and journal intents. Do not copy secrets, kubeconfigs or private
   keys into evidence. Journal contents may contain sensitive topology.
4. Mark a gate **PASS** only when all its stated observations exist and
   match. Mark a contradictory observation **FAIL** and unavailable evidence
   **BLOCKED**. Stop on uncertainty or ownership conflict. Do not repair by
   deleting journals, foreign resources, or manually inserting endpoint FDB
   entries. Log both worker outcomes separately.

The example's literal values are shown below; replace **all** of them with
the independently provisioned values when the environment differs:

| Identity | A | B | FRR remote |
|---|---|---|---|
| VTEP and BGP address | 192.0.2.11 | 192.0.2.12 | 192.0.2.30 |
| Endpoint IPv4 | 10.244.240.11 | 10.244.240.12 | 10.244.240.30 |
| Endpoint MAC | 02:00:00:00:00:11 | 02:00:00:00:00:12 | 02:00:00:00:00:30 |
| Network / VNI / RT | tenant-a / 1000 / 64512:1000 | same | same |

Set shell variables on the administrator machine, substituting actual Node
names. `kubectl` commands below require access to this one disposable cluster:

```sh
export NODE_A='worker-a' NODE_B='worker-b'
export POD_A='tenant-a-worker-a' POD_B='tenant-a-worker-b'
kubectl -n evpn-system get pod "$POD_A" "$POD_B" -o wide
kubectl get node "$NODE_A" "$NODE_B" -o json
```

The two `nodeName` fields, Kubernetes Node UIDs, inventory assignment,
per-node configuration, chassis and local VTEPs must agree exactly. Do not
put a second Pod on either fixed-IP NAD. Use a client image that actually
contains `ip` and `ping`.

## 1. Inventory, identity and OVN preflight

On the administrator machine obtain the actual CR UID, cluster UID,
ConfigMap revision and SHA-256, ConfigMap UID and each Node UID. Compare
them with the DaemonSet arguments and the local accepted inventory journal
using the checksum-verification command in [README](README.md#verify-and-recover).
The journal must have no pending intent. Compare the resulting assignment with
the local files, and re-check after each restart. `InventoryAccepted` and
`RuntimeReady` conditions can remain Unknown; Pod readiness is not proof.

On **each worker**, in the network namespace and OVSDB context used by its
own ovn-controller, collect:

```sh
sudo cat /run/evpn-node-agent/export-status.json
sudo cat /run/evpn-node-agent/import-status.json
sudo systemctl show gobgpd -p MainPID -p ControlGroup -p ActiveState
sudo sha256sum /usr/local/bin/gobgpd /etc/evpn/gobgpd.yaml
sudo ovs-vsctl --bare get Open_vSwitch . external_ids:system-id
sudo ovs-vsctl get Open_vSwitch . external_ids
sudo ip -d link show evpnbr1000
sudo ip -d link show evpnvx1000
sudo ip -d link show evpnadv1000
sudo bridge link show
```

The export status must show an error-free, fresh observation and the
`SpeakerProcess` PID, start ticks, executable and SHA-256 matching the
actually running local service and provisioned attestation, not merely a
listening socket. Both runtime statuses must advance without errors. Confirm
the exporter observations and receipts belong to that node's authorized
Pod only, and that recovery domains and journal paths are distinct across
workers. Validate that the bridge has the VXLAN and advertisement devices,
the VXLAN VNI is 1000, its local VTEP is the worker's own address and its
destination port is 60100. Inspect the active configuration, not just files
on the administrator machine.

From the shared NB/SB client and separately on each worker:

```sh
ovn-nbctl get Logical_Switch tenant-a _uuid other_config
ovn-nbctl --columns=name,_uuid,addresses,external_ids list Logical_Switch_Port
ovn-sbctl --columns=logical_port,chassis,up list Port_Binding
ovn-sbctl --columns=name,_uuid list Chassis
sudo ovs-vsctl --columns=name,ofport,external_ids list Interface
sudo ovs-vsctl list-ports br-int
```

Match each actual CNI-created LSP and its MAC/IP to the correct Pod UID and
switch; match its `iface-id` on the correct worker, positive OVS ofport,
`Port_Binding.up`, and chassis UUID to the worker's unique system-id.
Confirm switch VNI and all three device-name options, and each worker's
`ovn-evpn-local-ip` and `ovn-evpn-vxlan-ports=4789`. The two workers must
connect to the same NB/SB domain. The local Geneve baseline is separately
verified using both local endpoints and an underlay capture; do not infer it
from the external EVPN test.

## 2. BGP and receipt-owned import

Run on each worker using its local Unix socket:

```sh
sudo gobgp --target unix:///run/gobgp/gobgp.sock neighbor
sudo gobgp --target unix:///run/gobgp/gobgp.sock global rib -a evpn -j
sudo cat /run/evpn-node-agent/export-status.json
sudo cat /run/evpn-node-agent/import-status.json
sudo cat /var/lib/evpn-worker/export/journal.json
sudo cat /var/lib/evpn-worker/import/receipts-v1/journal.json
sudo bridge -d fdb show dev evpnvx1000
sudo ip neigh show dev evpnbr1000
```

On the dedicated FRR host:

```sh
sudo vtysh -c 'show bgp l2vpn evpn summary'
sudo vtysh -c 'show bgp l2vpn evpn route json'
sudo bridge -d fdb show dev vxlan1000
sudo ip neigh show dev br1000
```

For **each** worker require Established to FRR, the local endpoint's Type-2
MAC/IP, RD, RT, VNI, VXLAN encapsulation and local VTEP next hop, and the
same route at FRR. Match each `Result.Applied[].PathUUID` to its committed
export journal receipt, and ensure the endpoint is authorized and locally
bound. Require FRR's FDB entry for each local MAC to select its correct
distinct worker VTEP. Check the remote endpoint advertisement separately:
each worker must receive and validate `10.244.240.30` /
`02:00:00:00:00:30`, VNI 1000, RT `64512:1000`, next hop `192.0.2.30`.
Require an authoritative import snapshot and a matching durable receipt
whose FDB and neighbor are `confirmed` with `ACK=true`, no unresolved intent,
and matching actual Linux FDB and neighbor. Two complete observations must
converge; an error or non-authoritative snapshot is not a PASS.

If FRR does not advertise the remote endpoint, inspect its live bridge,
neighbor and BGP configuration and endpoint traffic. Do not fabricate an
imported route or unicast FDB entry. BGP Established and FDB receipt alone
are not forwarding evidence.

## 3. OVN/OVS and bidirectional packet evidence

Run the [native attachment verification commands](README.md#native-forwarding-attachment-and-ownership)
on both workers after remote import. Confirm a controller-created VXLAN
interface on `br-int`, positive ofport, intended flow-based options and
flows resolving the remote MAC/VNI/VTEP. Record the full `dump-flows` and
before/after `dump-ports` output. Interface presence alone does not prove a
correct flow.

Install the static **workload** neighbors as in
[BUM, ARP and safe cleanup](README.md#bum-arp-and-safe-cleanup), after
verifying each Pod's UID and network namespace. The sample Pods do not have
NET_ADMIN: an authorized host administrator must enter the matching Pod
network namespace. On the host, use `sudo crictl pods --name "$POD_A"` (or
`"$POD_B"`) and `sudo crictl inspectp "$SANDBOX_ID"` to identify the exact
Pod sandbox by namespace, name and Kubernetes UID. Obtain its live sandbox
PID from the runtime inspection, set `POD_PID` to that numeric PID and
independently verify its `/proc/$POD_PID/ns/net` and `net1` MAC/IP against
the Pod. Runtime versions differ; if the inspection does not provide a
verifiable PID, stop rather than guess. In the verified namespace run:

```sh
sudo nsenter -t "$POD_PID" -n ip addr show dev net1
sudo nsenter -t "$POD_PID" -n ip neigh replace 10.244.240.30 lladdr 02:00:00:00:00:30 nud permanent dev net1
sudo nsenter -t "$POD_PID" -n ip neigh show dev net1
```

Check the two remote-workload neighbors too. Do not preseed any
remote-unicast FDB.

Choose and record the actual underlay interface on A, B and FRR using
`ip route get 192.0.2.30` on each worker and `ip route get 192.0.2.11`
and `ip route get 192.0.2.12` on FRR. In a separate terminal on **each**
host, start a bounded capture immediately before probes; use three different
local paths in restricted evidence directories:

```sh
sudo timeout 90 tcpdump -U -n -i "$UNDERLAY_IF" -s 0 -w "$PCAP" 'udp port 4789'
```

`UNDERLAY_IF` and `PCAP` must be set on each host to its own verified
interface and writable evidence path. A timeout exit of 124 is expected when
the capture reaches 90 seconds; other capture failures block the gate. Start
all captures and verify they are running before sending traffic. Run:

```sh
kubectl -n evpn-system exec "$POD_A" -- ping -I net1 -c 3 -W 2 10.244.240.30
kubectl -n evpn-system exec "$POD_B" -- ping -I net1 -c 3 -W 2 10.244.240.30
```

On FRR, within the remote workload namespace:

```sh
sudo ip netns exec remote-workload ping -I frr-work0 -c 3 -W 2 10.244.240.11
sudo ip netns exec remote-workload ping -I frr-work0 -c 3 -W 2 10.244.240.12
```

Inspect each completed capture with `sudo tcpdump -nn -e -vv -r "$PCAP"`,
and preserve its SHA-256. For **each direction and each worker**, require
all three ICMP requests and replies with the expected outer source/destination
VTEPs, UDP 4789, VNI 1000 and inner MAC/IP pair, on both corresponding
underlays. Compare OVS VXLAN port counters before/after. Ping output without
attributable packet evidence is insufficient. Verify local A/B traffic
independently with both directions and captures of the actual Geneve
underlays; require UDP 6081 and the appropriate OVN datapath tunnel key.
After independently provisioning static neighbors between the two workload
namespaces, start bounded `tcpdump` captures on their Geneve underlay
interfaces using the same capture command with filter `udp port 6081`, and
run:

```sh
kubectl -n evpn-system exec "$POD_A" -- ping -I net1 -c 3 -W 2 10.244.240.12
kubectl -n evpn-system exec "$POD_B" -- ping -I net1 -c 3 -W 2 10.244.240.11
```

For these two workload neighbors use the verified `POD_PID` separately on
each host: `sudo nsenter -t "$POD_PID" -n ip neigh replace 10.244.240.12
lladdr 02:00:00:00:00:12 nud permanent dev net1` on A and the corresponding
`.11` / `02:00:00:00:00:11` on B.

## 4. Controlled recovery and exact withdrawal

Perform separately for A and B, only after the above gates pass. Preserve
before/after copies of statuses, committed journals, RIBs, FDBs, bindings,
speaker PID/start ticks and inventory checksum. Record a known unrelated
route already present in the isolated fabric, including its source, MAC/IP,
RT and next hop. If none exists, foreign-route preservation is **BLOCKED**;
do not manufacture an unowned route in a shared system.

1. **Agent restart:** identify the exact DaemonSet Pod running on the target
   Node via `kubectl -n evpn-system get pods -o wide`; save its UID, then
   delete only that Pod by its verified name. Wait for its replacement and
   collect fresh statuses, local inventory journal and matching receipts.
   The speaker incarnation must remain unchanged. Require both gates to
   converge and repeat the packet test. If the process or journal identity
   changes unexpectedly, stop and report blocked.
2. **BGP reconnect:** on the dedicated FRR host, after confirming the
   intended neighbor and permission, reset just the target worker's BGP
   session with `sudo vtysh -c 'clear bgp 192.0.2.11'` for A, or substitute
   `192.0.2.12` for B. Check the installed FRR version supports this command
   first. Capture the down/up transition and require Established again,
   same GoBGP PID/start ticks, authoritative RIB, receipts, FDB/neighbor
   and both packet directions. A changed speaker incarnation or ambiguous
   AddPath outcome is a fail-closed **BLOCKED** recovery, not a reason to
   clear state. This tests peer reconnection, not a speaker process restart.
3. **Exact local withdrawal:** save the target Pod UID and its export
   `PathUUID`; verify the other worker and foreign route are present. Delete
   only the target example Pod with `kubectl -n evpn-system delete pod
   "$POD_A" --wait=true` (or `$POD_B`). Observe its successful CNI DEL,
   matching receipt retirement and route absence at its speaker and FRR;
   verify the other worker's route and unrelated route remain, and the
   other endpoint still forwards. Record the actual route UUID and all
   before/after evidence. Never issue broad DeletePath or FDB cleanup.

To test B as well after withdrawing A, first restore A by reapplying the
rendered workload manifest only after the old UID, old route and receipts
are confirmed retired; recheck all gates and record the new Pod UID and new
receipt. A missing explicit foreign route makes only that preservation gate
BLOCKED, not an implied PASS.

For imported-state reconciliation, repeat the status and FDB/neighbor
comparison after each agent restart and peer reconnect, then repeat traffic
in both directions. Do not force-delete imported entries to simulate failure.
Stop if any journal reports pending or ambiguous state, an identity changes,
or foreign state is altered. No step authorizes deletion of reservations,
devices, bridges or journals.

## Record

| Gate | A | B | Evidence required |
|---|---|---|---|
| Inventory acceptance and identity | BLOCKED | BLOCKED | Published identity, local checksum-verified fence, fresh statuses |
| Speaker/BGP and local Type-2 export | BLOCKED | BLOCKED | Attestation, peer session, exact UUID and FRR RIB |
| Remote Type-2 and Linux import | BLOCKED | BLOCKED | Authoritative snapshot, receipts, FDB and neighbor |
| OVN/OVS port and flows | BLOCKED | BLOCKED | NB/SB binding, OVS port/options/flow and counters |
| Bidirectional external IPv4 packets | BLOCKED | BLOCKED | Ping and attributed captures on both underlays |
| Native local Geneve A↔B | BLOCKED | BLOCKED | Bidirectional traffic and Geneve captures |
| Agent restart / accepted inventory | BLOCKED | BLOCKED | Before/after identities, fence, journals and packets |
| BGP reconnect / import reconciliation | BLOCKED | BLOCKED | Session transition, same speaker identity, RIB and packets |
| Exact withdrawal / foreign preservation | BLOCKED | BLOCKED | Original UUID, before/after route and other endpoint |

These are initial placeholders, not automatically updated test results.
Record actual PASS/FAIL/BLOCKED, timestamp, artifact paths and reasons per
gate. Preserve evidence and perform only separately authorized,
identity-checked teardown.
