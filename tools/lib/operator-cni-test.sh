#!/usr/bin/env bash
# The real thin-Multus/CNI bootstrap, restricted to the run-owned worker.

operator_install_cni() {
  local multus_sha=88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e
  e2e_fetch_pinned_artifact \
    https://github.com/k8snetworkplumbingwg/multus-cni/releases/download/v4.3.1/multus-cni_4.3.1_linux_amd64.tar.gz \
    "${multus_sha}" multus-cni_4.3.1_linux_amd64.tar.gz "${EVIDENCE_DIR}/multus.tar.gz"
  tar -xzf "${EVIDENCE_DIR}/multus.tar.gz" -C "${EVIDENCE_DIR}" multus-cni_4.3.1_linux_amd64/multus
  k apply -f "${ROOT}/deploy/kind/cni-multus-test/rbac.yaml"
  k wait --for=condition=Established crd/network-attachment-definitions.k8s.cni.cncf.io --timeout=60s
  docker exec "${WORKER}" sh -ec 'test ! -e /opt/cni/bin/evpn-cni; test ! -L /opt/cni/bin/evpn-cni; test ! -e /opt/cni/bin/evpn-cni.real; test ! -L /opt/cni/bin/evpn-cni.real; test ! -e /opt/cni/bin/multus; mkdir -p /etc/cni/net.d/multus.d /run/evpn-cni-evidence'
  local size sha wrapper_sha token ca server
  size="$(stat -c %s "${EVIDENCE_DIR}/build/evpn-cni")"
  sha="$(sha256sum "${EVIDENCE_DIR}/build/evpn-cni")"
  sha="${sha%% *}"
  stage_worker_cni_file "${WORKER}" "${EVIDENCE_DIR}/build/evpn-cni" /opt/cni/bin/evpn-cni.real \
    "${size}" "${sha}" real | tee "${EVIDENCE_DIR}/worker-cni-integrity.txt"
  cat >"${EVIDENCE_DIR}/cni-wrapper" <<'EOF'
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
  wrapper_sha="$(sha256sum "${EVIDENCE_DIR}/cni-wrapper")"
  stage_worker_cni_file "${WORKER}" "${EVIDENCE_DIR}/cni-wrapper" /opt/cni/bin/evpn-cni \
    "$(stat -c %s "${EVIDENCE_DIR}/cni-wrapper")" "${wrapper_sha%% *}" wrapper \
    | tee "${EVIDENCE_DIR}/worker-cni-wrapper-integrity.txt"
  verify_worker_cni_version "${WORKER}" "${EVIDENCE_DIR}" "${size}" "${sha}"
  docker cp "${EVIDENCE_DIR}/multus-cni_4.3.1_linux_amd64/multus" "${WORKER}:/opt/cni/bin/multus"
  docker exec "${WORKER}" chmod 755 /opt/cni/bin/multus
  docker exec "${WORKER}" chown root:root /opt/cni/bin/evpn-cni.real /opt/cni/bin/evpn-cni /opt/cni/bin/multus
  docker exec "${WORKER}" /opt/cni/bin/multus --version 2>&1 | tee "${EVIDENCE_DIR}/multus-version.txt"
  token="$(k -n kube-system create token multus-test --duration=2h)"
  ca="$(k config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  server="$(docker exec "${WORKER}" awk '/server:/{print $2; exit}' /etc/kubernetes/kubelet.conf)"
  cat <<EOF | docker exec -i "${WORKER}" sh -ec 'umask 077; cat > /etc/cni/net.d/multus.d/kubeconfig'
apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: ${server}
    certificate-authority-data: ${ca}
users:
- name: multus
  user:
    token: ${token}
contexts:
- name: test
  context: {cluster: test, user: multus}
current-context: test
EOF
  unset token ca
  docker exec "${WORKER}" sh -ec 'cp /etc/cni/net.d/10-kindnet.conflist /run/kindnet-before.json'
  docker exec "${WORKER}" cat /etc/cni/net.d/10-kindnet.conflist | python3 -c '
import json,sys
primary=json.load(sys.stdin)
print(json.dumps({"cniVersion":"1.0.0","name":"multus-test","type":"multus","kubeconfig":"/etc/cni/net.d/multus.d/kubeconfig","logLevel":"debug","logFile":"/var/log/multus.log","logToStderr":True,"retryDeleteOnError":True,"delegates":[primary]}))
' | docker exec -i "${WORKER}" sh -ec 'cat > /etc/cni/net.d/00-multus.conf'
  docker exec "${WORKER}" cmp /run/kindnet-before.json /etc/cni/net.d/10-kindnet.conflist
  local which ip mac
  for which in a b; do
    ip=10; mac=0a
    if [[ "${which}" == b ]]; then ip=11; mac=0b; fi
    cat <<EOF | tee "${EVIDENCE_DIR}/nad-${which}.yaml" | k -n cni-test apply -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata: {name: evpn-${which}}
spec:
  config: '{"cniVersion":"1.0.0","name":"evpn-${which}","type":"evpn-cni","nodeAgentSocket":"/run/evpn-node-agent/agent.sock","networkID":"n24","attachmentID":"s","logicalSwitch":"ls24b","ipAddress":"10.244.240.${ip}/24","macAddress":"0a:58:f0:00:00:${mac}","creationIdentity":${CNI_CREATION_IDENTITY:-false}}'
EOF
  done
}

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

copy_calls() {
  mkdir -p "$1"
  docker exec "${WORKER}" tar -cf - -C /run/evpn-cni-evidence . | tar -xf - -C "$1"
}
