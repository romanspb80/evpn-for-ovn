#!/usr/bin/env bash
# Independent manager/speaker restarts use the existing test-only supervisor.
# The production k8gobgp image and implementation are not changed.

provider_exec() { k -n k8gobgp-system exec "${PROVIDER_POD}" -- "$@"; }
continuity_setup_provider() {
  EXPORT_HOST_PID='      hostPID: true'
  EXPORT_PROCESS_CONFIG='"ProcessContinuity":true,'
  k -n k8gobgp-system create configmap continuity-supervisor --from-file=supervisor.sh="${ROOT}/tools/bgp-provider-test-supervisor.sh"
  k -n k8gobgp-system patch ds k8gobgp --type=strategic -p \
    '{"spec":{"template":{"spec":{"volumes":[{"name":"continuity-supervisor","configMap":{"name":"continuity-supervisor"}}],"containers":[{"name":"k8gobgp","command":["/bin/bash","/test/supervisor.sh"],"volumeMounts":[{"name":"continuity-supervisor","mountPath":"/test","readOnly":true}]}]}}}}'
}

continuity_status() {
  local label="$1" substring="$2" applied="$3" deadline=$((SECONDS+45))
  while :; do
    ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/continuity-${label}.json" >/dev/null
    if python3 - "${EVIDENCE_DIR}/continuity-${label}.json" "${substring}" "${applied}" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
ok=(sys.argv[2] in s["Error"] and len(s["Result"]["Applied"] or [])==int(sys.argv[3])
    and s["SpeakerProcess"]["PID"]>0 and s["SpeakerProcess"]["StartTicks"] and s["SpeakerProcess"]["BootID"])
sys.exit(0 if ok else 1)
PY
    then break; fi
    if (( SECONDS>=deadline )); then cat "${EVIDENCE_DIR}/continuity-${label}.json"; return 1; fi
    sleep 2
  done
}

continuity_same_receipts() {
  python3 - "${EVIDENCE_DIR}/continuity-$1.json" "${EVIDENCE_DIR}/continuity-$2.json" <<'PY'
import json,sys
a,b=(json.load(open(f)) for f in sys.argv[1:])
assert a["SpeakerProcess"]==b["SpeakerProcess"],(a,b)
def receipts(s): return {r["Endpoint"]["EndpointID"]:(r["Endpoint"]["Incarnation"],r["PathUUID"]) for r in s["Result"]["Applied"]}
assert receipts(a)==receipts(b),(a,b)
print("same-live-process-and-confirmed-UUID-receipts=PASS")
PY
}

continuity_wait_file() {
  local deadline=$((SECONDS+90))
  until provider_exec test -f "/tmp/$1"; do
    (( SECONDS<deadline )) || return 1
    sleep 2
  done
}

continuity_pause() {
  SPEAKER_PID="$(provider_exec cat /tmp/speaker.pid)"
  [[ "${SPEAKER_PID}" =~ ^[0-9]+$ ]]
  provider_exec kill -STOP "${SPEAKER_PID}"
}

continuity_transients() {
  continuity_status initial "" 2
  ag env EVPN_EXPORT_LIVE=disposable /usr/local/bin/nodeagent.test \
    -test.run '^TestSpeakerContinuityReconnect_Live$' -test.v | tee "${EVIDENCE_DIR}/actual-grpc-reconnect.log"
  grep -q '^--- PASS: TestSpeakerContinuityReconnect_Live' "${EVIDENCE_DIR}/actual-grpc-reconnect.log"
  provider_exec cat /tmp/speaker.pid /tmp/manager.pid | tee "${EVIDENCE_DIR}/manager-before-pids.txt"
  provider_exec touch /tmp/restart-manager
  continuity_wait_file manager-restarted
  sleep 3
  export_check 2 2 manager-only-restart
  continuity_status manager-restored "" 2
  continuity_same_receipts initial manager-restored
  provider_exec cat /tmp/speaker.pid /tmp/manager.pid | tee "${EVIDENCE_DIR}/manager-after-pids.txt"

  for round in 1 2; do
    continuity_pause
    continuity_status "outage-${round}" "speaker GetBgp" 2
    provider_exec kill -CONT "${SPEAKER_PID}"
    export_check 2 2 "transient-recovered-${round}"
    continuity_status "restored-${round}" "" 2
    continuity_same_receipts initial "restored-${round}"
  done

  local old_uid new_uid sbpid
  old_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  continuity_pause
  continuity_status deletion-outage "speaker GetBgp" 2
  k -n cni-test delete pod pod-a --wait=true --timeout=90s
  wait_absent "${old_uid}"
  continuity_status deleted-while-frozen "speaker GetBgp" 2
  provider_exec kill -CONT "${SPEAKER_PID}"
  export_check 1 1 delete-after-transient
  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  export_check 2 2 before-reincarnation-outage
  old_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  continuity_pause
  continuity_status reincarnation-outage "speaker GetBgp" 2
  k -n cni-test delete pod pod-a --wait=true --timeout=90s
  wait_absent "${old_uid}"
  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  new_uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  [[ "${old_uid}" != "${new_uid}" ]]
  sbpid="$(rt cat /var/run/ovn/ovnsb_db.pid)"
  rt kill -STOP "${sbpid}"
  continuity_status observer-before-reconnect "endpoint observation failed" 2
  provider_exec kill -CONT "${SPEAKER_PID}"
  EXPORT_ERROR="endpoint observation failed" export_check 2 0 observer-during-recovery
  rt kill -CONT "${sbpid}"
  export_check 2 2 reincarnation-recovered
  continuity_status reincarnation-restored "" 2
  python3 - "${EVIDENCE_DIR}/continuity-reincarnation-outage.json" "${EVIDENCE_DIR}/continuity-reincarnation-restored.json" "${old_uid}" "${new_uid}" <<'PY'
import json,sys
a,b=(json.load(open(f)) for f in sys.argv[1:3])
old=next(r for r in a["Result"]["Applied"] if r["Endpoint"]["EndpointID"].endswith(sys.argv[3]))
new=next(r for r in b["Result"]["Applied"] if r["Endpoint"]["EndpointID"].endswith(sys.argv[4]))
assert old["PathUUID"]!=new["PathUUID"] and old["Endpoint"]["Incarnation"]!=new["Endpoint"]["Incarnation"]
assert all(not r["Endpoint"]["EndpointID"].endswith(sys.argv[3]) for r in b["Result"]["Applied"])
assert a["SpeakerProcess"]==b["SpeakerProcess"]
print("outage-incarnation-replacement-with-fresh-confirmed-UUID=PASS")
PY
}

continuity_restarts() {
  continuity_status before-speaker-restart "" 2
  k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/bgpconfig-before-manual-reinitialization.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
a=json.loads((r/"bgpconfig-before.json").read_text())
b=json.loads((r/"bgpconfig-before-manual-reinitialization.json").read_text())
assert a["spec"]==b["spec"] and a["metadata"]["generation"]==b["metadata"]["generation"]
print("exporter-did-not-write-BGPConfiguration=PASS")
PY
  provider_exec touch /tmp/restart-speaker
  continuity_wait_file speaker-restarted
  sleep 5
  provider_exec sh -c 'wget -qO- http://127.0.0.1:7474/readyz; gobgp global' | tee "${EVIDENCE_DIR}/speaker-restart-readyz-and-global.txt"
  EXPORT_APPLIED=2 EXPORT_FOREIGN=no EXPORT_STAGE=restarted-unconfigured EXPORT_ERROR="speaker continuity lost" \
    export_check 0 0 restarted-ASN-zero
  # A supported BGPConfiguration spec event is manual intervention, not an
  # exporter write and not a claim of periodic provider recovery.
  k -n k8gobgp-system patch bgpconfig cni-export --type=merge \
    -p '{"spec":{"neighbors":[{"config":{"neighborAddress":"192.0.2.254","peerAsn":64513,"description":"continuity-reinitialize"},"transport":{"passiveMode":true},"afiSafis":[{"family":"l2vpn-evpn","enabled":true}]}]}}'
  local deadline=$((SECONDS+90))
  until provider_exec gobgp global | grep -q '64512'; do
    (( SECONDS<deadline )) || return 1
    sleep 2
  done
  EXPORT_APPLIED=2 EXPORT_FOREIGN=no EXPORT_ERROR="speaker continuity lost" export_check 0 0 restarted-empty-rib-fenced
  continuity_status invalid-old-receipts "speaker continuity lost" 2
  python3 - "${EVIDENCE_DIR}/continuity-invalid-old-receipts.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert s["Result"]["OwnershipUnverified"]
assert "original speaker process exited" in s["Error"]
print("old-speaker-receipts-invalidated-for-mutation=PASS")
PY
  EXPORT_STAGE=seed-foreign export_test 0 0 | tee "${EVIDENCE_DIR}/restart-new-foreign.log"
  EXPORT_STAGE=seed-conflict export_test 0 0 | tee "${EVIDENCE_DIR}/restart-conflicting-route.log"
  EXPORT_APPLIED=2 EXPORT_UNCLAIMED=yes EXPORT_ERROR="speaker continuity lost" export_check 1 0 restarted-conflicting-rib-fenced
  provider_exec gobgp global rib -a evpn -j | tee "${EVIDENCE_DIR}/restart-conflicting-rib.json"

  local container current ready
  container="$(k -n k8gobgp-system get pod "${PROVIDER_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}')"
  provider_exec kill -TERM 1
  deadline=$((SECONDS+150))
  while :; do
    current="$(k -n k8gobgp-system get pod "${PROVIDER_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}')"
    ready="$(k -n k8gobgp-system get pod "${PROVIDER_POD}" -o jsonpath='{.status.containerStatuses[0].ready}')"
    [[ -n "${current}" && "${current}" != "${container}" && "${ready}" == true ]] && break
    (( SECONDS<deadline )) || return 1
    sleep 2
  done
  printf 'before=%s\nafter=%s\n' "${container}" "${current}" | tee "${EVIDENCE_DIR}/container-identities.txt"
  EXPORT_APPLIED=2 EXPORT_FOREIGN=no EXPORT_ERROR="speaker continuity lost" export_check 0 0 full-container-restart-fenced
  provider_exec gobgp global | tee "${EVIDENCE_DIR}/full-container-global.txt"
  EXPORT_STAGE=seed-foreign export_test 0 0 | tee "${EVIDENCE_DIR}/container-new-foreign.log"
  EXPORT_STAGE=seed-conflict export_test 0 0 | tee "${EVIDENCE_DIR}/container-conflicting-route.log"
  EXPORT_APPLIED=2 EXPORT_UNCLAIMED=yes EXPORT_ERROR="speaker continuity lost" export_check 1 0 container-foreign-preserved

  # Restart the real exporter/node-agent, not a mock reconciler. Managed Store
  # and receipts are process-local; unknown RIB paths must remain untouched.
  local agent_pid
  agent_pid="$(ag cat /run/evpn-node-agent/agent.pid)"
  [[ "${agent_pid}" =~ ^[0-9]+$ ]]
  ag kill -TERM "${agent_pid}"
  sleep 4
  EXPORT_APPLIED=0 EXPORT_UNCLAIMED=yes export_check 1 0 exporter-restart-no-adoption
  continuity_status exporter-restarted "" 0
  python3 - "${EVIDENCE_DIR}/continuity-before-speaker-restart.json" "${EVIDENCE_DIR}/continuity-exporter-restarted.json" <<'PY'
import json,sys
a,b=(json.load(open(p)) for p in sys.argv[1:])
assert a["SpeakerProcess"]!=b["SpeakerProcess"],(a,b)
assert not b["Result"]["Applied"]
print("new-exporter-preserves-unknown-RIB-without-adopting-old-receipts=PASS")
PY
}

export_finish() {
  provider_exec gobgp global rib -a evpn -j | tee "${EVIDENCE_DIR}/rib-after-normal-del.json"
  create_pod a
  create_pod b
  k -n cni-test wait --for=condition=Ready pod/pod-a pod/pod-b --timeout=180s
  export_check 2 2 continuity-initial
  continuity_transients
  continuity_restarts
  snapshot continuity-final
  provider_exec gobgp global rib -a evpn -j | tee "${EVIDENCE_DIR}/continuity-final-rib.json"
  k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/bgpconfig-after-manual-reinitialization.json" >/dev/null
  k -n k8gobgp-system logs "${PROVIDER_POD}" | tee "${EVIDENCE_DIR}/provider-final.log" >/dev/null
  echo "CNI-MVP-3.2b.1 transient-recovery=PASS restart-automatic-recovery=UNSUPPORTED restart-safety=PASS"
}
