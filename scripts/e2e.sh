#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "real e2e requires Linux network namespaces"
  exit 1
fi

if [[ ${EUID:-0} -ne 0 ]]; then
  exec sudo -E bash "$0" "$@"
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
BIN="${WORKDIR}/multipath"
echo "real e2e workdir: ${WORKDIR}"

SUFFIX="$$"
NS_C="mp_c_${SUFFIX}"
NS_S="mp_s_${SUFFIX}"

VETHC1="mpc1_${SUFFIX}"
VETHS1="mps1_${SUFFIX}"
VETHC2="mpc2_${SUFFIX}"
VETHS2="mps2_${SUFFIX}"

PORT_UDP=5001
PORT_TCP=5000
PORT_FEC=5002

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

cleanup() {
  set +e
  if [[ -n "${CLIENT_PID:-}" ]]; then
    kill "${CLIENT_PID}" >/dev/null 2>&1 || true
    wait "${CLIENT_PID}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
  fi

  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS2}" root >/dev/null 2>&1 || true

  ip netns del "${NS_C}" >/dev/null 2>&1 || true
  ip netns del "${NS_S}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

require_command() {
  local cmd="$1"
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}"
    exit 1
  fi
}

build_bin() {
  require_command go
  require_command ip
  require_command tc
  require_command ping

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

write_config() {
  local mode="$1"
  local port="$2"
  local tcp_flag="false"
  if [[ "${mode}" == "tcp" ]]; then
    tcp_flag="true"
  fi

  cat >"${WORKDIR}/server-${mode}.json" <<EOF
{
  "isServer": true,
  "tcp": ${tcp_flag},
  "server": { "listen": "0.0.0.0:${port}" },
  "tun": {
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
EOF

  cat >"${WORKDIR}/client-${mode}.json" <<EOF
{
  "tcp": ${tcp_flag},
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
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
EOF
}

write_fec_config() {
  local fec_flag="$1"
  local name="$2"

  cat >"${WORKDIR}/server-${name}.json" <<EOF
{
  "isServer": true,
  "server": { "listen": "0.0.0.0:${PORT_FEC}" },
  "tun": {
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
EOF

  cat >"${WORKDIR}/client-${name}.json" <<EOF
{
  "client": {
    "remotePaths": [
      { "remoteAddr": "${PATH1_REMOTE}:${PORT_FEC}", "weight": 1 }
    ]
  },
  "tun": {
    "localAddr": "${TUN_C_LOCAL}",
    "remoteAddr": "${TUN_C_REMOTE}",
    "allowedIPs": ["${TUN_C_REMOTE}/32"]
  },
  "fec": ${fec_flag},
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
EOF
}

ping_once() {
  ip netns exec "${NS_C}" ping -c 1 -W 1 "${TUN_C_REMOTE}" >/dev/null 2>&1
}

wait_ping_ok() {
  local label="$1"
  local deadline=$((SECONDS + 10))
  while (( SECONDS < deadline )); do
    if ping_once; then
      echo "[${label}] PASS: ping ok"
      return 0
    fi
    sleep 0.2
  done
  echo "[${label}] FAIL: ping did not recover"
  FAIL_COUNT=$((FAIL_COUNT + 1))
  return 0
}

expect_ping_fail() {
  local label="$1"
  if ping_once; then
    echo "[${label}] FAIL: ping unexpectedly succeeded"
    FAIL_COUNT=$((FAIL_COUNT + 1))
    return 0
  fi
  echo "[${label}] PASS: ping failed as expected"
}

run_iperf_if_available() {
  local label="$1"
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${label}] iperf3 not found, skip throughput smoke"
    return 0
  fi

  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >/dev/null 2>&1 &
  local iperf_server=$!
  sleep 1
  echo "[${label}] iperf3 over TUN"
  ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t 3 -i 1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true
}

start_multipath_configs() {
  local name="$1"
  local server_config="$2"
  local client_config="$3"
  local log_file="${WORKDIR}/${name}.multipath.log"

  ip netns exec "${NS_S}" "${BIN}" -config "${server_config}" >>"${log_file}" 2>&1 &
  SERVER_PID=$!
  ip netns exec "${NS_C}" "${BIN}" -config "${client_config}" >>"${log_file}" 2>&1 &
  CLIENT_PID=$!

  echo "[${name}] multipath log: ${log_file}"
}

start_multipath() {
  local mode="$1"
  start_multipath_configs "${mode}" "${WORKDIR}/server-${mode}.json" "${WORKDIR}/client-${mode}.json"
}

stop_multipath() {
  if [[ -n "${CLIENT_PID:-}" ]]; then
    kill "${CLIENT_PID}" >/dev/null 2>&1 || true
    wait "${CLIENT_PID}" >/dev/null 2>&1 || true
    CLIENT_PID=""
  fi
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
}

clear_loss() {
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS2}" root >/dev/null 2>&1 || true
}

run_mode() {
  local mode="$1"
  local port="$2"

  echo "==== ${mode} real e2e start ===="
  write_config "${mode}" "${port}"

  expect_ping_fail "${mode} precheck"

  start_multipath "${mode}"
  wait_ping_ok "${mode} baseline"
  run_iperf_if_available "${mode} baseline"

  echo "[${mode}] simulate path2 loss"
  ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC2}" root netem loss 100%
  sleep 1
  wait_ping_ok "${mode} path2-down"

  echo "[${mode}] restore path2"
  clear_loss
  sleep 1
  wait_ping_ok "${mode} restored"

  echo "[${mode}] simulate both paths down"
  ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC1}" root netem loss 100%
  ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC2}" root netem loss 100%
  ip netns exec "${NS_S}" tc qdisc replace dev "${VETHS1}" root netem loss 100%
  ip netns exec "${NS_S}" tc qdisc replace dev "${VETHS2}" root netem loss 100%
  sleep 1
  expect_ping_fail "${mode} both-down"

  echo "[${mode}] recover both paths"
  clear_loss
  sleep 2
  wait_ping_ok "${mode} post-recovery"
  run_iperf_if_available "${mode} post-recovery"

  stop_multipath
  clear_loss
  echo "==== ${mode} real e2e end ===="
}

run_ping_sample() {
  local label="$1"
  local count=160
  local interval=0.03
  local output
  output="$(ip netns exec "${NS_C}" ping -c "${count}" -i "${interval}" -W 1 "${TUN_C_REMOTE}" 2>&1 || true)"
  echo "${output}" >"${WORKDIR}/${label}.ping.log"
  echo "[${label}] ping log: ${WORKDIR}/${label}.ping.log"
  printf '%s\n' "${output}" | tail -n 2

  local loss
  loss="$(printf '%s\n' "${output}" | sed -nE 's/.* ([0-9]+([.][0-9]+)?)% packet loss.*/\1/p' | tail -n 1)"
  if [[ -z "${loss}" ]]; then
    echo "[${label}] FAIL: cannot parse packet loss"
    FAIL_COUNT=$((FAIL_COUNT + 1))
    loss="100"
  fi
  PING_SAMPLE_LOSS="${loss}"
}

run_fec_case() {
  local label="$1"
  local fec_flag="$2"

  write_fec_config "${fec_flag}" "${label}"
  start_multipath_configs "${label}" "${WORKDIR}/server-${label}.json" "${WORKDIR}/client-${label}.json"
  wait_ping_ok "${label} baseline"

  echo "[${label}] apply weak client-to-server loss"
  ip netns exec "${NS_C}" tc qdisc replace dev "${VETHC1}" root netem loss 20%
  sleep 1
  run_ping_sample "${label}-weak"
  clear_loss
  stop_multipath

  FEC_CASE_LOSS="${PING_SAMPLE_LOSS}"
}

run_fec_comparison() {
  echo "==== fec weak-net comparison start ===="
  clear_loss

  local off_loss
  run_fec_case "fec-off" "false"
  off_loss="${FEC_CASE_LOSS}"
  sleep 1

  local on_loss
  run_fec_case "fec-on" "true"
  on_loss="${FEC_CASE_LOSS}"

  echo "[fec] comparison under 20% client-to-server netem loss"
  echo "[fec] off packet_loss=${off_loss}%"
  echo "[fec] on  packet_loss=${on_loss}%"

  if awk -v off="${off_loss}" -v on="${on_loss}" 'BEGIN { exit !(on < off) }'; then
    echo "[fec] PASS: FEC reduced observed tunnel packet loss"
  else
    echo "[fec] FAIL: FEC did not reduce observed tunnel packet loss"
    FAIL_COUNT=$((FAIL_COUNT + 1))
  fi

  clear_loss
  echo "==== fec weak-net comparison end ===="
}

build_bin
setup_netns

run_mode "udp" "${PORT_UDP}"
run_mode "tcp" "${PORT_TCP}"
run_fec_comparison

if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "real e2e failed (${FAIL_COUNT} failures)"
  exit 1
fi

echo "real e2e ok"
