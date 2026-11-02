#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
action="${1:-up}"
directory="${EVIDENCE_DIR:-${HOME}/.local/state/evpn-for-ovn/containerlab/manual-kind-frr}"
cluster="${KIND_CLUSTER_NAME:-kind0}"
case "${action}" in
  up)
    exec python3 "${ROOT}/tools/e2e/containerlab.py" up --directory "${directory}" \
      --profile framework-netlink --cluster "${cluster}" \
      --network "evpn33b3-${cluster}" --frr "${cluster}-frr"
    ;;
  status|collect|down)
    exec python3 "${ROOT}/tools/e2e/containerlab.py" "${action}" --directory "${directory}"
    ;;
  *)
    echo "Usage: $0 [up|status|collect|down]; use the E2E runners for runtime deployment and assertions" >&2
    exit 2
    ;;
esac
