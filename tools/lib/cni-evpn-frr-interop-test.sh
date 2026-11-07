#!/usr/bin/env bash
# Sourced only by the disposable CNI runner in explicit FRR interop mode.

FRR_IMAGE='quay.io/frrouting/frr:10.7.1@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2'
FRR_IMAGE_ID='sha256:27be65e6d6382c092d5ec87105eb8d38bee9e257e21963289623b24ceb3c99cd'
UNDERLAY_CIDR='198.18.0.0/24'
LOCAL_VTEP='198.18.0.2'
FRR_PEER='198.18.0.3'
FRR_VTEP='198.18.0.4'
VNI=1000
REMOTE_MAC='0a:58:f0:01:00:0c'
REMOTE_IP='10.244.240.12'
LOCAL_IP='10.244.240.10'
BRIDGE_DEVICE='evpnbr33'
VXLAN_DEVICE='evpnvx33'
ADVERTISE_DEVICE='evpnad33'
INTEROP_NETWORK="evpn33b3-${KIND_CLUSTER}"
FOREIGN_SWITCH="interop-foreign-${KIND_CLUSTER}"
FOREIGN_LSP='interop-foreign-sentinel'
FRR_CONTAINER="${KIND_CLUSTER}-frr"
INTEROP_CREATED=0
INTEROP_CAPTURE_UNITS=()
INTEROP_CAPTURE_NAMES=()
INTEROP_CAPTURE_MAIN_PIDS=()
INTEROP_CAPTURE_CGROUPS=()
INTEROP_CAPTURE_STAGING_DIR=''
INTEROP_CAPTURE_STOPPED=0
INTEROP_CAPTURE_PHASE=initial
INTEROP_CAPTURE_FAILURE=0
INTEROP_CAPTURE_SHUTDOWN_FAILURE=0
INTEROP_PACKET_VERIFICATION_FAILURE=0
INTEROP_CAPTURE_PREFLIGHT_STATUS=NOT_RUN
INTEROP_CONTROL_PLANE_STATUS=NOT_RUN
INTEROP_FORWARDING_STATE_STATUS=NOT_RUN
INTEROP_CAPTURE_STARTUP_STATUS=NOT_RUN
INTEROP_FORWARD_ICMP_STATUS=NOT_RUN
INTEROP_REVERSE_ICMP_STATUS=NOT_RUN
INTEROP_PACKET_VERIFICATION_STATUS=NOT_RUN
INTEROP_CAPTURE_SHUTDOWN_STATUS=NOT_RUN
INTEROP_LIFECYCLE_STATUS=NOT_RUN
INTEROP_SHARED_VTEP_STATUS=NOT_RUN
INTEROP_FAILURE_CLASSIFICATION=UNRESOLVED
INTEROP_FAILURE_GATE=NOT_REACHED
INTEROP_TOPOLOGY_SCOPE=single-ovn-chassis-external-frr-peer
INTEROP_REMOTE_ROUTE_VTEP=''
INTEROP_OVS_TUNNEL=''
INTEROP_OVS_OFPORT=''
INTEROP_POD_VETH=''
INTEROP_WORKER_IFACE=''
INTEROP_FRR_IFACE=''
INTEROP_FRR_VXLAN_IFACE=''
INTEROP_FRR_HOST_IFACE=''
INTEROP_WORKER_PID=''
INTEROP_FRR_PID=''

provider_exec() {
  timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n k8gobgp-system exec "${PROVIDER_POD}" -- "$@"
}
interop_exec_pod() {
  timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec pod-a -- "$@"
}
interop_exec_ovn() {
  timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- "$@"
}
interop_exec_worker() {
  timeout --kill-after=2s 15s docker exec "${WORKER}" "$@"
}
interop_exec_frr() {
  timeout --kill-after=2s 15s docker exec "${FRR_CONTAINER}" "$@"
}

interop_create_remote_workload() {
  if docker exec "${FRR_CONTAINER}" ip netns list | grep -qw evpnwork; then
    echo "refusing pre-existing remote workload namespace" >&2
    return 1
  fi
  docker exec "${FRR_CONTAINER}" ip netns add evpnwork
  docker exec "${FRR_CONTAINER}" ip link add frr-host0 type veth peer name frr-work0
  docker exec "${FRR_CONTAINER}" ip link set frr-host0 master br1000
  docker exec "${FRR_CONTAINER}" ip link set frr-host0 up
  docker exec "${FRR_CONTAINER}" ip link set frr-work0 netns evpnwork
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ip link set lo up
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ip link set frr-work0 address "${REMOTE_MAC}"
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ip addr add "${REMOTE_IP}/24" dev frr-work0
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ip link set frr-work0 up
}

interop_setup_provider() {
  EXPORT_HOST_PID='      hostPID: true'
  EXPORT_PROCESS_CONFIG='"ProcessContinuity":true,'
  export_setup_provider
  REMOTE_FLAGS='--evpn-remote-config=/run/evpn-node-agent/remote-config.json --evpn-remote-status=/run/evpn-node-agent/remote-status.json --evpn-remote-interval=1s --evpn-remote-timeout=3s'
}

interop_setup_underlay() {
  [[ "$(docker image inspect "${FRR_IMAGE}" --format '{{.Id}}')" == "${FRR_IMAGE_ID}" ]]
  python3 "${ROOT}/tools/e2e/containerlab.py" status --directory "${EVIDENCE_DIR}"
  INTEROP_CREATED=1
  docker network inspect "${INTEROP_NETWORK}" --format '{{.Id}}' >"${EVIDENCE_DIR}/underlay-network-id.txt"
  docker container inspect "${FRR_CONTAINER}" --format '{{.Id}}' >"${EVIDENCE_DIR}/frr-container-id.txt"
  for _ in $(seq 1 60); do
    if docker exec "${FRR_CONTAINER}" sh -ec 'test -s /var/run/frr/bgpd.pid && kill -0 "$(cat /var/run/frr/bgpd.pid)"' >/dev/null 2>&1; then break; fi
    sleep 1
  done
  docker exec "${FRR_CONTAINER}" sh -ec 'test -s /var/run/frr/bgpd.pid && kill -0 "$(cat /var/run/frr/bgpd.pid)"'
  docker exec "${FRR_CONTAINER}" ip addr add "${FRR_VTEP}/24" dev eth0
  docker exec "${FRR_CONTAINER}" ip link add br1000 type bridge
  docker exec "${FRR_CONTAINER}" ip addr add "10.244.240.1/24" dev br1000
  docker exec "${FRR_CONTAINER}" ip link set br1000 up
  docker exec "${FRR_CONTAINER}" ip link add vxlan1000 type vxlan id "${VNI}" local "${FRR_VTEP}" dstport 4789 nolearning
  docker exec "${FRR_CONTAINER}" ip link set vxlan1000 master br1000
  docker exec "${FRR_CONTAINER}" ip link set vxlan1000 up
  # Type-2 advertises unicast endpoints; this peer entry only replicates BUM traffic.
  docker exec "${FRR_CONTAINER}" bridge fdb append 00:00:00:00:00:00 dev vxlan1000 dst "${LOCAL_VTEP}" self permanent
  docker exec "${FRR_CONTAINER}" bridge -j fdb show dev vxlan1000 | tee "${EVIDENCE_DIR}/frr-vxlan-flood-fdb.json" >/dev/null
  interop_create_remote_workload
  docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
    -c "bgp router-id ${FRR_PEER}" -c "neighbor ${LOCAL_VTEP} remote-as 64512" \
    -c 'address-family l2vpn evpn' -c "neighbor ${LOCAL_VTEP} activate" \
    -c 'advertise-all-vni' \
    -c "vni ${VNI}" -c "rd ${FRR_PEER}:${VNI}" \
    -c 'route-target import 64512:1000' -c 'route-target export 64512:1000' -c 'exit-vni' -c 'end'
  if [[ "${EVPN_FRR_INTEROP_INITIAL_ONLY:-0}" == 1 ]]; then
    docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 1 -W 2 10.244.240.1 \
      | tee "${EVIDENCE_DIR}/remote-linux-warmup.txt"
  else
    docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 10.244.240.1 \
      | tee "${EVIDENCE_DIR}/remote-linux-warmup.txt"
  fi
  docker exec "${FRR_CONTAINER}" ip -j addr show | tee "${EVIDENCE_DIR}/frr-addresses.json" >/dev/null
  docker exec "${FRR_CONTAINER}" ip -d -j link show | tee "${EVIDENCE_DIR}/frr-links.json" >/dev/null
  docker exec "${FRR_CONTAINER}" bridge -j fdb show | tee "${EVIDENCE_DIR}/frr-fdb-warmup.json" >/dev/null
  docker exec "${FRR_CONTAINER}" ip -4 -j neigh show nud all | tee "${EVIDENCE_DIR}/frr-neigh-warmup.json" >/dev/null
  docker exec "${FRR_CONTAINER}" vtysh -c 'show version' | tee "${EVIDENCE_DIR}/frr-version.txt"
  grep -Eq '^FRRouting 10\.7\.1(_git)?[ (]' "${EVIDENCE_DIR}/frr-version.txt" || {
    echo "FRR runtime is not the required 10.7.1 release" >&2; return 1;
  }
  docker image inspect "${FRR_IMAGE}" >"${EVIDENCE_DIR}/frr-image.json"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show running-config' \
    | tee "${EVIDENCE_DIR}/frr-running-config.txt" >/dev/null
  docker inspect "${FRR_CONTAINER}" | tee "${EVIDENCE_DIR}/frr-container.json" >/dev/null
  docker network inspect "${INTEROP_NETWORK}" | tee "${EVIDENCE_DIR}/underlay-network.json" >/dev/null
  docker exec "${WORKER}" ip -4 -o addr show | tee "${EVIDENCE_DIR}/worker-addresses.txt"
  python3 - "${EVIDENCE_DIR}/underlay-network.json" "${FRR_CONTAINER}" "${WORKER}" <<'PY'
import json,sys
n=json.load(open(sys.argv[1]))[0]
members={entry["Name"] for entry in n["Containers"].values()}
assert members=={sys.argv[2],sys.argv[3]},members
assert n["IPAM"]["Config"][0]["Subnet"]=="198.18.0.0/24"
print("isolated-underlay-members=PASS")
PY
}

interop_setup_config() {
  OVN_POD="$(k -n cni-test get pod -l "${OVN_RUNTIME_LABEL:-app=ovn-runtime}" -o jsonpath='{.items[0].metadata.name}')"
  LOCAL_CHASSIS="$(rt ovs-vsctl --bare get Open_vSwitch . external_ids:system-id | tr -d '"')"
  [[ -n "${LOCAL_CHASSIS}" ]]
  docker exec "${WORKER}" ip -4 -o addr show | grep -F " ${LOCAL_VTEP}/24 " >/dev/null
  for device in "${BRIDGE_DEVICE}" "${VXLAN_DEVICE}" "${ADVERTISE_DEVICE}"; do
    if docker exec "${WORKER}" ip link show "${device}" >/dev/null 2>&1; then
      echo "refusing pre-existing native Netlink device ${device}" >&2
      return 1
    fi
  done
  docker exec "${WORKER}" ip link add "${BRIDGE_DEVICE}" type bridge
  docker exec "${WORKER}" ip link set "${BRIDGE_DEVICE}" up
  docker exec "${WORKER}" ip link add "${VXLAN_DEVICE}" type vxlan id "${VNI}" local "${LOCAL_VTEP}" dstport 60100 nolearning
  docker exec "${WORKER}" ip link set "${VXLAN_DEVICE}" master "${BRIDGE_DEVICE}"
  docker exec "${WORKER}" ip link set "${VXLAN_DEVICE}" up
  docker exec "${WORKER}" ip link add "${ADVERTISE_DEVICE}" type dummy
  docker exec "${WORKER}" ip link set "${ADVERTISE_DEVICE}" master "${BRIDGE_DEVICE}"
  docker exec "${WORKER}" ip link set "${ADVERTISE_DEVICE}" up
  rt ovs-vsctl set Open_vSwitch . "external_ids:ovn-evpn-local-ip=${LOCAL_VTEP}" external_ids:ovn-evpn-vxlan-ports=4789
  rt ovn-nbctl -- ls-add ls24b \
    -- set Logical_Switch ls24b other_config:dynamic-routing-vni="${VNI}" \
      other_config:dynamic-routing-bridge-ifname="${BRIDGE_DEVICE}" \
      other_config:dynamic-routing-vxlan-ifname="${VXLAN_DEVICE}" \
      other_config:dynamic-routing-advertise-ifname="${ADVERTISE_DEVICE}" \
      other_config:dynamic-routing-redistribute=fdb,ip \
    -- ls-add "${FOREIGN_SWITCH}" \
    -- lsp-add "${FOREIGN_SWITCH}" "${FOREIGN_LSP}" \
    -- lsp-set-addresses "${FOREIGN_LSP}" 02:00:00:00:fa:02
  cat > "${EVIDENCE_DIR}/provider-config.yaml" <<EOF
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata: {name: cni-export, namespace: k8gobgp-system}
spec:
  global:
    asn: 64512
    routerID: "${LOCAL_VTEP}"
    listenPort: 179
    listenAddresses: ["${LOCAL_VTEP}"]
    families: ["l2vpn-evpn"]
  neighbors:
  - config:
      neighborAddress: "${FRR_PEER}"
      peerAsn: 64512
      description: "disposable-independent-FRR-EVPN-peer"
    afiSafis:
    - family: "l2vpn-evpn"
      enabled: true
EOF
  k apply -f "${EVIDENCE_DIR}/provider-config.yaml"
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
  PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
  export_record_provider
  cat > "${EVIDENCE_DIR}/export-config.json" <<EOF
{"Enabled":true,"DryRun":false,"ProcessContinuity":true,"NodeName":"${WORKER}","LocalChassis":"${LOCAL_CHASSIS}","GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Networks":[{"NetworkID":"n24","LogicalSwitch":"ls24b","Enabled":true,"ExporterSource":"ovn-netlink","BridgeDevice":"${BRIDGE_DEVICE}","VXLANDevice":"${VXLAN_DEVICE}","AdvertiseDevice":"${ADVERTISE_DEVICE}","VNI":${VNI},"RouteDistinguisher":"${LOCAL_VTEP}:${VNI}","RouteTargets":["64512:${VNI}"],"LocalVTEP":"${LOCAL_VTEP}"}]}
EOF
  cat > "${EVIDENCE_DIR}/remote-config.json" <<EOF
{"Enabled":true,"GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Import":{"LocalVTEP":"${LOCAL_VTEP}","Networks":[{"NetworkID":"n24","VNI":${VNI},"ImportRouteTargets":["64512:${VNI}"],"AllowedVTEPs":["${FRR_VTEP}"]}]},"Netlink":{"Enabled":true,"ExclusiveWriter":true,"StateDirectory":"/run/evpn-node-agent/remote-state","Networks":[{"NetworkID":"n24","LogicalSwitch":"ls24b","VNI":${VNI},"BridgeDevice":"${BRIDGE_DEVICE}","VXLANDevice":"${VXLAN_DEVICE}"}]}}
EOF
  docker exec "${WORKER}" mkdir -p /run/evpn-node-agent/remote-state
  docker exec "${WORKER}" chmod 700 /run/evpn-node-agent/remote-state
  docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.json' < "${EVIDENCE_DIR}/export-config.json"
  docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/remote-config.json' < "${EVIDENCE_DIR}/remote-config.json"
  cat > "${EVIDENCE_DIR}/interop-run.json" <<EOF
{"run":"${KIND_CLUSTER}","kindCluster":"${KIND_CLUSTER}","worker":"${WORKER}","underlayNetwork":"${INTEROP_NETWORK}","frrContainer":"${FRR_CONTAINER}","workerVtep":"${LOCAL_VTEP}","frrPeer":"${FRR_PEER}","frrVtep":"${FRR_VTEP}","vni":${VNI},"rd":"${LOCAL_VTEP}:${VNI}","rt":"64512:${VNI}","vxlanUdpPort":4789,"netlinkSyncUdpPort":60100}
EOF
  cat "${EVIDENCE_DIR}/interop-run.json"
}

interop_capture_discover() {
  local phase="${1:-initial}" prefix=""
  local pod_ifindex pod_net1_json worker_links_json frr_workload_json
  local worker_addresses_json frr_addresses_json frr_vxlan_json frr_host_json
  local -a pod_identity remote_identity
  [[ "${phase}" == initial ]] || prefix="${phase}-"
  INTEROP_WORKER_PID="$(docker inspect --format '{{.State.Pid}}' "${WORKER}")"
  INTEROP_FRR_PID="$(docker inspect --format '{{.State.Pid}}' "${FRR_CONTAINER}")"
  [[ "${INTEROP_WORKER_PID}" =~ ^[0-9]+$ && "${INTEROP_FRR_PID}" =~ ^[0-9]+$ ]]

  pod_net1_json="${EVIDENCE_DIR}/${prefix}pod-net1-link.json"
  interop_exec_pod ip -j addr show dev net1 | tee "${pod_net1_json}" >/dev/null
  interop_exec_pod ip -j link show dev net1 \
    | tee "${EVIDENCE_DIR}/${prefix}pod-net1-link-details.json" >/dev/null
  frr_workload_json="${EVIDENCE_DIR}/${prefix}frr-workload-link.json"
  interop_exec_frr ip netns exec evpnwork ip -j addr show dev frr-work0 \
    >"${frr_workload_json}"
  interop_exec_frr ip netns exec evpnwork ip -d addr show dev frr-work0 \
    >"${EVIDENCE_DIR}/${prefix}remote-workload.txt"
  worker_addresses_json="${EVIDENCE_DIR}/${prefix}worker-addresses-capture.json"
  frr_addresses_json="${EVIDENCE_DIR}/${prefix}frr-addresses-capture.json"
  worker_links_json="${EVIDENCE_DIR}/${prefix}worker-links-capture.json"
  local worker_sync_vxlan_json="${EVIDENCE_DIR}/${prefix}worker-sync-vxlan.json"
  frr_vxlan_json="${EVIDENCE_DIR}/${prefix}frr-vxlan-links.json"
  frr_host_json="${EVIDENCE_DIR}/${prefix}frr-host-interface.json"
  interop_exec_worker ip -j addr show >"${worker_addresses_json}"
  interop_exec_worker ip -j link show >"${worker_links_json}"
  interop_exec_worker ip -d -j link show dev "${VXLAN_DEVICE}" >"${worker_sync_vxlan_json}"
  interop_exec_frr ip -j addr show >"${frr_addresses_json}"
  interop_exec_frr ip -d -j link show type vxlan >"${frr_vxlan_json}"
  interop_exec_frr ip -j link show frr-host0 >"${frr_host_json}"
  pod_ifindex="$(interop_exec_pod cat /sys/class/net/net1/ifindex)"

  mapfile -t pod_identity < <(python3 - "${pod_net1_json}" <<'PY'
import json,sys
items=json.load(open(sys.argv[1]))
assert len(items)==1,items
link=items[0]
addresses=[x["local"] for x in link.get("addr_info",[]) if x.get("family")=="inet"]
assert len(addresses)==1,(link,addresses)
print(addresses[0])
print(link["address"].lower())
PY
)
  mapfile -t remote_identity < <(python3 - "${frr_workload_json}" <<'PY'
import json,sys
items=json.load(open(sys.argv[1]))
assert len(items)==1,items
link=items[0]
addresses=[x["local"] for x in link.get("addr_info",[]) if x.get("family")=="inet"]
assert len(addresses)==1,(link,addresses)
print(addresses[0])
print(link["address"].lower())
PY
)
  [[ "${#pod_identity[@]}" == 2 && "${#remote_identity[@]}" == 2 ]]
  [[ "${pod_identity[0]}" == "${LOCAL_IP}" && "${remote_identity[0]}" == "${REMOTE_IP}" ]]
  LOCAL_MAC="${pod_identity[1]}"
  REMOTE_MAC="${remote_identity[1]}"
  printf 'Pod net1: ip=%s mac=%s ifindex=%s\n' "${LOCAL_IP}" "${LOCAL_MAC}" "${pod_ifindex}" \
    >"${EVIDENCE_DIR}/${prefix}endpoint-identities.txt"
  printf 'FRR workload: ip=%s mac=%s\n' "${REMOTE_IP}" "${REMOTE_MAC}" \
    >>"${EVIDENCE_DIR}/${prefix}endpoint-identities.txt"

  INTEROP_POD_VETH="$(python3 - "${worker_links_json}" "${pod_ifindex}" <<'PY'
import json,sys
links=json.load(open(sys.argv[1]))
matches=[link["ifname"] for link in links if link.get("link_index")==int(sys.argv[2])]
assert len(matches)==1,matches
print(matches[0])
PY
)"
  INTEROP_WORKER_IFACE="$(python3 - "${worker_addresses_json}" "${LOCAL_VTEP}" <<'PY'
import json,sys
matches=[]
for link in json.load(open(sys.argv[1])):
    if any(x.get("local")==sys.argv[2] for x in link.get("addr_info",[])):
        matches.append(link["ifname"])
assert len(matches)==1,matches
print(matches[0])
PY
)"
  INTEROP_FRR_IFACE="$(python3 - "${frr_addresses_json}" "${FRR_VTEP}" <<'PY'
import json,sys
matches=[]
for link in json.load(open(sys.argv[1])):
    if any(x.get("local")==sys.argv[2] for x in link.get("addr_info",[])):
        matches.append(link["ifname"])
assert len(matches)==1,matches
print(matches[0])
PY
)"
  INTEROP_FRR_VXLAN_IFACE="$(python3 - "${frr_vxlan_json}" "${FRR_VTEP}" "${VNI}" <<'PY'
import json,sys
matches=[]
for link in json.load(open(sys.argv[1])):
    info=link.get("linkinfo",{}).get("info_data",{})
    if (str(info.get("id"))==sys.argv[3] or info.get("id")==int(sys.argv[3])) and \
       info.get("local")==sys.argv[2] and info.get("port")==4789:
        matches.append(link["ifname"])
assert len(matches)==1,matches
print(matches[0])
PY
)"
  INTEROP_FRR_HOST_IFACE="$(python3 - "${frr_host_json}" <<'PY'
import json,sys
items=json.load(open(sys.argv[1]))
assert len(items)==1,items
link=items[0]
assert link.get("ifname")=="frr-host0" and link.get("master")=="br1000",link
assert link.get("operstate") in ("UP","UNKNOWN"),link
print(link["ifname"])
PY
)"
  [[ -n "${INTEROP_POD_VETH}" && -n "${INTEROP_WORKER_IFACE}" && -n "${INTEROP_FRR_IFACE}" ]]
  python3 - "${worker_sync_vxlan_json}" "${LOCAL_VTEP}" "${VNI}" <<'PY'
import json,sys
links=json.load(open(sys.argv[1]))
assert len(links)==1,links
link=links[0]
info=link.get("linkinfo",{}).get("info_data",{})
assert str(info.get("id"))==sys.argv[3],link
assert info.get("local")==sys.argv[2] and info.get("port")==60100,link
print("kernel synchronization VXLAN: device=%s vni=%s local=%s dstport=%s master=%s" %
      (link["ifname"],info["id"],info["local"],info["port"],link.get("master")))
PY
  interop_exec_worker ip -d link show dev "${VXLAN_DEVICE}" \
    >"${EVIDENCE_DIR}/${prefix}worker-sync-vxlan.txt"
  grep -Fq "master ${BRIDGE_DEVICE}" "${EVIDENCE_DIR}/${prefix}worker-sync-vxlan.txt"
  interop_exec_frr ip -d link show dev "${INTEROP_FRR_VXLAN_IFACE}" \
    >"${EVIDENCE_DIR}/${prefix}frr-vxlan-interface.txt"
  grep -Fq 'master br1000' "${EVIDENCE_DIR}/${prefix}frr-vxlan-interface.txt"
  interop_exec_frr ip -d link show dev "${INTEROP_FRR_HOST_IFACE}" \
    | tee "${EVIDENCE_DIR}/${prefix}frr-host-interface.txt"
  interop_exec_worker readlink "/proc/1/ns/net" >"${EVIDENCE_DIR}/${prefix}worker-netns.txt"
  interop_exec_frr readlink "/proc/1/ns/net" >"${EVIDENCE_DIR}/${prefix}frr-netns.txt"
}

interop_capture_ovs_snapshot() {
  local phase="$1" capture_phase="${2:-initial}" prefix=""
  local tunnel_type tunnel_options tunnel_stats
  [[ "${capture_phase}" == initial ]] || prefix="${capture_phase}-"
  interop_exec_ovn ovs-vsctl show | tee "${EVIDENCE_DIR}/${prefix}ovs-show-${phase}.txt" >/dev/null
  interop_exec_ovn ovs-vsctl --columns=name,type,options,statistics,ofport list Interface \
    | tee "${EVIDENCE_DIR}/${prefix}ovs-interfaces-${phase}.txt" >/dev/null
  interop_exec_ovn ovs-ofctl dump-flows br-int | tee "${EVIDENCE_DIR}/${prefix}ovs-flows-${phase}.txt" >/dev/null
  interop_exec_ovn ovs-ofctl dump-ports br-int | tee "${EVIDENCE_DIR}/${prefix}ovs-ports-${phase}.txt" >/dev/null
  interop_exec_ovn ovs-appctl dpctl/show | tee "${EVIDENCE_DIR}/${prefix}ovs-dpctl-${phase}.txt" >/dev/null
  if [[ "${phase}" == before ]]; then
    local -a candidates=()
    mapfile -t candidates < <(interop_exec_ovn ovs-vsctl --bare --columns=name find Interface \
      type=vxlan options:dst_port=4789)
    if [[ "${#candidates[@]}" != 1 ]]; then
      printf 'expected one OVS VXLAN/4789 interface, found %s: %s\n' \
        "${#candidates[@]}" "${candidates[*]}" >&2
      return 1
    fi
    INTEROP_OVS_TUNNEL="${candidates[0]}"
  fi
  [[ -n "${INTEROP_OVS_TUNNEL}" ]]
  tunnel_type="$(interop_exec_ovn ovs-vsctl --bare get Interface "${INTEROP_OVS_TUNNEL}" type | tr -d '"')"
  tunnel_options="$(interop_exec_ovn ovs-vsctl --bare get Interface "${INTEROP_OVS_TUNNEL}" options)"
  tunnel_stats="$(interop_exec_ovn ovs-vsctl --bare get Interface "${INTEROP_OVS_TUNNEL}" statistics)"
  INTEROP_OVS_OFPORT="$(interop_exec_ovn ovs-vsctl --bare get Interface "${INTEROP_OVS_TUNNEL}" ofport | tr -d '[] "')"
  [[ "${tunnel_type}" == vxlan && "${INTEROP_OVS_OFPORT}" =~ ^[0-9]+$ ]]
  (( INTEROP_OVS_OFPORT > 0 ))
  {
    printf 'name=%s\n' "${INTEROP_OVS_TUNNEL}"
    printf 'type=%s\n' "${tunnel_type}"
    printf 'options=%s\n' "${tunnel_options}"
    printf 'ofport=%s\n' "${INTEROP_OVS_OFPORT}"
    printf 'statistics=%s\n' "${tunnel_stats}"
  } >"${EVIDENCE_DIR}/${prefix}ovs-tunnel-${phase}.txt"
}

interop_capture_create_staging_dir() {
  local owner expected
  [[ -z "${INTEROP_CAPTURE_STAGING_DIR}" ]] || {
    echo "capture staging directory already initialized" >&2
    return 1
  }
  INTEROP_CAPTURE_STAGING_DIR="$(mktemp -d /tmp/evpn4ovn-capture.XXXXXX)" || return 1
  chmod 0700 "${INTEROP_CAPTURE_STAGING_DIR}" || return 1
  owner="$(stat -c '%u:%g:%a' "${INTEROP_CAPTURE_STAGING_DIR}")" || return 1
  expected="$(id -u):$(id -g):700"
  if [[ "${owner}" != "${expected}" || -L "${INTEROP_CAPTURE_STAGING_DIR}" ]]; then
    echo "capture staging directory has unexpected owner or permissions: ${owner}" >&2
    return 1
  fi
  printf '%s staging-created directory=%s owner=%s apparmor-path=/tmp\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${INTEROP_CAPTURE_STAGING_DIR}" "${owner}" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log" || return 1
}

interop_capture_discard_staging_dir() {
  local name path failed=0 prefix=""
  [[ -n "${INTEROP_CAPTURE_STAGING_DIR}" ]] || return 0
  [[ "${INTEROP_CAPTURE_PHASE:-initial}" == initial ]] ||
    prefix="${INTEROP_CAPTURE_PHASE}-"
  for name in preflight; do
    path="${INTEROP_CAPTURE_STAGING_DIR}/${name}.pcap"
    if [[ -L "${path}" ]]; then
      echo "refusing symlink in capture staging directory: ${path}" >&2
      failed=1
    elif [[ -e "${path}" ]]; then
      rm -f -- "${path}" || failed=1
    fi
  done
  for name in pod-net1-peer worker-underlay frr-underlay frr-vxlan remote-workload-peer; do
    path="${INTEROP_CAPTURE_STAGING_DIR}/${prefix}${name}.pcap"
    if [[ -L "${path}" ]]; then
      echo "refusing symlink in capture staging directory: ${path}" >&2
      failed=1
    elif [[ -e "${path}" ]]; then
      rm -f -- "${path}" || failed=1
    fi
  done
  if (( failed == 0 )); then
    rmdir -- "${INTEROP_CAPTURE_STAGING_DIR}" || failed=1
  fi
  if (( failed == 0 )); then
    printf '%s staging-removed directory=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${INTEROP_CAPTURE_STAGING_DIR}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log" || return 1
    INTEROP_CAPTURE_STAGING_DIR=''
    return 0
  fi
  echo "capture staging cleanup failed: ${INTEROP_CAPTURE_STAGING_DIR}" >&2
  return 1
}

interop_capture_preflight_fail() {
  local unit="$1" failed=0 load_state=not-found
  if [[ -n "${unit}" ]]; then
    load_state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=LoadState --value 2>/dev/null || echo not-found)"
    if [[ "${load_state}" != not-found ]]; then
      timeout --kill-after=1s 5s sudo -n systemctl stop "${unit}" >/dev/null 2>&1 || failed=1
      load_state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
        --property=LoadState --value 2>/dev/null || echo not-found)"
      if [[ "${load_state}" != not-found ]]; then
        timeout --kill-after=1s 5s sudo -n systemctl reset-failed "${unit}" >/dev/null 2>&1 || failed=1
      fi
    fi
    timeout --kill-after=1s 5s sudo -n journalctl --no-pager --unit="${unit}" \
      --output=cat >>"${EVIDENCE_DIR}/capture-preflight-service.log" 2>&1 || true
  fi
  INTEROP_CAPTURE_PREFLIGHT_STATUS=FAIL
  interop_capture_discard_staging_dir || failed=1
  (( failed == 0 ))
}

interop_capture_preflight() {
  local unit log pcap expected_uid expected_gid state pid actual_uid actual_gid
  local deadline exit_status service_result file_owner file_mode file_size decoded decoded_path
  INTEROP_CAPTURE_PREFLIGHT_STATUS=FAIL
  interop_capture_create_staging_dir || {
    interop_capture_discard_staging_dir || true
    return 1
  }
  unit="cni-d22d-${KIND_CLUSTER}-capture-preflight.service"
  log="${EVIDENCE_DIR}/capture-preflight-service.log"
  pcap="${INTEROP_CAPTURE_STAGING_DIR}/preflight.pcap"
  if timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
    --property=LoadState --value 2>/dev/null | grep -Fxqv 'not-found'; then
    echo "refusing pre-existing capture preflight service: ${unit}" >&2
    interop_capture_preflight_fail "" || true
    return 1
  fi
  if [[ -e "${log}" || -L "${log}" ]]; then
    echo "refusing pre-existing capture preflight log: ${log}" >&2
    interop_capture_preflight_fail "" || true
    return 1
  fi
  (umask 077; : >"${log}") || {
    interop_capture_preflight_fail "" || true
    return 1
  }
  chmod 0600 "${log}" || {
    interop_capture_preflight_fail "" || true
    return 1
  }
  if ! getent passwd tcpdump >/dev/null; then
    echo "tcpdump execution account is unavailable" >&2
    interop_capture_preflight_fail "" || true
    return 1
  fi
  IFS=: read -r _ _ expected_uid expected_gid _ < <(getent passwd tcpdump)
  if ! timeout --kill-after=2s 10s sudo -n systemd-run --system --quiet \
    --remain-after-exit \
    --unit="${unit%.service}" \
    --property=User=root \
    --property=Group=root \
    --property=UMask=0022 \
    --property=RuntimeMaxSec=15s \
    --property=LimitFSIZE=69656 \
    --property=TimeoutStopSec=10s \
    --property=KillSignal=SIGINT \
    --property=KillMode=control-group \
    --property="StandardOutput=append:${log}" \
    --property="StandardError=append:${log}" \
    /usr/bin/nsenter --no-fork -t "$$" -n /usr/bin/tcpdump \
      -Z tcpdump -n -i lo -s 256 -U -w "${pcap}" -c 1 icmp; then
    echo "bounded tcpdump capture preflight could not launch" >&2
    interop_capture_preflight_fail "" || true
    return 1
  fi
  deadline=$((SECONDS+10))
  pid=''
  while (( SECONDS < deadline )); do
    state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=ActiveState --value 2>/dev/null || true)"
    if [[ "${state}" == active ]]; then
      if grep -Fq 'listening on lo' "${log}"; then
        pid="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
          --property=MainPID --value 2>/dev/null || true)"
        break
      fi
    elif [[ "${state}" == failed || "${state}" == inactive || "${state}" == unknown ]]; then
      cat "${log}" >&2 2>/dev/null || true
      echo "tcpdump preflight service exited before capture readiness: ${state}" >&2
      interop_capture_preflight_fail "${unit}" || true
      return 1
    fi
    sleep 0.1
  done
  if [[ ! "${pid}" =~ ^[0-9]+$ || "${pid}" == 0 || ! -r "/proc/${pid}/status" ]]; then
    echo "tcpdump preflight did not become ready within 10 seconds" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  actual_uid="$(awk '/^Uid:/ {print $3}' "/proc/${pid}/status")"
  actual_gid="$(awk '/^Gid:/ {print $3}' "/proc/${pid}/status")"
  if [[ "${actual_uid}" != "${expected_uid}" || "${actual_gid}" != "${expected_gid}" ]]; then
    echo "tcpdump dropped to unexpected identity uid=${actual_uid} gid=${actual_gid}" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  if ! timeout --kill-after=1s 5s ping -n -I lo -c 1 -W 1 127.0.0.1 \
    >"${EVIDENCE_DIR}/capture-preflight-ping.txt" 2>&1; then
    echo "bounded loopback stimulus failed during tcpdump preflight" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  deadline=$((SECONDS+5))
  while (( SECONDS < deadline )); do
    state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=ActiveState --value 2>/dev/null || true)"
    pid="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=MainPID --value 2>/dev/null || true)"
    [[ "${pid}" == 0 && ( "${state}" == active || "${state}" == failed ) ]] && break
    sleep 0.1
  done
  exit_status="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
    --property=ExecMainStatus --value 2>/dev/null || echo unknown)"
  service_result="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
    --property=Result --value 2>/dev/null || echo unknown)"
  if [[ "${exit_status}" != 0 || "${service_result}" != success || ! -s "${pcap}" ]] ||
    ! grep -Fq '1 packet captured' "${log}"; then
    echo "tcpdump preflight did not produce a successful capture" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  file_owner="$(stat -c '%u:%g' "${pcap}")"
  file_mode="$(stat -c '%a' "${pcap}")"
  file_size="$(stat -c '%s' "${pcap}")"
  if [[ "${file_owner}" != "${expected_uid}:${expected_gid}" || ! -r "${pcap}" ||
        "${file_mode}" != 644 || ! "${file_size}" =~ ^[0-9]+$ ]] ||
    (( file_size <= 24 || file_size > 69656 )); then
    echo "tcpdump preflight output ownership, permissions, or size is unexpected" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  decoded_path="${EVIDENCE_DIR}/capture-preflight-decoded.txt"
  if ! decoded="$(timeout --kill-after=2s 10s tcpdump -nn -r "${pcap}" 2>&1)"; then
    printf '%s\n' "${decoded}" >&2
    echo "tcpdump preflight PCAP decode failed" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  printf '%s\n' "${decoded}" >"${decoded_path}"
  if ! grep -Fq 'ICMP echo request' "${decoded_path}"; then
    printf '%s\n' "${decoded}" >&2
    echo "tcpdump preflight output did not contain the loopback ICMP packet" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  timeout --kill-after=2s 10s sudo -n systemctl stop "${unit}" || {
    echo "failed to stop completed tcpdump preflight service" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  }
  state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
    --property=LoadState --value 2>/dev/null || echo not-found)"
  if [[ "${state}" != not-found ]] &&
    ! timeout --kill-after=1s 5s sudo -n systemctl reset-failed "${unit}"; then
    echo "failed to reset completed tcpdump preflight service" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  if ! python3 - "${EVIDENCE_DIR}/capture-preflight.json" \
    "$(id -u)" "$(id -g)" "${expected_uid}" "${expected_gid}" \
    "${file_owner}" "${file_mode}" "${file_size}" <<'PY'
import json,sys
path,runner_uid,runner_gid,capture_uid,capture_gid,file_owner,file_mode,file_size=sys.argv[1:]
json.dump({
    "status":"PASS",
    "serviceIdentity":{"uid":0,"gid":0},
    "tcpdumpIdentity":{"effectiveUid":int(capture_uid),"effectiveGid":int(capture_gid)},
    "staging":{"path":"/tmp","ownerUid":int(runner_uid),"ownerGid":int(runner_gid),"directoryMode":"0700"},
    "output":{"owner":file_owner,"mode":file_mode,"bytes":int(file_size)},
    "command":{"interface":"lo","filter":"icmp","packetLimit":1,"maximumDurationSeconds":15,"privilegeDropUser":"tcpdump"},
},open(path,"w"),indent=2)
PY
  then
    echo "failed to record tcpdump preflight evidence" >&2
    interop_capture_preflight_fail "${unit}" || true
    return 1
  fi
  interop_capture_discard_staging_dir || {
    echo "tcpdump preflight passed but temporary output cleanup failed" >&2
    INTEROP_CAPTURE_PREFLIGHT_STATUS=FAIL
    return 1
  }
  INTEROP_CAPTURE_PREFLIGHT_STATUS=PASS
  echo "capture-output-preflight=PASS service=root tcpdump=${expected_uid}:${expected_gid} staging=/tmp"
}

interop_capture_publish_outputs() {
  local name source destination identity expected_identity before after prefix=""
  local failed=0
  [[ "${INTEROP_CAPTURE_PHASE}" == initial ]] || prefix="${INTEROP_CAPTURE_PHASE}-"
  [[ -n "${INTEROP_CAPTURE_STAGING_DIR}" ]] || {
    echo "capture staging directory is not initialized" >&2
    return 1
  }
  expected_identity="$(getent passwd tcpdump | cut -d: -f3,4)"
  for name in pod-net1-peer worker-underlay frr-underlay frr-vxlan remote-workload-peer; do
    source="${INTEROP_CAPTURE_STAGING_DIR}/${prefix}${name}.pcap"
    destination="${EVIDENCE_DIR}/${prefix}${name}.pcap"
    if [[ ! -s "${source}" || -L "${source}" ]]; then
      echo "missing or unsafe staged packet capture: ${source}" >&2
      failed=1
      continue
    fi
    identity="$(stat -c '%u:%g' "${source}")" || { failed=1; continue; }
    if [[ "${identity}" != "${expected_identity}" || ! -r "${source}" ]]; then
      echo "staged capture has unexpected owner or is unreadable: ${source} owner=${identity}" >&2
      failed=1
      continue
    fi
    before="$(stat -c '%s' "${source}")" || { failed=1; continue; }
    if (( before > 69656 )) || [[ -e "${destination}" || -L "${destination}" ]]; then
      echo "capture exceeds its bound or destination already exists: ${destination}" >&2
      failed=1
      continue
    fi
    if ! install -m 0600 -- "${source}" "${destination}"; then
      echo "failed to publish staged packet capture: ${destination}" >&2
      failed=1
      continue
    fi
    after="$(stat -c '%u:%g:%a:%s' "${destination}")" || { failed=1; continue; }
    if [[ "${after}" != "$(id -u):$(id -g):600:${before}" ]]; then
      echo "published capture ownership or permissions are unexpected: ${destination}" >&2
      failed=1
      continue
    fi
    printf '%s capture-published name=%s source-owner=%s destination-owner-mode-size=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${name}" "${identity}" "${after}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log" || failed=1
  done
  (( failed == 0 ))
}

interop_capture_decode_outputs() {
  local phase="${1:-initial}" prefix="" name pcap failed=0
  [[ "${phase}" == initial ]] || prefix="${phase}-"
  for name in pod-net1-peer worker-underlay frr-underlay frr-vxlan remote-workload-peer; do
    pcap="${EVIDENCE_DIR}/${prefix}${name}.pcap"
    if [[ ! -s "${pcap}" ]]; then
      echo "missing or empty capture: ${pcap}" >&2
      failed=1
      continue
    fi
    if (( $(stat -c %s "${pcap}") > 69656 )); then
      echo "capture exceeded its bounded packet/file size: ${pcap}" >&2
      failed=1
      continue
    fi
    if ! timeout --kill-after=2s 10s tcpdump -nn -vvv -e -r "${pcap}" 2>&1 \
      | tee "${EVIDENCE_DIR}/${prefix}${name}-decoded.txt" >/dev/null; then
      echo "pcap decode failed: ${pcap}" >&2
      failed=1
    fi
  done
  return "${failed}"
}

interop_capture_start_one() {
  local name="$1" namespace_pid="$2" interface="$3" filter="$4"
  local pcap="${INTEROP_CAPTURE_STAGING_DIR}/${name}.pcap"
  local log="${EVIDENCE_DIR}/${name}-capture.log"
  local unit="cni-d22d-${KIND_CLUSTER}-${name}.service"
  local pid deadline state cgroup cgroup_pids
  [[ -n "${INTEROP_CAPTURE_STAGING_DIR}" && ! -e "${pcap}" && ! -L "${pcap}" ]] || {
    echo "capture output path is not a fresh staged file: ${pcap}" >&2
    return 1
  }
  if [[ -e "${log}" || -L "${log}" ]]; then
    echo "refusing pre-existing capture service log: ${log}" >&2
    return 1
  fi
  (umask 077; : >"${log}") || return 1
  chmod 0600 "${log}" || return 1
  if timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
    --property=LoadState --value 2>/dev/null | grep -Fxqv 'not-found'; then
    echo "refusing pre-existing capture service: ${unit}" >&2
    return 1
  fi
  INTEROP_CAPTURE_UNITS+=("${unit}")
  INTEROP_CAPTURE_NAMES+=("${name}")
  INTEROP_CAPTURE_MAIN_PIDS+=("")
  INTEROP_CAPTURE_CGROUPS+=("")
  printf '%s start unit=%s interface=%s namespace_pid=%s service_uid=0 capture_user=tcpdump output=%q publish=%q command=tcpdump -Z tcpdump -n -i %q -s 256 -U -w %q %q\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${interface}" \
    "${namespace_pid}" "${pcap}" "${EVIDENCE_DIR}/${name}.pcap" \
    "${interface}" "${pcap}" "${filter}" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  if ! timeout --kill-after=2s 10s sudo -n systemd-run --system --quiet \
    --unit="${unit%.service}" \
    --property=User=root \
    --property=Group=root \
    --property=UMask=0022 \
    --property=RuntimeMaxSec=30s \
    --property=LimitFSIZE=69656 \
    --property=TimeoutStopSec=10s \
    --property=KillSignal=SIGINT \
    --property=KillMode=control-group \
    --property="StandardOutput=append:${log}" \
    --property="StandardError=append:${log}" \
    /usr/bin/nsenter --no-fork -t "${namespace_pid}" -n /usr/bin/tcpdump \
      -Z tcpdump -n -i "${interface}" -s 256 -U -w "${pcap}" "${filter}"; then
    cat "${log}" >&2 || true
    printf '%s launch-failed unit=%s name=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
    return 1
  fi
  printf '%s launched unit=%s name=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  deadline=$((SECONDS+10))
  while (( SECONDS < deadline )); do
    state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=ActiveState --value 2>/dev/null || true)"
    if [[ "${state}" == active ]] && grep -Fq "listening on ${interface}" "${log}"; then
      pid="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
        --property=MainPID --value)"
      cgroup="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
        --property=ControlGroup --value)"
      [[ "${pid}" =~ ^[0-9]+$ && "${pid}" != 0 && "${cgroup}" == /* ]]
      cgroup_pids="$(timeout --kill-after=1s 5s sudo -n cat "/sys/fs/cgroup${cgroup}/cgroup.procs" \
        | sort -n | tr '\n' ',')"
      [[ -n "${cgroup_pids}" ]]
      INTEROP_CAPTURE_MAIN_PIDS[-1]="${pid}"
      INTEROP_CAPTURE_CGROUPS[-1]="${cgroup}"
      printf '%s ready unit=%s name=%s main_pid=%s cgroup=%s cgroup_pids=%s interface=%s\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" "${pid}" \
        "${cgroup}" "${cgroup_pids}" "${interface}" \
        >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
      return 0
    fi
    if [[ "${state}" == failed || "${state}" == inactive || "${state}" == unknown ]]; then
      cat "${log}" >&2 || true
      timeout --kill-after=1s 5s sudo -n journalctl --no-pager --unit="${unit}" \
        --output=cat | tee "${EVIDENCE_DIR}/${name}-capture-service.log" >&2 || true
      printf '%s startup-failed unit=%s name=%s state=%s\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" "${state}" \
        >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
      return 1
    fi
    sleep 0.1
  done
  cat "${log}" >&2
  timeout --kill-after=1s 5s sudo -n journalctl --no-pager --unit="${unit}" \
    --output=cat | tee "${EVIDENCE_DIR}/${name}-capture-service.log" >&2 || true
  printf '%s readiness-timeout unit=%s name=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  return 1
}

interop_capture_assert_active() {
  local phase="$1" index unit name expected_pid state actual_pid
  local failed=0
  for index in "${!INTEROP_CAPTURE_UNITS[@]}"; do
    unit="${INTEROP_CAPTURE_UNITS[index]}"
    name="${INTEROP_CAPTURE_NAMES[index]}"
    expected_pid="${INTEROP_CAPTURE_MAIN_PIDS[index]}"
    state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=ActiveState --value 2>/dev/null || echo unknown)"
    actual_pid="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=MainPID --value 2>/dev/null || echo unknown)"
    printf '%s active-check phase=%s unit=%s name=%s state=%s expected_pid=%s actual_pid=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${phase}" "${unit}" "${name}" \
      "${state}" "${expected_pid}" "${actual_pid}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
    if [[ "${state}" != active || "${actual_pid}" != "${expected_pid}" ]]; then
      echo "capture not active at ${phase}: unit=${unit} name=${name} state=${state} pid=${actual_pid}" >&2
      failed=1
    fi
  done
  [[ "${#INTEROP_CAPTURE_UNITS[@]}" == 5 ]] || {
    echo "expected five active capture processes, found ${#INTEROP_CAPTURE_UNITS[@]}" >&2
    failed=1
  }
  return "${failed}"
}

interop_capture_start() {
  local phase="${1:-initial}" route_file="${EVIDENCE_DIR}/remote-status-initial-packet-path.json"
  local underlay_filter prefix="" manifest
  [[ "${phase}" == initial ]] || prefix="${phase}-"
  INTEROP_CAPTURE_PHASE="${phase}"
  INTEROP_REMOTE_ROUTE_VTEP="$(python3 - "${route_file}" "${REMOTE_MAC}" "${REMOTE_IP}" "${VNI}" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
snapshot=status.get("Snapshot") or {}
matches=[x for x in snapshot.get("Endpoints",[]) if x.get("MAC","").lower()==sys.argv[2].lower()
         and x.get("IP")==sys.argv[3] and x.get("VNI")==int(sys.argv[4])]
assert snapshot.get("Authoritative") is True and len(matches)==1,(snapshot,matches)
print(matches[0]["RemoteVTEP"])
PY
)"
  [[ "${INTEROP_REMOTE_ROUTE_VTEP}" == "${FRR_VTEP}" ]]
  [[ "${INTEROP_REMOTE_ROUTE_VTEP}" != "${FRR_PEER}" ]]
  python3 - "${route_file}" "${EVIDENCE_DIR}/evpn-route-next-hop.json" \
    "${REMOTE_MAC}" "${REMOTE_IP}" "${VNI}" "${INTEROP_REMOTE_ROUTE_VTEP}" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
snapshot=status.get("Snapshot") or {}
endpoint=next(x for x in snapshot.get("Endpoints",[]) if x.get("MAC","").lower()==sys.argv[3].lower()
             and x.get("IP")==sys.argv[4] and x.get("VNI")==int(sys.argv[5]))
json.dump({"source":"worker /run/evpn-node-agent/remote-status.json",
           "endpoint":endpoint,"remoteVtep":sys.argv[6]},open(sys.argv[2],"w"),indent=2)
PY
  interop_capture_ovs_snapshot before "${phase}"
  if [[ "${phase}" == initial ]]; then
    manifest="${EVIDENCE_DIR}/packet-path-manifest.json"
  else
    manifest="${EVIDENCE_DIR}/packet-path-${phase}-manifest.json"
  fi
  cat >"${manifest}" <<EOF
{
  "localIp": "${LOCAL_IP}",
  "localMac": "${LOCAL_MAC}",
  "remoteIp": "${REMOTE_IP}",
  "remoteMac": "${REMOTE_MAC}",
  "localVtep": "${LOCAL_VTEP}",
  "remoteVtep": "${INTEROP_REMOTE_ROUTE_VTEP}",
  "vni": ${VNI},
  "udpPort": 4789,
  "markers": {
    "pod-to-frr": "d22b3b3d00010203"
  },
  "captures": {
    "pod-facing": {"path": "${EVIDENCE_DIR}/${prefix}pod-net1-peer.pcap", "kind": "inner"},
    "worker-underlay": {"path": "${EVIDENCE_DIR}/${prefix}worker-underlay.pcap", "kind": "vxlan"},
    "frr-underlay": {"path": "${EVIDENCE_DIR}/${prefix}frr-underlay.pcap", "kind": "vxlan"},
    "frr-vxlan": {"path": "${EVIDENCE_DIR}/${prefix}frr-vxlan.pcap", "kind": "inner"},
    "remote-workload": {"path": "${EVIDENCE_DIR}/${prefix}remote-workload-peer.pcap", "kind": "inner"}
  },
  "ovs": {
    "tunnelConfig": "${EVIDENCE_DIR}/${prefix}ovs-tunnel-before.txt",
    "ofport": "${INTEROP_OVS_OFPORT}",
    "portStatsBefore": "${EVIDENCE_DIR}/${prefix}ovs-ports-before.txt",
    "portStatsAfter": "${EVIDENCE_DIR}/${prefix}ovs-ports-after.txt",
    "flowsBefore": "${EVIDENCE_DIR}/${prefix}ovs-flows-before.txt",
    "flowsAfter": "${EVIDENCE_DIR}/${prefix}ovs-flows-after.txt"
  }
}
EOF
  printf 'remote-vtep-source=EVPN-imported-Type-2-next-hop remote-vtep=%s peer=%s\n' \
    "${INTEROP_REMOTE_ROUTE_VTEP}" "${FRR_PEER}" >"${EVIDENCE_DIR}/${prefix}capture-topology.txt"
  printf 'pod-facing-interface=%s worker-underlay-interface=%s frr-underlay-interface=%s\n' \
    "${INTEROP_POD_VETH}" "${INTEROP_WORKER_IFACE}" "${INTEROP_FRR_IFACE}" \
    >>"${EVIDENCE_DIR}/${prefix}capture-topology.txt"
  printf 'frr-vxlan-interface=%s frr-workload-interface=%s vni=%s vxlan-port=4789 sync-port=60100\n' \
    "${INTEROP_FRR_VXLAN_IFACE}" "${INTEROP_FRR_HOST_IFACE}" "${VNI}" \
    >>"${EVIDENCE_DIR}/${prefix}capture-topology.txt"
  [[ -e "${EVIDENCE_DIR}/capture-process-lifecycle.log" ]] ||
    : >"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  interop_capture_create_staging_dir
  underlay_filter="arp or udp port 4789 or udp port 60100 or udp port 6081 or (ip and host ${LOCAL_IP} and host ${REMOTE_IP})"
  INTEROP_CAPTURE_STOPPED=0
  interop_capture_start_one "${prefix}pod-net1-peer" "${INTEROP_WORKER_PID}" "${INTEROP_POD_VETH}" 'arp or icmp'
  interop_capture_start_one "${prefix}worker-underlay" "${INTEROP_WORKER_PID}" "${INTEROP_WORKER_IFACE}" "${underlay_filter}"
  interop_capture_start_one "${prefix}frr-underlay" "${INTEROP_FRR_PID}" "${INTEROP_FRR_IFACE}" "${underlay_filter}"
  interop_capture_start_one "${prefix}frr-vxlan" "${INTEROP_FRR_PID}" "${INTEROP_FRR_VXLAN_IFACE}" 'arp or icmp'
  interop_capture_start_one "${prefix}remote-workload-peer" "${INTEROP_FRR_PID}" "${INTEROP_FRR_HOST_IFACE}" 'arp or icmp'
  interop_capture_assert_active pre-stimulus
  INTEROP_CAPTURE_STARTUP_STATUS=PASS
  echo "five packet captures ready: pod=${INTEROP_POD_VETH} worker=${INTEROP_WORKER_IFACE} frr=${INTEROP_FRR_IFACE} vxlan=${INTEROP_FRR_VXLAN_IFACE} workload=${INTEROP_FRR_HOST_IFACE}"
}

interop_capture_stop() {
  local index unit name rc failed=0 shutdown_failed=0 verification_failed=0
  local pcap load_state pid cgroup cgroup_pids deadline exit_status service_result
  local drain_started_ns drain_completed_ns drain_seconds
  if [[ "${#INTEROP_CAPTURE_UNITS[@]}" == 0 ]]; then
    INTEROP_CAPTURE_STOPPED=1
    if ! interop_capture_discard_staging_dir; then
      INTEROP_CAPTURE_FAILURE=1
      INTEROP_CAPTURE_SHUTDOWN_FAILURE=1
      INTEROP_CAPTURE_SHUTDOWN_STATUS=FAIL
      return 1
    fi
    return 0
  fi
  if ! interop_capture_assert_active before-drain; then
    failed=1
    shutdown_failed=1
  fi
  drain_started_ns="$(date +%s%N)"
  printf '%s drain-started seconds=2\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  sleep 2
  drain_completed_ns="$(date +%s%N)"
  drain_seconds="$(python3 - "${drain_started_ns}" "${drain_completed_ns}" <<'PY'
import sys
print("%.3f" % ((int(sys.argv[2]) - int(sys.argv[1])) / 1_000_000_000))
PY
)"
  printf '%s drain-completed seconds=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${drain_seconds}" \
    >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  python3 - "${EVIDENCE_DIR}/capture-drain.json" "${drain_started_ns}" \
    "${drain_completed_ns}" "${drain_seconds}" <<'PY'
import json,sys
json.dump({"startedAtNs":int(sys.argv[2]),"completedAtNs":int(sys.argv[3]),
           "durationSeconds":float(sys.argv[4]),"configuredMinimumSeconds":2},
          open(sys.argv[1],"w"),indent=2)
PY
  if ! interop_capture_assert_active after-drain; then
    failed=1
    shutdown_failed=1
  fi
  for index in "${!INTEROP_CAPTURE_UNITS[@]}"; do
    unit="${INTEROP_CAPTURE_UNITS[index]}"
    name="${INTEROP_CAPTURE_NAMES[index]}"
    cgroup="${INTEROP_CAPTURE_CGROUPS[index]}"
    load_state="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=LoadState --value 2>/dev/null || echo unknown)"
    if [[ -z "${cgroup}" && "${load_state}" != not-found ]]; then
      cgroup="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
        --property=ControlGroup --value 2>/dev/null || true)"
      INTEROP_CAPTURE_CGROUPS[index]="${cgroup}"
    fi
    pid="${INTEROP_CAPTURE_MAIN_PIDS[index]}"
    if [[ -z "${pid}" && "${load_state}" != not-found ]]; then
      pid="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
        --property=MainPID --value 2>/dev/null || true)"
      INTEROP_CAPTURE_MAIN_PIDS[index]="${pid}"
    fi
    rc=0
    if [[ "${load_state}" != not-found ]]; then
      if timeout --kill-after=2s 15s sudo -n systemctl stop "${unit}"; then
        rc=0
      else
        rc=$?
        failed=1
        shutdown_failed=1
      fi
    fi
    deadline=$((SECONDS+35))
    while [[ -n "${cgroup}" && -e "/sys/fs/cgroup${cgroup}/cgroup.procs" ]] &&
      (( SECONDS < deadline )); do
      cgroup_pids="$(timeout --kill-after=1s 5s sudo -n cat "/sys/fs/cgroup${cgroup}/cgroup.procs" \
        | sort -n | tr '\n' ',')"
      [[ -n "${cgroup_pids}" ]] || break
      sleep 0.2
    done
    if [[ -n "${cgroup}" && -e "/sys/fs/cgroup${cgroup}/cgroup.procs" ]]; then
      cgroup_pids="$(timeout --kill-after=1s 5s sudo -n cat "/sys/fs/cgroup${cgroup}/cgroup.procs" \
        | sort -n | tr '\n' ',')"
      if [[ -n "${cgroup_pids}" ]]; then
        echo "capture service still has processes: unit=${unit} pids=${cgroup_pids}" >&2
        failed=1
        shutdown_failed=1
      fi
    fi
    exit_status="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=ExecMainStatus --value 2>/dev/null || echo unknown)"
    service_result="$(timeout --kill-after=1s 5s sudo -n systemctl show "${unit}" \
      --property=Result --value 2>/dev/null || echo unknown)"
    printf '%s\n' "${exit_status}" >"${EVIDENCE_DIR}/${name}-capture-exit-status.txt"
    printf '%s\n' "${service_result}" >"${EVIDENCE_DIR}/${name}-capture-service-result.txt"
    if [[ "${exit_status}" != 0 || "${service_result}" != success ]]; then
      echo "capture process exit was not successful: unit=${unit} status=${exit_status} result=${service_result}" >&2
      failed=1
      shutdown_failed=1
    fi
    timeout --kill-after=1s 8s sudo -n journalctl --no-pager --unit="${unit}" \
      --output=cat >"${EVIDENCE_DIR}/${name}-capture-service.log" 2>&1 || true
    printf '%s stopped unit=%s name=%s main_pid=%s cgroup=%s stop_rc=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" \
      "${pid}" "${cgroup}" "${rc}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
    printf '%s process-exit unit=%s name=%s exit_status=%s result=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "${unit}" "${name}" \
      "${exit_status}" "${service_result}" \
      >>"${EVIDENCE_DIR}/capture-process-lifecycle.log"
  done
  if ! interop_capture_publish_outputs; then
    failed=1
    verification_failed=1
  fi
  INTEROP_CAPTURE_UNITS=()
  INTEROP_CAPTURE_NAMES=()
  INTEROP_CAPTURE_MAIN_PIDS=()
  INTEROP_CAPTURE_CGROUPS=()
  INTEROP_CAPTURE_STOPPED=1
  if [[ -n "${INTEROP_OVS_TUNNEL}" ]]; then
    if ! interop_capture_ovs_snapshot after "${INTEROP_CAPTURE_PHASE}"; then
      echo "failed to collect post-stimulus OVS snapshot" >&2
      failed=1
      verification_failed=1
    fi
  fi
  if ! interop_capture_decode_outputs "${INTEROP_CAPTURE_PHASE}"; then
    failed=1
    verification_failed=1
  fi
  local manifest="${EVIDENCE_DIR}/packet-path-manifest.json"
  local verification="${EVIDENCE_DIR}/packet-path-verification.json"
  if [[ "${INTEROP_CAPTURE_PHASE}" != initial ]]; then
    manifest="${EVIDENCE_DIR}/packet-path-${INTEROP_CAPTURE_PHASE}-manifest.json"
    verification="${EVIDENCE_DIR}/packet-path-${INTEROP_CAPTURE_PHASE}-verification.json"
  fi
  if [[ -s "${manifest}" ]]; then
    if ! timeout --kill-after=2s 15s python3 "${ROOT}/tools/lib/cni-vxlan-pcap-verify.py" \
      --manifest "${manifest}" \
      --output "${verification}"; then
      failed=1
      verification_failed=1
    fi
  else
    echo "packet-path manifest missing; verification incomplete" >&2
    failed=1
    verification_failed=1
  fi
  if ! interop_capture_discard_staging_dir; then
    failed=1
    shutdown_failed=1
  fi
  INTEROP_CAPTURE_FAILURE="${failed}"
  INTEROP_CAPTURE_SHUTDOWN_FAILURE="${shutdown_failed}"
  INTEROP_PACKET_VERIFICATION_FAILURE="${verification_failed}"
  INTEROP_CAPTURE_SHUTDOWN_STATUS=PASS
  INTEROP_PACKET_VERIFICATION_STATUS=PASS
  [[ "${shutdown_failed}" == 0 ]] || INTEROP_CAPTURE_SHUTDOWN_STATUS=FAIL
  [[ "${verification_failed}" == 0 ]] || INTEROP_PACKET_VERIFICATION_STATUS=PARTIAL
  [[ "${shutdown_failed}" == 0 ]] && echo "capture-shutdown=PASS" || echo "capture-shutdown=FAIL" >&2
  [[ "${verification_failed}" == 0 ]] && echo "packet-verification=PASS" || echo "packet-verification=FAIL" >&2
  return "${failed}"
}

interop_write_status() {
  python3 - "${EVIDENCE_DIR}/interop-status.json" "$1" "$2" \
    "${INTEROP_CONTROL_PLANE_STATUS}" "${INTEROP_FORWARDING_STATE_STATUS}" \
    "${INTEROP_CAPTURE_STARTUP_STATUS}" "${INTEROP_FORWARD_ICMP_STATUS}" \
    "${INTEROP_REVERSE_ICMP_STATUS}" "${INTEROP_PACKET_VERIFICATION_STATUS}" \
    "${INTEROP_CAPTURE_SHUTDOWN_STATUS}" "${3:-$1}" \
    "${CLUSTER_CREATION_STATUS:-NOT_STARTED}" "${CLUSTER_CLEANUP_STATUS:-SKIPPED}" \
    "${CREATED:-0}" "${KIND_CLUSTER}" "${CLUSTER_CREATE_ATTEMPTED:-0}" \
    "${INTEROP_CAPTURE_PREFLIGHT_STATUS}" "${INTEROP_LIFECYCLE_STATUS}" \
    "${INTEROP_SHARED_VTEP_STATUS}" "${INTEROP_FAILURE_CLASSIFICATION}" \
    "${INTEROP_FAILURE_GATE}" "${INTEROP_TOPOLOGY_SCOPE}" <<'PY'
import json,sys
keys=("control_plane_status","forwarding_state_status","capture_startup_status",
      "forward_icmp_status","reverse_icmp_status","packet_verification_status",
      "capture_shutdown_status","lifecycle_status","shared_vtep_lifecycle_status")
status=dict(zip(keys,sys.argv[4:11] + [sys.argv[18],sys.argv[19]]))
status["capture_preflight_status"]=sys.argv[17]
status["resource_cleanup_status"]="PASS" if sys.argv[3]=="0" else "FAIL"
status["failure_classification"]=sys.argv[20]
status["failure_gate"]=sys.argv[21]
status["topology_scope"]=sys.argv[22]
required=("control_plane_status","forwarding_state_status","capture_startup_status",
          "forward_icmp_status","reverse_icmp_status","packet_verification_status",
          "capture_shutdown_status","capture_preflight_status","lifecycle_status")
all_required_pass=all(status[key]=="PASS" for key in required)
if sys.argv[2]=="0" and sys.argv[3]=="0" and all_required_pass:
    status["overall_status"]=(
        "PASS" if status["shared_vtep_lifecycle_status"]=="PASS" else "PARTIAL"
    )
else:
    status["overall_status"]="FAIL"
status["exit_code"]=int(sys.argv[2])
status["original_exit_code"]=int(sys.argv[11])
status["cleanup_exit_code"]=int(sys.argv[3])
status["cluster_creation_status"]=sys.argv[12]
status["cluster_cleanup_status"]=sys.argv[13]
status["resources_created"]=sys.argv[14]=="1"
if not status["resources_created"] and status["cluster_cleanup_status"]=="FAIL":
    status["resources_created"]=None  # Residual nodes may exist after failed creation.
status["run_id"]=sys.argv[15]
status["cluster_creation_attempted"]=sys.argv[16]=="1"
if status["original_exit_code"] and status["control_plane_status"]=="NOT_RUN" and \
    status["failure_classification"]!="HARNESS FAILURE":
    status["overall_status"]="BLOCKED"
json.dump(status,open(sys.argv[1],"w"),indent=2)
PY
}

interop_ping_command() {
  local direction="$1"
  if [[ "${direction}" == forward ]]; then
    INTEROP_PING_COMMAND=(timeout --kill-after=2s 10s kubectl --context "kind-${KIND_CLUSTER}"
      -n cni-test exec pod-a -- ping -n -I net1 -c 3 -W 2 -s 56 -p d22b3b3d00010203 "${REMOTE_IP}")
  elif [[ "${direction}" == reverse ]]; then
    INTEROP_PING_COMMAND=(timeout --kill-after=2s 10s docker exec "${FRR_CONTAINER}"
      ip netns exec evpnwork ping -n -I frr-work0 -c 3 -W 2 -s 56 "${LOCAL_IP}")
  else
    echo "unknown interop ping direction: ${direction}" >&2
    return 1
  fi
}

interop_assert_forwarding_state() {
  interop_exec_worker bridge -j fdb show dev "${VXLAN_DEVICE}" \
    >"${EVIDENCE_DIR}/worker-fdb-before-ping.json"
  interop_exec_worker ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}" \
    >"${EVIDENCE_DIR}/worker-neigh-before-ping.json"
  interop_exec_frr bridge -j fdb show dev "${INTEROP_FRR_VXLAN_IFACE}" \
    >"${EVIDENCE_DIR}/frr-fdb-before-ping.json"
  interop_exec_frr ip -4 -j neigh show nud all dev br1000 \
    >"${EVIDENCE_DIR}/frr-neigh-before-ping.json"
  python3 - "${EVIDENCE_DIR}/worker-fdb-before-ping.json" \
    "${EVIDENCE_DIR}/worker-neigh-before-ping.json" \
    "${EVIDENCE_DIR}/frr-fdb-before-ping.json" \
    "${EVIDENCE_DIR}/frr-neigh-before-ping.json" \
    "${REMOTE_MAC}" "${REMOTE_IP}" "${INTEROP_REMOTE_ROUTE_VTEP:-${FRR_VTEP}}" \
    "${LOCAL_MAC}" "${LOCAL_IP}" "${LOCAL_VTEP}" "${VXLAN_DEVICE}" \
    "${INTEROP_FRR_VXLAN_IFACE}" "${BRIDGE_DEVICE}" <<'PY'
import json,sys
worker_fdb,worker_neigh,frr_fdb,frr_neigh=(
    json.load(open(path)) for path in sys.argv[1:5]
)
remote_mac,remote_ip,remote_vtep,local_mac,local_ip,local_vtep,worker_vxlan,frr_vxlan,worker_bridge=sys.argv[5:]
def on_selected_device(entry,device):
    return entry.get("dev",device)==device
def has_flags(entry,*required):
    return set(required).issubset(entry.get("flags",[]))
def neighbor_state(entry):
    state=entry.get("state",[])
    return "NOARP" in (state if isinstance(state,list) else [state])
assert any(x.get("mac","").lower()==remote_mac.lower() and x.get("dst")==remote_vtep
           and on_selected_device(x,worker_vxlan) and x.get("state")=="permanent"
           and has_flags(x,"self","extern_learn") for x in worker_fdb),worker_fdb
assert any(x.get("dst")==remote_ip and x.get("lladdr","").lower()==remote_mac.lower()
           and on_selected_device(x,worker_bridge) and neighbor_state(x) for x in worker_neigh),worker_neigh
assert any(x.get("mac","").lower()==local_mac.lower() and x.get("dst")==local_vtep
           and on_selected_device(x,frr_vxlan) and x.get("state") in ("","permanent")
           and has_flags(x,"self","extern_learn") for x in frr_fdb),frr_fdb
assert any(x.get("dst")==local_ip and x.get("lladdr","").lower()==local_mac.lower()
           and on_selected_device(x,"br1000") and neighbor_state(x) for x in frr_neigh),frr_neigh
print("remote FDB and neighbor programming on worker and FRR=PASS")
PY
}

interop_remove_owned() {
  local kind="$1" name="$2" id_file="$3" metadata id
  [[ -n "${name}" ]] || { echo "empty interop cleanup target" >&2; return 1; }
  if metadata="$(docker "${kind}" inspect "${name}" 2>&1)"; then
    [[ -s "${id_file}" ]] || { echo "missing ownership record: ${name}" >&2; return 1; }
    id="$(cat "${id_file}")"
    if ! python3 -c 'import json,sys
i=json.load(sys.stdin)[0]; kind,name,id,run=sys.argv[1:]
labels=i.get("Config",{}).get("Labels",{}) if kind=="container" else i.get("Labels",{})
if not id or i["Id"]!=id or i["Name"].lstrip("/")!=name or (labels or {}).get("evpn4ovn.test-run")!=run:
    sys.exit("resource ID, name or run label mismatch")
if kind=="network" and i.get("Containers"):
    sys.exit("network still has attached containers")
' "${kind}" "${name}" "${id}" "${KIND_CLUSTER}" <<<"${metadata}"; then
      echo "interop cleanup requires ownership review: ${name}" >&2
      return 1
    fi
    if [[ "${kind}" == container ]]; then
      docker rm -f "${id}" >/dev/null || return 1
    else
      docker network rm "${id}" >/dev/null || return 1
    fi
  elif [[ "${kind}" == container && "${metadata}" == *"No such container: ${name}" ]] ||
       [[ "${kind}" == network && "${metadata}" == *"Error response from daemon: network ${name} not found" ]]; then
    return 0
  else
    echo "interop cleanup inspection failed: ${metadata}" >&2
    return 1
  fi
}

interop_cleanup() {
  local rc=0
  interop_capture_stop || {
    [[ "${INTEROP_CAPTURE_SHUTDOWN_FAILURE}" == 0 ]] || rc=1
  }
  if [[ "${INTEROP_CREATED}" != 1 ]]; then return "${rc}"; fi
  e2e_lab_down || rc=1
  kind get clusters | sort > "${EVIDENCE_DIR}/clusters-after.txt" || rc=1
  docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort > "${EVIDENCE_DIR}/docker-containers-after.txt" || rc=1
  docker network ls --format '{{.ID}} {{.Name}} {{.Driver}}' | sort > "${EVIDENCE_DIR}/docker-networks-after.txt" || rc=1
  diff -u "${EVIDENCE_DIR}/clusters-before.txt" "${EVIDENCE_DIR}/clusters-after.txt" || rc=1
  diff -u "${EVIDENCE_DIR}/docker-containers-before.txt" "${EVIDENCE_DIR}/docker-containers-after.txt" || rc=1
  diff -u "${EVIDENCE_DIR}/docker-networks-before.txt" "${EVIDENCE_DIR}/docker-networks-after.txt" || rc=1
  [[ "${rc}" == 0 ]] && echo "kind-docker-inventory-restored=PASS"
  return "${rc}"
}

interop_wait_import_count() {
  local expected="$1" label="$2" deadline=$((SECONDS+90)) status_file="${EVIDENCE_DIR}/remote-status-${2}.json"
  while (( SECONDS < deadline )); do
    if docker exec "${WORKER}" cat /run/evpn-node-agent/remote-status.json >"${status_file}" 2>/dev/null &&
      python3 - "${status_file}" "${expected}" "${REMOTE_MAC}" "${REMOTE_IP}" "${FRR_VTEP}" "${VNI}" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
snapshot=s.get("Snapshot") or {}
expected=int(sys.argv[2])
assert not s.get("Error"),s.get("Error")
assert snapshot.get("Authoritative") is True,snapshot
assert len(snapshot.get("Endpoints") or [])==expected,snapshot
assert len(s.get("Receipts") or [])==expected,s.get("Receipts")
if expected:
    mac,ip,vtep,vni=sys.argv[3:]
    endpoint=snapshot["Endpoints"][0]
    receipt=s["Receipts"][0]
    assert (endpoint.get("MAC"),endpoint.get("IP"),endpoint.get("RemoteVTEP"),endpoint.get("VNI")) == \
           (mac,ip,vtep,int(vni)),endpoint
    assert (receipt.get("Endpoint") or {}).get("RemoteVTEP")==vtep,receipt
PY
    then
      echo "remote-import-${label}-count=${expected}=PASS"
      return 0
    fi
    sleep 1
  done
  cat "${status_file}" 2>/dev/null || true
  echo "remote importer did not confirm ${expected} endpoints for ${label}" >&2
  return 1
}

interop_capture_diagnostic() {
  local name="$1" context="$2" expectation="$3"
  shift 3
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" run \
    --command-id "${name}" \
    --context "${context}" \
    --expectation "${expectation}" \
    --stdout "${EVIDENCE_DIR}/${name}" \
    --stderr "${EVIDENCE_DIR}/${name}.stderr" \
    --metadata "${EVIDENCE_DIR}/${name}.command.json" \
    --exit-status "${EVIDENCE_DIR}/${name}.exit-status" \
    -- "$@"
}

interop_collect_replacement_binding_diagnostics() {
  local suffix="$1" context="host kubectl -> disposable kind context"
  local kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  interop_capture_diagnostic "pods-${suffix}.json" \
    "${context}" json "${kubectl[@]}" -n cni-test get pods -o json || return 1
  if [[ "${suffix}" != deleted && "${suffix}" != cleanup ]]; then
    interop_capture_diagnostic "replacement-pod-${suffix}.txt" \
      "${context}" required "${kubectl[@]}" -n cni-test describe pod pod-a || return 1
  fi
  interop_capture_diagnostic "ovn-switches-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,ports,other_config list Logical_Switch || return 1
  interop_capture_diagnostic "ovn-ports-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,addresses,dynamic_addresses,type,options,external_ids,enabled,parent_name,tag list Logical_Switch_Port || return 1
  interop_capture_diagnostic "ovn-bindings-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,logical_port,datapath,type,chassis,up,mac,options list Port_Binding || return 1
  interop_capture_diagnostic "ovn-datapaths-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,external_ids list Datapath_Binding || return 1
  interop_capture_diagnostic "ovn-chassis-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,name,hostname list Chassis || return 1
  interop_capture_diagnostic "ovn-chassis-private-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovn-sbctl list Chassis_Private || return 1
  interop_capture_diagnostic "ovn-nb-global-diagnostic-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovn-nbctl list NB_Global || return 1
  interop_capture_diagnostic "ovn-sb-global-diagnostic-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovn-sbctl list SB_Global || return 1
  interop_capture_diagnostic "ovs-interfaces-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,external_ids,ofport,error list Interface || return 1
  interop_capture_diagnostic "ovs-ports-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
  interop_capture_diagnostic "ovs-bridges-${suffix}.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
  interop_capture_diagnostic "ovs-external-ids-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovs-vsctl get Open_vSwitch . external_ids || return 1
  interop_capture_diagnostic "ovs-version-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovs-vsctl --version || return 1
  interop_capture_diagnostic "ovn-controller-status-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" optional \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovs-appctl -t ovn-controller connection-status || return 1
  interop_capture_diagnostic "ovn-processes-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ps -ef || return 1
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-processes \
    --input "${EVIDENCE_DIR}/ovn-processes-${suffix}.txt" \
    --process ovn-controller --process ovn-northd \
    >"${EVIDENCE_DIR}/ovn-process-health-${suffix}.json" || return 1
  interop_capture_diagnostic "ovn-controller-log-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- tail -n 300 /var/log/ovn/ovn-controller.log || return 1
  interop_capture_diagnostic "ovn-northd-log-${suffix}.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- tail -n 300 /var/log/ovn/ovn-northd.log || return 1
  interop_capture_diagnostic "ovn-pod-log-${suffix}.txt" \
    "${context}" optional "${kubectl[@]}" -n cni-test logs "${OVN_POD}" -c ovn --since=20m || return 1
  if [[ -n "${AGENT_POD:-}" ]]; then
    interop_capture_diagnostic "node-agent-log-${suffix}.txt" \
      "${context}" optional "${kubectl[@]}" -n cni-test logs "${AGENT_POD}" --since=20m || return 1
  fi
  interop_capture_diagnostic "worker-links-${suffix}.json" \
    "docker exec kind worker ${WORKER}" json timeout --kill-after=2s 15s docker exec "${WORKER}" ip -d -j link show || return 1
  interop_capture_diagnostic "worker-addresses-${suffix}.json" \
    "docker exec kind worker ${WORKER}" json timeout --kill-after=2s 15s docker exec "${WORKER}" ip -4 -j addr show || return 1
  interop_capture_diagnostic "export-status-${suffix}.json" \
    "docker exec kind worker ${WORKER}" json timeout --kill-after=2s 15s docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json || return 1
  interop_capture_diagnostic "export-config-${suffix}.json" \
    "docker exec kind worker ${WORKER}" json timeout --kill-after=2s 15s docker exec "${WORKER}" cat /run/evpn-node-agent/export-config.json || return 1
}

interop_diagnostic_preflight() {
  local context switch_result switch_uuid
  local kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  interop_capture_diagnostic diagnostic-control-nb-switch.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,ports list Logical_Switch ls24b || return 1
  switch_result="$(python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-control \
    --input "${EVIDENCE_DIR}/diagnostic-control-nb-switch.json" \
    --name-column name --name ls24b --uuid-column _uuid)" || return 1
  switch_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${switch_result}")" || return 1
  [[ -n "${switch_uuid}" ]] || return 1

  interop_capture_diagnostic diagnostic-control-sb-datapaths.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,external_ids list Datapath_Binding || return 1
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-control \
    --input "${EVIDENCE_DIR}/diagnostic-control-sb-datapaths.json" \
    --map-column external_ids --map-key logical-switch --map-value "${switch_uuid}" \
    --uuid-column _uuid >"${EVIDENCE_DIR}/diagnostic-control-sb-datapath.json" || return 1

  interop_capture_diagnostic diagnostic-control-chassis.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,name,hostname list Chassis || return 1
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-control \
    --input "${EVIDENCE_DIR}/diagnostic-control-chassis.json" \
    --name-column name --name "${LOCAL_CHASSIS}" --uuid-column _uuid \
    >"${EVIDENCE_DIR}/diagnostic-control-chassis-row.json" || return 1

  interop_capture_diagnostic diagnostic-control-ovs-bridges.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-control \
    --input "${EVIDENCE_DIR}/diagnostic-control-ovs-bridges.json" \
    --name-column name --name br-int --uuid-column _uuid \
    >"${EVIDENCE_DIR}/diagnostic-control-ovs-bridge-row.json" || return 1
  printf 'PASS\n' >"${EVIDENCE_DIR}/diagnostic-preflight.status"
}

interop_wait_replacement_binding() {
  local uid="$1" deadline logical_port switch_uuid expected_datapath local_chassis binding_json state
  local switch_result lsp_result chassis_result datapath_result iface owner workload_uid
  local context sample=0 sample_id sample_file rc classification
  local timeline="${EVIDENCE_DIR}/replacement-binding-timeline.jsonl"
  local probe="${ROOT}/tools/lib/cni_ovn_binding.py"
  local diagnostics="${ROOT}/tools/lib/cni_ovn_diagnostics.py"
  local suffix="replacement-binding-timeout"
  local -a kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")

  [[ ! -e "${timeline}" ]] || { echo "refusing existing binding timeline: ${timeline}" >&2; return 1; }
  : >"${timeline}" || return 1
  context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  if interop_capture_diagnostic ovn-nb-sb-sync.txt "${context}" status \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- ovn-nbctl --wait=sb sync; then
    :
  else
    rc=$?
    classification="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["classification"])' \
      "${EVIDENCE_DIR}/ovn-nb-sb-sync.txt.command.json")" || return 1
    if [[ "${classification}" != COMMAND_TIMEOUT ]]; then
      echo "NB/SB sync precheck failed (${classification}, status ${rc}); exact database queries will still determine binding state" >&2
    else
      echo "NB/SB sync precheck timed out; exact database queries will still determine binding state" >&2
    fi
  fi

  if [[ ! -e "${EVIDENCE_DIR}/diagnostic-preflight.status" ]]; then
    interop_diagnostic_preflight || return 1
  fi
  interop_capture_diagnostic binding-resolution-switches.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,ports list Logical_Switch || return 1
  interop_capture_diagnostic binding-resolution-lsps.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  interop_capture_diagnostic binding-resolution-chassis.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,name,hostname list Chassis || return 1
  interop_capture_diagnostic binding-resolution-datapaths.json "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,external_ids list Datapath_Binding || return 1

  switch_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/binding-resolution-switches.json" \
    --name-column name --name ls24b --uuid-column _uuid)" || return 1
  switch_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${switch_result}")" || return 1
  lsp_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/binding-resolution-lsps.json" \
    --map-column external_ids --map-key evpn.framework/workload-uid --map-value "${uid}" \
    --uuid-column _uuid)" || return 1
  logical_port="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["name"])' \
    <<<"${lsp_result}")" || return 1
  chassis_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/binding-resolution-chassis.json" \
    --name-column name --name "${LOCAL_CHASSIS}" --uuid-column _uuid)" || return 1
  local_chassis="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${chassis_result}")" || return 1
  datapath_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/binding-resolution-datapaths.json" \
    --map-column external_ids --map-key logical-switch --map-value "${switch_uuid}" \
    --uuid-column _uuid)" || return 1
  expected_datapath="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${datapath_result}")" || return 1
  [[ -n "${logical_port}" && -n "${switch_uuid}" && -n "${local_chassis}" && -n "${expected_datapath}" ]] || return 1

  deadline=$((SECONDS+90))
  while (( SECONDS < deadline )); do
    ((sample += 1))
    printf -v sample_id 'binding-poll-%03d.json' "${sample}"
    sample_file="binding-poll/${sample_id}"
    interop_capture_diagnostic "${sample_file}" "${context}" json \
      "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
      ovn-sbctl --format=json --columns=logical_port,datapath,type,chassis,up,mac,options \
      find Port_Binding "logical_port=${logical_port}" || return 1
    binding_json="$(cat "${EVIDENCE_DIR}/${sample_file}")" || return 1
    python3 "${diagnostics}" classify-object \
      --input "${EVIDENCE_DIR}/${sample_file}" --object-kind Port_Binding \
      --key-column logical_port --key-value "${logical_port}" \
      >"${EVIDENCE_DIR}/binding-poll/${sample_id}.object.json" || return 1
    state="$(python3 "${probe}" --logical-port "${logical_port}" \
      --local-chassis "${local_chassis}" --expected-datapath "${expected_datapath}" \
      --timeline "${timeline}" <"${EVIDENCE_DIR}/${sample_file}")" || return 1
    printf '%s state=%s logical_port=%s\n' "$(date -u +%FT%TZ)" "${state}" "${logical_port}" \
      >>"${EVIDENCE_DIR}/replacement-binding-wait.log" || return 1
    if [[ "${state}" == BOUND ]]; then
      interop_capture_diagnostic replacement-binding-ovs-interfaces.json "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,external_ids,ofport,error list Interface || return 1
      interop_capture_diagnostic replacement-binding-ovs-ports.json "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
      interop_capture_diagnostic replacement-binding-ovs-bridges.json "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
      python3 "${diagnostics}" verify-ovs-attachment \
        --interfaces "${EVIDENCE_DIR}/replacement-binding-ovs-interfaces.json" \
        --ports "${EVIDENCE_DIR}/replacement-binding-ovs-ports.json" \
        --bridges "${EVIDENCE_DIR}/replacement-binding-ovs-bridges.json" \
        --logical-port "${logical_port}" --expect present \
        >"${EVIDENCE_DIR}/replacement-binding-ovs-attachment.json" || return 1
      iface="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["interface"]["name"])' \
        <"${EVIDENCE_DIR}/replacement-binding-ovs-attachment.json")" || return 1
      owner="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["interface"]["external_ids"]["evpn.framework/managed-by"])' \
        <"${EVIDENCE_DIR}/replacement-binding-ovs-attachment.json")" || return 1
      workload_uid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["interface"]["external_ids"]["evpn.framework/workload-uid"])' \
        <"${EVIDENCE_DIR}/replacement-binding-ovs-attachment.json")" || return 1
      if [[ "${owner}" != evpn-node-agent || "${workload_uid}" != "${uid}" ]]; then
        interop_collect_replacement_binding_diagnostics "${suffix}"
        echo "replacement OVS interface ownership is incorrect: ${iface}" >&2
        return 1
      fi
      printf '%s\n' "${binding_json}" >"${EVIDENCE_DIR}/replacement-binding-ready.json" || return 1
      printf '%s\n' "${iface}" >"${EVIDENCE_DIR}/replacement-ovs-interface.txt"
      printf '%s\n' "${owner}" >"${EVIDENCE_DIR}/replacement-ovs-owner.txt"
      printf '%s\n' "${workload_uid}" >"${EVIDENCE_DIR}/replacement-ovs-workload-uid.txt"
      printf '%s\n' br-int >"${EVIDENCE_DIR}/replacement-ovs-bridge.txt"
      return 0
    fi
    sleep 1
  done
  interop_collect_replacement_binding_diagnostics "${suffix}" || return 1
  echo "replacement SB binding did not reach BOUND within 90 seconds; last state=${state}" >&2
  return 1
}

interop_wait_lifecycle_binding() {
  local uid="$1" label="$2" deadline=$((SECONDS+90)) logical_port state sample=0
  local switch_result lsp_result chassis_result datapath_result
  local switch_uuid chassis_uuid datapath_uuid binding_file
  local diagnostics="${ROOT}/tools/lib/cni_ovn_diagnostics.py"
  local timeline="${EVIDENCE_DIR}/${label}-binding-timeline.jsonl"
  local -a kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  local context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"

  interop_capture_diagnostic "${label}-binding-switches.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name list Logical_Switch || return 1
  switch_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-binding-switches.json" \
    --name-column name --name ls24b --uuid-column _uuid)" || return 1
  switch_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${switch_result}")" || return 1
  interop_capture_diagnostic "${label}-binding-lsps.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  lsp_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-binding-lsps.json" \
    --map-column external_ids --map-key evpn.framework/workload-uid --map-value "${uid}" \
    --uuid-column _uuid)" || return 1
  logical_port="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["name"])' \
    <<<"${lsp_result}")" || return 1
  interop_capture_diagnostic "${label}-binding-chassis.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,name,hostname list Chassis || return 1
  chassis_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-binding-chassis.json" \
    --name-column name --name "${LOCAL_CHASSIS}" --uuid-column _uuid)" || return 1
  chassis_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${chassis_result}")" || return 1
  interop_capture_diagnostic "${label}-binding-datapaths.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,external_ids list Datapath_Binding || return 1
  datapath_result="$(python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-binding-datapaths.json" \
    --map-column external_ids --map-key logical-switch --map-value "${switch_uuid}" \
    --uuid-column _uuid)" || return 1
  datapath_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${datapath_result}")" || return 1
  : >"${timeline}"
  while (( SECONDS < deadline )); do
    binding_file="${label}-binding-current-${sample}.json"
    interop_capture_diagnostic "${binding_file}" "${context}" json \
      "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
      ovn-sbctl --format=json --columns=logical_port,datapath,type,chassis,up \
      find Port_Binding "logical_port=${logical_port}" || return 1
    binding_file="${EVIDENCE_DIR}/${binding_file}"
    state="$(python3 "${ROOT}/tools/lib/cni_ovn_binding.py" \
      --logical-port "${logical_port}" --local-chassis "${chassis_uuid}" \
      --expected-datapath "${datapath_uuid}" --timeline "${timeline}" \
      <"${binding_file}")" || return 1
    if [[ "${state}" == BOUND ]]; then
      cp "${binding_file}" "${EVIDENCE_DIR}/${label}-binding-ready.json"
      interop_capture_diagnostic "${label}-binding-ovs-interfaces.json" "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,external_ids,ofport list Interface || return 1
      interop_capture_diagnostic "${label}-binding-ovs-ports.json" "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
      interop_capture_diagnostic "${label}-binding-ovs-bridges.json" "${context}" json \
        "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
        ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
      python3 "${diagnostics}" verify-ovs-attachment \
        --interfaces "${EVIDENCE_DIR}/${label}-binding-ovs-interfaces.json" \
        --ports "${EVIDENCE_DIR}/${label}-binding-ovs-ports.json" \
        --bridges "${EVIDENCE_DIR}/${label}-binding-ovs-bridges.json" \
        --logical-port "${logical_port}" --expect present \
        >"${EVIDENCE_DIR}/${label}-binding-ovs-attachment.json" || return 1
      return 0
    fi
    sample=$((sample + 1))
    sleep 1
  done
  echo "Port_Binding for ${logical_port} did not reach BOUND; last state=${state}" >&2
  return 1
}

interop_wait_rib_route() {
  local mac="$1" ip="$2" mode="$3" label="$4" deadline=$((SECONDS+90)) local_rib frr_rib found
  while (( SECONDS < deadline )); do
    local_rib="$(provider_exec gobgp global rib -a evpn 2>/dev/null || true)"
    frr_rib="$(docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route detail' 2>/dev/null || true)"
    found=0
    if grep -Fq "${mac}" <<<"${local_rib}" && grep -Fq "${ip}" <<<"${local_rib}" &&
      grep -Fq "${mac}" <<<"${frr_rib}" && grep -Fq "${ip}" <<<"${frr_rib}"; then
      found=1
    fi
    if [[ "${mode}" == present && "${found}" == 1 ]]; then
      if [[ "${mac}" == "${REMOTE_MAC}" ]] &&
        { ! grep -Fq "${FRR_VTEP}" <<<"${local_rib}" || ! grep -Fq "${FRR_VTEP}" <<<"${frr_rib}"; }; then
        sleep 1
        continue
      fi
      printf '%s\n' "${local_rib}" >"${EVIDENCE_DIR}/gobgp-rib-${label}.txt"
      printf '%s\n' "${frr_rib}" >"${EVIDENCE_DIR}/frr-rib-${label}.txt"
      return 0
    fi
    if [[ "${mode}" == absent ]] &&
      ! grep -Fq "${mac}" <<<"${local_rib}" && ! grep -Fq "${mac}" <<<"${frr_rib}"; then
      printf '%s\n' "${local_rib}" >"${EVIDENCE_DIR}/gobgp-rib-${label}.txt"
      printf '%s\n' "${frr_rib}" >"${EVIDENCE_DIR}/frr-rib-${label}.txt"
      return 0
    fi
    sleep 1
  done
  printf '%s\n' "${local_rib}" >"${EVIDENCE_DIR}/gobgp-rib-${label}-timeout.txt"
  printf '%s\n' "${frr_rib}" >"${EVIDENCE_DIR}/frr-rib-${label}-timeout.txt"
  if [[ "${label}" == local-readvertisement ]]; then
    interop_collect_replacement_binding_diagnostics local-readvertisement-timeout
    docker exec "${WORKER}" bridge -j fdb show >"${EVIDENCE_DIR}/worker-fdb-local-readvertisement-timeout.json" || true
    docker exec "${WORKER}" ip -4 -j neigh show nud all >"${EVIDENCE_DIR}/worker-neigh-local-readvertisement-timeout.json" || true
  fi
  echo "BGP RIB did not reach ${mode} state for ${label} ${mac}/${ip}" >&2
  return 1
}

interop_capture_local_uuids() {
  local status_file="${1:-${EVIDENCE_DIR}/export-status.json}"
  local output_file="${2:-${EVIDENCE_DIR}/local-path-uuids.json}"
  python3 - "${status_file}" "${output_file}" \
    "${LOCAL_MAC}" "${LOCAL_IP}" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
mac,ip=sys.argv[3:]
entries=[x for x in status["Result"]["Applied"] if x["Endpoint"]["MACAddress"].lower()==mac and
         (ip in (x["Endpoint"].get("IPAddresses") or []) or x["Endpoint"].get("MACOnly"))]
uuids=sorted(x["PathUUID"] for x in entries)
assert len(uuids)==2,entries
json.dump(uuids,open(sys.argv[2],"w"),indent=2)
print("local-endpoint-path-uuids="+",".join(uuids))
PY
}

interop_wait_local_uuid_withdrawal() {
  local uuid_file="${1:-${EVIDENCE_DIR}/local-path-uuids.json}"
  local label="${2:-local-withdrawal}"
  local deadline=$((SECONDS+90))
  local status_file="${EVIDENCE_DIR}/export-status-after-${label}.json" diagnostic=0
  while (( SECONDS < deadline )); do
    if docker exec "${WORKER}" sh -ec 'test -s /run/evpn-node-agent/export-status.json && cat /run/evpn-node-agent/export-status.json' \
      >"${status_file}" 2>/dev/null; then
      if [[ ! -s "${status_file}" && "${diagnostic}" == 0 ]]; then
        docker exec "${WORKER}" sh -c 'ls -la /run/evpn-node-agent; wc -c /run/evpn-node-agent/export-status.json 2>&1' \
          >"${EVIDENCE_DIR}/export-status-read-diagnostic-${label}.txt" 2>&1 || true
        diagnostic=1
      fi
      if [[ ! -s "${status_file}" ]]; then sleep 1; continue; fi
      if python3 - "${uuid_file}" "${status_file}" <<'PY'
import json,sys
old=set(json.load(open(sys.argv[1])))
status=json.load(open(sys.argv[2]))
current={x["PathUUID"] for x in (status.get("Result",{}).get("Applied") or [])}
sys.exit(bool(old & current))
PY
      then return 0; fi
    elif [[ "${diagnostic}" == 0 ]]; then
      docker exec "${WORKER}" sh -c 'ls -l /run/evpn-node-agent; wc -c /run/evpn-node-agent/export-status.json 2>&1' \
        >"${EVIDENCE_DIR}/export-status-read-diagnostic-${label}.txt" 2>&1 || true
      diagnostic=1
    fi
    sleep 1
  done
  cat "${status_file}" 2>/dev/null || true
  echo "UUID-exact local route withdrawal was not observed for ${label}" >&2
  return 1
}

interop_wait_local_kernel_withdrawal() {
  local deadline=$((SECONDS+90)) fdb neigh
  while (( SECONDS < deadline )); do
    fdb="$(docker exec "${FRR_CONTAINER}" bridge -j fdb show dev vxlan1000)"
    neigh="$(docker exec "${FRR_CONTAINER}" ip -4 -j neigh show nud all dev br1000)"
    if python3 - "${fdb}" "${neigh}" "${LOCAL_MAC}" "${LOCAL_IP}" <<'PY'
import json,sys
fdb=json.loads(sys.argv[1]);neigh=json.loads(sys.argv[2]);mac,ip=sys.argv[3:]
assert not any(x.get("mac","").lower()==mac for x in fdb),fdb
assert not any(x.get("dst")==ip for x in neigh),neigh
PY
    then
      printf '%s\n' "${fdb}" >"${EVIDENCE_DIR}/frr-fdb-after-local-withdrawal.json"
      printf '%s\n' "${neigh}" >"${EVIDENCE_DIR}/frr-neigh-after-local-withdrawal.json"
      return 0
    fi
    sleep 1
  done
  printf '%s\n' "${fdb}" >"${EVIDENCE_DIR}/frr-fdb-after-local-withdrawal.json"
  printf '%s\n' "${neigh}" >"${EVIDENCE_DIR}/frr-neigh-after-local-withdrawal.json"
  echo "FRR retained local endpoint forwarding after route withdrawal" >&2
  return 1
}

interop_assert_remote_kernel_withdrawal() {
  local fdb neigh
  fdb="$(docker exec "${WORKER}" bridge -j fdb show dev "${VXLAN_DEVICE}")"
  neigh="$(docker exec "${WORKER}" ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}")"
  python3 - "${fdb}" "${neigh}" "${REMOTE_MAC}" "${REMOTE_IP}" <<'PY'
import json,sys
fdb=json.loads(sys.argv[1]);neigh=json.loads(sys.argv[2]);mac,ip=sys.argv[3:]
assert not any(x.get("mac","").lower()==mac for x in fdb),fdb
assert not any(x.get("dst")==ip for x in neigh),neigh
PY
  printf '%s\n' "${fdb}" >"${EVIDENCE_DIR}/worker-fdb-after-remote-withdrawal.json"
  printf '%s\n' "${neigh}" >"${EVIDENCE_DIR}/worker-neigh-after-remote-withdrawal.json"
}

interop_verify_lifecycle() {
  local current_uid replacement_uid deadline neighbor
  LOCAL_MAC="$(k -n cni-test exec pod-a -- cat /sys/class/net/net1/address)"
  interop_capture_local_uuids
  current_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  printf '%s\n' "${current_uid}" >"${EVIDENCE_DIR}/pod-a-uid-before-withdrawal.txt"
  k -n cni-test delete pod pod-a --wait=true --timeout=90s | tee "${EVIDENCE_DIR}/local-delete.txt"
  interop_wait_local_uuid_withdrawal
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent local-withdrawal
  interop_wait_local_kernel_withdrawal
  docker exec "${WORKER}" bridge -j fdb show dev "${VXLAN_DEVICE}" \
    | tee "${EVIDENCE_DIR}/worker-fdb-after-local-withdrawal.json" >/dev/null
  docker exec "${WORKER}" ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}" \
    | tee "${EVIDENCE_DIR}/worker-neigh-after-local-withdrawal.json" >/dev/null
  if docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 1 -W 1 "${LOCAL_IP}" \
    >"${EVIDENCE_DIR}/remote-to-deleted-local-ping.txt" 2>&1; then
    echo "deleted local endpoint remained reachable" >&2
    return 1
  fi
  echo 'local endpoint withdrawal=PASS'

  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  replacement_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  printf '%s\n' "${replacement_uid}" >"${EVIDENCE_DIR}/pod-a-uid-after-readvertisement.txt"
  interop_wait_replacement_binding "${replacement_uid}"
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present local-readvertisement
  interop_wait_import_count 1 local-route-retained
  if ! k -n cni-test exec pod-a -- ping -I net1 -c 2 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/local-readvertisement-ping.txt"; then return 1; fi
  echo 'local endpoint re-advertisement=PASS'

  docker exec "${FRR_CONTAINER}" ip netns del evpnwork || return 1
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" absent remote-withdrawal
  interop_wait_import_count 0 remote-withdrawal
  interop_assert_remote_kernel_withdrawal
  if k -n cni-test exec pod-a -- ping -I net1 -c 1 -W 1 "${REMOTE_IP}" \
    >"${EVIDENCE_DIR}/local-to-deleted-remote-ping.txt" 2>&1; then
    echo "deleted remote endpoint remained reachable" >&2
    return 1
  fi
  echo 'remote endpoint withdrawal=PASS'

  interop_create_remote_workload
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 10.244.240.1 \
    | tee "${EVIDENCE_DIR}/remote-linux-readvertisement-warmup.txt"
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present remote-readvertisement
  interop_wait_import_count 1 remote-readvertisement
  if ! k -n cni-test exec pod-a -- ping -I net1 -c 2 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/remote-readvertisement-local-ping.txt"; then return 1; fi
  if ! docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 "${LOCAL_IP}" \
    | tee "${EVIDENCE_DIR}/remote-readvertisement-reverse-ping.txt"; then return 1; fi
  echo 'remote endpoint re-advertisement=PASS'

  docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
    -c "neighbor ${LOCAL_VTEP} shutdown" -c 'end'
  deadline=$((SECONDS+45))
  while (( SECONDS < deadline )); do
    neighbor="$(provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null || true)"
    if ! grep -qi 'Established' <<<"${neighbor}"; then break; fi
    sleep 1
  done
  if grep -qi 'Established' <<<"${neighbor}"; then
    echo 'FRR-GoBGP session did not interrupt' >&2
    return 1
  fi
  printf '%s\n' "${neighbor}" >"${EVIDENCE_DIR}/gobgp-neighbor-interrupted.txt"
  provider_exec gobgp global rib -a evpn | tee "${EVIDENCE_DIR}/gobgp-rib-interrupted.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn summary' \
    | tee "${EVIDENCE_DIR}/frr-neighbor-interrupted.txt"
  sleep 3
  docker exec "${WORKER}" cat /run/evpn-node-agent/remote-status.json \
    | tee "${EVIDENCE_DIR}/remote-status-bgp-interrupted.json" >/dev/null
  python3 - "${EVIDENCE_DIR}/remote-status-bgp-interrupted.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]));snapshot=s.get("Snapshot") or {}
endpoints=snapshot.get("Endpoints") or [];receipts=s.get("Receipts") or []
if snapshot.get("Authoritative"):
    assert (len(endpoints),len(receipts)) in ((0,0),(1,1)),s
else:
    assert len(receipts)==1,s
print("BGP interruption importer state: authoritative=%s endpoints=%d receipts=%d error=%s" %
      (snapshot.get("Authoritative"),len(endpoints),len(receipts),s.get("Error","")))
PY
  docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
    -c "no neighbor ${LOCAL_VTEP} shutdown" -c 'end'
  deadline=$((SECONDS+90))
  while (( SECONDS < deadline )); do
    neighbor="$(provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null || true)"
    if grep -qi 'Established' <<<"${neighbor}"; then break; fi
    sleep 1
  done
  if ! grep -qi 'Established' <<<"${neighbor}"; then
    echo 'FRR-GoBGP BGP recovery timed out' >&2
    return 1
  fi
  printf '%s\n' "${neighbor}" >"${EVIDENCE_DIR}/gobgp-neighbor-recovered.txt"
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present bgp-recovery
  interop_wait_import_count 1 bgp-recovery
  if ! k -n cni-test exec pod-a -- ping -I net1 -c 2 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/bgp-recovery-local-ping.txt"; then return 1; fi
  if ! docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 "${LOCAL_IP}" \
    | tee "${EVIDENCE_DIR}/bgp-recovery-reverse-ping.txt"; then return 1; fi
  echo 'FRR-GoBGP session interruption and recovery=PASS'
}

interop_assert_initial_route_scope() {
  python3 - "${EVIDENCE_DIR}/gobgp-evpn-rib.txt" "${EVIDENCE_DIR}/frr-evpn-rib.txt" \
    "${EVIDENCE_DIR}/frr-running-config.txt" "${EVIDENCE_DIR}/remote-status-initial-packet-path.json" \
    "${EVIDENCE_DIR}/fixture-route-scope.json" \
    "${FRR_PEER}" "${VNI}" "${REMOTE_MAC}" "${REMOTE_IP}" <<'PY'
import json,re,sys
gobgp_path,frr_path,config_path,status_path,output,peer,vni,mac,ip=sys.argv[1:]
gobgp=open(gobgp_path).read()
frr=open(frr_path).read()
config=open(config_path).read()
status=json.load(open(status_path))
rd=f"{peer}:{vni}"
gobgp_prefix=f"[type:macadv][rd:{rd}][etag:0][mac:{mac}]"
gobgp_mac_only=any(gobgp_prefix in line and "[ip:" not in line for line in gobgp.splitlines())
gobgp_mac_ip=any(gobgp_prefix in line and f"[ip:{ip}]" in line for line in gobgp.splitlines())
frr_prefix=f"BGP routing table entry for {rd}:[2]:[0]:[48]:[{mac}]"
blocks=re.split(r"(?=^BGP routing table entry for )",frr,flags=re.MULTILINE)
def active_advertised(route):
    return any(block.partition("\n")[0] == route and "Paths: (1 available" in block and
               re.search(r"(?m)^[ \t]*Advertised to (?:non peer-group )?peers:[ \t]*\n"
                         r"[ \t]*198\.18\.0\.2[ \t]*(?:\n|$)", block)
               for block in blocks)
frr_mac_only=active_advertised(frr_prefix)
frr_mac_ip=active_advertised(f"{frr_prefix}:[32]:[{ip}]")
assert gobgp_mac_only and gobgp_mac_ip,(gobgp_mac_only,gobgp_mac_ip,gobgp)
assert frr_mac_only and frr_mac_ip,(frr_mac_only,frr_mac_ip,frr)
assert "advertise-all-vni" in config, "workload VNI advertisements are not enabled"
assert "advertise-svi-ip" not in config, "out-of-scope SVI advertisement is still configured"
svi_mac="72:81:60:13:a0:6d"
svi_ip="fe80::9c52:7eff:fe00:188b"
assert svi_mac not in gobgp and svi_ip not in gobgp,(svi_mac,svi_ip,gobgp)
assert svi_mac not in frr and svi_ip not in frr,(svi_mac,svi_ip,frr)
snapshot=status.get("Snapshot") or {}
assert not status.get("Error"),status
assert snapshot.get("Authoritative") is True,snapshot
endpoints=snapshot.get("Endpoints") or []
assert len(endpoints)==1,endpoints
endpoint=endpoints[0]
assert (endpoint.get("MAC"),endpoint.get("IP"),endpoint.get("RemoteVTEP"),endpoint.get("VNI")) == \
       (mac,ip,"198.18.0.4",int(vni)),endpoint
routes=[entry.get("Route",{}) for entry in endpoint.get("Routes",[])]
assert len(routes)==2 and {route.get("IP","") for route in routes}=={"",ip},routes
assert all(route.get("MAC","").lower()==mac.lower() and route.get("Labels")==[int(vni)] and
           f"64512:{vni}" in route.get("RouteTargets",[]) and route.get("NextHop")=="198.18.0.4"
           for route in routes),routes
assert len(status.get("Receipts") or [])==1,status
result={
    "fixture":"disposable D2 initial-only FRR",
    "advertiseAllVni":True,
    "advertiseSviIp":False,
    "workloadMacOnly":gobgp_mac_only and frr_mac_only,
    "workloadMacIPv4":gobgp_mac_ip and frr_mac_ip,
    "sviMac":"72:81:60:13:a0:6d",
    "sviIPv6":"fe80::9c52:7eff:fe00:188b",
    "sviType2AbsentFromGoBGPAndFRRRibs":True,
}
json.dump(result,open(output,"w"),indent=2)
print("initial fixture route scope="+json.dumps(result,sort_keys=True))
PY
}

interop_capture_binding_routes() {
  local phase="$1" context frr_context worker_context
  local -a provider_kubectl
  local kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  provider_kubectl=("${kubectl[@]}" -n k8gobgp-system exec "${PROVIDER_POD}" --)
  frr_context="docker exec FRR container ${FRR_CONTAINER}"
  worker_context="docker exec kind worker ${WORKER}"
  context="kubectl exec GoBGP provider Pod ${PROVIDER_POD} in k8gobgp-system"
  printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
    >"${EVIDENCE_DIR}/route-snapshot-${phase}.timestamp"
  interop_capture_diagnostic "gobgp-neighbor-${phase}.txt" "${context}" required \
    "${provider_kubectl[@]}" gobgp neighbor "${FRR_PEER}" || return 1
  interop_capture_diagnostic "gobgp-evpn-rib-${phase}.txt" "${context}" required \
    "${provider_kubectl[@]}" gobgp global rib -a evpn || return 1
  context="${frr_context}"
  interop_capture_diagnostic "frr-neighbor-${phase}.txt" \
    "${context}" required timeout --kill-after=2s 15s docker exec "${FRR_CONTAINER}" \
    vtysh -c "show bgp neighbor ${LOCAL_VTEP}" || return 1
  interop_capture_diagnostic "frr-evpn-rib-${phase}.txt" "${context}" required \
    timeout --kill-after=2s 15s docker exec "${FRR_CONTAINER}" \
    vtysh -c 'show bgp l2vpn evpn route detail' || return 1
  context="${worker_context}"
  interop_capture_diagnostic "export-status-route-${phase}.json" "${context}" json \
    timeout --kill-after=2s 15s docker exec "${WORKER}" \
    cat /run/evpn-node-agent/export-status.json || return 1
  interop_capture_diagnostic "remote-status-route-${phase}.json" "${context}" json \
    timeout --kill-after=2s 15s docker exec "${WORKER}" \
    cat /run/evpn-node-agent/remote-status.json || return 1
}

interop_validate_replacement_snapshot() {
  local phase="$1" uid="$2" switch_row switch_uuid lsp_row lsp_uuid lsp_name
  local helper="${ROOT}/tools/lib/cni_ovn_diagnostics.py"
  python3 - "${EVIDENCE_DIR}/pods-${phase}.json" "${phase}" "${uid}" <<'PY'
import json,sys
path, phase, uid = sys.argv[1:]
pods = [p for p in json.load(open(path))["items"] if p["metadata"]["name"] == "pod-a"]
if phase in ("deleted", "cleanup"):
    assert not pods, pods
else:
    assert len(pods) == 1 and pods[0]["metadata"]["uid"] == uid, pods
PY
  [[ "$?" == 0 ]] || return 1
  switch_row="$(python3 "${helper}" verify-control \
    --input "${EVIDENCE_DIR}/ovn-ls24b-${phase}.json" \
    --name-column name --name ls24b --uuid-column _uuid)" || return 1
  switch_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${switch_row}")" || return 1
  if [[ "${phase}" == deleted || "${phase}" == cleanup ]]; then
    python3 "${helper}" classify-object --input "${EVIDENCE_DIR}/ovn-ports-${phase}.json" \
      --object-kind Logical_Switch_Port --map-column external_ids \
      --map-key evpn.framework/workload-uid --map-value "${uid}" --expect absent \
      >"${EVIDENCE_DIR}/ovn-lsp-state-${phase}.json" || return 1
    python3 "${helper}" classify-object --input "${EVIDENCE_DIR}/ovs-interfaces-${phase}.json" \
      --object-kind OVS_Interface --map-column external_ids \
      --map-key evpn.framework/workload-uid --map-value "${uid}" --expect absent \
      >"${EVIDENCE_DIR}/ovs-interface-state-${phase}.json" || return 1
    return 0
  fi
  lsp_row="$(python3 "${helper}" verify-control \
    --input "${EVIDENCE_DIR}/ovn-ports-${phase}.json" \
    --map-column external_ids --map-key evpn.framework/workload-uid --map-value "${uid}" \
    --uuid-column _uuid)" || return 1
  lsp_uuid="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["_uuid"])' \
    <<<"${lsp_row}")" || return 1
  lsp_name="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["row"]["name"])' \
    <<<"${lsp_row}")" || return 1
  [[ -n "${switch_uuid}" && -n "${lsp_uuid}" && -n "${lsp_name}" ]] || return 1

  python3 "${helper}" verify-control \
    --input "${EVIDENCE_DIR}/ovn-ls24b-${phase}.json" \
    --name-column name --name ls24b --uuid-column _uuid \
    --member-column ports --member-uuid "${lsp_uuid}" \
    >"${EVIDENCE_DIR}/ovn-switch-membership-${phase}.json" || return 1
  python3 "${helper}" verify-control \
    --input "${EVIDENCE_DIR}/ovn-datapaths-${phase}.json" \
    --map-column external_ids --map-key logical-switch --map-value "${switch_uuid}" \
    --uuid-column _uuid \
    >"${EVIDENCE_DIR}/ovn-expected-datapath-${phase}.json" || return 1
  python3 "${helper}" verify-control \
    --input "${EVIDENCE_DIR}/ovn-chassis-${phase}.json" \
    --name-column name --name "${LOCAL_CHASSIS}" --uuid-column _uuid \
    >"${EVIDENCE_DIR}/ovn-local-chassis-${phase}.json" || return 1

  python3 "${helper}" classify-object --input "${EVIDENCE_DIR}/ovn-bindings-${phase}.json" \
    --object-kind Port_Binding --key-column logical_port --key-value "${lsp_name}" \
    >"${EVIDENCE_DIR}/ovn-binding-state-${phase}.json" || return 1
  python3 "${helper}" verify-ovs-attachment \
    --interfaces "${EVIDENCE_DIR}/ovs-interfaces-${phase}.json" \
    --ports "${EVIDENCE_DIR}/ovs-ports-${phase}.json" \
    --bridges "${EVIDENCE_DIR}/ovs-bridges-${phase}.json" \
    --logical-port "${lsp_name}" \
    >"${EVIDENCE_DIR}/ovs-interface-state-${phase}.json" || return 1
  if [[ "${phase}" == initial ]]; then
    local datapath chassis state
    datapath="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["row"]["_uuid"])' \
      "${EVIDENCE_DIR}/ovn-expected-datapath-${phase}.json")" || return 1
    chassis="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["row"]["_uuid"])' \
      "${EVIDENCE_DIR}/ovn-local-chassis-${phase}.json")" || return 1
    state="$(python3 "${ROOT}/tools/lib/cni_ovn_binding.py" \
      --logical-port "${lsp_name}" --local-chassis "${chassis}" \
      --expected-datapath "${datapath}" \
      --timeline "${EVIDENCE_DIR}/initial-binding-timeline.jsonl" \
      <"${EVIDENCE_DIR}/ovn-bindings-${phase}.json")" || return 1
    [[ "${state}" == BOUND ]] || {
      echo "original endpoint binding is ${state}, not BOUND; lifecycle not started" >&2
      return 1
    }
    python3 "${helper}" verify-ovs-attachment \
      --interfaces "${EVIDENCE_DIR}/ovs-interfaces-${phase}.json" \
      --ports "${EVIDENCE_DIR}/ovs-ports-${phase}.json" \
      --bridges "${EVIDENCE_DIR}/ovs-bridges-${phase}.json" \
      --logical-port "${lsp_name}" --expect present \
      >"${EVIDENCE_DIR}/ovs-initial-attachment-verified.json" || return 1
  fi
}

interop_capture_binding_snapshot() {
  local phase="$1" uid="$2" context
  local kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
    >"${EVIDENCE_DIR}/ovn-snapshot-${phase}.timestamp"
  interop_collect_replacement_binding_diagnostics "${phase}" || return 1
  interop_capture_diagnostic "ovn-ls24b-${phase}.json" \
    "${context}" json "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,ports,other_config,external_ids \
    list Logical_Switch ls24b || return 1
  interop_capture_diagnostic "ovn-nb-global-${phase}.txt" \
    "${context}" required "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl list NB_Global || return 1
  interop_capture_diagnostic "ovn-sb-global-${phase}.txt" \
    "${context}" required "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl list SB_Global || return 1
  interop_validate_replacement_snapshot "${phase}" "${uid}" || return 1
  interop_capture_diagnostic "node-agent-state-${phase}.log" \
    "host kubectl logs node-agent Pod ${AGENT_POD} in cni-test" required \
    "${kubectl[@]}" -n cni-test logs "${AGENT_POD}" --timestamps || return 1
  if [[ "${phase}" == initial ]]; then
    interop_capture_diagnostic agent-runtime-binaries.sha256 \
      "kubectl exec node-agent Pod ${AGENT_POD} in cni-test" required \
      "${kubectl[@]}" -n cni-test exec "${AGENT_POD}" -- sha256sum \
      /usr/local/bin/evpn-node-agent /usr/local/bin/evpn-cni \
      /usr/local/bin/nodeagent.test /usr/local/bin/evpn.test || return 1
    interop_capture_diagnostic agent-runtime-pod.json \
      "host kubectl get node-agent Pod ${AGENT_POD} in cni-test" json \
      "${kubectl[@]}" -n cni-test get pod "${AGENT_POD}" -o json || return 1
  fi
  interop_capture_diagnostic "cni-calls-${phase}.tar" \
    "docker exec kind worker ${WORKER}" required \
    timeout --kill-after=2s 15s docker exec "${WORKER}" \
    tar -cf - -C /run/evpn-cni-evidence . || return 1
  mkdir "${EVIDENCE_DIR}/cni-${phase}" || return 1
  tar -xf "${EVIDENCE_DIR}/cni-calls-${phase}.tar" -C "${EVIDENCE_DIR}/cni-${phase}" || return 1
  python3 - "${EVIDENCE_DIR}/cni-${phase}" "${uid}" "${phase}" \
    >"${EVIDENCE_DIR}/cni-call-${phase}.json" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
uid, phase = sys.argv[2:]
matches = []
for call in root.glob("call.*"):
    env = dict(line.split("=", 1) for line in (call / "env").read_text().splitlines())
    expected_command = "DEL" if phase in ("deleted", "cleanup") else "ADD"
    if env.get("CNI_COMMAND") != expected_command:
        continue
    if uid not in env.get("CNI_ARGS", ""):
        continue
    matches.append((call, env))
result = {"phase": phase, "workloadUID": uid, "matches": []}
for call, env in matches:
    result["matches"].append({
        "directory": call.name,
        "command": env.get("CNI_COMMAND"),
        "args": env.get("CNI_ARGS"),
        "exitStatus": (call / "rc").read_text().strip(),
        "stdout": (call / "stdout").read_text(errors="replace"),
        "stderr": (call / "stderr").read_text(errors="replace"),
    })
if not matches or any(item["exitStatus"] != "0" for item in result["matches"]):
    raise SystemExit("missing successful UID-specific CNI lifecycle call")
print(json.dumps(result, indent=2, sort_keys=True))
PY
}

interop_wait_uid_absent() {
  local uid="$1" phase="${2:-deleted-endpoint}" interface lsp links
  local timeline="${EVIDENCE_DIR}/${phase}-timeline.jsonl"
  : >"${timeline}"
  for _ in $(seq 1 90); do
    if ! interface="$(rt ovs-vsctl --bare --columns=name find Interface \
      "external_ids:evpn.framework/workload-uid=${uid}")" ||
      ! lsp="$(rt ovn-nbctl --bare --columns=name find Logical_Switch_Port \
        "external_ids:evpn.framework/workload-uid=${uid}")" ||
      ! links="$(ag sh -ec 'for a in /sys/class/net/*/ifalias; do
        if [ "$(cat "$a")" = "evpn-node-agent|n24|s|n24|s|$1" ]; then echo "$a"; fi
      done' sh "${uid}")"; then
      printf '%s query-error\n' "$(date -u +%FT%T.%NZ)" >>"${timeline}"
      sleep 1
      continue
    fi
    printf '%s interface=%q lsp=%q links=%q\n' \
      "$(date -u +%FT%T.%NZ)" "${interface}" "${lsp}" "${links}" >>"${timeline}"
    if [[ -z "${interface}" && -z "${lsp}" && -z "${links}" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "owned endpoint resources remained after DEL for UID ${uid}" >&2
  return 1
}

interop_find_copied_cni_call() {
  local uid="$1" command="$2" calls_dir="${3:-${EVIDENCE_DIR}/initial-cni}"
  python3 "${ROOT}/tools/lib/cni_invocation_identity.py" select-successful \
    --calls-dir "${calls_dir}" --uid "${uid}" --command "${command}"
}

interop_capture_endpoint_identities() {
  local uid_a="$1" uid_b="$2" call_a call_b tool
  tool="${ROOT}/tools/lib/cni_evpn_acceptance.py"
  interop_capture_diagnostic "acceptance-nb-lsps-before-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  interop_capture_diagnostic "acceptance-sb-bindings-before-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,logical_port list Port_Binding || return 1
  interop_capture_diagnostic "acceptance-ovs-interfaces-before-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,external_ids list Interface || return 1
  interop_capture_diagnostic "acceptance-ovs-ports-before-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
  interop_capture_diagnostic "acceptance-ovs-bridges-before-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
  call_a="$(interop_find_copied_cni_call "${uid_a}" ADD)" || return 1
  call_b="$(interop_find_copied_cni_call "${uid_b}" ADD)" || return 1
  python3 "${tool}" capture-identity \
    --call-dir "${call_a}" --uid "${uid_a}" --pod-namespace cni-test --pod-name pod-a \
    --rd "${LOCAL_VTEP}:${VNI}" --vni "${VNI}" \
    --nb-lsps "${EVIDENCE_DIR}/acceptance-nb-lsps-before-delete.json" \
    --sb-bindings "${EVIDENCE_DIR}/acceptance-sb-bindings-before-delete.json" \
    --ovs-interfaces "${EVIDENCE_DIR}/acceptance-ovs-interfaces-before-delete.json" \
    --ovs-ports "${EVIDENCE_DIR}/acceptance-ovs-ports-before-delete.json" \
    --output "${EVIDENCE_DIR}/endpoint-a-identity.json" \
    >"${EVIDENCE_DIR}/endpoint-a-identity.stdout" || return 1
  python3 "${tool}" capture-identity \
    --call-dir "${call_b}" --uid "${uid_b}" --pod-namespace cni-test --pod-name pod-b \
    --rd "${LOCAL_VTEP}:${VNI}" --vni "${VNI}" \
    --nb-lsps "${EVIDENCE_DIR}/acceptance-nb-lsps-before-delete.json" \
    --sb-bindings "${EVIDENCE_DIR}/acceptance-sb-bindings-before-delete.json" \
    --ovs-interfaces "${EVIDENCE_DIR}/acceptance-ovs-interfaces-before-delete.json" \
    --ovs-ports "${EVIDENCE_DIR}/acceptance-ovs-ports-before-delete.json" \
    --output "${EVIDENCE_DIR}/endpoint-b-identity.json" \
    >"${EVIDENCE_DIR}/endpoint-b-identity.stdout" || return 1
  local identity logical_port
  for identity in endpoint-a-identity endpoint-b-identity; do
    logical_port="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["logicalPort"])' \
      "${EVIDENCE_DIR}/${identity}.json")" || return 1
    python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-ovs-attachment \
      --interfaces "${EVIDENCE_DIR}/acceptance-ovs-interfaces-before-delete.json" \
      --ports "${EVIDENCE_DIR}/acceptance-ovs-ports-before-delete.json" \
      --bridges "${EVIDENCE_DIR}/acceptance-ovs-bridges-before-delete.json" \
      --logical-port "${logical_port}" --expect present \
      >"${EVIDENCE_DIR}/${identity}-ovs-attachment.json" || return 1
  done
  LOCAL_MAC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mac"])' \
    "${EVIDENCE_DIR}/endpoint-a-identity.json")" || return 1
  LOCAL_IP="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' \
    "${EVIDENCE_DIR}/endpoint-a-identity.json")" || return 1
}

interop_capture_lifecycle_identity() {
  local uid="$1" call="$2" label="$3" pod_name="${4:-pod-a}"
  local tool="${ROOT}/tools/lib/cni_evpn_acceptance.py"
  local logical_port context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  local -a kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  interop_capture_diagnostic "${label}-nb-lsps.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  interop_capture_diagnostic "${label}-sb-bindings.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,logical_port list Port_Binding || return 1
  interop_capture_diagnostic "${label}-ovs-interfaces.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,external_ids list Interface || return 1
  interop_capture_diagnostic "${label}-ovs-ports.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
  interop_capture_diagnostic "${label}-ovs-bridges.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,ports list Bridge || return 1
  python3 "${tool}" capture-identity \
    --call-dir "${call}" --uid "${uid}" --pod-namespace cni-test --pod-name "${pod_name}" \
    --rd "${LOCAL_VTEP}:${VNI}" --vni "${VNI}" \
    --nb-lsps "${EVIDENCE_DIR}/${label}-nb-lsps.json" \
    --sb-bindings "${EVIDENCE_DIR}/${label}-sb-bindings.json" \
    --ovs-interfaces "${EVIDENCE_DIR}/${label}-ovs-interfaces.json" \
    --ovs-ports "${EVIDENCE_DIR}/${label}-ovs-ports.json" \
    --output "${EVIDENCE_DIR}/${label}-identity.json" \
    >"${EVIDENCE_DIR}/${label}-identity.stdout" || return 1
  logical_port="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["logicalPort"])' \
    "${EVIDENCE_DIR}/${label}-identity.json")" || return 1
  python3 "${ROOT}/tools/lib/cni_ovn_diagnostics.py" verify-ovs-attachment \
    --interfaces "${EVIDENCE_DIR}/${label}-ovs-interfaces.json" \
    --ports "${EVIDENCE_DIR}/${label}-ovs-ports.json" \
    --bridges "${EVIDENCE_DIR}/${label}-ovs-bridges.json" \
    --logical-port "${logical_port}" --expect present \
    >"${EVIDENCE_DIR}/${label}-ovs-attachment.json" || return 1
}

interop_assert_cni_check() {
  local call="$1" evidence="$2" netns_override="${3:-}" container_override="${4:-}"
  interop_replay_cni_operation "${call}" CHECK "${evidence}" success \
    "${netns_override}" "${container_override}" || return 1
  [[ ! -s "${evidence}.stdout" && ! -s "${evidence}.stderr" ]] || {
    echo "CNI CHECK emitted unexpected output: ${evidence}" >&2
    return 1
  }
}

interop_assert_single_owned_endpoint() {
  local uid="$1" label="$2"
  local diagnostics="${ROOT}/tools/lib/cni_ovn_diagnostics.py"
  local context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  local -a kubectl=(timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}")
  interop_capture_diagnostic "${label}-owned-nb-ports.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  interop_capture_diagnostic "${label}-owned-ovs-interfaces.json" "${context}" json \
    "${kubectl[@]}" -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,external_ids list Interface || return 1
  python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-owned-nb-ports.json" \
    --map-column external_ids --map-key evpn.framework/workload-uid --map-value "${uid}" \
    --uuid-column _uuid >"${EVIDENCE_DIR}/${label}-owned-nb-port.json" || return 1
  python3 "${diagnostics}" verify-control \
    --input "${EVIDENCE_DIR}/${label}-owned-ovs-interfaces.json" \
    --map-column external_ids --map-key evpn.framework/workload-uid --map-value "${uid}" \
    --uuid-column _uuid >"${EVIDENCE_DIR}/${label}-owned-ovs-interface.json" || return 1
  python3 - "${EVIDENCE_DIR}/${label}-owned-nb-port.json" \
    "${EVIDENCE_DIR}/${label}-owned-ovs-interface.json" <<'PY'
import json
import sys

nb_port, ovs_interface = (json.load(open(path, encoding="utf-8"))["row"] for path in sys.argv[1:])
assert nb_port["name"] == ovs_interface["external_ids"].get("iface-id"), (
    nb_port["name"], ovs_interface["external_ids"].get("iface-id")
)
PY
}

interop_prepare_full_lifecycle() {
  local uid_a uid_b call_a call_a_copy call_b call_b_copy
  copy_calls "${EVIDENCE_DIR}/initial-cni" || return 1
  uid_a="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')" || return 1
  uid_b="$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')" || return 1
  [[ -n "${uid_a}" && -n "${uid_b}" && "${uid_a}" != "${uid_b}" ]] || {
    echo "full lifecycle requires distinct initial and survivor Pod UIDs" >&2
    return 1
  }
  call_a="$(interop_find_cni_call "${uid_a}" ADD)" || return 1
  call_a_copy="$(interop_find_copied_cni_call "${uid_a}" ADD)" || return 1
  call_b="$(interop_find_cni_call "${uid_b}" ADD)" || return 1
  call_b_copy="$(interop_find_copied_cni_call "${uid_b}" ADD)" || return 1
  interop_capture_endpoint_identities "${uid_a}" "${uid_b}" || return 1
  interop_wait_endpoint_bound "${uid_a}" initial-pod-a "${call_a_copy}" pod-a || return 1
  interop_wait_endpoint_bound "${uid_b}" initial-survivor "${call_b_copy}" pod-b || return 1
  interop_assert_cni_check "${call_a}" "${EVIDENCE_DIR}/initial-cni-check" || return 1
  interop_assert_single_owned_endpoint "${uid_a}" initial-add || return 1

  interop_replay_cni_operation "${call_a}" ADD \
    "${EVIDENCE_DIR}/initial-add-replay" success || return 1
  interop_assert_cni_result_matches "${call_a_copy}" \
    "${EVIDENCE_DIR}/initial-add-replay.stdout" \
    "${EVIDENCE_DIR}/initial-add-result-comparison.json" || return 1
  interop_assert_single_owned_endpoint "${uid_a}" repeated-add || return 1

  LOCAL_MAC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mac"])' \
    "${EVIDENCE_DIR}/endpoint-a-identity.json")" || return 1
  LOCAL_IP="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' \
    "${EVIDENCE_DIR}/endpoint-a-identity.json")" || return 1
  SURVIVOR_MAC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mac"])' \
    "${EVIDENCE_DIR}/endpoint-b-identity.json")" || return 1
  SURVIVOR_IP="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' \
    "${EVIDENCE_DIR}/endpoint-b-identity.json")" || return 1
  printf '%s\n' "${uid_a}" >"${EVIDENCE_DIR}/initial-pod-a-uid.txt" || return 1
  printf '%s\n' "${uid_b}" >"${EVIDENCE_DIR}/survivor-pod-b-uid.txt" || return 1
  printf '%s\n' "${SURVIVOR_MAC}" >"${EVIDENCE_DIR}/survivor-mac.txt" || return 1
  printf '%s\n' "${SURVIVOR_IP}" >"${EVIDENCE_DIR}/survivor-ip.txt" || return 1
  interop_wait_rib_route "${SURVIVOR_MAC}" "${SURVIVOR_IP}" present initial-survivor || return 1
  echo "initial CNI CHECK, repeated ADD, exact result, and endpoint identities=PASS"
}

interop_wait_endpoint_bound() {
  local uid="$1" label="$2" call="$3" pod_name="${4:-pod-a}"
  interop_wait_lifecycle_binding "${uid}" "${label}" || return 1
  interop_capture_lifecycle_identity "${uid}" "${call}" "${label}" \
    "${pod_name}"
}

interop_assert_survivor() {
  local uid="$1" call_copy="$2" phase="$3"
  [[ "$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')" == "${uid}" ]] || {
    echo "survivor Pod identity changed during ${phase}" >&2
    return 1
  }
  interop_wait_endpoint_bound "${uid}" "${phase}-survivor" "${call_copy}" pod-b || return 1
  python3 - "${EVIDENCE_DIR}/endpoint-b-identity.json" \
    "${EVIDENCE_DIR}/${phase}-survivor-identity.json" <<'PY' || return 1
import json
import sys

expected, actual = (json.load(open(path, encoding="utf-8")) for path in sys.argv[1:])
assert actual == expected, {"expected": expected, "actual": actual}
PY
  interop_wait_rib_route "${SURVIVOR_MAC}" "${SURVIVOR_IP}" present \
    "${phase}-survivor" || return 1
  interop_assert_single_owned_endpoint "${uid}" "${phase}-survivor" || return 1
  k -n cni-test exec pod-b -- ping -I net1 -c 3 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/${phase}-survivor-to-remote-ping.txt" || return 1
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 \
    -c 3 -W 2 "${SURVIVOR_IP}" \
    | tee "${EVIDENCE_DIR}/${phase}-remote-to-survivor-ping.txt" || return 1
  echo "survivor OVN/SB/OVS identity, Type-2 route, and bidirectional traffic (${phase})=PASS"
}

interop_run_failed_add_rollback() {
  local source_call="$1" rollback_uid rollback_container rollback_ns rollback_netns
  local missing_netns worker_call local_call
  rollback_uid="$(python3 -c 'import uuid; print(uuid.uuid4())')" || return 1
  rollback_container="rollback-${rollback_uid}"
  rollback_ns="evpn-cni-${rollback_uid}"
  rollback_netns="/run/netns/${rollback_ns}"
  missing_netns="${rollback_netns}-missing"
  local_call="${EVIDENCE_DIR}/rollback-${rollback_uid}"
  worker_call="/tmp/rollback-${rollback_uid}"
  printf '%s\n' "${rollback_uid}" >"${EVIDENCE_DIR}/rollback-add-uid.txt" || return 1
  printf '%s\n' "${rollback_netns}" >"${EVIDENCE_DIR}/rollback-add-netns.txt" || return 1
  python3 - "${source_call}" "${local_call}" "${rollback_uid}" "${rollback_container}" <<'PY' || return 1
import pathlib
import shutil
import sys

source, destination, uid = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
shutil.copytree(source, destination)
env_path = destination / "env"
lines = env_path.read_text(encoding="utf-8").splitlines()
updated = []
found_args = False
for line in lines:
    key, separator, value = line.partition("=")
    if key == "CNI_ARGS":
        args = [item for item in value.split(";") if item]
        replacements = {
            "K8S_POD_NAMESPACE": "cni-test",
            "K8S_POD_NAME": "pod-rollback",
            "K8S_POD_UID": uid,
            "K8S_POD_INFRA_CONTAINER_ID": sys.argv[4],
        }
        seen = set()
        rewritten = []
        for item in args:
            arg_key, equals, arg_value = item.partition("=")
            if arg_key in replacements:
                rewritten.append(f"{arg_key}={replacements[arg_key]}")
                seen.add(arg_key)
            else:
                rewritten.append(item)
        if not {"K8S_POD_NAMESPACE", "K8S_POD_NAME", "K8S_POD_UID"}.issubset(seen):
            raise SystemExit(f"CNI_ARGS lacks required Kubernetes identity: {seen}")
        value = ";".join(rewritten)
        found_args = True
        line = f"{key}{separator}{value}"
    updated.append(line)
if not found_args:
    raise SystemExit("CNI call has no CNI_ARGS")
env_path.write_text("\n".join(updated) + "\n", encoding="utf-8")
PY
  if docker exec "${WORKER}" test -e "${worker_call}" ||
    docker exec "${WORKER}" ip netns list | awk '{print $1}' | grep -Fxq "${rollback_ns}"; then
    echo "rollback test identity already exists; refusing to overwrite it" >&2
    return 1
  fi
  tar -C "${local_call}" -cf - config env rc stderr stdout |
    docker exec -i "${WORKER}" sh -ec \
      'mkdir -p "$1"; tar -C "$1" -xf -' sh "${worker_call}" || return 1
  docker exec "${WORKER}" test -s "${worker_call}/config" || return 1
  docker exec "${WORKER}" test -s "${worker_call}/env" || return 1
  docker exec "${WORKER}" test ! -e "${missing_netns}" || return 1
  interop_replay_cni_operation "${worker_call}" ADD \
    "${EVIDENCE_DIR}/rollback-add-failed" failure "${missing_netns}" \
    "${rollback_container}" || return 1
  python3 - "${EVIDENCE_DIR}/rollback-add-failed.stdout" <<'PY' || return 1
import json
import sys

with open(sys.argv[1], encoding="utf-8") as output:
    result = json.load(output)
assert result.get("code") == 999, result
message=result.get("msg", "").lower()
assert "open netns" in message or message.startswith("hold cni network namespace: open "), result
assert "no such file or directory" in message, result
PY
  interop_wait_uid_absent "${rollback_uid}" rollback-failed-add || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent rollback-failed-add || return 1

  docker exec "${WORKER}" ip netns add "${rollback_ns}" || return 1
  docker exec "${WORKER}" ip netns exec "${rollback_ns}" ip link set lo up || return 1
  if [[ "${CNI_CREATION_IDENTITY:-false}" == true ]]; then
    docker exec "${WORKER}" cat /run/evpn-node-agent/cni-receipts-v1/journal.json \
      >"${EVIDENCE_DIR}/forged-add-journal-before.json" || return 1
    interop_replay_cni_operation "${worker_call}" ADD \
      "${EVIDENCE_DIR}/forged-add-rejected" failure "${rollback_netns}" \
      "${rollback_container}" || return 1
    docker exec "${WORKER}" cat /run/evpn-node-agent/cni-receipts-v1/journal.json \
      >"${EVIDENCE_DIR}/forged-add-journal-after.json" || return 1
    python3 - "${EVIDENCE_DIR}" <<'PY' || return 1
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
result=json.loads((p/"forged-add-rejected.stdout").read_text())
assert result.get("code")==999,result
message=result.get("msg","")
assert "code = FailedPrecondition desc = CNI continuity: " in message,result
before=json.loads((p/"forged-add-journal-before.json").read_text())
after=json.loads((p/"forged-add-journal-after.json").read_text())
assert before==after,"unauthorized ADD changed durable endpoint ownership"
PY
    interop_wait_uid_absent "${rollback_uid}" forged-add-rejected || return 1
    interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent forged-add-rejected || return 1
    docker exec "${WORKER}" ip -n "${rollback_ns}" -j link |
      tee "${EVIDENCE_DIR}/forged-add-netns.json" |
      python3 -c 'import json,sys; links=json.load(sys.stdin); assert [v["ifname"] for v in links]==["lo"],links' || return 1
    docker exec "${WORKER}" ip netns del "${rollback_ns}" || return 1
    echo "unpublished fake Pod/sandbox ADD intentionally BLOCKED with unchanged journal; real replacement ADD follows"
    return 0
  fi
  interop_replay_cni_operation "${worker_call}" ADD \
    "${EVIDENCE_DIR}/rollback-add-retry" success "${rollback_netns}" \
    "${rollback_container}" || return 1
  python3 - "${local_call}/config" "${EVIDENCE_DIR}/rollback-add-retry.stdout" \
    >"${EVIDENCE_DIR}/rollback-add-result-verification.json" <<'PY' || return 1
import ipaddress
import json
import pathlib
import sys

config = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
result = json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
expected_mac = config["macAddress"].lower()
expected_ip = str(ipaddress.ip_interface(config["ipAddress"]).ip)
assert [item.get("mac", "").lower() for item in result["interfaces"]
        if item.get("name") == "net1"] == [expected_mac], result
assert [str(ipaddress.ip_interface(item["address"]).ip) for item in result["ips"]
        if item.get("interface") == 0] == [expected_ip], result
print(json.dumps({"status": "PASS", "mac": expected_mac, "ip": expected_ip},
                 sort_keys=True))
PY
  cp "${EVIDENCE_DIR}/rollback-add-retry.stdout" "${local_call}/stdout" || return 1
  printf '0\n' >"${local_call}/rc" || return 1
  cp "${EVIDENCE_DIR}/rollback-add-retry.stderr" "${local_call}/stderr" || return 1
  tar -C "${local_call}" -cf - stdout rc stderr |
    docker exec -i "${WORKER}" tar -C "${worker_call}" -xf - || return 1
  interop_wait_endpoint_bound "${rollback_uid}" rollback-retry \
    "${local_call}" pod-rollback || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present rollback-retry || return 1
  interop_assert_cni_check "${worker_call}" "${EVIDENCE_DIR}/rollback-cni-check" \
    "${rollback_netns}" "${rollback_container}" || return 1
  interop_replay_cni_del "${worker_call}" \
    "${EVIDENCE_DIR}/rollback-cni-del.txt" "${rollback_container}" || return 1
  interop_wait_uid_absent "${rollback_uid}" rollback-cleanup || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent rollback-cleanup || return 1
  docker exec "${WORKER}" ip netns del "${rollback_ns}" || return 1
  echo "failed ADD rollback, valid retry, CHECK, and cleanup=PASS"
}

interop_verify_captured_traffic() {
  local phase="$1" forward_rc=0 reverse_rc=0 forward_start forward_end reverse_start reverse_end
  local forward_file="${EVIDENCE_DIR}/${phase}-local-to-remote-ping.txt"
  local reverse_file="${EVIDENCE_DIR}/${phase}-remote-to-local-ping.txt"
  [[ "${phase}" == initial ]] || interop_capture_discover "${phase}" || return 1
  interop_capture_start "${phase}" || return 1

  interop_ping_command forward
  forward_start="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if "${INTEROP_PING_COMMAND[@]}" 2>&1 | tee "${forward_file}"; then
    forward_rc=0
  else
    forward_rc=$?
  fi
  forward_end="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  interop_capture_assert_active "${phase}-after-forward-ping" || return 1

  interop_ping_command reverse
  reverse_start="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if "${INTEROP_PING_COMMAND[@]}" 2>&1 | tee "${reverse_file}"; then
    reverse_rc=0
  else
    reverse_rc=$?
  fi
  reverse_end="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  interop_capture_assert_active "${phase}-after-reverse-ping" || return 1

  python3 - "${forward_file}" "${reverse_file}" "${forward_rc}" "${reverse_rc}" \
    "${forward_start}" "${forward_end}" "${reverse_start}" "${reverse_end}" \
    "${EVIDENCE_DIR}/packet-path-${phase}-traffic.json" <<'PY'
import datetime
import json
import pathlib
import re
import sys

forward, reverse, forward_rc, reverse_rc, fs, fe, rs, re_, output = sys.argv[1:]
def inspect(path, rc, started, completed):
    text = pathlib.Path(path).read_text(errors="replace")
    match = re.search(r"(\d+) packets transmitted,\s*(\d+) (?:packets )?received,\s*(\d+)% packet loss", text)
    if not match:
        raise AssertionError(f"missing ping summary: {path}")
    sent, received, loss = map(int, match.groups())
    return {
        "exitStatus": int(rc),
        "startedAt": started,
        "completedAt": completed,
        "transmitted": sent,
        "received": received,
        "lossPercent": loss,
        "status": "PASS" if int(rc) == 0 and sent == received == 3 and loss == 0 else "FAIL",
    }

result = {
    "forward": inspect(forward, forward_rc, fs, fe),
    "reverse": inspect(reverse, reverse_rc, rs, re_),
}
pathlib.Path(output).write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
assert all(value["status"] == "PASS" for value in result.values()), result
manifest = pathlib.Path(output).with_name(f"packet-path-{pathlib.Path(output).name.split('-traffic.json')[0].removeprefix('packet-path-')}-manifest.json")
data = json.loads(manifest.read_text())
data["stimuli"] = {
    label: {
        "startedAt": datetime.datetime.fromisoformat(item["startedAt"]).timestamp(),
        "completedAt": datetime.datetime.fromisoformat(item["completedAt"]).timestamp(),
    }
    for label, item in (
        ("pod-to-frr", result["forward"]),
        ("frr-to-pod", result["reverse"]),
    )
}
manifest.write_text(json.dumps(data, indent=2) + "\n")
print("replacement bidirectional traffic=" + json.dumps(result, sort_keys=True))
PY
  interop_capture_stop || return 1
}

interop_prepare_no_replacement_lifecycle() {
  local uid_a uid_b
  copy_calls "${EVIDENCE_DIR}/initial-cni" || return 1
  uid_a="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')" || return 1
  uid_b="$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')" || return 1
  [[ -n "${uid_a}" && -n "${uid_b}" && "${uid_a}" != "${uid_b}" ]] || {
    echo "expected two distinct initial workload Pod UIDs" >&2
    return 1
  }
  interop_capture_endpoint_identities "${uid_a}" "${uid_b}" || return 1
  printf '%s\n' "${uid_a}" >"${EVIDENCE_DIR}/initial-pod-a-uid.txt"
  printf '%s\n' "${uid_b}" >"${EVIDENCE_DIR}/initial-pod-b-uid.txt"
  echo "initial CNI receipts and NB/SB/OVS endpoint identities=PASS"
}

interop_wait_endpoint_route_transition() {
  local deadline=$((SECONDS+90)) attempt=0 rc gobgp_file frr_file
  local tool="${ROOT}/tools/lib/cni_evpn_acceptance.py"
  while (( SECONDS < deadline )); do
    gobgp_file="acceptance-gobgp-route-transition-${attempt}.txt"
    frr_file="acceptance-frr-route-transition-${attempt}.txt"
    interop_capture_diagnostic "${gobgp_file}" \
      "kubectl exec GoBGP provider Pod ${PROVIDER_POD} in k8gobgp-system" required \
      timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
      -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgp global rib -a evpn || return 1
    interop_capture_diagnostic "${frr_file}" "docker exec FRR ${FRR_CONTAINER}" required \
      timeout --kill-after=2s 15s docker exec "${FRR_CONTAINER}" \
      vtysh -c 'show bgp l2vpn evpn route detail' || return 1
    if python3 "${tool}" verify-route-transition \
      --deleted "${EVIDENCE_DIR}/endpoint-b-identity.json" \
      --survivor "${EVIDENCE_DIR}/endpoint-a-identity.json" \
      --gobgp "${EVIDENCE_DIR}/${gobgp_file}" \
      --frr "${EVIDENCE_DIR}/${frr_file}" \
      --peer "${LOCAL_VTEP}" \
      --remote-rd "${FRR_PEER}:${VNI}" \
      --remote-mac "${REMOTE_MAC}" --remote-ip "${REMOTE_IP}" \
      --remote-vtep "${FRR_VTEP}" --vni "${VNI}" \
      >"${EVIDENCE_DIR}/route-transition-${attempt}.json"; then
      cp "${EVIDENCE_DIR}/${gobgp_file}" "${EVIDENCE_DIR}/gobgp-rib-pod-b-withdrawal.txt"
      cp "${EVIDENCE_DIR}/${frr_file}" "${EVIDENCE_DIR}/frr-rib-pod-b-withdrawal.txt"
      echo "exact deleted-Pod Type-2 withdrawal and survivor routes=PASS"
      return 0
    else
      rc=$?
      if (( rc != 1 )); then
        cat "${EVIDENCE_DIR}/route-transition-${attempt}.json" >&2 || true
        echo "route evidence is invalid; refusing to infer withdrawal" >&2
        return 1
      fi
    fi
    ((attempt+=1))
    sleep 1
  done
  echo "Type-2 deleted/survivor route transition timed out after ${attempt} samples" >&2
  return 1
}

interop_find_cni_call() {
  local uid="$1" command="$2"
  docker exec "${WORKER}" sh -ec '
    found=""
    for d in /run/evpn-cni-evidence/call.*; do
      [ -d "$d" ] || continue
      grep -qx "CNI_COMMAND=$2" "$d/env" || continue
      grep -Fq "K8S_POD_UID=$1" "$d/env" || continue
      [ "$(cat "$d/rc")" = 0 ] || continue
      if [ -n "$found" ]; then
        echo "multiple successful $2 calls found for UID $1" >&2
        exit 1
      fi
      found="$d"
    done
    [ -n "$found" ] || {
      echo "no successful $2 call found for UID $1" >&2
      exit 1
    }
    printf "%s\n" "$found"
  ' sh "${uid}" "${command}"
}

interop_assert_successful_cni_del() {
  local uid="$1" output="$2"
  docker exec "${WORKER}" sh -ec '
    found=""
    failures=""
    for d in /run/evpn-cni-evidence/call.*; do
      [ -d "$d" ] || continue
      grep -qx "CNI_COMMAND=DEL" "$d/env" || continue
      grep -Fq "K8S_POD_UID=$1" "$d/env" || continue
      rc="$(cat "$d/rc")"
      if [ "$rc" = 0 ]; then
        found="$d"
      else
        failures="${failures}${d}=${rc} "
      fi
    done
    [ -n "$found" ] || {
      echo "no successful runtime CNI DEL found for UID $1; failures: ${failures:-none}" >&2
      exit 1
    }
    [ -z "$failures" ] || echo "earlier CNI DEL failures for UID $1: $failures" >&2
    cat "$found/env"
  ' sh "${uid}" >"${output}"
}

interop_replay_cni_del() {
  local call="$1" output="$2" container_override="${3:-}"
  docker exec "${WORKER}" sh -ec '
    while IFS= read -r line; do export "$line"; done < "$1/env"
    export CNI_COMMAND=DEL
    export CNI_NETNS=""
    [ -z "$2" ] || export CNI_CONTAINERID="$2"
    if [ -n "$2" ]; then
      exec /opt/cni/bin/evpn-cni.real < "$1/config"
    fi
    exec /opt/cni/bin/evpn-cni < "$1/config"
  ' sh "${call}" "${container_override}" | tee "${output}"
}

interop_replay_cni_operation() {
  local call="$1" command="$2" evidence="$3" expected="$4"
  local netns_override="${5:-}" container_override="${6:-}" rc
  if docker exec "${WORKER}" sh -ec '
    call="$1"
    while IFS= read -r line; do export "$line"; done < "${call}/env"
    export CNI_COMMAND="$2"
    [ -z "$3" ] || export CNI_NETNS="$3"
    [ -z "$4" ] || export CNI_CONTAINERID="$4"
    exec /opt/cni/bin/evpn-cni.real < "${call}/config"
  ' sh "${call}" "${command}" "${netns_override}" "${container_override}" \
    >"${evidence}.stdout" 2>"${evidence}.stderr"; then
    rc=0
  else
    rc=$?
  fi
  printf '%s\n' "${rc}" >"${evidence}.exit-status"
  if [[ "${expected}" == success && "${rc}" != 0 ]]; then
    echo "CNI ${command} replay failed (status ${rc}): ${evidence}" >&2
    cat "${evidence}.stderr" >&2
    return 1
  fi
  if [[ "${expected}" == failure && "${rc}" == 0 ]]; then
    echo "CNI ${command} replay unexpectedly succeeded: ${evidence}" >&2
    return 1
  fi
}

interop_assert_cni_result_matches() {
  local original_call="$1" replay_output="$2" output="${3}"
  python3 - "${original_call}/stdout" "${replay_output}" <<'PY' >"${output}"
import json
import pathlib
import sys

original = json.loads(pathlib.Path(sys.argv[1]).read_text())
replayed = json.loads(pathlib.Path(sys.argv[2]).read_text())
assert original == replayed, {"original": original, "replayed": replayed}
print(json.dumps({"status": "PASS", "result": replayed}, indent=2, sort_keys=True))
PY
}

interop_capture_northd_health() {
  local phase="$1" process_file="${EVIDENCE_DIR}/ovn-processes-${1}.txt"
  local log_file="${EVIDENCE_DIR}/ovn-northd-log-${phase}.txt"
  local line_count
  rt ps -ef | tee "${process_file}" >/dev/null || return 1
  rt cat /var/log/ovn/ovn-northd.log | tee "${log_file}" >/dev/null || return 1
  python3 "${ROOT}/tools/lib/cni_evpn_acceptance.py" verify-runtime-health \
    --processes "${process_file}" --northd-log "${log_file}" \
    --baseline-pids "${EVIDENCE_DIR}/northd-pid-baseline.json" \
    >"${EVIDENCE_DIR}/ovn-process-health-${phase}.json" || return 1
  line_count="$(rt sh -ec 'wc -l < /var/log/ovn/ovn-northd.log')" || return 1
  [[ "${line_count}" =~ ^[0-9]+$ ]] || {
    echo "invalid northd log line count at ${phase}: ${line_count}" >&2
    return 1
  }
  printf '%s\n' "${line_count}" >"${EVIDENCE_DIR}/ovn-northd-log-lines-${phase}.txt"
  if [[ -n "${INTEROP_NORTHD_LAST_LOG_LINES:-}" ]]; then
    if (( line_count < INTEROP_NORTHD_LAST_LOG_LINES )); then
      echo "northd log shrank during acceptance; cannot prove continuous health" >&2
      return 1
    fi
  fi
  INTEROP_NORTHD_LAST_LOG_LINES="${line_count}"
}

interop_verify_final_ovn_cleanup() {
  local context="kubectl exec OVN runtime Pod ${OVN_POD} in cni-test"
  local -a identity_args=(
    --identity "${EVIDENCE_DIR}/endpoint-a-identity.json"
    --identity "${EVIDENCE_DIR}/endpoint-b-identity.json"
  )
  [[ ! -s "${EVIDENCE_DIR}/endpoint-a-replacement-identity.json" ]] ||
    identity_args+=(--identity "${EVIDENCE_DIR}/endpoint-a-replacement-identity.json")
  [[ ! -s "${EVIDENCE_DIR}/rollback-retry-identity.json" ]] ||
    identity_args+=(--identity "${EVIDENCE_DIR}/rollback-retry-identity.json")
  interop_capture_northd_health before-final-sb-convergence || return 1
  interop_capture_diagnostic "ovn-nb-sb-sync-final.txt" "${context}" status \
    timeout --kill-after=2s 90s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovn-nbctl --wait=sb sync || return 1
  interop_capture_diagnostic "cleanup-nb-lsps.json" "${context}" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=_uuid,name,external_ids list Logical_Switch_Port || return 1
  interop_capture_diagnostic "cleanup-sb-bindings.json" "${context}" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovn-sbctl --format=json --columns=_uuid,logical_port list Port_Binding || return 1
  interop_capture_diagnostic "cleanup-ovs-interfaces.json" "${context}" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,external_ids list Interface || return 1
  interop_capture_diagnostic "cleanup-ovs-ports.json" "${context}" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port || return 1
  interop_capture_diagnostic "cleanup-ovs-bridges.json" "${context}" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovs-vsctl --format=json --columns=name,ports list Bridge || return 1
  python3 "${ROOT}/tools/lib/cni_evpn_acceptance.py" verify-cleanup \
    "${identity_args[@]}" \
    --nb-lsps "${EVIDENCE_DIR}/cleanup-nb-lsps.json" \
    --sb-bindings "${EVIDENCE_DIR}/cleanup-sb-bindings.json" \
    --ovs-interfaces "${EVIDENCE_DIR}/cleanup-ovs-interfaces.json" \
    --ovs-ports "${EVIDENCE_DIR}/cleanup-ovs-ports.json" \
    --ovs-bridges "${EVIDENCE_DIR}/cleanup-ovs-bridges.json" \
    >"${EVIDENCE_DIR}/cleanup-owned-state.json" || return 1
  interop_capture_northd_health after-final-sb-convergence || return 1
  echo "exact NB LSP/SB Port_Binding/OVS cleanup and NB-SB convergence=PASS"
}

interop_verify_no_replacement_lifecycle() {
  local uid_a uid_b call_b call_a
  local primary_a primary_b

  uid_a="$(cat "${EVIDENCE_DIR}/initial-pod-a-uid.txt")" || return 1
  uid_b="$(cat "${EVIDENCE_DIR}/initial-pod-b-uid.txt")" || return 1
  [[ -n "${uid_a}" && -n "${uid_b}" && "${uid_a}" != "${uid_b}" ]]
  [[ "$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')" == "${uid_a}" ]] || {
    echo "pod-a UID changed after initial evidence validation" >&2
    return 1
  }
  [[ "$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')" == "${uid_b}" ]] || {
    echo "pod-b UID changed after initial evidence validation" >&2
    return 1
  }
  call_b="$(interop_find_cni_call "${uid_b}" ADD)" || return 1
  call_a="$(interop_find_cni_call "${uid_a}" ADD)" || return 1
  primary_a="$(k -n cni-test get pod pod-a -o jsonpath='{.status.podIP}')"
  primary_b="$(k -n cni-test get pod pod-b -o jsonpath='{.status.podIP}')"
  [[ -n "${primary_a}" && -n "${primary_b}" ]]
  k -n cni-test exec pod-a -- ping -I eth0 -c 2 -W 2 "${primary_b}" \
    | tee "${EVIDENCE_DIR}/pod-a-to-pod-b-primary-ping.txt"
  k -n cni-test exec pod-b -- ping -I eth0 -c 2 -W 2 "${primary_a}" \
    | tee "${EVIDENCE_DIR}/pod-b-to-pod-a-primary-ping.txt"
  k -n cni-test exec pod-a -- ping -I net1 -c 3 -W 2 10.244.240.11 \
    | tee "${EVIDENCE_DIR}/pod-a-to-pod-b-secondary-ping.txt"
  k -n cni-test exec pod-b -- ping -I net1 -c 3 -W 2 10.244.240.10 \
    | tee "${EVIDENCE_DIR}/pod-b-to-pod-a-secondary-ping.txt"

  interop_capture_northd_health before-pod-b-delete || return 1
  k -n cni-test delete pod pod-b --wait=true --timeout=120s \
    | tee "${EVIDENCE_DIR}/pod-b-delete.txt" || return 1
  interop_wait_uid_absent "${uid_b}" pod-b-deleted || return 1
  interop_assert_successful_cni_del "${uid_b}" \
    "${EVIDENCE_DIR}/runtime-del-pod-b.txt" || return 1
  interop_replay_cni_del "${call_b}" "${EVIDENCE_DIR}/pod-b-del-replay-1.txt" || return 1
  interop_replay_cni_del "${call_b}" "${EVIDENCE_DIR}/pod-b-del-replay-2.txt" || return 1
  interop_wait_endpoint_route_transition || return 1
  k -n cni-test exec pod-a -- ip addr show net1 \
    | tee "${EVIDENCE_DIR}/pod-a-retained-after-pod-b-delete.txt" >/dev/null
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present pod-b-withdrawal || return 1
  interop_wait_import_count 1 pod-b-withdrawal || return 1
  k -n cni-test exec pod-a -- ping -I net1 -c 2 -W 2 "${REMOTE_IP}" \
    | tee "${EVIDENCE_DIR}/pod-a-after-pod-b-delete-ping.txt" || return 1
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 \
    -c 2 -W 2 "${LOCAL_IP}" \
    | tee "${EVIDENCE_DIR}/remote-after-pod-b-delete-ping.txt" || return 1
  interop_capture_northd_health after-pod-b-delete || return 1

  LOCAL_MAC="$(k -n cni-test exec pod-a -- cat /sys/class/net/net1/address)"
  interop_capture_local_uuids
  printf '%s\n' "${uid_a}" >"${EVIDENCE_DIR}/pod-a-uid-before-withdrawal.txt"
  interop_capture_northd_health before-pod-a-delete || return 1

  if docker exec "${WORKER}" bridge -j fdb show dev "${VXLAN_DEVICE}" \
      >"${EVIDENCE_DIR}/foreign-fdb-before.json" &&
     docker exec "${WORKER}" ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}" \
      >"${EVIDENCE_DIR}/foreign-neigh-before.json"; then
    python3 - "${EVIDENCE_DIR}/foreign-fdb-before.json" \
      "${EVIDENCE_DIR}/foreign-neigh-before.json" <<'PY'
import json,sys
fdb=json.load(open(sys.argv[1]))
neigh=json.load(open(sys.argv[2]))
mac="02:00:00:00:fa:01"
ip="10.244.240.250"
assert not any(row.get("mac","").lower()==mac for row in fdb),fdb
assert not any(row.get("dst")==ip for row in neigh),neigh
PY
  else
    echo "failed to capture worker forwarding state before local withdrawal" >&2
    return 1
  fi
  rt ovn-nbctl get Logical_Switch_Port "${FOREIGN_LSP}" _uuid \
    >"${EVIDENCE_DIR}/foreign-lsp-before.txt"
  docker exec "${WORKER}" bridge fdb add 02:00:00:00:fa:01 dev "${VXLAN_DEVICE}" \
    dst "${FRR_VTEP}" self permanent
  docker exec "${WORKER}" ip neigh add 10.244.240.250 lladdr 02:00:00:00:fa:01 \
    dev "${BRIDGE_DEVICE}" nud permanent
  docker exec "${WORKER}" bridge -j fdb show dev "${VXLAN_DEVICE}" \
    >"${EVIDENCE_DIR}/foreign-fdb-installed.json"
  docker exec "${WORKER}" ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}" \
    >"${EVIDENCE_DIR}/foreign-neigh-installed.json"

  k -n cni-test delete pod pod-a --wait=true --timeout=90s \
    | tee "${EVIDENCE_DIR}/local-delete.txt" || return 1
  interop_wait_uid_absent "${uid_a}" local-endpoint-deleted || return 1
  interop_assert_successful_cni_del "${uid_a}" \
    "${EVIDENCE_DIR}/runtime-del-pod-a.txt" || return 1
  interop_wait_local_uuid_withdrawal || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent local-withdrawal || return 1
  interop_wait_local_kernel_withdrawal || return 1
  interop_replay_cni_del "${call_a}" "${EVIDENCE_DIR}/pod-a-del-replay.txt" || return 1
  interop_capture_northd_health after-pod-a-withdrawal || return 1
  docker exec "${WORKER}" bridge -j fdb show dev "${VXLAN_DEVICE}" \
    >"${EVIDENCE_DIR}/worker-fdb-after-local-withdrawal.json"
  docker exec "${WORKER}" ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}" \
    >"${EVIDENCE_DIR}/worker-neigh-after-local-withdrawal.json"

  if docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 \
      -c 1 -W 1 "${LOCAL_IP}" >"${EVIDENCE_DIR}/remote-to-deleted-local-ping.txt" 2>&1; then
    echo "deleted local endpoint remained reachable" >&2
    return 1
  fi
  echo 'local endpoint withdrawal=PASS'

  docker exec "${FRR_CONTAINER}" ip netns del evpnwork
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" absent remote-withdrawal || return 1
  interop_wait_import_count 0 remote-withdrawal || return 1
  interop_assert_remote_kernel_withdrawal || return 1
  interop_verify_final_ovn_cleanup || return 1
  for _ in $(seq 1 30); do
    rt ovs-ofctl dump-flows br-int >"${EVIDENCE_DIR}/ovs-flows-after-withdrawal.txt" || return 1
    if ! grep -qi "${REMOTE_MAC}" "${EVIDENCE_DIR}/ovs-flows-after-withdrawal.txt"; then
      break
    fi
    sleep 1
  done
  if grep -qi "${REMOTE_MAC}" "${EVIDENCE_DIR}/ovs-flows-after-withdrawal.txt"; then
    echo "remote endpoint remains in OVS flows after normal withdrawal" >&2
    return 1
  fi
  python3 - "${EVIDENCE_DIR}/foreign-fdb-installed.json" \
    "${EVIDENCE_DIR}/foreign-neigh-installed.json" \
    "${EVIDENCE_DIR}/worker-fdb-after-local-withdrawal.json" \
    "${EVIDENCE_DIR}/worker-neigh-after-local-withdrawal.json" <<'PY'
import json,sys
before_fdb,before_neigh,after_fdb,after_neigh=(json.load(open(path)) for path in sys.argv[1:])
mac="02:00:00:00:fa:01"
ip="10.244.240.250"
before_selected_fdb=[row for row in before_fdb if row.get("mac","").lower()==mac]
after_selected_fdb=[row for row in after_fdb if row.get("mac","").lower()==mac]
before_selected_neigh=[row for row in before_neigh if row.get("dst")==ip]
after_selected_neigh=[row for row in after_neigh if row.get("dst")==ip]
assert len(before_selected_fdb)==1 and before_selected_fdb==after_selected_fdb
assert len(before_selected_neigh)==1 and before_selected_neigh==after_selected_neigh
print("unrelated FDB and neighbor preserved=PASS")
PY
  rt ovn-nbctl get Logical_Switch_Port "${FOREIGN_LSP}" _uuid \
    >"${EVIDENCE_DIR}/foreign-lsp-after.txt"
  cmp "${EVIDENCE_DIR}/foreign-lsp-before.txt" "${EVIDENCE_DIR}/foreign-lsp-after.txt"
  interop_capture_northd_health after-remote-withdrawal || return 1
  if [[ -n "$(k -n cni-test get pod pod-a pod-b --ignore-not-found -o name)" ]]; then
    echo "workload Pods remain after no-replacement lifecycle" >&2
    return 1
  fi
  echo "no-replacement Type-2 lifecycle=PASS"
}

interop_binding_diagnostic() {
  local old_uid new_uid delete_rc binding_rc binding_state
  local poll_pid
  local started_at
  started_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if [[ ! -e "${EVIDENCE_DIR}/diagnostic-preflight.status" ]]; then
    interop_diagnostic_preflight || {
      echo "diagnostic positive-control preflight failed; replacement lifecycle not started" >&2
      return 1
    }
  fi
  old_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  [[ -n "${old_uid}" ]]
  interop_capture_binding_snapshot initial "${old_uid}"
  interop_capture_binding_routes initial

  docker exec "${WORKER}" systemctl stop kubelet
  if k -n cni-test delete pod pod-a --grace-period=0 --force --wait=true --timeout=60s; then
    delete_rc=0
  else
    delete_rc=$?
  fi
  if ! docker exec "${WORKER}" systemctl start kubelet; then
    echo "failed to restart kubelet on disposable worker ${WORKER}" >&2
    return 1
  fi
  (( delete_rc == 0 )) || return "${delete_rc}"
  interop_wait_uid_absent "${old_uid}"
  interop_capture_binding_snapshot deleted "${old_uid}"

  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  new_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  [[ -n "${new_uid}" && "${new_uid}" != "${old_uid}" ]]
  interop_capture_binding_snapshot replacement-add "${new_uid}"
  interop_capture_binding_routes replacement-add

  interop_wait_replacement_binding "${new_uid}" \
    >"${EVIDENCE_DIR}/replacement-binding-poll.log" 2>&1 &
  poll_pid=$!
  sleep 30
  local observation_rc=0
  interop_capture_binding_snapshot replacement-30s "${new_uid}" || observation_rc=1
  interop_capture_binding_routes replacement-30s || observation_rc=1
  if wait "${poll_pid}"; then
    binding_rc=0
    binding_state=BOUND
  else
    binding_rc=$?
    binding_state="$(python3 - "${EVIDENCE_DIR}/replacement-binding-timeline.jsonl" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
rows = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
print(rows[-1]["state"] if rows else "NO_BINDING_OBSERVATION")
PY
)"
  fi
  (( observation_rc == 0 )) || {
    echo "required during-poll evidence failed; diagnostic cannot pass" >&2
    return 1
  }
  interop_capture_binding_snapshot replacement-final "${new_uid}"
  interop_capture_binding_routes replacement-final

  printf '%s\n' "${binding_rc}" >"${EVIDENCE_DIR}/replacement-binding-poll.exit-status"
  python3 - "${EVIDENCE_DIR}/binding-diagnostic-summary.json" \
    "${started_at}" "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
    "${old_uid}" "${new_uid}" "${binding_state}" "${binding_rc}" <<'PY'
import json
import sys

path, started, completed, old_uid, new_uid, state, rc = sys.argv[1:]
with open(path, "w", encoding="utf-8") as output:
    json.dump({
        "startedAt": started,
        "completedAt": completed,
        "initialWorkloadUID": old_uid,
        "replacementWorkloadUID": new_uid,
        "replacementBindingState": state,
        "bindingPollExitStatus": int(rc),
        "fullForwardingAcceptanceRun": False,
    }, output, indent=2, sort_keys=True)
    output.write("\n")
PY

  docker exec "${WORKER}" systemctl stop kubelet
  if k -n cni-test delete pod pod-a --grace-period=0 --force --wait=true --timeout=60s; then
    delete_rc=0
  else
    delete_rc=$?
  fi
  if ! docker exec "${WORKER}" systemctl start kubelet; then
    echo "failed to restart kubelet on disposable worker ${WORKER}" >&2
    return 1
  fi
  (( delete_rc == 0 )) || return "${delete_rc}"
  interop_wait_uid_absent "${new_uid}"
  interop_capture_binding_snapshot cleanup "${new_uid}"
  interop_capture_binding_routes cleanup
  echo "replacement-binding-diagnostic=${binding_state} old_uid=${old_uid} new_uid=${new_uid}"
}

interop_verify_full_lifecycle() {
  local uid_a uid_b call_a call_a_copy call_b call_b_copy replacement_uid replacement_call replacement_call_copy
  uid_a="$(cat "${EVIDENCE_DIR}/initial-pod-a-uid.txt")" || return 1
  uid_b="$(cat "${EVIDENCE_DIR}/survivor-pod-b-uid.txt")" || return 1
  call_a="$(interop_find_cni_call "${uid_a}" ADD)" || return 1
  call_a_copy="$(interop_find_copied_cni_call "${uid_a}" ADD)" || return 1
  call_b="$(interop_find_cni_call "${uid_b}" ADD)" || return 1
  call_b_copy="$(interop_find_copied_cni_call "${uid_b}" ADD)" || return 1
  [[ "$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')" == "${uid_a}" ]] || {
    echo "initial Pod A identity changed before lifecycle deletion" >&2
    return 1
  }

  # Earlier snapshots can predate activation, speaker replacement or Pod A replacement.
  docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json \
    >"${EVIDENCE_DIR}/export-status-before-initial-delete.json" || return 1
  interop_capture_local_uuids "${EVIDENCE_DIR}/export-status-before-initial-delete.json" || return 1
  interop_capture_northd_health before-initial-cni-del || return 1
  k -n cni-test delete pod pod-a --wait=true --timeout=120s \
    | tee "${EVIDENCE_DIR}/initial-cni-del.txt" || return 1
  interop_wait_uid_absent "${uid_a}" initial-delete || return 1
  interop_assert_successful_cni_del "${uid_a}" \
    "${EVIDENCE_DIR}/initial-runtime-del.txt" || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent initial-delete || return 1
  interop_wait_local_uuid_withdrawal || return 1
  interop_wait_local_kernel_withdrawal || return 1
  interop_replay_cni_del "${call_a}" \
    "${EVIDENCE_DIR}/initial-cni-del-replay-1.txt" || return 1
  interop_replay_cni_del "${call_a}" \
    "${EVIDENCE_DIR}/initial-cni-del-replay-2.txt" || return 1
  interop_replay_cni_operation "${call_a}" CHECK \
    "${EVIDENCE_DIR}/negative-check-after-delete" failure || return 1
  python3 - "${EVIDENCE_DIR}/negative-check-after-delete.stdout" <<'PY' || return 1
import json
import sys

with open(sys.argv[1], encoding="utf-8") as output:
    result = json.load(output)
assert result.get("code") == 999, result
message=result.get("msg", "")
assert "not found" in message.lower() or message == (
    "node-agent Check RPC failed: rpc error: code = FailedPrecondition desc = "
    "no matching durable active endpoint ACK"), result
PY
  interop_wait_uid_absent "${uid_a}" negative-check-no-recreate || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent negative-check-no-recreate || return 1
  echo "initial CNI DEL, withdrawal, repeated DEL, and negative CHECK=PASS"

  interop_assert_survivor "${uid_b}" "${call_b_copy}" after-initial-delete || return 1
  interop_run_failed_add_rollback "${call_a_copy}" || return 1
  interop_assert_survivor "${uid_b}" "${call_b_copy}" after-failed-add-rollback || return 1

  interop_capture_northd_health before-replacement-add || return 1
  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  replacement_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')" || return 1
  [[ -n "${replacement_uid}" && "${replacement_uid}" != "${uid_a}" ]] || {
    echo "replacement ADD did not receive a new Pod UID" >&2
    return 1
  }
  printf '%s\n' "${replacement_uid}" >"${EVIDENCE_DIR}/replacement-pod-a-uid.txt"
  interop_wait_uid_absent "${uid_a}" stale-initial-ownership-after-replacement || return 1
  interop_wait_replacement_binding "${replacement_uid}" || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present replacement-add || return 1
  interop_wait_import_count 1 replacement-add || return 1
  replacement_call="$(interop_find_cni_call "${replacement_uid}" ADD)" || return 1
  copy_calls "${EVIDENCE_DIR}/replacement-cni" || return 1
  replacement_call_copy="$(interop_find_copied_cni_call "${replacement_uid}" ADD \
    "${EVIDENCE_DIR}/replacement-cni")" || return 1
  interop_capture_lifecycle_identity "${replacement_uid}" "${replacement_call_copy}" \
    endpoint-a-replacement pod-a || return 1
  interop_assert_cni_check "${replacement_call}" \
    "${EVIDENCE_DIR}/replacement-cni-check" || return 1
  interop_assert_single_owned_endpoint "${replacement_uid}" replacement-add || return 1
  echo "replacement CNI ADD, NB/SB/OVS binding, Type-2, and CHECK=PASS"

  interop_verify_captured_traffic replacement || return 1
  interop_assert_survivor "${uid_b}" "${call_b_copy}" after-replacement-add || return 1

  interop_capture_northd_health before-replacement-cni-del || return 1
  docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json \
    >"${EVIDENCE_DIR}/export-status-before-replacement-delete.json" || return 1
  interop_capture_local_uuids \
    "${EVIDENCE_DIR}/export-status-before-replacement-delete.json" \
    "${EVIDENCE_DIR}/replacement-local-path-uuids.json" || return 1
  k -n cni-test delete pod pod-a --wait=true --timeout=120s \
    | tee "${EVIDENCE_DIR}/replacement-cni-del.txt" || return 1
  interop_wait_uid_absent "${replacement_uid}" replacement-delete || return 1
  interop_assert_successful_cni_del "${replacement_uid}" \
    "${EVIDENCE_DIR}/replacement-runtime-del.txt" || return 1
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent replacement-delete || return 1
  interop_wait_local_uuid_withdrawal \
    "${EVIDENCE_DIR}/replacement-local-path-uuids.json" replacement-delete || return 1
  interop_wait_local_kernel_withdrawal || return 1
  interop_replay_cni_del "${replacement_call}" \
    "${EVIDENCE_DIR}/replacement-cni-del-replay.txt" || return 1
  interop_assert_survivor "${uid_b}" "${call_b_copy}" after-replacement-delete || return 1
  echo "replacement CNI DEL, route withdrawal, and repeated DEL=PASS"

  interop_capture_northd_health before-survivor-cni-del || return 1
  k -n cni-test delete pod pod-b --wait=true --timeout=120s \
    | tee "${EVIDENCE_DIR}/survivor-cni-del.txt" || return 1
  interop_wait_uid_absent "${uid_b}" survivor-delete || return 1
  interop_assert_successful_cni_del "${uid_b}" \
    "${EVIDENCE_DIR}/survivor-runtime-del.txt" || return 1
  interop_wait_rib_route "${SURVIVOR_MAC}" "${SURVIVOR_IP}" absent survivor-delete || return 1
  interop_replay_cni_del "${call_b}" \
    "${EVIDENCE_DIR}/survivor-cni-del-replay.txt" || return 1
  docker exec "${FRR_CONTAINER}" ip netns del evpnwork
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" absent remote-withdrawal || return 1
  interop_wait_import_count 0 remote-withdrawal || return 1
  interop_assert_remote_kernel_withdrawal || return 1
  interop_verify_final_ovn_cleanup || return 1

  interop_capture_northd_health before-network-delete || return 1
  interop_capture_diagnostic "test-network-before-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovn-nbctl get Logical_Switch ls24b _uuid || return 1
  interop_capture_diagnostic "shared-bridge-before-network-delete.json" \
    "kubectl exec node-agent Pod ${AGENT_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${AGENT_POD}" -- ip -j link show dev "${BRIDGE_DEVICE}" || return 1
  interop_capture_diagnostic "shared-vxlan-before-network-delete.json" \
    "kubectl exec node-agent Pod ${AGENT_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${AGENT_POD}" -- ip -j link show dev "${VXLAN_DEVICE}" || return 1
  interop_capture_diagnostic "br-int-before-network-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" status \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovs-vsctl br-exists br-int || return 1
  interop_capture_diagnostic "foreign-lsp-before-network-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovn-nbctl get Logical_Switch_Port "${FOREIGN_LSP}" _uuid || return 1
  interop_capture_diagnostic "test-network-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" status \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovn-nbctl ls-del ls24b || return 1
  interop_capture_diagnostic "logical-switches-after-network-delete.json" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- \
    ovn-nbctl --format=json --columns=name list Logical_Switch || return 1
  python3 - "${EVIDENCE_DIR}/logical-switches-after-network-delete.json" <<'PY' || return 1
import json
import sys

table = json.load(open(sys.argv[1], encoding="utf-8"))
names = [row[0] for row in table["data"]]
assert "ls24b" not in names, names
PY
  interop_capture_diagnostic "foreign-lsp-after-network-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" required \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovn-nbctl get Logical_Switch_Port "${FOREIGN_LSP}" _uuid || return 1
  cmp "${EVIDENCE_DIR}/foreign-lsp-before-network-delete.txt" \
    "${EVIDENCE_DIR}/foreign-lsp-after-network-delete.txt" || return 1
  interop_capture_diagnostic "shared-bridge-after-network-delete.json" \
    "kubectl exec node-agent Pod ${AGENT_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${AGENT_POD}" -- ip -j link show dev "${BRIDGE_DEVICE}" || return 1
  interop_capture_diagnostic "shared-vxlan-after-network-delete.json" \
    "kubectl exec node-agent Pod ${AGENT_POD} in cni-test" json \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${AGENT_POD}" -- ip -j link show dev "${VXLAN_DEVICE}" || return 1
  interop_capture_diagnostic "br-int-after-network-delete.txt" \
    "kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" status \
    timeout --kill-after=2s 15s kubectl --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -- ovs-vsctl br-exists br-int || return 1
  interop_capture_northd_health after-network-delete || return 1
  [[ -z "$(k -n cni-test get pod pod-a pod-b --ignore-not-found -o name)" ]] || {
    echo "CNI workloads remain after full lifecycle" >&2
    return 1
  }
  echo "survivor DEL, remote withdrawal, endpoint cleanup, and test-network cleanup=PASS"
}

interop_verify() {
  local local_rib frr_rib deadline=$((SECONDS+180))
  local forward_start forward_end reverse_start reverse_end
  local forward_rc=0 reverse_rc=0 ping_failed=0 capture_failed=0
  local forward_command reverse_command
  INTEROP_CONTROL_PLANE_STATUS=FAIL
  k -n cni-test get pods -o json | tee "${EVIDENCE_DIR}/interop-pods.json" >/dev/null
  LOCAL_MAC="$(k -n cni-test exec pod-a -- cat /sys/class/net/net1/address | tr '[:upper:]' '[:lower:]')"
  REMOTE_MAC="$(docker exec "${FRR_CONTAINER}" ip netns exec evpnwork cat /sys/class/net/frr-work0/address | tr '[:upper:]' '[:lower:]')"
  [[ "${LOCAL_MAC}" =~ ^([[:xdigit:]]{2}:){5}[[:xdigit:]]{2}$ ]]
  [[ "${REMOTE_MAC}" == '0a:58:f0:01:00:0c' ]]
  if [[ "${EVPN_FRR_INTEROP_INITIAL_ONLY:-0}" == 1 ]]; then
    python3 - "${EVIDENCE_DIR}/interop-pods.json" <<'PY'
import json,sys
pods=json.load(open(sys.argv[1])).get("items",[])
workloads=[pod for pod in pods if pod.get("metadata",{}).get("name","").startswith("pod-")]
assert len(workloads)==1 and workloads[0].get("metadata",{}).get("name")=="pod-a",workloads
print("initial-only-workload-pod-count=1")
PY
  fi
  docker exec "${WORKER}" ip -o -4 addr show | tee "${EVIDENCE_DIR}/worker-vtep.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn summary' | tee "${EVIDENCE_DIR}/frr-neighbor.txt"
  until provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null | grep -qi 'Established' &&
        local_rib="$(provider_exec gobgp global rib -a evpn 2>/dev/null)" &&
        frr_rib="$(docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route detail' 2>/dev/null)" &&
        grep -qi "${REMOTE_MAC}" <<<"${local_rib}" && grep -qi "${REMOTE_IP}" <<<"${local_rib}" &&
        grep -qi "${LOCAL_MAC}" <<<"${frr_rib}" && grep -q "${LOCAL_IP}" <<<"${frr_rib}"; do
    if (( SECONDS >= deadline )); then
      provider_exec gobgp neighbor "${FRR_PEER}" || true
      provider_exec gobgp global rib -a evpn || true
      docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route detail' || true
      echo "bidirectional Type-2 route exchange timed out" >&2
      INTEROP_FAILURE_CLASSIFICATION="BGP CONTROL-PLANE FAILURE"
      INTEROP_FAILURE_GATE="TYPE-2 ROUTE EXCHANGE"
      return 1
    fi
    sleep 2
  done
  provider_exec gobgp neighbor "${FRR_PEER}" | tee "${EVIDENCE_DIR}/gobgp-neighbor.txt"
  provider_exec gobgp global rib -a evpn | tee "${EVIDENCE_DIR}/gobgp-evpn-rib.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route detail' | tee "${EVIDENCE_DIR}/frr-evpn-rib.txt"
  docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/export-status.json" >/dev/null
  docker exec "${WORKER}" cat /run/evpn-node-agent/remote-status.json | tee "${EVIDENCE_DIR}/remote-status.json" >/dev/null
  interop_wait_import_count 1 initial-packet-path
  interop_assert_initial_route_scope
  INTEROP_CONTROL_PLANE_STATUS=PASS
  docker exec "${FRR_CONTAINER}" bridge -j fdb show | tee "${EVIDENCE_DIR}/frr-fdb.json" >/dev/null
  docker exec "${FRR_CONTAINER}" ip -4 -j neigh show nud all | tee "${EVIDENCE_DIR}/frr-neigh.json" >/dev/null
  ag ip -d -j link show | tee "${EVIDENCE_DIR}/worker-links.json" >/dev/null
  ag bridge -j fdb show | tee "${EVIDENCE_DIR}/worker-fdb.json" >/dev/null
  ag ip -4 -j neigh show nud all | tee "${EVIDENCE_DIR}/worker-neigh.json" >/dev/null
  rt ovs-vsctl show | tee "${EVIDENCE_DIR}/ovs-show.txt" >/dev/null
  rt ovs-ofctl dump-flows br-int | tee "${EVIDENCE_DIR}/ovs-flows.txt" >/dev/null
  k -n cni-test exec pod-a -- ip -d addr show dev net1 | tee "${EVIDENCE_DIR}/pod-a-net1.txt"
  docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ip -d addr show dev frr-work0 \
    | tee "${EVIDENCE_DIR}/remote-workload.txt" >/dev/null
  interop_capture_discover
  INTEROP_FORWARDING_STATE_STATUS=FAIL
  interop_assert_forwarding_state || {
    INTEROP_FAILURE_CLASSIFICATION="NETLINK INTEGRATION FAILURE"
    INTEROP_FAILURE_GATE="FORWARDING STATE"
    return 1
  }
  INTEROP_FORWARDING_STATE_STATUS=PASS
  if [[ "${EVPN_FRR_INTEROP_FULL_LIFECYCLE:-0}" == 1 ]]; then
    interop_prepare_full_lifecycle || {
      INTEROP_FAILURE_CLASSIFICATION="CNI LIFECYCLE FAILURE"
      INTEROP_FAILURE_GATE="INITIAL ADD/CHECK"
      return 1
    }
  fi
  INTEROP_CAPTURE_STARTUP_STATUS=FAIL
  interop_capture_start || {
    INTEROP_FAILURE_CLASSIFICATION="HARNESS FAILURE"
    INTEROP_FAILURE_GATE="CAPTURE STARTUP"
    return 1
  }
  interop_ping_command forward
  printf -v forward_command '%q ' "${INTEROP_PING_COMMAND[@]}"
  forward_start="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if "${INTEROP_PING_COMMAND[@]}" 2>&1 | tee "${EVIDENCE_DIR}/local-to-remote-ping.txt"; then
    forward_rc=0
  else
    forward_rc=$?
    ping_failed=1
  fi
  forward_end="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if ! interop_capture_assert_active after-forward-ping; then capture_failed=1; fi
  interop_ping_command reverse
  printf -v reverse_command '%q ' "${INTEROP_PING_COMMAND[@]}"
  reverse_start="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if "${INTEROP_PING_COMMAND[@]}" 2>&1 | tee "${EVIDENCE_DIR}/remote-to-local-ping.txt"; then
    reverse_rc=0
  else
    reverse_rc=$?
    ping_failed=1
  fi
  reverse_end="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
  if ! interop_capture_assert_active after-reverse-ping; then capture_failed=1; fi
  python3 - "${EVIDENCE_DIR}/local-to-remote-ping.txt" "${EVIDENCE_DIR}/remote-to-local-ping.txt" \
    "${EVIDENCE_DIR}/ping-results.json" "${forward_rc}" "${reverse_rc}" \
    "${forward_start}" "${forward_end}" "${reverse_start}" "${reverse_end}" \
    "${forward_command}" "${reverse_command}" "${EVIDENCE_DIR}/packet-path-manifest.json" <<'PY'
import datetime,json,re,sys
def result(path,rc,start,end,command):
    text=open(path,errors="replace").read()
    match=re.search(r"(\d+) packets transmitted,\s*(\d+) (?:packets )?received,\s*(\d+)% packet loss",text)
    transmitted,received,loss=map(int,match.groups()) if match else (None,None,None)
    return {"command":command,"startedAt":start,"completedAt":end,"exitStatus":int(rc),
            "transmitted":transmitted,"received":received,"lossPercent":loss,
            "status":"PASS" if int(rc)==0 and transmitted==3 and received==3 and loss==0 else "FAIL"}
data={"forward":result(sys.argv[1],sys.argv[4],sys.argv[6],sys.argv[7],sys.argv[10]),
      "reverse":result(sys.argv[2],sys.argv[5],sys.argv[8],sys.argv[9],sys.argv[11])}
json.dump(data,open(sys.argv[3],"w"),indent=2)
manifest=json.load(open(sys.argv[12]))
manifest["stimuli"]={
    label:{"startedAt":datetime.datetime.fromisoformat(item["startedAt"]).timestamp(),
           "completedAt":datetime.datetime.fromisoformat(item["completedAt"]).timestamp()}
    for label,item in zip(("pod-to-frr","frr-to-pod"),data.values())
}
json.dump(manifest,open(sys.argv[12],"w"),indent=2)
print("ping-results="+json.dumps(data,sort_keys=True))
PY
  read -r INTEROP_FORWARD_ICMP_STATUS INTEROP_REVERSE_ICMP_STATUS < <(
    python3 - "${EVIDENCE_DIR}/ping-results.json" <<'PY'
import json,sys
data=json.load(open(sys.argv[1]))
print(data["forward"]["status"],data["reverse"]["status"])
PY
  )
  if ! interop_capture_stop; then capture_failed=1; fi
  [[ "${ping_failed}" == 0 ]] || {
    echo 'bidirectional workload ICMP failed' >&2
    INTEROP_FAILURE_CLASSIFICATION="OVN/OVS FORWARDING FAILURE"
    INTEROP_FAILURE_GATE="BIDIRECTIONAL WORKLOAD TRAFFIC"
    return 1
  }
  [[ "${capture_failed}" == 0 ]] || {
    echo 'bidirectional packet-path verification incomplete' >&2
    INTEROP_FAILURE_CLASSIFICATION="UNRESOLVED"
    INTEROP_FAILURE_GATE="PACKET-PATH VERIFICATION"
    return 1
  }
  echo 'initial bidirectional BGP Type-2, remote forwarding state and ICMP=PASS'
  interop_run_lifecycle || {
    INTEROP_FAILURE_CLASSIFICATION="LIFECYCLE FAILURE"
    INTEROP_FAILURE_GATE="ENDPOINT LIFECYCLE"
    return 1
  }
}

interop_run_lifecycle() {
  if [[ "${EVPN_FRR_INTEROP_INITIAL_ONLY:-0}" == 1 ]]; then
    echo 'initial-only packet-path verification=PASS; endpoint lifecycle tests not run'
    return 0
  fi
  INTEROP_LIFECYCLE_STATUS=FAIL
  if [[ "${EVPN_FRR_INTEROP_NO_REPLACEMENT:-0}" == 1 ]]; then
    interop_prepare_no_replacement_lifecycle || return 1
    interop_verify_no_replacement_lifecycle || return 1
  elif [[ "${EVPN_FRR_INTEROP_FULL_LIFECYCLE:-0}" == 1 ]]; then
    interop_verify_full_lifecycle || return 1
  else
    interop_verify_lifecycle || return 1
  fi
  INTEROP_LIFECYCLE_STATUS=PASS
}
