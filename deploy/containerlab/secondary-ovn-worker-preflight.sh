#!/bin/sh
set -eu

: "${OVS_RUN_DIR:?missing isolated OVS runtime directory}"
: "${OVS_DB_DIR:?missing isolated OVS database directory}"
: "${OVN_RUN_DIR:?missing isolated OVN runtime directory}"
: "${IDENTITY_DIR:?missing isolated ownership directory}"

PS="${PS:-ps}"
IP="${IP:-ip}"
SYS_CLASS_NET_ROOT="${SYS_CLASS_NET_ROOT:-/sys/class/net}"
PROC_ROOT="${PROC_ROOT:-/proc}"
OVS_MODULE_DIR="${OVS_MODULE_DIR:-/sys/module/openvswitch}"

for directory in "$OVS_RUN_DIR" "$OVS_DB_DIR" "$OVN_RUN_DIR" "$IDENTITY_DIR"; do
  [ -d "$directory" ] || {
    echo "isolated runtime directory is missing: $directory" >&2
    exit 1
  }
  contents="$(find "$directory" -mindepth 1 -maxdepth 1 -print -quit)" || {
    echo "cannot inspect isolated runtime directory: $directory" >&2
    exit 1
  }
  if [ -n "$contents" ]; then
    echo "isolated runtime directory is not empty: $directory" >&2
    exit 1
  fi
done

self_netns="$(readlink "$PROC_ROOT/self/ns/net")"
init_netns="$(readlink "$PROC_ROOT/1/ns/net")"
[ -n "$self_netns" ] && [ "$self_netns" = "$init_netns" ] || {
  echo "ambiguous worker network namespace ownership" >&2
  exit 1
}

processes="$("$PS" -eo pid=,comm=)" || {
  echo "cannot inspect processes in the worker PID namespace" >&2
  exit 1
}
conflicts="$(printf '%s\n' "$processes" | awk '
  $2 == "ovsdb-server" || $2 == "ovs-vswitchd" || $2 == "ovn-controller" {
    print $1, $2
  }
')" || {
  echo "cannot classify processes in the worker PID namespace" >&2
  exit 1
}
if [ -n "$conflicts" ]; then
  printf '%s\n' "$conflicts" | while read -r pid name; do
    case "$pid" in
      ''|*[!0-9]*) echo "conflicting process has invalid PID: $pid" >&2; continue ;;
    esac
    echo "conflicting process pid=$pid name=$name" >&2
    for kind in pid net; do
      value="$(readlink "$PROC_ROOT/$pid/ns/$kind" 2>/dev/null)" || value="unavailable"
      echo "  $kind namespace=$value" >&2
    done
    executable="$(readlink "$PROC_ROOT/$pid/exe" 2>/dev/null)" || executable="unavailable"
    echo "  executable=$executable" >&2
    if [ -r "$PROC_ROOT/$pid/stat" ]; then
      stat="$(cat "$PROC_ROOT/$pid/stat")" || stat=""
      fields="${stat##*) }"
      parent="$(printf '%s\n' "$fields" | awk '{print $2}')"
      start="$(printf '%s\n' "$fields" | awk '{print $20}')"
      echo "  parent=${parent:-unavailable} start_ticks=${start:-unavailable}" >&2
    else
      echo "  parent=unavailable start_ticks=unavailable" >&2
    fi
    if [ -r "$PROC_ROOT/$pid/cmdline" ]; then
      command="$(tr '\000' ' ' < "$PROC_ROOT/$pid/cmdline")" || command=""
      echo "  command=${command:-unavailable}" >&2
    else
      echo "  command=unavailable" >&2
    fi
    if [ -r "$PROC_ROOT/$pid/cgroup" ]; then
      echo "  cgroup:" >&2
      cat "$PROC_ROOT/$pid/cgroup" >&2 || echo "  unavailable" >&2
    else
      echo "  cgroup=unavailable" >&2
    fi
  done
  echo "conflicting OVS/OVN process exists in the worker PID namespace" >&2
  exit 1
fi

for interface in br-int ovs-system; do
  [ ! -e "$SYS_CLASS_NET_ROOT/$interface" ] || {
    echo "conflicting OVS interface exists: $interface" >&2
    exit 1
  }
done
links="$("$IP" -o -d link show)" || {
  echo "cannot inspect links in the worker network namespace" >&2
  exit 1
}
if printf '%s\n' "$links" | grep -Eiq '(^|[ :])br-int([ :@]|$)|ovs-system|openvswitch'; then
  echo "conflicting OVS datapath or bridge exists in the worker network namespace" >&2
  exit 1
fi

[ -d "$OVS_MODULE_DIR" ] || {
  echo "Open vSwitch kernel module is not loaded; refusing unsafe module manipulation" >&2
  exit 1
}
