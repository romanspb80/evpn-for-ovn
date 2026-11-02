#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${REPO_ROOT}/services/ovsdb_mgmt/ovnnb"
SCHEMA_PATH="${1:-${OVN_SCHEMADIR:-/tmp/ovn-26.09.0}/ovn-nb.ovsschema}"

if [[ ! -f "${SCHEMA_PATH}" ]]; then
  echo "OVN NB schema not found: ${SCHEMA_PATH}" >&2
  echo "Download OVN v26.09.0 and set OVN_SCHEMADIR or pass the schema path explicitly." >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

go run github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p ovnnb -o "${TMPDIR}" "${SCHEMA_PATH}"

find "${MODEL_DIR}" -maxdepth 1 -type f -name '*.go' -delete
cp "${TMPDIR}"/*.go "${MODEL_DIR}/"

echo "Regenerated OVN NB models in ${MODEL_DIR} from ${SCHEMA_PATH}"
