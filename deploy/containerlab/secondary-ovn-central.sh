#!/bin/sh
set -eu

: "${SECONDARY_OWNER:?missing run ownership identity}"

OVN_RUN_DIR="${OVN_RUN_DIR:-/var/run/ovn}"
OVN_DATA_DIR="${OVN_DATA_DIR:-/var/lib/ovn}"
PROC_ROOT="${PROC_ROOT:-/proc}"
OVN_CTL="${OVN_CTL:-/usr/share/ovn/scripts/ovn-ctl}"
OVN_NBCTL="${OVN_NBCTL:-ovn-nbctl}"
OVN_SBCTL="${OVN_SBCTL:-ovn-sbctl}"
PS="${PS:-ps}"
OWNER_RECEIPT="${OWNER_RECEIPT:-/tmp/evpn-secondary-central-owner}"

process_identity() {
  process_name="$1"
  pid_file="$2"
  expected_pid_file="$3"
  [ -s "$pid_file" ] || return 1
  process_pid="$(cat "$pid_file")"
  case "$process_pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ -r "$PROC_ROOT/$process_pid/comm" ] || return 1
  [ "$(cat "$PROC_ROOT/$process_pid/comm")" = "$process_name" ] || return 1
  process_cmdline="$(tr '\000' ' ' < "$PROC_ROOT/$process_pid/cmdline")"
  case "$process_cmdline" in
    *"$expected_pid_file"*) ;;
    *) return 1 ;;
  esac
  process_start="$(sed 's/^.*) //' "$PROC_ROOT/$process_pid/stat" | awk '{print $20}')"
  [ -n "$process_start" ] || return 1
  printf '%s:%s\n' "$process_pid" "$process_start"
}

verify_process_identity() {
  process_identity "$1" "$2" "$3" | grep -Fx "$4" >/dev/null
}

receipt_value() {
  awk -F= -v key="$1" '$1 == key {sub(/^[^=]*=/, ""); print; found=1} END {if (!found) exit 1}' \
    "$OWNER_RECEIPT"
}

cleanup() {
  [ -f "$OWNER_RECEIPT" ] || {
    echo "secondary OVN ownership receipt is missing; preserving runtime" >&2
    return 1
  }
  [ "$(receipt_value owner 2>/dev/null || true)" = "$SECONDARY_OWNER" ] || {
    echo "secondary OVN ownership receipt is stale or foreign; preserving runtime" >&2
    return 1
  }
  nb_identity="$(receipt_value nb 2>/dev/null || true)"
  sb_identity="$(receipt_value sb 2>/dev/null || true)"
  northd_identity="$(receipt_value northd 2>/dev/null || true)"
  if [ -n "$nb_identity" ] && [ -n "$sb_identity" ] && [ -n "$northd_identity" ] &&
      verify_process_identity ovsdb-server "$OVN_RUN_DIR/ovnnb_db.pid" \
        /var/run/ovn/ovnnb_db.pid "$nb_identity" &&
      verify_process_identity ovsdb-server "$OVN_RUN_DIR/ovnsb_db.pid" \
        /var/run/ovn/ovnsb_db.pid "$sb_identity" &&
      verify_process_identity ovn-northd "$OVN_RUN_DIR/ovn-northd.pid" \
        /var/run/ovn/ovn-northd.pid "$northd_identity"; then
    "$OVN_CTL" stop || {
      echo "owned secondary OVN stop failed" >&2
      return 1
    }
  else
    echo "secondary OVN process identity is incomplete or changed; preserving runtime" >&2
    return 1
  fi
}

finish() {
  status=$?
  trap - EXIT
  cleanup || status=1
  exit "$status"
}

write_receipt() {
  receipt_tmp="$OWNER_RECEIPT.tmp"
  {
    printf 'owner=%s\n' "$SECONDARY_OWNER"
    [ -z "${1:-}" ] || printf 'nb=%s\n' "$1"
    [ -z "${2:-}" ] || printf 'sb=%s\n' "$2"
    [ -z "${3:-}" ] || printf 'northd=%s\n' "$3"
  } > "$receipt_tmp"
  mv "$receipt_tmp" "$OWNER_RECEIPT"
}

[ -d "$OVN_RUN_DIR" ] || {
  echo "secondary OVN runtime directory is missing" >&2
  exit 1
}
[ -d "$OVN_DATA_DIR" ] || {
  echo "secondary OVN data directory is missing" >&2
  exit 1
}
[ ! -e "$OWNER_RECEIPT" ] && [ ! -e "$OWNER_RECEIPT.tmp" ] || {
  echo "stale secondary OVN ownership receipt; refusing startup" >&2
  exit 1
}
for path in \
  "$OVN_RUN_DIR/ovnnb_db.pid" "$OVN_RUN_DIR/ovnsb_db.pid" \
  "$OVN_RUN_DIR/ovn-northd.pid" "$OVN_RUN_DIR/ovnnb_db.sock" \
  "$OVN_RUN_DIR/ovnsb_db.sock" \
  "$OVN_DATA_DIR/ovnnb_db.db" "$OVN_DATA_DIR/ovnsb_db.db"; do
  [ ! -e "$path" ] || {
    echo "conflicting secondary OVN resource exists: $path" >&2
    exit 1
  }
done
for directory in "$OVN_RUN_DIR" "$OVN_DATA_DIR"; do
  contents="$(find "$directory" -mindepth 1 -maxdepth 1 -print -quit)" || {
    echo "cannot inspect secondary OVN directory: $directory" >&2
    exit 1
  }
  if [ -n "$contents" ]; then
    echo "secondary OVN directory is not empty: $directory" >&2
    exit 1
  fi
done
processes="$("$PS" -eo pid=,comm=)" || {
  echo "cannot inspect processes in the central Pod" >&2
  exit 1
}
if printf '%s\n' "$processes" | awk '
  $2 == "ovsdb-server" || $2 == "ovn-northd" ||
  $2 == "ovn-controller" || $2 == "ovs-vswitchd" { found = 1 }
  END { exit !found }
'; then
  echo "conflicting OVS/OVN process exists in the central Pod" >&2
  exit 1
fi

trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
start_status=0
"$OVN_CTL" --db-nb-create-insecure-remote=yes \
  --db-sb-create-insecure-remote=yes start_northd || start_status=$?
nb_identity="$(process_identity ovsdb-server "$OVN_RUN_DIR/ovnnb_db.pid" \
  /var/run/ovn/ovnnb_db.pid 2>/dev/null || true)"
sb_identity="$(process_identity ovsdb-server "$OVN_RUN_DIR/ovnsb_db.pid" \
  /var/run/ovn/ovnsb_db.pid 2>/dev/null || true)"
northd_identity="$(process_identity ovn-northd "$OVN_RUN_DIR/ovn-northd.pid" \
  /var/run/ovn/ovn-northd.pid 2>/dev/null || true)"
write_receipt "$nb_identity" "$sb_identity" "$northd_identity"
if [ "$start_status" -ne 0 ]; then
  echo "secondary OVN provisioning failed with status $start_status" >&2
  exit "$start_status"
fi
if [ -z "$nb_identity" ] || [ -z "$sb_identity" ] || [ -z "$northd_identity" ]; then
  echo "cannot establish exact secondary OVN process ownership" >&2
  exit 1
fi
"$OVN_NBCTL" set-connection ptcp:6641:0.0.0.0
"$OVN_SBCTL" set-connection ptcp:6642:0.0.0.0

sleep infinity &
wait "$!"
