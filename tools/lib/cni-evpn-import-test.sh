#!/usr/bin/env bash
# Sourced only inside the existing disposable exporter acceptance runner.
import_setup_config() {
  cat >> "${EVIDENCE_DIR}/provider-config.yaml" <<'EOF'
  neighbors:
  - config:
      neighborAddress: "127.0.0.2"
      peerAsn: 64513
      description: "disposable-upstream-GoBGP-remote-Type2"
    transport:
      passiveMode: true
    afiSafis:
    - family: "l2vpn-evpn"
      enabled: true
      addPaths:
        receive: true
EOF
}

import_readonly_snapshot() {
  local label="$1"
  {
    rt ovn-nbctl --format=json --columns=name,ports list Logical_Switch
    rt ovn-nbctl --format=json --columns=name,addresses,external_ids list Logical_Switch_Port
    rt ovn-sbctl --format=json --columns=logical_port,chassis,up list Port_Binding
    rt ovs-vsctl --format=json --columns=name,external_ids,interfaces list Port
    rt ovs-vsctl --format=json --columns=name,external_ids,ofport,error list Interface
    rt ovs-vsctl --format=json --columns=name,ports list Bridge
  } | tee "${EVIDENCE_DIR}/import-${label}-databases.json" >/dev/null
  {
    ag ip -o -d link show
    ag ip -o route show table all
    ag ip -o neigh show nud permanent
    ag bridge fdb show | sort
  } | tee "${EVIDENCE_DIR}/import-${label}-netlink.txt" >/dev/null
  ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/import-${label}-exporter.json" >/dev/null
  docker exec "${WORKER}" find /run/evpn-cni-evidence -type f | sort \
    | tee "${EVIDENCE_DIR}/import-${label}-cni-calls.txt" >/dev/null
  k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/import-${label}-bgpconfig.json" >/dev/null
}

import_verify() {
  import_readonly_snapshot before
  (ag env EVPN_IMPORT_LIVE=disposable EVPN_IMPORT_LOCAL_VTEP="${LOCAL_VTEP}" \
      /usr/local/bin/evpn.test -test.run '^TestRemoteType2Import_Live$' -test.v \
      | tee "${EVIDENCE_DIR}/remote-import-live.log") &
  local test_pid=$! deadline=$((SECONDS+100)) pid
  until ag test -f /run/evpn-node-agent/import-api-outage-request >/dev/null 2>&1; do
    kill -0 "${test_pid}" || { wait "${test_pid}"; return 1; }
    (( SECONDS<deadline )) || { echo "import outage handshake timed out" >&2; return 1; }
    sleep 1
  done
  pid="$(provider_exec cat /tmp/speaker.pid)"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  provider_exec kill -STOP "${pid}"
  ag touch /run/evpn-node-agent/import-api-outage-paused
  deadline=$((SECONDS+30))
  until ag test -f /run/evpn-node-agent/import-api-outage-observed >/dev/null 2>&1; do
    if ! kill -0 "${test_pid}" || (( SECONDS>=deadline )); then
      provider_exec kill -CONT "${pid}"
      echo "import did not acknowledge actual API outage" >&2
      return 1
    fi
    sleep 1
  done
  provider_exec kill -CONT "${pid}"
  wait "${test_pid}"
  grep -q '^--- PASS: TestRemoteType2Import_Live' "${EVIDENCE_DIR}/remote-import-live.log"
  import_readonly_snapshot after
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
def tables(label):
    text=(r/f"import-{label}-databases.json").read_text()
    decoder=json.JSONDecoder(); result=[]
    while text.strip():
        table,n=decoder.raw_decode(text.lstrip())
        result.append((table["headings"],sorted(table["data"],key=lambda x:json.dumps(x,sort_keys=True))))
        text=text.lstrip()[n:]
    assert len(result)==6
    return result
assert tables("before")==tables("after"),"NB/SB/OVS state changed"
assert (r/"import-before-netlink.txt").read_text()==(r/"import-after-netlink.txt").read_text(),"netlink state changed"
assert (r/"import-before-cni-calls.txt").read_text()==(r/"import-after-cni-calls.txt").read_text(),"CNI invoked during import"
a,b=(json.loads((r/f"import-{x}-exporter.json").read_text()) for x in ["before","after"])
assert a["Result"]["Applied"]==b["Result"]["Applied"] and len(b["Result"]["Applied"])==2
a,b=(json.loads((r/f"import-{x}-bgpconfig.json").read_text()) for x in ["before","after"])
assert a["spec"]==b["spec"] and a["metadata"]["generation"]==b["metadata"]["generation"]
print("import-readonly-NB-SB-OVS-netlink-CNI-exporter-BGPConfiguration=PASS")
PY
  export_check 2 2 after-readonly-import
}
