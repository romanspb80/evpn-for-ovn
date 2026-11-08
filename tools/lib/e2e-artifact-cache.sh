#!/usr/bin/env bash
# Fetches immutable, checksum-pinned E2E release artifacts into a local cache.

e2e_fetch_pinned_artifact() {
  local url="$1" sha256="$2" filename="$3" destination="$4"
  local cache_dir cache_path temporary actual_sha

  [[ "${sha256}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid pinned SHA-256 for ${filename}" >&2
    return 1
  }
  [[ "${url}" == https://* ]] || {
    echo "pinned artifact URL must use HTTPS: ${url}" >&2
    return 1
  }
  [[ -n "${filename}" && "${filename}" != */* ]] || {
    echo "invalid pinned artifact filename: ${filename}" >&2
    return 1
  }

  cache_dir="${EVPN_E2E_ARTIFACT_CACHE_DIR:-${HOME}/.cache/evpn-for-ovn/e2e-artifacts}"
  mkdir -p "${cache_dir}"
  cache_path="${cache_dir}/${filename}.${sha256}"

  if [[ -e "${cache_path}" || -L "${cache_path}" ]]; then
    [[ -f "${cache_path}" && ! -L "${cache_path}" ]] || {
      echo "cached artifact is not a regular file: ${cache_path}" >&2
      return 1
    }
    actual_sha="$(sha256sum "${cache_path}")"
    actual_sha="${actual_sha%% *}"
    [[ "${actual_sha}" == "${sha256}" ]] || {
      echo "cached artifact checksum mismatch: ${cache_path}" >&2
      return 1
    }
    echo "using checksum-verified cached artifact: ${cache_path}"
  else
    if [[ "${EVPN_E2E_ALLOW_DOWNLOAD:-1}" != 1 ]]; then
      echo "pinned artifact unavailable in cache and downloads are disabled: ${cache_path}" >&2
      return 1
    fi
    temporary="$(mktemp "${cache_dir}/.${filename}.download.XXXXXX")"
    if ! curl --fail --silent --show-error --location --retry 2 \
        --proto '=https' --tlsv1.2 "${url}" --output "${temporary}"; then
      rm -f "${temporary}"
      echo "unable to download pinned artifact ${filename} from ${url}" >&2
      return 1
    fi
    actual_sha="$(sha256sum "${temporary}")"
    actual_sha="${actual_sha%% *}"
    if [[ "${actual_sha}" != "${sha256}" ]]; then
      rm -f "${temporary}"
      echo "downloaded artifact checksum mismatch for ${filename}: expected ${sha256}, got ${actual_sha}" >&2
      return 1
    fi
    chmod 600 "${temporary}"
    if ! mv -- "${temporary}" "${cache_path}"; then
      rm -f "${temporary}"
      echo "unable to store pinned artifact in cache: ${cache_path}" >&2
      return 1
    fi
    echo "downloaded and checksum-verified pinned artifact: ${cache_path}"
  fi

  cp -- "${cache_path}" "${destination}"
  actual_sha="$(sha256sum "${destination}")"
  actual_sha="${actual_sha%% *}"
  [[ "${actual_sha}" == "${sha256}" ]] || {
    echo "copied artifact checksum mismatch: ${destination}" >&2
    return 1
  }
}
