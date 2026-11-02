#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
generate_cluster_name() {
  local suffix
  suffix="$(python3 -c 'import secrets; print(secrets.token_hex(6))')" || return 1
  printf 'cni-24b-%s-%s\n' "$(date -u +%Y%m%d%H%M%S)" \
    "${suffix}"
}
validate_cluster_name() {
  # Reserve 14 characters for -control-plane within a 63-character DNS label.
  if (( ${#KIND_CLUSTER} > 49 )) ||
    [[ ! "${KIND_CLUSTER}" =~ ^cni-24b-[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ||
       "${KIND_CLUSTER}" == *..* || "${KIND_CLUSTER}" == *.-* || "${KIND_CLUSTER}" == *-.* ]]; then
    echo "invalid disposable cluster name: ${KIND_CLUSTER}; require cni-24b- prefix, lowercase DNS labels (kind: ^[a-z0-9.-]+$), maximum 49 characters; no normalization or truncation" >&2
    return 1
  fi
}
KIND_CLUSTER="${KIND_CLUSTER:-$(generate_cluster_name)}" || exit 1
EVPN_FRR_INTEROP_TEST="${EVPN_FRR_INTEROP_TEST:-0}"
EVPN_FRR_INTEROP_INITIAL_ONLY="${EVPN_FRR_INTEROP_INITIAL_ONLY:-0}"
EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC="${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC:-0}"
EVPN_FRR_INTEROP_NO_REPLACEMENT="${EVPN_FRR_INTEROP_NO_REPLACEMENT:-0}"
EVPN_FRR_INTEROP_FULL_LIFECYCLE="${EVPN_FRR_INTEROP_FULL_LIFECYCLE:-0}"
[[ "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 0 || "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ]] || {
  echo "EVPN_FRR_INTEROP_INITIAL_ONLY must be 0 or 1" >&2; exit 1;
}
[[ "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 0 || "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ]] || {
  echo "EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC must be 0 or 1" >&2; exit 1;
}
[[ "${EVPN_FRR_INTEROP_NO_REPLACEMENT}" == 0 || "${EVPN_FRR_INTEROP_NO_REPLACEMENT}" == 1 ]] || {
  echo "EVPN_FRR_INTEROP_NO_REPLACEMENT must be 0 or 1" >&2; exit 1;
}
[[ "${EVPN_FRR_INTEROP_FULL_LIFECYCLE}" == 0 || "${EVPN_FRR_INTEROP_FULL_LIFECYCLE}" == 1 ]] || {
  echo "EVPN_FRR_INTEROP_FULL_LIFECYCLE must be 0 or 1" >&2; exit 1;
}
if [[ "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 && "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then
  echo "initial-only mode requires EVPN_FRR_INTEROP_TEST=1" >&2; exit 1
fi
if [[ "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 &&
      ( "${EVPN_FRR_INTEROP_TEST}" != 1 || "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ) ]]; then
  echo "binding diagnostic mode requires FRR interop and cannot be combined with initial-only mode" >&2
  exit 1
fi
if [[ "${EVPN_FRR_INTEROP_NO_REPLACEMENT}" == 1 &&
      ( "${EVPN_FRR_INTEROP_TEST}" != 1 || "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ||
        "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ) ]]; then
  echo "no-replacement acceptance mode requires FRR interop and cannot be combined with initial-only or binding diagnostic mode" >&2
  exit 1
fi
if [[ "${EVPN_FRR_INTEROP_FULL_LIFECYCLE}" == 1 &&
      ( "${EVPN_FRR_INTEROP_TEST}" != 1 || "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ||
        "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ||
        "${EVPN_FRR_INTEROP_NO_REPLACEMENT}" == 1 ) ]]; then
  echo "full-lifecycle mode requires FRR interop and cannot be combined with initial-only, binding diagnostic, or no-replacement mode" >&2
  exit 1
fi
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
  EVPN_EXPORT_TEST=1
  [[ "${KEEP_CLUSTER:-0}" != 1 ]] || { echo "FRR interop mode requires disposable cluster cleanup" >&2; exit 1; }
fi
EVIDENCE_DIR="${EVIDENCE_DIR:-/tmp/${KIND_CLUSTER}-evidence}"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 && -e "${EVIDENCE_DIR}" ]]; then
  echo "refusing pre-existing evidence directory: ${EVIDENCE_DIR}" >&2; exit 1
fi
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
NODE_IMAGE="kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c"
OVN_ID="sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
OVN_IMAGE="evpn4ovn/ovn-ovs:kind-26.09"
BASE_ID="sha256:f01f29f4d8b1fd4f377103015aa7b7dab6f8189416ce2a363f63d7e538c4e5a7"
source "${ROOT}/tools/lib/cni-agent-image-test.sh"
source "${ROOT}/tools/lib/containerlab-e2e.sh"
e2e_lab_acquire
EVPN_AGENT_IMAGE_MODE="${EVPN_AGENT_IMAGE_MODE:-build}"
if [[ "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ]]; then
  EVPN_AGENT_IMAGE_MODE=pinned
fi
case "${EVPN_AGENT_IMAGE_MODE}" in
  pinned) AGENT_IMAGE="${EVPN_PINNED_AGENT_IMAGE:-${PINNED_AGENT_REF}}" ;;
  build) AGENT_IMAGE="evpn4ovn/node-agent-test:${KIND_CLUSTER}" ;;
  *) echo "EVPN_AGENT_IMAGE_MODE must be pinned or build" >&2; exit 1 ;;
esac
MULTUS_SHA="88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e"
CREATED=0
CLUSTER_CREATE_ATTEMPTED=0
CLUSTER_CREATION_STATUS=NOT_STARTED
CLUSTER_CLEANUP_STATUS=SKIPPED
CLUSTER_OWNED_NODES=""
WORKER=""
AGENT_POD=""
OVN_POD=""
EXPORT_FLAGS=""
REMOTE_FLAGS=""
EXPORT_MOUNTS=""
EXPORT_VOLUMES=""
mkdir -p "${EVIDENCE_DIR}/build"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
  source "${ROOT}/tools/lib/cni-evpn-frr-interop-test.sh"
fi
chmod 700 "${EVIDENCE_DIR}"
export KUBECONFIG="${EVIDENCE_DIR}/kubeconfig"
k() { kubectl --context "kind-${KIND_CLUSTER}" "$@"; }
rt() { k -n cni-test exec "${OVN_POD}" -- "$@"; }
ag() { k -n cni-test exec "${AGENT_POD}" -- "$@"; }
copy_calls() {
  [[ -n "${WORKER:-}" ]] || return 0
  mkdir -p "$1"
  docker exec "${WORKER}" tar -cf - -C /run/evpn-cni-evidence . | tar -xf - -C "$1"
}
cluster_nodes() {
  docker ps -a --no-trunc --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER}" \
    --format '{{.ID}} {{.Names}} {{.Label "io.x-k8s.kind.cluster"}}' | sort
}
cleanup_cluster() {
  local current
  [[ "${CLUSTER_CREATE_ATTEMPTED:-0}" == 1 ]] || return 0
  [[ -n "${KUBECONFIG:-}" ]] || { CLUSTER_CLEANUP_STATUS=FAIL; return 1; }
  e2e_lab_down || {
    CLUSTER_CLEANUP_STATUS=FAIL; return 1;
  }
  current="$(cluster_nodes)" || { CLUSTER_CLEANUP_STATUS=FAIL; return 1; }
  [[ -z "${current}" ]] || { CLUSTER_CLEANUP_STATUS=FAIL; return 1; }
  CLUSTER_CLEANUP_STATUS=PASS
}
cleanup() {
  local rc=$?
  local original_rc="${rc}"
  local cleanup_rc=0
  local capture_rc=0
  trap - EXIT
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 &&
        ( "${#INTEROP_CAPTURE_UNITS[@]}" -gt 0 || -n "${INTEROP_CAPTURE_STAGING_DIR:-}" ) ]]; then
    interop_capture_stop || capture_rc=1
  fi
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 && "${INTEROP_CAPTURE_SHUTDOWN_FAILURE:-0}" == 1 ]]; then
    cleanup_rc=1
  fi
  if [[ "${CREATED:-0}" == 1 ]]; then
    if [[ -n "${KUBECONFIG:-}" && -s "${KUBECONFIG}" ]]; then
      k get events -A --sort-by=.lastTimestamp 2>&1 | tee "${EVIDENCE_DIR}/events.txt" || true
      k -n cni-test get pods -o wide 2>&1 | tee "${EVIDENCE_DIR}/pods-final.txt" || true
    fi
    [[ -z "${AGENT_POD:-}" ]] || k -n cni-test logs "${AGENT_POD}" 2>&1 | tee "${EVIDENCE_DIR}/agent.log" || true
    copy_calls "${EVIDENCE_DIR}/cni" || true
    if [[ -n "${WORKER:-}" ]]; then
      docker cp "${WORKER}:/var/log/multus.log" "${EVIDENCE_DIR}/multus.log" 2>/dev/null || true
    fi
  fi
  if [[ "${KEEP_CLUSTER:-0}" != 1 ]]; then
    cleanup_cluster || cleanup_rc=1
    if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
      interop_cleanup || cleanup_rc=1
    fi
    test_image_cleanup || cleanup_rc=1
  elif [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
    interop_cleanup || cleanup_rc=1
  fi
  echo "cluster-cleanup=${CLUSTER_CLEANUP_STATUS:-SKIPPED}"
  echo "resource-cleanup=$([[ "${cleanup_rc}" == 0 ]] && echo PASS || echo FAIL)"
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
    echo "capture-shutdown=${INTEROP_CAPTURE_SHUTDOWN_STATUS}"
    echo "packet-verification=${INTEROP_PACKET_VERIFICATION_STATUS}"
    echo "lifecycle=${INTEROP_LIFECYCLE_STATUS}"
    echo "shared-vtep-lifecycle=${INTEROP_SHARED_VTEP_STATUS}"
    if [[ "${INTEROP_PACKET_VERIFICATION_FAILURE}" == 1 ]]; then capture_rc=1; fi
  fi
  if (( rc == 0 && capture_rc != 0 )); then rc="${capture_rc}"; fi
  if (( rc == 0 && cleanup_rc != 0 )); then rc="${cleanup_rc}"; fi
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
    if ! interop_write_status "${rc}" "${cleanup_rc}" "${original_rc}"; then
      echo "failed to write interop status" >&2
      if (( rc == 0 )); then rc=1; fi
    fi
  fi
  exit "${rc}"
}
trap cleanup EXIT
validate_cluster_name || exit 1
printf 'run=%s\nkind-cluster=%s\nevidence-directory=%s\nstarted-at=%s\n' \
  "${KIND_CLUSTER}" "${KIND_CLUSTER}" "${EVIDENCE_DIR}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  > "${EVIDENCE_DIR}/run-metadata.txt"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
  interop_capture_preflight || {
    INTEROP_FAILURE_CLASSIFICATION="HARNESS FAILURE"
    INTEROP_FAILURE_GATE="CAPTURE PREFLIGHT"
    echo "capture output preflight failed; refusing acceptance setup" >&2
    exit 1
  }
fi
if [[ "${EVPN_AGENT_IMAGE_MODE}" == pinned ]]; then
  test_image_pinned_preflight > "${EVIDENCE_DIR}/agent-preflight.txt"
  cat "${EVIDENCE_DIR}/agent-preflight.txt"
else
  test_image_snapshot
  test_image_require_new_tag "${AGENT_IMAGE}"
fi
kind get clusters | sort | tee "${EVIDENCE_DIR}/clusters-before.txt"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
  docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort > "${EVIDENCE_DIR}/docker-containers-before.txt"
  docker network ls --format '{{.ID}} {{.Name}} {{.Driver}}' | sort > "${EVIDENCE_DIR}/docker-networks-before.txt"
fi
if kind get clusters | grep -qx "${KIND_CLUSTER}"; then
  echo "refusing existing cluster" >&2; exit 1
fi
CLUSTER_NODES_BEFORE="$(cluster_nodes)"
[[ -z "${CLUSTER_NODES_BEFORE}" ]] || { echo "refusing pre-existing kind node containers" >&2; exit 1; }
[[ "$(docker image inspect "${OVN_IMAGE}" --format '{{.Id}}')" == "${OVN_ID}" ]]
if [[ "${EVPN_AGENT_IMAGE_MODE}" == build ]]; then
  test_image_ensure_base
fi
if [[ ! -f "${EVIDENCE_DIR}/multus.tar.gz" ]]; then
  curl -fsSL --retry 2 "https://github.com/k8snetworkplumbingwg/multus-cni/releases/download/v4.3.1/multus-cni_4.3.1_linux_amd64.tar.gz" -o "${EVIDENCE_DIR}/multus.tar.gz"
fi
echo "${MULTUS_SHA}  ${EVIDENCE_DIR}/multus.tar.gz" | sha256sum -c -
tar -xzf "${EVIDENCE_DIR}/multus.tar.gz" -C "${EVIDENCE_DIR}" multus-cni_4.3.1_linux_amd64/multus
if [[ "${EVPN_AGENT_IMAGE_MODE}" == build ]]; then
(
  cd "${ROOT}"
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-node-agent" ./cmd/evpn-node-agent
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-cni" ./cmd/evpn-cni
  CGO_ENABLED=0 go test -c -o "${EVIDENCE_DIR}/build/nodeagent.test" ./pkg/nodeagent
  if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then
    CGO_ENABLED=0 go test -c -o "${EVIDENCE_DIR}/build/evpn.test" ./pkg/evpn
  fi
)
# A local image ID has no registry manifest digest. Verify it above, then
# build from its task-owned local tag without trying to pull that ID.
test_image_require_new_tag "${AGENT_IMAGE}"
docker build --quiet -t "${AGENT_IMAGE}" -f "${ROOT}/deploy/kind/cni-multus-test/Dockerfile" "${EVIDENCE_DIR}/build"
test_image_record_created "${AGENT_IMAGE}"
fi
docker image inspect "${AGENT_IMAGE}" --format '{{.Id}}' | tee "${EVIDENCE_DIR}/agent-image.txt"
CLUSTER_CREATE_ATTEMPTED=1
CLUSTER_CREATION_STATUS=FAIL
if e2e_lab_up cni; then
  CREATED=1
  CLUSTER_CREATION_STATUS=PASS
else
  exit "$?"
fi
CLUSTER_OWNED_NODES="$(cluster_nodes)"
if ! python3 -c 'import re,sys
rows=[line.split() for line in sys.stdin if line.strip()]
valid=len(rows)==2 and all(len(r)==3 and re.fullmatch("[a-f0-9]{64}",r[0]) and r[2]==sys.argv[1] for r in rows)
if not valid or {r[1] for r in rows}!={sys.argv[1]+"-worker",sys.argv[1]+"-control-plane"}:
    sys.exit("unexpected kind node identity or membership")
' "${KIND_CLUSTER}" <<<"${CLUSTER_OWNED_NODES}"; then
  CLUSTER_OWNED_NODES=""
  echo "created cluster nodes require ownership review" >&2
  exit 1
fi
printf '%s\n' "${CLUSTER_OWNED_NODES}" > "${EVIDENCE_DIR}/owned-kind-nodes.txt"
WORKER="${KIND_CLUSTER}-worker"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then interop_setup_underlay; fi
k wait --for=condition=Ready nodes --all --timeout=180s
kind load docker-image "${OVN_IMAGE}" "${AGENT_IMAGE}" --name "${KIND_CLUSTER}"
k create namespace cni-test
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then
  source "${ROOT}/tools/lib/cni-evpn-export-test.sh"
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
    interop_setup_provider
  else
    export_setup_provider
    if [[ "${EVPN_IMPORT_TEST:-0}" == 1 ]]; then
      source "${ROOT}/tools/lib/cni-evpn-import-test.sh"
    fi
    if [[ "${EVPN_CONTINUITY_TEST:-0}" == 1 ]]; then
      source "${ROOT}/tools/lib/cni-evpn-continuity-test.sh"
      continuity_setup_provider
    fi
  fi
fi
cat <<EOF | tee "${EVIDENCE_DIR}/runtime-pods.yaml" | k -n cni-test apply -f -
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ovn-runtime
spec:
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
        securityContext: {privileged: true}
        env:
        - name: OVN_EVPN_LOCAL_IP
          valueFrom:
            fieldRef: {fieldPath: status.hostIP}
        - name: OVN_EVPN_VXLAN_PORTS
          value: "4789"
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
      volumes:
      - name: ovs
        hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}
      - name: ovn
        hostPath: {path: /run/ovn, type: DirectoryOrCreate}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-agent
spec:
  selector:
    matchLabels: {app: node-agent}
  template:
    metadata:
      labels: {app: node-agent}
    spec:
      hostNetwork: true
${EXPORT_HOST_PID:-}
      dnsPolicy: ClusterFirstWithHostNet
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      containers:
      - name: agent
        image: ${AGENT_IMAGE}
        imagePullPolicy: Never
        securityContext: {privileged: true, runAsUser: 0}
        command:
        - /bin/sh
        - -c
        - |
          for i in \$(seq 1 90); do
            test -S /var/run/ovn/ovnnb_db.sock && test -S /var/run/openvswitch/db.sock && break
            sleep 1
          done
          ${EXPORT_WAIT:-:}
          trap 'kill -TERM "\$pid"; wait "\$pid"; exit' TERM INT
          while :; do
            /usr/local/bin/evpn-node-agent --socket=/run/evpn-node-agent/agent.sock --mode=real-ovn-ovs --network-backend=linux-veth --ovn-nb-endpoint=unix:/var/run/ovn/ovnnb_db.sock --ovsdb-endpoint=unix:/var/run/openvswitch/db.sock ${EXPORT_FLAGS} ${REMOTE_FLAGS} &
            pid=\$!
            echo "\$pid" > /run/evpn-node-agent/agent.pid
            wait "\$pid"
            sleep 1
          done
        volumeMounts:
        - {name: ovs, mountPath: /var/run/openvswitch}
        - {name: ovn, mountPath: /var/run/ovn}
        - {name: agent, mountPath: /run/evpn-node-agent}
        - {name: netns, mountPath: /run/netns, mountPropagation: HostToContainer}
${EXPORT_MOUNTS}
      volumes:
      - name: ovs
        hostPath: {path: /run/openvswitch, type: DirectoryOrCreate}
      - name: ovn
        hostPath: {path: /run/ovn, type: DirectoryOrCreate}
      - name: agent
        hostPath: {path: /run/evpn-node-agent, type: DirectoryOrCreate}
      - name: netns
        hostPath: {path: /run/netns, type: DirectoryOrCreate}
${EXPORT_VOLUMES}
EOF
k -n cni-test rollout status ds/ovn-runtime --timeout=240s
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then interop_setup_config; else export_setup_config; fi
  if [[ "${EVPN_IMPORT_TEST:-0}" == 1 && "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then import_setup_config; fi
fi
k -n cni-test rollout status ds/node-agent --timeout=240s
OVN_POD="$(k -n cni-test get pod -l app=ovn-runtime -o jsonpath='{.items[0].metadata.name}')"
AGENT_POD="$(k -n cni-test get pod -l app=node-agent -o jsonpath='{.items[0].metadata.name}')"
for i in $(seq 1 60); do ag nc -z -U /run/evpn-node-agent/agent.sock && break; sleep 1; done
ag nc -z -U /run/evpn-node-agent/agent.sock
if [[ "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then rt sh -ec 'ovn-nbctl ls-add ls24b'; fi
rt sh -ec 'ovn-nbctl --version; ovs-vsctl --version; test "$(ovsdb-client get-schema-version unix:/var/run/ovn/ovnnb_db.sock OVN_Northbound)" = 7.19.0; test "$(ovsdb-client get-schema-version unix:/var/run/ovn/ovnsb_db.sock OVN_Southbound)" = 21.13.0; ovs-vsctl show; ovn-sbctl show' | tee "${EVIDENCE_DIR}/runtime.txt"
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
  grep -Fq '26.09.0' "${EVIDENCE_DIR}/runtime.txt"
  grep -Fq '4.0.0' "${EVIDENCE_DIR}/runtime.txt"
  k -n cni-test get pod -l app=ovn-runtime -o json \
    | tee "${EVIDENCE_DIR}/runtime-image.json" >/dev/null
  python3 - "${EVIDENCE_DIR}/runtime-image.json" "${OVN_ID}" <<'PY'
import json,sys
pods=json.load(open(sys.argv[1])).get("items",[])
assert len(pods)==1,pods
statuses=pods[0].get("status",{}).get("containerStatuses",[])
assert len(statuses)==1,statuses
image_id=statuses[0].get("imageID","").removeprefix("docker://")
assert image_id==sys.argv[2],(image_id,sys.argv[2])
print("running OVN image ID="+image_id)
PY
fi
k version -o yaml | tee "${EVIDENCE_DIR}/kubernetes-version.yaml"
ag env EVPN_NODEAGENT_RUN_PRIV_TESTS=1 /usr/local/bin/nodeagent.test -test.run '^TestLinuxVethBackend.*_Privileged$' -test.v | tee "${EVIDENCE_DIR}/privileged-tests.txt"

# Install thin Multus directly on the disposable worker. There is no daemon
# or shim: containerd invokes the official standalone executable.
cat "${ROOT}/deploy/kind/cni-multus-test/rbac.yaml" | k apply -f -
k wait --for=condition=Established crd/network-attachment-definitions.k8s.cni.cncf.io --timeout=60s
docker exec "${WORKER}" sh -ec 'test ! -e /opt/cni/bin/evpn-cni; test ! -L /opt/cni/bin/evpn-cni; test ! -e /opt/cni/bin/evpn-cni.real; test ! -L /opt/cni/bin/evpn-cni.real; test ! -e /opt/cni/bin/multus; mkdir -p /etc/cni/net.d/multus.d /run/evpn-cni-evidence'
if [[ "${EVPN_AGENT_IMAGE_MODE}" == pinned ]]; then
  stage_pinned_cni_binary "${EVIDENCE_DIR}/build/evpn-cni" \
    /usr/local/bin/evpn-cni 15242639 \
    39a499c490738a3c1d26a82623e7964bc790f30952eab3f6491ab6e8c9afad98
fi
CNI_SIZE="$(stat -c %s -- "${EVIDENCE_DIR}/build/evpn-cni")"
CNI_SHA="$(sha256sum -- "${EVIDENCE_DIR}/build/evpn-cni")"
CNI_SHA="${CNI_SHA%% *}"
if [[ "${EVPN_AGENT_IMAGE_MODE}" == pinned ]]; then
  CNI_SIZE=15242639
  CNI_SHA=39a499c490738a3c1d26a82623e7964bc790f30952eab3f6491ab6e8c9afad98
fi
stage_worker_cni_file "${WORKER}" "${EVIDENCE_DIR}/build/evpn-cni" \
  /opt/cni/bin/evpn-cni.real "${CNI_SIZE}" "${CNI_SHA}" real \
  | tee "${EVIDENCE_DIR}/worker-cni-integrity.txt"
docker cp "${EVIDENCE_DIR}/multus-cni_4.3.1_linux_amd64/multus" "${WORKER}:/opt/cni/bin/multus"
cat > "${EVIDENCE_DIR}/cni-wrapper" <<'EOF'
#!/bin/sh
dir=$(mktemp -d /run/evpn-cni-evidence/call.XXXXXXXX)
env | grep '^CNI_' > "$dir/env"
cat > "$dir/config"
/opt/cni/bin/evpn-cni.real < "$dir/config" > "$dir/stdout" 2> "$dir/stderr"
rc=$?
echo "$rc" > "$dir/rc"
cat "$dir/stdout"
cat "$dir/stderr" >&2
exit "$rc"
EOF
WRAPPER_SHA="$(sha256sum -- "${EVIDENCE_DIR}/cni-wrapper")"
stage_worker_cni_file "${WORKER}" "${EVIDENCE_DIR}/cni-wrapper" \
  /opt/cni/bin/evpn-cni "$(stat -c %s -- "${EVIDENCE_DIR}/cni-wrapper")" "${WRAPPER_SHA%% *}" wrapper \
  | tee "${EVIDENCE_DIR}/worker-cni-wrapper-integrity.txt"
docker exec "${WORKER}" chmod 755 /opt/cni/bin/multus
verify_worker_cni_version "${WORKER}" "${EVIDENCE_DIR}" "${CNI_SIZE}" "${CNI_SHA}"
docker exec "${WORKER}" /opt/cni/bin/multus --version 2>&1 | tee "${EVIDENCE_DIR}/multus-version.txt"
TOKEN="$(k -n kube-system create token multus-test --duration=2h)"
CA="$(k config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
APISERVER="$(docker exec "${WORKER}" awk '/server:/{print $2; exit}' /etc/kubernetes/kubelet.conf)"
cat <<EOF | docker exec -i "${WORKER}" sh -c 'umask 077; cat > /etc/cni/net.d/multus.d/kubeconfig'
apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: ${APISERVER}
    certificate-authority-data: ${CA}
users:
- name: multus
  user:
    token: ${TOKEN}
contexts:
- name: test
  context: {cluster: test, user: multus}
current-context: test
EOF
unset TOKEN CA
docker exec "${WORKER}" sh -ec 'cp /etc/cni/net.d/10-kindnet.conflist /run/kindnet-before.json'
docker exec "${WORKER}" cat /etc/cni/net.d/10-kindnet.conflist | python3 -c '
import json,sys
primary=json.load(sys.stdin)
print(json.dumps({"cniVersion":"1.0.0","name":"multus-test","type":"multus","kubeconfig":"/etc/cni/net.d/multus.d/kubeconfig","logLevel":"debug","logFile":"/var/log/multus.log","logToStderr":True,"retryDeleteOnError":True,"delegates":[primary]}))
' | docker exec -i "${WORKER}" sh -c 'cat > /etc/cni/net.d/00-multus.conf'
docker exec "${WORKER}" cmp /run/kindnet-before.json /etc/cni/net.d/10-kindnet.conflist
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 &&
      ( "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ||
        "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ) ]]; then
  attachment_names=(a)
else
  attachment_names=(a b)
fi
for which in "${attachment_names[@]}"; do
  ip=10; [[ "${which}" == a ]] || ip=11
  mac=0a; [[ "${which}" == a ]] || mac=0b
  cat <<EOF | tee "${EVIDENCE_DIR}/nad-${which}.yaml" | k -n cni-test apply -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata: {name: evpn-${which}}
spec:
  config: '{"cniVersion":"1.0.0","name":"evpn-${which}","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"n24","attachmentID":"s","logicalSwitch":"ls24b","ipAddress":"10.244.240.${ip}/24","macAddress":"0a:58:f0:00:00:${mac}"}'
EOF
done
create_pod() {
  local which="$1"
  cat <<EOF | tee "${EVIDENCE_DIR}/pod-${which}.yaml" | k -n cni-test apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: pod-${which}
  annotations: {k8s.v1.cni.cncf.io/networks: evpn-${which}}
spec:
  nodeName: ${WORKER}
  containers:
  - name: client
    image: ${AGENT_IMAGE}
    imagePullPolicy: Never
    command: ["/bin/sh", "-c", "sleep infinity"]
EOF
}
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 && "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then export_before_pods; fi
if [[ "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ]]; then
  interop_diagnostic_preflight
fi
create_pod a
if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 &&
      ( "${EVPN_FRR_INTEROP_INITIAL_ONLY}" == 1 ||
        "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ) ]]; then
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
else
  create_pod b
  k -n cni-test wait --for=condition=Ready pod/pod-a pod/pod-b --timeout=180s
fi
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then
  if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then
    if [[ "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ]]; then
      interop_binding_diagnostic
      exit 0
    fi
    interop_verify
    echo "CNI-MVP-3.3b.3 FRR interoperability scenario=PASS"
    exit 0
  fi
  export_verify_dry_and_enable
  if [[ "${EVPN_IMPORT_TEST:-0}" == 1 && "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then import_verify; fi
fi

snapshot() {
  local phase="$1"
  k -n cni-test get pods -o json | tee "${EVIDENCE_DIR}/${phase}-pods.json"
  rt sh -ec 'ovs-vsctl --columns=name,external_ids,ofport list Interface; ovs-vsctl --columns=name,external_ids list Port; ovn-nbctl list Logical_Switch_Port; ovn-sbctl --columns=logical_port,chassis,up list Port_Binding' | tee "${EVIDENCE_DIR}/${phase}-ovs-ovn.txt"
  ag sh -ec 'ip -d link show; cat /proc/self/mountinfo | grep netns' | tee "${EVIDENCE_DIR}/${phase}-linux.txt"
}
verify_ownership() {
  rt ovs-vsctl --format=json --columns=_uuid,name,external_ids,ofport list Interface | tee "${EVIDENCE_DIR}/interfaces.json" >/dev/null
  rt ovs-vsctl --format=json --columns=_uuid,name,interfaces list Port | tee "${EVIDENCE_DIR}/ports.json" >/dev/null
  rt ovs-vsctl --format=json --columns=name,ports list Bridge | tee "${EVIDENCE_DIR}/bridges.json" >/dev/null
  rt ovn-nbctl --format=json --columns=name,external_ids,addresses list Logical_Switch_Port | tee "${EVIDENCE_DIR}/lsps.json" >/dev/null
  rt ovn-sbctl --format=json --columns=logical_port,chassis,up list Port_Binding | tee "${EVIDENCE_DIR}/bindings.json" >/dev/null
  ag ip -j -d link show | tee "${EVIDENCE_DIR}/host-links.json" >/dev/null
  k -n cni-test get pods -o json | tee "${EVIDENCE_DIR}/verify-pods.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1])
def value(v):
    if isinstance(v,list) and len(v)==2 and v[0] in ("map","set","uuid"):
        if v[0]=="map": return dict(v[1])
        if v[0]=="set": return [value(x) for x in v[1]]
        return v[1]
    return v
def rows(name):
    doc=json.loads((root/(name+".json")).read_text())
    return [dict(zip(doc["headings"],map(value,row))) for row in doc["data"]]
def members(v): return v if isinstance(v,list) else [v]
interfaces,ports,bridges,lsps,bindings=map(rows,("interfaces","ports","bridges","lsps","bindings"))
links=json.loads((root/"host-links.json").read_text())
pods=json.loads((root/"verify-pods.json").read_text())["items"]
for suffix,ip,mac in (("a","10.244.240.10","0a:58:f0:00:00:0a"),("b","10.244.240.11","0a:58:f0:00:00:0b")):
    pod=next(p for p in pods if p["metadata"]["name"]=="pod-"+suffix)
    uid=pod["metadata"]["uid"]
    status=json.loads(pod["metadata"]["annotations"]["k8s.v1.cni.cncf.io/network-status"])
    secondary=[s for s in status if s.get("interface")=="net1"]
    assert len(secondary)==1 and secondary[0]["ips"]==[ip] and secondary[0]["mac"]==mac,secondary
    interface=[i for i in interfaces if i["external_ids"].get("evpn.framework/workload-uid")==uid]
    assert len(interface)==1,interface
    interface=interface[0]; ids=interface["external_ids"]
    assert ids["evpn.framework/managed-by"]=="evpn-node-agent" and ids["evpn.framework/network-id"]=="n24"
    assert ids["evpn.framework/endpoint-id"]=="n24|s|"+uid and ids["evpn.framework/incarnation"]
    assert ids["evpn.framework/attachment-id"]=="s" and interface["ofport"]>0
    port=next(p for p in ports if p["name"]==interface["name"])
    assert members(port["interfaces"])==[interface["_uuid"]]
    assert port["_uuid"] in members(next(b for b in bridges if b["name"]=="br-int")["ports"])
    lsp=next(p for p in lsps if p["name"]==ids["iface-id"])
    for key in ("workload-uid","endpoint-id","incarnation","managed-by","network-id","attachment-id"):
        assert lsp["external_ids"]["evpn.framework/"+key]==ids["evpn.framework/"+key],(key,lsp,ids)
    assert any(mac in a and ip in a for a in members(lsp["addresses"])),lsp
    binding=next(b for b in bindings if b["logical_port"]==lsp["name"])
    assert binding["chassis"] and binding["up"] is True,binding
    link=next(l for l in links if l["ifname"]==interface["name"])
    assert link["linkinfo"]["info_kind"]=="veth" and "UP" in link["flags"]
    assert link["ifalias"]=="evpn-node-agent|n24|s|n24|s|"+uid,link
    print("ownership-and-Port_Binding=PASS pod="+suffix+" uid="+uid+" host="+interface["name"]+" lsp="+lsp["name"]+" chassis="+str(binding["chassis"]))
PY
}
verify_pods() {
  for which in a b; do
    k -n cni-test exec "pod-${which}" -- ip -d addr | tee "${EVIDENCE_DIR}/pod-${which}-links.txt"
    k -n cni-test exec "pod-${which}" -- ip route get 10.96.0.1
    k -n cni-test exec "pod-${which}" -- nc -z -w 3 10.96.0.1 443
  done
  local primary_a primary_b
  primary_a="$(k -n cni-test get pod pod-a -o jsonpath='{.status.podIP}')"
  primary_b="$(k -n cni-test get pod pod-b -o jsonpath='{.status.podIP}')"
  k -n cni-test exec pod-a -- ping -I eth0 -c 2 -W 2 "${primary_b}"
  k -n cni-test exec pod-b -- ping -I eth0 -c 2 -W 2 "${primary_a}"
  k -n cni-test exec pod-a -- ping -I net1 -c 3 -W 2 10.244.240.11
  k -n cni-test exec pod-b -- ping -I net1 -c 3 -W 2 10.244.240.10
  k -n cni-test exec pod-a -- ip neigh show dev net1
  k -n cni-test exec pod-b -- ip neigh show dev net1
}
verify_pods | tee "${EVIDENCE_DIR}/connectivity.txt"
snapshot initial
verify_ownership | tee "${EVIDENCE_DIR}/initial-ownership.txt"

# Captured delegate inputs are runtime-generated, not invented identities.
copy_calls "${EVIDENCE_DIR}/initial-cni"
python3 - "${EVIDENCE_DIR}/initial-cni" <<'PY'
import json,pathlib,sys
calls=[]
for p in pathlib.Path(sys.argv[1]).glob("call.*"):
    env=dict(line.split("=",1) for line in (p/"env").read_text().splitlines())
    if env["CNI_COMMAND"]=="ADD" and (p/"rc").read_text().strip()=="0":
        assert env["CNI_NETNS"].startswith("/var/run/netns/") or env["CNI_NETNS"].startswith("/run/netns/"),env
        assert "K8S_POD_UID=" in env["CNI_ARGS"],env
        result=json.loads((p/"stdout").read_text())
        assert result["cniVersion"]=="1.0.0" and result["interfaces"][0]["name"]=="net1",result
        calls.append(p)
assert len(calls)==2,calls
print("runtime-generated-ADD-contract=PASS")
PY
find_call() {
  docker exec "${WORKER}" sh -ec "for d in /run/evpn-cni-evidence/call.*; do grep -q '^CNI_COMMAND=ADD$' \"\$d/env\" && grep -q 'K8S_POD_UID=${1}' \"\$d/env\" && test \"\$(cat \"\$d/rc\")\" = 0 && { echo \"\$d\"; exit; }; done; exit 1"
}
OLD_UID="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
CALL_A="$(find_call "${OLD_UID}")"
UID_B="$(k -n cni-test get pod pod-b -o jsonpath='{.metadata.uid}')"
CALL_B="$(find_call "${UID_B}")"
NETNS="$(docker exec "${WORKER}" sed -n 's/^CNI_NETNS=//p' "${CALL_A}/env")"
HOST_NS="$(docker exec "${WORKER}" stat -Lc %i "${NETNS}")"
[[ "$(ag stat -Lc %i "${NETNS}")" == "${HOST_NS}" ]]
echo "namespace-visibility=PASS netns=${NETNS} inode=${HOST_NS}"

replay() {
  local call="$1" command="$2" empty="${3:-0}" stale="${4:-0}"
  docker exec "${WORKER}" sh -ec '
    while IFS= read -r line; do export "$line"; done < "$1/env"
    export CNI_COMMAND="$2"
    if [ "$3" = 1 ]; then export CNI_NETNS=""; fi
    if [ "$4" = 1 ]; then export CNI_ARGS="${CNI_ARGS};INCARNATION=stale-test-incarnation"; fi
    exec /opt/cni/bin/evpn-cni < "$1/config"
  ' sh "${call}" "${command}" "${empty}" "${stale}"
}
# These are supplemental delegate checks, NOT proof of automatic CRI CHECK.
replay "${CALL_A}" CHECK
replay "${CALL_B}" CHECK
echo "automatic-containerd-CHECK=NOT-TESTED (CRI sandbox lifecycle has no automatic CHECK)"
if [[ "${EVPN_EXPORT_TEST:-0}" != 1 ]]; then
BEFORE="$(ag sh -ec 'for p in /sys/class/net/evp*/ifindex; do echo "$p=$(cat "$p")"; done')"
PID="$(ag cat /run/evpn-node-agent/agent.pid)"
CONTAINER="$(k -n cni-test get pod "${AGENT_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}')"
ag kill -TERM "${PID}"
for i in $(seq 1 60); do
  if [[ "$(ag cat /run/evpn-node-agent/agent.pid)" != "${PID}" ]] && ag nc -z -U /run/evpn-node-agent/agent.sock; then break; fi
  sleep 1
done
[[ "$(ag cat /run/evpn-node-agent/agent.pid)" != "${PID}" ]]
[[ "$(k -n cni-test get pod "${AGENT_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}')" == "${CONTAINER}" ]]
[[ "$(ag sh -ec 'for p in /sys/class/net/evp*/ifindex; do echo "$p=$(cat "$p")"; done')" == "${BEFORE}" ]]
echo "process-only-restart=PASS old-pid=${PID} new-pid=$(ag cat /run/evpn-node-agent/agent.pid) container=${CONTAINER}" | tee "${EVIDENCE_DIR}/restart.txt"
replay "${CALL_A}" CHECK
replay "${CALL_B}" CHECK
verify_pods | tee "${EVIDENCE_DIR}/after-restart-connectivity.txt"
snapshot after-restart
fi

# Stop only this disposable worker's kubelet so the API object is gone before
# the runtime receives DEL, then resume normal kubelet/containerd cleanup.
docker exec "${WORKER}" systemctl stop kubelet
k -n cni-test delete pod pod-a --grace-period=0 --force --wait=true
[[ "$(k -n cni-test get pod pod-a --ignore-not-found -o name)" == "" ]]
docker exec "${WORKER}" systemctl start kubelet
wait_absent() {
  local uid="$1"
  local interfaces lsps links
  for i in $(seq 1 90); do
    if interfaces="$(rt ovs-vsctl --bare --columns=name find Interface "external_ids:evpn.framework/workload-uid=${uid}")" &&
       lsps="$(rt ovn-nbctl --bare --columns=name find Logical_Switch_Port "external_ids:evpn.framework/workload-uid=${uid}")" &&
       links="$(ag sh -ec 'for a in /sys/class/net/*/ifalias; do if [ "$(cat "$a")" = "evpn-node-agent|n24|s|n24|s|$1" ]; then echo "$a"; fi; done' sh "${uid}")" &&
       [[ -z "${interfaces}" && -z "${lsps}" && -z "${links}" ]]; then
      echo "runtime-owned-resource-cleanup=PASS uid=${uid}"
      return
    fi
    sleep 1
  done
  echo "runtime DEL cleanup timeout for ${uid}" >&2; return 1
}
wait_absent "${OLD_UID}"
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then export_check 1 1 runtime-del; fi
docker exec "${WORKER}" sh -ec "for d in /run/evpn-cni-evidence/call.*; do if grep -q '^CNI_COMMAND=DEL$' \"\$d/env\" && grep -q 'K8S_POD_UID=${OLD_UID}' \"\$d/env\"; then test \"\$(cat \"\$d/rc\")\" = 0 && { cat \"\$d/env\"; exit; }; fi; done; exit 1" | tee "${EVIDENCE_DIR}/runtime-del-absent-pod.txt"
replay "${CALL_A}" DEL 1
echo "API-absent-runtime-DEL-and-empty-netns-replay=PASS"
create_pod a
k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
NEW_UID="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
[[ "${NEW_UID}" != "${OLD_UID}" ]]
NEW_CALL="$(find_call "${NEW_UID}")"
replay "${CALL_A}" DEL 1
replay "${NEW_CALL}" DEL 1 1
replay "${NEW_CALL}" CHECK
verify_pods | tee "${EVIDENCE_DIR}/recreated-connectivity.txt"
snapshot recreated
verify_ownership | tee "${EVIDENCE_DIR}/recreated-ownership.txt"
echo "Pod-recreation-and-stale-DEL=PASS old=${OLD_UID} new=${NEW_UID}"
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then export_check 2 2 recreated-and-stale-del; export_failures; fi
k -n cni-test delete pod pod-a pod-b --wait=true --timeout=120s
wait_absent "${NEW_UID}"
wait_absent "${UID_B}"
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then export_check 0 0 all-runtime-del; fi
replay "${NEW_CALL}" DEL 1
replay "${CALL_B}" DEL 1
rt sh -ec 'test -z "$(ovs-vsctl --bare --columns=name find Interface external_ids:evpn.framework/managed-by=evpn-node-agent)"; if ovs-vsctl --bare --columns=name list Port | grep "^evp"; then exit 1; fi; test -z "$(ovn-nbctl --bare --columns=name list Logical_Switch_Port)"; ovn-nbctl --wait=sb sync; test -z "$(ovn-sbctl --bare --columns=logical_port list Port_Binding)"'
ag sh -ec 'for a in /sys/class/net/*/ifalias; do case "$(cat "$a")" in evpn-node-agent\|*) echo "remaining owned veth: $a"; exit 1;; esac; done'
docker exec "${WORKER}" cmp /run/kindnet-before.json /etc/cni/net.d/10-kindnet.conflist
snapshot cleaned
if [[ "${EVPN_EXPORT_TEST:-0}" == 1 ]]; then export_finish; fi
echo "CNI-MVP-2.4b executed scenarios=PASS"
