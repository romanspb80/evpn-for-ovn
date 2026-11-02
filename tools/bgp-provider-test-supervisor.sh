#!/usr/bin/env bash
set -euo pipefail

# Test-only process isolation: the released entrypoint intentionally couples
# manager and speaker termination. Do not use this as a deployment entrypoint.
start_speaker() {
  /usr/local/bin/gobgpd --api-hosts "unix://${GOBGP_SOCKET}" \
    --metrics-host "${NODE_IP}:7475" --pprof-disable &
  SPEAKER=$!
  echo "${SPEAKER}" > /tmp/speaker.pid
}
start_manager() {
  /usr/local/bin/manager --gobgp-endpoint="unix://${GOBGP_SOCKET}" &
  MANAGER=$!
  echo "${MANAGER}" > /tmp/manager.pid
}
stop_process() {
  local pid="$1" status=0
  kill -TERM "${pid}"
  wait "${pid}" || status=$?
  [[ "${status}" == 0 || "${status}" == 143 ]] || {
    echo "unexpected process exit ${pid}: ${status}" >&2
    exit 1
  }
}
cleanup() {
  kill -TERM "${SPEAKER}" "${MANAGER}"
  wait
}
trap cleanup EXIT
trap 'exit 0' TERM INT
start_speaker
start_manager
while sleep 1; do
  if [[ -f /tmp/restart-manager ]]; then
    rm /tmp/restart-manager
    stop_process "${MANAGER}"
    start_manager
    touch /tmp/manager-restarted
  fi
  if [[ -f /tmp/restart-speaker ]]; then
    rm /tmp/restart-speaker
    stop_process "${SPEAKER}"
    start_speaker
    touch /tmp/speaker-restarted
  fi
  kill -0 "${SPEAKER}" "${MANAGER}"
done
