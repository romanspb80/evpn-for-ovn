#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
KIND_CLUSTER="evpn-op-type2-${RUN_ID,,}"
WORKER="${KIND_CLUSTER}-worker"
EVIDENCE_DIR="${EVPN_OPERATOR_TYPE2_EVIDENCE_ROOT:-/tmp/evpn-operator-type2-e2e}/attempt-${RUN_ID}"
KUBECONFIG="${EVIDENCE_DIR}/kubeconfig"
AGENT_IMAGE="evpn4ovn/operator-type2:${RUN_ID,,}"
OVN_IMAGE="evpn4ovn/ovn-ovs:kind-26.09"
OVN_ID=sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505
BASE_ID=sha256:f01f29f4d8b1fd4f377103015aa7b7dab6f8189416ce2a363f63d7e538c4e5a7
EVPN_FRR_INTEROP_TEST=1
EVPN_FRR_INTEROP_FULL_LIFECYCLE=1
CNI_CREATION_IDENTITY=true
OVN_RUNTIME_LABEL=app=ovn-ovs
ACTIVE_CASE=setup
OPERATOR_INSTALLED=0
CNI_INSTALLED=0
PROVIDER_PIN_CONTAINER=""
export ROOT KIND_CLUSTER WORKER EVIDENCE_DIR KUBECONFIG EVPN_FRR_INTEROP_TEST
source "${ROOT}/tools/lib/containerlab-e2e.sh"
source "${ROOT}/tools/lib/cni-agent-image-test.sh"
source "${ROOT}/tools/lib/cni-evpn-export-test.sh"
source "${ROOT}/tools/lib/cni-evpn-frr-interop-test.sh"
source "${ROOT}/tools/lib/operator-cni-test.sh"
e2e_lab_acquire
[[ ! -e "${EVIDENCE_DIR}" ]]
mkdir -p "${EVIDENCE_DIR}/build"
chmod 700 "${EVIDENCE_DIR}"
: >"${EVIDENCE_DIR}/cases.tsv"
exec > >(tee "${EVIDENCE_DIR}/run.log") 2>&1
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "kind-${KIND_CLUSTER}" "$@"; }
rt() { k -n cni-test exec "${OVN_POD}" -c ovn-ovs -- "$@"; }
ag() { k -n cni-test exec "${AGENT_POD}" -c agent -- "$@"; }
case_status() {
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${3:-}" >>"${EVIDENCE_DIR}/cases.tsv"
  printf 'acceptance-case=%s status=%s %s\n' "$1" "$2" "${3:-}"
}
run_case() {
  ACTIVE_CASE="$1"
  shift
  "$@"
  case_status "${ACTIVE_CASE}" PASS
}
cleanup() {
  local rc=$? cleanup_rc=0 result_rc=0
  trap - EXIT ERR
  if (( rc != 0 )); then case_status "${ACTIVE_CASE}" FAIL "runner exited ${rc}"; fi
  if [[ -n "${PROVIDER_PIN_CONTAINER}" ]]; then
    docker rm "${PROVIDER_PIN_CONTAINER}" >/dev/null || cleanup_rc=1
  fi
  if [[ -f "${EVIDENCE_DIR}/lab.json" ]]; then
    if (( ${#INTEROP_CAPTURE_UNITS[@]} > 0 )) || [[ -n "${INTEROP_CAPTURE_STAGING_DIR}" ]]; then
      interop_capture_stop || cleanup_rc=1
    fi
    if [[ "${OPERATOR_INSTALLED}" == 1 ]]; then
      k -n evpn-operator-system logs deployment/evpn-core-operator | tee "${EVIDENCE_DIR}/operator-final.log" >/dev/null || cleanup_rc=1
      k get evpncores.evpn.romanspb80.github.io -A -o yaml | tee "${EVIDENCE_DIR}/cores-final.yaml" >/dev/null || cleanup_rc=1
      k get events -A -o yaml | tee "${EVIDENCE_DIR}/events-final.yaml" >/dev/null || cleanup_rc=1
      if [[ -n "${AGENT_POD:-}" ]]; then
        docker exec "${WORKER}" cat /run/evpn-node-agent/export-status.json |
          tee "${EVIDENCE_DIR}/export-final.json" >/dev/null || cleanup_rc=1
        k -n cni-test get pod "${AGENT_POD}" -o yaml | tee "${EVIDENCE_DIR}/agent-final-pod.yaml" >/dev/null || cleanup_rc=1
        if ! k -n cni-test logs "${AGENT_POD}" -c agent | tee "${EVIDENCE_DIR}/agent-final.log" >/dev/null; then
          echo "agent logs unavailable; possibly unstarted replacement: see agent-final-pod.yaml" |
            tee "${EVIDENCE_DIR}/agent-final-log-unavailable.txt"
        fi
      fi
      if [[ -n "${PROVIDER_POD:-}" ]]; then
        provider_exec gobgp global rib -a evpn -j | tee "${EVIDENCE_DIR}/rib-final.json" >/dev/null || cleanup_rc=1
        k -n k8gobgp-system logs "${PROVIDER_POD}" | tee "${EVIDENCE_DIR}/provider-final.log" >/dev/null || cleanup_rc=1
      fi
      if [[ "${CNI_INSTALLED}" == 1 ]]; then
        copy_calls "${EVIDENCE_DIR}/final-cni" || cleanup_rc=1
      fi
    fi
    e2e_lab_down || cleanup_rc=1
  fi
  test_image_cleanup || cleanup_rc=1
  python3 - "${EVIDENCE_DIR}" "${rc}" "${cleanup_rc}" <<'PY'
import csv, json, pathlib, sys
p=pathlib.Path(sys.argv[1])
cases={name:{"status":"NOT TESTED"} for name in (
    "setup","operator_ownership","cni_lifecycle","cni_identity","type2_export","type2_import",
    "mp_bgp_exchange","dataplane_forwarding","route_withdrawal","operator_restart",
    "importer_restart","node_agent_restart","speaker_restart","bgp_session_loss",
    "nbdb_unavailability","ovsdb_unavailability","withdrawal_during_failure","cleanup")}
for row in csv.reader((p/"cases.tsv").open(), delimiter="\t"):
    name,status,time,*detail=row
    cases[name]={"status":status,"time":time,"detail":detail[0] if detail else ""}
cases["cleanup"]={"status":"PASS" if sys.argv[3]=="0" else "FAIL"}
result={"suite":"operator-managed-type2","cases":cases,"testExit":int(sys.argv[2]),
        "cleanupExit":int(sys.argv[3]),"evidenceDirectory":str(p),
        "topology":"single operator OVN chassis plus independent FRR/Linux VXLAN endpoint",
        "restartRecovery":"acknowledged UUID recovery and independently attested empty replacement RIB; ambiguous retained AddPath remains blocked"}
result["status"]=("FAIL" if result["testExit"] or result["cleanupExit"] or any(c["status"]=="FAIL" for c in cases.values())
                  else "PASS" if all(c["status"]=="PASS" for c in cases.values()) else "BLOCKED")
(p/"result.json").write_text(json.dumps(result,indent=2)+"\n")
print(json.dumps(result,indent=2))
PY
  echo "Operator Type-2 E2E: test-exit=${rc} cleanup-exit=${cleanup_rc} evidence=${EVIDENCE_DIR}"
  if (( rc != 0 )); then exit "${rc}"; fi
  if (( cleanup_rc != 0 )); then exit "${cleanup_rc}"; fi
  python3 - "${EVIDENCE_DIR}/result.json" <<'PY' || result_rc=$?
import json,sys
sys.exit(0 if json.load(open(sys.argv[1]))["status"]=="PASS" else 3)
PY
  exit "${result_rc}"
}
trap cleanup EXIT
trap 'exit $?' ERR
for command in go docker kubectl kind curl python3 sudo; do command -v "${command}" >/dev/null; done
test_image_snapshot
test_image_ensure_base
test_image_require_new_tag "${AGENT_IMAGE}"
[[ "$(docker image inspect "${OVN_IMAGE}" --format '{{.Id}}')" == "${OVN_ID}" ]]
docker image inspect evpn4ovn/app:kind >/dev/null
(
  cd "${ROOT}"
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/operator" ./deploy/operator
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-node-agent" ./cmd/evpn-node-agent
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-cni" ./cmd/evpn-cni
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/provider-grpc-proxy" ./tools/e2e/operator/grpc-proxy
  CGO_ENABLED=0 go test -c -o "${EVIDENCE_DIR}/build/nodeagent.test" ./pkg/nodeagent
  CGO_ENABLED=0 go test -c -o "${EVIDENCE_DIR}/build/evpn.test" ./pkg/evpn
)
cp "${ROOT}/deploy/operator/core-resources.yaml" "${EVIDENCE_DIR}/build/core-resources.yaml"
git -C "${ROOT}" rev-parse HEAD | tee "${EVIDENCE_DIR}/source-head.txt"
git -C "${ROOT}" status --porcelain | tee "${EVIDENCE_DIR}/source-status.txt"
sha256sum "${EVIDENCE_DIR}"/build/* >"${EVIDENCE_DIR}/build-sha256.txt"
docker build --quiet -t "${AGENT_IMAGE}" -f "${ROOT}/tools/e2e/operator/Dockerfile.type2" "${EVIDENCE_DIR}/build"
test_image_record_created "${AGENT_IMAGE}"
e2e_lab_up cni
interop_setup_underlay
k wait --for=condition=Ready nodes --all --timeout=180s
kind load docker-image --name "${KIND_CLUSTER}" "${AGENT_IMAGE}" "${OVN_IMAGE}" evpn4ovn/app:kind
k create namespace cni-test
interop_setup_provider
# Compatibility app transport only; exporter/importer bypass this proxy.
cat <<EOF | k apply -f -
apiVersion: v1
kind: Service
metadata: {name: k8gobgp, namespace: k8gobgp-system}
spec:
  selector: {app: operator-provider-grpc-proxy}
  ports: [{port: 50051, targetPort: 50054}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: operator-provider-grpc-proxy, namespace: k8gobgp-system}
spec:
  replicas: 1
  selector:
    matchLabels: {app: operator-provider-grpc-proxy}
  template:
    metadata:
      labels: {app: operator-provider-grpc-proxy}
    spec:
      nodeSelector: {kubernetes.io/hostname: "${WORKER}"}
      automountServiceAccountToken: false
      containers:
      - name: transport
        image: ${AGENT_IMAGE}
        imagePullPolicy: Never
        command: ["/usr/local/bin/provider-grpc-proxy"]
        volumeMounts: [{name: gobgp, mountPath: /var/run/gobgp, readOnly: true}]
      volumes:
      - name: gobgp
        hostPath: {path: /run/gobgp-export-test, type: DirectoryOrCreate}
EOF
sed "s|evpn4ovn/operator:kind|${AGENT_IMAGE}|" "${ROOT}/deploy/operator/manifests/operator.yaml" |
  k apply -f -
OPERATOR_INSTALLED=1
k wait --for=condition=Established crd/evpncores.evpn.romanspb80.github.io --timeout=60s
# The general test image has no entrypoint: preserve the actual operator binary.
k -n evpn-operator-system patch deployment evpn-core-operator --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"operator","command":["/usr/local/bin/evpn-core-operator"]}]}}}}'
k -n evpn-operator-system rollout status deployment/evpn-core-operator --timeout=120s
cat <<EOF | tee "${EVIDENCE_DIR}/core.yaml" | k apply -f -
apiVersion: evpn.romanspb80.github.io/v1alpha1
kind: EvpnCore
metadata: {name: type2, namespace: cni-test}
spec:
  enabled: true
  dataplaneNode: ${WORKER}
  appNamespace: cni-test
  ovnNamespace: cni-test
  ovnImage: ${OVN_IMAGE}
  nodeAgent: {enabled: false}
EOF
for i in $(seq 1 90); do
  if k -n cni-test get deployment/ovn-ovs >/dev/null 2>&1; then break; fi
  sleep 1
done
k -n cni-test rollout status deployment/ovn-ovs --timeout=180s
OVN_POD="$(k -n cni-test get pod -l app=ovn-ovs -o jsonpath='{.items[0].metadata.name}')"
interop_setup_config
# Recovery identities come from this disposable installation's actual API
# objects, never Pod names or a guessed node/installation identifier.
INSTALLATION_UID="$(k -n cni-test get evpncore/type2 -o jsonpath='{.metadata.uid}')"
NODE_UID="$(k get node "${WORKER}" -o jsonpath='{.metadata.uid}')"
DAEMONSET_UID="$(k -n k8gobgp-system get ds/k8gobgp -o jsonpath='{.metadata.uid}')"
PROVIDER_PIN_CONTAINER="$(docker create --name "evpn-provider-pin-${RUN_ID,,}" "${K8GOBGP_IMAGE}")"
docker cp "${PROVIDER_PIN_CONTAINER}:/usr/local/bin/gobgpd" "${EVIDENCE_DIR}/gobgpd-pin"
GOBGPD_SHA="$(sha256sum "${EVIDENCE_DIR}/gobgpd-pin")"
GOBGPD_SHA="${GOBGPD_SHA%% *}"
docker rm "${PROVIDER_PIN_CONTAINER}" >/dev/null
PROVIDER_PIN_CONTAINER=""
docker exec "${WORKER}" ctr version | tee "${EVIDENCE_DIR}/containerd-version.txt"
CTR_VERSION="$(awk '/Version:/ && !seen++ {print $2}' "${EVIDENCE_DIR}/containerd-version.txt")"
[[ "${CTR_VERSION}" =~ ^v2\.[0-9]+\.[0-9]+$ ]]
go mod download -json "github.com/containerd/containerd/v2@${CTR_VERSION}" |
  tee "${EVIDENCE_DIR}/ctr-source.json"
CTR_SOURCE="$(go list -m -f '{{.Dir}}' "github.com/containerd/containerd/v2@${CTR_VERSION}")"
(cd "${CTR_SOURCE}" && CGO_ENABLED=0 GOWORK=off go build -mod=readonly -o "${EVIDENCE_DIR}/build/ctr" ./cmd/ctr)
docker exec "${WORKER}" mkdir -p /etc/evpn/provider-attestation /usr/local/libexec
docker cp "${EVIDENCE_DIR}/build/ctr" "${WORKER}:/usr/local/libexec/evpn-ctr"
docker exec "${WORKER}" sh -ec 'chown root:root /usr/local/libexec/evpn-ctr; chmod 755 /usr/local/libexec/evpn-ctr; chmod 600 /run/containerd/containerd.sock'
CTR_SHA="$(docker exec "${WORKER}" sha256sum /usr/local/libexec/evpn-ctr)"
CTR_SHA="${CTR_SHA%% *}"
k -n k8gobgp-system get bgpconfig/cni-export -o json |
  tee "${EVIDENCE_DIR}/attested-provider-config.json" >/dev/null
sed -e 's/namespace: evpn-system/namespace: cni-test/g' \
  -e "s/evpn-lab-control-plane/${WORKER}/g" \
  "${ROOT}/deploy/operator/manifests/provider-attestation.yaml" | k apply -f -
python3 - "${EVIDENCE_DIR}" "${WORKER}" "${NODE_UID}" "${DAEMONSET_UID}" "${K8GOBGP_IMAGE}" "${GOBGPD_SHA}" "${CTR_SHA}" "${INSTALLATION_UID}" <<'PY'
import hashlib,json,pathlib,sys
p=pathlib.Path(sys.argv[1])
provider=json.loads((p/"attested-provider-config.json").read_text())
spec=json.dumps(provider["spec"],sort_keys=True,separators=(",",":")).encode()
attestation={"nodeName":sys.argv[2],"nodeUID":sys.argv[3],
  "namespace":"k8gobgp-system","daemonSetName":"k8gobgp","daemonSetUID":sys.argv[4],
  "containerName":"k8gobgp","image":sys.argv[5],"imageDigest":sys.argv[5].split("@")[1],
  "expectedUID":0,"executableSHA256":sys.argv[6],"runtime":"containerd-ctr-v1",
  "runtimeSocket":"/run/containerd/containerd.sock","ctrPath":"/usr/local/libexec/evpn-ctr","ctrSHA256":sys.argv[7],
  "configNamespace":"k8gobgp-system","configName":"cni-export","configUID":provider["metadata"]["uid"],
  "configDigest":hashlib.sha256(spec).hexdigest()}
(p/"provider-attestation.json").write_text(json.dumps(attestation)+"\n")
for filename in ("export-config.json","remote-config.json"):
    c=json.loads((p/filename).read_text())
    c["AttestationConfigPath"]="/etc/evpn/provider-attestation/provider.json"
    if filename=="export-config.json":
        c["Recovery"]={"StateDirectory":"/run/evpn-node-agent/export-receipts-v1",
                       "Domain":{"InstallationUID":sys.argv[8],"NodeUID":sys.argv[3]}}
    (p/filename).write_text(json.dumps(c)+"\n")
PY
docker exec -i "${WORKER}" sh -ec 'umask 077; cat > /etc/evpn/provider-attestation/provider.json' <"${EVIDENCE_DIR}/provider-attestation.json"
docker exec -i "${WORKER}" sh -ec 'umask 077; cat > /run/evpn-node-agent/export-config.json' <"${EVIDENCE_DIR}/export-config.json"
BOOT_UID="$(docker exec "${WORKER}" cat /proc/sys/kernel/random/boot_id)"
HOST_NETNS="$(docker exec "${WORKER}" readlink /proc/self/ns/net)"
operator_install_cni
CNI_INSTALLED=1
CNI_SHA="$(sha256sum "${EVIDENCE_DIR}/build/evpn-cni")"; CNI_SHA="${CNI_SHA%% *}"
MULTUS_SHA="$(sha256sum "${EVIDENCE_DIR}/multus-cni_4.3.1_linux_amd64/multus")"; MULTUS_SHA="${MULTUS_SHA%% *}"
RUNTIME_SHA="$(docker exec "${WORKER}" sh -ec 'sha256sum "$(command -v containerd)"')"; RUNTIME_SHA="${RUNTIME_SHA%% *}"
SHELL_SHA="$(docker exec "${WORKER}" sha256sum /bin/sh)"; SHELL_SHA="${SHELL_SHA%% *}"
WRAPPER_SHA="$(sha256sum "${EVIDENCE_DIR}/cni-wrapper")"; WRAPPER_SHA="${WRAPPER_SHA%% *}"
docker exec "${WORKER}" sh -ec 'stat -Lc "%n uid=%u gid=%g mode=%a links=%h" /opt/cni/bin/evpn-cni.real /opt/cni/bin/multus /bin/sh "$(command -v containerd)" /opt/cni/bin/evpn-cni' |
  tee "${EVIDENCE_DIR}/cni-authority-executables.txt"
python3 - "${INSTALLATION_UID}" "${NODE_UID}" "${BOOT_UID}" "${HOST_NETNS}" "${WORKER}" "${CTR_SHA}" "${CNI_SHA}" "${MULTUS_SHA}" "${RUNTIME_SHA}" "${SHELL_SHA}" "${WRAPPER_SHA}" <<'PY' |
import json,sys
c={"JournalDirectory":"/run/evpn-node-agent/cni-receipts-v1","Create":True,
   "Domain":{"InstallationUID":sys.argv[1],"NodeUID":sys.argv[2],"BootID":sys.argv[3],"NetworkNamespace":sys.argv[4]},
   "NodeName":sys.argv[5],"Kubeconfig":"","CRIEndpoint":"unix:///run/containerd/containerd.sock",
   "Networks":{"n24":"ls24b"},"CTRPath":"/usr/local/libexec/evpn-ctr","CTRSHA256":sys.argv[6],
   "Admission":{"CNIExecutableSHA256":sys.argv[7],"MultusExecutableSHA256":sys.argv[8],
                "RuntimeExecutableSHA256":sys.argv[9],"WrapperExecutableSHA256":sys.argv[10],
                "WrapperScriptSHA256":sys.argv[11],"WrapperScriptPath":"/opt/cni/bin/evpn-cni"}}
print(json.dumps(c))
PY
  tee "${EVIDENCE_DIR}/cni-continuity.json" |
  docker exec -i "${WORKER}" sh -ec 'umask 077; cat > /run/evpn-node-agent/cni-continuity.json'
cat <<EOF | k apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: evpn-cni-workload-observer, namespace: cni-test}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: evpn-cni-workload-observer, namespace: cni-test}
subjects:
- {kind: ServiceAccount, name: evpn-provider-observer, namespace: cni-test}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: evpn-cni-workload-observer}
EOF
python3 - "${EVIDENCE_DIR}/remote-config.json" "${INSTALLATION_UID}" "${NODE_UID}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
c=json.loads(p.read_text())
c["Netlink"]["Recovery"]={"Domain":{"InstallationUID":sys.argv[2],"NodeUID":sys.argv[3]}}
p.write_text(json.dumps(c)+"\n")
PY
docker exec -i "${WORKER}" sh -ec 'umask 077; cat > /run/evpn-node-agent/remote-config.json' <"${EVIDENCE_DIR}/remote-config.json"
k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/provider-config-before.json" >/dev/null
k -n cni-test patch evpncore type2 --type=merge -p \
  "{\"spec\":{\"nodeAgent\":{\"enabled\":true,\"image\":\"${AGENT_IMAGE}\",\"providerAttestation\":true,\"providerCTRHostPath\":\"/usr/local/libexec/evpn-ctr\",\"cniContinuityConfigPath\":\"/run/evpn-node-agent/cni-continuity.json\",\"exportConfigPath\":\"/run/evpn-node-agent/export-config.json\",\"exportStatusPath\":\"/run/evpn-node-agent/export-status.json\",\"exportInterval\":\"1s\",\"exportTimeout\":\"90s\",\"remoteConfigPath\":\"/run/evpn-node-agent/remote-config.json\",\"remoteStatusPath\":\"/run/evpn-node-agent/remote-status.json\",\"remoteInterval\":\"1s\",\"remoteTimeout\":\"30s\",\"gobgpHostPath\":\"/run/gobgp-export-test\"}}}"
for i in $(seq 1 90); do
  if k -n cni-test get ds/evpn-node-agent >/dev/null 2>&1; then break; fi
  sleep 1
done
k -n cni-test rollout status ds/evpn-node-agent --timeout=240s
AGENT_POD="$(k -n cni-test get pod -l app=evpn-node-agent -o jsonpath='{.items[0].metadata.name}')"
ag nc -z -U /run/evpn-node-agent/agent.sock
k -n cni-test wait --for=condition=Ready evpncore/type2 --timeout=240s
rt sh -ec 'ovn-nbctl --version; ovs-vsctl --version; test "$(ovsdb-client get-schema-version unix:/var/run/ovn/ovnnb_db.sock OVN_Northbound)" = 7.19.0; test "$(ovsdb-client get-schema-version unix:/var/run/ovn/ovnsb_db.sock OVN_Southbound)" = 21.13.0' | tee "${EVIDENCE_DIR}/runtime.txt"
case_status setup PASS

verify_operator_ownership() {
  k -n cni-test get evpncore/type2 deployment/ovn-ovs ds/evpn-node-agent -o json |
    tee "${EVIDENCE_DIR}/operator-ownership.json" >/dev/null
  k -n evpn-operator-system get lease/evpn-core-installation -o json |
    tee "${EVIDENCE_DIR}/installation-lease.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" "${WORKER}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
objects=json.loads((p/"operator-ownership.json").read_text())["items"]
core=next(o for o in objects if o["kind"]=="EvpnCore")
uid=core["metadata"]["uid"]
assert core["spec"]["dataplaneNode"]==sys.argv[2]
assert json.loads((p/"installation-lease.json").read_text())["spec"]["holderIdentity"]==uid
for obj in objects:
    if obj["kind"]=="EvpnCore": continue
    assert obj["metadata"]["annotations"]["evpn.romanspb80.github.io/installation-uid"]==uid
    pod=obj["spec"]["template"]["spec"]
    terms=pod["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
    assert terms==[{"matchFields":[{"key":"metadata.name","operator":"In","values":[sys.argv[2]]}]}],terms
    if obj["kind"]=="DaemonSet":
        assert pod["hostPID"]
        assert obj["status"]["desiredNumberScheduled"]==1
        args=pod["containers"][0]["args"]
        assert "--evpn-export-config=/run/evpn-node-agent/export-config.json" in args
        assert "--evpn-remote-config=/run/evpn-node-agent/remote-config.json" in args
print("UID ownership, singleton Lease, exact-node placement and actual runtime arguments=PASS")
PY
}

wait_export() {
  local label="$1" substring="${2:-}" count="${3:-4}" since="${4:-}" deadline=$((SECONDS+120))
  while :; do
    ag cat /run/evpn-node-agent/export-status.json | tee "${EVIDENCE_DIR}/export-${label}.json" >/dev/null
    if python3 - "${EVIDENCE_DIR}/export-${label}.json" "${substring}" "${count}" "${since}" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
error=s.get("Error","")
process=s.get("SpeakerProcess") or {}
ok=(sys.argv[2] in error if sys.argv[2] else not error)
if sys.argv[2]=="endpoint observation failed":
    # Recovery validates network identity before endpoint observation.
    ok=ok or error=="RECOVERY_BLOCKED: exporter network identity: read Logical_Switch: context deadline exceeded"
ok=ok and len((s.get("Result") or {}).get("Applied") or [])==int(sys.argv[3])
receipts=(s.get("Result") or {}).get("Applied") or []
ok=ok and all(r.get("PathUUID") for r in receipts)
ok=ok and len({r.get("PathUUID") for r in receipts})==len(receipts)
ok=ok and process.get("PID",0)>0 and process.get("StartTicks") and process.get("BootID")
ok=ok and process.get("Executable","").endswith("/gobgpd")
if sys.argv[4]:
    prior=json.load(open(sys.argv[4]))
    ok=ok and s["Time"]>prior["Time"] and (s.get("Result") or {}).get("RecoveryState")=="RUNNING"
sys.exit(0 if ok else 1)
PY
    then return 0; fi
    (( SECONDS<deadline )) || return 1
    sleep 2
  done
}

verify_wire_routes() {
  wait_export initial
  verify_wire_snapshot initial
}

verify_wire_snapshot() {
  local label="$1"
  ag env EVPN_OPERATOR_TYPE2_LIVE=disposable \
    EVPN_OPERATOR_LOCAL_VTEP="${LOCAL_VTEP}" EVPN_OPERATOR_REMOTE_VTEP="${FRR_VTEP}" \
    /usr/local/bin/evpn.test -test.run '^TestOperatorManagedType2Wire_Live$' -test.v |
    tee "${EVIDENCE_DIR}/type2-wire-${label}.log" || return 1
  grep -q '^--- PASS: TestOperatorManagedType2Wire_Live' "${EVIDENCE_DIR}/type2-wire-${label}.log"
}

wait_frozen_export() {
  local label="$1"
  wait_export "${label}-frozen" "endpoint observation failed" 4 || return 1
  python3 - "${EVIDENCE_DIR}/export-${label}-before.json" "${EVIDENCE_DIR}/export-${label}-frozen.json" <<'PY' || return 1
import json,sys
before,after=(json.load(open(p)) for p in sys.argv[1:])
assert after["Time"]>before["Time"],"outage status must be fresh"
assert after["Result"]["OwnershipUnverified"],"failed observation must fail closed"
assert after["Result"]["Applied"]==before["Result"]["Applied"],"receipt/payload changed during observation outage"
assert after["SpeakerProcess"]==before["SpeakerProcess"],"speaker identity changed during database outage"
PY
  verify_wire_snapshot "${label}-frozen"
}

restart_operator() {
  k -n evpn-operator-system rollout restart deployment/evpn-core-operator
  k -n evpn-operator-system rollout status deployment/evpn-core-operator --timeout=120s
  sleep 20
  verify_operator_ownership
  wait_export operator-restart
  interop_wait_import_count 1 operator-restart
  interop_assert_cni_check "$(interop_find_cni_call "$(cat "${EVIDENCE_DIR}/initial-pod-a-uid.txt")" ADD)" \
    "${EVIDENCE_DIR}/operator-restart-check"
  interop_verify_captured_traffic operator-restart
}

session_loss() {
  docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
    -c "neighbor ${LOCAL_VTEP} shutdown" -c end
  interop_wait_import_count 0 session-down
  interop_assert_remote_kernel_withdrawal
  docker exec "${FRR_CONTAINER}" vtysh -c 'configure terminal' -c 'router bgp 64512' \
    -c "no neighbor ${LOCAL_VTEP} shutdown" -c end
  interop_wait_import_count 1 session-restored
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present session-restored
  interop_verify_captured_traffic session-restored
}

database_outage() {
  local database="$1" label="$2" pid
  pid="$(rt cat "/var/run/${database}")"
  [[ "${pid}" =~ ^[0-9]+$ ]]
  wait_export "${label}-before"
  rt kill -STOP "${pid}"
  if ! wait_frozen_export "${label}"; then
    rt kill -CONT "${pid}"
    return 1
  fi
  rt kill -CONT "${pid}"
  wait_export "${label}-restored"
  interop_wait_import_count 1 "${label}-restored"
  interop_verify_captured_traffic "${label}-restored"
}

withdrawal_outage() {
  local pid call old_uid
  old_uid="$(cat "${EVIDENCE_DIR}/initial-pod-a-uid.txt")"
  call="$(interop_find_cni_call "${old_uid}" ADD)"
  pid="$(rt cat /var/run/ovn/ovnnb_db.pid)"
  wait_export withdrawal-observer-before
  rt kill -STOP "${pid}"
  if ! wait_frozen_export withdrawal-observer ||
     ! interop_replay_cni_operation "${call}" DEL "${EVIDENCE_DIR}/del-during-nb-outage" failure; then
    rt kill -CONT "${pid}"
    return 1
  fi
  rt kill -CONT "${pid}"
  k -n cni-test delete pod pod-a --wait=true --timeout=120s
  interop_wait_uid_absent "${old_uid}" outage-delete
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" absent outage-delete
  interop_wait_local_kernel_withdrawal
  create_pod a
  k -n cni-test wait --for=condition=Ready pod/pod-a --timeout=180s
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present outage-replacement
  # Preserve the original evidence; point subsequent lifecycle assertions at the
  # replacement's independently captured identity instead of rerunning collectors.
  copy_calls "${EVIDENCE_DIR}/initial-cni"
  local uid replacement_call_copy
  uid="$(k -n cni-test get pod pod-a -o jsonpath='{.metadata.uid}')"
  replacement_call_copy="$(interop_find_copied_cni_call "${uid}" ADD)"
  interop_wait_endpoint_bound "${uid}" outage-replacement "${replacement_call_copy}" pod-a
  cp "${EVIDENCE_DIR}/endpoint-a-identity.json" "${EVIDENCE_DIR}/endpoint-a-pre-outage-identity.json"
  cp "${EVIDENCE_DIR}/outage-replacement-identity.json" "${EVIDENCE_DIR}/endpoint-a-identity.json"
  cp "${EVIDENCE_DIR}/initial-pod-a-uid.txt" "${EVIDENCE_DIR}/pre-outage-pod-a-uid.txt"
  printf '%s\n' "${uid}" >"${EVIDENCE_DIR}/initial-pod-a-uid.txt"
  interop_verify_captured_traffic withdrawal-recovered
}

restart_agent_with_endpoints() {
  local old_agent="${AGENT_POD}" before="${EVIDENCE_DIR}/agent-recovery-before.json"
  wait_export agent-recovery-before
  cp "${EVIDENCE_DIR}/export-agent-recovery-before.json" "${before}"
  copy_calls "${EVIDENCE_DIR}/agent-recovery-cni-before"
  if [[ "${EVPN_OPERATOR_TYPE2_FORCE_AGENT_CRASH:-0}" == 1 ]]; then
    local container_id task_pid restart_before deadline=$((SECONDS+180))
    container_id="$(k -n cni-test get pod "${AGENT_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="agent")].containerID}')"
    [[ "${container_id}" =~ ^containerd://[a-f0-9]{64}$ ]]
    restart_before="$(k -n cni-test get pod "${AGENT_POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="agent")].restartCount}')"
    task_pid="$(docker exec "${WORKER}" ctr --namespace k8s.io tasks list |
      awk -v id="${container_id#containerd://}" '$1==id && $3=="RUNNING" {print $2}')"
    [[ "${task_pid}" =~ ^[1-9][0-9]*$ ]]
    [[ "$(docker exec "${WORKER}" readlink "/proc/${task_pid}/exe")" == /usr/local/bin/evpn-node-agent ]]
    printf 'forced-agent-crash container=%s pid=%s\n' "${container_id}" "${task_pid}" |
      tee "${EVIDENCE_DIR}/forced-agent-crash.txt"
    docker exec "${WORKER}" kill -KILL "${task_pid}"
    while :; do
      k -n cni-test get pod "${AGENT_POD}" -o json |
        tee "${EVIDENCE_DIR}/forced-agent-replacement.json" >/dev/null
      if python3 - "${EVIDENCE_DIR}/forced-agent-replacement.json" "${restart_before}" <<'PY'
import json,sys
p=json.load(open(sys.argv[1]))
s=next(s for s in p["status"]["containerStatuses"] if s["name"]=="agent")
sys.exit(0 if s["restartCount"]>int(sys.argv[2]) and s.get("ready") and "running" in s["state"] else 1)
PY
      then break; fi
      (( SECONDS<deadline )) || return 1
      sleep 1
    done
  else
    k -n cni-test rollout restart ds/evpn-node-agent
    k -n cni-test rollout status ds/evpn-node-agent --timeout=180s
    AGENT_POD="$(k -n cni-test get pod -l app=evpn-node-agent -o jsonpath='{.items[0].metadata.name}')"
    [[ "${AGENT_POD}" != "${old_agent}" ]]
  fi
  wait_export agent-recovery-after "" 4 "${before}"
  interop_wait_import_count 1 agent-recovery-after
  interop_assert_cni_check "$(interop_find_cni_call "$(cat "${EVIDENCE_DIR}/initial-pod-a-uid.txt")" ADD)" \
    "${EVIDENCE_DIR}/agent-recovery-check"
  interop_verify_captured_traffic agent-recovery
  copy_calls "${EVIDENCE_DIR}/agent-recovery-cni-after"
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
a=json.loads((p/"agent-recovery-before.json").read_text())
b=json.loads((p/"export-agent-recovery-after.json").read_text())
assert b["Time"]>a["Time"],"stale pre-restart status"
assert b["Result"]["RecoveryState"]=="RUNNING" and not b.get("Error"),b
assert a["Result"]["Applied"]==b["Result"]["Applied"],"same-speaker UUID receipts changed"
assert a["SpeakerProcess"]==b["SpeakerProcess"],"speaker changed during agent-only restart"
def additions(directory):
    result={}
    for f in directory.glob("call.*/env"):
        env=dict(line.split("=",1) for line in f.read_text().splitlines() if "=" in line)
        if env.get("CNI_COMMAND")=="ADD":
            result[str(f.parent.relative_to(directory))]=(env,(f.parent/"config").read_text(),(f.parent/"rc").read_text())
    return result
before=additions(p/"agent-recovery-cni-before")
assert len(before)>=2 and before==additions(p/"agent-recovery-cni-after"),"CNI ADD replay or missing capture"
print("acknowledged receipts retained under the same attested speaker=PASS")
PY
}

restart_speaker_with_endpoints() {
  local old_provider="${PROVIDER_POD}"
  wait_export speaker-recovery-before
  k -n k8gobgp-system get bgpconfig/cni-export -o json |
    tee "${EVIDENCE_DIR}/speaker-recovery-config-before.json" >/dev/null
  k -n k8gobgp-system delete pod "${old_provider}" --wait=true --timeout=120s
  k -n k8gobgp-system rollout status ds/k8gobgp --timeout=180s
  PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
  [[ "${PROVIDER_POD}" != "${old_provider}" ]]
  wait_export speaker-recovery-after "" 4 "${EVIDENCE_DIR}/export-speaker-recovery-before.json"
  interop_wait_import_count 1 speaker-recovery-after
  interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present speaker-recovery
  interop_verify_captured_traffic speaker-recovery
  k -n k8gobgp-system get bgpconfig/cni-export -o json |
    tee "${EVIDENCE_DIR}/speaker-recovery-config-after.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
a,b=(json.loads((p/f"export-speaker-recovery-{x}.json").read_text()) for x in ("before","after"))
assert b["Time"]>a["Time"] and b["Result"]["RecoveryState"]=="RUNNING" and not b.get("Error"),b
assert a["SpeakerProcess"]!=b["SpeakerProcess"],"replacement speaker not observed"
old={r["PathUUID"] for r in a["Result"]["Applied"]}
new={r["PathUUID"] for r in b["Result"]["Applied"]}
assert len(old)==len(new)==4 and old.isdisjoint(new),"UUIDs transferred across incarnations"
c,d=(json.loads((p/f"speaker-recovery-config-{x}.json").read_text()) for x in ("before","after"))
assert c["metadata"]["uid"]==d["metadata"]["uid"] and c["spec"]==d["spec"]
assert c["metadata"]["generation"]==d["metadata"]["generation"],"competing provider configuration writer"
print("independently attested new speaker, fresh UUIDs and unchanged provider configuration=PASS")
PY
}

restart_importer() {
  local old_agent="${AGENT_POD}" marker_before marker_after deadline=$((SECONDS+90))
  # No local endpoints exist yet. Importer receipts are live, but there are no
  # exporter UUID receipts or CNI cache entries requiring Phase D recovery.
  [[ -z "$(k -n cni-test get pod -l app=pod-a -o name)" ]]
  interop_wait_import_count 1 importer-before-restart
  k -n k8gobgp-system get pod "${PROVIDER_POD}" -o json | tee "${EVIDENCE_DIR}/importer-provider-before.json" >/dev/null
  ag cat /run/evpn-node-agent/remote-status.json | tee "${EVIDENCE_DIR}/importer-before-restart.json" >/dev/null
  marker_before="$(docker exec "${WORKER}" sha256sum /run/evpn-node-agent/remote-state/remote-netlink-owner)"
  k -n cni-test rollout restart ds/evpn-node-agent
  k -n cni-test rollout status ds/evpn-node-agent --timeout=180s
  AGENT_POD="$(k -n cni-test get pod -l app=evpn-node-agent -o jsonpath='{.items[0].metadata.name}')"
  [[ "${AGENT_POD}" != "${old_agent}" ]]
  while :; do
    ag cat /run/evpn-node-agent/remote-status.json | tee "${EVIDENCE_DIR}/importer-after-restart.json" >/dev/null
    k -n k8gobgp-system get pod "${PROVIDER_POD}" -o json | tee "${EVIDENCE_DIR}/importer-provider-after.json" >/dev/null
    k -n k8gobgp-system get bgpconfig cni-export -o json | tee "${EVIDENCE_DIR}/provider-config-after.json" >/dev/null
    if python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
a,b=(json.loads((p/f"importer-{stage}-restart.json").read_text()) for stage in ("before","after"))
sys.exit(0 if b["Time"]>a["Time"] and b.get("RecoveryState")=="RUNNING" and not b.get("Error") else 1)
PY
    then
      if k -n cni-test logs "${AGENT_POD}" -c agent |
        tee "${EVIDENCE_DIR}/importer-restarted-agent.log" | grep -q '"RecoveryState":"RUNNING"'; then break; fi
    fi
    (( SECONDS<deadline )) || return 1
    sleep 1
  done
  interop_wait_import_count 1 importer-after-restart
  marker_after="$(docker exec "${WORKER}" sha256sum /run/evpn-node-agent/remote-state/remote-netlink-owner)"
  [[ "${marker_before}" == "${marker_after}" ]]
  ag cat /run/evpn-node-agent/remote-status.json | tee "${EVIDENCE_DIR}/importer-after-restart.json" >/dev/null
  docker exec "${WORKER}" cat /run/evpn-node-agent/remote-state/receipts-v1/journal.json |
    tee "${EVIDENCE_DIR}/importer-recovered-journal.json" >/dev/null
  python3 - "${EVIDENCE_DIR}" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
a,b=(json.loads((p/f"importer-{stage}-restart.json").read_text()) for stage in ("before","after"))
assert a["Receipts"] and a["Receipts"]==b["Receipts"],(a,b)
assert b["RecoveryState"]=="RUNNING" and not b.get("Error"),b
assert b["Snapshot"]["Authoritative"]
j=json.loads((p/"importer-recovered-journal.json").read_text())["Record"]
assert j["Pending"] is None,j
a,b=(json.loads((p/f"importer-provider-{stage}.json").read_text()) for stage in ("before","after"))
assert a["metadata"]["uid"]==b["metadata"]["uid"]
assert a["status"]["containerStatuses"]==b["status"]["containerStatuses"],"provider restarted during importer-only case"
a,b=(json.loads((p/f"provider-config-{stage}.json").read_text()) for stage in ("before","after"))
assert a["metadata"]["uid"]==b["metadata"]["uid"] and a["spec"]==b["spec"]
assert a["metadata"]["generation"]==b["metadata"]["generation"]
print("same durable controller, exact receipts, fresh authoritative snapshot and unchanged marker=PASS")
PY
}

# Keep the initial packet assertions, then run recovery at the top level with
# errexit enabled before the unchanged full withdrawal/survivor assertions.
interop_run_lifecycle() { return 0; }

run_case operator_ownership verify_operator_ownership
run_case importer_restart restart_importer
ACTIVE_CASE=cni_lifecycle
create_pod a
create_pod b
k -n cni-test wait --for=condition=Ready pod/pod-a pod/pod-b --timeout=180s
ACTIVE_CASE=cni_identity
for i in $(seq 1 90); do
  docker exec "${WORKER}" cat /run/evpn-node-agent/cni-receipts-v1/journal.json >"${EVIDENCE_DIR}/cni-identity-journal.json"
  if python3 - "${EVIDENCE_DIR}/cni-identity-journal.json" <<'PY'
import json,sys
record=json.load(open(sys.argv[1]))["Record"]
assert record["Pending"] is None
endpoints=list(record["State"]["Endpoints"].values())
assert len(endpoints)==2 and all(e["State"]=="active" for e in endpoints)
for e in endpoints:
    admission=e["Admission"]
    assert admission["ContainerID"]==e["Runtime"]["container_id"]
    assert admission["PodUID"]==e["Identity"]["workload_uid"]
    assert admission["NetNSDevice"]==e["Receipt"]["NetNSDevice"]
    assert admission["NetNSInode"]==e["Receipt"]["NetNSInode"]
    assert e["VerifiedAt"]>=e["CreatedAt"]
PY
  then break; fi
  [[ "${i}" != 90 ]]
  sleep 1
done
k -n cni-test logs "${AGENT_POD}" -c agent |
  tee "${EVIDENCE_DIR}/cni-identity-transitions.log" >/dev/null
[[ "$(grep -c 'CNI identity state=pending_creation' "${EVIDENCE_DIR}/cni-identity-transitions.log")" -ge 2 ]]
[[ "$(grep -c 'CNI identity state=active' "${EVIDENCE_DIR}/cni-identity-transitions.log")" -ge 2 ]]
case_status cni_identity PASS "authenticated invocation and durable pending-to-active transition"
ACTIVE_CASE=mp_bgp_exchange
interop_verify
case_status type2_import PASS "actual authoritative importer, owned FDB/neighbor receipts, remote MAC-only+MAC/IP aggregation"
case_status mp_bgp_exchange PASS "Established real GoBGP/FRR EVPN session and routes in both RIBs"
case_status dataplane_forwarding PASS "bidirectional ICMP and packet-path VXLAN/OpenFlow verification"
run_case type2_export verify_wire_routes
run_case operator_restart restart_operator
run_case node_agent_restart restart_agent_with_endpoints
run_case speaker_restart restart_speaker_with_endpoints
run_case bgp_session_loss session_loss
run_case nbdb_unavailability database_outage ovn/ovnnb_db.pid nbdb
run_case ovsdb_unavailability database_outage openvswitch/ovsdb-server.pid ovsdb
run_case withdrawal_during_failure withdrawal_outage
run_case cni_lifecycle interop_verify_full_lifecycle
case_status route_withdrawal PASS "local/remote RIB, UUID, kernel, OVN/OVS withdrawal and foreign survivor checks"
ACTIVE_CASE=cleanup
k -n cni-test patch evpncore/type2 --type=merge -p '{"spec":{"nodeAgent":{"enabled":false}}}'
k -n cni-test wait --for=delete ds/evpn-node-agent --timeout=120s
AGENT_POD=""
k -n cni-test delete evpncore/type2 --wait=true --timeout=180s
k -n k8gobgp-system get ds/k8gobgp bgpconfig/cni-export >/dev/null
echo "operator-managed acknowledged agent recovery and verified replacement speaker recovery=PASS; retained ambiguous AddPath recovery remains BLOCKED"
