#!/usr/bin/env bash

PINNED_AGENT_ID="sha256:cba1675ad8093310d116f5befb91949eba970b165626d3472a17c8e4e877ae82"
PINNED_AGENT_REF="evpn4ovn/node-agent-test:cni-24b-d22e-20261027200001-r1"
declare -A TEST_IMAGE_TAG_OWNER=()
declare -A TEST_IMAGE_TAG_ID=()
TEST_IMAGE_IDS_BEFORE=""
TEST_IMAGE_SNAPSHOT_READY=0

test_image_inspect() {
  local error
  if error="$(docker image inspect "$1" 2>&1)"; then
    printf '%s\n' "${error}"
  elif [[ "${error}" == *"No such image:"* ]]; then
    return 1
  else
    echo "image inspection failed: ${error}" >&2
    return 2
  fi
}

test_image_identity() {
  python3 -c 'import json,sys; i=json.load(sys.stdin)[0]; print(i["Id"], i["Os"]+"/"+i["Architecture"])'
}

test_image_pinned_preflight() {
  local metadata identity
  metadata="$(test_image_inspect "${AGENT_IMAGE}")" || {
    echo "required pinned agent image unavailable: ${AGENT_IMAGE}" >&2; return 1;
  }
  identity="$(test_image_identity <<<"${metadata}")" || return 1
  [[ "${identity}" == "${PINNED_AGENT_ID} linux/amd64" ]] || {
    echo "pinned agent identity mismatch: ${identity}" >&2; return 1;
  }
  TEST_IMAGE_TAG_OWNER["${AGENT_IMAGE}"]=external
  TEST_IMAGE_TAG_ID["${AGENT_IMAGE}"]="${PINNED_AGENT_ID}"
  echo "pinned-agent-preflight=PASS ${AGENT_IMAGE} ${identity}"
}

test_image_snapshot() {
  TEST_IMAGE_IDS_BEFORE="$(docker image ls --all --quiet --no-trunc)" || return 1
  TEST_IMAGE_SNAPSHOT_READY=1
}

test_image_require_new_tag() {
  local metadata rc
  if metadata="$(test_image_inspect "$1")"; then
    echo "refusing to overwrite pre-existing image reference: $1" >&2
    return 1
  else
    rc=$?
    [[ "${rc}" == 1 ]] || return "${rc}"
  fi
  TEST_IMAGE_TAG_OWNER["$1"]=pending
}

test_image_record_created() {
  local metadata identity id
  [[ "${TEST_IMAGE_SNAPSHOT_READY}" == 1 && "${TEST_IMAGE_TAG_OWNER[$1]:-}" == pending ]] || {
    echo "cannot establish created image ownership: $1" >&2; return 1;
  }
  metadata="$(test_image_inspect "$1")" || return 1
  identity="$(test_image_identity <<<"${metadata}")" || return 1
  id="${identity%% *}"
  TEST_IMAGE_TAG_ID["$1"]="${id}"
  if grep -Fxq "${id}" <<<"${TEST_IMAGE_IDS_BEFORE}"; then
    TEST_IMAGE_TAG_OWNER["$1"]=tag
  else
    TEST_IMAGE_TAG_OWNER["$1"]=image
  fi
}

test_image_ensure_base() {
  local ref="evpn4ovn/node-agent-test:cni-24b-base" metadata identity rc
  if metadata="$(test_image_inspect "${ref}")"; then
    identity="$(test_image_identity <<<"${metadata}")" || return 1
    [[ "${identity%% *}" == "${BASE_ID}" ]] || {
      echo "pre-existing base reference has unexpected target: ${identity}" >&2; return 1;
    }
    TEST_IMAGE_TAG_OWNER["${ref}"]=external
    TEST_IMAGE_TAG_ID["${ref}"]="${BASE_ID}"
  else
    rc=$?
    [[ "${rc}" == 1 ]] || return "${rc}"
    test_image_require_new_tag "${ref}" || return 1
    docker tag "${BASE_ID}" "${ref}" || return 1
    test_image_record_created "${ref}" || return 1
  fi
}

test_image_cleanup_one() {
  local ref="$1" owner="${TEST_IMAGE_TAG_OWNER[$1]:-unknown}" metadata identity rc other
  case "${owner}" in
    external|removed) return 0 ;;
    tag|image) ;;
    *) echo "image cleanup requires ownership review: ${ref} (${owner})" >&2; return 1 ;;
  esac
  if metadata="$(test_image_inspect "${ref}")"; then
    identity="$(test_image_identity <<<"${metadata}")" || return 1
    [[ "${identity%% *}" == "${TEST_IMAGE_TAG_ID[$ref]:-}" ]] || {
      echo "image cleanup target changed: ${ref} (${identity})" >&2; return 1;
    }
  else
    rc=$?
    [[ "${rc}" == 1 ]] || return "${rc}"
    TEST_IMAGE_TAG_OWNER["${ref}"]=removed
    return 0
  fi
  other="$(python3 -c 'import json,sys
i=json.load(sys.stdin)[0]
print(sum(r != sys.argv[1] for r in (i.get("RepoTags") or [])+(i.get("RepoDigests") or [])))
' "${ref}" <<<"${metadata}")" || return 1
  if [[ "${owner}" == tag && "${other}" == 0 ]]; then
    echo "refusing to remove last reference to pre-existing image: ${ref}" >&2
    return 1
  fi
  # Never remove by ID or force: external references must remain intact.
  docker image rm "${ref}" || return 1
  TEST_IMAGE_TAG_OWNER["${ref}"]=removed
}

test_image_cleanup() {
  local ref rc=0
  for ref in "${!TEST_IMAGE_TAG_OWNER[@]}"; do
    test_image_cleanup_one "${ref}" || rc=1
  done
  return "${rc}"
}

stage_pinned_cni_binary() (
  local destination="${1:-}" source="${2:-}" expected_size="${3:-}" expected_sha="${4:-}"
  local temp rc
  if [[ "$#" != 4 || -z "${destination}" || -z "${source}" ||
        ! "${expected_size}" =~ ^[0-9]+$ ||
        ! "${expected_sha}" =~ ^[a-f0-9]{64}$ ]]; then
    echo "pinned CNI staging failed at arguments: source=${source:-unknown} destination=${destination:-unknown}" >&2
    return 2
  fi
  if [[ -d "${destination}" || ( ( -e "${destination}" || -L "${destination}" ) &&
        ( ! -f "${destination}" || -L "${destination}" ) ) ]]; then
    echo "pinned CNI staging failed at destination check: source=${source} destination=${destination} is not a regular file path" >&2
    return 1
  fi

  temp="$(mktemp -- "${destination}.tmp.XXXXXXXX")" || {
    rc=$?
    echo "pinned CNI staging failed at temporary-file creation: source=${source} destination=${destination}" >&2
    return "${rc}"
  }
  trap 'if [[ -e "${temp}" || -L "${temp}" ]]; then
    rm -f -- "${temp}" || echo "pinned CNI staging temporary cleanup failed: ${temp}" >&2
  fi' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
  if ag cat "${source}" > "${temp}"; then
    :
  else
    rc=$?
    echo "pinned CNI staging failed at extraction: source=${source} destination=${destination}" >&2
    return "${rc}"
  fi

  verify_pinned_cni_file() {
    python3 - "$1" "${expected_size}" "${expected_sha}" <<'PY'
import hashlib
import os
import stat
import sys

path, expected_size, expected_sha = sys.argv[1:]
try:
    metadata = os.lstat(path)
    if not stat.S_ISREG(metadata.st_mode):
        raise ValueError("artifact is not a regular file")
    if metadata.st_size == 0:
        raise ValueError("artifact is empty")
    if metadata.st_size != int(expected_size):
        raise ValueError(f"size mismatch: got {metadata.st_size}, expected {expected_size}")
    digest = hashlib.sha256()
    with open(path, "rb") as artifact:
        header = artifact.read(20)
        if (len(header) < 20 or header[:4] != b"\x7fELF" or
                header[4] != 2 or header[5] != 1 or
                int.from_bytes(header[18:20], "little") != 62):
            raise ValueError("artifact is not an ELF64 little-endian x86-64 executable")
        artifact.seek(0)
        for chunk in iter(lambda: artifact.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != expected_sha:
        raise ValueError(f"SHA-256 mismatch: got {digest.hexdigest()}, expected {expected_sha}")
except (OSError, ValueError) as error:
    print(f"pinned CNI artifact validation failed: {error}", file=sys.stderr)
    raise SystemExit(1)
PY
  }
  if verify_pinned_cni_file "${temp}"; then
    :
  else
    rc=$?
    echo "pinned CNI staging failed at artifact validation: source=${source} destination=${destination}" >&2
    return "${rc}"
  fi

  if chmod 755 -- "${temp}" && [[ "$(stat -c '%a' -- "${temp}")" == 755 ]]; then
    :
  else
    rc=$?
    echo "pinned CNI staging failed at permissions: source=${source} destination=${destination}" >&2
    return "${rc:-1}"
  fi
  if mv -fT -- "${temp}" "${destination}"; then
    :
  else
    rc=$?
    echo "pinned CNI staging failed at publish: source=${source} destination=${destination}" >&2
    return "${rc}"
  fi
  if verify_pinned_cni_file "${destination}" &&
      [[ "$(stat -c '%a' -- "${destination}")" == 755 ]]; then
    :
  else
    rc=$?
    echo "pinned CNI staging failed at published-file verification: source=${source} destination=${destination}" >&2
    return "${rc:-1}"
  fi
)

stage_worker_cni_file() (
  local worker="$1" host="$2" destination="$3" size="$4" sha="$5" kind="$6"
  local script="${BASH_SOURCE[0]%/*}/cni-worker-artifact.sh" directory="" rc
  trap 'rc=$?; if [[ -n "${directory}" ]]; then
    docker exec -i "${worker}" sh -s -- cleanup "${destination}" "${kind}" "${size}" "${sha}" "${directory}" < "${script}" ||
      { echo "worker CNI temporary cleanup failed: ${directory}" >&2; [[ "${rc}" != 0 ]] || rc=1; }
  fi; exit "${rc}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if directory="$(docker exec -i "${worker}" sh -s -- prepare "${destination}" "${kind}" "${size}" "${sha}" < "${script}")"; then
    :
  else
    rc=$?
    echo "worker CNI staging failed at preparation: ${destination}" >&2
    return "${rc}"
  fi
  if [[ ! "${directory}" =~ ^/opt/cni/bin/\.evpn-cni-stage\.[a-zA-Z0-9]{8}$ ]]; then
    echo "worker CNI staging returned an unexpected temporary path: ${directory}" >&2
    directory=""
    return 1
  fi
  if docker cp "${host}" "${worker}:${directory}/artifact"; then
    :
  else
    rc=$?
    echo "worker CNI staging failed at copy: ${destination}" >&2
    return "${rc}"
  fi
  docker exec -i "${worker}" sh -s -- publish "${destination}" "${kind}" "${size}" "${sha}" "${directory}" < "${script}"
)

verify_worker_cni_version() {
  local worker="$1" evidence="$2" size="$3" sha="$4" rc wrapper_sha wrapper_size
  local script="${BASH_SOURCE[0]%/*}/cni-worker-artifact.sh"
  wrapper_size="$(stat -c %s -- "${evidence}/cni-wrapper")" || return $?
  wrapper_sha="$(sha256sum -- "${evidence}/cni-wrapper")" || return $?
  docker exec -i "${worker}" sh -s -- verify /opt/cni/bin/evpn-cni wrapper \
    "${wrapper_size}" "${wrapper_sha%% *}" < "${script}" \
    > "${evidence}/worker-cni-wrapper-before-version.txt" || return $?
  docker exec -i "${worker}" sh -s -- verify /opt/cni/bin/evpn-cni.real real "${size}" "${sha}" \
    < "${script}" > "${evidence}/worker-cni-before-version.txt" || return $?
  if docker exec -i "${worker}" env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
      CNI_COMMAND=VERSION CNI_PATH=/opt/cni/bin /opt/cni/bin/evpn-cni \
      < /dev/null > "${evidence}/cni-version.json" 2> "${evidence}/cni-version.stderr"; then
    rc=0
  else
    rc=$?
  fi
  printf '%s\n' "${rc}" > "${evidence}/cni-version.exit-code"
  cat "${evidence}/cni-version.stderr" >&2
  if [[ "${rc}" != 0 ]]; then
    echo "worker CNI VERSION failed: exit=${rc}" >&2
    return "${rc}"
  fi
  python3 - "${evidence}/cni-version.json" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as response:
        info = json.load(response)
    if not isinstance(info, dict) or set(info) != {"cniVersion", "supportedVersions"}:
        raise ValueError("expected VERSION object with cniVersion and supportedVersions")
    if info["cniVersion"] != "1.1.0" or info["supportedVersions"] != ["1.0.0"]:
        raise ValueError("unexpected VERSION or supported versions")
except (OSError, UnicodeError, ValueError) as error:
    print(f"worker CNI VERSION validation failed: {error}", file=sys.stderr)
    raise SystemExit(1)
PY
}
