#!/bin/sh
set -eu

: "${SECONDARY_OWNER:?missing run ownership identity}"
: "${OVN_REMOTE:?missing Southbound endpoint}"
: "${OVN_ENCAP_IP:?missing worker encapsulation address}"

OVS_RUN_DIR="${OVS_RUN_DIR:-/var/run/openvswitch}"
OVS_DB_DIR="${OVS_DB_DIR:-/etc/openvswitch}"
OVN_RUN_DIR="${OVN_RUN_DIR:-/var/run/ovn}"
IDENTITY_DIR="${IDENTITY_DIR:-/run/evpn-secondary/identity}"
PROC_ROOT="${PROC_ROOT:-/proc}"
PS="${PS:-ps}"
OVS_CTL="${OVS_CTL:-/usr/share/openvswitch/scripts/ovs-ctl}"
OVS_VSWITCHD="${OVS_VSWITCHD:-ovs-vswitchd}"
OVN_CTL="${OVN_CTL:-/usr/share/ovn/scripts/ovn-ctl}"
OVS_VSCTL="${OVS_VSCTL:-ovs-vsctl}"
PREFLIGHT_SCRIPT="${PREFLIGHT_SCRIPT:-/lab/worker-preflight.sh}"
OWNER_RECEIPT="$IDENTITY_DIR/worker-owner"

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
  process_name="$1"
  pid_file="$2"
  expected_pid_file="$3"
  recorded="$4"
  current="$(process_identity "$process_name" "$pid_file" "$expected_pid_file")" || return 1
  [ "$current" = "$recorded" ]
}

receipt_value() {
  awk -F= -v key="$1" '$1 == key {sub(/^[^=]*=/, ""); print; found=1} END {if (!found) exit 1}' \
    "$OWNER_RECEIPT"
}

cleanup() {
  [ -f "$OWNER_RECEIPT" ] || {
    echo "secondary OVS ownership receipt is missing; preserving runtime" >&2
    return 1
  }
  [ "$(receipt_value owner 2>/dev/null || true)" = "$SECONDARY_OWNER" ] || {
    echo "secondary OVS ownership receipt is stale or foreign; preserving runtime" >&2
    return 1
  }
  controller_identity="$(receipt_value controller 2>/dev/null || true)"
  ovsdb_identity="$(receipt_value ovsdb 2>/dev/null || true)"
  vswitchd_identity="$(receipt_value vswitchd 2>/dev/null || true)"
  ownership_uncertain=0
  if [ -n "$controller_identity" ]; then
    if verify_process_identity ovn-controller "$OVN_RUN_DIR/ovn-controller.pid" \
        /var/run/ovn/ovn-controller.pid "$controller_identity"; then
      "$OVN_CTL" stop_controller || {
        echo "owned ovn-controller stop failed" >&2
        ownership_uncertain=1
      }
      [ ! -e "$OVN_RUN_DIR/ovn-controller.pid" ] || ownership_uncertain=1
    else
      echo "ovn-controller identity changed; preserving runtime" >&2
      ownership_uncertain=1
    fi
  elif [ -e "$OVN_RUN_DIR/ovn-controller.pid" ]; then
    echo "unreceipted ovn-controller PID file; preserving runtime" >&2
    ownership_uncertain=1
  fi
  if ! processes="$("$PS" -eo pid=,comm=)"; then
    echo "cannot inspect processes during scoped OVS cleanup" >&2
    ownership_uncertain=1
  fi
  if printf '%s\n' "${processes:-}" | awk '$2 == "ovn-controller" { found = 1 }
      END { exit !found }'; then
    echo "ovn-controller remains after scoped cleanup; preserving OVS runtime" >&2
    ownership_uncertain=1
  fi
  if [ "$ownership_uncertain" -ne 0 ]; then
    return 1
  fi
  if [ -n "$ovsdb_identity" ] && [ -n "$vswitchd_identity" ] &&
      verify_process_identity ovsdb-server "$OVS_RUN_DIR/ovsdb-server.pid" \
        /var/run/openvswitch/ovsdb-server.pid "$ovsdb_identity" &&
      verify_process_identity ovs-vswitchd "$OVS_RUN_DIR/ovs-vswitchd.pid" \
        /var/run/openvswitch/ovs-vswitchd.pid "$vswitchd_identity"; then
    "$OVS_CTL" stop || {
      echo "owned OVS stop failed" >&2
      return 1
    }
  else
    echo "OVS process identity is incomplete or changed; preserving runtime" >&2
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
    printf 'phase=%s\n' "$1"
    [ -z "${2:-}" ] || printf 'ovsdb=%s\n' "$2"
    [ -z "${3:-}" ] || printf 'vswitchd=%s\n' "$3"
    [ -z "${4:-}" ] || printf 'controller=%s\n' "$4"
  } > "$receipt_tmp"
  mv "$receipt_tmp" "$OWNER_RECEIPT"
}

OVS_RUN_DIR="$OVS_RUN_DIR" OVS_DB_DIR="$OVS_DB_DIR" \
  OVN_RUN_DIR="$OVN_RUN_DIR" IDENTITY_DIR="$IDENTITY_DIR" \
  /bin/sh "$PREFLIGHT_SCRIPT"
write_receipt starting
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

if ! "$OVS_CTL" --no-ovs-vswitchd --system-id=random start; then
  echo "OVS startup failed; exact lab teardown is required" >&2
  exit 1
fi
if ! "$OVS_VSWITCHD" "unix:$OVS_RUN_DIR/db.sock" \
    --pidfile=/var/run/openvswitch/ovs-vswitchd.pid --detach --monitor \
    --log-file=/var/log/openvswitch/ovs-vswitchd.log; then
  echo "ovs-vswitchd startup failed; exact lab teardown is required" >&2
  exit 1
fi
ovsdb_identity="$(process_identity ovsdb-server "$OVS_RUN_DIR/ovsdb-server.pid" \
  /var/run/openvswitch/ovsdb-server.pid)" || {
  echo "cannot establish ovsdb-server process identity" >&2
  exit 1
}
vswitchd_identity="$(process_identity ovs-vswitchd "$OVS_RUN_DIR/ovs-vswitchd.pid" \
  /var/run/openvswitch/ovs-vswitchd.pid)" || {
  echo "cannot establish ovs-vswitchd process identity" >&2
  exit 1
}
write_receipt ovs-running "$ovsdb_identity" "$vswitchd_identity"

bridge_list="$("$OVS_VSCTL" list-br)"
for bridge in $bridge_list; do
  if [ "$bridge" = br-int ]; then
    echo "br-int appeared after read-only preflight; refusing attachment" >&2
    exit 1
  fi
done
"$OVS_VSCTL" add-br br-int
ip link set dev br-int alias "$SECONDARY_OWNER"
"$OVS_VSCTL" set Bridge br-int "external_ids:evpn.e2e.owner=$SECONDARY_OWNER"
ip link set dev br-int up
"$OVS_VSCTL" set Open_vSwitch . \
  "external_ids:ovn-remote=$OVN_REMOTE" \
  external_ids:ovn-encap-type=geneve \
  "external_ids:ovn-encap-ip=$OVN_ENCAP_IP"
"$OVN_CTL" start_controller
controller_identity="$(process_identity ovn-controller "$OVN_RUN_DIR/ovn-controller.pid" \
  /var/run/ovn/ovn-controller.pid)" || {
  echo "cannot establish ovn-controller process identity" >&2
  exit 1
}
write_receipt ready "$ovsdb_identity" "$vswitchd_identity" "$controller_identity"

sleep infinity &
wait "$!"
