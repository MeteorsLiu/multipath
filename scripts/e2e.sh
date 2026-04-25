#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "real e2e requires Linux network namespaces"
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="${MULTIPATH_REAL_E2E_WORKDIR:-$(mktemp -d)}"
BIN="${MULTIPATH_REAL_E2E_BIN:-${WORKDIR}/multipath}"
PREBUILT_BIN="${MULTIPATH_REAL_E2E_PREBUILT_BIN:-0}"
REAL_E2E_DEBUG="${MULTIPATH_REAL_E2E_DEBUG:-1}"
FEC_PING_COUNT="${MULTIPATH_REAL_E2E_FEC_PING_COUNT:-1000}"
FEC_PING_INTERVAL="${MULTIPATH_REAL_E2E_FEC_PING_INTERVAL:-0.02}"

require_command() {
  local cmd="$1"
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}"
    exit 1
  fi
}

if [[ ${EUID:-0} -ne 0 ]]; then
  require_command go
  require_command sudo
  (cd "${ROOT_DIR}" && go build -o "${BIN}" .)
  exec sudo -E env \
    MULTIPATH_REAL_E2E_WORKDIR="${WORKDIR}" \
    MULTIPATH_REAL_E2E_BIN="${BIN}" \
    MULTIPATH_REAL_E2E_PREBUILT_BIN=1 \
    MULTIPATH_REAL_E2E_DEBUG="${REAL_E2E_DEBUG}" \
    MULTIPATH_REAL_E2E_FEC_PING_COUNT="${FEC_PING_COUNT}" \
    MULTIPATH_REAL_E2E_FEC_PING_INTERVAL="${FEC_PING_INTERVAL}" \
    bash "$0" "$@"
fi

echo "real e2e workdir: ${WORKDIR}"

SUFFIX="$$"
NS_C="mp_c_${SUFFIX}"
NS_S="mp_s_${SUFFIX}"

VETHC1="mpc1_${SUFFIX}"
VETHS1="mps1_${SUFFIX}"
VETHC2="mpc2_${SUFFIX}"
VETHS2="mps2_${SUFFIX}"

PORT_MULTIPATH=5001
PORT_LEGACY=5002
PORT_FALLBACK=5003
PORT_FEC=5004

PATH1_C="10.201.1.1/24"
PATH1_S="10.201.1.2/24"
PATH2_C="10.201.2.1/24"
PATH2_S="10.201.2.2/24"
PATH1_REMOTE="10.201.1.2"
PATH2_REMOTE="10.201.2.2"

TUN_C_LOCAL="172.31.0.1"
TUN_S_LOCAL="172.31.0.2"
TUN_C_REMOTE="${TUN_S_LOCAL}"
TUN_S_REMOTE="${TUN_C_LOCAL}"

FAIL_COUNT=0
PING_SAMPLE_LOSS=""
FEC_CASE_LOSS=""
CLIENT_PID=""
SERVER_PID=""
CURRENT_LOG_FILE=""

cleanup() {
  set +e
  if declare -F stop_multipath >/dev/null 2>&1; then
    stop_multipath
  fi
  if declare -F clear_loss >/dev/null 2>&1; then
    clear_loss
  fi
  ip netns del "${NS_C}" >/dev/null 2>&1 || true
  ip netns del "${NS_S}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

build_bin() {
  require_command ip
  require_command tc
  require_command ping

  if [[ "${PREBUILT_BIN}" == "1" ]]; then
    if [[ ! -x "${BIN}" ]]; then
      echo "prebuilt multipath binary missing or not executable: ${BIN}"
      exit 1
    fi
    return 0
  fi

  require_command go
  (cd "${ROOT_DIR}" && go build -o "${BIN}" .)
}

setup_netns() {
  ip netns del "${NS_C}" >/dev/null 2>&1 || true
  ip netns del "${NS_S}" >/dev/null 2>&1 || true

  ip netns add "${NS_C}"
  ip netns add "${NS_S}"

  ip link add "${VETHC1}" type veth peer name "${VETHS1}"
  ip link add "${VETHC2}" type veth peer name "${VETHS2}"

  ip link set "${VETHC1}" netns "${NS_C}"
  ip link set "${VETHS1}" netns "${NS_S}"
  ip link set "${VETHC2}" netns "${NS_C}"
  ip link set "${VETHS2}" netns "${NS_S}"

  ip netns exec "${NS_C}" ip addr add "${PATH1_C}" dev "${VETHC1}"
  ip netns exec "${NS_S}" ip addr add "${PATH1_S}" dev "${VETHS1}"
  ip netns exec "${NS_C}" ip addr add "${PATH2_C}" dev "${VETHC2}"
  ip netns exec "${NS_S}" ip addr add "${PATH2_S}" dev "${VETHS2}"

  ip netns exec "${NS_C}" ip link set lo up
  ip netns exec "${NS_S}" ip link set lo up
  ip netns exec "${NS_C}" ip link set "${VETHC1}" up
  ip netns exec "${NS_C}" ip link set "${VETHC2}" up
  ip netns exec "${NS_S}" ip link set "${VETHS1}" up
  ip netns exec "${NS_S}" ip link set "${VETHS2}" up
}

write_two_lane_config() {
  local name="$1"
  local port="$2"
  local legacy_tcp_flag="$3"
  local fec_flag="$4"
  local probe_interval_ms="${5:-200}"
  local probe_timeout_ms="${6:-600}"

  cat >"${WORKDIR}/server-${name}.json" <<EOF
{
  "isServer": true,
  "tcp": ${legacy_tcp_flag},
  "server": { "listen": "0.0.0.0:${port}" },
  "tun": {
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": ${probe_interval_ms},
  "probeTimeoutMS": ${probe_timeout_ms}
}
EOF

  cat >"${WORKDIR}/client-${name}.json" <<EOF
{
  "tcp": ${legacy_tcp_flag},
  "client": {
    "remotePaths": [
      { "remoteAddr": "${PATH1_REMOTE}:${port}", "weight": 1 },
      { "remoteAddr": "${PATH2_REMOTE}:${port}", "weight": 1 }
    ]
  },
  "tun": {
    "localAddr": "${TUN_C_LOCAL}",
    "remoteAddr": "${TUN_C_REMOTE}",
    "allowedIPs": ["${TUN_C_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": ${probe_interval_ms},
  "probeTimeoutMS": ${probe_timeout_ms}
}
EOF
}

write_one_lane_config() {
  local name="$1"
  local port="$2"
  local legacy_tcp_flag="$3"
  local fec_flag="$4"
  local probe_interval_ms="${5:-200}"
  local probe_timeout_ms="${6:-600}"

  cat >"${WORKDIR}/server-${name}.json" <<EOF
{
  "isServer": true,
  "tcp": ${legacy_tcp_flag},
  "server": { "listen": "0.0.0.0:${port}" },
  "tun": {
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": ${probe_interval_ms},
  "probeTimeoutMS": ${probe_timeout_ms}
}
EOF

  cat >"${WORKDIR}/client-${name}.json" <<EOF
{
  "tcp": ${legacy_tcp_flag},
  "client": {
    "remotePaths": [
      { "remoteAddr": "${PATH1_REMOTE}:${port}", "weight": 1 }
    ]
  },
  "tun": {
    "localAddr": "${TUN_C_LOCAL}",
    "remoteAddr": "${TUN_C_REMOTE}",
    "allowedIPs": ["${TUN_C_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": ${probe_interval_ms},
  "probeTimeoutMS": ${probe_timeout_ms}
}
EOF
}

fail() {
  local label="$1"
  local message="$2"
  echo "[${label}] FAIL: ${message}"
  FAIL_COUNT=$((FAIL_COUNT + 1))
  dump_current_log
}

pass() {
  local label="$1"
  local message="$2"
  echo "[${label}] PASS: ${message}"
}

dump_current_log() {
  if [[ -n "${CURRENT_LOG_FILE}" && -f "${CURRENT_LOG_FILE}" ]]; then
    echo "---- ${CURRENT_LOG_FILE} tail ----"
    tail -n 80 "${CURRENT_LOG_FILE}" || true
    echo "---- end log tail ----"
  fi
}

check_multipath_alive() {
  local label="$1"
  if [[ -n "${SERVER_PID}" ]] && ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
    fail "${label}" "server process exited"
    SERVER_PID=""
    return 1
  fi
  if [[ -n "${CLIENT_PID}" ]] && ! kill -0 "${CLIENT_PID}" >/dev/null 2>&1; then
    fail "${label}" "client process exited"
    CLIENT_PID=""
    return 1
  fi
  return 0
}

ping_once() {
  ip netns exec "${NS_C}" ping -c 1 -W 1 "${TUN_C_REMOTE}" >/dev/null 2>&1
}

wait_ping_ok() {
  local label="$1"
  local timeout="${2:-12}"
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if ! check_multipath_alive "${label}"; then
      return 0
    fi
    if ping_once; then
      pass "${label}" "ping ok"
      return 0
    fi
    sleep 0.2
  done
  fail "${label}" "ping did not recover within ${timeout}s"
}

expect_ping_fail_for() {
  local label="$1"
  local duration="${2:-3}"
  local deadline=$((SECONDS + duration))
  while (( SECONDS < deadline )); do
    if ping_once; then
      fail "${label}" "ping unexpectedly succeeded"
      return 0
    fi
    sleep 0.2
  done
  pass "${label}" "ping failed for ${duration}s"
}

run_iperf_if_available() {
  local label="$1"
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${label}] iperf3 not found, skip throughput smoke"
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${label}] timeout not found, skip throughput smoke"
    return 0
  fi

  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >/dev/null 2>&1 &
  local iperf_server=$!
  sleep 1
  echo "[${label}] iperf3 over TUN"
  timeout 8s ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t 3 -i 1 || true
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true
}

start_multipath() {
  local name="$1"
  local server_config="${WORKDIR}/server-${name}.json"
  local client_config="${WORKDIR}/client-${name}.json"
  local log_file="${WORKDIR}/${name}.multipath.log"

  CURRENT_LOG_FILE="${log_file}"
  ip netns exec "${NS_S}" env MULTIPATH_DEBUG="${REAL_E2E_DEBUG}" "${BIN}" -config "${server_config}" >>"${log_file}" 2>&1 &
  SERVER_PID=$!
  ip netns exec "${NS_C}" env MULTIPATH_DEBUG="${REAL_E2E_DEBUG}" "${BIN}" -config "${client_config}" >>"${log_file}" 2>&1 &
  CLIENT_PID=$!

  echo "[${name}] multipath log: ${log_file} (MULTIPATH_DEBUG=${REAL_E2E_DEBUG})"
}

stop_multipath() {
  if [[ -n "${CLIENT_PID}" ]]; then
    kill "${CLIENT_PID}" >/dev/null 2>&1 || true
    wait "${CLIENT_PID}" >/dev/null 2>&1 || true
    CLIENT_PID=""
  fi
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
  CURRENT_LOG_FILE=""
}

clear_loss() {
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS2}" root >/dev/null 2>&1 || true
}

setup_prio_qdisc() {
  local ns="$1"
  local dev="$2"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" root handle 1: prio bands 4
}

add_loss_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local loss="$5"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem loss "${loss}"
}

add_port_filter() {
  local ns="$1"
  local dev="$2"
  local prio="$3"
  local proto="$4"
  local field="$5"
  local port="$6"
  local band="$7"
  local proto_num

  case "${proto}" in
  udp)
    proto_num=17
    ;;
  tcp)
    proto_num=6
    ;;
  *)
    echo "unsupported filter proto: ${proto}"
    exit 1
    ;;
  esac

  ip netns exec "${ns}" tc filter replace dev "${dev}" protocol ip parent 1:0 prio "${prio}" u32 \
    match ip protocol "${proto_num}" 0xff \
    match ip "${field}" "${port}" 0xffff \
    flowid "1:${band}"
}

apply_path_loss() {
  local path="$1"
  local loss="$2"
  case "${path}" in
  1)
    ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC1}" root netem loss "${loss}"
    ip netns exec "${NS_S}" tc qdisc replace dev "${VETHS1}" root netem loss "${loss}"
    ;;
  2)
    ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC2}" root netem loss "${loss}"
    ip netns exec "${NS_S}" tc qdisc replace dev "${VETHS2}" root netem loss "${loss}"
    ;;
  *)
    echo "unsupported path: ${path}"
    exit 1
    ;;
  esac
}

apply_tcp_client_block_all_paths() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHC1}"
  add_loss_band "${NS_C}" "${VETHC1}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHC1}" 1 tcp dport "${port}" 3

  setup_prio_qdisc "${NS_C}" "${VETHC2}"
  add_loss_band "${NS_C}" "${VETHC2}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHC2}" 1 tcp dport "${port}" 3
}

apply_udp_tunnel_block_path1() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHC1}"
  add_loss_band "${NS_C}" "${VETHC1}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHC1}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${VETHS1}"
  add_loss_band "${NS_S}" "${VETHS1}" 3 30 100%
  add_port_filter "${NS_S}" "${VETHS1}" 1 udp sport "${port}" 3
}

apply_tcp_tunnel_block_path1() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHC1}"
  add_loss_band "${NS_C}" "${VETHC1}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHC1}" 1 tcp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${VETHS1}"
  add_loss_band "${NS_S}" "${VETHS1}" 3 30 100%
  add_port_filter "${NS_S}" "${VETHS1}" 1 tcp sport "${port}" 3
}

apply_fec_loss() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHC1}"
  add_loss_band "${NS_C}" "${VETHC1}" 3 30 20%
  add_port_filter "${NS_C}" "${VETHC1}" 1 udp dport "${port}" 3
  add_loss_band "${NS_C}" "${VETHC1}" 4 40 100%
  add_port_filter "${NS_C}" "${VETHC1}" 2 tcp dport "${port}" 4
}

run_multipath_case() {
  local name="multipath"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_MULTIPATH}" false false
  expect_ping_fail_for "${name} precheck" 2
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  run_iperf_if_available "${name} baseline"

  echo "[${name}] drop path2 completely"
  apply_path_loss 2 100%
  wait_ping_ok "${name} path2-down" 15

  echo "[${name}] restore path2"
  clear_loss
  wait_ping_ok "${name} path2-restored" 12

  echo "[${name}] drop path1 completely"
  apply_path_loss 1 100%
  wait_ping_ok "${name} path1-down" 15

  echo "[${name}] restore path1"
  clear_loss
  wait_ping_ok "${name} path1-restored" 12

  echo "[${name}] drop both paths"
  apply_path_loss 1 100%
  apply_path_loss 2 100%
  expect_ping_fail_for "${name} both-down" 3

  echo "[${name}] recover both paths"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_legacy_tcp_flag_case() {
  local name="legacy-tcp-flag"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_LEGACY}" true false

  echo "[${name}] block client TCP dials; legacy tcp flag must still bootstrap over UDP"
  apply_tcp_client_block_all_paths "${PORT_LEGACY}"
  start_multipath "${name}"
  wait_ping_ok "${name} udp-bootstrap-with-tcp-blocked" 12

  echo "[${name}] drop path2 while TCP dials remain blocked"
  apply_path_loss 2 100%
  wait_ping_ok "${name} path2-down" 15

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_fallback_case() {
  local name="fallback"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_FALLBACK}" false false 200 600
  start_multipath "${name}"
  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP tunnel traffic; TCP fallback must carry the lane"
  apply_udp_tunnel_block_path1 "${PORT_FALLBACK}"
  wait_ping_ok "${name} tcp-fallback" 20

  echo "[${name}] restore UDP and block TCP; lane must recover to UDP"
  clear_loss
  apply_tcp_tunnel_block_path1 "${PORT_FALLBACK}"
  wait_ping_ok "${name} udp-recovered" 20

  clear_loss
  wait_ping_ok "${name} final-clean" 12
  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_ping_sample() {
  local label="$1"
  local count="${FEC_PING_COUNT}"
  local interval="${FEC_PING_INTERVAL}"
  local output
  echo "[${label}] ping sample: count=${count} interval=${interval}s"
  output="$(ip netns exec "${NS_C}" ping -c "${count}" -i "${interval}" -W 1 "${TUN_C_REMOTE}" 2>&1 || true)"
  echo "${output}" >"${WORKDIR}/${label}.ping.log"
  echo "[${label}] ping log: ${WORKDIR}/${label}.ping.log"
  printf '%s\n' "${output}" | tail -n 2

  local loss
  loss="$(printf '%s\n' "${output}" | sed -nE 's/.* ([0-9]+([.][0-9]+)?)% packet loss.*/\1/p' | tail -n 1)"
  if [[ -z "${loss}" ]]; then
    fail "${label}" "cannot parse packet loss"
    loss="100"
  fi
  PING_SAMPLE_LOSS="${loss}"
}

run_fec_case() {
  local label="$1"
  local fec_flag="$2"

  clear_loss
  write_one_lane_config "${label}" "${PORT_FEC}" false "${fec_flag}" 200 3000
  start_multipath "${label}"
  wait_ping_ok "${label} baseline" 12

  echo "[${label}] apply 20% UDP data loss and block TCP fallback"
  apply_fec_loss "${PORT_FEC}"
  sleep 1
  run_ping_sample "${label}-weak"

  clear_loss
  stop_multipath
  FEC_CASE_LOSS="${PING_SAMPLE_LOSS}"
}

run_fec_comparison() {
  echo "==== fec e2e start ===="

  local off_loss
  run_fec_case "fec-off" false
  off_loss="${FEC_CASE_LOSS}"
  sleep 1

  local on_loss
  run_fec_case "fec-on" true
  on_loss="${FEC_CASE_LOSS}"

  echo "[fec] comparison under 20% client-to-server UDP tunnel loss"
  echo "[fec] off packet_loss=${off_loss}%"
  echo "[fec] on  packet_loss=${on_loss}%"

  if awk -v off="${off_loss}" -v on="${on_loss}" 'BEGIN { exit !(on < off) }'; then
    pass "fec" "FEC reduced observed tunnel packet loss"
  else
    fail "fec" "FEC did not reduce observed tunnel packet loss"
  fi

  clear_loss
  echo "==== fec e2e end ===="
}

build_bin
setup_netns

run_multipath_case
run_legacy_tcp_flag_case
run_fallback_case
run_fec_comparison

if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "real e2e failed (${FAIL_COUNT} failures)"
  exit 1
fi

echo "real e2e ok"
