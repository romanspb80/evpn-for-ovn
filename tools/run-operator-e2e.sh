#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
KIND_CLUSTER="evpn-operator-${RUN_ID,,}"
WORKER="${KIND_CLUSTER}-worker"
EVIDENCE_DIR="${EVPN_OPERATOR_EVIDENCE_ROOT:-/tmp/evpn-operator-e2e}/attempt-${RUN_ID}"
KUBECONFIG="${EVIDENCE_DIR}/kubeconfig"
IMAGE="evpn4ovn/operator-e2e:${RUN_ID,,}"
BASE_ID="sha256:f01f29f4d8b1fd4f377103015aa7b7dab6f8189416ce2a363f63d7e538c4e5a7"
OVN_IMAGE="evpn4ovn/ovn-ovs:kind-26.09"
OPERATOR_INSTALLED=0
export ROOT KIND_CLUSTER WORKER EVIDENCE_DIR KUBECONFIG
source "${ROOT}/tools/lib/containerlab-e2e.sh"
source "${ROOT}/tools/lib/cni-agent-image-test.sh"
source "${ROOT}/tools/lib/cni-evpn-export-test.sh"
e2e_lab_acquire
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "kind-${KIND_CLUSTER}" "$@"; }
wait_current_ready() {
  for i in $(seq 1 150); do
    state="$(k -n evpn-installation get evpncore/core -o jsonpath='{.metadata.generation}{" "}{.status.observedGeneration}{" "}{.status.ready}')"
    read -r generation observed ready <<<"${state}"
    if [[ "${generation}" == "${observed}" && "${ready}" == true ]]; then return 0; fi
    sleep 2
  done
  echo "current-generation EvpnCore Ready timed out: ${state}" >&2
  return 1
}
cleanup() {
  local rc=$? cleanup_rc=0
  trap - EXIT
  if [[ -f "${EVIDENCE_DIR}/lab.json" ]]; then
    if [[ "${OPERATOR_INSTALLED}" == 1 ]]; then
      k -n evpn-operator-system logs deployment/evpn-core-operator 2>&1 | tee "${EVIDENCE_DIR}/operator.log" >/dev/null || cleanup_rc=1
      k get evpncores.evpn.romanspb80.github.io -A -o yaml 2>&1 | tee "${EVIDENCE_DIR}/cores-final.yaml" >/dev/null || cleanup_rc=1
    fi
    e2e_lab_down || cleanup_rc=1
  fi
  test_image_cleanup || cleanup_rc=1
  printf 'test-exit=%s\ncleanup-exit=%s\n' "${rc}" "${cleanup_rc}" >"${EVIDENCE_DIR}/result.txt"
  echo "Operator lifecycle E2E: test-exit=${rc} cleanup-exit=${cleanup_rc} evidence=${EVIDENCE_DIR}"
  if (( rc != 0 )); then exit "${rc}"; fi
  exit "${cleanup_rc}"
}
mkdir -p "${EVIDENCE_DIR}/build"
trap cleanup EXIT
test_image_snapshot
test_image_ensure_base
test_image_require_new_tag "${IMAGE}"
docker image inspect "${OVN_IMAGE}" evpn4ovn/app:kind >/dev/null
(
  cd "${ROOT}"
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/operator" ./deploy/operator
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/evpn-node-agent" ./cmd/evpn-node-agent
  CGO_ENABLED=0 go build -o "${EVIDENCE_DIR}/build/provider-grpc-proxy" ./tools/e2e/operator/grpc-proxy
)
cp "${ROOT}/deploy/operator/core-resources.yaml" "${EVIDENCE_DIR}/build/core-resources.yaml"
docker build --quiet -t "${IMAGE}" -f "${ROOT}/tools/e2e/operator/Dockerfile" "${EVIDENCE_DIR}/build"
test_image_record_created "${IMAGE}"
e2e_lab_up cni
kind load docker-image --name "${KIND_CLUSTER}" "${IMAGE}" "${OVN_IMAGE}" evpn4ovn/app:kind
k wait --for=condition=Ready nodes --all --timeout=180s
k label node "${WORKER}" evpn.romanspb80.github.io/dataplane-ready=true
export_setup_provider
LOCAL_VTEP="$(k get node "${WORKER}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
cat <<EOF | k apply -f -
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata: {name: operator-independent, namespace: k8gobgp-system}
spec:
  global:
    asn: 64512
    routerID: "${LOCAL_VTEP}"
    listenPort: 1179
    listenAddresses: ["127.0.0.1"]
    families: ["l2vpn-evpn"]
---
apiVersion: v1
kind: Service
metadata: {name: k8gobgp, namespace: k8gobgp-system}
spec:
  selector: {app: operator-provider-grpc-proxy}
  ports: [{name: grpc, port: 50051, targetPort: 50054}]
---
# Test transport only: no BGP configuration or route writer.
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
          image: ${IMAGE}
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/provider-grpc-proxy"]
          readinessProbe:
            exec:
              command: ["/bin/sh", "-ec", "test -S /var/run/gobgp/gobgp.sock && nc -z 127.0.0.1 50054"]
          volumeMounts:
            - {name: gobgp, mountPath: /var/run/gobgp, readOnly: true}
      volumes:
        - name: gobgp
          hostPath: {path: /run/gobgp-export-test, type: DirectoryOrCreate}
EOF
k -n k8gobgp-system rollout status ds/k8gobgp --timeout=240s
k -n k8gobgp-system rollout status deployment/operator-provider-grpc-proxy --timeout=120s
PROVIDER_POD="$(k -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')"
for i in $(seq 1 60); do
  if k -n k8gobgp-system exec "${PROVIDER_POD}" -- gobgp global | tee "${EVIDENCE_DIR}/provider-global.txt" &&
      grep -Eq 'AS:[[:space:]]+64512' "${EVIDENCE_DIR}/provider-global.txt"; then break; fi
  sleep 1
done
grep -Eq 'AS:[[:space:]]+64512' "${EVIDENCE_DIR}/provider-global.txt"
k -n k8gobgp-system get ds/k8gobgp deployment/operator-provider-grpc-proxy service/k8gobgp bgpconfig/operator-independent -o json | tee "${EVIDENCE_DIR}/provider-before.json" >/dev/null
sed "s|evpn4ovn/operator:kind|${IMAGE}|" "${ROOT}/deploy/operator/manifests/operator.yaml" | k apply -f -
OPERATOR_INSTALLED=1
k wait --for=condition=Established crd/evpncores.evpn.romanspb80.github.io --timeout=60s
k -n evpn-operator-system rollout status deployment/evpn-core-operator --timeout=120s
k create namespace evpn-installation
k -n evpn-installation create rolebinding namespace-editor --clusterrole=edit --serviceaccount=evpn-installation:default
for verb in create update patch; do
  if answer="$(k auth can-i "${verb}" evpncores.evpn.romanspb80.github.io -n evpn-installation --as=system:serviceaccount:evpn-installation:default)"; then
    echo "namespace editor unexpectedly has EvpnCore ${verb} access" >&2
    exit 1
  fi
  [[ "${answer}" == no ]]
done
k -n evpn-installation create serviceaccount trusted-admin
k -n evpn-installation create rolebinding trusted-core-admin --clusterrole=evpn-core-administrator --serviceaccount=evpn-installation:trusted-admin
for verb in create update patch; do
  [[ "$(k auth can-i "${verb}" evpncores.evpn.romanspb80.github.io -n evpn-installation --as=system:serviceaccount:evpn-installation:trusted-admin)" == yes ]]
done
echo "ordinary-namespace-editor-denied, explicit-trusted-administrator-authorized=PASS"
docker exec "${WORKER}" mkdir -p /run/evpn-node-agent
printf '{"Enabled":false}\n' | docker exec -i "${WORKER}" sh -c 'cat > /run/evpn-node-agent/export-config.json'
cat <<EOF | k apply -f -
apiVersion: evpn.romanspb80.github.io/v1alpha1
kind: EvpnCore
metadata: {name: core, namespace: evpn-installation}
spec:
  enabled: true
  dataplaneNode: ${WORKER}
  appNamespace: operator-apps
  ovnNamespace: operator-ovn
  ovnImage: ${OVN_IMAGE}
  nodeAgent:
    enabled: true
    image: ${IMAGE}
    exportConfigPath: /run/evpn-node-agent/export-config.json
    gobgpHostPath: /run/gobgp-export-test
EOF
wait_current_ready
[[ "$(k -n evpn-installation get evpncore/core -o jsonpath='{.spec.bgp.provider}')" == k8gobgp ]]
k -n operator-apps get ds/evpn-node-agent -o json | tee "${EVIDENCE_DIR}/agent.json" >/dev/null
k -n evpn-installation get evpncore/core -o json | tee "${EVIDENCE_DIR}/core-ready.json" >/dev/null
AGENT_POD="$(k -n operator-apps get pods -l app=evpn-node-agent -o jsonpath='{.items[0].metadata.name}')"
k -n operator-apps exec "${AGENT_POD}" -- nc -z -U /run/evpn-node-agent/agent.sock
CORE_UID="$(k -n evpn-installation get evpncore/core -o jsonpath='{.metadata.uid}')"
sleep 5
k get deployments,daemonsets,services -A -l "evpn.romanspb80.github.io/installation-uid=${CORE_UID}" -o json | tee "${EVIDENCE_DIR}/stable-before.json" >/dev/null
sleep 35
k get deployments,daemonsets,services -A -l "evpn.romanspb80.github.io/installation-uid=${CORE_UID}" -o json | tee "${EVIDENCE_DIR}/stable-after.json" >/dev/null
python3 - "${EVIDENCE_DIR}" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
versions = lambda file: sorted((o["kind"], o["metadata"]["namespace"], o["metadata"]["name"], o["metadata"]["resourceVersion"]) for o in json.loads((p/file).read_text())["items"])
assert versions("stable-before.json") == versions("stable-after.json"), "unchanged managed resources were rewritten"
print("real-api-server-idempotency=PASS")
PY
# Restore a lost journal using installation labels after an operator restart.
k -n evpn-operator-system rollout restart deployment/evpn-core-operator
k -n evpn-operator-system rollout status deployment/evpn-core-operator --timeout=120s
k -n evpn-installation patch evpncore/core --subresource=status --type=merge -p '{"status":{"managedResources":[]}}'
for i in $(seq 1 60); do
  inventory="$(k -n evpn-installation get evpncore/core -o jsonpath='{.status.managedResources[*].uid}')"
  if [[ "$(wc -w <<<"${inventory}")" == 9 ]]; then break; fi
  sleep 1
done
[[ "$(wc -w <<<"${inventory}")" == 9 ]]
# Namespace rollback must restore declarative endpoints, not preserve old DNS.
k -n evpn-installation patch evpncore/core --type=merge -p '{"spec":{"appNamespace":"evpn-system","ovnNamespace":"ovn-system"}}'
wait_current_ready
[[ "$(k -n evpn-system get deployment/ovsdb-mgmt -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" == tcp:ovn-ovs.ovn-system.svc.cluster.local:6641 ]]
[[ "$(k -n evpn-system get deployment/bgp-ad-handler -o jsonpath='{.spec.template.spec.containers[0].env[2].value}')" == ovsdb-mgmt.evpn-system.svc.cluster.local:50053 ]]
k -n evpn-installation patch evpncore/core --type=merge -p '{"spec":{"appNamespace":"operator-apps","ovnNamespace":"operator-ovn"}}'
wait_current_ready
[[ "$(k -n operator-apps get deployment/ovsdb-mgmt -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" == tcp:ovn-ovs.operator-ovn.svc.cluster.local:6641 ]]
[[ "$(k -n operator-apps get pods -l app=evpn-node-agent -o jsonpath='{.items[0].spec.nodeName}')" == "${WORKER}" ]]
[[ "$(k -n operator-ovn get pods -l app=ovn-ovs -o jsonpath='{.items[0].spec.nodeName}')" == "${WORKER}" ]]
echo "restart, missing-journal-recovery, namespace-round-trip, exact-node-placement=PASS"
cat <<EOF | k apply -f -
apiVersion: evpn.romanspb80.github.io/v1alpha1
kind: EvpnCore
metadata: {name: second, namespace: evpn-installation}
spec: {enabled: true, dataplaneNode: "${WORKER}"}
EOF
k -n evpn-installation wait --for=condition=Degraded evpncore/second --timeout=60s
k -n evpn-installation get evpncore/second -o json | tee "${EVIDENCE_DIR}/singleton-conflict.json" >/dev/null
k -n evpn-installation patch evpncore/second --type=merge -p '{"spec":{"enabled":false}}'
if cat <<EOF | k apply -f - 2>&1 | tee "${EVIDENCE_DIR}/unsupported-provider.log"
apiVersion: evpn.romanspb80.github.io/v1alpha1
kind: EvpnCore
metadata: {name: unsupported, namespace: evpn-installation}
spec: {enabled: true, dataplaneNode: "${WORKER}", bgp: {provider: standalone}}
EOF
then
  echo "standalone provider was admitted unexpectedly" >&2
  exit 1
fi
grep -q 'Unsupported value: "standalone"' "${EVIDENCE_DIR}/unsupported-provider.log"
k -n evpn-installation patch evpncore/core --type=merge -p '{"spec":{"nodeAgent":{"exportInterval":"3s"}}}'
for i in $(seq 1 60); do
  args="$(k -n operator-apps get ds/evpn-node-agent -o jsonpath='{.spec.template.spec.containers[0].args}')"
  if [[ "${args}" == *"--evpn-export-interval=3s"* ]]; then break; fi
  sleep 1
done
[[ "${args}" == *"--evpn-export-interval=3s"* ]]
k -n operator-apps rollout status ds/evpn-node-agent --timeout=180s
k -n evpn-installation patch evpncore/core --type=merge -p '{"spec":{"nodeAgent":{"enabled":false}}}'
k -n operator-apps wait --for=delete ds/evpn-node-agent --timeout=120s
k -n operator-apps create configmap independent --from-literal=preserve=true
k -n evpn-installation delete evpncore/core --wait=true --timeout=180s
k -n operator-apps get deployments,daemonsets,services -o json | tee "${EVIDENCE_DIR}/apps-after-delete.json" >/dev/null
k -n operator-ovn get deployments,daemonsets,services -o json | tee "${EVIDENCE_DIR}/ovn-after-delete.json" >/dev/null
k -n operator-apps get configmap/independent >/dev/null
k -n k8gobgp-system get ds/k8gobgp deployment/operator-provider-grpc-proxy service/k8gobgp bgpconfig/operator-independent -o json | tee "${EVIDENCE_DIR}/provider-after.json" >/dev/null
python3 - "${EVIDENCE_DIR}" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
before = json.loads((p/"provider-before.json").read_text())["items"]
after = json.loads((p/"provider-after.json").read_text())["items"]
identity = lambda items: sorted((o["kind"], o["metadata"]["name"], o["metadata"]["uid"], json.dumps(o["spec"], sort_keys=True)) for o in items)
assert identity(before) == identity(after), "independent provider changed"
for file in ("apps-after-delete.json", "ovn-after-delete.json"):
    assert not json.loads((p/file).read_text())["items"], file
conflict = json.loads((p/"singleton-conflict.json").read_text())
assert not conflict["status"].get("managedResources"), "singleton loser managed resources"
assert any(c["reason"] == "OwnershipConflict" and c["status"] == "True" for c in conflict["status"]["conditions"])
print("operator-managed-agent, singleton, defaults, updates, readiness, finalizer, cross-namespace and provider-preservation=PASS")
PY
docker exec "${WORKER}" test -f /run/evpn-node-agent/export-config.json
k get namespace operator-apps operator-ovn >/dev/null
