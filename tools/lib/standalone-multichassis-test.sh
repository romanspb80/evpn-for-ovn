#!/usr/bin/env bash
# Opt-in extension of the existing standalone smoke, not a separate runner.

multichassis_exec() {
  local chassis="$1"; shift
  if [[ "${chassis}" == a ]]; then
    standalone_smoke_agent_exec "$@"
  else
    timeout --kill-after=2s 20s docker exec "${MULTICHASSIS_CONTAINER}" "$@" </dev/null
  fi
}

multichassis_b_scope() (
  EVIDENCE_DIR="${EVIDENCE_DIR}/chassis-b"
  LOCAL_VTEP=198.18.0.5
  FRR_PEER=198.18.0.2
  NODE_UID="${MULTICHASSIS_NODE_UID}"
  LOCAL_CHASSIS="${MULTICHASSIS_CHASSIS}"
  DAEMONSET_UID="${MULTICHASSIS_SERVICE_UID}"
  PROVIDER_POD=""
  STANDALONE_NB_ENDPOINT=tcp:198.18.0.2:6641
  STANDALONE_SB_ENDPOINT=tcp:198.18.0.2:6642
  standalone_smoke_agent_exec() { multichassis_exec b "$@"; }
  standalone_smoke_copy_to_agent() {
    docker exec -i "${MULTICHASSIS_CONTAINER}" sh -ec \
      "umask 077; mkdir -p \"\$(dirname '$2')\"; cat > '$2.next'; chmod 600 '$2.next'; mv '$2.next' '$2'" <"$1"
  }
  provider_exec() {
    if [[ "$1" == gobgp ]]; then shift; set -- gobgp --target unix:///run/gobgp/gobgp.sock "$@"; fi
    multichassis_exec b "$@"
  }
  "$@"
)

multichassis_start_b_agent() {
  multichassis_b_scope standalone_smoke_start_agent
}

multichassis_prepare() {
  MULTICHASSIS_CONTAINER="${KIND_CLUSTER}-chassis-b"
  mkdir -m 700 "${EVIDENCE_DIR}/chassis-b" "${EVIDENCE_DIR}/chassis-b/build"
  cp "${EVIDENCE_DIR}/gobgp-pin" "${EVIDENCE_DIR}/gobgpd-pin" "${EVIDENCE_DIR}/chassis-b/"
  cp "${EVIDENCE_DIR}/build/evpn-node-agent" "${EVIDENCE_DIR}/chassis-b/build/"
  local lab_name lab_owner
  read -r lab_name lab_owner < <(python3 - "${EVIDENCE_DIR}/lab.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]));print(s["name"],s["owner"])
PY
)
  docker create --name "${MULTICHASSIS_CONTAINER}" --network "${INTEROP_NETWORK}" \
    --ip 198.18.0.5 --privileged --init --cgroupns=host \
    --label "containerlab=${lab_name}" --label "evpn.e2e.owner=${lab_owner}" \
    --entrypoint /bin/sh "${OVN_IMAGE}" -ec '
      /usr/share/openvswitch/scripts/ovs-ctl --system-id=random start
      ovs-vsctl add-br br-int
      ip link set br-int up
      ovs-vsctl set Open_vSwitch . external_ids:ovn-remote=tcp:198.18.0.2:6642 \
        external_ids:ovn-encap-type=geneve external_ids:ovn-encap-ip=198.18.0.5 \
        external_ids:ovn-evpn-local-ip=198.18.0.5 external_ids:ovn-evpn-vxlan-ports=4789
      /usr/share/ovn/scripts/ovn-ctl start_controller
      exec sleep infinity
    ' >"${EVIDENCE_DIR}/chassis-b/container-id.txt"
  PYTHONDONTWRITEBYTECODE=1 python3 - "${ROOT}" "${EVIDENCE_DIR}" "${MULTICHASSIS_CONTAINER}" <<'PY'
import importlib.util,pathlib,sys
spec=importlib.util.spec_from_file_location("lab",pathlib.Path(sys.argv[1])/"tools/e2e/containerlab.py")
lab=importlib.util.module_from_spec(spec);spec.loader.exec_module(lab)
with lab.exclusive():lab.register_auxiliary(pathlib.Path(sys.argv[2]),sys.argv[3])
PY
  # Shared databases, but disjoint switches: no ordinary OVN inter-chassis
  # logical-switch tunnel can satisfy the endpoint packet assertions.
  rt ovs-appctl -t /var/run/ovn/ovnsb_db.ctl ovsdb-server/add-remote ptcp:6642:198.18.0.2
  docker start "${MULTICHASSIS_CONTAINER}"
  local deadline=$((SECONDS+90))
  until multichassis_exec b ovs-vsctl get Open_vSwitch . external_ids:system-id \
    >"${EVIDENCE_DIR}/chassis-b/system-id.txt" 2>"${EVIDENCE_DIR}/chassis-b/setup.stderr"; do
    (( SECONDS < deadline )) || return 1
    sleep 1
  done
  MULTICHASSIS_CHASSIS="$(tr -d '\"\n' <"${EVIDENCE_DIR}/chassis-b/system-id.txt")"
  [[ -n "${MULTICHASSIS_CHASSIS}" && "${MULTICHASSIS_CHASSIS}" != "${LOCAL_CHASSIS}" ]]
  read -r MULTICHASSIS_NODE_UID MULTICHASSIS_SERVICE_UID < <(
    python3 - "${EVIDENCE_DIR}/chassis-b/container-id.txt" <<'PY'
import pathlib,sys,uuid
identity=pathlib.Path(sys.argv[1]).read_text().strip()
print(uuid.uuid5(uuid.NAMESPACE_OID,"node:"+identity),uuid.uuid5(uuid.NAMESPACE_OID,"speaker:"+identity))
PY
  )
  multichassis_exec b ip link add evpnbr33 type bridge
  multichassis_exec b ip link set evpnbr33 up
  multichassis_exec b ip link add evpnvx33 type vxlan id 1000 local 198.18.0.5 dstport 60100 nolearning
  multichassis_exec b ip link set evpnvx33 master evpnbr33
  multichassis_exec b ip link set evpnvx33 up
  multichassis_exec b ip link add evpnad33 type dummy
  multichassis_exec b ip link set evpnad33 master evpnbr33
  multichassis_exec b ip link set evpnad33 up
  rt ovn-nbctl -- ls-add ls24c \
    -- set Logical_Switch ls24c other_config:dynamic-routing-vni=1000 \
      other_config:dynamic-routing-bridge-ifname=evpnbr33 \
      other_config:dynamic-routing-vxlan-ifname=evpnvx33 \
      other_config:dynamic-routing-advertise-ifname=evpnad33 \
      other_config:dynamic-routing-redistribute=fdb,ip \
    -- lsp-add ls24c standalone-b \
    -- lsp-set-addresses standalone-b '0a:58:f0:00:00:14 10.244.240.20' \
    -- set Logical_Switch_Port standalone-b \
      external_ids:evpn.framework/network-id=n24 \
      external_ids:evpn.framework/endpoint-id=standalone-b \
      external_ids:evpn.framework/attachment-id=standalone-b-attachment \
      external_ids:evpn.framework/attachment-generation=1 \
      options:requested-chassis="${MULTICHASSIS_CHASSIS}"
  multichassis_exec b ip netns add endpoint-b
  multichassis_exec b ip link add endpoint-b-host type veth peer name endpoint-b
  multichassis_exec b ip link set endpoint-b netns endpoint-b
  multichassis_exec b ip netns exec endpoint-b ip link set lo up
  multichassis_exec b ip netns exec endpoint-b ip link set endpoint-b address 0a:58:f0:00:00:14
  multichassis_exec b ip netns exec endpoint-b ip addr add 10.244.240.20/24 dev endpoint-b
  multichassis_exec b ip netns exec endpoint-b ip link set endpoint-b up
  multichassis_exec b ip link set endpoint-b-host up
  multichassis_exec b ovs-vsctl add-port br-int endpoint-b-host \
    -- set Interface endpoint-b-host external_ids:iface-id=standalone-b
  rt ovn-nbctl --wait=hv sync
  rt ovn-nbctl --bare get Logical_Switch_Port standalone-b _uuid \
    >"${EVIDENCE_DIR}/chassis-b/lsp-uuid.txt"
  multichassis_b_scope standalone_smoke_configure_speaker
  multichassis_b_scope standalone_smoke_attest_speaker
  python3 - "${EVIDENCE_DIR}" "${MULTICHASSIS_NODE_UID}" "${MULTICHASSIS_CHASSIS}" "${MULTICHASSIS_CONTAINER}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);b=p/"chassis-b";node,chassis,name=sys.argv[2:]
export=json.loads((p/"standalone-export-config.json").read_text())
export.update(NodeName=name,LocalChassis=chassis,RouterID="198.18.0.5")
export["Recovery"]["Domain"]["NodeUID"]=node
export["Networks"][0].update(LogicalSwitch="ls24c",LocalVTEP="198.18.0.5",RouteDistinguisher="198.18.0.5:1000")
remote=json.loads((p/"standalone-import-config.json").read_text())
remote["RouterID"]="198.18.0.5";remote["Import"]["LocalVTEP"]="198.18.0.5"
remote["Import"]["Networks"][0]["AllowedVTEPs"]=["198.18.0.2"]
remote["Netlink"]["Networks"][0]["LogicalSwitch"]="ls24c"
remote["Netlink"]["Recovery"]["Domain"]["NodeUID"]=node
runtime=json.loads((p/"standalone-runtime.json").read_text())
runtime.update(nodeUID=node,localChassis=chassis)
policy={"version":1,"revision":1,"installationUID":runtime["installationUID"],"nodeUID":node,"localChassis":chassis,
        "endpoints":[{"networkID":"n24","endpointID":"standalone-b","attachmentID":"standalone-b-attachment",
         "attachmentGeneration":1,"logicalSwitchPort":"standalone-b","logicalSwitchPortUUID":(b/"lsp-uuid.txt").read_text().strip(),
         "mac":"0a:58:f0:00:00:14","ip":"10.244.240.20"}]}
for file,data in (("standalone-export-config.json",export),("standalone-import-config.json",remote),
                  ("standalone-runtime.json",runtime),("standalone-policy-1.json",policy)):
    (b/file).write_text(json.dumps(data)+"\n")
PY
  for pair in standalone-export-config.json:export-config.json \
    standalone-import-config.json:import-config.json standalone-runtime.json:runtime.json \
    standalone-policy-1.json:authorization.json standalone-speaker.json:speaker.json; do
    multichassis_b_scope standalone_smoke_copy_to_agent \
      "${EVIDENCE_DIR}/chassis-b/${pair%%:*}" "/run/evpn-node-agent/standalone-smoke/${pair#*:}"
  done
  multichassis_exec b chmod 700 /run/evpn-node-agent/standalone-smoke
  multichassis_exec b install -d -m 700 /run/evpn-node-agent/standalone-smoke/import-receipts
  multichassis_start_b_agent
  docker inspect "${MULTICHASSIS_CONTAINER}" >"${EVIDENCE_DIR}/chassis-b/container.json"
  rt ovn-sbctl --format=json list Chassis >"${EVIDENCE_DIR}/multichassis-chassis.json"
  rt ovn-sbctl --format=json list Port_Binding >"${EVIDENCE_DIR}/multichassis-bindings.json"
}

multichassis_capture_snapshot() {
  local phase="$1" chassis
  for chassis in a b; do
    local prefix="multi-${phase}-${chassis}"
    for pair in export-status:/run/evpn-node-agent/export-status.json \
      import-status:/run/evpn-node-agent/remote-status.json \
      export-journal:/run/evpn-node-agent/standalone-smoke/export-receipts/journal.json \
      import-journal:/run/evpn-node-agent/standalone-smoke/import-receipts/receipts-v1/journal.json \
      fence:/run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
      agent-log:/run/evpn-node-agent/standalone-smoke/agent.log; do
      standalone_smoke_capture_command "${prefix}-${pair%%:*}" \
        multichassis_exec "${chassis}" cat "${pair#*:}"
    done
    standalone_smoke_capture_command "${prefix}-rib" \
      multichassis_exec "${chassis}" gobgp --target unix:///run/gobgp/gobgp.sock global rib -a evpn -j
    standalone_smoke_capture_command "${prefix}-peer" \
      multichassis_exec "${chassis}" gobgp --target unix:///run/gobgp/gobgp.sock neighbor -j
    standalone_smoke_capture_command "${prefix}-fdb" multichassis_exec "${chassis}" bridge -j fdb show dev evpnvx33
    standalone_smoke_capture_command "${prefix}-neighbor" multichassis_exec "${chassis}" ip -j neigh show dev evpnbr33
    standalone_smoke_capture_command "${prefix}-links" multichassis_exec "${chassis}" ip -d -j link show
    standalone_smoke_capture_command "${prefix}-flows" multichassis_exec "${chassis}" ovs-ofctl dump-flows br-int
    standalone_smoke_capture_command "${prefix}-ovs" multichassis_exec "${chassis}" ovs-vsctl --format=json list Interface
    standalone_smoke_capture_command "${prefix}-processes" multichassis_exec "${chassis}" sh -ec \
      'for f in /run/gobgp/gobgpd.pid /run/evpn-node-agent/standalone-smoke/agent.pid; do pid=$(cat "$f"); kill -0 "$pid"; cat "/proc/$pid/stat"; done; test ! -S /run/evpn-node-agent/agent.sock'
  done
}

multichassis_snapshot() {
  local phase="$1"
  multichassis_capture_snapshot "${phase}"
  python3 - "${EVIDENCE_DIR}" "${phase}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);phase=sys.argv[2]
def read(node,name):return json.loads((p/f"multi-{phase}-{node}-{name}.stdout").read_text())
owners=[];uuids=[];netns=[];pidns=[];nodes=[];installations=[]
for node,local,remote,ip,vtep,count in (("a","0a:58:f0:00:00:0a","0a:58:f0:00:00:14","10.244.240.20","198.18.0.5",2),
                                     ("b","0a:58:f0:00:00:14","0a:58:f0:00:00:0a","10.244.240.10","198.18.0.2",1)):
    e=read(node,"export-status");s=read(node,"import-status")
    assert not e.get("Error") and not s.get("Error"),(e,s)
    assert e["Result"]["RecoveryState"]==s["RecoveryState"]=="RUNNING"
    assert s["Snapshot"]["Authoritative"] and len(s["Receipts"])==count
    imports=read(node,"import-journal")["Record"]["State"]
    assert not imports["Quarantine"] and len(imports["Receipts"])==count
    assert sorted(imports["Receipts"].values(),key=lambda x:x["Endpoint"]["MAC"])==sorted(s["Receipts"],key=lambda x:x["Endpoint"]["MAC"])
    assert len(e["Result"]["Applied"])==1 and e["Result"]["Applied"][0]["Endpoint"]["MACAddress"]==local
    uuids.append(e["Result"]["Applied"][0]["PathUUID"])
    speaker=e["SpeakerProcess"];assert speaker["PID"]>0 and speaker["standalone"]["routerID"]==("198.18.0.2" if node=="a" else "198.18.0.5")
    pidns.append(speaker["PIDNamespace"]);nodes.append(speaker["standalone"]["nodeUID"])
    installations.append(speaker["standalone"]["installationUID"])
    netns.append(read(node,"import-journal")["Record"]["Domain"]["NetworkNamespace"])
    for name in ("export-journal","import-journal","fence"):
        assert read(node,name)["Record"]["Pending"] is None
    assert read(node,"fence")["Record"]["State"]["revision"]==1
    r=next(x for x in s["Receipts"] if x["Endpoint"]["MAC"]==remote)
    assert (r["Endpoint"]["IP"],r["Endpoint"]["RemoteVTEP"])==(ip,vtep)
    assert r["Network"]=={"NetworkID":"n24","LogicalSwitch":"ls24b" if node=="a" else "ls24c",
                         "VNI":1000,"BridgeDevice":"evpnbr33","VXLANDevice":"evpnvx33"}
    assert all(x["Route"]["Classification"]=="remote" for x in r["Endpoint"]["Routes"])
    assert all(x["Route"]["Source"]["NeighborIP"]==vtep for x in r["Endpoint"]["Routes"])
    assert all(x["Endpoint"]["MAC"]!=local for x in s["Receipts"])
    for kind in ("FDB","Neighbor"):assert r[kind]["ACK"] and r[kind]["Status"]=="confirmed"
    owners.append(r["Controller"])
    assert len(read(node,"export-journal")["Record"]["State"]["Receipts"])==1
    assert read(node,"export-journal")["Record"]["State"]["Receipts"][0]["Owned"]["PathUUID"]==uuids[-1]
    assert any(x.get("mac")==remote and x.get("dst")==vtep and x.get("state")=="permanent" and
               {"self","extern_learn"}<=set(x.get("flags",[])) for x in read(node,"fdb"))
    assert any(x.get("dst")==ip and x.get("lladdr")==remote and "NOARP" in x.get("state",[]) for x in read(node,"neighbor"))
    links=read(node,"links");bridge=next(x for x in links if x["ifname"]=="evpnbr33")
    tunnel=next(x for x in links if x["ifname"]=="evpnvx33")
    assert bridge["linkinfo"]["info_kind"]=="bridge" and tunnel["master"]=="evpnbr33"
    assert tunnel["linkinfo"]["info_kind"]=="vxlan"
    data=tunnel["linkinfo"]["info_data"]
    assert data["id"]==1000 and data["port"]==60100 and data["local"]==("198.18.0.2" if node=="a" else "198.18.0.5")
    ovs=read(node,"ovs");interfaces=[dict(zip(ovs["headings"],row)) for row in ovs["data"]]
    assert any(x["type"]=="vxlan" and dict(x["options"][1])=={"dst_port":"4789","key":"flow","local_ip":"flow","remote_ip":"flow"} for x in interfaces)
    rib=read(node,"rib")
    local_paths=[x for key,paths in rib.items() if local in key for x in paths]
    remote_paths=[x for key,paths in rib.items() if remote in key for x in paths]
    assert len(local_paths)==len(remote_paths)==1,"duplicate or missing endpoint advertisement"
    assert not local_paths[0].get("peer-address") and remote_paths[0]["peer-address"]==vtep,"reflected/local route misclassification"
    peer=next(x for x in read(node,"peer") if x["state"]["neighbor_address"]==vtep)
    assert peer["state"]["session_state"]==6 and peer["state"]["peer_asn"]==64512
    assert any(x["state"]["family"]=={"afi":25,"safi":70} and x["state"]["accepted"]==1 for x in peer["afi_safis"])
assert len(set(owners))==len(set(uuids))==len(set(netns))==len(set(pidns))==len(set(nodes))==2
assert len(set(installations))==1
print("two independent attested exporters, authoritative remote importers, exact receipts and actual FDB/neighbor=PASS")
PY
}

multichassis_wait() {
  local phase="${1:-initial}" deadline=$((SECONDS+120))
  until multichassis_snapshot "${phase}" >"${EVIDENCE_DIR}/multi-observation-poll.log" 2>&1; do
    (( SECONDS < deadline )) || { cat "${EVIDENCE_DIR}/multi-observation-poll.log"; return 1; }
    sleep 2
  done
  cat "${EVIDENCE_DIR}/multi-observation-poll.log"
}

multichassis_stable() {
  python3 - "${EVIDENCE_DIR}" "$1" "$2" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);before,after=sys.argv[2:]
for node in ("a","b"):
    def read(phase,name):return json.loads((p/f"multi-{phase}-{node}-{name}.stdout").read_text())
    assert read(after,"export-status")["Time"]>read(before,"export-status")["Time"]
    assert read(after,"import-status")["Time"]>read(before,"import-status")["Time"]
    assert read(before,"export-status")["Result"]["Applied"]==read(after,"export-status")["Result"]["Applied"]
    assert read(before,"import-status")["Receipts"]==read(after,"import-status")["Receipts"]
    old=(p/f"multi-{before}-{node}-processes.stdout").read_text().splitlines()
    new=(p/f"multi-{after}-{node}-processes.stdout").read_text().splitlines()
    assert old[0].split()[:2]==new[0].split()[:2] and old[0].split()[21]==new[0].split()[21],"speaker restarted"
print("both loops advanced without UUID, importer receipt or speaker identity change=PASS")
PY
}

standalone_multichassis() {
  ACTIVE_CASE=multichassis_control_plane
  multichassis_prepare
  multichassis_wait
  case_status multichassis_control_plane PASS "two real independent chassis, direct GoBGP session, exact export receipts and remote native installation"
  ACTIVE_CASE=multichassis_forwarding
  multichassis_packets initial
  case_status multichassis_forwarding PASS "bidirectional endpoint ICMP with captured EVPN VXLAN/4789/VNI1000 underlay packets"
  ACTIVE_CASE=multichassis_stability
  sleep 4
  multichassis_snapshot stable
  multichassis_stable initial stable
  case_status multichassis_stability PASS "both reconciliation loops advanced with stable independent receipts and foreign FRR route"
  ACTIVE_CASE=multichassis_agent_recovery
  printf '%s stop-agent-a\n' "$(date -u +%FT%TZ)" >>"${EVIDENCE_DIR}/multi-restart-timeline.txt"
  standalone_smoke_stop_agent
  multichassis_packets stopped
  standalone_smoke_start_agent
  printf '%s restart-agent-a\n' "$(date -u +%FT%TZ)" >>"${EVIDENCE_DIR}/multi-restart-timeline.txt"
  multichassis_wait recovered
  multichassis_stable stable recovered
  python3 - "${EVIDENCE_DIR}" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1])
def identity(phase,node):
    line=(p/f"multi-{phase}-{node}-processes.stdout").read_text().splitlines()[1].split()
    return (line[0],line[21])
assert identity("stable","a")!=identity("recovered","a"),"agent A was not replaced"
assert identity("stable","b")==identity("recovered","b"),"agent B unexpectedly restarted"
print("only Node-Agent A changed PID/start time; Node-Agent B remained alive=PASS")
PY
  multichassis_packets recovered
  case_status multichassis_agent_recovery PASS "agent A only restarted; both speakers and B preserved; exact durable receipts and bidirectional traffic retained"
  if [[ "${EVPN_STANDALONE_LIFECYCLE_SMOKE:-0}" == 1 ]]; then
    multichassis_lifecycle
  fi
  ACTIVE_CASE=multichassis_cleanup
  multichassis_retire
  case_status multichassis_cleanup PASS "B policy revoked exact UUID, A removed B remote entries, B evidence retained and shared OVN switch cleaned"
}

multichassis_packets() {
  local phase="$1" pid
  multichassis_exec b sh -ec \
    'tcpdump --immediate-mode -U -n -i eth0 -s 0 -w "/tmp/multi-$1.pcap" "udp and host 198.18.0.2 and host 198.18.0.5" >"/tmp/multi-$1.capture.log" 2>&1 </dev/null & echo "$!" >"/tmp/multi-$1.capture.pid"' \
    capture "${phase}"
  sleep 2
  pid="$(multichassis_exec b cat "/tmp/multi-${phase}.capture.pid")"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  multichassis_exec b kill -0 "${pid}"
  local forward_rc=0 reverse_rc=0
  if [[ -n "${MULTICHASSIS_PACKET_A_NAMESPACE:-}" ]]; then
    standalone_smoke_capture_command "multi-${phase}-a-to-b" \
      multichassis_exec a ip netns exec "${MULTICHASSIS_PACKET_A_NAMESPACE}" \
      ping -I lifecycle-port -c 3 -W 2 "${MULTICHASSIS_PACKET_B_IP:-10.244.240.20}" || forward_rc=$?
  else
    standalone_smoke_capture_command "multi-${phase}-a-to-b" \
      k -n cni-test exec pod-a -- ping -I net1 -c 3 -W 2 "${MULTICHASSIS_PACKET_B_IP:-10.244.240.20}" || forward_rc=$?
  fi
  standalone_smoke_capture_command "multi-${phase}-b-to-a" \
    multichassis_exec b ip netns exec "${MULTICHASSIS_PACKET_B_NAMESPACE:-endpoint-b}" \
    ping -c 3 -W 2 "${MULTICHASSIS_PACKET_A_IP:-10.244.240.10}" || reverse_rc=$?
  sleep 2
  multichassis_exec b kill -INT "${pid}"
  for _ in $(seq 1 20); do
    if ! multichassis_exec b kill -0 "${pid}" 2>/dev/null; then break; fi
    sleep 1
  done
  if multichassis_exec b kill -0 "${pid}" 2>/dev/null; then
    echo "multi-chassis capture did not stop" >&2; return 1
  fi
  docker cp "${MULTICHASSIS_CONTAINER}:/tmp/multi-${phase}.pcap" "${EVIDENCE_DIR}/multi-${phase}.pcap"
  multichassis_exec b cat "/tmp/multi-${phase}.capture.log" >"${EVIDENCE_DIR}/multi-${phase}-capture.log"
  [[ "${forward_rc}" == 0 && "${reverse_rc}" == 0 ]]
  PYTHONDONTWRITEBYTECODE=1 python3 - "${ROOT}" "${EVIDENCE_DIR}/multi-${phase}.pcap" \
    "${MULTICHASSIS_PACKET_A_IP:-10.244.240.10}" "${MULTICHASSIS_PACKET_A_MAC:-0a:58:f0:00:00:0a}" \
    "${MULTICHASSIS_PACKET_B_IP:-10.244.240.20}" "${MULTICHASSIS_PACKET_B_MAC:-0a:58:f0:00:00:14}" \
    >"${EVIDENCE_DIR}/multi-${phase}-packet-verification.json" <<'PY'
import collections,importlib.util,json,pathlib,struct,sys
spec=importlib.util.spec_from_file_location("pcap",pathlib.Path(sys.argv[1])/"tools/lib/cni-vxlan-pcap-verify.py")
decoder=importlib.util.module_from_spec(spec);spec.loader.exec_module(decoder)
ip_a,mac_a,ip_b,mac_b=sys.argv[3:]
_,records=decoder.read_pcap(sys.argv[2]);counts=collections.Counter();identities=collections.defaultdict(set);evidence=[]
for r in records:
    frame=r["frame"];eth=decoder.ethernet_packet(frame)
    if not eth or eth[2]!=0x0800:continue
    outer=decoder.ipv4_packet(frame,eth[3])
    if not outer or outer["protocol"]!=17:continue
    offset=outer["payloadOffset"];srcport,dstport,length=struct.unpack_from("!HHH",frame,offset)
    if dstport!=4789:continue
    if frame[offset+8]!=8:continue
    vni=int.from_bytes(frame[offset+12:offset+15],"big")
    inner=decoder.inner_packet(frame,offset+16)
    if not inner or "icmp" not in inner:continue
    key=(inner["sourceIp"],inner["destinationIp"],inner["icmp"]["type"])
    if {key[0],key[1]}!={ip_a,ip_b}:continue
    expected_outer=("198.18.0.2","198.18.0.5") if key[0]==ip_a else ("198.18.0.5","198.18.0.2")
    assert (outer["source"],outer["destination"])==expected_outer and vni==1000
    assert inner["sourceMac"]==(mac_a if key[0]==ip_a else mac_b)
    assert inner["destinationMac"]==(mac_b if key[1]==ip_b else mac_a)
    counts[key]+=1;identities[key].add((inner["icmp"]["identifier"],inner["icmp"]["sequence"]))
    evidence.append({"outer":expected_outer,"vni":vni,"source":key[0],"destination":key[1],
                     "icmpType":key[2],"id":inner["icmp"]["identifier"],"sequence":inner["icmp"]["sequence"]})
for src,dst in ((ip_a,ip_b),(ip_b,ip_a)):
    req=(src,dst,8);reply=(dst,src,0)
    assert len(identities[req])==3 and identities[req]==identities[reply],dict(counts)
print(json.dumps({"status":"PASS","capture":sys.argv[2],"udpPort":4789,"vni":1000,
                  "matchedPackets":evidence},indent=2))
PY
}

multichassis_retire() {
  multichassis_exec b cat /run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
    >"${EVIDENCE_DIR}/multi-b-before-retire-fence.json"
  python3 - "${EVIDENCE_DIR}/chassis-b/standalone-policy-1.json" \
    "${EVIDENCE_DIR}/chassis-b/standalone-policy-2.json" "${EVIDENCE_DIR}/multi-b-before-retire-fence.json" <<'PY'
import json,pathlib,sys
s=json.loads(pathlib.Path(sys.argv[1]).read_text())
s.update(revision=json.loads(pathlib.Path(sys.argv[3]).read_text())["Record"]["State"]["revision"]+1,endpoints=[])
pathlib.Path(sys.argv[2]).write_text(json.dumps(s)+"\n")
PY
  multichassis_b_scope standalone_smoke_copy_to_agent \
    "${EVIDENCE_DIR}/chassis-b/standalone-policy-2.json" /run/evpn-node-agent/standalone-smoke/authorization.json
  local deadline=$((SECONDS+90))
  while :; do
    multichassis_exec b cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/multi-b-retired-status.json"
    standalone_smoke_agent_exec cat /run/evpn-node-agent/remote-status.json >"${EVIDENCE_DIR}/multi-a-drained-status.json"
    if python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);b=json.loads((p/"multi-b-retired-status.json").read_text());a=json.loads((p/"multi-a-drained-status.json").read_text())
assert not b.get("Error") and not a.get("Error") and not b["Result"]["Applied"]
assert a["Snapshot"]["Authoritative"] and len(a["Receipts"])==1
assert a["Receipts"][0]["Endpoint"]["MAC"]=="0a:58:f0:01:00:0c"
PY
    then break; fi
    (( SECONDS < deadline )) || return 1
    sleep 1
  done
  multichassis_exec b cat /run/evpn-node-agent/standalone-smoke/export-receipts/journal.json \
    >"${EVIDENCE_DIR}/multi-b-retired-journal.json"
  multichassis_exec b cat /run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
    >"${EVIDENCE_DIR}/multi-b-retired-fence.json"
  for node in a b; do
    standalone_smoke_capture_command "multi-retired-${node}-rib" \
      multichassis_exec "${node}" gobgp --target unix:///run/gobgp/gobgp.sock global rib -a evpn -j
  done
  standalone_smoke_capture_command multi-retired-a-fdb multichassis_exec a bridge -j fdb show dev evpnvx33
  standalone_smoke_capture_command multi-retired-a-neighbor multichassis_exec a ip -j neigh show dev evpnbr33
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
def read(name):return json.loads((p/name).read_text())
withdrawn=read("multi-b-retired-journal.json")["Record"]
assert withdrawn["Pending"] is None and not withdrawn["State"]["Receipts"]
assert read("multi-b-retired-fence.json")["Record"]["State"]["revision"]==read("multi-b-before-retire-fence.json")["Record"]["State"]["revision"]+1
for node in ("a","b"):
    rib=read(f"multi-retired-{node}-rib.stdout")
    assert not any("0a:58:f0:00:00:14" in key for key in rib)
    assert any("0a:58:f0:00:00:0a" in key for key in rib)
assert any("0a:58:f0:01:00:0c" in key for key in read("multi-retired-a-rib.stdout"))
assert all(x.get("mac")!="0a:58:f0:00:00:14" for x in read("multi-retired-a-fdb.stdout"))
assert all(x.get("dst")!="10.244.240.20" for x in read("multi-retired-a-neighbor.stdout"))
print("B exact receipt cleared under next durable revision; B route/FDB/neighbor absent; A and foreign FRR routes preserved=PASS")
PY
  multichassis_capture_diagnostics
  multichassis_b_scope standalone_smoke_stop_agent
  multichassis_exec b sh -ec 'kill -TERM "$(cat /run/gobgp/gobgpd.pid)"'
  docker logs "${MULTICHASSIS_CONTAINER}" >"${EVIDENCE_DIR}/chassis-b/final.log" 2>&1
  rt ovn-nbctl ls-del ls24c
  docker stop --time 10 "${MULTICHASSIS_CONTAINER}"
  # The auxiliary identity remains in the Containerlab ownership manifest;
  # its normal teardown removes the stopped container and shared network.
  MULTICHASSIS_CONTAINER=""
  interop_wait_import_count 1 multichassis-foreign-survivor standalone
}

multichassis_capture_diagnostics() {
  [[ -n "${MULTICHASSIS_CONTAINER}" ]] || return 0
  docker inspect "${MULTICHASSIS_CONTAINER}" >"${EVIDENCE_DIR}/chassis-b/final-container.json"
  docker logs "${MULTICHASSIS_CONTAINER}" >"${EVIDENCE_DIR}/chassis-b/final-container.log" 2>&1
  if [[ "$(docker inspect -f '{{.State.Running}}' "${MULTICHASSIS_CONTAINER}")" == true ]]; then
    multichassis_exec b sh -ec '
      for dir in /run/evpn-node-agent /run/gobgp /var/log/ovn /var/log/openvswitch; do
        if test -d "$dir"; then find "$dir" -type f \( -name "*.json" -o -name "*.log" -o -name "*.pid" \) -print; fi
      done' >"${EVIDENCE_DIR}/chassis-b/final-evidence-files.txt"
    local file
    while IFS= read -r file; do
      mkdir -p "${EVIDENCE_DIR}/chassis-b/final$(dirname "${file}")"
      multichassis_exec b cat "${file}" >"${EVIDENCE_DIR}/chassis-b/final${file}"
    done <"${EVIDENCE_DIR}/chassis-b/final-evidence-files.txt"
  fi
}

# Use a non-CNI workload so destructive OVN lifecycle operations cannot bypass
# or invalidate the original pod's independently authenticated CNI receipt.
multichassis_lifecycle_create() {
  local node="$1" generation="$2" switch chassis
  if [[ "${node}" == a ]]; then switch=ls24b; chassis="${LOCAL_CHASSIS}"
  else switch=ls24c; chassis="${MULTICHASSIS_CHASSIS}"; fi
  rt ovn-nbctl -- lsp-add "${switch}" lifecycle-a \
    -- lsp-set-addresses lifecycle-a '0a:58:f0:00:00:1e 10.244.240.30' \
    -- set Logical_Switch_Port lifecycle-a \
      external_ids:evpn.framework/network-id=n24 \
      external_ids:evpn.framework/endpoint-id=lifecycle-a \
      external_ids:evpn.framework/attachment-id="lifecycle-attachment-${generation}" \
      external_ids:evpn.framework/attachment-generation="${generation}" \
      options:requested-chassis="${chassis}"
  multichassis_lifecycle_attach "${node}"
  rt ovn-nbctl --wait=hv sync
  rt ovn-nbctl --bare get Logical_Switch_Port lifecycle-a _uuid \
    >"${EVIDENCE_DIR}/lifecycle-lsp-${generation}.txt"
  python3 - "${EVIDENCE_DIR}" "${generation}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);g=int(sys.argv[2])
r={"networkID":"n24","endpointID":"lifecycle-a","attachmentID":f"lifecycle-attachment-{g}",
   "attachmentGeneration":g,"logicalSwitchPort":"lifecycle-a",
   "logicalSwitchPortUUID":(p/f"lifecycle-lsp-{g}.txt").read_text().strip(),
   "mac":"0a:58:f0:00:00:1e","ip":"10.244.240.30"}
(p/f"lifecycle-record-{g}.json").write_text(json.dumps(r)+"\n")
PY
}

multichassis_lifecycle_attach() {
  local node="$1"
  multichassis_exec "${node}" ip netns add lifecycle-endpoint
  multichassis_exec "${node}" ip link add lifecycle-host type veth peer name lifecycle-port
  multichassis_exec "${node}" ip link set lifecycle-port netns lifecycle-endpoint
  multichassis_exec "${node}" ip netns exec lifecycle-endpoint ip link set lo up
  multichassis_exec "${node}" ip netns exec lifecycle-endpoint ip link set lifecycle-port address 0a:58:f0:00:00:1e
  multichassis_exec "${node}" ip netns exec lifecycle-endpoint ip addr add 10.244.240.30/24 dev lifecycle-port
  multichassis_exec "${node}" ip netns exec lifecycle-endpoint ip link set lifecycle-port up
  multichassis_exec "${node}" ip link set lifecycle-host up
  multichassis_exec "${node}" ovs-vsctl add-port br-int lifecycle-host \
    -- set Interface lifecycle-host external_ids:iface-id=lifecycle-a
}

multichassis_lifecycle_detach() {
  local node="$1"
  multichassis_exec "${node}" ovs-vsctl del-port br-int lifecycle-host
  multichassis_exec "${node}" ip netns del lifecycle-endpoint
}

multichassis_lifecycle_policy() {
  local node="$1" action="$2" generation="${3:-0}" reason="${4:-}" previous="${5:-0}"
  python3 - "${EVIDENCE_DIR}" "${node}" "${action}" "${generation}" "${reason}" "${previous}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);node,action,generation,reason,previous=sys.argv[2:]
path=p/f"lifecycle-policy-{node}.json";doc=json.loads(path.read_text());doc["revision"]+=1
doc["version"]=2
doc["tombstones"]=[x for x in doc.get("tombstones",[]) if x["record"]["endpointID"]!="lifecycle-a"]
if action!="compact":
    doc["endpoints"]=[x for x in doc["endpoints"] if x["endpointID"]!="lifecycle-a"]
if action in ("add","replace"):
    doc["endpoints"].append(json.loads((p/f"lifecycle-record-{generation}.json").read_text()))
if action in ("retire","replace"):
    old=previous if action=="replace" else generation
    doc["tombstones"].append({"reason":reason,"record":json.loads((p/f"lifecycle-record-{old}.json").read_text())})
assert action in ("add","replace","retire","compact")
path.write_text(json.dumps(doc)+"\n")
(p/f"lifecycle-policy-{node}-revision-{doc['revision']}.json").write_text(json.dumps(doc)+"\n")
PY
  multichassis_lifecycle_publish "${node}" "${EVIDENCE_DIR}/lifecycle-policy-${node}.json"
}

multichassis_lifecycle_publish() {
  if [[ "$1" == a ]]; then
    standalone_smoke_copy_to_agent "$2" /run/evpn-node-agent/standalone-smoke/authorization.json
  else
    multichassis_b_scope standalone_smoke_copy_to_agent "$2" /run/evpn-node-agent/standalone-smoke/authorization.json
  fi
}

multichassis_lifecycle_snapshot() {
  local phase="$1" mode="$2"
  multichassis_capture_snapshot "lifecycle-${phase}"
  standalone_smoke_capture_command "lifecycle-${phase}-nb" \
    rt ovn-nbctl --format=json --columns=_uuid,name,addresses,external_ids list Logical_Switch_Port
  standalone_smoke_capture_command "lifecycle-${phase}-switches" \
    rt ovn-nbctl --format=json --columns=_uuid,name,ports list Logical_Switch
  standalone_smoke_capture_command "lifecycle-${phase}-sb" \
    rt ovn-sbctl --format=json --columns=_uuid,logical_port,chassis,up list Port_Binding
  standalone_smoke_capture_command "lifecycle-${phase}-chassis" \
    rt ovn-sbctl --format=json --columns=_uuid,name list Chassis
  standalone_smoke_capture_command "lifecycle-${phase}-frr" \
    docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route json'
  python3 - "${EVIDENCE_DIR}" "${phase}" "${mode}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);phase,mode=sys.argv[2:];mac="0a:58:f0:00:00:1e";ip="10.244.240.30"
def read(node,name):return json.loads((p/f"multi-lifecycle-{phase}-{node}-{name}.stdout").read_text())
for node,base,hop in (("a","0a:58:f0:00:00:0a","198.18.0.2"),("b","0a:58:f0:00:00:14","198.18.0.5")):
    status=read(node,"export-status");remote=read(node,"import-status");journal=read(node,"export-journal")["Record"]
    initial=json.loads((p/f"multi-initial-{node}-export-status.stdout").read_text())
    if mode=="uncertain" and node=="a":
        assert "retaining applied routes" in status["Error"] and "without authoritative deletion evidence" in status["Error"],status
    else:
        assert not status["Error"] and status["Result"]["RecoveryState"]=="RUNNING",status
    assert not remote.get("Error") and remote["Snapshot"]["Authoritative"],remote
    assert journal["Pending"] is None and read(node,"import-journal")["Record"]["Pending"] is None
    baseline=next(x for x in journal["State"]["Receipts"] if x["Owned"]["Endpoint"]["MACAddress"]==base)
    assert baseline["Owned"]["PathUUID"]==initial["Result"]["Applied"][0]["PathUUID"],"unrelated owned UUID changed"
    for key in ("PID","StartTicks","PIDNamespace"):
        assert status["SpeakerProcess"][key]==initial["SpeakerProcess"][key],"speaker restarted"
    owned=[x for x in journal["State"]["Receipts"] if x["Owned"]["Endpoint"]["MACAddress"]==mac]
    local=mode in (node,"uncertain") and (node=="a" or mode=="b")
    imported=(mode in ("a","uncertain") and node=="b") or (mode=="b" and node=="a")
    assert len(owned)==int(local),(node,mode,owned)
    receipts=[x for x in remote["Receipts"] if x["Endpoint"]["MAC"]==mac]
    assert len(receipts)==int(imported),(node,mode,remote)
    desired=[x for x in remote["Snapshot"]["Endpoints"] or [] if x["MAC"]==mac]
    assert len(desired)==int(imported),"remote authoritative desired state differs"
    baseline_remote=json.loads((p/f"multi-initial-{node}-import-status.stdout").read_text())
    for original in baseline_remote["Receipts"]:
        assert original in remote["Receipts"],"unrelated importer receipt/ownership changed"
    for receipt in remote["Receipts"]:
        assert receipt["Controller"]==baseline_remote["Receipts"][0]["Controller"],"importer ownership transfer"
        endpoint=receipt["Endpoint"]
        assert any(x.get("mac")==endpoint["MAC"] and x.get("dst")==endpoint["RemoteVTEP"] for x in read(node,"fdb"))
        assert any(x.get("dst")==endpoint["IP"] and x.get("lladdr")==endpoint["MAC"] for x in read(node,"neighbor"))
    imports=read(node,"import-journal")["Record"]["State"]
    assert not imports["Quarantine"]
    assert sorted(imports["Receipts"].values(),key=lambda x:x["Endpoint"]["MAC"])==sorted(remote["Receipts"],key=lambda x:x["Endpoint"]["MAC"])
    rib=read(node,"rib");paths=[x for key,rows in rib.items() if mac in key for x in rows]
    assert len(paths)==int(local or imported),(node,mode,paths)
    if local:
        assert not paths[0].get("peer-address")
        assert owned[0]["Owned"]["Endpoint"]["LocalVTEP"]==hop
    if imported:
        vtep="198.18.0.5" if mode=="b" else "198.18.0.2";r=receipts[0]
        assert r["Endpoint"]["IP"]==ip and r["Endpoint"]["RemoteVTEP"]==vtep
        assert paths[0]["peer-address"]==vtep
        assert all(x["Route"]["Classification"]=="remote" and x["Route"]["NextHop"]==vtep for x in r["Endpoint"]["Routes"])
        for kind in ("FDB","Neighbor"):assert r[kind]["ACK"] and r[kind]["Status"]=="confirmed"
    fdb=[x for x in read(node,"fdb") if x.get("mac")==mac]
    neigh=[x for x in read(node,"neighbor") if x.get("dst")==ip]
    assert len(fdb)==len(neigh)==int(imported),(node,mode,fdb,neigh)
    if imported:
        assert fdb[0]["dst"]==vtep and fdb[0]["state"]=="permanent" and {"self","extern_learn"}<=set(fdb[0]["flags"])
        assert neigh[0]["lladdr"]==mac and "NOARP" in neigh[0]["state"]
    fence=read(node,"fence")["Record"];policy=json.loads((p/f"lifecycle-policy-{node}.json").read_text())
    assert fence["Pending"] is None and fence["State"]["revision"]==policy["revision"] and fence["State"]["digest"]
    if node=="a":
        assert any(x["Endpoint"]["MAC"]=="0a:58:f0:01:00:0c" for x in remote["Receipts"])
        assert any("0a:58:f0:01:00:0c" in key for key in rib)
def table(name):
    data=json.loads((p/f"lifecycle-{phase}-{name}.stdout").read_text())
    return [dict(zip(data["headings"],row)) for row in data["data"]]
ports=[r for r in table("nb") if r["name"]=="lifecycle-a"]
bindings=[r for r in table("sb") if r["logical_port"]=="lifecycle-a"]
if mode in ("absent","uncertain"):
    assert not ports and not bindings,"deletion lacks complete NB/SB absence"
else:
    assert len(ports)==len(bindings)==1
    generation=int(dict(ports[0]["external_ids"][1])["evpn.framework/attachment-generation"])
    record=json.loads((p/f"lifecycle-record-{generation}.json").read_text())
    assert ports[0]["_uuid"][1]==record["logicalSwitchPortUUID"]
    chassis={r["_uuid"][1]:r["name"] for r in table("chassis")}
    binding=bindings[0]
    expected_node="b" if mode in ("b","migrating") else "a"
    expected=json.loads((p/f"multi-initial-{expected_node}-export-status.stdout").read_text())["Result"]["Applied"][0]["Endpoint"]["Chassis"]
    if mode=="down":
        assert binding["up"] is False,"source is not authoritatively down"
    else:
        assert binding["up"] is True and chassis[binding["chassis"][1]]==expected
    switch=next(r for r in table("switches") if r["name"]==("ls24c" if mode in ("b","migrating") else "ls24b"))
    raw=switch["ports"];members=raw[1] if raw[0]=="set" else [raw]
    assert ports[0]["_uuid"] in members
print(f"lifecycle {phase}/{mode}: BGP, exact journals, independent importer/kernel, policy fence, NB/SB and foreign survivors=PASS")
PY
}

multichassis_lifecycle_wait() {
  local phase="$1" mode="$2" deadline=$((SECONDS+90))
  until multichassis_lifecycle_snapshot "${phase}" "${mode}" \
      >"${EVIDENCE_DIR}/lifecycle-${phase}-poll.log" 2>&1; do
    (( SECONDS < deadline )) || { cat "${EVIDENCE_DIR}/lifecycle-${phase}-poll.log"; return 1; }
    sleep 2
  done
  cat "${EVIDENCE_DIR}/lifecycle-${phase}-poll.log"
}

multichassis_lifecycle_packets() (
  local phase="$1" node="$2"
  if [[ "${node}" == a ]]; then
    MULTICHASSIS_PACKET_A_NAMESPACE=lifecycle-endpoint
    MULTICHASSIS_PACKET_A_MAC=0a:58:f0:00:00:1e
    MULTICHASSIS_PACKET_A_IP=10.244.240.30
  else
    MULTICHASSIS_PACKET_B_NAMESPACE=lifecycle-endpoint
    MULTICHASSIS_PACKET_B_MAC=0a:58:f0:00:00:1e
    MULTICHASSIS_PACKET_B_IP=10.244.240.30
  fi
  multichassis_packets "lifecycle-${phase}"
)

multichassis_lifecycle_restart() {
  local phase="$1" mode="$2"
  multichassis_lifecycle_wait "${phase}-before-restart" "${mode}"
  printf '%s %s stop-agents\n' "$(date -u +%FT%TZ)" "${phase}" >>"${EVIDENCE_DIR}/lifecycle-timeline.txt"
  standalone_smoke_stop_agent
  multichassis_b_scope standalone_smoke_stop_agent
  standalone_smoke_start_agent
  multichassis_start_b_agent
  printf '%s %s start-agents\n' "$(date -u +%FT%TZ)" "${phase}" >>"${EVIDENCE_DIR}/lifecycle-timeline.txt"
  multichassis_lifecycle_wait "${phase}-after-restart" "${mode}"
  python3 - "${EVIDENCE_DIR}" "${phase}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);phase=sys.argv[2]
for node in ("a","b"):
    def read(stage,name):return json.loads((p/f"multi-lifecycle-{phase}-{stage}-restart-{node}-{name}.stdout").read_text())
    for name in ("fence","export-journal","import-journal"):
        assert read("before",name)["Record"]["State"]==read("after",name)["Record"]["State"],(node,name,"durable state changed")
    assert read("before","import-status")["Receipts"]==read("after","import-status")["Receipts"]
    def pid(stage):
        line=(p/f"multi-lifecycle-{phase}-{stage}-restart-{node}-processes.stdout").read_text().splitlines()[1].split()
        return line[0],line[21]
    assert pid("before")!=pid("after"),"agent was not restarted"
print(f"{phase}: both Node-Agents restarted, speakers unchanged, exact fences/export/import state preserved=PASS")
PY
}

multichassis_lifecycle() {
  ACTIVE_CASE=lifecycle_baseline
  cp "${EVIDENCE_DIR}/standalone-policy-1.json" "${EVIDENCE_DIR}/lifecycle-policy-a.json"
  cp "${EVIDENCE_DIR}/chassis-b/standalone-policy-1.json" "${EVIDENCE_DIR}/lifecycle-policy-b.json"
  multichassis_lifecycle_create a 1
  multichassis_lifecycle_wait unauthorized unauthorized
  multichassis_lifecycle_policy a add 1
  multichassis_lifecycle_wait baseline a
  multichassis_lifecycle_packets baseline a
  case_status lifecycle_baseline PASS "additional non-CNI endpoint A exactly authorized by LSP/SB UUIDs; reciprocal imports and real VXLAN traffic; baseline endpoints remain survivors"

  ACTIVE_CASE=lifecycle_uncertain
  multichassis_lifecycle_detach a
  rt ovn-nbctl --wait=sb lsp-del lifecycle-a
  multichassis_lifecycle_wait uncertain uncertain
  sleep 4
  multichassis_lifecycle_wait uncertain-stable uncertain
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
def journal(phase,node):return json.loads((p/f"multi-lifecycle-{phase}-{node}-export-journal.stdout").read_text())["Record"]
for node in ("a","b"):
    assert journal("baseline",node)==journal("uncertain-stable",node),"uncertainty mutated export ownership"
    def status(phase):return json.loads((p/f"multi-lifecycle-{phase}-{node}-export-status.stdout").read_text())
    assert status("uncertain-stable")["Time"]>status("uncertain")["Time"],"observation loop did not advance"
PY
  case_status lifecycle_uncertain PASS "complete NB/SB absence without trusted deletion remains uncertain across advancing observations; exact owned route retained, no new export"

  ACTIVE_CASE=lifecycle_delete
  multichassis_lifecycle_policy a retire 1 deleted
  multichassis_lifecycle_wait deleted absent
  multichassis_lifecycle_restart deleted absent
  case_status lifecycle_delete PASS "trusted deleted tombstone plus complete NB/SB absence withdrew exact UUID; B desired/receipt/FDB/neighbor absent; both restarts preserve retirement and foreign routes"

  ACTIVE_CASE=lifecycle_replace
  multichassis_lifecycle_policy a compact
  multichassis_lifecycle_create a 2
  multichassis_lifecycle_wait recreated-unauthorized unauthorized
  multichassis_lifecycle_policy a add 2
  multichassis_lifecycle_wait recreated a
  multichassis_lifecycle_packets recreated a
  multichassis_lifecycle_detach a
  rt ovn-nbctl --wait=sb lsp-del lifecycle-a
  multichassis_lifecycle_create a 3
  multichassis_lifecycle_policy a replace 3 replaced 2
  multichassis_lifecycle_wait replaced a
  multichassis_lifecycle_restart replaced a
  multichassis_lifecycle_packets replaced a
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);uuids=[];ports=[]
for phase,g in (("baseline",1),("recreated",2),("replaced",3)):
    e=json.loads((p/f"multi-lifecycle-{phase}-a-export-status.stdout").read_text())
    uuids.append(next(x["PathUUID"] for x in e["Result"]["Applied"] if x["Endpoint"]["MACAddress"]=="0a:58:f0:00:00:1e"))
    ports.append(json.loads((p/f"lifecycle-record-{g}.json").read_text())["logicalSwitchPortUUID"])
assert len(set(uuids))==len(set(ports))==3
f=json.loads((p/"multi-lifecycle-replaced-a-fence.stdout").read_text())["Record"]["State"]
entry=next(x for x in f["endpoints"] if x["endpointID"]=="lifecycle-a")
assert entry["active"] and entry["highestGeneration"]==3 and entry["retiredGeneration"]==2 and entry["retirementReason"]=="replaced"
print("three distinct LSP/path UUIDs; exact replaced tombstone retired generation2; active generation3=PASS")
PY
  case_status lifecycle_replace PASS "deleted generation1 recreated as generation2; active replacement uses new LSP UUID, exact replaced tombstone and generation3; owned remote state and packets recovered"

  ACTIVE_CASE=lifecycle_replay
  for kind in revision digest generation; do
    python3 - "${EVIDENCE_DIR}" "${kind}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);kind=sys.argv[2];doc=json.loads((p/"lifecycle-policy-a.json").read_text())
if kind=="revision":doc["revision"]-=1
if kind=="digest":doc["endpoints"][0]["attachmentID"]="conflicting-digest"
if kind=="generation":
    doc["revision"]+=1;doc["tombstones"]=[]
    doc["endpoints"]=[x for x in doc["endpoints"] if x["endpointID"]!="lifecycle-a"]
    doc["endpoints"].append(json.loads((p/"lifecycle-record-1.json").read_text()))
(p/f"lifecycle-replay-{kind}.json").write_text(json.dumps(doc)+"\n")
PY
    multichassis_lifecycle_publish a "${EVIDENCE_DIR}/lifecycle-replay-${kind}.json"
    local deadline=$((SECONDS+30))
    while :; do
      multichassis_exec a cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/lifecycle-replay-${kind}-status.json"
      if python3 - "${EVIDENCE_DIR}/lifecycle-replay-${kind}-status.json" "${kind}" <<'PY'
import json,sys
error=json.load(open(sys.argv[1]))["Error"]
expected={"revision":"below accepted high-water","digest":"conflicts with accepted digest","generation":"below retired generation high-water"}[sys.argv[2]]
assert expected in error,error
PY
      then break; fi
      (( SECONDS < deadline )) || return 1
      sleep 1
    done
    multichassis_capture_snapshot "lifecycle-replay-${kind}"
    multichassis_lifecycle_publish a "${EVIDENCE_DIR}/lifecycle-policy-a.json"
    multichassis_lifecycle_wait "replay-${kind}-recovered" a
  done
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
for kind in ("revision","digest","generation"):
    for node in ("a","b"):
        for name in ("export-journal","fence","import-journal"):
            def read(phase):return json.loads((p/f"multi-lifecycle-{phase}-{node}-{name}.stdout").read_text())["Record"]
            before,after=read("replaced-after-restart"),read(f"replay-{kind}")
            for field in ("Domain","State","Pending"):
                assert before[field]==after[field],(kind,node,name,field)
print("rollback, same-revision digest conflict and retired generation replay rejected without durable/route ownership mutation=PASS")
PY
  case_status lifecycle_replay PASS "revision rollback, digest equivocation and retired-generation replay rejected; current journals/routes unchanged; healthy policy resumes"

  ACTIVE_CASE=lifecycle_migrate
  printf '%s source-disable-and-drain\n' "$(date -u +%FT%TZ)" >>"${EVIDENCE_DIR}/lifecycle-timeline.txt"
  rt ovn-nbctl lsp-set-enabled lifecycle-a disabled
  multichassis_lifecycle_detach a
  multichassis_lifecycle_wait migration-source-down down
  local gap_rc=0
  standalone_smoke_capture_command lifecycle-migration-forwarding-gap \
    k -n cni-test exec pod-a -- ping -I net1 -c 1 -W 1 10.244.240.30 || gap_rc=$?
  [[ "${gap_rc}" != 0 ]]
  python3 - "${EVIDENCE_DIR}/lifecycle-migration-forwarding-gap.stdout" <<'PY'
import pathlib,re,sys
text=pathlib.Path(sys.argv[1]).read_text()
assert re.search(r"1 packets transmitted, 0 (?:packets )?received, 100% packet loss",text),text
PY
  printf '%s source-drained-migration-policy\n' "$(date -u +%FT%TZ)" >>"${EVIDENCE_DIR}/lifecycle-timeline.txt"
  multichassis_lifecycle_policy a retire 3 migrated
  local port_uuid
  port_uuid="$(cat "${EVIDENCE_DIR}/lifecycle-lsp-3.txt")"
  rt ovn-nbctl -- remove Logical_Switch ls24b ports "${port_uuid}" \
    -- add Logical_Switch ls24c ports "${port_uuid}" \
    -- set Logical_Switch_Port lifecycle-a options:requested-chassis="${MULTICHASSIS_CHASSIS}" \
    -- lsp-set-enabled lifecycle-a enabled
  multichassis_lifecycle_attach b
  rt ovn-nbctl --wait=hv sync
  # Wait for the source's exact withdrawal before authorizing the destination.
  local deadline=$((SECONDS+90))
  while :; do
    multichassis_exec a cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/lifecycle-migration-source-withdrawn.json"
    if python3 - "${EVIDENCE_DIR}/lifecycle-migration-source-withdrawn.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]));assert not s["Error"],s
assert all(x["Endpoint"]["MACAddress"]!="0a:58:f0:00:00:1e" for x in s["Result"]["Applied"])
assert any(x["Reason"]=="trusted migration tombstone matches authoritative remote SB binding" for x in s["Observations"])
PY
    then break; fi
    (( SECONDS < deadline )) || return 1
    sleep 1
  done
  multichassis_lifecycle_wait migration-withdrawn migrating
  printf '%s source-withdrawn-destination-authorize\n' "$(date -u +%FT%TZ)" >>"${EVIDENCE_DIR}/lifecycle-timeline.txt"
  multichassis_lifecycle_policy b add 3
  multichassis_lifecycle_wait migrated b
  multichassis_lifecycle_packets migrated b
  multichassis_lifecycle_restart migrated b
  multichassis_lifecycle_packets migrated-recovered b
  case_status lifecycle_migrate PASS "authoritatively down A drained exact path/remote receipts before same LSP UUID moved; migrated tombstone matches ready B binding; destination authorization and A remote import/VXLAN follow"
  case_status lifecycle_recovery PASS "deletion/replacement/migration Node-Agent restarts retained exact policy digests, tombstones/generation high-water and independent importer/exporter journals; speakers alive"

  ACTIVE_CASE=lifecycle_cleanup
  multichassis_lifecycle_policy a compact
  multichassis_lifecycle_wait compacted b
  multichassis_lifecycle_detach b
  rt ovn-nbctl --wait=sb lsp-del lifecycle-a
  multichassis_lifecycle_policy b retire 3 deleted
  multichassis_lifecycle_wait final-deleted absent
  multichassis_lifecycle_restart final-deleted absent
  multichassis_packets lifecycle-baseline-restored
  case_status lifecycle_cleanup PASS "destination trusted deletion removes migrated route and source remote entries; compact source migration retirement remains durable; original baseline forwarding preserved"
}
