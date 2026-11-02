#!/usr/bin/env bash
# Shared environment ownership; suite assertions remain in their runners.

e2e_lab_acquire() {
  command -v containerlab >/dev/null || { echo "Containerlab 0.79.0 is required for E2E" >&2; return 1; }
  mkdir -p "${HOME}/.local/state/evpn-for-ovn/containerlab"
  exec {E2E_LAB_LOCK}>"${HOME}/.local/state/evpn-for-ovn/containerlab/e2e.lock"
  flock -n "${E2E_LAB_LOCK}" || {
    echo "another EVPN E2E run is active; parallel runs are not supported" >&2; return 1;
  }
  export EVPN_E2E_LOCK_FD="${E2E_LAB_LOCK}"
}

e2e_lab_up() {
  local profile="$1"
  local -a options=(--directory "${EVIDENCE_DIR}" --profile "${profile}"
    --cluster "${KIND_CLUSTER}" --network "evpn33b3-${KIND_CLUSTER}")
  if [[ "${EVPN_FRR_INTEROP_TEST:-0}" == 1 || "${profile}" == framework-* ]]; then
    options+=(--frr "${KIND_CLUSTER}-frr")
  fi
  PYTHONDONTWRITEBYTECODE=1 python3 "${ROOT}/tools/e2e/containerlab.py" up "${options[@]}"
}

e2e_lab_down() {
  [[ -f "${EVIDENCE_DIR}/lab.json" ]] || return 0
  PYTHONDONTWRITEBYTECODE=1 python3 "${ROOT}/tools/e2e/containerlab.py" down --directory "${EVIDENCE_DIR}"
}
