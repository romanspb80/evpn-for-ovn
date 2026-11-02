#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${ROOT}/deploy/containerlab/.lab-tls"
regenerate=0
dns_names=(localhost clab-evpn-kind-frr-frr kind0-control-plane kind0-worker)
ip_addresses=(127.0.0.1 ::1)
fail() { printf 'lab TLS: %s\n' "$*" >&2; exit 1; }
while (($#)); do
  case "$1" in
    --output-dir|--dns-name|--ip-address)
      (($# >= 2)) && [[ -n "$2" ]] || fail "$1 requires a value"
      case "$1" in
        --output-dir) output="$2" ;;
        --dns-name) dns_names+=("$2") ;;
        --ip-address) ip_addresses+=("$2") ;;
      esac
      shift 2 ;;
    --regenerate) regenerate=1; shift ;;
    --help)
      echo "Usage: $0 [--output-dir DIR] [--dns-name NAME] [--ip-address IP] [--regenerate]"
      echo "Reuses a valid local bundle; --regenerate rotates the CA and both leaf certificates."
      exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
command -v openssl >/dev/null || fail "OpenSSL is required"
for name in "${dns_names[@]}"; do
  [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || fail "invalid DNS SAN"
done
for address in "${ip_addresses[@]}"; do
  [[ "$address" =~ ^[0-9a-fA-F:.]+$ ]] || fail "invalid IP SAN"
done
[[ ! -L "$output" ]] || fail "output directory must not be a symlink"
mkdir -p -- "$output"
output="$(cd "$output" && pwd)"
chmod 700 "$output"
mkdir "$output/.generate.lock" 2>/dev/null || fail "generation lock exists; check for another generator"
temp=""
cleanup() {
  if [[ -n "$temp" ]]; then
    rm -f -- "$temp/ca.key" "$temp/ca.pem" "$temp/server.key" "$temp/server.pem" \
      "$temp/client.key" "$temp/client.pem" "$temp/server.csr" "$temp/client.csr" \
      "$temp/server.ext" "$temp/client.ext"
    rmdir -- "$temp"
  fi
  rmdir -- "$output/.generate.lock"
}
trap cleanup EXIT
files=(ca.key ca.pem server.key server.pem client.key client.pem)
present=0
for file in "${files[@]}"; do
  [[ ! -L "$output/$file" ]] || fail "refusing symlink: $file"
  if [[ -e "$output/$file" ]]; then
    [[ -f "$output/$file" ]] || fail "not a regular file: $file"
    present=$((present + 1))
  fi
done

public_key_hash() { openssl pkey -pubin -outform DER | openssl dgst -sha256; }
validate() {
  local dir="$1" role certificate_key private_key ca_key
  ca_key="$(openssl x509 -in "$dir/ca.pem" -pubkey -noout | public_key_hash)" || return 1
  # Public-key hash of the compromised historical CA, not private material.
  [[ "$ca_key" != *a9df097ef2199f05c4cebe58ddedadb2053954e7e3f891739c8b8bf424a37c7f ]] || return 1
  openssl x509 -in "$dir/ca.pem" -noout -ext basicConstraints | grep -q 'CA:TRUE' || return 1
  openssl verify -check_ss_sig -CAfile "$dir/ca.pem" "$dir/ca.pem" >/dev/null || return 1
  for role in ca server client; do
    openssl x509 -in "$dir/$role.pem" -checkend 86400 -noout >/dev/null || return 1
    certificate_key="$(openssl x509 -in "$dir/$role.pem" -pubkey -noout | public_key_hash)" || return 1
    private_key="$(openssl pkey -in "$dir/$role.key" -passin pass: -pubout | public_key_hash)" || return 1
    [[ "$certificate_key" == "$private_key" ]] || return 1
  done
  openssl verify -purpose sslserver -CAfile "$dir/ca.pem" "$dir/server.pem" >/dev/null || return 1
  openssl verify -purpose sslclient -CAfile "$dir/ca.pem" "$dir/client.pem" >/dev/null || return 1
  openssl x509 -in "$dir/server.pem" -noout -ext extendedKeyUsage | grep -q 'TLS Web Server Authentication' || return 1
  openssl x509 -in "$dir/client.pem" -noout -ext extendedKeyUsage | grep -q 'TLS Web Client Authentication' || return 1
  openssl x509 -in "$dir/client.pem" -noout -ext subjectAltName | grep -q 'URI:urn:evpn-for-ovn:lab-client' || return 1
  for name in "${dns_names[@]}"; do
    openssl verify -CAfile "$dir/ca.pem" -verify_hostname "$name" "$dir/server.pem" >/dev/null || return 1
  done
  for address in "${ip_addresses[@]}"; do
    openssl verify -CAfile "$dir/ca.pem" -verify_ip "$address" "$dir/server.pem" >/dev/null || return 1
  done
}

if ((present && !regenerate)); then
  ((present == ${#files[@]})) || fail "incomplete bundle; use --regenerate after reviewing existing files"
  validate "$output" || fail "invalid, expiring, mismatched or compromised bundle; use --regenerate"
  chmod 600 "$output/ca.key" "$output/server.key" "$output/client.key"
  echo "lab TLS: existing bundle verified (unchanged)"
  exit 0
fi

temp="$(mktemp -d "$output/.generate.XXXXXXXX")"
openssl genrsa -out "$temp/ca.key" 3072
openssl req -new -x509 -sha256 -days 365 -key "$temp/ca.key" \
  -subj '/CN=EVPN disposable lab CA' -out "$temp/ca.pem" \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
for role in server client; do
  openssl genrsa -out "$temp/$role.key" 2048
  openssl req -new -sha256 -key "$temp/$role.key" \
    -subj "/CN=EVPN lab $role" -out "$temp/$role.csr"
  {
    echo 'basicConstraints=critical,CA:FALSE'
    echo 'keyUsage=critical,digitalSignature,keyEncipherment'
    if [[ "$role" == server ]]; then
      echo 'extendedKeyUsage=serverAuth'
      echo 'subjectAltName=@sans'
      echo '[sans]'
      index=0
      for name in "${dns_names[@]}"; do
        index=$((index + 1)); printf 'DNS.%s=%s\n' "$index" "$name"
      done
      index=0
      for address in "${ip_addresses[@]}"; do
        index=$((index + 1)); printf 'IP.%s=%s\n' "$index" "$address"
      done
    else
      echo 'extendedKeyUsage=clientAuth'
      echo 'subjectAltName=URI:urn:evpn-for-ovn:lab-client'
    fi
  } >"$temp/$role.ext"
  openssl x509 -req -sha256 -days 30 -in "$temp/$role.csr" \
    -CA "$temp/ca.pem" -CAkey "$temp/ca.key" -set_serial "0x$(openssl rand -hex 16)" \
    -extfile "$temp/$role.ext" -out "$temp/$role.pem"
done
validate "$temp" || fail "generated certificate validation failed"
for file in "${files[@]}"; do
  mv -f -- "$temp/$file" "$output/$file"
done
chmod 600 "$output/ca.key" "$output/server.key" "$output/client.key"
echo "lab TLS: fresh CA and server/client certificates generated"
