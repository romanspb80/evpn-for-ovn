#!/usr/bin/env bash
# Sourced by the disposable Framework Netlink runner.

native_gap_capture_start() {
  local iface
  iface="$(ag ip -j addr show | python3 -c 'import json,sys; v=sys.argv[1]; a=[x["ifname"] for x in json.load(sys.stdin) if any(i.get("local")==v for i in x.get("addr_info",[]))]; assert len(a)==1; print(a[0])' "${LOCAL_VTEP}")"
  NATIVE_GAP_CAPTURE_DIR="/run/evpn-node-agent/native-gap-${RUN_SUFFIX}"
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- mkdir "${NATIVE_GAP_CAPTURE_DIR}"
  NATIVE_GAP_CAPTURE_ACTIVE=1
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- sh -ec '
    d="$1"
    tcpdump -U -n -i "$2" -w "$d/bgp.pcap" "tcp port 179" >"$d/tcpdump.log" 2>&1 </dev/null &
    echo "$!" >"$d/tcpdump.pid"
  ' sh "${NATIVE_GAP_CAPTURE_DIR}" "${iface}"
  sleep 2
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
    sh -ec 'kill -0 "$(cat "$1/tcpdump.pid")"' sh "${NATIVE_GAP_CAPTURE_DIR}"
}

native_gap_capture_stop() {
  [[ "${NATIVE_GAP_CAPTURE_ACTIVE:-0}" == 1 ]] || return 0
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- sh -ec '
    d="$1"; pid="$(cat "$d/tcpdump.pid")"
    case "$pid" in *[!0-9]*|"") exit 1;; esac
    test -r "/proc/$pid/cmdline" || exit 1
    tr "\000" " " <"/proc/$pid/cmdline" | grep -F "$d/bgp.pcap" >/dev/null
    kill -INT "$pid"
    for i in $(seq 1 10); do
      test -e "/proc/$pid/status" || exit 0
      grep -Eq "^State:[[:space:]]+Z" "/proc/$pid/status" && exit 0
      sleep 1
    done
    exit 1
  ' sh "${NATIVE_GAP_CAPTURE_DIR}" || return 1
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- cat "${NATIVE_GAP_CAPTURE_DIR}/bgp.pcap" \
    >"${EVIDENCE_DIR}/M008-M009-bgp.pcap"
  k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- cat "${NATIVE_GAP_CAPTURE_DIR}/tcpdump.log" \
    >"${EVIDENCE_DIR}/M008-M009-tcpdump.log"
  tcpdump -nn -vvv -r "${EVIDENCE_DIR}/M008-M009-bgp.pcap" \
    >"${EVIDENCE_DIR}/M008-M009-bgp-decoded.txt" 2>"${EVIDENCE_DIR}/M008-M009-bgp-decode.log"
  test -s "${EVIDENCE_DIR}/M008-M009-bgp-decoded.txt"
  NATIVE_GAP_CAPTURE_ACTIVE=0
}

native_gap_snapshot() {
  local label="$1" variant="$2" reference="${3:-}"
  ag cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/${label}-status.json"
  provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock global rib -a evpn \
    >"${EVIDENCE_DIR}/${label}-gobgp.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
    >"${EVIDENCE_DIR}/${label}-frr.txt"
  ag bridge -j fdb show dev "${FRAMEWORK_ADVERTISE_DEVICE}" >"${EVIDENCE_DIR}/${label}-fdb.json"
  ag ip -4 -j neigh show nud all dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/${label}-neighbors4.json"
  ag ip -6 -j neigh show nud all dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/${label}-neighbors6.json"
  local -a args=("${label}" "${variant}" "${LOCAL_VTEP}" "${FRR_VTEP}" "${EVIDENCE_DIR}")
  [[ -z "${reference}" ]] || args+=("${reference}")
  python3 "${ROOT}/tools/lib/native_gap_verify.py" "${args[@]}"
}

native_gap_wait() {
  local label="$1" variant="$2" reference="${3:-}" deadline=$((SECONDS+100))
  until native_gap_snapshot "${label}" "${variant}" "${reference}" \
      2>"${EVIDENCE_DIR}/${label}-verify.stderr"; do
    (( SECONDS<deadline )) || {
      cat "${EVIDENCE_DIR}/${label}-verify.stderr" >&2
      echo "${label} did not reach expected state" >&2
      return 1
    }
    sleep 2
  done
  date -u +%FT%TZ >"${EVIDENCE_DIR}/${label}-time.txt"
}

native_gap_restore_sb() {
  [[ -n "${NATIVE_GAP_PAUSED_SB_PID:-}" ]] || return 0
  rt kill -CONT "${NATIVE_GAP_PAUSED_SB_PID}"
  NATIVE_GAP_PAUSED_SB_PID=""
}

native_gap_cases() {
  local a b pid phase
  native_gap_capture_start
  a="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["endpoints"][0]["logicalSwitchPort"])' "${EVIDENCE_DIR}/framework-endpoints.json")"
  b="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["endpoints"][1]["logicalSwitchPort"])' "${EVIDENCE_DIR}/framework-endpoints.json")"
  native_gap_wait M008-before base
  rt ovn-nbctl lsp-set-addresses "${a}" '0a:58:f0:01:00:0a 10.244.240.10 2001:db8:77::10'
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M008-dual dual-a
  rt ovn-nbctl lsp-set-addresses "${a}" '0a:58:f0:01:00:0a'
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M008-withdrawn mac-a
  rt ovn-nbctl lsp-set-addresses "${a}" '0a:58:f0:01:00:0a 10.244.240.10'
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M008-restored base
  python3 - "${EVIDENCE_DIR}" <<'PY' | tee "${EVIDENCE_DIR}/M008-result.txt"
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
def paths(n):
    return {(x["mac"],x["ip"]):x["uuid"] for x in json.loads((r/(n+"-verified.json")).read_text())["receipts"]}
a,b,c,d=(paths(n) for n in ("M008-before","M008-dual","M008-withdrawn","M008-restored"))
assert {k:b[k] for k in a}==a
assert {k:b[k] for k in c}==c=={k:d[k] for k in c}
assert set(b)-set(c)=={("0a:58:f0:01:00:0a","10.244.240.10"),
                       ("0a:58:f0:01:00:0a","2001:db8:77::10")}
assert d[("0a:58:f0:01:00:0a","10.244.240.10")] != a[("0a:58:f0:01:00:0a","10.244.240.10")]
print("M008 exact IPv4/IPv6 withdrawal and survivor UUID preservation=PASS")
PY

  rt ovn-nbctl lsp-set-addresses "${a}" '0a:58:f0:01:00:0a 10.244.240.10 2001:db8:77::10'
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M009-before dual-a
  pid="$(rt cat /var/run/ovn/ovnsb_db.pid)"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  rt cat "/proc/${pid}/cmdline" >"${EVIDENCE_DIR}/M009-sb-process.bin"
  tr '\0' ' ' <"${EVIDENCE_DIR}/M009-sb-process.bin" | grep -q 'ovnsb_db.db'
  rt cat "/proc/${pid}/stat" >"${EVIDENCE_DIR}/M009-sb-before.stat"
  NATIVE_GAP_PAUSED_SB_PID="${pid}"
  rt kill -STOP "${pid}"
  rt cat "/proc/${pid}/status" >"${EVIDENCE_DIR}/M009-sb-paused-status.txt"
  grep -Eq '^State:[[:space:]]+T' "${EVIDENCE_DIR}/M009-sb-paused-status.txt"
  native_gap_wait M009-unavailable dual-a M009-before
  rt ovn-nbctl lsp-set-addresses "${a}" '0a:58:f0:01:00:0a 10.244.240.10'
  rt ovn-nbctl lsp-set-addresses "${b}" '0a:58:f0:01:00:0b 10.244.240.11 2001:db8:77::11'
  rt ovn-nbctl --format=json --columns=name,addresses list Logical_Switch_Port \
    >"${EVIDENCE_DIR}/M009-nb-during.json"
  python3 - "${EVIDENCE_DIR}/M009-nb-during.json" "${a}" "${b}" <<'PY'
import json,sys
table=json.load(open(sys.argv[1]))
rows={row[0]:row[1] for row in table["data"]}
assert rows[sys.argv[2]] == "0a:58:f0:01:00:0a 10.244.240.10", rows
assert rows[sys.argv[3]] == "0a:58:f0:01:00:0b 10.244.240.11 2001:db8:77::11", rows
print("M009 NB mutations committed while SB process stopped=PASS")
PY
  for phase in 1 2 3; do
    sleep 3
    native_gap_snapshot "M009-frozen-${phase}" dual-a M009-before
    date -u +%FT%TZ >"${EVIDENCE_DIR}/M009-frozen-${phase}-time.txt"
  done
  native_gap_restore_sb
  rt cat "/proc/${pid}/cmdline" >"${EVIDENCE_DIR}/M009-sb-recovered-process.bin"
  cmp "${EVIDENCE_DIR}/M009-sb-process.bin" "${EVIDENCE_DIR}/M009-sb-recovered-process.bin"
  rt cat "/proc/${pid}/stat" >"${EVIDENCE_DIR}/M009-sb-after.stat"
  [[ "$(awk '{print $22}' "${EVIDENCE_DIR}/M009-sb-before.stat")" == \
     "$(awk '{print $22}' "${EVIDENCE_DIR}/M009-sb-after.stat")" ]]
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M009-recovered dual-b
  rt ovn-nbctl lsp-set-addresses "${b}" '0a:58:f0:01:00:0b 10.244.240.11'
  rt ovn-nbctl --timeout=30 --wait=hv sync
  native_gap_wait M009-cleaned base
  python3 - "${EVIDENCE_DIR}" <<'PY' | tee "${EVIDENCE_DIR}/M009-result.txt"
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
def paths(n):
    return {(x["mac"],x["ip"]):x["uuid"] for x in json.loads((r/(n+"-verified.json")).read_text())["receipts"]}
a,b,c=(paths(n) for n in ("M009-before","M009-recovered","M009-cleaned"))
common=set(a)&set(b)&set(c)
assert {k:a[k] for k in common}=={k:b[k] for k in common}=={k:c[k] for k in common}
assert set(a)-set(b)=={("0a:58:f0:01:00:0a","2001:db8:77::10")}
assert set(b)-set(a)=={("0a:58:f0:01:00:0b","2001:db8:77::11")}
print("M009 SBDB outage frozen and exact recovery=PASS")
PY
  native_gap_capture_stop
}
