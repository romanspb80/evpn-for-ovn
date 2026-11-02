#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-framework-netlink-e2e-c4k}"
EVIDENCE_ROOT="${EVIDENCE_ROOT:-/tmp/framework-netlink-e2e-c4k-evidence}"
RUN_SUFFIX="$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-${RUN_SUFFIX}"
EVIDENCE_DIR="${EVIDENCE_ROOT}/attempt-${RUN_ID}"
LOGICAL_SWITCH="fw-${RUN_SUFFIX}"
NAMESPACE="framework-e2e"
AGENT_IMAGE="evpn4ovn/framework-netlink-agent:${KIND_CLUSTER}"
APP_IMAGE="evpn4ovn/framework-netlink-app:${KIND_CLUSTER}"
OVN_IMAGE="evpn4ovn/ovn-ovs:kind-26.09"
OVN_ID="sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
BASE_ID="sha256:f01f29f4d8b1fd4f377103015aa7b7dab6f8189416ce2a363f63d7e538c4e5a7"
APP_BASE_ID="sha256:b5c41de41ae7df5b20351b3c44ec3ce65afeaa6b06a5d482fb2fd12e2460cc45"
NODE_IMAGE="kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c"
FRAMEWORK_VNI=1000
FRAMEWORK_BRIDGE_DEVICE="br-evpn${FRAMEWORK_VNI}"
FRAMEWORK_VXLAN_DEVICE="vxlan${FRAMEWORK_VNI}"
FRAMEWORK_ADVERTISE_DEVICE="evpnadv${FRAMEWORK_VNI}"
REMOTE_STATE_DIR="/run/evpn-node-agent/remote-state-${RUN_SUFFIX}"
SHARED_DIR="/run/framework-netlink-e2e-${RUN_SUFFIX}"
WORKER="${KIND_CLUSTER}-worker"
KUBECONFIG="${EVIDENCE_DIR}/kubeconfig"
CREATED_CLUSTER=0
CLUSTER_CREATE_ATTEMPTED=0
CREATED_FFR=0
CREATED_CAPTURE=0
PROVISION_ATTEMPTED=0
OWNED_NETLINK_PREREQUISITES=0
AGENT_POD=""
OVN_POD=""
PROVIDER_POD=""
cleanup_rc=0
capture_running=0
REMOTE_WORKLOAD_PID=""

export KIND_CLUSTER EVIDENCE_DIR WORKER
source "${ROOT}/tools/lib/cni-agent-image-test.sh"
source "${ROOT}/tools/lib/cni-evpn-export-test.sh"
source "${ROOT}/tools/lib/cni-evpn-frr-interop-test.sh"
source "${ROOT}/tools/lib/containerlab-e2e.sh"
e2e_lab_acquire

RT_CAPTURE_SEQUENCE=0

export KUBECONFIG
k() { kubectl --context "kind-${KIND_CLUSTER}" "$@"; }
rt() {
  local remote_path host_path status capture_rc
  RT_CAPTURE_SEQUENCE=$((RT_CAPTURE_SEQUENCE + 1))
  remote_path="/framework-e2e-share/.framework-e2e-rt-${RUN_SUFFIX}-${RT_CAPTURE_SEQUENCE}"
  host_path="${SHARED_DIR}/.framework-e2e-rt-${RUN_SUFFIX}-${RT_CAPTURE_SEQUENCE}"
  if ! kubectl --context "kind-${KIND_CLUSTER}" -n ovn-system \
    exec "${OVN_POD}" -c ovn -- sh -c \
    'output="$1"; shift; command "$@" >"$output" 2>"$output.err"; printf "%s\n" "$?" >"$output.status"' \
    sh "${remote_path}" "$@"; then
    return 1
  fi
  status="$(docker exec "${WORKER}" cat "${host_path}.status")"
  if [[ "${status}" != 0 ]]; then
    docker exec "${WORKER}" cat "${host_path}.err" >&2 || true
    docker exec "${WORKER}" rm -f "${host_path}" "${host_path}.err" "${host_path}.status"
    return 1
  fi
  docker exec "${WORKER}" cat "${host_path}"
  capture_rc=$?
  docker exec "${WORKER}" rm -f "${host_path}" "${host_path}.err" "${host_path}.status"
  return "${capture_rc}"
}
ag() { docker exec "${WORKER}" "$@"; }
fwtest() {
  docker exec \
    -e FRAMEWORK_NETLINK_E2E=1 \
    -e FRAMEWORK_E2E_ACTION="${FRAMEWORK_E2E_ACTION}" \
    -e FRAMEWORK_E2E_RUN_ID="${RUN_SUFFIX}" \
    -e FRAMEWORK_E2E_LOGICAL_SWITCH="${LOGICAL_SWITCH}" \
    -e FRAMEWORK_E2E_OVSDB_MGMT=127.0.0.1:50053 \
    "${WORKER}" /usr/local/bin/framework-nodeagent.test \
    -test.run '^TestFrameworkNetlinkEndpointLifecycle_Live$' -test.v
}

create_netlink_prerequisites() {
  local device
  for device in "${FRAMEWORK_BRIDGE_DEVICE}" "${FRAMEWORK_VXLAN_DEVICE}" "${FRAMEWORK_ADVERTISE_DEVICE}"; do
    if docker exec "${WORKER}" ip link show "${device}" >/dev/null 2>&1; then
      echo "refusing pre-existing integration device ${device}" >&2
      return 1
    fi
  done
  OWNED_NETLINK_PREREQUISITES=1
  docker exec "${WORKER}" ip link add "${FRAMEWORK_BRIDGE_DEVICE}" type bridge
  docker exec "${WORKER}" ip link set "${FRAMEWORK_BRIDGE_DEVICE}" up
  docker exec "${WORKER}" ip link add "${FRAMEWORK_VXLAN_DEVICE}" type vxlan id "${FRAMEWORK_VNI}" local "${LOCAL_VTEP}" dstport 60100 nolearning
  docker exec "${WORKER}" ip link set "${FRAMEWORK_VXLAN_DEVICE}" master "${FRAMEWORK_BRIDGE_DEVICE}"
  docker exec "${WORKER}" ip link set "${FRAMEWORK_VXLAN_DEVICE}" up
  docker exec "${WORKER}" ip link add "${FRAMEWORK_ADVERTISE_DEVICE}" type dummy
  docker exec "${WORKER}" ip link set "${FRAMEWORK_ADVERTISE_DEVICE}" master "${FRAMEWORK_BRIDGE_DEVICE}"
  docker exec "${WORKER}" ip link set "${FRAMEWORK_ADVERTISE_DEVICE}" up
  for device in "${FRAMEWORK_BRIDGE_DEVICE}" "${FRAMEWORK_VXLAN_DEVICE}" "${FRAMEWORK_ADVERTISE_DEVICE}"; do
    docker exec "${WORKER}" ip -d -j link show "${device}" \
      >"${EVIDENCE_DIR}/integration-${device}.json"
  done
  printf 'runner-owned OVN integration links provisioned: %s %s %s\n' \
    "${FRAMEWORK_BRIDGE_DEVICE}" "${FRAMEWORK_VXLAN_DEVICE}" "${FRAMEWORK_ADVERTISE_DEVICE}"
}

remove_netlink_prerequisites() {
  local device kind expected
  [[ "${OWNED_NETLINK_PREREQUISITES}" == 1 ]] || return 0
  docker container inspect "${WORKER}" >/dev/null 2>&1 || return 1
  for device in "${FRAMEWORK_VXLAN_DEVICE}" "${FRAMEWORK_ADVERTISE_DEVICE}" "${FRAMEWORK_BRIDGE_DEVICE}"; do
    if ! docker exec "${WORKER}" ip link show "${device}" >/dev/null 2>&1; then
      continue
    fi
    case "${device}" in
      "${FRAMEWORK_VXLAN_DEVICE}") expected=vxlan ;;
      "${FRAMEWORK_ADVERTISE_DEVICE}") expected=dummy ;;
      "${FRAMEWORK_BRIDGE_DEVICE}") expected=bridge ;;
    esac
    docker exec "${WORKER}" ip -d -j link show "${device}" \
      >"${EVIDENCE_DIR}/cleanup-${device}.json"
    python3 - "${EVIDENCE_DIR}/cleanup-${device}.json" "${expected}" "${FRAMEWORK_VNI}" <<'PY'
import json,sys
link=json.load(open(sys.argv[1]))[0]
assert link.get("linkinfo",{}).get("info_kind")==sys.argv[2],link
if sys.argv[2]=="vxlan":
    data=link["linkinfo"]["info_data"]
    assert int(data["id"])==int(sys.argv[3]),link
PY
    docker exec "${WORKER}" ip link del "${device}"
  done
  for device in "${FRAMEWORK_BRIDGE_DEVICE}" "${FRAMEWORK_VXLAN_DEVICE}" "${FRAMEWORK_ADVERTISE_DEVICE}"; do
    if docker exec "${WORKER}" ip link show "${device}" >/dev/null 2>&1; then
      echo "integration device cleanup failed for ${device}" >&2
      return 1
    fi
    printf 'absent\n' >"${EVIDENCE_DIR}/after-cleanup-${device}.txt"
  done
  OWNED_NETLINK_PREREQUISITES=0
}

stop_remote_capture_keeper() {
  [[ -n "${REMOTE_WORKLOAD_PID}" ]] || return 0
  if docker container inspect "${FRR_CONTAINER}" >/dev/null 2>&1; then
    docker exec "${FRR_CONTAINER}" sh -ec '
      pid="$1"
      cmd="$(tr "\000" " " <"/proc/${pid}/cmdline")"
      printf "%s\n" "${cmd}" | grep -Eq "(^|/)sleep 86400 *$"
      kill "${pid}"
    ' sh "${REMOTE_WORKLOAD_PID}" || {
      cleanup_rc=1
      return 1
    }
  fi
  REMOTE_WORKLOAD_PID=""
}

wait_exporter_empty() {
  local evidence_name="$1" deadline
  deadline=$((SECONDS+120))
  while (( SECONDS < deadline )); do
    if ag cat /run/evpn-node-agent/export-status.json \
      >"${EVIDENCE_DIR}/export-status-${evidence_name}.json" 2>/dev/null &&
      python3 - "${EVIDENCE_DIR}/export-status-${evidence_name}.json" <<'PY' >/dev/null 2>&1
import json,sys
status=json.load(open(sys.argv[1]))
assert not status.get("Error"),status
assert not (status.get("Result") or {}).get("Applied"),status
PY
    then
      return 0
    fi
    sleep 1
  done
  cat "${EVIDENCE_DIR}/export-status-${evidence_name}.json" >&2 2>/dev/null || true
  echo "local Type-2 exporter did not withdraw all routes before integration cleanup" >&2
  return 1
}

capture_stop() {
  local capture_label log pid
  [[ "${capture_running}" == 1 ]] || return 0
  if [[ -n "${AGENT_POD}" ]] && k -n "${NAMESPACE}" get pod "${AGENT_POD}" >/dev/null 2>&1; then
    for log in local-host worker-underlay; do
      ag sh -c 'test ! -f "$1" || cat "$1"' sh \
        "/run/evpn-node-agent/framework-captures/${log}.log" \
        >"${EVIDENCE_DIR}/${log}-capture.log" 2>/dev/null || true
    done
  fi
  for pid in local-host.pid worker-underlay.pid; do
    if [[ -n "${AGENT_POD}" ]] && k -n "${NAMESPACE}" get pod "${AGENT_POD}" >/dev/null 2>&1; then
      ag sh -c 'test ! -s "$1" || kill "$(cat "$1")" 2>/dev/null || true' sh \
        "/run/evpn-node-agent/framework-captures/${pid}" || cleanup_rc=1
    fi
  done
  if [[ "${CREATED_CAPTURE}" == 1 ]]; then
    if docker container inspect "${KIND_CLUSTER}-frr-capture" >/dev/null 2>&1; then
      capture_label="$(docker inspect --format '{{index .Config.Labels "evpn4ovn.test-run"}}' \
        "${KIND_CLUSTER}-frr-capture")"
      if [[ "${capture_label}" != "${KIND_CLUSTER}" ]]; then
        echo "refusing to clean capture container with unexpected ownership label" >&2
        cleanup_rc=1
      else
        docker logs "${KIND_CLUSTER}-frr-capture" \
          >"${EVIDENCE_DIR}/frr-capture.log" 2>&1 || true
        if [[ "$(docker inspect --format '{{.State.Running}}' "${KIND_CLUSTER}-frr-capture")" == true ]]; then
          docker stop --time 5 "${KIND_CLUSTER}-frr-capture" >/dev/null || cleanup_rc=1
        fi
        docker rm "${KIND_CLUSTER}-frr-capture" >/dev/null || cleanup_rc=1
      fi
    fi
    CREATED_CAPTURE=0
  fi
  if [[ -n "${AGENT_POD}" ]] && k -n "${NAMESPACE}" get pod "${AGENT_POD}" >/dev/null 2>&1; then
    for _ in $(seq 1 20); do
      if ag sh -c 'for f in /run/evpn-node-agent/framework-captures/*.pid; do test -e "$f" && kill -0 "$(cat "$f")" 2>/dev/null && exit 1; done'; then
        break
      fi
      sleep 1
    done
    ag cat /run/evpn-node-agent/framework-captures/local-host.pcap >"${EVIDENCE_DIR}/local-host.pcap" || cleanup_rc=1
    ag cat /run/evpn-node-agent/framework-captures/worker-underlay.pcap >"${EVIDENCE_DIR}/worker-underlay.pcap" || cleanup_rc=1
  fi
  capture_running=0
}

write_status() {
  local rc="$1"
  cat >"${EVIDENCE_DIR}/run-status.txt" <<EOF
run=${RUN_ID}
cluster=${KIND_CLUSTER}
result=$([[ "${rc}" == 0 ]] && echo PASS || echo FAIL)
failure-classification=${FAILURE_CLASSIFICATION:-UNRESOLVED}
failure-gate=${FAILURE_GATE:-NOT_REACHED}
completed-at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
}

cleanup() {
  local original_rc=$?
  trap - EXIT INT TERM
  set +e
  capture_stop
  stop_remote_capture_keeper || true
  if [[ "${CREATED_CLUSTER}" == 1 || "${CLUSTER_CREATE_ATTEMPTED}" == 1 ]]; then
    if (( original_rc != 0 )) && kind get clusters 2>/dev/null | grep -Fxq "${KIND_CLUSTER}"; then
      k get pods -A -o wide >"${EVIDENCE_DIR}/pods-at-failure.txt" 2>&1
      k get events -A --sort-by=.lastTimestamp >"${EVIDENCE_DIR}/events-at-failure.txt" 2>&1
      k describe pods -A >"${EVIDENCE_DIR}/pod-descriptions-at-failure.txt" 2>&1
      while IFS='|' read -r namespace pod; do
        [[ -n "${namespace}" && -n "${pod}" ]] || continue
        k -n "${namespace}" logs "${pod}" --all-containers=true \
          >"${EVIDENCE_DIR}/logs-${namespace}-${pod}.txt" 2>&1
        k -n "${namespace}" logs "${pod}" --all-containers=true --previous \
          >"${EVIDENCE_DIR}/logs-${namespace}-${pod}-previous.txt" 2>&1 || true
      done < <(k get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}')
      printf 'failure-classification=%s\nfailure-gate=%s\n' \
        "${FAILURE_CLASSIFICATION:-UNRESOLVED}" "${FAILURE_GATE:-NOT_REACHED}" \
        >"${EVIDENCE_DIR}/failure-summary.txt"
    fi
    if [[ "${PROVISION_ATTEMPTED}" == 1 ]] &&
       docker container inspect "${WORKER}" >/dev/null 2>&1 &&
       k -n "${NAMESPACE}" get service ovsdb-mgmt >/dev/null 2>&1; then
      FRAMEWORK_E2E_ACTION=cleanup \
        fwtest >"${EVIDENCE_DIR}/framework-cleanup.log" 2>&1 || cleanup_rc=1
      if wait_exporter_empty cleanup; then
        FRAMEWORK_E2E_ACTION=delete-network \
          fwtest >"${EVIDENCE_DIR}/framework-network-cleanup.log" 2>&1 || cleanup_rc=1
        remove_netlink_prerequisites || cleanup_rc=1
      else
        cleanup_rc=1
      fi
    else
      remove_netlink_prerequisites || cleanup_rc=1
    fi
    e2e_lab_down || cleanup_rc=1
    CREATED_CLUSTER=0
  fi
  if [[ "${CREATED_FFR}" == 1 ]]; then
    interop_cleanup || cleanup_rc=1
    CREATED_FFR=0
  fi
  test_image_cleanup || cleanup_rc=1
  if [[ -d "${EVIDENCE_DIR}" ]]; then
    local reported_rc="${original_rc:-1}"
    if (( cleanup_rc != 0 && reported_rc == 0 )); then reported_rc=1; fi
    write_status "${reported_rc}"
    printf 'cleanup=%s\n' "$([[ "${cleanup_rc}" == 0 ]] && echo PASS || echo FAIL)" \
      >>"${EVIDENCE_DIR}/run-status.txt"
  fi
  if (( original_rc == 0 && cleanup_rc != 0 )); then original_rc=1; fi
  exit "${original_rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ "${KIND_CLUSTER}" == "framework-netlink-e2e-c4k" ]] || {
  echo "KIND_CLUSTER is fixed for this milestone: framework-netlink-e2e-c4k" >&2; exit 2;
}
[[ ! -L "${EVIDENCE_ROOT}" ]] || { echo "evidence root must not be a symlink" >&2; exit 1; }
if [[ -e "${EVIDENCE_ROOT}" && ! -d "${EVIDENCE_ROOT}" ]]; then
  echo "evidence root exists but is not a directory: ${EVIDENCE_ROOT}" >&2; exit 1
fi
if [[ -e "${EVIDENCE_ROOT}" ]]; then
  [[ -f "${EVIDENCE_ROOT}/.framework-netlink-e2e-owner" && ! -L "${EVIDENCE_ROOT}/.framework-netlink-e2e-owner" ]] || {
    echo "existing evidence root has no regular ownership marker" >&2; exit 1;
  }
  grep -Fxq "repository=${ROOT}" "${EVIDENCE_ROOT}/.framework-netlink-e2e-owner" &&
    grep -Fxq "purpose=Framework Netlink E2E evidence root" "${EVIDENCE_ROOT}/.framework-netlink-e2e-owner" || {
      echo "evidence root ownership marker does not match this repository" >&2; exit 1;
    }
else
  mkdir -m 700 "${EVIDENCE_ROOT}"
  printf 'repository=%s\npurpose=Framework Netlink E2E evidence root\n' "${ROOT}" \
    >"${EVIDENCE_ROOT}/.framework-netlink-e2e-owner"
  chmod 600 "${EVIDENCE_ROOT}/.framework-netlink-e2e-owner"
fi
mkdir -m 700 "${EVIDENCE_DIR}"
mkdir -p "${EVIDENCE_DIR}/build"
printf 'run=%s\ncluster=%s\nlogical-switch=%s\nstarted-at=%s\n' \
  "${RUN_ID}" "${KIND_CLUSTER}" "${LOGICAL_SWITCH}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  >"${EVIDENCE_DIR}/run-metadata.txt"
export FRAMEWORK_E2E_RUN_ID="${RUN_SUFFIX}"
export FRAMEWORK_E2E_LOGICAL_SWITCH="${LOGICAL_SWITCH}"
export FRAMEWORK_E2E_OVSDB_MGMT="127.0.0.1:50053"

if kind get clusters | grep -Fxq "${KIND_CLUSTER}" ||
   docker ps -a --format '{{.Names}}' | grep -Fxq "${KIND_CLUSTER}-worker" ||
   docker ps -a --format '{{.Names}}' | grep -Fxq "${KIND_CLUSTER}-control-plane" ||
   docker container inspect "${FRR_CONTAINER}" >/dev/null 2>&1 ||
   docker network inspect "${INTEROP_NETWORK}" >/dev/null 2>&1; then
  echo "refusing a pre-existing dedicated cluster or FRR resource" >&2
  exit 1
fi
[[ "$(docker image inspect "${OVN_IMAGE}" --format '{{.Id}}')" == "${OVN_ID}" ]]
[[ "$(docker image inspect evpn4ovn/node-agent-test:cni-24b-base --format '{{.Id}}')" == "${BASE_ID}" ]]
[[ "$(docker image inspect evpn4ovn/app:kind --format '{{.Id}}')" == "${APP_BASE_ID}" ]]
test_image_snapshot
test_image_require_new_tag "${AGENT_IMAGE}"
test_image_require_new_tag "${APP_IMAGE}"

kind get clusters | sort >"${EVIDENCE_DIR}/clusters-before.txt"
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort >"${EVIDENCE_DIR}/docker-containers-before.txt"
docker network ls --format '{{.ID}} {{.Name}} {{.Driver}}' | sort >"${EVIDENCE_DIR}/docker-networks-before.txt"

(
  cd "${ROOT}"
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-node-agent" ./cmd/evpn-node-agent
  CGO_ENABLED=0 go test -c -o "${EVIDENCE_DIR}/build/nodeagent.test" ./pkg/nodeagent
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/ovsdb_mgmt" ./services/ovsdb_mgmt
)
docker build --quiet -t "${AGENT_IMAGE}" -f "${ROOT}/tools/e2e/framework/Dockerfile" "${EVIDENCE_DIR}/build"
test_image_record_created "${AGENT_IMAGE}"
docker build --quiet -t "${APP_IMAGE}" -f "${ROOT}/tools/e2e/framework/Dockerfile.app" "${EVIDENCE_DIR}/build"
test_image_record_created "${APP_IMAGE}"
docker image inspect "${AGENT_IMAGE}" --format '{{.Id}}' >"${EVIDENCE_DIR}/agent-image.txt"
docker image inspect "${APP_IMAGE}" --format '{{.Id}}' >"${EVIDENCE_DIR}/app-image.txt"

FAILURE_CLASSIFICATION=HARNESS
FAILURE_GATE=F1
CLUSTER_CREATE_ATTEMPTED=1
e2e_lab_up framework-netlink
CREATED_CLUSTER=1
CLUSTER_NODES="$(docker ps -a --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER}" --format '{{.ID}} {{.Names}} {{.Label "io.x-k8s.kind.cluster"}}' | sort)"
printf '%s\n' "${CLUSTER_NODES}" >"${EVIDENCE_DIR}/owned-kind-nodes.txt"
python3 - "${KIND_CLUSTER}" "${CLUSTER_NODES}" <<'PY'
import sys
rows=[line.split() for line in sys.argv[2].splitlines() if line.strip()]
assert len(rows)==2 and {r[1] for r in rows}=={sys.argv[1]+"-worker",sys.argv[1]+"-control-plane"}
assert all(r[2]==sys.argv[1] for r in rows),rows
PY
kind load docker-image --name "${KIND_CLUSTER}" "${OVN_IMAGE}" "${AGENT_IMAGE}" "${APP_IMAGE}"
k wait --for=condition=Ready nodes --all --timeout=180s
docker cp "${EVIDENCE_DIR}/build/nodeagent.test" "${WORKER}:/usr/local/bin/framework-nodeagent.test"
docker exec "${WORKER}" mkdir -m 777 "${SHARED_DIR}"

CREATED_FFR=1
interop_setup_underlay
k create namespace "${NAMESPACE}"
k create namespace ovn-system
k create namespace evpn-system

cat >"${EVIDENCE_DIR}/ovn-runtime.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovn-runtime
  namespace: ovn-system
spec:
  replicas: 1
  selector:
    matchLabels: {app: ovn-runtime}
  template:
    metadata:
      labels: {app: ovn-runtime}
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      containers:
      - name: ovn
        image: ${OVN_IMAGE}
        imagePullPolicy: Never
        securityContext: {privileged: true, runAsUser: 0}
        env:
        - name: OVN_EVPN_LOCAL_IP
          value: "${LOCAL_VTEP}"
        - name: OVN_EVPN_VXLAN_PORTS
          value: "4789"
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
        - {name: evidence, mountPath: /framework-e2e-share}
      volumes:
      - name: ovs
        hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}
      - name: ovn
        hostPath: {path: /run/ovn, type: DirectoryOrCreate}
      - name: evidence
        hostPath: {path: ${SHARED_DIR}, type: Directory}
EOF
k apply -f "${EVIDENCE_DIR}/ovn-runtime.yaml"
k -n ovn-system rollout status deployment/ovn-runtime --timeout=240s
OVN_POD="$(k -n ovn-system get pod -l app=ovn-runtime -o jsonpath='{.items[0].metadata.name}')"
rt sh -ec 'ovn-nbctl --version; ovs-vsctl --version' | tee "${EVIDENCE_DIR}/dataplane-version.txt"
docker image inspect "${OVN_IMAGE}" >"${EVIDENCE_DIR}/dataplane-image.json"
k version -o json | tee "${EVIDENCE_DIR}/kubernetes-version.json" >/dev/null
rt ovs-vsctl set Open_vSwitch . "external_ids:ovn-evpn-local-ip=${LOCAL_VTEP}" external_ids:ovn-evpn-vxlan-ports=4789

interop_setup_provider
cat >"${EVIDENCE_DIR}/provider-config.yaml" <<EOF
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata: {name: framework-netlink-e2e, namespace: k8gobgp-system}
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
      description: "framework-netlink-e2e-external-frr-peer"
    afiSafis:
    - family: "l2vpn-evpn"
      enabled: true
EOF
k apply -f "${EVIDENCE_DIR}/provider-config.yaml"
k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
export_record_provider

LOCAL_CHASSIS="$(rt ovs-vsctl --bare get Open_vSwitch . external_ids:system-id | tr -d '"')"
[[ -n "${LOCAL_CHASSIS}" ]]
cat >"${EVIDENCE_DIR}/export-config.json" <<EOF
{"Enabled":true,"DryRun":false,"ProcessContinuity":true,"NodeName":"${WORKER}","LocalChassis":"${LOCAL_CHASSIS}","GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Networks":[{"NetworkID":"framework-netlink-e2e","LogicalSwitch":"${LOGICAL_SWITCH}","Enabled":true,"ExporterSource":"ovn-netlink","BridgeDevice":"${FRAMEWORK_BRIDGE_DEVICE}","VXLANDevice":"${FRAMEWORK_VXLAN_DEVICE}","AdvertiseDevice":"${FRAMEWORK_ADVERTISE_DEVICE}","VNI":${FRAMEWORK_VNI},"RouteDistinguisher":"${LOCAL_VTEP}:${FRAMEWORK_VNI}","RouteTargets":["64512:${FRAMEWORK_VNI}"],"LocalVTEP":"${LOCAL_VTEP}"}]}
EOF
cat >"${EVIDENCE_DIR}/remote-config.json" <<EOF
{"Enabled":true,"GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Import":{"LocalVTEP":"${LOCAL_VTEP}","Networks":[{"NetworkID":"framework-netlink-e2e","VNI":${FRAMEWORK_VNI},"ImportRouteTargets":["64512:${FRAMEWORK_VNI}"],"AllowedVTEPs":["${FRR_VTEP}"]}]},"Netlink":{"Enabled":true,"ExclusiveWriter":true,"StateDirectory":"${REMOTE_STATE_DIR}","Networks":[{"NetworkID":"framework-netlink-e2e","LogicalSwitch":"${LOGICAL_SWITCH}","VNI":${FRAMEWORK_VNI},"BridgeDevice":"${FRAMEWORK_BRIDGE_DEVICE}","VXLANDevice":"${FRAMEWORK_VXLAN_DEVICE}"}]}}
EOF
docker exec "${WORKER}" mkdir -p /run/evpn-node-agent
docker exec "${WORKER}" chmod 700 /run/evpn-node-agent
docker exec "${WORKER}" mkdir -m 700 "${REMOTE_STATE_DIR}"
docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.json' <"${EVIDENCE_DIR}/export-config.json"
docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/remote-config.json' <"${EVIDENCE_DIR}/remote-config.json"
cat >"${EVIDENCE_DIR}/ovsdb-mgmt.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ovsdb-mgmt
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels: {app: ovsdb-mgmt}
  template:
    metadata:
      labels: {app: ovsdb-mgmt}
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      containers:
      - name: ovsdb-mgmt
        image: ${APP_IMAGE}
        imagePullPolicy: Never
        command: ["/usr/local/bin/ovsdb_mgmt"]
        securityContext: {runAsUser: 0}
        env:
        - {name: OVN_NB_ENDPOINT, value: "unix:/var/run/ovn/ovnnb_db.sock"}
        - {name: OVS_DB_ENDPOINT, value: "unix:/var/run/openvswitch/db.sock"}
        - {name: DEFAULT_DYNAMIC_ROUTING_VNI, value: "${FRAMEWORK_VNI}"}
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
      volumes:
      - name: ovs
        hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}
      - name: ovn
        hostPath: {path: /run/ovn, type: DirectoryOrCreate}
---
apiVersion: v1
kind: Service
metadata:
  name: ovsdb-mgmt
  namespace: ${NAMESPACE}
spec:
  selector: {app: ovsdb-mgmt}
  ports:
  - {name: grpc, port: 50053, targetPort: 50053}
EOF
k apply -f "${EVIDENCE_DIR}/ovsdb-mgmt.yaml"
k -n "${NAMESPACE}" rollout status deployment/ovsdb-mgmt --timeout=120s

FAILURE_CLASSIFICATION=PROVISIONING
FAILURE_GATE=F3
PROVISION_ATTEMPTED=1
FRAMEWORK_E2E_ACTION=network \
  fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-network-provision.log"
create_netlink_prerequisites

cat >"${EVIDENCE_DIR}/node-agent.yaml" <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: framework-node-agent
  namespace: ${NAMESPACE}
spec:
  selector:
    matchLabels: {app: framework-node-agent}
  template:
    metadata:
      labels: {app: framework-node-agent}
    spec:
      hostNetwork: true
      hostPID: true
      dnsPolicy: ClusterFirstWithHostNet
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      containers:
      - name: agent
        image: ${AGENT_IMAGE}
        imagePullPolicy: Never
        securityContext: {privileged: true, runAsUser: 0}
        command: ["/bin/sh", "-ec"]
        args:
        - |
          for i in \$(seq 1 180); do
            test -S /var/run/ovn/ovnnb_db.sock && test -S /var/run/openvswitch/db.sock && test -f /run/evpn-node-agent/export-config.json && break
            sleep 1
          done
          exec /usr/local/bin/evpn-node-agent --socket=/run/evpn-node-agent/agent.sock --mode=real-ovn-ovs --network-backend=linux-veth --ovn-nb-endpoint=unix:/var/run/ovn/ovnnb_db.sock --ovsdb-endpoint=unix:/var/run/openvswitch/db.sock ${EXPORT_FLAGS} ${REMOTE_FLAGS}
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
        - {name: evidence, mountPath: /framework-e2e-share}
        - {name: agent, mountPath: /run/evpn-node-agent}
        - {name: netns, mountPath: /run/netns, mountPropagation: HostToContainer}
        - {name: gobgp, mountPath: /var/run/gobgp, readOnly: true}
      volumes:
      - {name: ovs, hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}}
      - {name: ovn, hostPath: {path: /run/ovn, type: DirectoryOrCreate}}
      - {name: evidence, hostPath: {path: ${SHARED_DIR}, type: Directory}}
      - {name: agent, hostPath: {path: /run/evpn-node-agent, type: DirectoryOrCreate}}
      - {name: netns, hostPath: {path: /run/netns, type: DirectoryOrCreate}}
      - {name: gobgp, hostPath: {path: /run/gobgp-export-test, type: DirectoryOrCreate}}
EOF
k apply -f "${EVIDENCE_DIR}/node-agent.yaml"
k -n "${NAMESPACE}" rollout status ds/framework-node-agent --timeout=180s
AGENT_POD="$(k -n "${NAMESPACE}" get pod -l app=framework-node-agent -o jsonpath='{.items[0].metadata.name}')"
if ! k -n "${NAMESPACE}" logs "${AGENT_POD}" >"${EVIDENCE_DIR}/agent-initial.log" 2>&1; then
  printf 'kubectl logs failed for agent pod %s\n' "${AGENT_POD}" \
    >>"${EVIDENCE_DIR}/agent-initial.log"
fi
if ! k -n "${NAMESPACE}" get pod "${AGENT_POD}" -o json \
  >"${EVIDENCE_DIR}/agent-pod-initial.json" 2>&1; then
  printf 'kubectl get failed for agent pod %s\n' "${AGENT_POD}" \
    >>"${EVIDENCE_DIR}/agent-pod-initial.json"
fi
printf 'agent-pod=%s\novn-pod=%s\nprovider-pod=%s\n' "${AGENT_POD}" "${OVN_POD}" "${PROVIDER_POD}" \
  >"${EVIDENCE_DIR}/runtime-pods.txt"

FAILURE_CLASSIFICATION=PROVISIONING
FAILURE_GATE=F3
FRAMEWORK_E2E_ACTION=endpoints \
  fwtest \
  2>&1 | tee "${EVIDENCE_DIR}/framework-provision.log"
python3 - "${EVIDENCE_DIR}/framework-provision.log" \
  "${EVIDENCE_DIR}/framework-endpoints.json" <<'PY'
import json,pathlib,sys
source=pathlib.Path(sys.argv[1]).read_text().splitlines()
prefix="FRAMEWORK_E2E_RECEIPT="
matches=[line.split(prefix,1)[1] for line in source if prefix in line]
assert len(matches)==1,matches
receipt=json.loads(matches[0])
pathlib.Path(sys.argv[2]).write_text(json.dumps(receipt,indent=2)+"\n")
PY
{
  k get pods -A -o wide
  printf 'CNI namespace: '
  k get namespace cni-test -o name --ignore-not-found
  printf 'Multus pods: '
  k get pods -A -o json | python3 -c 'import json,sys; p=json.load(sys.stdin)["items"]; print(" ".join(x["metadata"]["name"] for x in p if "multus" in x["metadata"]["name"].lower()))'
  printf 'NetworkAttachmentDefinition CRD: '
  k get crd network-attachment-definitions.k8s.cni.cncf.io -o name --ignore-not-found
} >"${EVIDENCE_DIR}/cni-independence.txt"
! grep -q '^cni-test$' "${EVIDENCE_DIR}/cni-independence.txt"
! grep -q 'multus' "${EVIDENCE_DIR}/cni-independence.txt"
for file in export-config.json remote-config.json; do
  cp "${EVIDENCE_DIR}/${file}" "${EVIDENCE_DIR}/runtime-${file}"
done

FAILURE_CLASSIFICATION=OVN/OVS
FAILURE_GATE=F4
docker exec "${WORKER}" ip -d -j link \
  >"${EVIDENCE_DIR}/worker-links-initial.json"
rt ovn-nbctl --wait=sb sync
rt ovn-nbctl --format=json --columns=name,addresses,external_ids list Logical_Switch_Port \
  >"${EVIDENCE_DIR}/ovn-nb-ports-initial.json"
rt ovn-sbctl --format=json --columns=logical_port,chassis,up list Port_Binding \
  >"${EVIDENCE_DIR}/ovn-sb-bindings-initial.json"
rt ovs-vsctl show >"${EVIDENCE_DIR}/ovs-show-initial.txt"
rt ovs-ofctl dump-flows br-int >"${EVIDENCE_DIR}/ovs-flows-initial.txt"
rt ovs-vsctl --format=json --columns=name,ofport,external_ids list Interface \
  >"${EVIDENCE_DIR}/ovs-interfaces-initial.json"
docker exec "${WORKER}" ip -j link >"${EVIDENCE_DIR}/worker-links-initial.json"
rt ip -j link >"${EVIDENCE_DIR}/ovn-runtime-links-initial.json"
python3 - "${EVIDENCE_DIR}/framework-endpoints.json" "${EVIDENCE_DIR}/ovn-nb-ports-initial.json" <<'PY'
import json,sys
receipt=json.load(open(sys.argv[1]))
ports=json.load(open(sys.argv[2]))
head=ports["headings"]
rows=[dict(zip(head,row)) for row in ports["data"]]
for endpoint in receipt["endpoints"]:
    matches=[r for r in rows if r["name"]==endpoint["logicalSwitchPort"]]
    assert len(matches)==1,matches
    assert endpoint["mac"] in str(matches[0]["addresses"]),matches[0]
    assert endpoint["ip"] in str(matches[0]["addresses"]),matches[0]
print("Framework-owned logical endpoint state=PASS")
PY
while IFS= read -r lsp; do
  [[ -n "${lsp}" ]] || continue
  binding="$(rt ovn-sbctl --bare --columns=_uuid find Port_Binding "logical_port=${lsp}")"
  [[ -n "${binding}" ]]
  chassis="$(rt ovn-sbctl --bare get Port_Binding "${binding}" chassis | tr -d '[]\"')"
  [[ -n "${chassis}" ]]
  [[ "$(rt ovn-sbctl --bare get Chassis "${chassis}" name | tr -d '\"')" == "${LOCAL_CHASSIS}" ]]
  [[ "$(rt ovn-sbctl --bare get Port_Binding "${binding}" up | tr -d '[] ')" == *true* ]]
  iface="$(rt ovs-vsctl --bare --columns=name find Interface "external_ids:iface-id=${lsp}")"
  [[ -n "${iface}" ]]
  ofport="$(rt ovs-vsctl --bare get Interface "${iface}" ofport | tr -d '[] ')"
  [[ "${ofport}" =~ ^[1-9][0-9]*$ ]]
done < <(python3 -c 'import json,sys; [print(e["logicalSwitchPort"]) for e in json.load(open(sys.argv[1]))["endpoints"]]' "${EVIDENCE_DIR}/framework-endpoints.json")

FAILURE_CLASSIFICATION=BGP
FAILURE_GATE=F5
deadline=$((SECONDS+180))
F5_PEER_ESTABLISHED=0
while (( SECONDS < deadline )); do
  if docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" \
      >"${EVIDENCE_DIR}/frr-peer-initial.txt" 2>&1 &&
    grep -q 'BGP state = Established' "${EVIDENCE_DIR}/frr-peer-initial.txt"
  then
    F5_PEER_ESTABLISHED=1
    break
  fi
  sleep 2
done
if [[ "${F5_PEER_ESTABLISHED}" != 1 ]]; then
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn summary' \
    >"${EVIDENCE_DIR}/frr-neighbors-at-failure.txt" 2>&1 || true
  provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock neighbor "${FRR_PEER}" \
    >"${EVIDENCE_DIR}/gobgp-neighbors.txt" 2>&1 || true
  provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock global \
    >"${EVIDENCE_DIR}/gobgp-global-at-failure.txt" 2>&1 || true
  provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock neighbor "${FRR_PEER}" \
    >"${EVIDENCE_DIR}/gobgp-neighbor-at-failure.txt" 2>&1 || true
  k -n k8gobgp-system get bgpconfig framework-netlink-e2e -o yaml \
    >"${EVIDENCE_DIR}/bgpconfig-at-failure.yaml" 2>&1 || true
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn summary' \
    >"${EVIDENCE_DIR}/frr-neighbors-at-failure.txt" 2>&1 || true
  docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" \
    >"${EVIDENCE_DIR}/frr-peer-at-failure.txt" 2>&1 || true
  docker exec "${WORKER}" ip route get "${FRR_PEER}" \
    >"${EVIDENCE_DIR}/worker-route-to-frr.txt" 2>&1 || true
  docker exec "${WORKER}" ss -lntp \
    >"${EVIDENCE_DIR}/worker-listeners-at-failure.txt" 2>&1 || true
  echo "GoBGP-FRR peer did not reach Established; captured peer, config, route, and listener diagnostics" >&2
  exit 1
fi
k get bgpnodestatuses -o json \
  >"${EVIDENCE_DIR}/bgp-node-status-initial.json" 2>&1 || true
docker exec "${WORKER}" ls -la /run/evpn-node-agent \
  >"${EVIDENCE_DIR}/node-agent-state-files.txt" 2>&1 || true
docker exec "${WORKER}" ss -xlpn \
  >"${EVIDENCE_DIR}/worker-unix-sockets.txt" 2>&1 || true
docker exec "${WORKER}" sh -c \
  'test ! -f /run/evpn-node-agent/export-status.json || cat /run/evpn-node-agent/export-status.json' \
  >"${EVIDENCE_DIR}/export-status-node-view.json" 2>&1 || true

FAILURE_CLASSIFICATION=GO-BGP-TYPE2
FAILURE_GATE=F6
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  if ag cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/export-status-initial.json" 2>/dev/null &&
     python3 - "${EVIDENCE_DIR}/export-status-initial.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert not s.get("Error"),s
result=s.get("Result") or {}
applied=result.get("Applied") or []
assert len(result.get("Desired") or [])==4,s
assert len(applied)==4,s
routes={
    (e.get("MACAddress","").lower(),tuple(e.get("IPAddresses") or []),bool(e.get("MACOnly")))
    for item in applied for e in [item.get("Endpoint") or {}]
}
assert routes=={
    ("0a:58:f0:01:00:0a",(),True),
    ("0a:58:f0:01:00:0a",("10.244.240.10",),False),
    ("0a:58:f0:01:00:0b",(),True),
    ("0a:58:f0:01:00:0b",("10.244.240.11",),False),
},routes
PY
  then break; fi
  sleep 2
done
provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock global rib -a evpn \
  >"${EVIDENCE_DIR}/gobgp-rib-initial.txt" 2>&1 || true
docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
  >"${EVIDENCE_DIR}/frr-rib-initial.txt"
grep -q '10.244.240.10' "${EVIDENCE_DIR}/frr-rib-initial.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-initial.txt"
interop_wait_import_count 1 initial
FAILURE_CLASSIFICATION=NETLINK
FAILURE_GATE=F7
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  if ag ip -d -j link show dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/netlink-bridge-initial.json" 2>/dev/null &&
     ag ip -d -j link show dev "${FRAMEWORK_VXLAN_DEVICE}" >"${EVIDENCE_DIR}/netlink-vxlan-initial.json" 2>/dev/null &&
     ag ip -d -j link show dev "${FRAMEWORK_ADVERTISE_DEVICE}" >"${EVIDENCE_DIR}/netlink-advertise-initial.json" 2>/dev/null; then break; fi
  sleep 2
done
ag ip -d -j link show dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/netlink-bridge-initial.json"
ag ip -d -j link show dev "${FRAMEWORK_VXLAN_DEVICE}" >"${EVIDENCE_DIR}/netlink-vxlan-initial.json"
ag bridge -j fdb show dev "${FRAMEWORK_VXLAN_DEVICE}" >"${EVIDENCE_DIR}/netlink-fdb-initial.json"
ag ip -4 -j neigh show nud all dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/netlink-neighbors-initial.json"
python3 - "${EVIDENCE_DIR}" "${FRAMEWORK_VNI}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
status=json.load((r/"remote-status-initial.json").open())
snap=status.get("Snapshot") or {}
assert snap.get("Authoritative") is True
assert len(snap.get("Endpoints") or [])==1
e=snap["Endpoints"][0]
assert (e["MAC"].lower(),e["IP"],e["VNI"])==("0a:58:f0:01:00:0c","10.244.240.12",int(sys.argv[2])),e
fdb=json.load((r/"netlink-fdb-initial.json").open())
assert any(x.get("mac","").lower()=="0a:58:f0:01:00:0c" and x.get("dst")=="198.18.0.4" for x in fdb),fdb
print("remote Type-2 import and Linux FDB ownership evidence=PASS")
PY

FAILURE_CLASSIFICATION=OVN-CONTROLLER
FAILURE_GATE=F8
rt ovs-vsctl show >"${EVIDENCE_DIR}/ovs-show-initial.txt"
rt ovs-ofctl dump-flows br-int >"${EVIDENCE_DIR}/ovs-flows-initial.txt"
python3 - "${EVIDENCE_DIR}/framework-endpoints.json" "${EVIDENCE_DIR}/ovs-interfaces-initial.json" <<'PY'
import json,sys
receipt=json.load(open(sys.argv[1]))
data=json.load(open(sys.argv[2]))
rows=[dict(zip(data["headings"],r)) for r in data["data"]]
def unwrap(value):
    return value[1] if isinstance(value,list) and len(value)==2 and value[0]=="set" else value
for e in receipt["endpoints"]:
    found=[r for r in rows if e["logicalSwitchPort"] in str(r["external_ids"])]
    assert len(found)==1,found
    ofport=unwrap(found[0]["ofport"])
    if isinstance(ofport,list):
        assert len(ofport)==1 and int(ofport[0])>0,found
    else:
        assert int(ofport)>0,found
print("ovn-controller-programmed OVS endpoint attachment=PASS")
PY

FAILURE_CLASSIFICATION=PACKET-CAPTURE
FAILURE_GATE=F10
REMOTE_CAPTURE_PID_FILE="/tmp/framework-netlink-capture-${RUN_SUFFIX}.pid"
docker exec -d "${FRR_CONTAINER}" sh -ec \
  "ip netns exec evpnwork sh -ec 'echo \$\$ > ${REMOTE_CAPTURE_PID_FILE}; exec sleep 86400'"
REMOTE_WORKLOAD_PID="$(docker exec "${FRR_CONTAINER}" cat "${REMOTE_CAPTURE_PID_FILE}")"
docker exec "${FRR_CONTAINER}" ip netns pids evpnwork \
  >"${EVIDENCE_DIR}/remote-workload-netns-pids.txt"
grep -Fxq "${REMOTE_WORKLOAD_PID}" "${EVIDENCE_DIR}/remote-workload-netns-pids.txt"
[[ "${REMOTE_WORKLOAD_PID}" =~ ^[0-9]+$ ]]
printf '%s\n' "${REMOTE_WORKLOAD_PID}" >"${EVIDENCE_DIR}/remote-workload-capture-pid.txt"
LOCAL_VTEP_IFACE="$(ag ip -j addr show | python3 -c 'import json,sys; v="198.18.0.2"; m=[x["ifname"] for x in json.load(sys.stdin) if any(a.get("local")==v for a in x.get("addr_info",[]))]; assert len(m)==1,m; print(m[0])')"
ag mkdir -p /run/evpn-node-agent/framework-captures
capture_running=1
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- sh -ec '
  d=/run/evpn-node-agent/framework-captures
  for f in local-host.pcap worker-underlay.pcap local-host.pid worker-underlay.pid; do
    test ! -e "$d/$f" || { echo "pre-existing capture path: $d/$f" >&2; exit 1; }
  done
  nohup tcpdump -U -n -i "$1" -w "$d/local-host.pcap" "arp or icmp" >"$d/local-host.log" 2>&1 </dev/null &
  echo $! >"$d/local-host.pid"
  nohup tcpdump -U -n -i "$2" -w "$d/worker-underlay.pcap" "udp port 4789 or udp port 60100" >"$d/worker-underlay.log" 2>&1 </dev/null &
  echo $! >"$d/worker-underlay.pid"
' sh "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["endpoints"][0]["interfaceName"])' "${EVIDENCE_DIR}/framework-endpoints.json")" "${LOCAL_VTEP_IFACE}"
if docker container inspect "${KIND_CLUSTER}-frr-capture" >/dev/null 2>&1; then
  echo "refusing pre-existing FRR capture sidecar" >&2
  exit 1
fi
CREATED_CAPTURE=1
docker run -d --name "${KIND_CLUSTER}-frr-capture" \
  --label "evpn4ovn.test-run=${KIND_CLUSTER}" --network "container:${FRR_CONTAINER}" \
  --pid "container:${FRR_CONTAINER}" --privileged \
  -v "${EVIDENCE_DIR}:/evidence" --entrypoint /bin/sh "${AGENT_IMAGE}" -ec '
    tcpdump -U -n -i eth0 -w /evidence/frr-underlay.pcap "udp port 4789 or udp port 60100" &
    a=$!
    tcpdump -U -n -i vxlan1000 -w /evidence/frr-vxlan.pcap "arp or icmp" &
    b=$!
    nsenter -t '"${REMOTE_WORKLOAD_PID}"' -n tcpdump -U -n -i frr-work0 -w /evidence/remote-workload.pcap "arp or icmp" &
    c=$!
    trap "kill $a $b $c 2>/dev/null || true; wait" TERM INT
    wait
  ' >"${EVIDENCE_DIR}/frr-capture-container-id.txt"
sleep 2
for f in local-host.pcap worker-underlay.pcap frr-underlay.pcap frr-vxlan.pcap remote-workload.pcap; do
  if [[ "${f}" == local-host.pcap || "${f}" == worker-underlay.pcap ]]; then
    if ! ag sh -c 'kill -0 "$(cat "$1")"' sh \
      "/run/evpn-node-agent/framework-captures/${f%.pcap}.pid"; then
      ag sh -c 'test ! -f "$1" || cat "$1"' sh \
        "/run/evpn-node-agent/framework-captures/${f%.pcap}.log" >&2 || true
      echo "endpoint/underlay capture process failed: ${f}" >&2
      exit 1
    fi
  else
    if ! docker exec "${KIND_CLUSTER}-frr-capture" true; then
      docker logs "${KIND_CLUSTER}-frr-capture" >&2 || true
      echo "FRR capture sidecar exited before dataplane traffic: ${f}" >&2
      exit 1
    fi
  fi
done
cat >"${EVIDENCE_DIR}/packet-path-manifest.json" <<EOF
{"localIp":"10.244.240.10","localMac":"0a:58:f0:01:00:0a","remoteIp":"${REMOTE_IP}","remoteMac":"${REMOTE_MAC}","localVtep":"${LOCAL_VTEP}","remoteVtep":"${FRR_VTEP}","vni":1000,"udpPort":4789,"captures":{"endpoint-veth":"local-host.pcap","worker-underlay":"worker-underlay.pcap","frr-underlay":"frr-underlay.pcap","frr-vxlan":"frr-vxlan.pcap","remote-workload":"remote-workload.pcap"}}
EOF

FAILURE_CLASSIFICATION=OVN-OVS-FORWARDING
FAILURE_GATE=F9
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
  ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
  | tee "${EVIDENCE_DIR}/endpoint-a-to-remote-ping.txt"
docker exec "${KIND_CLUSTER}-frr-capture" nsenter -t "${REMOTE_WORKLOAD_PID}" -n \
  ping -c 3 -W 2 10.244.240.10 \
  | tee "${EVIDENCE_DIR}/remote-to-endpoint-a-ping.txt"

FAILURE_CLASSIFICATION=LIFECYCLE
FAILURE_GATE=F11
FRAMEWORK_E2E_ACTION=delete-a \
  fwtest \
  2>&1 | tee "${EVIDENCE_DIR}/framework-delete-a.log"
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  if ag cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/export-status-after-delete-a.json" 2>/dev/null &&
     python3 - "${EVIDENCE_DIR}/export-status-after-delete-a.json" <<'PY' >/dev/null 2>&1
import json,sys
s=json.load(open(sys.argv[1]))
assert not s.get("Error"),s
result=s.get("Result") or {}
expected={
    ("0a:58:f0:01:00:0b",(),True),
    ("0a:58:f0:01:00:0b",("10.244.240.11",),False),
}
def route_key(item):
    endpoint=item.get("Endpoint") or item
    return (
        endpoint.get("MACAddress","").lower(),
        tuple(endpoint.get("IPAddresses") or []),
        bool(endpoint.get("MACOnly")),
    )
assert {route_key(item) for item in result.get("Desired") or []}==expected,s
assert {route_key(item) for item in result.get("Applied") or []}==expected,s
PY
  then break; fi
  sleep 2
done
provider_exec gobgp --target unix:///var/run/gobgp/gobgp.sock global rib -a evpn \
  >"${EVIDENCE_DIR}/gobgp-rib-after-delete-a.txt"
docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
  >"${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
! grep -q '10.244.240.10' "${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
interop_wait_import_count 1 after-delete-a
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
  ip netns exec "fw-${RUN_SUFFIX}-b" ping -c 3 -W 2 "${REMOTE_IP}" \
  | tee "${EVIDENCE_DIR}/endpoint-b-to-remote-ping.txt"
docker exec "${KIND_CLUSTER}-frr-capture" nsenter -t "${REMOTE_WORKLOAD_PID}" -n \
  ping -c 3 -W 2 10.244.240.11 \
  | tee "${EVIDENCE_DIR}/remote-to-endpoint-b-ping.txt"
capture_stop
stop_remote_capture_keeper
for pcap in local-host worker-underlay frr-underlay frr-vxlan remote-workload; do
  tcpdump -nn -r "${EVIDENCE_DIR}/${pcap}.pcap" >"${EVIDENCE_DIR}/${pcap}.decoded.txt" 2>"${EVIDENCE_DIR}/${pcap}.decode.log"
  test -s "${EVIDENCE_DIR}/${pcap}.decoded.txt"
done
grep -q '4789' "${EVIDENCE_DIR}/worker-underlay.decoded.txt"
grep -q '4789' "${EVIDENCE_DIR}/frr-underlay.decoded.txt"
grep -q '10.244.240.12' "${EVIDENCE_DIR}/local-host.decoded.txt"
grep -q '10.244.240.10' "${EVIDENCE_DIR}/frr-vxlan.decoded.txt"
grep -q '10.244.240.10' "${EVIDENCE_DIR}/remote-workload.decoded.txt"
rt ovn-nbctl --wait=sb sync
rt ovn-nbctl --format=json --columns=name list Logical_Switch_Port \
  >"${EVIDENCE_DIR}/ovn-nb-ports-after-delete-a.json"
rt ovs-vsctl --format=json --columns=name,ofport,external_ids list Interface \
  >"${EVIDENCE_DIR}/ovs-interfaces-after-delete-a.json"
python3 - "${EVIDENCE_DIR}/framework-endpoints.json" "${EVIDENCE_DIR}/ovn-nb-ports-after-delete-a.json" "${EVIDENCE_DIR}/ovs-interfaces-after-delete-a.json" <<'PY'
import json,sys
receipt=json.load(open(sys.argv[1]))
deleted=receipt["endpoints"][0]
survivor=receipt["endpoints"][1]
ports=json.load(open(sys.argv[2]))
names=[row[0] for row in ports["data"]]
assert deleted["logicalSwitchPort"] not in names,names
assert survivor["logicalSwitchPort"] in names,names
data=json.load(open(sys.argv[3]))
rows=[dict(zip(data["headings"],r)) for r in data["data"]]
assert not any(deleted["logicalSwitchPort"] in str(r["external_ids"]) for r in rows)
assert any(survivor["logicalSwitchPort"] in str(r["external_ids"]) for r in rows)
print("deleted endpoint ownership cleanup and survivor attachment=PASS")
PY

docker stop --time 10 "${FRR_CONTAINER}" >/dev/null
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  if interop_wait_import_count 0 final-withdrawal >/dev/null 2>&1; then break; fi
  sleep 2
done
interop_wait_import_count 0 final-withdrawal
ag bridge -j fdb show dev "${FRAMEWORK_VXLAN_DEVICE}" >"${EVIDENCE_DIR}/netlink-fdb-after-peer-withdrawal.json"
ag ip -4 -j neigh show nud all dev "${FRAMEWORK_BRIDGE_DEVICE}" >"${EVIDENCE_DIR}/netlink-neighbors-after-peer-withdrawal.json"
python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
fdb=json.load((r/"netlink-fdb-after-peer-withdrawal.json").open())
neighbors=json.load((r/"netlink-neighbors-after-peer-withdrawal.json").open())
assert not any(x.get("mac","").lower()=="0a:58:f0:01:00:0c" for x in fdb),fdb
assert not any(x.get("dst")=="10.244.240.12" for x in neighbors),neighbors
print("ownership-scoped remote Netlink withdrawal=PASS")
PY

FRAMEWORK_E2E_ACTION=cleanup \
  fwtest \
  2>&1 | tee "${EVIDENCE_DIR}/framework-final-cleanup.log"
wait_exporter_empty final
FRAMEWORK_E2E_ACTION=delete-network \
  fwtest \
  2>&1 | tee "${EVIDENCE_DIR}/framework-network-cleanup.log"
remove_netlink_prerequisites
rt ovn-nbctl --wait=sb sync
rt ovn-nbctl --bare --columns=name find Logical_Switch "name=${LOGICAL_SWITCH}" \
  >"${EVIDENCE_DIR}/logical-switch-after-cleanup.txt"
test ! -s "${EVIDENCE_DIR}/logical-switch-after-cleanup.txt"
echo "local Type-2 exporter cleanup=PASS"
PROVISION_ATTEMPTED=0
FAILURE_CLASSIFICATION=PASS
FAILURE_GATE=ALL_SUPPORTED_GATES
printf '%s\n' "Framework Netlink E2E completed; evidence=${EVIDENCE_DIR}"
exit 0
