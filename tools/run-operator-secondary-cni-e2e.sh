#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="${ROOT}/tools/e2e${PYTHONPATH:+:${PYTHONPATH}}" \
  python3 tools/e2e/operator_secondary_cni.py "$@"
