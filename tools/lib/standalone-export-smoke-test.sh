#!/usr/bin/env bash
# Opt-in smoke scenario using the operator runner's already-provisioned topology.

standalone_smoke_copy_to_worker() {
  local source="$1" destination="$2"
  docker exec -i "${WORKER}" sh -ec \
    "umask 077; cat > '${destination}.next'; chown root:root '${destination}.next'; chmod 600 '${destination}.next'; mv '${destination}.next' '${destination}'" \
    <"${source}"
}

standalone_smoke_copy_to_agent() {
  local source="$1" destination="$2"
  k -n cni-test exec -i "${OVN_POD}" -c ovn-ovs -- sh -ec \
    "umask 077; mkdir -p \"\$(dirname '${destination}')\"; cat > '${destination}.next'; chown root:root '${destination}.next'; chmod 600 '${destination}.next'; mv '${destination}.next' '${destination}'" \
    <"${source}"
}

standalone_smoke_agent_exec() {
  k -n cni-test exec "${OVN_POD}" -c ovn-ovs -- "$@" </dev/null
}

standalone_smoke_capture_command() {
  local name="$1" rc=0
  shift
  printf '%q ' "$@" >"${EVIDENCE_DIR}/${name}.command"
  printf '\n' >>"${EVIDENCE_DIR}/${name}.command"
  "$@" >"${EVIDENCE_DIR}/${name}.stdout" \
    2>"${EVIDENCE_DIR}/${name}.stderr" || rc=$?
  printf '%s\n' "${rc}" >"${EVIDENCE_DIR}/${name}.exit-code"
  printf '%s\t%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${name}" "${rc}" \
    >>"${EVIDENCE_DIR}/standalone-exec-results.tsv"
  if (( rc != 0 )); then
    echo "standalone diagnostic ${name} failed (${rc}); see ${EVIDENCE_DIR}/${name}.stderr" >&2
  fi
  return "${rc}"
}

standalone_smoke_capture_diagnostics() {
  local rc=0
  standalone_smoke_capture_command standalone-speaker-process \
    standalone_smoke_agent_exec sh -ec '
      pid=$(cat /run/gobgp/gobgpd.pid)
      printf "pid=%s\n" "$pid"
      case "$pid" in ""|*[!0-9]*) exit 1;; esac
      kill -0 "$pid"
      cat "/proc/$pid/status"
      tr "\000" " " <"/proc/$pid/cmdline"
      printf "\n"
      readlink "/proc/$pid/exe"
      test -S /run/gobgp/gobgp.sock
    ' || rc=1
  standalone_smoke_capture_command standalone-gobgpd-log \
    standalone_smoke_agent_exec cat /run/gobgp/gobgpd.log || rc=1
  standalone_smoke_capture_command standalone-gobgp-global \
    provider_exec gobgp global || rc=1
  standalone_smoke_capture_command standalone-gobgp-neighbor \
    provider_exec gobgp neighbor "${FRR_PEER}" || rc=1
  standalone_smoke_capture_command standalone-agent-log \
    standalone_smoke_agent_exec cat /run/evpn-node-agent/standalone-smoke/agent.log || rc=1
  standalone_smoke_capture_command standalone-agent-startup \
    standalone_smoke_agent_exec sh -ec '
      if test -f /run/evpn-node-agent/standalone-smoke/agent.pid; then
        pid=$(cat /run/evpn-node-agent/standalone-smoke/agent.pid)
        printf "pid=%s\n" "$pid"
        cat "/proc/$pid/status"
      else
        echo "Node-Agent launch not reached"
      fi
    ' || rc=1
  return "${rc}"
}

standalone_smoke_read_status() {
  standalone_smoke_agent_exec cat /run/evpn-node-agent/export-status.json \
    >"${EVIDENCE_DIR}/standalone-export-status.json"
}

standalone_smoke_wait_status() {
  local desired="$1" deadline=$((SECONDS+120))
  while (( SECONDS < deadline )); do
    if standalone_smoke_read_status &&
      python3 - "${EVIDENCE_DIR}/standalone-export-status.json" "${desired}" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
observations=status.get("Observations") or []
applied=(status.get("Result") or {}).get("Applied") or []
want=sys.argv[2]
if status.get("Error"):
    sys.exit(1)
if want == "advertised":
    sys.exit(0 if len(applied)==1 and len(observations)==1 and observations[0].get("State")=="eligible" else 1)
if want == "withdrawn":
    sys.exit(0 if not applied and len(observations)==1 and observations[0].get("State")=="ineligible" else 1)
raise SystemExit("unknown expected status")
PY
    then
      return 0
    fi
    sleep 1
  done
  cat "${EVIDENCE_DIR}/standalone-export-status.json" >&2 || true
  echo "standalone exporter did not reach expected ${desired} state" >&2
  return 1
}

standalone_smoke_assert_cni_export_dry_run() {
  docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json \
    >"${EVIDENCE_DIR}/cni-export-dry-run.json"
  python3 - "${EVIDENCE_DIR}/cni-export-dry-run.json" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
result=status.get("Result") or {}
assert result.get("DryRun") is True,result
assert not result.get("Applied"),result
PY
}

standalone_smoke_wait_cni_socket_absent() {
  local deadline=$((SECONDS+30))
  while (( SECONDS < deadline )); do
    if docker exec "${WORKER}" test ! -S /run/evpn-node-agent/agent.sock; then
      return 0
    fi
    sleep 1
  done
  echo "operator Node-Agent CNI socket remained after its DaemonSet stopped" >&2
  docker exec "${WORKER}" ls -l /run/evpn-node-agent/agent.sock >&2 || true
  return 1
}

standalone_smoke_configure_speaker() {
  cat >"${EVIDENCE_DIR}/standalone-gobgpd.yaml" <<EOF
global:
  config:
    as: 64512
    router-id: "${LOCAL_VTEP}"
    port: 179
    local-address-list: ["${LOCAL_VTEP}"]
neighbors:
- config:
    neighbor-address: "${FRR_PEER}"
    peer-as: 64512
  afi-safis:
  - config:
      afi-safi-name: l2vpn-evpn
EOF
  if [[ "${EVPN_STANDALONE_MULTICHASSIS_SMOKE:-0}" == 1 &&
        "${LOCAL_VTEP}" == 198.18.0.2 ]]; then
    cat >>"${EVIDENCE_DIR}/standalone-gobgpd.yaml" <<'EOF'
- config:
    neighbor-address: "198.18.0.5"
    peer-as: 64512
  afi-safis:
  - config:
      afi-safi-name: l2vpn-evpn
EOF
  fi
  k -n k8gobgp-system patch ds k8gobgp --type=merge -p \
    '{"spec":{"template":{"spec":{"nodeSelector":{"evpn-standalone-smoke-disabled":"true"}}}}}'
  if [[ -n "${PROVIDER_POD:-}" ]]; then
    k -n k8gobgp-system wait --for=delete "pod/${PROVIDER_POD}" --timeout=120s
  fi
  PROVIDER_POD=""

  standalone_smoke_agent_exec mkdir -p /run/gobgp /run/evpn-node-agent/standalone-smoke
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/gobgpd-pin" /usr/local/bin/gobgpd
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/gobgp-pin" /usr/local/bin/gobgp
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/build/evpn-node-agent" \
    /usr/local/bin/evpn-node-agent
  standalone_smoke_agent_exec chmod 755 \
    /usr/local/bin/gobgpd /usr/local/bin/gobgp /usr/local/bin/evpn-node-agent
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-gobgpd.yaml" \
    /run/gobgp/gobgpd.yaml
  STANDALONE_GOBGP_POD="${OVN_POD}"
  standalone_smoke_capture_command standalone-speaker-launch \
    standalone_smoke_agent_exec sh -ec \
    '/usr/local/bin/gobgpd -f /run/gobgp/gobgpd.yaml --api-hosts unix:///run/gobgp/gobgp.sock >>/run/gobgp/gobgpd.log 2>&1 </dev/null & echo "$!" >/run/gobgp/gobgpd.pid'
  local deadline=$((SECONDS+90))
  until standalone_smoke_capture_command standalone-speaker-global-poll \
      provider_exec gobgp global &&
    standalone_smoke_capture_command standalone-speaker-neighbor-poll \
      provider_exec gobgp neighbor "${FRR_PEER}" &&
    grep -qi Established "${EVIDENCE_DIR}/standalone-speaker-neighbor-poll.stdout"; do
    (( SECONDS < deadline )) || {
      echo "standalone GoBGP readiness failed; retained exec status distinguishes API/peer failure from exec-stream failure" >&2
      return 1
    }
    sleep 1
  done
  provider_exec gobgp global | tee "${EVIDENCE_DIR}/standalone-gobgp-global.txt"
  provider_exec gobgp neighbor "${FRR_PEER}" | tee "${EVIDENCE_DIR}/standalone-gobgp-neighbor.txt"
}

standalone_smoke_assert_fence_revision() {
  local revision="$1"
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
    >"${EVIDENCE_DIR}/authorization-fence-${revision}.json"
  python3 - "${EVIDENCE_DIR}/authorization-fence-${revision}.json" "${revision}" <<'PY'
import json,sys
j=json.load(open(sys.argv[1]))
record=j.get("Record") or {}
state=record.get("State") or {}
assert record.get("Pending") is None,record
assert state.get("revision")==int(sys.argv[2]),state
assert state.get("digest"),state
print(f"durably-accepted-policy-revision={sys.argv[2]}=PASS")
PY
}

standalone_smoke_wait_fence_revision() {
  local revision="$1" deadline=$((SECONDS+120))
  while (( SECONDS < deadline )); do
    if standalone_smoke_agent_exec cat \
        /run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
        >"${EVIDENCE_DIR}/authorization-fence-poll.json" 2>/dev/null &&
      python3 - "${EVIDENCE_DIR}/authorization-fence-poll.json" "${revision}" <<'PY'
import json,sys
try:
    record=json.load(open(sys.argv[1])).get("Record") or {}
    state=record.get("State") or {}
    accepted=record.get("Pending") is None and state.get("revision")==int(sys.argv[2]) and bool(state.get("digest"))
except (OSError,ValueError,TypeError):
    accepted=False
sys.exit(0 if accepted else 1)
PY
    then
      standalone_smoke_assert_fence_revision "${revision}"
      return 0
    fi
    sleep 1
  done
  echo "authorization fence did not durably accept revision ${revision}" >&2
  return 1
}

standalone_smoke_start_agent() {
  standalone_smoke_agent_arguments
  standalone_smoke_agent_exec rm -f /run/evpn-node-agent/export-status.json \
    /run/evpn-node-agent/agent.sock
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    standalone_smoke_agent_exec rm -f /run/evpn-node-agent/remote-status.json
  fi
  standalone_smoke_capture_command standalone-agent-launch \
    standalone_smoke_agent_exec sh -ec \
    '/usr/local/bin/evpn-node-agent "$@" >>/run/evpn-node-agent/standalone-smoke/agent.log 2>&1 </dev/null & echo "$!" >/run/evpn-node-agent/standalone-smoke/agent.pid' \
    standalone-agent "${STANDALONE_AGENT_ARGUMENTS[@]}"
  for _ in $(seq 1 120); do
    if ! standalone_smoke_agent_exec sh -ec \
      'kill -0 "$(cat /run/evpn-node-agent/standalone-smoke/agent.pid)"'; then
      standalone_smoke_agent_exec cat /run/evpn-node-agent/standalone-smoke/agent.log >&2
      echo "standalone Node-Agent exited during startup" >&2
      return 1
    fi
    if standalone_smoke_read_status; then
      return 0
    fi
    sleep 1
  done
  echo "standalone Node-Agent did not write exporter status" >&2
  standalone_smoke_agent_exec tail -100 \
    /run/evpn-node-agent/standalone-smoke/agent.log >&2 || true
  return 1
}

standalone_smoke_agent_arguments() {
  STANDALONE_AGENT_ARGUMENTS=(
    --mode=real-ovn-ovs \
    --network-backend=linux-veth \
    "--ovn-nb-endpoint=${STANDALONE_NB_ENDPOINT:-unix:/var/run/ovn/ovnnb_db.sock}" \
    --ovsdb-endpoint=unix:/var/run/openvswitch/db.sock \
    "--ovn-sb-endpoint=${STANDALONE_SB_ENDPOINT:-unix:/var/run/ovn/ovnsb_db.sock}" \
    --evpn-export-status=/run/evpn-node-agent/export-status.json \
    --evpn-export-interval=1s \
    --evpn-export-timeout=3s \
    --standalone-evpn-export-config=/run/evpn-node-agent/standalone-smoke/runtime.json
  )
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    STANDALONE_AGENT_ARGUMENTS+=(
      --evpn-remote-config=/run/evpn-node-agent/standalone-smoke/import-config.json
      --evpn-remote-status=/run/evpn-node-agent/remote-status.json
      --evpn-remote-interval=1s --evpn-remote-timeout=3s
    )
  fi
}

standalone_smoke_stop_agent() {
  local pid
  pid="$(standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/agent.pid)"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  standalone_smoke_agent_exec kill -TERM "${pid}"
  for _ in $(seq 1 30); do
    if ! standalone_smoke_agent_exec kill -0 "${pid}" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "standalone Node-Agent did not stop after SIGTERM" >&2
  return 1
}

standalone_smoke_write_policy() {
  local revision="$1" include_endpoint="$2"
  python3 - "${EVIDENCE_DIR}/standalone-policy-${revision}.json" \
    "${revision}" "${INSTALLATION_UID}" "${NODE_UID}" "${LOCAL_CHASSIS}" \
    "${SMOKE_ENDPOINT_ID}" "${SMOKE_ATTACHMENT_ID}" "${SMOKE_LSP}" \
    "${SMOKE_LSP_UUID}" "${include_endpoint}" <<'PY'
import json,os,sys
path,revision,installation,node,chassis,endpoint,attachment,lsp,lsp_uuid,active=sys.argv[1:]
doc={"version":1,"revision":int(revision),"installationUID":installation,
     "nodeUID":node,"localChassis":chassis,"endpoints":[]}
if active=="yes":
    doc["endpoints"].append({
        "networkID":"n24","endpointID":endpoint,"attachmentID":attachment,
        "attachmentGeneration":1,"logicalSwitchPort":lsp,
        "logicalSwitchPortUUID":lsp_uuid,"mac":"0a:58:f0:00:00:0a",
        "ip":"10.244.240.10"})
with open(path,"w") as out:
    json.dump(doc,out,separators=(",",":"))
    out.write("\n")
os.chmod(path,0o600)
PY
}

standalone_cohost_configure() {
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
runtime=json.loads((p/"standalone-runtime.json").read_text())
runtime["enableRemoteImport"]=True
export=json.loads((p/"standalone-export-config.json").read_text())
remote=json.loads((p/"remote-config.json").read_text())
remote["GRPCEndpoint"]=export["GRPCEndpoint"]
remote["AttestationConfigPath"]=export["AttestationConfigPath"]
remote["Netlink"]["StateDirectory"]="/run/evpn-node-agent/standalone-smoke/import-receipts"
remote["Netlink"]["Recovery"]={"Domain":{
    "InstallationUID":runtime["installationUID"],"NodeUID":runtime["nodeUID"]}}
if __import__("os").environ.get("EVPN_STANDALONE_MULTICHASSIS_SMOKE")=="1":
    remote["Import"]["Networks"][0]["AllowedVTEPs"].append("198.18.0.5")
assert remote["ASN"]==export["ASN"] and remote["RouterID"]==export["RouterID"]
assert len({runtime["authorizationFenceDirectory"],export["Recovery"]["StateDirectory"],
            remote["Netlink"]["StateDirectory"]})==3
for n in remote["Netlink"]["Networks"]:
    assert any(e["Enabled"] and (e["NetworkID"],e["LogicalSwitch"],e["VNI"])==
               (n["NetworkID"],n["LogicalSwitch"],n["VNI"]) for e in export["Networks"])
(p/"standalone-runtime.json").write_text(json.dumps(runtime)+"\n")
(p/"standalone-import-config.json").write_text(json.dumps(remote)+"\n")
PY
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-runtime.json" \
    /run/evpn-node-agent/standalone-smoke/runtime.json
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-import-config.json" \
    /run/evpn-node-agent/standalone-smoke/import-config.json
  standalone_smoke_agent_exec install -d -m 700 -o root -g root \
    /run/evpn-node-agent/standalone-smoke/import-receipts
}

standalone_cohost_snapshot() {
  local label="$1" local_count="$2"
  interop_wait_import_count 1 "cohost-${label}" standalone
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present "cohost-${label}-remote"
  standalone_smoke_capture_command "cohost-${label}-export-status" \
    standalone_smoke_agent_exec cat /run/evpn-node-agent/export-status.json
  standalone_smoke_capture_command "cohost-${label}-export-journal" \
    standalone_smoke_agent_exec cat /run/evpn-node-agent/standalone-smoke/export-receipts/journal.json
  standalone_smoke_capture_command "cohost-${label}-import-journal" \
    standalone_smoke_agent_exec cat /run/evpn-node-agent/standalone-smoke/import-receipts/receipts-v1/journal.json
  standalone_smoke_capture_command "cohost-${label}-fdb" \
    standalone_smoke_agent_exec bridge -j fdb show dev "${VXLAN_DEVICE}"
  standalone_smoke_capture_command "cohost-${label}-neighbor" \
    standalone_smoke_agent_exec ip -4 -j neigh show nud all dev "${BRIDGE_DEVICE}"
  standalone_smoke_capture_command "cohost-${label}-links" \
    standalone_smoke_agent_exec ip -d -j link show
  standalone_smoke_capture_command "cohost-${label}-ovn-switch" \
    standalone_smoke_agent_exec ovn-nbctl --format=json --columns=_uuid,name,other_config list Logical_Switch
  standalone_smoke_capture_command "cohost-${label}-ovs" \
    standalone_smoke_agent_exec ovs-vsctl show
  standalone_smoke_capture_command "cohost-${label}-flows" \
    standalone_smoke_agent_exec ovs-ofctl dump-flows br-int
  standalone_smoke_capture_command "cohost-${label}-gobgp" \
    provider_exec gobgp global rib -a evpn -j
  standalone_smoke_capture_command "cohost-${label}-agent" \
    standalone_smoke_agent_exec sh -ec \
    'pid=$(cat /run/evpn-node-agent/standalone-smoke/agent.pid); kill -0 "$pid"; cat "/proc/$pid/status"; tr "\000" " " <"/proc/$pid/cmdline"; test ! -S /run/evpn-node-agent/agent.sock'
  python3 - "${EVIDENCE_DIR}" "${label}" "${local_count}" "${REMOTE_MAC}" "${REMOTE_IP}" "${FRR_VTEP}" \
    "${VXLAN_DEVICE}" "${BRIDGE_DEVICE}" "${VNI}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);label=sys.argv[2];count=int(sys.argv[3])
mac,ip,vtep,vxlan,bridge,vni=sys.argv[4:]
def read(name): return json.loads((p/f"cohost-{label}-{name}.stdout").read_text())
status=read("export-status");remote=json.loads((p/f"remote-status-cohost-{label}.json").read_text())
assert not status.get("Error") and not remote.get("Error"),(status,remote)
assert remote["Snapshot"]["Authoritative"] and remote["RecoveryState"]=="RUNNING",remote
assert status["Result"]["RecoveryState"]=="RUNNING",status
assert status["SpeakerProcess"]["PID"]>0 and status["SpeakerProcess"]["StartTicks"],status
exports=read("export-journal")["Record"];imports=read("import-journal")["Record"]
assert exports["Pending"] is None and imports["Pending"] is None
assert not imports["State"]["Quarantine"]
receipts=exports["State"]["Receipts"] or []
assert len(receipts)==count and len(status["Result"]["Applied"] or [])==count
if count:
    uuid=(p/"standalone-owned-route-uuid.txt").read_text().strip()
    assert receipts[0]["Owned"]["PathUUID"]==uuid
    assert status["Result"]["Applied"][0]["PathUUID"]==uuid
assert len(imports["State"]["Receipts"])==len(remote["Receipts"])==1
r=next(iter(imports["State"]["Receipts"].values()))
assert (r["Endpoint"]["MAC"],r["Endpoint"]["IP"],r["Endpoint"]["RemoteVTEP"])==(mac,ip,vtep),r
assert all(x["Route"]["Classification"]=="remote" and x["Route"]["Source"]["NeighborIP"]=="198.18.0.3"
           for x in r["Endpoint"]["Routes"]),r
assert r["FDB"]["ACK"] and r["FDB"]["Status"]=="confirmed",r
assert r["Neighbor"]["ACK"] and r["Neighbor"]["Status"]=="confirmed",r
assert r["Controller"]==imports["State"]["Controller"]==remote["Receipts"][0]["Controller"]
assert all(x["Owned"]["Endpoint"]["MACAddress"]!=mac for x in receipts),receipts
fdb=read("fdb");neighbors=read("neighbor");links=read("links")
assert any(x.get("mac")==mac and x.get("dst")==vtep and x.get("state")=="permanent" and
           {"self","extern_learn"}<=set(x.get("flags",[])) for x in fdb),fdb
assert any(x.get("dst")==ip and x.get("lladdr")==mac and
           "NOARP" in x.get("state",[]) for x in neighbors),neighbors
vx=next(x for x in links if x["ifname"]==vxlan)
assert vx["linkinfo"]["info_kind"]=="vxlan" and vx["linkinfo"]["info_data"]["id"]==int(vni),vx
assert vx["master"]==bridge,vx
ovs=read("ovn-switch");columns=ovs["headings"]
switch=next(dict(zip(columns,row)) for row in ovs["data"] if row[columns.index("name")]=="ls24b")
mapping=dict(switch["other_config"][1])
assert (mapping["dynamic-routing-vni"],mapping["dynamic-routing-bridge-ifname"],
        mapping["dynamic-routing-vxlan-ifname"])==(vni,bridge,vxlan),mapping
rib=read("gobgp")
assert any(mac in key and ip in key for key in rib)
assert any("0a:58:f0:00:00:0a" in key for key in rib)==bool(count),rib
agent=(p/f"cohost-{label}-agent.stdout").read_text()
assert "--standalone-evpn-export-config=" in agent and "--evpn-remote-config=" in agent
print(f"cohost-{label}: exact export receipts, independent importer ownership, actual mapped FDB/neighbor, single attested agent without CNI=PASS")
PY
}

standalone_cohost_assert_progress() {
  python3 - "${EVIDENCE_DIR}" "$1" "$2" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);before,after=sys.argv[2:]
def get(label,name):return json.loads((p/f"cohost-{label}-{name}.stdout").read_text())
assert get(after,"export-status")["Time"]>get(before,"export-status")["Time"]
r=[json.loads((p/f"remote-status-cohost-{x}.json").read_text()) for x in (before,after)]
assert r[1]["Time"]>r[0]["Time"] and r[1]["RecoveryState"]=="RUNNING"
assert get(before,"import-journal")["Record"]["State"]["Controller"]==get(after,"import-journal")["Record"]["State"]["Controller"]
assert r[0]["Receipts"][0]["Devices"]==r[1]["Receipts"][0]["Devices"]
print("simultaneous reconciliation progressed without changing importer ownership/device identity=PASS")
PY
}

standalone_cohost_failure_isolation() {
  standalone_smoke_stop_agent
  standalone_smoke_agent_arguments
  local rc=0
  standalone_smoke_capture_command standalone-cohost-failed-runtime \
    timeout --kill-after=2s 20s \
    kubectl --kubeconfig "${KUBECONFIG}" --context "kind-${KIND_CLUSTER}" \
    -n cni-test exec "${OVN_POD}" -c ovn-ovs -- /usr/local/bin/evpn-node-agent \
    "${STANDALONE_AGENT_ARGUMENTS[@]}" --evpn-remote-interval=0s </dev/null || rc=$?
  [[ "${rc}" == 1 ]]
  grep -F "remote import interval and timeout must be positive" \
    "${EVIDENCE_DIR}/standalone-cohost-failed-runtime.stderr"
  grep -F 'standalone-evpn-export ' "${EVIDENCE_DIR}/standalone-cohost-failed-runtime.stderr" |
    grep -F 'operation was canceled'
  standalone_smoke_start_agent
  standalone_smoke_wait_status withdrawn
  standalone_cohost_snapshot failure-recovered 0
}

standalone_export_smoke() {
  local gobgp_pid gobgp_config
  local local_route_uuid
  local cni_agent_pod

  k -n k8gobgp-system get ds k8gobgp -o json \
    >"${EVIDENCE_DIR}/k8gobgp-daemonset-before-smoke.json"
  python3 - "${EVIDENCE_DIR}/standalone-dry-export-config.json" \
    "${EVIDENCE_DIR}/export-config.json" <<'PY'
import json,sys
config=json.load(open(sys.argv[2]))
config["DryRun"]=True
with open(sys.argv[1],"w") as out:
    json.dump(config,out,separators=(",",":"))
    out.write("\n")
PY
  standalone_smoke_copy_to_worker "${EVIDENCE_DIR}/standalone-dry-export-config.json" \
    /run/evpn-node-agent/export-config.json
  for _ in $(seq 1 30); do
    if standalone_smoke_assert_cni_export_dry_run >/dev/null 2>&1; then break; fi
    sleep 1
  done
  standalone_smoke_assert_cni_export_dry_run
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    # Drain through the original owner; a new journal must never adopt its
    # existing kernel entries merely because the tuples happen to match.
    docker exec "${FRR_CONTAINER}" ip netns del evpnwork
    interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" absent standalone-owner-handoff
    interop_wait_import_count 0 standalone-owner-handoff
    interop_assert_remote_kernel_withdrawal
  fi
  sleep 2
  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  SMOKE_LSP="$(rt ovn-nbctl --bare --columns=name find Logical_Switch_Port \
    'addresses{>=}"0a:58:f0:00:00:0a 10.244.240.10/24"')"
  [[ -n "${SMOKE_LSP}" ]]
  SMOKE_LSP_UUID="$(rt ovn-nbctl --bare --columns=_uuid find Logical_Switch_Port "name=${SMOKE_LSP}")"
  [[ "${SMOKE_LSP_UUID}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]
  SMOKE_ENDPOINT_ID="$(rt ovn-nbctl get Logical_Switch_Port "${SMOKE_LSP}" \
    external_ids:evpn.framework/endpoint-id | tr -d '"')"
  SMOKE_ATTACHMENT_ID="$(rt ovn-nbctl get Logical_Switch_Port "${SMOKE_LSP}" \
    external_ids:evpn.framework/attachment-id | tr -d '"')"
  [[ -n "${SMOKE_ENDPOINT_ID}" && -n "${SMOKE_ATTACHMENT_ID}" ]]
  [[ "$(rt ovn-nbctl get Logical_Switch_Port "${SMOKE_LSP}" \
    external_ids:evpn.framework/network-id | tr -d '"')" == n24 ]]
  rt ovn-nbctl set Logical_Switch_Port "${SMOKE_LSP}" \
    external_ids:evpn.framework/attachment-generation=1
  rt ovn-nbctl --format=json --columns=_uuid,name,addresses,external_ids \
    list Logical_Switch_Port | tee "${EVIDENCE_DIR}/standalone-lsp.json" >/dev/null
  rt ovn-sbctl --format=json --columns=_uuid,logical_port,chassis,up \
    list Port_Binding | tee "${EVIDENCE_DIR}/standalone-port-bindings.json" >/dev/null
  rt ovn-nbctl --format=json --columns=_uuid,name,ports \
    list Logical_Switch | tee "${EVIDENCE_DIR}/standalone-logical-switches.json" >/dev/null

  ACTIVE_CASE=standalone_export_advertise
  cni_agent_pod="$(k -n cni-test get pods -l app=evpn-node-agent \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  k -n cni-test patch evpncore/type2 --type=merge -p '{"spec":{"nodeAgent":{"enabled":false}}}'
  local deadline=$((SECONDS+120))
  for _ in $(seq 1 120); do
    if k -n cni-test get ds evpn-node-agent >/dev/null 2>&1; then
      (( SECONDS < deadline )) || {
        echo "operator Node-Agent DaemonSet remained after being disabled" >&2
        return 1
      }
      sleep 1
      continue
    fi
    cni_agent_pod="$(k -n cni-test get pods -l app=evpn-node-agent \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
    [[ -z "${cni_agent_pod}" ]] && break
    (( SECONDS < deadline )) || break
    sleep 1
  done
  [[ -z "${cni_agent_pod}" ]] || {
    echo "operator Node-Agent Pod remained after its DaemonSet was deleted: ${cni_agent_pod}" >&2
    return 1
  }
  AGENT_POD=""
  standalone_smoke_wait_cni_socket_absent

  standalone_smoke_agent_exec mkdir -p /run/evpn-node-agent
  cp "${EVIDENCE_DIR}/export-config.json" "${EVIDENCE_DIR}/standalone-export-config.json"
  python3 - "${EVIDENCE_DIR}/standalone-export-config.json" \
    "${INSTALLATION_UID}" "${NODE_UID}" "${LOCAL_CHASSIS}" <<'PY'
import json,sys
path,installation,node,chassis=sys.argv[1:]
c=json.load(open(path))
c["Enabled"]=True
c["DryRun"]=False
c["ProcessContinuity"]=True
c["LocalChassis"]=chassis
c["GRPCEndpoint"]="unix:///run/gobgp/gobgp.sock"
c["AttestationConfigPath"]="/run/evpn-node-agent/standalone-smoke/speaker.json"
c["Recovery"]={"StateDirectory":"/run/evpn-node-agent/standalone-smoke/export-receipts",
               "Domain":{"InstallationUID":installation,"NodeUID":node}}
for network in c["Networks"]:
    network["ExporterSource"]="framework"
    network.pop("BridgeDevice",None)
    network.pop("VXLANDevice",None)
    network.pop("AdvertiseDevice",None)
with open(path,"w") as out:
    json.dump(c,out,separators=(",",":"))
    out.write("\n")
PY

  standalone_smoke_configure_speaker
  standalone_smoke_agent_exec chmod 700 \
    /run/evpn-node-agent/standalone-smoke
  gobgp_pid="$(standalone_smoke_agent_exec cat /run/gobgp/gobgpd.pid)"
  [[ "${gobgp_pid}" =~ ^[0-9]+$ ]] || {
    echo "expected exactly one pinned standalone gobgpd process, found: ${gobgp_pid}" >&2
    return 1
  }
  standalone_smoke_attest_speaker
  standalone_smoke_write_policy 1 yes
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-export-config.json" \
    /run/evpn-node-agent/standalone-smoke/export-config.json
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-speaker.json" \
    /run/evpn-node-agent/standalone-smoke/speaker.json
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-policy-1.json" \
    /run/evpn-node-agent/standalone-smoke/authorization.json
  cat >"${EVIDENCE_DIR}/standalone-runtime.json" <<EOF
{"enabled":true,"installationUID":"${INSTALLATION_UID}","nodeUID":"${NODE_UID}","localChassis":"${LOCAL_CHASSIS}","exporterConfigPath":"/run/evpn-node-agent/standalone-smoke/export-config.json","authorizationPolicyPath":"/run/evpn-node-agent/standalone-smoke/authorization.json","authorizationFenceDirectory":"/run/evpn-node-agent/standalone-smoke/authorization-fence"}
EOF
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-runtime.json" \
    /run/evpn-node-agent/standalone-smoke/runtime.json
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    standalone_cohost_configure
  fi
  standalone_smoke_start_agent
  standalone_smoke_capture_command standalone-cni-socket-absent-startup \
    standalone_smoke_agent_exec test \
    ! -S /run/evpn-node-agent/agent.sock
  interop_wait_rib_route 0a:58:f0:00:00:0a 10.244.240.10 present standalone-advertised
  standalone_smoke_wait_status advertised
  standalone_smoke_wait_fence_revision 1
  python3 - "${EVIDENCE_DIR}/standalone-export-status.json" \
    >"${EVIDENCE_DIR}/standalone-owned-route-uuid.txt" <<'PY'
import json,sys
status=json.load(open(sys.argv[1]))
applied=(status.get("Result") or {}).get("Applied") or []
assert len(applied)==1,applied
uuid=applied[0].get("PathUUID","")
assert uuid,applied
print(uuid)
PY
  local_route_uuid="$(cat "${EVIDENCE_DIR}/standalone-owned-route-uuid.txt")"
  [[ "${local_route_uuid}" =~ ^[0-9a-fA-F-]{36}$ ]]
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/export-receipts/journal.json \
    >"${EVIDENCE_DIR}/standalone-export-journal-advertised.json"
  python3 - "${EVIDENCE_DIR}/standalone-export-journal-advertised.json" \
    "${local_route_uuid}" <<'PY'
import json,sys
record=json.load(open(sys.argv[1]))["Record"]
assert record["Pending"] is None,record
receipts=record["State"]["Receipts"]
assert len(receipts)==1,receipts
assert receipts[0]["Owned"]["PathUUID"]==sys.argv[2],receipts
print("advertised-exact-owned-path-receipt=PASS")
PY
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/agent.log |
    tee "${EVIDENCE_DIR}/standalone-agent-advertised.log" >/dev/null
  provider_exec gobgp global rib -a evpn -j |
    tee "${EVIDENCE_DIR}/standalone-gobgp-rib-advertised.json" >/dev/null
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    ACTIVE_CASE=standalone_remote_import
    interop_create_remote_workload
    docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 10.244.240.1
    standalone_cohost_snapshot advertised 1
    case_status standalone_remote_import PASS "FRR Type-2 accepted, durable FDB/neighbor ACKs and actual OVN-mapped kernel installation"
    ACTIVE_CASE=standalone_simultaneous
    sleep 4
    standalone_cohost_snapshot simultaneous 1
    standalone_cohost_assert_progress advertised simultaneous
    case_status standalone_simultaneous PASS "both authoritative loops advanced; exporter UUID and importer ownership controller stable"
    if [[ "${EVPN_STANDALONE_MULTICHASSIS_SMOKE:-0}" == 1 ]]; then
      standalone_multichassis
    fi
  fi
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present standalone-foreign-survivor-before
  ACTIVE_CASE=standalone_export_advertise
  case_status standalone_export_advertise PASS \
    "durable policy revision 1, authorized OVN LSP ${SMOKE_LSP_UUID}, owned path ${local_route_uuid}, external peer RIB"

  ACTIVE_CASE=standalone_export_revoke
  local revoke_revision
  standalone_smoke_agent_exec cat /run/evpn-node-agent/standalone-smoke/authorization-fence/journal.json \
    >"${EVIDENCE_DIR}/standalone-before-revoke-fence.json"
  revoke_revision="$(python3 - "${EVIDENCE_DIR}/standalone-before-revoke-fence.json" <<'PY'
import json,sys
print(json.load(open(sys.argv[1]))["Record"]["State"]["revision"]+1)
PY
)"
  standalone_smoke_write_policy "${revoke_revision}" no
  standalone_smoke_copy_to_agent "${EVIDENCE_DIR}/standalone-policy-${revoke_revision}.json" \
    /run/evpn-node-agent/standalone-smoke/authorization.json
  standalone_smoke_wait_fence_revision "${revoke_revision}"
  standalone_smoke_wait_status withdrawn
  interop_wait_rib_route 0a:58:f0:00:00:0a 10.244.240.10 absent standalone-revoked
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present standalone-foreign-survivor-after
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/export-receipts/journal.json \
    >"${EVIDENCE_DIR}/standalone-export-journal-revoked.json"
  provider_exec gobgp global rib -a evpn -j \
    >"${EVIDENCE_DIR}/standalone-gobgp-rib-revoked.json"
  python3 - "${EVIDENCE_DIR}/standalone-export-journal-revoked.json" \
    "${local_route_uuid}" <<'PY'
import json,sys
j=json.load(open(sys.argv[1]))
record=j.get("Record") or {}
state=record.get("State") or {}
assert record.get("Pending") is None,record
receipts=state.get("Receipts") or []
assert all((r.get("Owned") or {}).get("PathUUID")!=sys.argv[2] for r in receipts),receipts
print("revoked-owned-path-receipt-removed=PASS")
PY
  ACTIVE_CASE=standalone_export_revoke
  case_status standalone_export_revoke PASS \
    "policy revision ${revoke_revision} durably accepted; exact UUID ${local_route_uuid} absent at GoBGP/FRR; remote route preserved"
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    standalone_cohost_snapshot revoked 0
  fi

  ACTIVE_CASE=standalone_export_restart
  standalone_smoke_stop_agent
  standalone_smoke_start_agent
  standalone_smoke_capture_command standalone-cni-socket-absent-restart \
    standalone_smoke_agent_exec test \
    ! -S /run/evpn-node-agent/agent.sock
  standalone_smoke_assert_fence_revision "${revoke_revision}"
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/agent.log |
    tee "${EVIDENCE_DIR}/standalone-agent-recovered.log" >/dev/null
  standalone_smoke_wait_status withdrawn
  standalone_smoke_agent_exec cat \
    /run/evpn-node-agent/standalone-smoke/export-receipts/journal.json \
    >"${EVIDENCE_DIR}/standalone-export-journal-recovered.json"
  python3 - "${EVIDENCE_DIR}/standalone-export-journal-recovered.json" <<'PY'
import json,sys
record=json.load(open(sys.argv[1]))["Record"]
assert record["Pending"] is None,record
assert not record["State"]["Receipts"],record
print("recovered-route-journal-without-revoked-receipt=PASS")
PY
  interop_wait_rib_route 0a:58:f0:00:00:0a 10.244.240.10 absent standalone-recovered
  interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" present standalone-foreign-survivor-recovered
  provider_exec gobgp global rib -a evpn -j |
    tee "${EVIDENCE_DIR}/standalone-gobgp-rib-recovered.json" >/dev/null
  case_status standalone_export_restart PASS \
    "Node-Agent restarted with recovered authorization fence and route journal; revoked route remained absent"
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    ACTIVE_CASE=standalone_cohost_recovery
    standalone_cohost_snapshot recovered 0
    standalone_cohost_assert_progress revoked recovered
    case_status standalone_cohost_recovery PASS "exporter and importer journals recovered; local route absent and remote FDB/neighbor ownership retained"
    ACTIVE_CASE=standalone_failure_isolation
    standalone_cohost_failure_isolation
    case_status standalone_failure_isolation PASS "invalid importer interval failed the real child runtime; parent exited and joined exporter; journals reopened on healthy restart"
  fi
  standalone_smoke_capture_diagnostics
  ACTIVE_CASE=standalone_restore
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    docker exec "${FRR_CONTAINER}" ip netns del evpnwork
    interop_wait_rib_route "${REMOTE_MAC}" "${REMOTE_IP}" absent standalone-restore-drain
    interop_wait_import_count 0 standalone-restore-drain standalone
    interop_assert_remote_kernel_withdrawal
  fi
  standalone_smoke_stop_agent
  standalone_smoke_agent_exec kill -TERM "${gobgp_pid}"
  for _ in $(seq 1 30); do
    if ! standalone_smoke_agent_exec kill -0 "${gobgp_pid}" 2>/dev/null; then
      break
    fi
    sleep 1
  done

  # Restore the operator-managed CNI agent and its original exporter config so
  # the ordinary DEL invocation and runner cleanup continue through their
  # existing authenticated path.
  docker exec -i "${WORKER}" sh -ec \
    'cat > /run/evpn-node-agent/export-config.json.next && chown root:root /run/evpn-node-agent/export-config.json.next && chmod 600 /run/evpn-node-agent/export-config.json.next && mv /run/evpn-node-agent/export-config.json.next /run/evpn-node-agent/export-config.json' \
    <"${EVIDENCE_DIR}/export-config.json"
  STANDALONE_GOBGP_POD=""
  local restore_selector
  restore_selector="$(python3 - "${EVIDENCE_DIR}/k8gobgp-daemonset-before-smoke.json" <<'PY'
import json,sys
spec=json.load(open(sys.argv[1]))["spec"]["template"]["spec"]
print(json.dumps([{"op":"replace","path":"/spec/template/spec/nodeSelector",
                  "value":spec["nodeSelector"]}]))
PY
)"
  k -n k8gobgp-system patch ds k8gobgp --type=json -p "${restore_selector}"
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=180s
  PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "${PROVIDER_POD}" ]]
  k -n cni-test patch evpncore/type2 --type=merge -p '{"spec":{"nodeAgent":{"enabled":true}}}'
  local restore_deadline=$((SECONDS+120))
  until k -n cni-test get ds evpn-node-agent >/dev/null 2>&1; do
    (( SECONDS < restore_deadline )) || {
      echo "operator Node-Agent DaemonSet was not restored" >&2
      return 1
    }
    sleep 1
  done
  k -n cni-test rollout status ds/evpn-node-agent --timeout=180s
  AGENT_POD="$(k -n cni-test get pod -l app=evpn-node-agent -o jsonpath='{.items[0].metadata.name}')"
  ag nc -z -U /run/evpn-node-agent/agent.sock
  if [[ "${EVPN_STANDALONE_COHOST_SMOKE:-0}" == 1 ]]; then
    interop_create_remote_workload
    docker exec "${FRR_CONTAINER}" ip netns exec evpnwork ping -I frr-work0 -c 2 -W 2 10.244.240.1
    interop_wait_import_count 1 standalone-operator-restored
  fi
  k -n cni-test delete pod pod-a --wait=true --timeout=90s
  case_status standalone_restore PASS "operator-managed speaker and authenticated CNI runtime restored"
}

standalone_smoke_attest_speaker() {
  local gobgp_pid gobgp_config
  gobgp_pid="$(standalone_smoke_agent_exec cat /run/gobgp/gobgpd.pid)"
  [[ "${gobgp_pid}" =~ ^[0-9]+$ ]]
  standalone_smoke_agent_exec cat "/proc/${gobgp_pid}/cmdline" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-cmdline.bin"
  standalone_smoke_agent_exec cat "/proc/${gobgp_pid}/cgroup" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-cgroup.txt"
  standalone_smoke_agent_exec cat "/proc/${gobgp_pid}/status" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-status.txt"
  standalone_smoke_agent_exec readlink "/proc/${gobgp_pid}/exe" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-executable.txt"
  standalone_smoke_agent_exec sha256sum "/proc/${gobgp_pid}/exe" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-executable.sha256"
  python3 - "${EVIDENCE_DIR}/standalone-gobgpd-cmdline.bin" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-config-path.txt" <<'PY'
import pathlib,sys
args=pathlib.Path(sys.argv[1]).read_bytes().rstrip(b"\0").decode().split("\0")
if len(args)<3:
    raise SystemExit(f"GoBGP arguments are incomplete: {args!r}")
config=None
for i,arg in enumerate(args):
    if arg in ("-f","--config-file"):
        if i+1>=len(args) or config is not None: raise SystemExit("ambiguous GoBGP config argument")
        config=args[i+1]
    elif arg.startswith("-f=") or arg.startswith("--config-file="):
        if config is not None: raise SystemExit("ambiguous GoBGP config argument")
        config=arg.split("=",1)[1]
if not config or not config.startswith("/") or ".." in pathlib.PurePosixPath(config).parts:
    raise SystemExit(f"GoBGP config path is not a clean absolute path: {config!r}")
print(config)
PY
  gobgp_config="$(cat "${EVIDENCE_DIR}/standalone-gobgpd-config-path.txt")"
  standalone_smoke_agent_exec sha256sum \
    "/proc/${gobgp_pid}/root${gobgp_config}" \
    >"${EVIDENCE_DIR}/standalone-gobgpd-config.sha256"
  python3 - "${EVIDENCE_DIR}" "${INSTALLATION_UID}" "${NODE_UID}" "${DAEMONSET_UID}" \
    "${gobgp_pid}" "${gobgp_config}" "${LOCAL_VTEP}" <<'PY'
import hashlib,json,pathlib,re,sys
p=pathlib.Path(sys.argv[1])
installation,node,service,pid,config,router=sys.argv[2:]
args=p.joinpath("standalone-gobgpd-cmdline.bin").read_bytes().rstrip(b"\0").decode().split("\0")
exe=p.joinpath("standalone-gobgpd-executable.txt").read_text().strip()
uid_line=next((x for x in p.joinpath("standalone-gobgpd-status.txt").read_text().splitlines() if x.startswith("Uid:")),None)
if not uid_line: raise SystemExit("GoBGP UID evidence is missing")
uid=int(uid_line.split()[1])
cgroup_lines=[x[3:] for x in p.joinpath("standalone-gobgpd-cgroup.txt").read_text().splitlines() if x.startswith("0::")]
if len(cgroup_lines)!=1 or not cgroup_lines[0].startswith("/"):
    raise SystemExit("GoBGP unified cgroup evidence is missing or ambiguous")
exe_hash=p.joinpath("standalone-gobgpd-executable.sha256").read_text().split()[0]
cfg_hash=p.joinpath("standalone-gobgpd-config.sha256").read_text().split()[0]
if args[0]!=exe:
    raise SystemExit(f"GoBGP argv[0] {args[0]!r} differs from executable {exe!r}")
if not re.fullmatch(r"[0-9a-f]{64}",exe_hash) or not re.fullmatch(r"[0-9a-f]{64}",cfg_hash):
    raise SystemExit("GoBGP executable/config SHA256 evidence is invalid")
attestation={"mode":"standalone","standalone":{
    "installationUID":installation,"nodeUID":node,"serviceUID":service,
    "expectedUID":uid,"cgroupPath":cgroup_lines[0],"executablePath":exe,
    "executableSHA256":exe_hash,"arguments":args,"goBGPConfigPath":config,
    "goBGPConfigSHA256":cfg_hash,"asn":64512,"routerID":router}}
p.joinpath("standalone-speaker.json").write_text(json.dumps(attestation,separators=(",",":"))+"\n")
PY
}
