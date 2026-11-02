#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date -u +%Y%m%dt%H%M%Sz)-$(python3 -c 'import secrets; print(secrets.token_hex(4))')"

export KIND_CLUSTER="${KIND_CLUSTER:-cni-24b-full-${RUN_ID}}"
export EVIDENCE_DIR="${EVIDENCE_DIR:-/tmp/cni-full-lifecycle-c4m-evidence/attempt-${RUN_ID}}"
export EVPN_FRR_INTEROP_TEST=1
export EVPN_IMPORT_TEST=1
export EVPN_FRR_INTEROP_FULL_LIFECYCLE=1

exec bash "${ROOT}/tools/run-cni-mvp-2.4b-multus.sh"
