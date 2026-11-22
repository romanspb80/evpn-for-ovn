#!/usr/bin/env bash
# Sourced only inside the existing disposable exporter acceptance runner.
import_gap_setup_provider() {
  source "${ROOT}/tools/lib/cni-evpn-frr-interop-test.sh"
  interop_setup_provider
  REMOTE_FLAGS=""
}

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
  if [[ "${EVPN_GAP_CLOSURE:-0}" == 1 ]]; then
    k apply -f "${EVIDENCE_DIR}/provider-config.yaml"
    k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
    PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
    export_record_provider
    interop_setup_provider_attestation cni-export cni-test
    python3 - "${EVIDENCE_DIR}/export-config.json" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
config=json.loads(p.read_text())
config["AttestationConfigPath"]="/etc/evpn/provider-attestation/provider.json"
p.write_text(json.dumps(config)+"\n")
PY
    export_replace_config "${EVIDENCE_DIR}/export-config.json"
  fi
}

import_gap_ready() {
  export_check 0 2 gap-dry-ready
  EXPORT_STAGE=seed-foreign export_test 0 2 | tee "${EVIDENCE_DIR}/export-foreign-seed.log"
  EXPORT_FOREIGN=yes
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
config=json.loads((r/"export-config.json").read_text())
config["DryRun"]=False
(r/"export-live.json").write_text(json.dumps(config)+"\n")
PY
  export_replace_config "${EVIDENCE_DIR}/export-live.json"
  export_check 2 2 gap-fixture-ready
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
  (
  (ag env EVPN_IMPORT_LIVE=disposable EVPN_IMPORT_LOCAL_VTEP="${LOCAL_VTEP}" \
      /usr/local/bin/evpn.test -test.run '^TestRemoteType2Import_Live$' -test.v \
      | tee "${EVIDENCE_DIR}/remote-import-live.log") &
  local test_pid=$! deadline=$((SECONDS+100)) pid paused=0
  trap 'if [[ "${paused}" == 1 ]]; then k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -CONT "${pid}" || true; fi' EXIT
  until ag test -f /run/evpn-node-agent/import-api-outage-request >/dev/null 2>&1; do
    kill -0 "${test_pid}" || { wait "${test_pid}"; return 1; }
    (( SECONDS<deadline )) || { echo "import outage handshake timed out" >&2; return 1; }
    sleep 1
  done
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- ps -o pid,comm \
    >"${EVIDENCE_DIR}/import-speaker-processes.txt"
  pid="$(awk '$2=="gobgpd" {print $1}' "${EVIDENCE_DIR}/import-speaker-processes.txt")"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  paused=1
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -STOP "${pid}"
  deadline=$((SECONDS+10))
  until k -n k8gobgp-system exec "${PROVIDER_POD}" -- cat "/proc/${pid}/status" \
      >"${EVIDENCE_DIR}/import-speaker-paused-status.txt" &&
      grep -Eq '^State:[[:space:]]+T' "${EVIDENCE_DIR}/import-speaker-paused-status.txt"; do
    (( SECONDS<deadline )) || { echo "speaker did not stop" >&2; return 1; }
    sleep 1
  done
  ag touch /run/evpn-node-agent/import-api-outage-paused
  deadline=$((SECONDS+30))
  until ag test -f /run/evpn-node-agent/import-api-outage-observed >/dev/null 2>&1; do
    if ! kill -0 "${test_pid}" || (( SECONDS>=deadline )); then
      echo "import did not acknowledge actual API outage" >&2
      return 1
    fi
    sleep 1
  done
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -CONT "${pid}"
  paused=0
  wait "${test_pid}"
  )
  grep -q '^--- PASS: TestRemoteType2Import_Live' "${EVIDENCE_DIR}/remote-import-live.log"
  grep -q 'actual-speaker-API-outage-retains-uncertain-snapshot=PASS' "${EVIDENCE_DIR}/remote-import-live.log"
  export_check 2 2 after-readonly-import
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
  if [[ "${EVPN_GAP_CLOSURE:-0}" == 1 ]]; then
    import_gap_negatives
  fi
}

import_gap_negatives() {
  local remote_dir=/run/evpn-node-agent/gap-negatives rc=0
  ag mkdir "${remote_dir}"
  ag env EVPN_IMPORT_LIVE=disposable EVPN_IMPORT_LOCAL_VTEP="${LOCAL_VTEP}" \
    EVPN_GAP_CLOSURE=1 EVPN_GAP_EVIDENCE_DIR="${remote_dir}" \
    /usr/local/bin/evpn.test -test.run '^TestRemoteType2GapNegatives_Live$' -test.v \
    2>&1 | tee "${EVIDENCE_DIR}/remote-gap-negatives.log" || rc=$?
  mkdir "${EVIDENCE_DIR}/remote-gap-negatives"
  ag tar -cf - -C "${remote_dir}" . \
    | tar -xf - -C "${EVIDENCE_DIR}/remote-gap-negatives/" || rc=1
  [[ "${rc}" == 0 ]] || return "${rc}"
  grep -q '^--- PASS: TestRemoteType2GapNegatives_Live' "${EVIDENCE_DIR}/remote-gap-negatives.log"
}
