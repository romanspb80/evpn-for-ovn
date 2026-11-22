#!/usr/bin/env bash

framework_ovsdb_gap_capture() {
  local phase="$1" dir="${EVIDENCE_DIR}/ovsdb-gap/${1}"
  mkdir -m 700 -p "${dir}"
  date -u +%Y-%m-%dT%H:%M:%SZ >"${dir}/observed-at.txt"
  printf 'importer=%s\nprovider=%s\n' "${IMPORTER_POD}" "${PROVIDER_POD}" \
    >"${dir}/pods.txt"
  if [[ "${phase}" == m012-during-snapshot-outage || "${phase}" == m012-after-snapshot-outage-ping ]]; then
    printf 'speaker process intentionally paused; GoBGP API unavailable\n' >"${dir}/gobgp-neighbor.txt"
    if provider_exec gobgp global rib -a evpn >"${dir}/gobgp-rib.txt" 2>"${dir}/gobgp-api-error.txt"; then
      echo "GoBGP returned an EVPN snapshot while its speaker was paused" >&2
      return 1
    fi
    printf 'expected API request failure while speaker was paused\n' >"${dir}/gobgp-api-unavailable.txt"
  else
    provider_exec gobgp neighbor "${FRR_PEER}" >"${dir}/gobgp-neighbor.txt"
    provider_exec gobgp global rib -a evpn >"${dir}/gobgp-rib.txt"
  fi
  docker exec "${FRR_CONTAINER}" vtysh \
    -c 'show bgp l2vpn evpn route type macip' >"${dir}/frr-rib.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" \
    >"${dir}/frr-neighbor.txt"
  k -n "${NAMESPACE}" logs "${IMPORTER_POD}" --all-containers=true \
    >"${dir}/importer.log" 2>&1 || true
  ag cat /run/evpn-node-agent/export-status.json >"${dir}/export-status.json" 2>&1 || true
  rt ovs-vsctl --format=json --columns=_uuid,name,ports,external_ids,fail_mode,controller list Bridge \
    >"${dir}/ovs-bridges.json"
  rt ovs-vsctl --format=json --columns=_uuid,name,interfaces,external_ids list Port \
    >"${dir}/ovs-ports.json"
  rt ovs-vsctl --format=json --columns=_uuid,name,type,options,external_ids,statistics list Interface \
    >"${dir}/ovs-interfaces.json"
  rt ovs-vsctl --format=json --columns=_uuid,external_ids,bridges list Open_vSwitch \
    >"${dir}/ovs-root.json"
  rt ovs-ofctl -O OpenFlow13 dump-flows br-int >"${dir}/ovs-flows-br-int.txt"
  if [[ "${phase}" == m012-during-peer-outage ]]; then
    if rt ovs-vsctl br-exists "${PROVIDER_BRIDGE}"; then
      rt ovs-ofctl -O OpenFlow13 dump-flows "${PROVIDER_BRIDGE}" >"${dir}/ovs-flows-provider.txt"
    else
      printf 'owned provider bridge absent after authoritative peer withdrawal\n' >"${dir}/ovs-flows-provider.txt"
    fi
  else
    rt ovs-ofctl -O OpenFlow13 dump-flows "${PROVIDER_BRIDGE}" >"${dir}/ovs-flows-provider.txt"
  fi
  rt ovn-nbctl --format=json --columns=_uuid,name,type,options,addresses,external_ids,tag \
    list Logical_Switch_Port >"${dir}/ovn-nb-lsps.json"
  rt ovn-sbctl --format=json --columns=_uuid,logical_port,chassis,up,datapath,type \
    list Port_Binding >"${dir}/ovn-sb-bindings.json"
}

framework_ovsdb_gap_wait_route() {
  local label="$1" deadline=$((SECONDS+180)) gobgp_rib frr_rib neighbor
  while (( SECONDS < deadline )); do
    if gobgp_rib="$(provider_exec gobgp global rib -a evpn 2>/dev/null)" &&
       frr_rib="$(docker exec "${FRR_CONTAINER}" vtysh \
         -c 'show bgp l2vpn evpn route type macip' 2>/dev/null)" &&
       neighbor="$(provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null)" &&
       grep -qi Established <<<"${neighbor}" &&
       grep -Fqi "${REMOTE_MAC}" <<<"${gobgp_rib}" &&
       grep -Fqi "${REMOTE_IP}" <<<"${gobgp_rib}" &&
       grep -Fqi "${REMOTE_MAC}" <<<"${frr_rib}" &&
       grep -Fqi "${REMOTE_IP}" <<<"${frr_rib}"; then
      return 0
    fi
    sleep 2
  done
  printf '%s\n' "${gobgp_rib:-unavailable}" >"${EVIDENCE_DIR}/ovsdb-gap/${label}-gobgp-rib-timeout.txt"
  printf '%s\n' "${frr_rib:-unavailable}" >"${EVIDENCE_DIR}/ovsdb-gap/${label}-frr-rib-timeout.txt"
  printf '%s\n' "${neighbor:-unavailable}" >"${EVIDENCE_DIR}/ovsdb-gap/${label}-gobgp-neighbor-timeout.txt"
  echo "OVSDB gap route ${REMOTE_MAC}/${REMOTE_IP} did not return after ${label}" >&2
  return 1
}

framework_ovsdb_gap_wait_owned_state() {
  local label="$1" deadline=$((SECONDS+120)) snapshot
  snapshot="${EVIDENCE_DIR}/ovsdb-gap/${label}-wait.json"
  while (( SECONDS < deadline )); do
    if rt ovs-vsctl --format=json --columns=name,type,options,external_ids list Interface \
      >"${snapshot}" 2>/dev/null &&
      python3 - "${snapshot}" "${FRR_VTEP}" "${FRAMEWORK_VNI}" <<'PY' >/dev/null 2>&1
import json,sys
d=json.load(open(sys.argv[1]))
def m(v):
    return dict(v[1]) if isinstance(v,list) and len(v)==2 and v[0]=="map" else v or {}
rows=[dict(zip(d["headings"],r)) for r in d["data"]]
matches=[r for r in rows if r["type"]=="vxlan" and
         m(r["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e" and
         m(r["options"]).get("remote_ip")==sys.argv[2] and
         m(r["options"]).get("key")==sys.argv[3]]
assert len(matches)==1
PY
    then
      return 0
    fi
    sleep 2
  done
  echo "OVSDB gap owned tunnel did not converge after ${label}" >&2
  return 1
}

framework_ovsdb_gap_assert_route() {
  local phase="$1" dir="${EVIDENCE_DIR}/ovsdb-gap/${1}"
  python3 - "${dir}" "${REMOTE_MAC}" "${REMOTE_IP}" "${FRR_PEER}" \
    "${FRR_VTEP}" "${FRAMEWORK_VNI}" "64512:${FRAMEWORK_VNI}" <<'PY'
import pathlib,sys
directory=pathlib.Path(sys.argv[1])
mac,ip,rd_host,vtep,vni,rt=sys.argv[2:]
for name in ("gobgp-rib.txt","frr-rib.txt"):
    text=(directory/name).read_text().lower()
    required=(mac.lower(),ip,rd_host.lower(),vtep,vni,rt)
    missing=[value for value in required if value not in text]
    assert not missing,(name,missing,text)
print("route attributes retained: MAC/IP, RD peer, VTEP next-hop and VNI/RT text")
PY
}

framework_ovsdb_gap_assert_stable() {
  local before="$1" after="$2" mode="$3" foreign_phys="${4:-}"
  python3 - "${EVIDENCE_DIR}/ovsdb-gap/${before}" "${EVIDENCE_DIR}/ovsdb-gap/${after}" \
    "${mode}" "framework-ovsdb-e2e" "${PROVIDER_BRIDGE}" "${LOGICAL_SWITCH}" \
    "${PHYSICAL_NETWORK}" "${FRR_VTEP}" "${FRAMEWORK_VNI}" "${foreign_phys}" <<'PY'
import json,pathlib,sys
before,after=map(pathlib.Path,sys.argv[1:3])
mode,network,bridge,logical,phys,vtep,vni,foreign=sys.argv[3:]
def decode(value):
    if isinstance(value,list) and len(value)==2:
        if value[0]=="uuid":
            return value[1]
        if value[0]=="set":
            return [decode(item) for item in value[1]]
    return value
def load(directory,name):
    data=json.load((directory/name).open())
    return [dict(zip(data["headings"],map(decode,row))) for row in data["data"]]
def mapping(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
def external(row):
    return mapping(row.get("external_ids"))
def normalized(rows,directory,keep_uuid=False,strip_stats=False):
    ports=load(directory,"ovs-ports.json")
    interfaces=load(directory,"ovs-interfaces.json")
    port_names={r.get("_uuid"):r.get("name") for r in ports}
    interface_names={r.get("_uuid"):r.get("name") for r in interfaces}
    result=[]
    for row in rows:
        item=dict(row)
        if "ports" in item:
            values=item["ports"] if isinstance(item["ports"],list) else [item["ports"]]
            item["ports"]=sorted(port_names.get(value,"<missing-port>") for value in values)
        if "interfaces" in item:
            values=item["interfaces"] if isinstance(item["interfaces"],list) else [item["interfaces"]]
            item["interfaces"]=sorted(interface_names.get(value,"<missing-interface>") for value in values)
        if not keep_uuid:
            item.pop("_uuid",None)
        if strip_stats:
            item.pop("statistics",None)
        result.append(item)
    return sorted(result,key=lambda x:(x.get("name",""),x.get("logical_port",""),x.get("_uuid","")))
def owned(rows):
    return [r for r in rows if external(r).get("evpn4ovn-network-id")==network]
def foreign_rows(directory,table):
    rows=load(directory,table)
    localnet={r["name"] for r in load(directory,"ovn-nb-lsps.json")
              if r.get("type")=="localnet" and external(r).get("evpn4ovn-network-id")==network}
    patches={name for port in localnet for name in (f"patch-{port}-to-br-int",f"patch-br-int-to-{port}")}
    result=[]
    for row in rows:
        if external(row).get("evpn4ovn-network-id"):
            continue
        derived=row.get("name") in patches
        result.extend(normalized([row],directory,keep_uuid=mode=="strict" or not derived,strip_stats=True))
    return sorted(result,key=lambda r:r.get("name",""))
for table in ("ovs-interfaces.json","ovs-ports.json","ovs-bridges.json","ovn-nb-lsps.json"):
    left,right=load(before,table),load(after,table)
    a,b=owned(left),owned(right)
    assert a and b,(table,a,b)
    strict=(mode=="strict")
    assert normalized(a,before,keep_uuid=strict,strip_stats=True)==normalized(b,after,keep_uuid=strict,strip_stats=True),\
        (table,a,b)
for table in ("ovs-interfaces.json","ovs-ports.json","ovs-bridges.json","ovn-nb-lsps.json"):
    assert foreign_rows(before,table)==foreign_rows(after,table),(table,"foreign rows changed")
left=load(before,"ovs-root.json")
right=load(after,"ovs-root.json")
assert len(left)==len(right)==1
left_ids,right_ids=external(left[0]),external(right[0])
left_map=left_ids.get("ovn-bridge-mappings","")
right_map=right_ids.get("ovn-bridge-mappings","")
if foreign:
    parts=[part for part in right_map.split(",") if part]
    assert f"{foreign}:{bridge}" in parts and f"{phys}:{bridge}" in parts,(right_map,foreign,phys,bridge)
    adjusted=dict(right_ids)
    adjusted["ovn-bridge-mappings"]=",".join(part for part in parts if part!=f"{foreign}:{bridge}")
    assert adjusted==left_ids,(left_ids,right_ids)
else:
    assert left_ids==right_ids,(left_ids,right_ids)
left_bindings=load(before,"ovn-sb-bindings.json")
right_bindings=load(after,"ovn-sb-bindings.json")
assert normalized(left_bindings,before,keep_uuid=mode=="strict")==normalized(right_bindings,after,keep_uuid=mode=="strict"),\
    ("SB bindings changed",left_bindings,right_bindings)
for directory in (before,after):
    interfaces=load(directory,"ovs-interfaces.json")
    tunnels=[r for r in interfaces if r.get("type")=="vxlan" and
             external(r).get("evpn4ovn-network-id")==network]
    assert len(tunnels)==1,tunnels
    opts=mapping(tunnels[0].get("options"))
    ids=external(tunnels[0])
    assert opts=={"remote_ip":vtep,"key":vni,"dst_port":"4789"},opts
    assert ids.get("evpn4ovn-managed-by")=="model1-nbdb" and \
           ids.get("evpn4ovn-remote-vtep")==vtep and ids.get("evpn4ovn-vni")==vni,ids
    stats=mapping(tunnels[0].get("statistics"))
    assert {"rx_packets","tx_packets","rx_bytes","tx_bytes"}<=set(stats),stats
    for key in ("rx_packets","tx_packets","rx_bytes","tx_bytes"):
        assert str(stats[key]).isdecimal(),(key,stats[key])
    nb=load(directory,"ovn-nb-lsps.json")
    localnet=[r for r in nb if r.get("type")=="localnet" and external(r).get("evpn4ovn-network-id")==network]
    assert len(localnet)==1,localnet
    assert mapping(localnet[0].get("options"))=={"network_name":phys},localnet
    assert external(localnet[0]).get("evpn4ovn-logical-switch")==logical
    assert external(localnet[0]).get("evpn4ovn-provider-bridge")==bridge
    bindings=load(directory,"ovn-sb-bindings.json")
    names={r.get("logical_port") for r in bindings}
    assert localnet[0]["name"] in names,(localnet[0]["name"],names)
if foreign:
    assert any(f"{foreign}:{bridge}" == part for part in right_map.split(","))
print(f"{mode} OVSDB/OVN ownership state retained; tunnel counters and NB/SB rows captured")
PY
}

framework_ovsdb_gap_assert_counter_delta() {
  local before="$1" after="$2"
  python3 - "${EVIDENCE_DIR}/ovsdb-gap/${before}/ovs-interfaces.json" \
    "${EVIDENCE_DIR}/ovsdb-gap/${after}/ovs-interfaces.json" <<'PY'
import json,sys
def tunnel(path):
    d=json.load(open(path))
    def m(v):
        return dict(v[1]) if isinstance(v,list) and len(v)==2 and v[0]=="map" else v or {}
    rows=[dict(zip(d["headings"],r)) for r in d["data"]]
    matches=[r for r in rows if r["type"]=="vxlan" and
             m(r["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
    assert len(matches)==1,matches
    stats=m(matches[0]["statistics"])
    return sum(int(stats[k]) for k in ("rx_packets","tx_packets"))
a,b=map(tunnel,sys.argv[1:])
assert b>a,(a,b)
print(f"owned OVS VXLAN packet counters increased: {a} -> {b}")
PY
}

framework_ovsdb_gap_assert_peer_withdrawal() {
  local before="$1" during="$2"
  python3 - "${EVIDENCE_DIR}/ovsdb-gap/${before}" "${EVIDENCE_DIR}/ovsdb-gap/${during}" \
    "${PHYSICAL_NETWORK}" "${FRR_VTEP}" "${REMOTE_MAC}" "${REMOTE_IP}" <<'PY'
import json,pathlib,sys
before,during=map(pathlib.Path,sys.argv[1:3])
phys,vtep,mac,ip=sys.argv[3:]
rib=(during/"gobgp-rib.txt").read_text().lower()
assert mac.lower() not in rib and ip not in rib,rib
frr=(during/"frr-rib.txt").read_text().lower()
assert mac.lower() in frr and ip in frr and vtep in frr,(mac,ip,vtep,frr)
def rows(directory,name):
    data=json.load((directory/name).open())
    return [dict(zip(data["headings"],row)) for row in data["data"]]
def m(v):
    return dict(v[1]) if isinstance(v,list) and len(v)==2 and v[0]=="map" else v or {}
for table in ("ovs-interfaces.json","ovs-ports.json","ovs-bridges.json"):
    old=[r for r in rows(before,table) if m(r.get("external_ids")).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
    new=[r for r in rows(during,table) if m(r.get("external_ids")).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
    assert old and not new,(table,old,new)
old_nb=[r for r in rows(before,"ovn-nb-lsps.json") if m(r.get("external_ids")).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
new_nb=[r for r in rows(during,"ovn-nb-lsps.json") if m(r.get("external_ids")).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
assert old_nb and not new_nb,(old_nb,new_nb)
localnet={r["name"] for r in old_nb if r.get("type")=="localnet"}
patches={name for port in localnet for name in (f"patch-{port}-to-br-int",f"patch-br-int-to-{port}")}
for table in ("ovs-interfaces.json","ovs-ports.json","ovn-nb-lsps.json"):
    old=[r for r in rows(before,table) if not m(r.get("external_ids")).get("evpn4ovn-network-id") and r.get("name") not in patches]
    new=[r for r in rows(during,table) if not m(r.get("external_ids")).get("evpn4ovn-network-id") and r.get("name") not in patches]
    def clean(items):
        return sorted(({k:v for k,v in r.items() if k not in ("statistics",)} for r in items),
                      key=lambda r:(r.get("name",""),r.get("_uuid","")))
    assert clean(old)==clean(new),(table,"foreign rows changed",old,new)
old_bridges=[r for r in rows(before,"ovs-bridges.json") if not m(r.get("external_ids")).get("evpn4ovn-network-id")]
new_bridges=[r for r in rows(during,"ovs-bridges.json") if not m(r.get("external_ids")).get("evpn4ovn-network-id")]
def bridge_identity(items):
    result=[]
    for row in items:
        item={k:v for k,v in row.items() if k!="ports"}
        item["external_ids"]={k:v for k,v in m(item.get("external_ids")).items()
                              if k not in {f"ct-zone-{name}" for name in localnet}}
        result.append(item)
    return sorted(result,key=lambda r:(r.get("name",""),r.get("_uuid","")))
assert bridge_identity(old_bridges)==bridge_identity(new_bridges),("foreign bridges changed",old_bridges,new_bridges)
old_root=rows(before,"ovs-root.json")
new_root=rows(during,"ovs-root.json")
assert len(old_root)==len(new_root)==1
old_map=m(old_root[0]["external_ids"]).get("ovn-bridge-mappings","")
new_map=m(new_root[0]["external_ids"]).get("ovn-bridge-mappings","")
assert f"{phys}:" in old_map and f"{phys}:" not in new_map,(old_map,new_map,vtep)
PY
}

framework_ovsdb_gap_run_m011() {
  FRAMEWORK_OVSDB_GAP_CURRENT=M011
  FAILURE_CLASSIFICATION=OVSDB-M011
  FAILURE_GATE=M011
  echo 'M011=RUNNING' | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  framework_ovsdb_gap_capture m011-before
  framework_ovsdb_gap_assert_route m011-before
  local old_importer="${IMPORTER_POD}"
  (
    trap 'k -n "${NAMESPACE}" scale deployment/ovsdb-route-importer --replicas=1 >/dev/null 2>&1 || true' EXIT
    k -n "${NAMESPACE}" scale deployment/ovsdb-route-importer --replicas=0
    k -n "${NAMESPACE}" wait --for=delete "pod/${old_importer}" --timeout=120s
    framework_ovsdb_gap_capture m011-importer-down
    framework_ovsdb_gap_assert_route m011-importer-down
    framework_ovsdb_gap_assert_stable m011-before m011-importer-down strict
    k -n "${NAMESPACE}" scale deployment/ovsdb-route-importer --replicas=1
    trap - EXIT
  )
  k -n "${NAMESPACE}" rollout status deployment/ovsdb-route-importer --timeout=180s
  IMPORTER_POD="$(k -n "${NAMESPACE}" get pod -l app=ovsdb-route-importer \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ "${IMPORTER_POD}" != "${old_importer}" ]]
  framework_ovsdb_gap_wait_route importer-restart
  framework_ovsdb_gap_wait_owned_state importer-restart
  framework_ovsdb_gap_capture m011-importer-restarted
  framework_ovsdb_gap_assert_route m011-importer-restarted
  framework_ovsdb_gap_assert_stable m011-before m011-importer-restarted semantic

  k -n k8gobgp-system rollout restart ds/k8gobgp
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
  PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp \
    -o jsonpath='{.items[0].metadata.name}')"
  framework_ovsdb_gap_wait_route speaker-restart
  framework_ovsdb_gap_wait_owned_state speaker-restart
  k -n "${NAMESPACE}" rollout status deployment/ovsdb-route-importer --timeout=120s
  IMPORTER_POD="$(k -n "${NAMESPACE}" get pod -l app=ovsdb-route-importer \
    -o jsonpath='{.items[0].metadata.name}')"
  framework_ovsdb_gap_capture m011-after
  framework_ovsdb_gap_assert_route m011-after
  framework_ovsdb_gap_assert_stable m011-before m011-after semantic
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
    ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/ovsdb-gap/m011-forward-ping.txt"
  docker exec "${FRR_CONTAINER}" nsenter -t "${REMOTE_WORKLOAD_PID}" -n \
    ping -c 3 -W 2 10.244.240.10 \
    | tee "${EVIDENCE_DIR}/ovsdb-gap/m011-reverse-ping.txt"
  framework_ovsdb_gap_capture m011-after-ping
  framework_ovsdb_gap_assert_route m011-after-ping
  framework_ovsdb_gap_assert_stable m011-before m011-after-ping semantic
  framework_ovsdb_gap_assert_counter_delta m011-after m011-after-ping
  printf 'capture_window=local-host.pcap,worker-underlay.pcap,frr-underlay.pcap,frr-vxlan.pcap,remote-workload.pcap\n' \
    >"${EVIDENCE_DIR}/ovsdb-gap/m011-capture-window.txt"
  echo 'M011=PASS importer and speaker restart reconverged with owned route and foreign state intact' \
    | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  FRAMEWORK_OVSDB_GAP_CURRENT=''
}

framework_ovsdb_gap_run_m012() {
  FRAMEWORK_OVSDB_GAP_CURRENT=M012
  FAILURE_CLASSIFICATION=OVSDB-M012
  FAILURE_GATE=M012
  echo 'M012=RUNNING' | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  framework_ovsdb_gap_capture m012-before
  framework_ovsdb_gap_assert_route m012-before
  (
    local speaker_pid
    speaker_pid="$(k -n k8gobgp-system exec "${PROVIDER_POD}" -- sh -ec 'pidof gobgpd')"
    [[ "${speaker_pid}" =~ ^[0-9]+$ ]]
    printf '%s\n' "${speaker_pid}" >"${EVIDENCE_DIR}/ovsdb-gap/m012-speaker-pid.txt"
    trap 'k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -CONT "${speaker_pid}" >/dev/null 2>&1 || true' EXIT
    k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -STOP "${speaker_pid}"
    sleep 3
    k -n k8gobgp-system exec "${PROVIDER_POD}" -- sh -ec \
      "grep '^State:' /proc/${speaker_pid}/status" \
      >"${EVIDENCE_DIR}/ovsdb-gap/m012-speaker-stopped-state.txt"
    grep -Eq 'State:[[:space:]]+T' "${EVIDENCE_DIR}/ovsdb-gap/m012-speaker-stopped-state.txt"
    framework_ovsdb_gap_capture m012-during-snapshot-outage
    framework_ovsdb_gap_assert_stable m012-before m012-during-snapshot-outage strict
    k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
      ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
      | tee "${EVIDENCE_DIR}/ovsdb-gap/m012-snapshot-outage-forward-ping.txt"
    framework_ovsdb_gap_capture m012-after-snapshot-outage-ping
    framework_ovsdb_gap_assert_stable m012-before m012-after-snapshot-outage-ping strict
    k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -CONT "${speaker_pid}"
    trap - EXIT
  )
  framework_ovsdb_gap_wait_route speaker-api-restoration
  framework_ovsdb_gap_wait_owned_state speaker-api-restoration
  framework_ovsdb_gap_capture m012-after-snapshot-restoration
  framework_ovsdb_gap_assert_route m012-after-snapshot-restoration
  (
    trap 'docker exec "${FRR_CONTAINER}" vtysh -c "configure terminal" -c "router bgp 64512" -c "no neighbor ${LOCAL_VTEP} shutdown" -c end >/dev/null 2>&1 || true' EXIT
    docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
      -c "neighbor ${LOCAL_VTEP} shutdown" -c end \
      >"${EVIDENCE_DIR}/ovsdb-gap/m012-peer-shutdown.txt"
    local deadline=$((SECONDS+60)) neighbor=''
    while (( SECONDS < deadline )); do
      neighbor="$(provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null || true)"
      if ! grep -qi Established <<<"${neighbor}"; then break; fi
      sleep 1
    done
    printf '%s\n' "${neighbor}" >"${EVIDENCE_DIR}/ovsdb-gap/m012-neighbor-down.txt"
    ! grep -qi Established <<<"${neighbor}"
    sleep 5
    framework_ovsdb_gap_capture m012-during-peer-outage
    framework_ovsdb_gap_assert_peer_withdrawal m012-before m012-during-peer-outage
    docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
      >"${EVIDENCE_DIR}/ovsdb-gap/m012-frr-rib-during.txt"
    grep -Fqi "${REMOTE_MAC}" "${EVIDENCE_DIR}/ovsdb-gap/m012-frr-rib-during.txt"
  )
  local deadline=$((SECONDS+120)) neighbor=''
  while (( SECONDS < deadline )); do
    neighbor="$(provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null || true)"
    if grep -qi Established <<<"${neighbor}"; then break; fi
    sleep 1
  done
  grep -qi Established <<<"${neighbor}"
  framework_ovsdb_gap_wait_route peer-restoration
  framework_ovsdb_gap_wait_owned_state peer-restoration
  framework_ovsdb_gap_capture m012-after
  framework_ovsdb_gap_assert_route m012-after
  framework_ovsdb_gap_assert_stable m012-before m012-after semantic
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
    ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/ovsdb-gap/m012-restored-ping.txt"
  framework_ovsdb_gap_capture m012-after-ping
  framework_ovsdb_gap_assert_stable m012-before m012-after-ping semantic
  framework_ovsdb_gap_assert_counter_delta m012-after m012-after-ping
  printf 'capture_window=local-host.pcap,worker-underlay.pcap,frr-underlay.pcap,frr-vxlan.pcap,remote-workload.pcap\n' \
    >"${EVIDENCE_DIR}/ovsdb-gap/m012-capture-window.txt"
  echo 'M012=PASS FRR BGP peer outage/restoration retained and recovered the OVSDB route' \
    | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  FRAMEWORK_OVSDB_GAP_CURRENT=''
}

framework_ovsdb_gap_run_m013() {
  FRAMEWORK_OVSDB_GAP_CURRENT=M013
  FAILURE_CLASSIFICATION=OVSDB-M013
  FAILURE_GATE=M013
  echo 'M013=RUNNING' | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  framework_ovsdb_gap_capture m013-before
  framework_ovsdb_gap_assert_route m013-before
  (
    local foreign_phys="fg${RUN_SUFFIX}" original_mappings
    original_mappings="$(rt ovs-vsctl --bare get Open_vSwitch . external_ids:ovn-bridge-mappings | tr -d '"')"
    [[ -n "${original_mappings}" && ",${original_mappings}," != *",${foreign_phys}:"* ]]
    trap 'rt ovs-vsctl set Open_vSwitch . "external_ids:ovn-bridge-mappings=${original_mappings}" >/dev/null 2>&1 || true' EXIT
    rt ovs-vsctl set Open_vSwitch . \
      "external_ids:ovn-bridge-mappings=${original_mappings},${foreign_phys}:${PROVIDER_BRIDGE}"
    framework_ovsdb_gap_capture m013-during
    python3 - "${EVIDENCE_DIR}/ovsdb-gap/m013-during/ovs-root.json" \
      "${foreign_phys}" "${PROVIDER_BRIDGE}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
assert len(d["data"])==1
head=d["headings"]
row=dict(zip(head,d["data"][0]))
ids=row["external_ids"]
if isinstance(ids,list) and len(ids)==2 and ids[0]=="map": ids=dict(ids[1])
assert f"{sys.argv[2]}:{sys.argv[3]}" in ids.get("ovn-bridge-mappings",""),ids
PY
    sleep 5
    k -n "${NAMESPACE}" logs "${IMPORTER_POD}" --all-containers=true \
      >"${EVIDENCE_DIR}/ovsdb-gap/m013-live-write-errors.log" 2>&1
    grep -Fq 'already assigned to physical network' \
      "${EVIDENCE_DIR}/ovsdb-gap/m013-live-write-errors.log"
    framework_ovsdb_gap_capture m013-live-writes
    framework_ovsdb_gap_assert_route m013-live-writes
    framework_ovsdb_gap_assert_stable m013-before m013-live-writes strict "${foreign_phys}"
    k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
      ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
      | tee "${EVIDENCE_DIR}/ovsdb-gap/m013-existing-route-ping.txt"
    framework_ovsdb_gap_capture m013-after-ping
    framework_ovsdb_gap_assert_stable m013-before m013-after-ping strict "${foreign_phys}"
    framework_ovsdb_gap_assert_counter_delta m013-before m013-after-ping
    rt ovs-vsctl set Open_vSwitch . "external_ids:ovn-bridge-mappings=${original_mappings}"
    framework_ovsdb_gap_wait_owned_state mapping-restoration
    framework_ovsdb_gap_capture m013-after
    framework_ovsdb_gap_assert_route m013-after
    framework_ovsdb_gap_assert_stable m013-before m013-after semantic
    printf 'capture_window=local-host.pcap,worker-underlay.pcap,frr-underlay.pcap,frr-vxlan.pcap,remote-workload.pcap\n' \
      >"${EVIDENCE_DIR}/ovsdb-gap/m013-capture-window.txt"
    echo 'M013=PASS conflicting foreign provider mapping was refused and preserved alongside the live route' \
      | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
    trap - EXIT
  )
  FRAMEWORK_OVSDB_GAP_CURRENT=''
}

framework_ovsdb_gap_cases() {
  case "${FRAMEWORK_OVSDB_GAP_CLOSURE:-0}" in
    0) return 0 ;;
    1|supplemental) ;;
    *) echo "FRAMEWORK_OVSDB_GAP_CLOSURE must be 0, 1 or supplemental" >&2; return 2 ;;
  esac
  mkdir -m 700 -p "${EVIDENCE_DIR}/ovsdb-gap"
  FRAMEWORK_OVSDB_GAP_CURRENT=''
  printf 'scope=%s\nsource=%s\n' "${FRAMEWORK_OVSDB_GAP_CLOSURE}" "$(git -C "${ROOT}" rev-parse HEAD)" \
    >"${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  if [[ "${FRAMEWORK_OVSDB_GAP_CLOSURE}" == 1 ]]; then
    framework_ovsdb_gap_run_m011
    framework_ovsdb_gap_run_m012
    framework_ovsdb_gap_run_m013
  fi
  framework_ovsdb_gap_supplemental
}

framework_ovsdb_gap_wire_source() {
  cat <<'GO'
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Send exact UPDATE bytes: a routing daemon's local AddPath can synthesize
// MAC-mobility instead of sending the supplied attributes verbatim.
func startWirePeer(ctx context.Context, address string, target ...string) (net.Listener, <-chan net.Conn, error) {
	listener, err := net.Listen("tcp4", address)
	if err != nil { return nil, nil, err }
	ready := make(chan net.Conn, 1)
	go func() {
		var connection net.Conn
		var err error
		if len(target) == 1 {
			dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: listener.Addr().(*net.TCPAddr).IP}, Timeout: 5*time.Second}
			connection, err = dialer.DialContext(ctx, "tcp4", target[0])
		} else {
			connection, err = listener.Accept()
		}
		if err != nil { return }
		go func() { <-ctx.Done(); connection.Close() }()
		open, err := bgp.NewBGPOpenMessage(64513, 30, netip.MustParseAddr("192.0.2.13"),
			[]bgp.OptionParameterInterface{bgp.NewOptionParameterCapability(
				[]bgp.ParameterCapabilityInterface{bgp.NewCapMultiProtocol(bgp.RF_EVPN), bgp.NewCapFourOctetASNumber(64513)})})
		if err != nil { connection.Close(); return }
		var mu sync.Mutex
		write := func(message *bgp.BGPMessage) error {
			bytes, err := message.Serialize()
			if err != nil { return err }
			mu.Lock()
			defer mu.Unlock()
			for len(bytes) > 0 {
				n, err := connection.Write(bytes)
				if err != nil { return err }
				bytes = bytes[n:]
			}
			return nil
		}
		if err := write(open); err != nil { connection.Close(); return }
		established := false
		go func() {
			ticker := time.NewTicker(5*time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done(): return
				case <-ticker.C:
					if write(bgp.NewBGPKeepAliveMessage()) != nil { return }
				}
			}
		}()
		for {
			header := make([]byte, 19)
			if _, err := io.ReadFull(connection, header); err != nil { connection.Close(); return }
			length := int(binary.BigEndian.Uint16(header[16:18]))
			if length < 19 || length > 4096 { connection.Close(); return }
			body := make([]byte, length-19)
			if _, err := io.ReadFull(connection, body); err != nil { connection.Close(); return }
			message, err := bgp.ParseBGPMessage(append(header, body...))
			if err != nil { connection.Close(); return }
			switch message.Header.Type {
			case bgp.BGP_MSG_OPEN:
				if err := write(bgp.NewBGPKeepAliveMessage()); err != nil { connection.Close(); return }
			case bgp.BGP_MSG_KEEPALIVE:
				if !established {
					// All later writes use the same lock as keepalives.
					ready <- &wireConnection{Conn: connection, mu: &mu}
					established = true
				}
			case bgp.BGP_MSG_NOTIFICATION:
				connection.Close()
				return
			}
		}
	}()
	return listener, ready, nil
}

type wireConnection struct {
	net.Conn
	mu *sync.Mutex
}

func (connection *wireConnection) Write(data []byte) (int, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.Conn.Write(data)
}

func sendWireUpdate(connection net.Conn, attrs []bgp.PathAttributeInterface) error {
	data, err := bgp.NewBGPUpdateMessage(nil, attrs, nil).Serialize()
	if err != nil { return err }
	for len(data) > 0 {
		n, err := connection.Write(data)
		if err != nil { return err }
		data = data[n:]
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 7 {
		return fmt.Errorf("requires directory, local VTEP, allowed VTEP, valid MAC, valid IP and disallowed reachable VTEP")
	}
	dir, local, allowed, validMAC, validIP := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, 20*time.Minute)
	defer cancel()
	conn, err := grpc.NewClient("unix:///run/gobgp-export-test/gobgp.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil { return err }
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	family := &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN}
	if _, err := client.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf: &api.PeerConf{NeighborAddress: "127.0.0.2", PeerAsn: 64513},
		Transport: &api.Transport{LocalAddress: local, PassiveMode: true},
		Timers: &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		AfiSafis: []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: family, Enabled: true}}},
	}}); err != nil { return err }
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.DeletePeer(cleanup, &api.DeletePeerRequest{Address: "127.0.0.2"}); err != nil {
			fmt.Fprintln(os.Stderr, "supplemental peer cleanup:", err)
		}
	}()
	listener, ready, err := startWirePeer(ctx, "127.0.0.2:1179", net.JoinHostPort(local, "179"))
	if err != nil { return err }
	defer listener.Close()
	var wire net.Conn
	select {
	case wire = <-ready:
		defer wire.Close()
	case <-ctx.Done(): return ctx.Err()
	}
	source := &evpn.GoBGPRemoteRIBSource{Client: client, Gate: evpn.GoBGPIdentityReadiness{
		Client: client, ASN: 64512, RouterID: local,
	}}
	config := evpn.RemoteImportConfig{LocalVTEP: local, Networks: []evpn.RemoteImportNetwork{{
		NetworkID: "framework-ovsdb-e2e", VNI: 1000, ImportRouteTargets: []string{"64512:1000"}, AllowedVTEPs: []string{allowed},
	}}}
	reader, err := evpn.NewReadOnlyType2ImporterFromSnapshot(source, config)
	if err != nil { return err }
	write := func(name string, value any) error {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil { return err }
		return os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0600)
	}
	type observation struct {
		RIB evpn.RemoteType2Snapshot
		Snapshot evpn.RemoteImportSnapshot
		Error string
	}
	observe := func() observation {
		raw, rawErr := source.ReadRemoteType2RIB(ctx)
		snapshot, pollErr := reader.Poll(ctx)
		o := observation{RIB: raw, Snapshot: snapshot}
		if rawErr != nil { o.Error = rawErr.Error() } else if pollErr != nil { o.Error = pollErr.Error() }
		return o
	}
	wait := func(label string, predicate func(observation) bool) (observation, error) {
		deadline := time.Now().Add(60*time.Second)
		var o observation
		for time.Now().Before(deadline) && ctx.Err() == nil {
			o = observe()
			if predicate(o) {
				if err := write(label+".json", o); err != nil { return o, err }
				return o, nil
			}
			time.Sleep(500*time.Millisecond)
		}
		write(label+"-timeout.json", o)
		data, _ := json.Marshal(o)
		fmt.Fprintln(os.Stderr, string(data))
		return o, fmt.Errorf("%s did not reach expected received-RIB/validation state", label)
	}
	baselineIssues := -1
	healthy := func(o observation) bool {
		return o.Error == "" && o.Snapshot.Authoritative && len(o.Snapshot.Endpoints) == 1 &&
			o.Snapshot.Endpoints[0].MAC == validMAC && o.Snapshot.Endpoints[0].IP == validIP &&
			o.Snapshot.Endpoints[0].RemoteVTEP == allowed &&
			(baselineIssues < 0 || len(o.Snapshot.Issues) == baselineIssues)
	}
	baseline, err := wait("baseline", healthy)
	if err != nil { return err }
	baselineIssues = len(baseline.Snapshot.Issues)
	ack := func(label string) error {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(label+"\n"), 0600); err != nil { return err }
		for ctx.Err() == nil {
			data, _ := os.ReadFile(filepath.Join(dir, "ack"))
			if strings.TrimSpace(string(data)) == label { return nil }
			time.Sleep(200*time.Millisecond)
		}
		return ctx.Err()
	}
	if err := ack("baseline"); err != nil { return err }
	for _, scenario := range []struct{ label, mac, ip, nextHop, rt, reason string; vni uint32 }{
		{"unauthorized-rt", "02:00:00:00:00:99", "10.244.240.99", allowed, "64513:999", "no import RT intersection", 1000},
		{"unauthorized-vtep", "02:00:00:00:00:99", "10.244.240.99", os.Args[6], "64512:1000", "remote VTEP not allowlisted", 1000},
		{"mismatched-vni", "02:00:00:00:00:99", "10.244.240.99", allowed, "64512:1000", "no configured VNI mapping", 1001},
		{"duplicate-endpoint", validMAC, validIP, allowed, "64512:1000", "", 1000},
		{"conflicting-ip-binding", validMAC, "10.244.240.99", allowed, "64512:1000", "conflicting IPv4 bindings", 1000},
	} {
		rd, err := bgp.ParseRouteDistinguisher("64513:1100")
		if err != nil { return err }
		mac, err := net.ParseMAC(scenario.mac)
		if err != nil { return err }
		rt, err := bgp.ParseRouteTarget(scenario.rt)
		if err != nil { return err }
		nlri := bgp.NewEVPNNLRI(bgp.EVPN_ROUTE_TYPE_MAC_IP_ADVERTISEMENT, &bgp.EVPNMacIPAdvertisementRoute{
			RD: rd, MacAddressLength: 48, MacAddress: mac,
			IPAddressLength: 32, IPAddress: netip.MustParseAddr(scenario.ip), Labels: []uint32{scenario.vni},
		})
		reach, err := bgp.NewPathAttributeMpReachNLRI(bgp.RF_EVPN, []bgp.PathNLRI{{NLRI: nlri}}, netip.MustParseAddr(scenario.nextHop))
		if err != nil { return err }
		attrs := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0),
			bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{64513})}), reach,
			bgp.NewPathAttributeExtendedCommunities([]bgp.ExtendedCommunityInterface{rt, &bgp.EncapExtended{TunnelType: bgp.TUNNEL_TYPE_VXLAN}})}
		if err := write(scenario.label+"-sent.json", attrs); err != nil { return err }
		if err := sendWireUpdate(wire, attrs); err != nil { return err }
		_, err = wait(scenario.label, func(o observation) bool {
			seen := false
			if !o.RIB.Complete { return false }
			for _, path := range o.RIB.Paths {
				r := path.Route
				if path.DecodeError == nil && r.Classification == "remote" &&
					r.Source.NeighborIP == "127.0.0.2" && r.Source.SourceASN == 64513 &&
					r.RD == "64513:1100" && r.MAC == scenario.mac && r.IP == scenario.ip &&
					r.NextHop == scenario.nextHop && len(r.Labels) == 1 && r.Labels[0] == scenario.vni {
					for _, target := range r.RouteTargets { if target == scenario.rt { seen = true } }
				}
			}
			if !seen { return false }
			if scenario.label == "conflicting-ip-binding" {
				return !o.Snapshot.Authoritative && strings.Contains(o.Error, scenario.reason) &&
					reflect.DeepEqual(o.Snapshot.Endpoints, baseline.Snapshot.Endpoints)
			}
			if !o.Snapshot.Authoritative || o.Error != "" || len(o.Snapshot.Endpoints) != 1 ||
				o.Snapshot.Endpoints[0].MAC != validMAC || o.Snapshot.Endpoints[0].IP != validIP { return false }
			if scenario.label == "duplicate-endpoint" {
				return len(o.Snapshot.Issues) == baselineIssues && len(o.Snapshot.Endpoints[0].Routes) == len(baseline.Snapshot.Endpoints[0].Routes)+1
			}
			if len(o.Snapshot.Issues) != baselineIssues+1 { return false }
			for _, issue := range o.Snapshot.Issues { if issue.Reason == scenario.reason { return true } }
			return false
		})
		if err != nil { return err }
		if err := ack(scenario.label); err != nil { return err }
		withdraw, err := bgp.NewPathAttributeMpUnreachNLRI(bgp.RF_EVPN, []bgp.PathNLRI{{NLRI: nlri}})
		if err != nil { return err }
		if err := sendWireUpdate(wire, []bgp.PathAttributeInterface{withdraw}); err != nil { return err }
		if _, err := wait(scenario.label+"-withdrawn", func(o observation) bool {
			if !healthy(o) || len(o.Snapshot.Endpoints[0].Routes) != len(baseline.Snapshot.Endpoints[0].Routes) { return false }
			if !reflect.DeepEqual(o.Snapshot.Endpoints, baseline.Snapshot.Endpoints) { return false }
			for _, path := range o.RIB.Paths {
				if path.Route.Source.NeighborIP == "127.0.0.2" { return false }
			}
			return true
		}); err != nil { return err }
		if err := ack(scenario.label+"-withdrawn"); err != nil { return err }
	}
	return write("complete.json", map[string]string{
		"result": "PASS", "backend": "OVSDB",
		"scope": "remote TCP BGP received-path RT/VTEP/VNI rejection, distinct-RD endpoint aggregation, same-VTEP conflicting IPv4 freeze",
		"limits": "duplicate endpoint uses a distinct RD, not same-NLRI AddPaths; VTEP allowlist contains exactly one address; no multi-VTEP conflict parity",
	})
}
GO
}

framework_ovsdb_gap_supplemental() {
  FAILURE_CLASSIFICATION=OVSDB-SUPPLEMENTAL-REMOTE-WIRE
  FAILURE_GATE=SUPPLEMENTAL
  local directory="${EVIDENCE_DIR}/ovsdb-gap/supplemental"
  local remote_directory="${SHARED_DIR}/ovsdb-supplemental"
  mkdir -m 700 -p "${directory}"
  framework_ovsdb_gap_wire_source >"${EVIDENCE_DIR}/build/ovsdb-gap-wire.go"
  (cd "${ROOT}" && CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/ovsdb-gap-wire" "${EVIDENCE_DIR}/build/ovsdb-gap-wire.go")
  docker cp "${EVIDENCE_DIR}/build/ovsdb-gap-wire" "${WORKER}:/usr/local/bin/ovsdb-gap-wire"
  ag mkdir -m 700 "${remote_directory}"
  (
    local pid='' label deadline
    trap 'if [[ -n "${pid}" ]]; then ag sh -ec '"'"'test ! -f "$1/pid" || kill -TERM "$(cat "$1/pid")"'"'"' sh "${remote_directory}" >/dev/null 2>&1 || true; wait "${pid}" 2>/dev/null || true; fi; while read -r name; do [[ "${name}" =~ ^[a-z0-9-]+[.]json$ ]] || continue; ag cat "${remote_directory}/${name}" >"${directory}/${name}" || true; done < <(ag find "${remote_directory}" -maxdepth 1 -name "*.json" -printf "%f\n")' EXIT
    ag sh -ec 'echo "$$" >"$1/pid"; exec /usr/local/bin/ovsdb-gap-wire "$@"' sh \
      "${remote_directory}" "${LOCAL_VTEP}" "${FRR_VTEP}" "${REMOTE_MAC}" "${REMOTE_IP}" "${FRR_PEER}" \
      >"${directory}/wire.log" 2>&1 &
    pid=$!
    for label in baseline unauthorized-rt unauthorized-rt-withdrawn unauthorized-vtep unauthorized-vtep-withdrawn \
      mismatched-vni mismatched-vni-withdrawn duplicate-endpoint duplicate-endpoint-withdrawn \
      conflicting-ip-binding conflicting-ip-binding-withdrawn; do
      deadline=$((SECONDS+100))
      while (( SECONDS < deadline )); do
        if [[ "$(ag cat "${remote_directory}/ready" 2>/dev/null || true)" == "${label}" ]]; then break; fi
        kill -0 "${pid}" 2>/dev/null || { cat "${directory}/wire.log" >&2; return 1; }
        sleep 1
      done
      [[ "$(ag cat "${remote_directory}/ready")" == "${label}" ]]
      sleep 4
      framework_ovsdb_gap_capture "supplemental-${label}"
      framework_ovsdb_gap_assert_route "supplemental-${label}"
      if [[ "${label}" != baseline ]]; then
        framework_ovsdb_gap_assert_stable supplemental-baseline "supplemental-${label}" strict
      fi
      if [[ "${label}" == conflicting-ip-binding ]]; then
        grep -Fq 'conflicting IPv4 bindings' "${EVIDENCE_DIR}/ovsdb-gap/supplemental-${label}/importer.log"
      fi
      k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
        ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
        >"${directory}/${label}-ping.txt"
      sleep 6
      framework_ovsdb_gap_capture "supplemental-${label}-after-ping"
      framework_ovsdb_gap_assert_stable "supplemental-${label}" "supplemental-${label}-after-ping" strict
      framework_ovsdb_gap_assert_counter_delta "supplemental-${label}" "supplemental-${label}-after-ping"
      ag sh -ec 'printf "%s\n" "$2" >"$1/ack"' sh "${remote_directory}" "${label}"
    done
    wait "${pid}"
    pid=''
    ag cat "${remote_directory}/complete.json" >"${directory}/complete.json"
    printf 'SUPPLEMENTAL_OVSDB=PASS\n' | tee -a "${EVIDENCE_DIR}/ovsdb-gap/case-results.txt"
  )
}
