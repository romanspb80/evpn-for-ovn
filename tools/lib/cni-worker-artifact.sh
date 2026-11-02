#!/bin/sh
set -eu
export LC_ALL=C

fail() { echo "worker CNI integrity: $*" >&2; exit 1; }
for tool in stat sha256sum od tr mktemp chmod mv rm rmdir readlink; do
  command -v "$tool" >/dev/null 2>&1 || fail "required utility unavailable: $tool"
done

action=$1 destination=$2 kind=$3 expected_size=$4 expected_sha=$5
case "$kind:$destination" in
  real:/opt/cni/bin/evpn-cni.real|wrapper:/opt/cni/bin/evpn-cni) ;;
  *) fail "unexpected destination or artifact kind: $kind:$destination" ;;
esac
case "$expected_size" in ''|*[!0-9]*) fail "invalid expected size" ;; esac
case "$expected_sha" in ''|*[!0-9a-f]*) fail "invalid expected SHA-256" ;; esac
[ "${#expected_sha}" = 64 ] || fail "invalid expected SHA-256 length"
[ "$(readlink -f /opt/cni/bin)" = /opt/cni/bin ] || fail "unexpected CNI directory resolution"

verify() {
  path=$1 executable=$2
  [ -f "$path" ] && [ ! -L "$path" ] || fail "not a regular non-symlink file: $path"
  [ "$(stat -c %s -- "$path")" = "$expected_size" ] || fail "size mismatch: $path"
  digest=$(sha256sum -- "$path") || fail "SHA-256 read failed: $path"
  [ "${digest%% *}" = "$expected_sha" ] || fail "SHA-256 mismatch: $path"
  if [ "$kind" = real ]; then
    header=$(od -An -tx1 -N20 -- "$path") || fail "ELF header read failed: $path"
    header=$(printf '%s' "$header" | tr -d ' \n')
    case "$header" in
      7f454c46020101??????????????????????3e00) ;;
      *) fail "not ELF64 little-endian x86-64: $path" ;;
    esac
  fi
  if [ "$executable" = yes ]; then
    [ -x "$path" ] && [ "$(stat -c %a -- "$path")" = 755 ] ||
      fail "not an executable mode-0755 file: $path"
  fi
  printf 'verified path=%s size=%s sha256=%s mode=%s kind=%s\n' \
    "$path" "$expected_size" "$expected_sha" "$(stat -c %a -- "$path")" "$kind"
}

if [ "$action" = prepare ]; then
  [ ! -e "$destination" ] && [ ! -L "$destination" ] ||
    fail "refusing existing destination of unknown ownership: $destination"
  mktemp -d /opt/cni/bin/.evpn-cni-stage.XXXXXXXX
  exit
fi
if [ "$action" = verify ]; then
  verify "$destination" yes
  exit
fi

directory=${6:-}
suffix=${directory#/opt/cni/bin/.evpn-cni-stage.}
case "$suffix" in ''|*[!a-zA-Z0-9]*) fail "invalid owned temporary directory" ;; esac
[ "${#suffix}" = 8 ] && [ "$directory" = "/opt/cni/bin/.evpn-cni-stage.$suffix" ] ||
  fail "invalid owned temporary directory"
[ -d "$directory" ] && [ ! -L "$directory" ] &&
  [ "$(readlink -f "$directory")" = "$directory" ] || fail "temporary directory changed"

case "$action" in
  publish)
    verify "$directory/artifact" no
    chmod 755 -- "$directory/artifact"
    verify "$directory/artifact" yes
    [ ! -e "$destination" ] && [ ! -L "$destination" ] ||
      fail "refusing existing destination of unknown ownership: $destination"
    [ "$(stat -c %d -- "$directory")" = "$(stat -c %d -- /opt/cni/bin)" ] ||
      fail "publication would cross filesystems"
    if mv -nT -- "$directory/artifact" "$destination"; then
      :
    else
      rc=$?
      echo "worker CNI integrity: atomic publication failed: $destination" >&2
      exit "$rc"
    fi
    [ ! -e "$directory/artifact" ] || fail "destination appeared during publication"
    verify "$destination" yes
    ;;
  cleanup)
    rm -f -- "$directory/artifact"
    rmdir -- "$directory"
    ;;
  *) fail "unknown action: $action" ;;
esac
