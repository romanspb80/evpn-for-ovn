#!/usr/bin/env bash
# Sourced only by the disposable CNI runner with EVPN_EXPORT_TEST=1.

K8GOBGP_IMAGE='ghcr.io/purelb/k8gobgp@sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de'
K8GOBGP_IMAGE_ID='sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0'

export_setup_provider() {
  local image="${K8GOBGP_IMAGE}"
  e2e_fetch_pinned_artifact \
    https://github.com/purelb/k8gobgp/releases/download/v0.2.8/install.yaml \
    f28c6fe7e45b5e535524de09fd13986c6956e112903c960e14f5b1cdeae91008 \
    k8gobgp-v0.2.8-install.yaml "${EVIDENCE_DIR}/provider-install.yaml"
  sed "s|ghcr.io/purelb/k8gobgp:0.2.8|${image}|" "${EVIDENCE_DIR}/provider-install.yaml" | k apply -f -
  k -n k8gobgp-system patch ds k8gobgp --type=strategic -p \
    "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"${WORKER}\"},\"volumes\":[{\"name\":\"gobgp-socket\",\"emptyDir\":null,\"hostPath\":{\"path\":\"/run/gobgp-export-test\",\"type\":\"DirectoryOrCreate\"}}]}}}}"
  k wait --for=condition=Established crd/configs.bgp.purelb.io --timeout=60s
  EXPORT_FLAGS="--evpn-export-config=/run/evpn-node-agent/export-config.json --evpn-export-status=/run/evpn-node-agent/export-status.json --evpn-export-interval=1s --evpn-export-timeout=3s"
  EXPORT_WAIT='while ! test -f /run/evpn-node-agent/export-config.json; do sleep 1; done'
  EXPORT_MOUNTS='        - {name: gobgp, mountPath: /var/run/gobgp, readOnly: true}'
  EXPORT_VOLUMES='      - name: gobgp
        hostPath: {path: /run/gobgp-export-test, type: DirectoryOrCreate}'
}

export_record_provider() {
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
  # Pipe captures also work with snap-confined kubectl, which cannot open host evidence files.
  k -n k8gobgp-system get ds k8gobgp -o json | tee "${EVIDENCE_DIR}/provider-daemonset.json" >/dev/null
  k -n k8gobgp-system get pod "${PROVIDER_POD}" -o json | tee "${EVIDENCE_DIR}/provider-pod.json" >/dev/null
  k get crd configs.bgp.purelb.io -o json | tee "${EVIDENCE_DIR}/provider-crd.json" >/dev/null
  k -n k8gobgp-system get bgpconfig -o json | tee "${EVIDENCE_DIR}/provider-configs.json" >/dev/null
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgpd --version |
    tee "${EVIDENCE_DIR}/speaker-version.txt"
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- ps -o pid,args | tee "${EVIDENCE_DIR}/provider-processes.txt" >/dev/null
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgp global | tee "${EVIDENCE_DIR}/speaker-global.txt" >/dev/null
  k -n k8gobgp-system logs "${PROVIDER_POD}" | tee "${EVIDENCE_DIR}/provider-startup.log" >/dev/null
  docker image inspect "${K8GOBGP_IMAGE}" >"${EVIDENCE_DIR}/provider-image.json"
  python3 - "${EVIDENCE_DIR}" "${K8GOBGP_IMAGE}" "${K8GOBGP_IMAGE_ID}" <<'PY'
import json, pathlib, sys
r = pathlib.Path(sys.argv[1])
pod = json.loads((r / "provider-pod.json").read_text())
ds = json.loads((r / "provider-daemonset.json").read_text())
image = json.loads((r / "provider-image.json").read_text())[0]
crd = json.loads((r / "provider-crd.json").read_text())
container = next(c for c in pod["spec"]["containers"] if c["name"] == "k8gobgp")
status = next(c for c in pod["status"]["containerStatuses"] if c["name"] == "k8gobgp")
if container["image"] != sys.argv[2] or not status["ready"]:
    raise SystemExit("k8gobgp pod image/readiness mismatch")
if image["Id"] != sys.argv[3] or image["Config"]["Labels"]["org.opencontainers.image.version"] != "0.2.8":
    raise SystemExit("k8gobgp release image identity mismatch")
allowed_ids = {sys.argv[3], *image.get("RepoDigests", [])}
if status["imageID"].removeprefix("docker-pullable://") not in allowed_ids:
    raise SystemExit("running k8gobgp imageID does not match the pinned image")
if ds["status"].get("numberReady", 0) != ds["status"]["desiredNumberScheduled"]:
    raise SystemExit("k8gobgp DaemonSet is not ready")
if not any(v["name"] == "v1" and v["served"] and v["storage"] for v in crd["spec"]["versions"]):
    raise SystemExit("expected bgp.purelb.io/v1 CRD is not installed")
processes = (r / "provider-processes.txt").read_text()
if "/usr/local/bin/manager" not in processes or "/usr/local/bin/gobgpd" not in processes:
    raise SystemExit("managed k8gobgp processes not running")
version = (r / "speaker-version.txt").read_text()
if "PureLB-fork:01.03.08" not in version or "[base: gobgp-4.9.0]" not in version:
    raise SystemExit("unexpected managed GoBGP version")
configs = json.loads((r / "provider-configs.json").read_text())["items"]
if len(configs) != 1:
    raise SystemExit("expected one authoritative BGPConfiguration")
if any(configs[0]["spec"].get(k, {}).get("enabled", False) for k in ("netlinkImport", "netlinkExport")):
    raise SystemExit("k8gobgp Netlink writer competes with the Framework")
summary = {"k8gobgp": "0.2.8", "image": sys.argv[2], "imageID": status["imageID"],
           "gobgp": version.strip(), "crd": "bgp.purelb.io/v1", "managed": True}
(r / "provider-runtime.json").write_text(json.dumps(summary, indent=2) + "\n")
print("managed-k8gobgp-v0.2.8-runtime=PASS")
PY
}

export_setup_config() {
  OVN_POD="$(k -n cni-test get pod -l app=ovn-runtime -o jsonpath='{.items[0].metadata.name}')"
  LOCAL_CHASSIS="$(rt ovs-vsctl --bare get Open_vSwitch . external_ids:system-id | tr -d '"')"
  LOCAL_VTEP="$(k get node "${WORKER}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
  cat > "${EVIDENCE_DIR}/provider-config.yaml" <<EOF
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata: {name: cni-export, namespace: k8gobgp-system}
spec:
  global:
    asn: 64512
    routerID: "${LOCAL_VTEP}"
    listenPort: 1179
    listenAddresses: ["127.0.0.1"]
    families: ["l2vpn-evpn"]
EOF
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
  PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgpd --version | tee "${EVIDENCE_DIR}/speaker-version.txt"
  docker exec "${WORKER}" mkdir -p /run/evpn-node-agent
  cat > "${EVIDENCE_DIR}/export-config.json" <<EOF
{"Enabled":true,"DryRun":true,${EXPORT_PROCESS_CONFIG:-}"NodeName":"${WORKER}","LocalChassis":"${LOCAL_CHASSIS}","GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Networks":[{"NetworkID":"n24","LogicalSwitch":"ls24b","Enabled":true,"VNI":100,"RouteDistinguisher":"64512:100","RouteTargets":["64512:100"],"LocalVTEP":"${LOCAL_VTEP}"}]}
EOF
  docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.json' < "${EVIDENCE_DIR}/export-config.json"
}

export_test() {
  ag env EVPN_EXPORT_LIVE=disposable EVPN_EXPORT_VTEP="${LOCAL_VTEP}" \
    EVPN_EXPORT_COUNT="${1}" EVPN_EXPORT_DESIRED="${2}" EVPN_EXPORT_FOREIGN="${EXPORT_FOREIGN:-no}" \
    EVPN_EXPORT_APPLIED="${EXPORT_APPLIED:-$1}" EVPN_EXPORT_UNCLAIMED="${EXPORT_UNCLAIMED:-no}" \
    EVPN_EXPORT_ERROR="${EXPORT_ERROR:-}" EVPN_EXPORT_STAGE="${EXPORT_STAGE:-}" \
    /usr/local/bin/nodeagent.test -test.run '^TestCNIExporterRIB_Live$' -test.v
}
export_check() {
  local count="$1" desired="$2" label="$3" deadline=$((SECONDS+90))
  until export_test "${count}" "${desired}" 2>&1 | tee "${EVIDENCE_DIR}/export-${label}.log" >/dev/null &&
      grep -q '^--- PASS: TestCNIExporterRIB_Live' "${EVIDENCE_DIR}/export-${label}.log"; do
    if (( SECONDS>=deadline )); then cat "${EVIDENCE_DIR}/export-${label}.log"; return 1; fi
    sleep 2
  done
  cat "${EVIDENCE_DIR}/export-${label}.log"
}
export_pause_controller() {
  OVN_CONTROLLER_PID="$(rt cat /var/run/ovn/ovn-controller.pid)"
  rt kill -STOP "${OVN_CONTROLLER_PID}"
}
export_replace_config() {
  docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.next && mv /run/evpn-node-agent/export-config.next /run/evpn-node-agent/export-config.json' < "$1"
}
export_before_pods() {
  sed 's/"DryRun":true/"DryRun":false/' "${EVIDENCE_DIR}/export-config.json" > "${EVIDENCE_DIR}/export-live.json"
  export_replace_config "${EVIDENCE_DIR}/export-live.json"
  EXPORT_STAGE=unconfigured EXPORT_ERROR="speaker not ready" export_check 0 0 unconfigured-speaker
  k apply -f "${EVIDENCE_DIR}/provider-config.yaml"
  export_check 0 0 configured-speaker
  ag env EVPN_PROVIDER_DISPOSABLE=yes EVPN_PROVIDER_TARGET=unix:///var/run/gobgp/gobgp.sock \
    /usr/local/bin/evpn.test -test.run '^TestDisposableProviderType2$' -test.v | tee "${EVIDENCE_DIR}/provider-safety-regressions.log"
  grep -q '^--- PASS: TestDisposableProviderType2' "${EVIDENCE_DIR}/provider-safety-regressions.log"
  k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/bgpconfig-before.json" >/dev/null
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgp global | tee "${EVIDENCE_DIR}/speaker-before.txt"
  export_replace_config "${EVIDENCE_DIR}/export-config.json"
  export_check 0 0 dry-initial
  export_pause_controller
}

export_verify_dry_and_enable() {
  export_check 0 0 before-readiness
  rt kill -CONT "${OVN_CONTROLLER_PID}"
  export_check 0 2 dry-ready
  LSP_A="$(rt ovn-nbctl --bare --columns=name find Logical_Switch_Port 'addresses{>=}"0a:58:f0:00:00:0a 10.244.240.10/24"')"
  [[ -n "${LSP_A}" ]]
  export_pause_controller
  rt ovn-nbctl set Logical_Switch_Port "${LSP_A}" external_ids:evpn.framework/managed-by=foreign
  export_check 0 1 dry-wrong-owner
  rt ovn-nbctl set Logical_Switch_Port "${LSP_A}" external_ids:evpn.framework/managed-by=evpn-node-agent
  export_check 0 2 dry-owner-restored
  rt ovn-sbctl set Port_Binding "${LSP_A}" up=false
  export_check 0 1 dry-pb-down
  rt ovn-sbctl set Port_Binding "${LSP_A}" up=true
  export_check 0 2 dry-pb-restored
  CHASSIS_UUID="$(rt ovn-sbctl --bare --columns=_uuid find Chassis "name=${LOCAL_CHASSIS}")"
  rt ovn-sbctl set Chassis "${CHASSIS_UUID}" name=export-test-wrong-chassis
  export_check 0 0 dry-wrong-chassis
  rt ovn-sbctl set Chassis "${CHASSIS_UUID}" "name=${LOCAL_CHASSIS}"
  rt kill -CONT "${OVN_CONTROLLER_PID}"
  export_check 0 2 dry-restored
  EXPORT_STAGE=seed-foreign export_test 0 2 | tee "${EVIDENCE_DIR}/export-foreign-seed.log"
  EXPORT_FOREIGN=yes
  export_replace_config "${EVIDENCE_DIR}/export-live.json"
  export_check 2 2 real-ready
  ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/first-applied.json" >/dev/null
  sleep 4
  export_check 2 2 real-idempotent
  ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/second-applied.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
def receipts(name): return {e["Endpoint"]["EndpointID"]:e["PathUUID"] for e in json.loads((r/name).read_text())["Result"]["Applied"]}
assert receipts("first-applied.json")==receipts("second-applied.json")
print("confirmed-UUID-idempotence=PASS")
PY
}

export_failures() {
  local sbpid
  sbpid="$(rt cat /var/run/ovn/ovnsb_db.pid)"
  rt kill -STOP "${sbpid}"
  EXPORT_ERROR="endpoint observation failed" export_check 2 0 observer-error
  rt kill -CONT "${sbpid}"
  export_check 2 2 observer-restored
}

export_finish() {
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgp global rib -a evpn -j | tee "${EVIDENCE_DIR}/rib-after-del.json"
  create_pod a
  create_pod b
  k -n cni-test wait --for=condition=Ready pod/pod-a pod/pod-b --timeout=180s
  export_check 2 2 before-speaker-outage
  local failure_a failure_b
  failure_a="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  failure_b="$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')"
  local pid
  pid="$(k -n k8gobgp-system exec "${PROVIDER_POD}" -- ps -o pid,comm | awk '$2=="gobgpd"{print $1}')"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -STOP "${pid}"
  sleep 5
  ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/speaker-api-error.json"
  python3 - "${EVIDENCE_DIR}/speaker-api-error.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert "speaker GetBgp" in s["Error"],s
assert len(s["Result"]["Applied"])==2,s
print("speaker-API-outage-retains-confirmed-receipts=PASS")
PY
  k -n k8gobgp-system exec "${PROVIDER_POD}" -- kill -CONT "${pid}"
  EXPORT_ERROR="speaker continuity lost" export_check 2 0 api-restored-failclosed
  k -n cni-test delete pod pod-a pod-b --wait=true --timeout=90s
  wait_absent "${failure_a}"
  wait_absent "${failure_b}"
  EXPORT_ERROR="speaker continuity lost" export_check 2 0 del-after-continuity-loss-fenced
  k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/bgpconfig-after.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
a=json.loads((r/"bgpconfig-before.json").read_text());b=json.loads((r/"bgpconfig-after.json").read_text())
assert a["spec"]==b["spec"] and a["metadata"]["generation"]==b["metadata"]["generation"]
print("BGPConfiguration-ownership-preserved=PASS")
PY
  EXPORT_STAGE=delete-foreign export_test 0 0 | tee "${EVIDENCE_DIR}/foreign-cleanup.log"
  EXPORT_FOREIGN=no
  EXPORT_ERROR="speaker continuity lost" export_check 2 0 final-failclosed-rib
  snapshot failclosed-final
  k -n k8gobgp-system logs "${PROVIDER_POD}" | tee "${EVIDENCE_DIR}/provider.log" >/dev/null
  echo "CNI-MVP-3.2a.3 actual-Multus-export=PASS"
}
