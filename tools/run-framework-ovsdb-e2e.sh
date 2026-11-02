#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="framework-ovsdb-e2e-c4l"
EVIDENCE_ROOT="/tmp/framework-ovsdb-e2e-c4l-evidence"
RUN_SUFFIX="$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-${RUN_SUFFIX}"
EVIDENCE_DIR="${EVIDENCE_ROOT}/attempt-${RUN_ID}"
LOGICAL_SWITCH="fw-${RUN_SUFFIX}"
NAMESPACE="framework-e2e"
AGENT_IMAGE="evpn4ovn/framework-ovsdb-agent:${KIND_CLUSTER}"
APP_IMAGE="evpn4ovn/framework-ovsdb-app:${KIND_CLUSTER}"
OVN_IMAGE="evpn4ovn/ovn-ovs:kind-26.09"
OVN_ID="sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
BASE_ID="sha256:f01f29f4d8b1fd4f377103015aa7b7dab6f8189416ce2a363f63d7e538c4e5a7"
APP_BASE_ID="sha256:b5c41de41ae7df5b20351b3c44ec3ce65afeaa6b06a5d482fb2fd12e2460cc45"
NODE_IMAGE="kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c"
FRAMEWORK_VNI=1000
PHYSICAL_NETWORK="fwphys${RUN_SUFFIX}"
PROVIDER_BRIDGE="br-${RUN_SUFFIX}"
SHARED_DIR="/run/framework-ovsdb-e2e-${RUN_SUFFIX}"
WORKER="${KIND_CLUSTER}-worker"
KUBECONFIG="${EVIDENCE_DIR}/kubeconfig"
CREATED_CLUSTER=0
CLUSTER_CREATE_ATTEMPTED=0
PROVISION_ATTEMPTED=0
AGENT_POD=""
OVN_POD=""
PROVIDER_POD=""
IMPORTER_POD=""
REMOTE_WORKLOAD_PID=""
CREATED_CAPTURE=0
CAPTURES_RUNNING=0
CLEANUP_RC=0
PREVIOUS_ATTEMPT=""
PREVIOUS_ATTEMPT_PASSED=0
FAILURE_CLASSIFICATION=UNRESOLVED
FAILURE_GATE=NOT_REACHED
GATES=(O1 O2 O3 O4 O5 O6 O7 O8 O9 O10 O11 O12 O13 O14)
declare -A GATE_STATUS=()

export KIND_CLUSTER EVIDENCE_DIR WORKER
source "${ROOT}/tools/lib/cni-agent-image-test.sh"
source "${ROOT}/tools/lib/cni-evpn-export-test.sh"
source "${ROOT}/tools/lib/cni-evpn-frr-interop-test.sh"
source "${ROOT}/tools/lib/containerlab-e2e.sh"
e2e_lab_acquire
export KUBECONFIG
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "kind-${KIND_CLUSTER}" "$@"; }
rt() {
  local remote_path host_path status command_rc
  remote_path="/framework-e2e-share/.ovsdb-rt-${RUN_SUFFIX}"
  host_path="${SHARED_DIR}/.ovsdb-rt-${RUN_SUFFIX}"
  k -n ovn-system exec "${OVN_POD}" -c ovn -- sh -c \
    'output="$1"; shift; command "$@" >"$output" 2>"$output.err"; printf "%s\n" "$?" >"$output.status"' \
    sh "${remote_path}" "$@"
  status="$(docker exec "${WORKER}" cat "${host_path}.status")"
  if [[ "${status}" != 0 ]]; then
    docker exec "${WORKER}" cat "${host_path}.err" >&2 || true
    docker exec "${WORKER}" rm -f "${host_path}" "${host_path}.err" "${host_path}.status"
    return 1
  fi
  docker exec "${WORKER}" cat "${host_path}"
  command_rc=$?
  docker exec "${WORKER}" rm -f "${host_path}" "${host_path}.err" "${host_path}.status"
  return "${command_rc}"
}
ag() { docker exec "${WORKER}" "$@"; }
fwtest() {
  docker exec \
    -e FRAMEWORK_OVSDB_E2E=1 \
    -e FRAMEWORK_E2E_ACTION="${FRAMEWORK_E2E_ACTION}" \
    -e FRAMEWORK_E2E_RUN_ID="${RUN_SUFFIX}" \
    -e FRAMEWORK_E2E_LOGICAL_SWITCH="${LOGICAL_SWITCH}" \
    -e FRAMEWORK_E2E_OVSDB_MGMT=127.0.0.1:50053 \
    "${WORKER}" /usr/local/bin/framework-nodeagent.test \
    -test.run '^TestFrameworkOVSDBEndpointLifecycle_Live$' -test.v
}
gate() {
  GATE_STATUS["$1"]="PASS"
  printf '%s=PASS\n' "$1" | tee -a "${EVIDENCE_DIR}/acceptance-gates.txt"
}
write_status() {
  local rc="$1" gate
  {
    printf 'run=%s\ncluster=%s\nresult=%s\nfailure-classification=%s\nfailure-gate=%s\n' \
      "${RUN_ID}" "${KIND_CLUSTER}" "$([[ "${rc}" == 0 ]] && echo PASS || echo FAIL)" \
      "${FAILURE_CLASSIFICATION}" "${FAILURE_GATE}"
    for gate in "${GATES[@]}"; do
      local status="${GATE_STATUS[${gate}]:-NOT_RUN}"
      if [[ "${gate}" == "${FAILURE_GATE}" && "${status}" == NOT_RUN ]]; then status=FAIL; fi
      printf '%s=%s\n' "${gate}" "${status}"
    done
    printf 'completed-at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"${EVIDENCE_DIR}/run-status.txt"
}
cleanup() {
  local original_rc=$? reported_rc
  trap - EXIT INT TERM
  set +e
  if [[ "${CREATED_CAPTURE}" == 1 ]]; then
    docker logs "${KIND_CLUSTER}-frr-capture" \
      >"${EVIDENCE_DIR}/frr-capture.log" 2>&1 || true
    docker stop --time 5 "${KIND_CLUSTER}-frr-capture" >/dev/null 2>&1 || CLEANUP_RC=1
    docker rm "${KIND_CLUSTER}-frr-capture" >/dev/null 2>&1 || CLEANUP_RC=1
  fi
  if [[ "${CAPTURES_RUNNING}" == 1 && -n "${AGENT_POD}" ]] &&
     k -n "${NAMESPACE}" get pod "${AGENT_POD}" >/dev/null 2>&1; then
    for name in local-host worker-underlay; do
      ag sh -c 'test ! -s "$1" || kill "$(cat "$1")" 2>/dev/null || true' sh \
        "/run/evpn-node-agent/framework-captures/${name}.pid" || CLEANUP_RC=1
      ag sh -c 'test ! -f "$1" || cat "$1"' sh \
        "/run/evpn-node-agent/framework-captures/${name}.pcap" \
        >"${EVIDENCE_DIR}/${name}.pcap" 2>/dev/null || CLEANUP_RC=1
    done
  fi
  if (( original_rc != 0 )) && kind get clusters 2>/dev/null | grep -Fxq "${KIND_CLUSTER}"; then
    k get pods -A -o wide >"${EVIDENCE_DIR}/pods-at-failure.txt" 2>&1
    k get events -A --sort-by=.lastTimestamp >"${EVIDENCE_DIR}/events-at-failure.txt" 2>&1
    k describe pods -A >"${EVIDENCE_DIR}/pod-descriptions-at-failure.txt" 2>&1
    while IFS='|' read -r namespace pod; do
      [[ -n "${namespace}" && -n "${pod}" ]] || continue
      k -n "${namespace}" logs "${pod}" --all-containers=true \
        >"${EVIDENCE_DIR}/logs-${namespace}-${pod}.txt" 2>&1
    done < <(k get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}')
  fi
  if [[ "${PROVISION_ATTEMPTED}" == 1 ]] && docker container inspect "${WORKER}" >/dev/null 2>&1 &&
     [[ -n "${AGENT_POD}" ]]; then
    FRAMEWORK_E2E_ACTION=cleanup fwtest >"${EVIDENCE_DIR}/framework-cleanup.log" 2>&1 || CLEANUP_RC=1
    FRAMEWORK_E2E_ACTION=delete-network fwtest >"${EVIDENCE_DIR}/framework-network-cleanup.log" 2>&1 || CLEANUP_RC=1
  fi
  e2e_lab_down || CLEANUP_RC=1
  interop_cleanup || CLEANUP_RC=1
  test_image_cleanup || CLEANUP_RC=1
  if (( CLEANUP_RC == 0 )); then
    gate O13
  else
    GATE_STATUS[O13]=FAIL
    printf 'O13=FAIL\n' | tee -a "${EVIDENCE_DIR}/acceptance-gates.txt"
  fi
  if (( original_rc == 0 && CLEANUP_RC == 0 )) &&
     [[ "${PREVIOUS_ATTEMPT_PASSED}" == 1 ]]; then
    gate O14
  else
    GATE_STATUS[O14]=NOT_ESTABLISHED
    printf 'O14=NOT_ESTABLISHED\n' | tee -a "${EVIDENCE_DIR}/acceptance-gates.txt"
  fi
  if [[ -d "${EVIDENCE_DIR}" ]]; then
    reported_rc="${original_rc}"
    if (( CLEANUP_RC != 0 )); then reported_rc=1; fi
    write_status "${reported_rc}"
    printf 'cleanup=%s\n' "$([[ "${CLEANUP_RC}" == 0 ]] && echo PASS || echo FAIL)" \
      >>"${EVIDENCE_DIR}/run-status.txt"
  fi
  if (( original_rc == 0 && CLEANUP_RC != 0 )); then original_rc=1; fi
  exit "${original_rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ "${KIND_CLUSTER}" == "framework-ovsdb-e2e-c4l" ]]
[[ ! -L "${EVIDENCE_ROOT}" ]] || { echo "evidence root must not be a symlink" >&2; exit 1; }
if [[ -e "${EVIDENCE_ROOT}" ]]; then
  [[ -f "${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner" && ! -L "${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner" ]] ||
    { echo "existing evidence root has no regular ownership marker" >&2; exit 1; }
  grep -Fxq "repository=${ROOT}" "${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner" &&
    grep -Fxq "purpose=Framework OVSDB E2E evidence root" "${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner" ||
    { echo "evidence root ownership marker does not match this repository" >&2; exit 1; }
else
  mkdir -m 700 "${EVIDENCE_ROOT}"
  printf 'repository=%s\npurpose=Framework OVSDB E2E evidence root\n' "${ROOT}" \
    >"${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner"
  chmod 600 "${EVIDENCE_ROOT}/.framework-ovsdb-e2e-owner"
fi
mkdir -m 700 "${EVIDENCE_DIR}"
mkdir -p "${EVIDENCE_DIR}/build"
for attempt in "${EVIDENCE_ROOT}"/attempt-*; do
  [[ -d "${attempt}" && "${attempt}" != "${EVIDENCE_DIR}" ]] || continue
  PREVIOUS_ATTEMPT="${attempt}"
done
if [[ -n "${PREVIOUS_ATTEMPT}" && -f "${PREVIOUS_ATTEMPT}/run-status.txt" ]]; then
  if grep -Fxq 'result=PASS' "${PREVIOUS_ATTEMPT}/run-status.txt" &&
     grep -Fxq 'cleanup=PASS' "${PREVIOUS_ATTEMPT}/run-status.txt" &&
     python3 - "${PREVIOUS_ATTEMPT}/run-status.txt" <<'PY'
import sys
values=dict(line.rstrip("\n").split("=",1) for line in open(sys.argv[1]) if "=" in line)
assert all(values.get(f"O{i}")=="PASS" for i in range(1,14)),values
PY
  then PREVIOUS_ATTEMPT_PASSED=1; fi
fi
printf 'run=%s\ncluster=%s\nlogical-switch=%s\nstarted-at=%s\n' \
  "${RUN_ID}" "${KIND_CLUSTER}" "${LOGICAL_SWITCH}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  >"${EVIDENCE_DIR}/run-metadata.txt"
export FRAMEWORK_E2E_RUN_ID="${RUN_SUFFIX}" FRAMEWORK_E2E_LOGICAL_SWITCH="${LOGICAL_SWITCH}"
export FRAMEWORK_E2E_OVSDB_MGMT="127.0.0.1:50053"

if kind get clusters | grep -Fxq "${KIND_CLUSTER}" ||
   docker ps -a --format '{{.Names}}' | grep -Fxq "${KIND_CLUSTER}-worker" ||
   docker ps -a --format '{{.Names}}' | grep -Fxq "${KIND_CLUSTER}-control-plane" ||
   docker container inspect "${FRR_CONTAINER}" >/dev/null 2>&1 ||
   docker network inspect "${INTEROP_NETWORK}" >/dev/null 2>&1; then
  echo "refusing pre-existing dedicated cluster or FRR resource" >&2
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
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-ovsdb" ./cmd/evpn-ovsdb
)
cat >"${EVIDENCE_DIR}/build/Dockerfile" <<'EOF'
FROM evpn4ovn/node-agent-test:cni-24b-base
COPY evpn-node-agent /usr/local/bin/evpn-node-agent
COPY nodeagent.test /usr/local/bin/framework-nodeagent.test
COPY evpn-ovsdb /usr/local/bin/evpn-ovsdb
EOF
docker build --quiet -t "${AGENT_IMAGE}" -f "${EVIDENCE_DIR}/build/Dockerfile" "${EVIDENCE_DIR}/build"
test_image_record_created "${AGENT_IMAGE}"
docker build --quiet -t "${APP_IMAGE}" -f "${ROOT}/tools/e2e/framework/Dockerfile.app" "${EVIDENCE_DIR}/build"
test_image_record_created "${APP_IMAGE}"
docker image inspect "${AGENT_IMAGE}" --format '{{.Id}}' >"${EVIDENCE_DIR}/agent-image.txt"
docker image inspect "${APP_IMAGE}" --format '{{.Id}}' >"${EVIDENCE_DIR}/app-image.txt"

FAILURE_CLASSIFICATION=TOPOLOGY
FAILURE_GATE=O1
CLUSTER_CREATE_ATTEMPTED=1
e2e_lab_up framework-ovsdb
CREATED_CLUSTER=1
nodes="$(docker ps -a --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER}" --format '{{.ID}} {{.Names}} {{.Label "io.x-k8s.kind.cluster"}}' | sort)"
printf '%s\n' "${nodes}" >"${EVIDENCE_DIR}/owned-kind-nodes.txt"
python3 - "${KIND_CLUSTER}" "${nodes}" <<'PY'
import sys
rows=[line.split() for line in sys.argv[2].splitlines() if line.strip()]
assert len(rows)==2 and {r[1] for r in rows}=={sys.argv[1]+"-worker",sys.argv[1]+"-control-plane"}
assert all(r[2]==sys.argv[1] for r in rows),rows
PY
kind load docker-image --name "${KIND_CLUSTER}" "${OVN_IMAGE}" "${AGENT_IMAGE}" "${APP_IMAGE}"
k wait --for=condition=Ready nodes --all --timeout=180s
docker cp "${EVIDENCE_DIR}/build/nodeagent.test" "${WORKER}:/usr/local/bin/framework-nodeagent.test"
docker exec "${WORKER}" mkdir -m 777 "${SHARED_DIR}"
gate O1
gate O2

FAILURE_CLASSIFICATION=UNDERLAY
FAILURE_GATE=O1
interop_setup_underlay
k create namespace "${NAMESPACE}"
k create namespace ovn-system
k create namespace evpn-system
cat >"${EVIDENCE_DIR}/ovn-runtime.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: ovn-runtime, namespace: ovn-system}
spec:
  replicas: 1
  selector: {matchLabels: {app: ovn-runtime}}
  template:
    metadata: {labels: {app: ovn-runtime}}
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
        - {name: OVN_EVPN_LOCAL_IP, value: "${LOCAL_VTEP}"}
        - {name: OVN_EVPN_VXLAN_PORTS, value: "4789"}
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
        - {name: evidence, mountPath: /framework-e2e-share}
      volumes:
      - {name: ovs, hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}}
      - {name: ovn, hostPath: {path: /run/ovn, type: DirectoryOrCreate}}
      - {name: evidence, hostPath: {path: ${SHARED_DIR}, type: Directory}}
EOF
k apply -f "${EVIDENCE_DIR}/ovn-runtime.yaml"
k -n ovn-system rollout status deployment/ovn-runtime --timeout=240s
OVN_POD="$(k -n ovn-system get pod -l app=ovn-runtime -o jsonpath='{.items[0].metadata.name}')"
rt sh -ec 'ovn-nbctl --version; ovs-vsctl --version' | tee "${EVIDENCE_DIR}/dataplane-version.txt"
docker image inspect "${OVN_IMAGE}" >"${EVIDENCE_DIR}/dataplane-image.json"
k version -o json | tee "${EVIDENCE_DIR}/kubernetes-version.json" >/dev/null
rt ovs-vsctl set Open_vSwitch . "external_ids:ovn-evpn-local-ip=${LOCAL_VTEP}" external_ids:ovn-evpn-vxlan-ports=4789
export_setup_provider
cat >"${EVIDENCE_DIR}/provider-config.yaml" <<EOF
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata: {name: framework-ovsdb-e2e, namespace: k8gobgp-system}
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
      description: "framework-ovsdb-e2e-external-frr-peer"
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
gate O3

cat >"${EVIDENCE_DIR}/ovsdb-mgmt.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: ovsdb-mgmt, namespace: ${NAMESPACE}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ovsdb-mgmt}}
  template:
    metadata: {labels: {app: ovsdb-mgmt}}
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
      - {name: ovs, hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}}
      - {name: ovn, hostPath: {path: /run/ovn, type: DirectoryOrCreate}}
---
apiVersion: v1
kind: Service
metadata: {name: ovsdb-mgmt, namespace: ${NAMESPACE}}
spec:
  selector: {app: ovsdb-mgmt}
  ports:
  - {name: grpc, port: 50053, targetPort: 50053}
EOF
k apply -f "${EVIDENCE_DIR}/ovsdb-mgmt.yaml"
k -n "${NAMESPACE}" rollout status deployment/ovsdb-mgmt --timeout=120s
PROVISION_ATTEMPTED=1
FAILURE_CLASSIFICATION=FRAMEWORK-PROVISIONING
FAILURE_GATE=O3
FRAMEWORK_E2E_ACTION=network fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-network-provision.log"

cat >"${EVIDENCE_DIR}/export-config.json" <<EOF
{"Enabled":true,"DryRun":false,"ProcessContinuity":true,"NodeName":"${WORKER}","LocalChassis":"${LOCAL_CHASSIS}","GRPCEndpoint":"unix:///var/run/gobgp/gobgp.sock","ASN":64512,"RouterID":"${LOCAL_VTEP}","Networks":[{"NetworkID":"framework-ovsdb-e2e","LogicalSwitch":"${LOGICAL_SWITCH}","Enabled":true,"ExporterSource":"framework","VNI":${FRAMEWORK_VNI},"RouteDistinguisher":"${LOCAL_VTEP}:${FRAMEWORK_VNI}","RouteTargets":["64512:${FRAMEWORK_VNI}"],"LocalVTEP":"${LOCAL_VTEP}"}]}
EOF
cat >"${EVIDENCE_DIR}/ovsdb-importer.yaml" <<EOF
gobgp:
  endpoint: unix:///var/run/gobgp/gobgp.sock
  asn: 64512
  router_id: "${LOCAL_VTEP}"
local_vtep: "${LOCAL_VTEP}"
ovn_nb_endpoint: unix:/var/run/ovn/ovnnb_db.sock
ovsdb_endpoint: unix:/var/run/openvswitch/db.sock
poll_interval: 1s
udp_port: 4789
exclusive_writer: true
network:
  id: framework-ovsdb-e2e
  logical_switch: "${LOGICAL_SWITCH}"
  vni: ${FRAMEWORK_VNI}
  physical_network: "${PHYSICAL_NETWORK}"
  provider_bridge: "${PROVIDER_BRIDGE}"
import_route_targets: ["64512:${FRAMEWORK_VNI}"]
allowed_remote_vteps: ["${FRR_VTEP}"]
EOF
docker exec "${WORKER}" mkdir -p /run/evpn-node-agent
docker exec "${WORKER}" chmod 700 /run/evpn-node-agent
docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.json' \
  <"${EVIDENCE_DIR}/export-config.json"
docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/ovsdb-importer.yaml' \
  <"${EVIDENCE_DIR}/ovsdb-importer.yaml"
cat >"${EVIDENCE_DIR}/node-agent.yaml" <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: framework-node-agent, namespace: ${NAMESPACE}}
spec:
  selector: {matchLabels: {app: framework-node-agent}}
  template:
    metadata: {labels: {app: framework-node-agent}}
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
          while ! test -S /var/run/ovn/ovnnb_db.sock || ! test -S /var/run/openvswitch/db.sock || ! test -f /run/evpn-node-agent/export-config.json; do sleep 1; done
          exec /usr/local/bin/evpn-node-agent --socket=/run/evpn-node-agent/agent.sock --mode=real-ovn-ovs --network-backend=linux-veth --ovn-nb-endpoint=unix:/var/run/ovn/ovnnb_db.sock --ovsdb-endpoint=unix:/var/run/openvswitch/db.sock ${EXPORT_FLAGS}
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
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: ovsdb-route-importer, namespace: ${NAMESPACE}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ovsdb-route-importer}}
  template:
    metadata: {labels: {app: ovsdb-route-importer}}
    spec:
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      containers:
      - name: importer
        image: ${AGENT_IMAGE}
        imagePullPolicy: Never
        command: ["/usr/local/bin/evpn-ovsdb", "--config=/run/evpn-node-agent/ovsdb-importer.yaml"]
        securityContext: {runAsUser: 0}
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
        - {name: agent, mountPath: /run/evpn-node-agent, readOnly: true}
        - {name: gobgp, mountPath: /var/run/gobgp, readOnly: true}
      volumes:
      - {name: ovs, hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}}
      - {name: ovn, hostPath: {path: /run/ovn, type: DirectoryOrCreate}}
      - {name: agent, hostPath: {path: /run/evpn-node-agent, type: Directory}}
      - {name: gobgp, hostPath: {path: /run/gobgp-export-test, type: DirectoryOrCreate}}
EOF
k apply -f "${EVIDENCE_DIR}/node-agent.yaml"
k -n "${NAMESPACE}" rollout status ds/framework-node-agent --timeout=180s
k -n "${NAMESPACE}" rollout status deployment/ovsdb-route-importer --timeout=120s
AGENT_POD="$(k -n "${NAMESPACE}" get pod -l app=framework-node-agent -o jsonpath='{.items[0].metadata.name}')"
IMPORTER_POD="$(k -n "${NAMESPACE}" get pod -l app=ovsdb-route-importer -o jsonpath='{.items[0].metadata.name}')"
gate O4

FAILURE_CLASSIFICATION=FRAMEWORK-PROVISIONING
FAILURE_GATE=O3
FRAMEWORK_E2E_ACTION=endpoints fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-provision.log"
python3 - "${EVIDENCE_DIR}/framework-provision.log" "${EVIDENCE_DIR}/framework-endpoints.json" <<'PY'
import json,pathlib,sys
lines=pathlib.Path(sys.argv[1]).read_text().splitlines()
prefix="FRAMEWORK_E2E_RECEIPT="
matches=[line.split(prefix,1)[1] for line in lines if prefix in line]
assert len(matches)==1,matches
receipt=json.loads(matches[0])
assert len(receipt["endpoints"])==2,receipt
assert all(e["networkId"]=="framework-ovsdb-e2e" for e in receipt["endpoints"]),receipt
pathlib.Path(sys.argv[2]).write_text(json.dumps(receipt,indent=2)+"\n")
PY
gate O3

FAILURE_CLASSIFICATION=OVN-OVS
FAILURE_GATE=O5
rt ovn-nbctl --wait=sb sync
rt ovn-nbctl --format=json --columns=name,addresses,external_ids list Logical_Switch_Port \
  >"${EVIDENCE_DIR}/ovn-nb-ports-initial.json"
rt ovn-sbctl --format=json --columns=logical_port,chassis,up list Port_Binding \
  >"${EVIDENCE_DIR}/ovn-sb-bindings-initial.json"
rt ovs-vsctl --format=json --columns=name,type,options,ofport,external_ids list Interface \
  >"${EVIDENCE_DIR}/ovs-interfaces-initial.json"
python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
receipt=json.load((r/"framework-endpoints.json").open())
nb=json.load((r/"ovn-nb-ports-initial.json").open())
rows=[dict(zip(nb["headings"],row)) for row in nb["data"]]
for endpoint in receipt["endpoints"]:
    matches=[row for row in rows if row["name"]==endpoint["logicalSwitchPort"]]
    assert len(matches)==1,matches
    assert endpoint["mac"] in str(matches[0]["addresses"]),matches
    assert endpoint["ip"] in str(matches[0]["addresses"]),matches
PY
for port in $(python3 -c 'import json,sys; [print(e["logicalSwitchPort"]) for e in json.load(open(sys.argv[1]))["endpoints"]]' "${EVIDENCE_DIR}/framework-endpoints.json"); do
  binding="$(rt ovn-sbctl --bare --columns=_uuid find Port_Binding "logical_port=${port}")"
  [[ -n "${binding}" ]]
  chassis="$(rt ovn-sbctl --bare get Port_Binding "${binding}" chassis | tr -d '[]\"')"
  [[ -n "${chassis}" ]]
  [[ "$(rt ovn-sbctl --bare get Chassis "${chassis}" name | tr -d '"')" == "${LOCAL_CHASSIS}" ]]
  iface="$(rt ovs-vsctl --bare --columns=name find Interface "external_ids:iface-id=${port}")"
  [[ -n "${iface}" ]]
  [[ "$(rt ovs-vsctl --bare get Interface "${iface}" ofport | tr -d '[] ')" =~ ^[1-9][0-9]*$ ]]
done
gate O5

FAILURE_CLASSIFICATION=GOBGP-FRR
FAILURE_GATE=O6
deadline=$((SECONDS+150))
while (( SECONDS < deadline )); do
  if provider_exec gobgp neighbor "${FRR_PEER}" 2>/dev/null | grep -qi Established &&
     docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" 2>/dev/null | grep -qi Established; then
    break
  fi
  sleep 2
done
provider_exec gobgp neighbor "${FRR_PEER}" | tee "${EVIDENCE_DIR}/gobgp-neighbor.txt"
docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" \
  | tee "${EVIDENCE_DIR}/frr-neighbor.txt"
grep -qi Established "${EVIDENCE_DIR}/gobgp-neighbor.txt"
grep -qi Established "${EVIDENCE_DIR}/frr-neighbor.txt"
provider_exec gobgp global rib -a evpn | tee "${EVIDENCE_DIR}/gobgp-rib-initial.txt"
docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
  | tee "${EVIDENCE_DIR}/frr-rib-initial.txt"
grep -q '10.244.240.10' "${EVIDENCE_DIR}/frr-rib-initial.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-initial.txt"
gate O6

FAILURE_CLASSIFICATION=OVSDB-ROUTE-IMPORT
FAILURE_GATE=O7
[[ "$(k -n "${NAMESPACE}" get pod "${IMPORTER_POD}" -o jsonpath='{.status.phase}')" == "Running" ]]
[[ "$(k -n "${NAMESPACE}" get pod "${IMPORTER_POD}" -o jsonpath='{.status.containerStatuses[0].ready}')" == "true" ]]
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  rt ovs-vsctl --format=json --columns=name,type,options,external_ids list Interface \
    >"${EVIDENCE_DIR}/ovsdb-imported-route-state.json"
  if python3 - "${EVIDENCE_DIR}/ovsdb-imported-route-state.json" "${FRR_VTEP}" "${FRAMEWORK_VNI}" \
    >/dev/null 2>&1 <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
def ovs_map(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
rows=[dict(zip(d["headings"],row)) for row in d["data"]]
matches=[]
for row in rows:
    ids=ovs_map(row["external_ids"])
    options=ovs_map(row["options"])
    if ids.get("evpn4ovn-network-id")=="framework-ovsdb-e2e" and row["type"]=="vxlan" and \
       options.get("remote_ip")==sys.argv[2] and options.get("key")==sys.argv[3]:
        matches.append(row)
assert len(matches)==1,matches
PY
  then break; fi
  sleep 2
done
grep -q '10.244.240.12' "${EVIDENCE_DIR}/gobgp-rib-initial.txt"
python3 - "${EVIDENCE_DIR}/ovsdb-imported-route-state.json" "${FRR_VTEP}" "${FRAMEWORK_VNI}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
def ovs_map(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
rows=[dict(zip(d["headings"],row)) for row in d["data"]]
matches=[row for row in rows if row["type"]=="vxlan" and
         ovs_map(row["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e" and
         ovs_map(row["options"]).get("remote_ip")==sys.argv[2] and
         ovs_map(row["options"]).get("key")==sys.argv[3]]
assert len(matches)==1,matches
print("production OVSDB importer programmed the received Type-2 route=PASS")
PY
gate O7

FAILURE_CLASSIFICATION=OVN-OVS-PROGRAMMING
FAILURE_GATE=O8
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  if rt ovs-vsctl br-exists "${PROVIDER_BRIDGE}" >/dev/null 2>&1 &&
     rt ovs-vsctl --format=json --columns=name,type,options,external_ids list Interface \
       >"${EVIDENCE_DIR}/ovs-interfaces-imported.json" 2>/dev/null &&
     grep -q 'remote_ip' "${EVIDENCE_DIR}/ovs-interfaces-imported.json"; then break; fi
  sleep 2
done
rt ovs-vsctl show >"${EVIDENCE_DIR}/ovs-show-imported.txt"
rt ovn-nbctl --format=json --columns=name,type,options,external_ids list Logical_Switch_Port \
  >"${EVIDENCE_DIR}/ovn-nb-lsps-imported.json"
rt ovn-sbctl --format=json --columns=logical_port,chassis,up list Port_Binding \
  >"${EVIDENCE_DIR}/ovn-sb-bindings-imported.json"
python3 - "${EVIDENCE_DIR}" "${FRR_VTEP}" "${FRAMEWORK_VNI}" "${PROVIDER_BRIDGE}" \
  "${PHYSICAL_NETWORK}" "${LOGICAL_SWITCH}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
data=json.load((r/"ovs-interfaces-imported.json").open())
def ovs_map(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
rows=[dict(zip(data["headings"],row)) for row in data["data"]]
tunnels=[row for row in rows if row["type"]=="vxlan" and
         ovs_map(row["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
assert len(tunnels)==1,tunnels
options=ovs_map(tunnels[0]["options"])
ids=ovs_map(tunnels[0]["external_ids"])
assert options.get("remote_ip")==sys.argv[2] and options.get("key")==sys.argv[3] and options.get("dst_port")=="4789",options
assert ids.get("evpn4ovn-managed-by")=="model1-nbdb",ids
assert ids.get("evpn4ovn-network-id")=="framework-ovsdb-e2e",ids
assert ids.get("evpn4ovn-remote-vtep")==sys.argv[2],ids
assert ids.get("evpn4ovn-vni")==sys.argv[3],ids
import hashlib
addresses=json.dumps(["0a:58:f0:01:00:0c 10.244.240.12"],separators=(",",":")).encode()
assert ids.get("evpn4ovn-endpoints-sha256")==hashlib.sha256(addresses).hexdigest(),ids
nb=json.load((r/"ovn-nb-lsps-imported.json").open())
nb_rows=[dict(zip(nb["headings"],row)) for row in nb["data"]]
localnet=[row for row in nb_rows if row["type"]=="localnet" and
          ovs_map(row["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"]
assert len(localnet)==1,localnet
localnet_options=ovs_map(localnet[0]["options"])
localnet_ids=ovs_map(localnet[0]["external_ids"])
assert localnet_options.get("network_name")==sys.argv[5],localnet_options
assert localnet_ids.get("evpn4ovn-logical-switch")==sys.argv[6],localnet_ids
assert localnet_ids.get("evpn4ovn-provider-bridge")==sys.argv[4],localnet_ids
assert localnet_ids.get("evpn4ovn-vni")==sys.argv[3],localnet_ids
(r/"ovsdb-tunnel-name.txt").write_text(tunnels[0]["name"]+"\n")
(r/"ovn-localnet-port-name.txt").write_text(localnet[0]["name"]+"\n")
print("route identity mapped to owned localnet LSP, provider bridge and OVS VXLAN tunnel=PASS")
PY
TUNNEL="$(cat "${EVIDENCE_DIR}/ovsdb-tunnel-name.txt")"
LOCALNET_PORT="$(cat "${EVIDENCE_DIR}/ovn-localnet-port-name.txt")"
[[ "$(rt ovs-vsctl port-to-br "${TUNNEL}")" == "${PROVIDER_BRIDGE}" ]]
bridge_mappings="$(rt ovs-vsctl --bare get Open_vSwitch . external_ids:ovn-bridge-mappings | tr -d '"')"
grep -Fq "${PHYSICAL_NETWORK}:${PROVIDER_BRIDGE}" <<<"${bridge_mappings}"
rt ovs-vsctl show >"${EVIDENCE_DIR}/ovs-show-imported.txt"
grep -Fq "patch-${LOCALNET_PORT}-to-br-int" "${EVIDENCE_DIR}/ovs-show-imported.txt"
grep -Fq "patch-br-int-to-${LOCALNET_PORT}" "${EVIDENCE_DIR}/ovs-show-imported.txt"
gate O8

FAILURE_CLASSIFICATION=PACKET-PATH
FAILURE_GATE=O10
REMOTE_CAPTURE_PID_FILE="/tmp/framework-ovsdb-capture-${RUN_SUFFIX}.pid"
docker exec -d "${FRR_CONTAINER}" sh -ec \
  "ip netns exec evpnwork sh -ec 'echo \$\$ > ${REMOTE_CAPTURE_PID_FILE}; exec sleep 86400'"
REMOTE_WORKLOAD_PID="$(docker exec "${FRR_CONTAINER}" cat "${REMOTE_CAPTURE_PID_FILE}")"
[[ "${REMOTE_WORKLOAD_PID}" =~ ^[0-9]+$ ]]
LOCAL_VTEP_IFACE="$(ag ip -j addr show | python3 -c 'import json,sys; v="198.18.0.2"; m=[x["ifname"] for x in json.load(sys.stdin) if any(a.get("local")==v for a in x.get("addr_info",[]))]; assert len(m)==1,m; print(m[0])')"
ag mkdir -p /run/evpn-node-agent/framework-captures
CAPTURES_RUNNING=1
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- sh -ec '
  d=/run/evpn-node-agent/framework-captures
  for f in local-host.pcap worker-underlay.pcap local-host.pid worker-underlay.pid; do test ! -e "$d/$f" || exit 1; done
  nohup tcpdump -U -n -i "$1" -w "$d/local-host.pcap" "arp or icmp" >"$d/local-host.log" 2>&1 </dev/null &
  echo $! >"$d/local-host.pid"
  nohup tcpdump -U -n -i "$2" -w "$d/worker-underlay.pcap" "udp port 4789" >"$d/worker-underlay.log" 2>&1 </dev/null &
  echo $! >"$d/worker-underlay.pid"
' sh "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["endpoints"][0]["interfaceName"])' "${EVIDENCE_DIR}/framework-endpoints.json")" "${LOCAL_VTEP_IFACE}"
if docker container inspect "${KIND_CLUSTER}-frr-capture" >/dev/null 2>&1; then
  echo "refusing pre-existing FRR capture container" >&2
  exit 1
fi
CREATED_CAPTURE=1
docker run -d --name "${KIND_CLUSTER}-frr-capture" \
  --label "evpn4ovn.test-run=${KIND_CLUSTER}" --network "container:${FRR_CONTAINER}" \
  --pid "container:${FRR_CONTAINER}" --privileged -v "${EVIDENCE_DIR}:/evidence" \
  --entrypoint /bin/sh "${AGENT_IMAGE}" -ec '
    tcpdump -U -n -i eth0 -w /evidence/frr-underlay.pcap "udp port 4789" &
    a=$!
    tcpdump -U -n -i vxlan1000 -w /evidence/frr-vxlan.pcap "arp or icmp" &
    b=$!
    nsenter -t '"${REMOTE_WORKLOAD_PID}"' -n tcpdump -U -n -i frr-work0 -w /evidence/remote-workload.pcap "arp or icmp" &
    c=$!
    trap "kill $a $b $c 2>/dev/null || true; wait" TERM INT
    wait
  ' >"${EVIDENCE_DIR}/frr-capture-container-id.txt"
sleep 2
docker exec "${KIND_CLUSTER}-frr-capture" true
for name in local-host worker-underlay; do
  ag sh -c 'kill -0 "$(cat "$1")"' sh "/run/evpn-node-agent/framework-captures/${name}.pid"
done
gate O10

FAILURE_CLASSIFICATION=OVN-OVS-FORWARDING
FAILURE_GATE=O9
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
  ip netns exec "fw-${RUN_SUFFIX}-a" ping -c 3 -W 2 "${REMOTE_IP}" \
  | tee "${EVIDENCE_DIR}/endpoint-a-to-remote-ping.txt"
docker exec "${KIND_CLUSTER}-frr-capture" nsenter -t "${REMOTE_WORKLOAD_PID}" -n \
  ping -c 3 -W 2 10.244.240.10 | tee "${EVIDENCE_DIR}/remote-to-endpoint-a-ping.txt"
gate O9

FAILURE_CLASSIFICATION=LOCAL-WITHDRAWAL
FAILURE_GATE=O11
FRAMEWORK_E2E_ACTION=delete-a fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-delete-a.log"
deadline=$((SECONDS+120))
while (( SECONDS < deadline )); do
  ag cat /run/evpn-node-agent/export-status.json >"${EVIDENCE_DIR}/export-status-after-delete-a.json" 2>/dev/null || true
  if python3 - "${EVIDENCE_DIR}/export-status-after-delete-a.json" <<'PY' >/dev/null 2>&1
import json,sys
s=json.load(open(sys.argv[1]))
assert not s.get("Error"),s
assert len((s.get("Result") or {}).get("Desired") or [])==1,s
assert len((s.get("Result") or {}).get("Applied") or [])==1,s
assert all((item.get("Endpoint") or item).get("MACAddress","").lower()=="0a:58:f0:01:00:0b" for item in (s.get("Result") or {}).get("Applied") or []),s
PY
  then break; fi
  sleep 2
done
docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
  >"${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
! grep -q '10.244.240.10' "${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-after-delete-a.txt"
k -n "${NAMESPACE}" exec "${AGENT_POD}" -c agent -- \
  ip netns exec "fw-${RUN_SUFFIX}-b" ping -c 3 -W 2 "${REMOTE_IP}" \
  | tee "${EVIDENCE_DIR}/survivor-to-remote-ping.txt"
gate O11

FAILURE_CLASSIFICATION=REMOTE-WITHDRAWAL
FAILURE_GATE=O12
docker exec "${FRR_CONTAINER}" ip link del frr-host0
deadline=$((SECONDS+150))
while (( SECONDS < deadline )); do
  if ! rt ovs-vsctl --format=json --columns=name,type,options,external_ids list Interface \
       >"${EVIDENCE_DIR}/ovs-interfaces-after-withdrawal.json" 2>/dev/null; then sleep 2; continue; fi
  if ! gobgp_rib_after_withdrawal="$(provider_exec gobgp global rib -a evpn \
       2>"${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.stderr")" ||
     [[ -z "${gobgp_rib_after_withdrawal}" ]]; then
    sleep 2
    continue
  fi
  printf '%s\n' "${gobgp_rib_after_withdrawal}" \
    >"${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c 'show bgp l2vpn evpn route type macip' \
    >"${EVIDENCE_DIR}/frr-rib-after-remote-withdrawal.txt"
  docker exec "${FRR_CONTAINER}" vtysh -c "show bgp neighbor ${LOCAL_VTEP}" \
    >"${EVIDENCE_DIR}/frr-neighbor-after-remote-withdrawal.txt"
  if ! gobgp_neighbor_after_withdrawal="$(provider_exec gobgp neighbor "${FRR_PEER}" \
       2>"${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.stderr")" ||
     [[ -z "${gobgp_neighbor_after_withdrawal}" ]]; then
    sleep 2
    continue
  fi
  printf '%s\n' "${gobgp_neighbor_after_withdrawal}" \
    >"${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.txt"
  if python3 - "${EVIDENCE_DIR}/ovs-interfaces-after-withdrawal.json" <<'PY'
import json,sys
data=json.load(open(sys.argv[1]))
def ovs_map(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
rows=[dict(zip(data["headings"],row)) for row in data["data"]]
raise SystemExit(any(
    row["type"]=="vxlan" and ovs_map(row["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"
    for row in rows
))
PY
  then
    ! grep -qi '0a:58:f0:01:00:0c' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt" &&
      ! grep -qi '0a:58:f0:01:00:0c' "${EVIDENCE_DIR}/frr-rib-after-remote-withdrawal.txt" &&
      grep -Fq '[type:multicast]' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt" &&
      grep -qi Established "${EVIDENCE_DIR}/frr-neighbor-after-remote-withdrawal.txt" &&
      grep -qi Established "${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.txt" &&
      grep -q '10.244.240.11' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt" &&
      grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-after-remote-withdrawal.txt" && break
  fi
  sleep 2
done
[[ -s "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt" ]] ||
  { cat "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.stderr" >&2; echo "GoBGP withdrawal RIB snapshot unavailable" >&2; exit 1; }
[[ -s "${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.txt" ]] ||
  { cat "${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.stderr" >&2; echo "GoBGP post-withdrawal neighbor snapshot unavailable" >&2; exit 1; }
! grep -qi '0a:58:f0:01:00:0c' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt"
! grep -qi '0a:58:f0:01:00:0c' "${EVIDENCE_DIR}/frr-rib-after-remote-withdrawal.txt"
grep -Fq '[type:multicast]' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt"
grep -qi Established "${EVIDENCE_DIR}/frr-neighbor-after-remote-withdrawal.txt"
grep -qi Established "${EVIDENCE_DIR}/gobgp-neighbor-after-remote-withdrawal.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/gobgp-rib-after-remote-withdrawal.txt"
grep -q '10.244.240.11' "${EVIDENCE_DIR}/frr-rib-after-remote-withdrawal.txt"
python3 - "${EVIDENCE_DIR}/ovs-interfaces-after-withdrawal.json" <<'PY'
import json,sys
data=json.load(open(sys.argv[1]))
def ovs_map(value):
    if isinstance(value,list) and len(value)==2 and value[0]=="map":
        return dict(value[1])
    return value if isinstance(value,dict) else {}
rows=[dict(zip(data["headings"],row)) for row in data["data"]]
assert not any(
    row["type"]=="vxlan" and ovs_map(row["external_ids"]).get("evpn4ovn-network-id")=="framework-ovsdb-e2e"
    for row in rows
),rows
initial=json.load(open(sys.argv[1].replace("after-withdrawal","initial")))
initial_rows=[dict(zip(initial["headings"],row)) for row in initial["data"]]
foreign={row["name"] for row in initial_rows if row["type"]=="vxlan" and
         ovs_map(row["external_ids"]).get("evpn4ovn-network-id")!="framework-ovsdb-e2e"}
remaining={row["name"] for row in rows}
assert foreign and foreign <= remaining,(foreign,remaining)
PY
if rt ovs-vsctl br-exists "${PROVIDER_BRIDGE}"; then
  echo "owned provider bridge remained after remote route withdrawal" >&2
  exit 1
fi
rt ovn-nbctl --format=json --columns=name,addresses,external_ids list Logical_Switch_Port \
  >"${EVIDENCE_DIR}/ovn-nb-ports-after-remote-withdrawal.json"
python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
r=pathlib.Path(sys.argv[1])
receipt=json.load((r/"framework-endpoints.json").open())
data=json.load((r/"ovn-nb-ports-after-remote-withdrawal.json").open())
names={row[0] for row in data["data"]}
assert receipt["endpoints"][1]["logicalSwitchPort"] in names,names
assert not any(name.startswith("m1ln-") for name in names),names
PY
gate O12

FAILURE_CLASSIFICATION=PACKET-CAPTURE
FAILURE_GATE=O10
for name in local-host worker-underlay; do
  ag sh -c 'test ! -s "$1" || kill "$(cat "$1")" 2>/dev/null || true' sh \
    "/run/evpn-node-agent/framework-captures/${name}.pid"
  ag cat "/run/evpn-node-agent/framework-captures/${name}.pcap" >"${EVIDENCE_DIR}/${name}.pcap"
done
docker stop --time 5 "${KIND_CLUSTER}-frr-capture" >/dev/null
docker cp "${KIND_CLUSTER}-frr-capture:/evidence/frr-underlay.pcap" "${EVIDENCE_DIR}/frr-underlay.pcap"
docker cp "${KIND_CLUSTER}-frr-capture:/evidence/frr-vxlan.pcap" "${EVIDENCE_DIR}/frr-vxlan.pcap"
docker cp "${KIND_CLUSTER}-frr-capture:/evidence/remote-workload.pcap" "${EVIDENCE_DIR}/remote-workload.pcap"
CREATED_CAPTURE=0
docker rm "${KIND_CLUSTER}-frr-capture" >/dev/null
for pcap in local-host worker-underlay frr-underlay frr-vxlan remote-workload; do
  tcpdump -nn -r "${EVIDENCE_DIR}/${pcap}.pcap" \
    >"${EVIDENCE_DIR}/${pcap}.decoded.txt" 2>"${EVIDENCE_DIR}/${pcap}.decode.log"
  test -s "${EVIDENCE_DIR}/${pcap}.decoded.txt"
done
grep -q '4789' "${EVIDENCE_DIR}/worker-underlay.decoded.txt"
grep -q '4789' "${EVIDENCE_DIR}/frr-underlay.decoded.txt"
grep -q '10.244.240.10' "${EVIDENCE_DIR}/remote-workload.decoded.txt"
grep -q '10.244.240.12' "${EVIDENCE_DIR}/local-host.decoded.txt"
gate O10
gate O12

FAILURE_CLASSIFICATION=CLEANUP
FAILURE_GATE=O13
FRAMEWORK_E2E_ACTION=cleanup fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-final-cleanup.log"
FRAMEWORK_E2E_ACTION=delete-network fwtest 2>&1 | tee "${EVIDENCE_DIR}/framework-network-cleanup.log"
PROVISION_ATTEMPTED=0
rt ovn-nbctl --wait=sb sync
rt ovn-nbctl --bare --columns=name find Logical_Switch "name=${LOGICAL_SWITCH}" \
  >"${EVIDENCE_DIR}/logical-switch-after-cleanup.txt"
test ! -s "${EVIDENCE_DIR}/logical-switch-after-cleanup.txt"
GATE_STATUS[O13]=FRAMEWORK_RESOURCES_CLEAN
printf 'O13=FRAMEWORK_RESOURCES_CLEAN\n' | tee -a "${EVIDENCE_DIR}/acceptance-gates.txt"
FAILURE_CLASSIFICATION=PASS
FAILURE_GATE=ALL_O1_O14
printf '%s\n' "Framework OVSDB E2E completed; evidence=${EVIDENCE_DIR}"
