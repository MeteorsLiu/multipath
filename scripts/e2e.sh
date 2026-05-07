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
FEC_HIGH_RTT_DELAY="${MULTIPATH_REAL_E2E_FEC_HIGH_RTT_DELAY:-50ms}"

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
    MULTIPATH_REAL_E2E_FEC_HIGH_RTT_DELAY="${FEC_HIGH_RTT_DELAY}" \
    bash "$0" "$@"
fi

echo "real e2e workdir: ${WORKDIR}"

SUFFIX="$$"
NS_C="mp_c_${SUFFIX}"
NS_S="mp_s_${SUFFIX}"
NS_N="mp_n_${SUFFIX}"

VETHC1="mpc1_${SUFFIX}"
VETHS1="mps1_${SUFFIX}"
VETHC2="mpc2_${SUFFIX}"
VETHS2="mps2_${SUFFIX}"
VETHCN="mpcn_${SUFFIX}"
VETHNC="mpnc_${SUFFIX}"
VETHNS="mpns_${SUFFIX}"
VETHSN="mpsn_${SUFFIX}"

PORT_MULTIPATH=5001
PORT_LEGACY=5002
PORT_FALLBACK=5003
PORT_FEC=5004
PORT_NAT=5005
PORT_PER_LANE_FALLBACK=5006
PORT_FEC_TCP_FALLBACK=5007
PORT_MULTIPATH_FEC=5008
PORT_CONCURRENT_FALLBACK=5009
PORT_FALLBACK_DIAL_ERROR=5010
PORT_WEIGHTED=5011
PORT_MTU=5012
PORT_FEC_LOADED_LATENCY=5013
PORT_LEG_SELECTOR=5014
PORT_SERVER_RESTART=5015
PORT_BW_PROBE_CONVERGENCE=5016
PORT_BW_PROBE_GUARD=5017

PATH1_C="10.201.1.1/24"
PATH1_S="10.201.1.2/24"
PATH2_C="10.201.2.1/24"
PATH2_S="10.201.2.2/24"
PATH1_REMOTE="10.201.1.2"
PATH2_REMOTE="10.201.2.2"

NAT_CLIENT_ADDR="10.202.1.2/24"
NAT_ROUTER_CLIENT_ADDR="10.202.1.1/24"
NAT_ROUTER_SERVER_ADDR="10.202.2.1/24"
NAT_SERVER_ADDR="10.202.2.2/24"
NAT_CLIENT_SUBNET="10.202.1.0/24"
NAT_SERVER_SUBNET="10.202.2.0/24"
NAT_CLIENT_GW="10.202.1.1"
NAT_ROUTER_SERVER_IP="10.202.2.1"
NAT_SERVER_REMOTE="10.202.2.2"

TUN_C_LOCAL="172.31.0.1"
TUN_S_LOCAL="172.31.0.2"
TUN_C_REMOTE="${TUN_S_LOCAL}"
TUN_S_REMOTE="${TUN_C_LOCAL}"

FAIL_COUNT=0
PING_SAMPLE_LOSS=""
FEC_CASE_LOSS=""
FEC_CASE_RECOVERED=""
FEC_CASE_RECOVER_ERR=""
CLIENT_PID=""
SERVER_PID=""
CURRENT_LOG_FILE=""
CURRENT_CLIENT_LOG=""
CURRENT_SERVER_LOG=""
CURRENT_EXTRA_ENV=()

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
  ip netns del "${NS_N}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

build_bin() {
  require_command ip
  require_command tc
  require_command ping
  require_command iptables

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
  ip netns del "${NS_N}" >/dev/null 2>&1 || true

  ip netns add "${NS_C}"
  ip netns add "${NS_S}"
  ip netns add "${NS_N}"

  ip link add "${VETHC1}" type veth peer name "${VETHS1}"
  ip link add "${VETHC2}" type veth peer name "${VETHS2}"
  ip link add "${VETHCN}" type veth peer name "${VETHNC}"
  ip link add "${VETHNS}" type veth peer name "${VETHSN}"

  ip link set "${VETHC1}" netns "${NS_C}"
  ip link set "${VETHS1}" netns "${NS_S}"
  ip link set "${VETHC2}" netns "${NS_C}"
  ip link set "${VETHS2}" netns "${NS_S}"
  ip link set "${VETHCN}" netns "${NS_C}"
  ip link set "${VETHNC}" netns "${NS_N}"
  ip link set "${VETHNS}" netns "${NS_N}"
  ip link set "${VETHSN}" netns "${NS_S}"

  ip netns exec "${NS_C}" ip addr add "${PATH1_C}" dev "${VETHC1}"
  ip netns exec "${NS_S}" ip addr add "${PATH1_S}" dev "${VETHS1}"
  ip netns exec "${NS_C}" ip addr add "${PATH2_C}" dev "${VETHC2}"
  ip netns exec "${NS_S}" ip addr add "${PATH2_S}" dev "${VETHS2}"
  ip netns exec "${NS_C}" ip addr add "${NAT_CLIENT_ADDR}" dev "${VETHCN}"
  ip netns exec "${NS_N}" ip addr add "${NAT_ROUTER_CLIENT_ADDR}" dev "${VETHNC}"
  ip netns exec "${NS_N}" ip addr add "${NAT_ROUTER_SERVER_ADDR}" dev "${VETHNS}"
  ip netns exec "${NS_S}" ip addr add "${NAT_SERVER_ADDR}" dev "${VETHSN}"

  ip netns exec "${NS_C}" ip link set lo up
  ip netns exec "${NS_S}" ip link set lo up
  ip netns exec "${NS_N}" ip link set lo up
  ip netns exec "${NS_C}" ip link set "${VETHC1}" up
  ip netns exec "${NS_C}" ip link set "${VETHC2}" up
  ip netns exec "${NS_S}" ip link set "${VETHS1}" up
  ip netns exec "${NS_S}" ip link set "${VETHS2}" up
  ip netns exec "${NS_C}" ip link set "${VETHCN}" up
  ip netns exec "${NS_N}" ip link set "${VETHNC}" up
  ip netns exec "${NS_N}" ip link set "${VETHNS}" up
  ip netns exec "${NS_S}" ip link set "${VETHSN}" up

  ip netns exec "${NS_C}" ip route add "${NAT_SERVER_SUBNET}" via "${NAT_CLIENT_GW}" dev "${VETHCN}"
  ip netns exec "${NS_N}" sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
  ip netns exec "${NS_N}" iptables -P FORWARD ACCEPT
  ip netns exec "${NS_N}" iptables -t nat -A POSTROUTING \
    -s "${NAT_CLIENT_SUBNET}" -d "${NAT_SERVER_SUBNET}" -o "${VETHNS}" \
    -j SNAT --to-source "${NAT_ROUTER_SERVER_IP}"

  echo "real e2e namespaces: client=${NS_C} server=${NS_S} nat=${NS_N}"
  echo "real e2e nat: client_dev=${VETHCN} router_client_dev=${VETHNC} router_server_dev=${VETHNS} server_dev=${VETHSN}"
  print_netns_debug
}

print_netns_debug() {
  echo "---- real e2e netns debug ----"
  ip netns list | grep -E "${NS_C}|${NS_S}|${NS_N}" || true
  echo "[client routes]"
  ip netns exec "${NS_C}" ip -4 route show
  echo "[nat routes]"
  ip netns exec "${NS_N}" ip -4 route show
  echo "[server routes]"
  ip netns exec "${NS_S}" ip -4 route show
  echo "[nat ip_forward]"
  ip netns exec "${NS_N}" cat /proc/sys/net/ipv4/ip_forward
  echo "[nat iptables filter]"
  ip netns exec "${NS_N}" iptables -S FORWARD
  echo "[nat iptables nat]"
  ip netns exec "${NS_N}" iptables -t nat -S
  echo "---- end real e2e netns debug ----"
}

write_two_lane_config() {
  local name="$1"
  local port="$2"
  local legacy_tcp_flag="$3"
  local fec_flag="$4"
  local probe_interval_ms="${5:-200}"
  local probe_timeout_ms="${6:-600}"
  local path1_weight="${7:-1}"
  local path2_weight="${8:-1}"

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
      { "remoteAddr": "${PATH1_REMOTE}:${port}", "weight": ${path1_weight} },
      { "remoteAddr": "${PATH2_REMOTE}:${port}", "weight": ${path2_weight} }
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

write_nat_config() {
  local name="$1"
  local port="$2"
  local probe_interval_ms="${3:-200}"
  local probe_timeout_ms="${4:-600}"

  cat >"${WORKDIR}/server-${name}.json" <<EOF
{
  "isServer": true,
  "tcp": false,
  "server": { "listen": "0.0.0.0:${port}" },
  "tun": {
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "fec": false,
  "probeIntervalMS": ${probe_interval_ms},
  "probeTimeoutMS": ${probe_timeout_ms}
}
EOF

  cat >"${WORKDIR}/client-${name}.json" <<EOF
{
  "tcp": false,
  "client": {
    "remotePaths": [
      { "remoteAddr": "${NAT_SERVER_REMOTE}:${port}", "weight": 1 }
    ]
  },
  "tun": {
    "localAddr": "${TUN_C_LOCAL}",
    "remoteAddr": "${TUN_C_REMOTE}",
    "allowedIPs": ["${TUN_C_REMOTE}/32"]
  },
  "fec": false,
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
  if [[ -n "${CURRENT_CLIENT_LOG}" && -f "${CURRENT_CLIENT_LOG}" ]]; then
    echo "---- ${CURRENT_CLIENT_LOG} tail ----"
    tail -n 80 "${CURRENT_CLIENT_LOG}" || true
    echo "---- end client log tail ----"
  fi
  if [[ -n "${CURRENT_SERVER_LOG}" && -f "${CURRENT_SERVER_LOG}" ]]; then
    echo "---- ${CURRENT_SERVER_LOG} tail ----"
    tail -n 80 "${CURRENT_SERVER_LOG}" || true
    echo "---- end server log tail ----"
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
  ping_once_from "${NS_C}" "${TUN_C_REMOTE}"
}

ping_once_from() {
  local ns="$1"
  local remote="$2"
  ip netns exec "${ns}" ping -c 1 -W 1 "${remote}" >/dev/null 2>&1
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
  local server_log="${WORKDIR}/${name}.server.log"
  local client_log="${WORKDIR}/${name}.client.log"

  CURRENT_LOG_FILE="${client_log}"
  CURRENT_CLIENT_LOG="${client_log}"
  CURRENT_SERVER_LOG="${server_log}"
  start_server_process "${name}"
  ip netns exec "${NS_C}" env MULTIPATH_DEBUG="${REAL_E2E_DEBUG}" "${CURRENT_EXTRA_ENV[@]}" "${BIN}" -config "${client_config}" >>"${client_log}" 2>&1 &
  CLIENT_PID=$!

  echo "[${name}] client log: ${client_log}"
  echo "[${name}] server log: ${server_log} (MULTIPATH_DEBUG=${REAL_E2E_DEBUG})"
}

start_server_process() {
  local name="$1"
  local server_config="${WORKDIR}/server-${name}.json"
  local server_log="${WORKDIR}/${name}.server.log"

  CURRENT_SERVER_LOG="${server_log}"
  ip netns exec "${NS_S}" env MULTIPATH_DEBUG="${REAL_E2E_DEBUG}" "${CURRENT_EXTRA_ENV[@]}" "${BIN}" -config "${server_config}" >>"${server_log}" 2>&1 &
  SERVER_PID=$!
}

restart_server_process() {
  local name="$1"
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
  start_server_process "${name}"
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
  CURRENT_CLIENT_LOG=""
  CURRENT_SERVER_LOG=""
  CURRENT_EXTRA_ENV=()
}

clear_loss() {
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHCN}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS1}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHS2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" tc qdisc del dev "${VETHSN}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_N}" tc qdisc del dev "${VETHNC}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_N}" tc qdisc del dev "${VETHNS}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" iptables -F INPUT >/dev/null 2>&1 || true
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

add_loss_delay_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local loss="$5"
  local delay="$6"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem delay "${delay}" loss "${loss}"
}

add_delay_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local delay="$5"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem delay "${delay}"
}

add_rate_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local rate="$5"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem rate "${rate}"
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

path_client_dev() {
  case "$1" in
  1) printf '%s' "${VETHC1}" ;;
  2) printf '%s' "${VETHC2}" ;;
  *)
    echo "unsupported path: $1"
    exit 1
    ;;
  esac
}

path_server_dev() {
  case "$1" in
  1) printf '%s' "${VETHS1}" ;;
  2) printf '%s' "${VETHS2}" ;;
  *)
    echo "unsupported path: $1"
    exit 1
    ;;
  esac
}

apply_udp_tunnel_block_path() {
  local path="$1"
  local port="$2"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 100%
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 100%
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
}

apply_tcp_tunnel_block_path() {
  local path="$1"
  local port="$2"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 100%
  add_port_filter "${NS_C}" "${client_dev}" 1 tcp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 100%
  add_port_filter "${NS_S}" "${server_dev}" 1 tcp sport "${port}" 3
}

apply_udp_tunnel_block_path1() {
  apply_udp_tunnel_block_path 1 "$1"
}

apply_tcp_tunnel_block_path1() {
  apply_tcp_tunnel_block_path 1 "$1"
}

apply_udp_partial_loss() {
  local path="$1"
  local port="$2"
  local loss="$3"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 "${loss}"
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 "${loss}"
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
}

apply_udp_tunnel_rate_path() {
  local path="$1"
  local port="$2"
  local rate="$3"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_rate_band "${NS_C}" "${client_dev}" 3 30 "${rate}"
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_rate_band "${NS_S}" "${server_dev}" 3 30 "${rate}"
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
}

apply_nat_tcp_block() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHCN}"
  add_loss_band "${NS_C}" "${VETHCN}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHCN}" 1 tcp dport "${port}" 3
}

apply_tcp_server_reject() {
  local port="$1"
  ip netns exec "${NS_S}" iptables -A INPUT -p tcp --dport "${port}" -j REJECT --reject-with tcp-reset
}

apply_fec_loss_path() {
  local path="$1"
  local port="$2"
  local client_dev
  client_dev="$(path_client_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 20%
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3
  add_loss_band "${NS_C}" "${client_dev}" 4 40 100%
  add_port_filter "${NS_C}" "${client_dev}" 2 tcp dport "${port}" 4
}

apply_fec_loss() {
  apply_fec_loss_path 1 "$1"
}

apply_fec_loss_high_rtt() {
  local port="$1"
  local delay="$2"

  setup_prio_qdisc "${NS_C}" "${VETHC1}"
  add_loss_delay_band "${NS_C}" "${VETHC1}" 3 30 20% "${delay}"
  add_port_filter "${NS_C}" "${VETHC1}" 1 udp dport "${port}" 3
  add_loss_band "${NS_C}" "${VETHC1}" 4 40 100%
  add_port_filter "${NS_C}" "${VETHC1}" 2 tcp dport "${port}" 4

  setup_prio_qdisc "${NS_S}" "${VETHS1}"
  add_delay_band "${NS_S}" "${VETHS1}" 3 30 "${delay}"
  add_port_filter "${NS_S}" "${VETHS1}" 1 udp sport "${port}" 3
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

run_server_restart_reconnect_case() {
  local name="server-restart-reconnect"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_SERVER_RESTART}" false false 200 600
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP and stop server; first TCP fallback dial should fail"
  apply_udp_tunnel_block_path 1 "${PORT_SERVER_RESTART}"
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
  sleep 2
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "fallback_result_err .*lane=1" 20 "client observed TCP fallback dial failure while server was down"

  echo "[${name}] restart server with UDP still blocked; client must retry TCP fallback and reconnect"
  start_server_process "${name}"
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "fallback_result_start_lane session=[0-9]+ lane=1 leg=\\{tcp conn=" 20 "client retried TCP fallback after server restart"
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "accept_hello_ack session=[0-9]+ lane=1 .*tcp conn=" 20 "client accepted TCP HELLO_ACK after server restart"
  wait_ping_ok "${name} post-restart-tcp" 20

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_leg_selector_case() {
  local name="leg-selector"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_LEG_SELECTOR}" false false 200 600
  echo "[${name}] apply 50% UDP loss before startup; initial bandwidth probes should mark UDP QoS-limited"
  apply_udp_partial_loss 1 "${PORT_LEG_SELECTOR}" 50%
  local client_qos_start_line
  local server_qos_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_qos_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_qos_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"
  echo "[${name}] DEBUG: CURRENT_CLIENT_LOG=${CURRENT_CLIENT_LOG} CURRENT_SERVER_LOG=${CURRENT_SERVER_LOG} client_qos_start_line=${client_qos_start_line} server_qos_start_line=${server_qos_start_line}"
  wait_log_pattern "${name}" "accept_hello_ack session=[0-9]+ lane=1 .*tcp conn=" 20 "warm TCP fallback leg reached HELLO_ACK"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision .*lane=1 .*udp_qos_limited=true .*tcp_better=true selected_leg=tcp" 45 "client produced bandwidth-probe QoS decision" "${client_qos_start_line}"
  local client_select_start_line
  client_select_start_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*leg=\\{tcp .*frame=type=DATA" 20 "client leg selector chose TCP for DATA after UDP QoS detection" "${client_select_start_line}" "${NS_C}" "${TUN_C_REMOTE}"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "bandwidth_probe_decision .*lane=1 .*udp_qos_limited=true .*tcp_better=true selected_leg=tcp" 45 "server produced bandwidth-probe QoS decision" "${server_qos_start_line}"
  local server_select_start_line
  server_select_start_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_SERVER_LOG}" "schedule_select.*leg=\\{tcp .*frame=type=DATA" 20 "server leg selector chose TCP for DATA after UDP QoS detection" "${server_select_start_line}" "${NS_S}" "${TUN_S_REMOTE}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_convergence_case() {
  local name="bandwidth-probe-convergence"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_CONVERGENCE}" false false 200 1000
  echo "[${name}] rate-limit UDP tunnel before startup; bandwidth probe should classify UDP relative to TCP"
  apply_udp_tunnel_rate_path 1 "${PORT_BW_PROBE_CONVERGENCE}" 80mbit
  local client_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  start_multipath "${name}"

  wait_ping_ok "${name} baseline-under-rate-limit" 12
  echo "[${name}] DEBUG: CURRENT_CLIENT_LOG=${CURRENT_CLIENT_LOG} start_line=${client_start_line}"
  wait_client_tcp_reference_probe "${name}" "${client_start_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision .*lane=1 .*udp_qos_limited=true .*tcp_better=true selected_leg=tcp" 35 "client bandwidth probe classified UDP relative to TCP" "${client_start_line}"
  wait_ping_ok "${name} post-convergence" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_tcp_reference_case() {
  local name="bandwidth-probe-tcp-reference"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_GUARD}" false false 200 1000
  echo "[${name}] apply 200mbit UDP tunnel bottleneck; bandwidth probe should classify UDP relative to TCP reference"
  apply_udp_tunnel_rate_path 1 "${PORT_BW_PROBE_GUARD}" 200mbit
  local client_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  start_multipath "${name}"
  wait_ping_ok "${name} baseline" 12
  echo "[${name}] DEBUG: CURRENT_CLIENT_LOG=${CURRENT_CLIENT_LOG} start_line=${client_start_line}"
  wait_client_tcp_reference_probe "${name}" "${client_start_line}"
  wait_bandwidth_probe_udp_rate_window "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 40 "client UDP probe measured 200mbit bottleneck without excessive probe target" 160000000 260000000 300000000
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision .*lane=1 .*udp_qos_limited=true .*tcp_better=true selected_leg=tcp" 35 "client classified UDP relative to TCP reference" "${client_start_line}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_nat_case() {
  local name="nat"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_nat_config "${name}" "${PORT_NAT}" 200 600

  echo "[${name}] block TCP fallback; UDP must traverse SNAT and conntrack"
  apply_nat_tcp_block "${PORT_NAT}"
  start_multipath "${name}"
  wait_ping_ok "${name} udp-through-snat" 15

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

count_log_pattern() {
  local pattern="$1"
  local log_file="$2"
  if [[ ! -f "${log_file}" ]]; then
    printf '0\n'
    return 0
  fi
  grep -c "${pattern}" "${log_file}" || true
}

wait_bandwidth_probe_udp_rate_window() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local timeout="${4:-35}"
  local message="$5"
  local min_window_bps="$6"
  local max_window_bps="$7"
  local max_target_bps="$8"
  local deadline=$((SECONDS + timeout))
  local output status

  while (( SECONDS < deadline )); do
    set +e
    output="$(awk \
      -v start="${start_line}" \
      -v min_window="${min_window_bps}" \
      -v max_window="${max_window_bps}" \
      -v max_target="${max_target_bps}" '
        NR <= start { next }
        /send\/bw_probe: round_start/ && /leg=\{udp/ {
          rate = 0
          for (i = 1; i <= NF; i++) {
            if ($i ~ /^rate_bps=/) {
              split($i, parts, "=")
              rate = parts[2] + 0
              break
            }
          }
          if (rate > max_rate) {
            max_rate = rate
          }
        }
        /send\/bw_probe: train_finish/ && /leg=\{udp/ {
          for (i = 1; i <= NF; i++) {
            if ($i ~ /^window_bps=/) {
              split($i, parts, "=")
              window_bps = parts[2] + 0
              finish = 1
            }
          }
        }
        END {
          if (!finish) {
            print "need udp train_finish"
            exit 2
          }
          if (window_bps < min_window || window_bps > max_window) {
            printf("bad-window window_bps=%d max_rate_bps=%d\n", window_bps, max_rate)
            exit 1
          }
          if (max_rate > max_target) {
            printf("bad-target window_bps=%d max_rate_bps=%d\n", window_bps, max_rate)
            exit 1
          }
          printf("ok window_bps=%d max_rate_bps=%d\n", window_bps, max_rate)
          exit 0
        }
      ' "${log_file}" 2>/dev/null)"
    status=$?
    set -e
    case "${status}" in
    0)
      pass "${label}" "${message}: ${output#ok }"
      return 0
      ;;
    1)
      fail "${label}" "${message}: ${output}"
      return 1
      ;;
    esac
    if ! check_multipath_alive "${label}"; then
      return 1
    fi
    ping_once_from "${NS_C}" "${TUN_C_REMOTE}" || true
    sleep 0.2
  done
  fail "${label}" "${message}: UDP train_finish not observed within ${timeout}s"
  return 1
}

assert_log_not_contains() {
  local label="$1"
  local pattern="$2"
  local message="$3"
  if [[ -z "${CURRENT_LOG_FILE}" || ! -f "${CURRENT_LOG_FILE}" ]]; then
    fail "${label}" "${message}: log file missing"
    return
  fi
  if grep -E -q "${pattern}" "${CURRENT_LOG_FILE}"; then
    fail "${label}" "${message}: unexpected pattern found: ${pattern}"
  else
    pass "${label}" "${message}"
  fi
}

wait_log_pattern() {
  local label="$1"
  local pattern="$2"
  local timeout="${3:-15}"
  local message="$4"
  if [[ -z "${CURRENT_LOG_FILE}" ]]; then
    fail "${label}" "${message}: log file unset"
    return 1
  fi
  wait_log_file_pattern "${label}" "${CURRENT_LOG_FILE}" "${pattern}" "${timeout}" "${message}"
}

wait_log_file_pattern() {
  local label="$1"
  local log_file="$2"
  local pattern="$3"
  local timeout="${4:-15}"
  local message="$5"
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if [[ -f "${log_file}" ]] && grep -E -q "${pattern}" "${log_file}"; then
      pass "${label}" "${message}"
      return 0
    fi
    sleep 0.2
  done
  fail "${label}" "${message}: pattern not seen within ${timeout}s: ${pattern}"
  return 1
}

current_log_file_line_count() {
  local log_file="$1"
  if [[ -z "${log_file}" || ! -f "${log_file}" ]]; then
    printf '0\n'
    return 0
  fi
  wc -l <"${log_file}"
}

wait_log_file_pattern_while_ping() {
  wait_log_file_pattern_while_ping_from "$@" "${NS_C}" "${TUN_C_REMOTE}"
}

wait_client_tcp_reference_probe() {
  local name="$1"
  local start_line="$2"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "accept_hello_ack session=[0-9]+ lane=1 .*tcp conn=" 25 "client warmed TCP reference leg" "${start_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw_probe: train_start .*leg=\\{tcp " 10 "client started TCP reference probe" "${start_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw_probe: train_finish .*leg=\\{tcp .*window_bps=[0-9]+" 25 "client finished TCP reference probe" "${start_line}"
  assert_client_tcp_reference_before_udp_probe "${name}" "${start_line}"
}

assert_client_tcp_reference_before_udp_probe() {
  local label="$1"
  local start_line="$2"
  local output status

  set +e
  output="$(awk -v start="${start_line}" '
    NR <= start { next }
    /send\/bw_probe: train_start/ && /leg=\{udp/ {
      message = "udp train_start before tcp train_finish: " $0
      status = 1
      done = 1
      exit
    }
    /send\/bw_probe: train_finish/ && /leg=\{tcp/ {
      message = "tcp train_finish before udp train_start"
      status = 0
      done = 1
      exit
    }
    END {
      if (!done) {
        message = "need tcp train_finish"
        status = 2
      }
      print message
      exit status
    }
  ' "${CURRENT_CLIENT_LOG}" 2>/dev/null)"
  status=$?
  set -e

  case "${status}" in
  0)
    pass "${label}" "client TCP reference completed before UDP probe"
    ;;
  1)
    fail "${label}" "client UDP bandwidth probe started before TCP reference completed: ${output}"
    return 1
    ;;
  *)
    fail "${label}" "client TCP reference order could not be verified: ${output}"
    return 1
    ;;
  esac
}

wait_log_file_any_pattern_while_ping() {
  local label="$1"
  local log_file="$2"
  local timeout="${3:-15}"
  local message="$4"
  local start_line="${5:-0}"
  shift 5
  wait_log_file_any_pattern_while_ping_from "${label}" "${log_file}" "${timeout}" "${message}" "${start_line}" "${NS_C}" "${TUN_C_REMOTE}" "$@"
}

wait_log_file_any_pattern_while_ping_from() {
  local label="$1"
  local log_file="$2"
  local timeout="${3:-15}"
  local message="$4"
  local start_line="${5:-0}"
  local ping_ns="$6"
  local ping_remote="$7"
  shift 7
  if [[ -z "${log_file}" ]]; then
    fail "${label}" "${message}: log file unset"
    return 1
  fi

  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if log_file_has_any_pattern_since "${log_file}" "${start_line}" "$@"; then
      pass "${label}" "${message}"
      return 0
    fi
    if ! check_multipath_alive "${label}"; then
      return 1
    fi
    ping_once_from "${ping_ns}" "${ping_remote}" || true
    if log_file_has_any_pattern_since "${log_file}" "${start_line}" "$@"; then
      pass "${label}" "${message}"
      return 0
    fi
    sleep 0.2
  done
  if log_file_has_any_pattern_since "${log_file}" "${start_line}" "$@"; then
    pass "${label}" "${message}"
    return 0
  fi
  echo "[${label}] traffic probe debug: ns=${ping_ns} remote=${ping_remote}"
  ip netns exec "${ping_ns}" ip -4 route get "${ping_remote}" || true
  ip netns exec "${ping_ns}" ping -c 1 -W 1 "${ping_remote}" || true
  fail "${label}" "${message}: patterns not seen within ${timeout}s: $*"
  return 1
}

log_file_has_any_pattern_since() {
  local log_file="$1"
  local start_line="$2"
  shift 2
  if [[ ! -f "${log_file}" ]]; then
    return 1
  fi
  local pattern
  for pattern in "$@"; do
    if tail -n "+$((start_line + 1))" "${log_file}" | grep -E -q "${pattern}"; then
      return 0
    fi
  done
  return 1
}

wait_log_file_pattern_while_ping_from() {
  local label="$1"
  local log_file="$2"
  local pattern="$3"
  local timeout="${4:-15}"
  local message="$5"
  local start_line="${6:-0}"
  local ping_ns="$7"
  local ping_remote="$8"
  if [[ -z "${log_file}" ]]; then
    fail "${label}" "${message}: log file unset"
    return 1
  fi

  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if [[ -f "${log_file}" ]] && tail -n "+$((start_line + 1))" "${log_file}" | grep -E -q "${pattern}"; then
      pass "${label}" "${message}"
      return 0
    fi
    if ! check_multipath_alive "${label}"; then
      return 1
    fi
    ping_once_from "${ping_ns}" "${ping_remote}" || true
    if [[ -f "${log_file}" ]] && tail -n "+$((start_line + 1))" "${log_file}" | grep -E -q "${pattern}"; then
      pass "${label}" "${message}"
      return 0
    fi
    sleep 0.2
  done
  if [[ -f "${log_file}" ]] && tail -n "+$((start_line + 1))" "${log_file}" | grep -E -q "${pattern}"; then
    pass "${label}" "${message}"
    return 0
  fi
  echo "[${label}] traffic probe debug: ns=${ping_ns} remote=${ping_remote}"
  ip netns exec "${ping_ns}" ip -4 route get "${ping_remote}" || true
  ip netns exec "${ping_ns}" ping -c 1 -W 1 "${ping_remote}" || true
  fail "${label}" "${message}: pattern not seen within ${timeout}s: ${pattern}"
  return 1
}

run_fec_case() {
  local label="$1"
  local fec_flag="$2"
  local high_rtt_delay="${3:-}"
  local log_file="${WORKDIR}/${label}.server.log"

  clear_loss
  write_one_lane_config "${label}" "${PORT_FEC}" false "${fec_flag}" 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${label}"
  wait_ping_ok "${label} baseline" 12

  if [[ -n "${high_rtt_delay}" ]]; then
    echo "[${label}] apply 20% UDP data loss, ${high_rtt_delay} one-way UDP tunnel delay, and block TCP fallback"
    apply_fec_loss_high_rtt "${PORT_FEC}" "${high_rtt_delay}"
  else
    echo "[${label}] apply 20% UDP data loss and block TCP fallback"
    apply_fec_loss "${PORT_FEC}"
  fi
  sleep 1
  run_ping_sample "${label}-weak"

  clear_loss
  stop_multipath
  FEC_CASE_LOSS="${PING_SAMPLE_LOSS}"
  FEC_CASE_RECOVERED="$(count_log_pattern "recv: recover_emit" "${log_file}")"
  FEC_CASE_RECOVER_ERR="$(count_log_pattern "recv: recover_err" "${log_file}")"
  if [[ "${fec_flag}" == "true" ]]; then
    echo "[${label}] fec recover_emit=${FEC_CASE_RECOVERED} recover_err=${FEC_CASE_RECOVER_ERR}"
  fi
}

run_fec_comparison() {
  echo "==== fec e2e start ===="

  local off_loss
  run_fec_case "fec-off" false
  off_loss="${FEC_CASE_LOSS}"
  sleep 1

  local on_loss
  local on_recovered
  local on_recover_err
  run_fec_case "fec-on" true
  on_loss="${FEC_CASE_LOSS}"
  on_recovered="${FEC_CASE_RECOVERED}"
  on_recover_err="${FEC_CASE_RECOVER_ERR}"

  echo "[fec] comparison under 20% client-to-server UDP tunnel loss"
  echo "[fec] off packet_loss=${off_loss}%"
  echo "[fec] on  packet_loss=${on_loss}%"
  echo "[fec] on recover_emit=${on_recovered} recover_err=${on_recover_err}"

  if (( on_recover_err > 0 )); then
    fail "fec" "FEC recovery errors were observed"
  elif awk -v off="${off_loss}" -v on="${on_loss}" 'BEGIN { exit !(on < off) }'; then
    pass "fec" "FEC reduced observed tunnel packet loss"
  else
    fail "fec" "FEC did not reduce observed tunnel packet loss"
  fi

  local high_off_loss
  run_fec_case "fec-off-high-rtt" false "${FEC_HIGH_RTT_DELAY}"
  high_off_loss="${FEC_CASE_LOSS}"
  sleep 1

  local high_on_loss
  local high_on_recovered
  local high_on_recover_err
  run_fec_case "fec-on-high-rtt" true "${FEC_HIGH_RTT_DELAY}"
  high_on_loss="${FEC_CASE_LOSS}"
  high_on_recovered="${FEC_CASE_RECOVERED}"
  high_on_recover_err="${FEC_CASE_RECOVER_ERR}"

  echo "[fec-high-rtt] comparison under 20% client-to-server UDP tunnel loss and ${FEC_HIGH_RTT_DELAY} one-way UDP tunnel delay"
  echo "[fec-high-rtt] off packet_loss=${high_off_loss}%"
  echo "[fec-high-rtt] on  packet_loss=${high_on_loss}%"
  echo "[fec-high-rtt] on recover_emit=${high_on_recovered} recover_err=${high_on_recover_err}"

  if (( high_on_recover_err > 0 )); then
    fail "fec-high-rtt" "FEC recovery errors were observed under high RTT"
  elif awk -v off="${high_off_loss}" -v on="${high_on_loss}" 'BEGIN { exit !(on < off) }'; then
    pass "fec-high-rtt" "FEC reduced observed tunnel packet loss under high RTT"
  elif (( high_on_recovered > 0 )); then
    pass "fec-high-rtt" "FEC recovered packets under high RTT; packet-loss comparison was noisy"
  else
    fail "fec-high-rtt" "FEC did not recover packets under high RTT"
  fi

  clear_loss
  echo "==== fec e2e end ===="
}

run_per_lane_fallback_case() {
  local name="per-lane-fallback"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_PER_LANE_FALLBACK}" false false 200 600
  expect_ping_fail_for "${name} precheck" 2
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP only on path2; lane=2 must fall back to TCP while lane=1 stays on UDP"
  apply_udp_tunnel_block_path 2 "${PORT_PER_LANE_FALLBACK}"
  local client_block_line
  local server_block_line
  client_block_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  server_block_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"

  wait_ping_ok "${name} mixed-lane-state" 15
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select .*lane=2 .*leg=\\{tcp" 15 "client sent lane=2 DATA over TCP after path2 UDP block" "${client_block_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "schedule_select .*lane=1 .*leg=\\{udp" 15 "server kept lane=1 DATA on UDP while only path2 was blocked" "${server_block_line}"

  echo "[${name}] restore UDP; lane=2 must come back to UDP"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15
  wait_log_pattern "${name}" "target_recovered .*lane=2" 15 "lane=2 probe target recovered after UDP restore"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_fec_tcp_fallback_case() {
  local name="fec-tcp-fallback"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_FEC_TCP_FALLBACK}" false true 200 600
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP on path1; lane must fall back to TCP while preserving FEC capability"
  apply_udp_tunnel_block_path 1 "${PORT_FEC_TCP_FALLBACK}"

  wait_ping_ok "${name} tcp-fallback" 20
  wait_log_pattern "${name}" "accept_hello_ack session=[0-9]+ lane=1 .*tcp conn=.*negotiated_caps=0x3 negotiated_fec_profile=2" 20 "TCP HELLO_ACK preserved FEC capability and variable FEC profile after fallback"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_multipath_fec_case() {
  local name="multipath-fec"
  local log_file="${WORKDIR}/${name}.server.log"
  local threshold=10
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_MULTIPATH_FEC}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] apply 20% UDP loss + TCP block on path1; path2 stays clean"
  apply_fec_loss_path 1 "${PORT_MULTIPATH_FEC}"
  sleep 1
  run_ping_sample "${name}-weak"

  clear_loss
  stop_multipath

  local recovered
  local recover_err
  recovered="$(count_log_pattern "recv: recover_emit" "${log_file}")"
  recover_err="$(count_log_pattern "recv: recover_err" "${log_file}")"
  echo "[${name}] observed loss=${PING_SAMPLE_LOSS}% recover_emit=${recovered} recover_err=${recover_err}"

  if (( recover_err > 0 )); then
    fail "${name}" "FEC recovery errors observed"
  elif awk -v loss="${PING_SAMPLE_LOSS}" -v threshold="${threshold}" 'BEGIN { exit !(loss < threshold) }'; then
    pass "${name}" "observed loss ${PING_SAMPLE_LOSS}% < ${threshold}% under multipath + FEC"
  else
    fail "${name}" "observed loss ${PING_SAMPLE_LOSS}% >= ${threshold}% under multipath + FEC"
  fi

  echo "==== ${name} e2e end ===="
}

run_fec_loaded_latency_case() {
  local name="fec-loaded-latency"
  local iperf_rate="${MULTIPATH_REAL_E2E_FEC_LOAD_RATE:-10M}"
  local iperf_duration="${MULTIPATH_REAL_E2E_FEC_LOAD_DURATION:-25}"
  local ping_count="${MULTIPATH_REAL_E2E_FEC_LOAD_PING_COUNT:-400}"
  local ping_interval="${MULTIPATH_REAL_E2E_FEC_LOAD_PING_INTERVAL:-0.05}"

  echo "==== ${name} e2e start ===="
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${name}] iperf3 not found, skip loaded-latency measurement"
    echo "==== ${name} e2e end ===="
    return 0
  fi

  clear_loss
  write_one_lane_config "${name}" "${PORT_FEC_LOADED_LATENCY}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"
  wait_ping_ok "${name} baseline" 12

  echo "[${name}] apply 20% UDP data loss and block TCP fallback"
  apply_fec_loss "${PORT_FEC_LOADED_LATENCY}"
  sleep 1

  echo "[${name}] start iperf3 UDP background load: rate=${iperf_rate} duration=${iperf_duration}s"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >"${WORKDIR}/${name}.iperf-server.log" 2>&1 &
  local iperf_server=$!
  sleep 1
  ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -u -b "${iperf_rate}" -t "${iperf_duration}" -i 0 \
    >"${WORKDIR}/${name}.iperf-client.log" 2>&1 &
  local iperf_client=$!
  sleep 0.5

  echo "[${name}] sparse ping under load: count=${ping_count} interval=${ping_interval}s"
  local output
  output="$(ip netns exec "${NS_C}" ping -c "${ping_count}" -i "${ping_interval}" -W 1 "${TUN_C_REMOTE}" 2>&1 || true)"
  echo "${output}" >"${WORKDIR}/${name}.ping.log"
  echo "[${name}] ping log: ${WORKDIR}/${name}.ping.log"
  echo "[${name}] iperf3 client log: ${WORKDIR}/${name}.iperf-client.log"
  echo "[${name}] iperf3 server log: ${WORKDIR}/${name}.iperf-server.log"
  printf '%s\n' "${output}" | tail -n 2

  kill "${iperf_client}" >/dev/null 2>&1 || true
  wait "${iperf_client}" >/dev/null 2>&1 || true
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true

  local rtt_line
  rtt_line="$(printf '%s\n' "${output}" | grep -E '^rtt min/avg/max/mdev' | tail -n 1)"
  if [[ -n "${rtt_line}" ]]; then
    pass "${name}" "loaded-latency captured: ${rtt_line}"
  else
    fail "${name}" "ping under load did not produce an rtt summary"
  fi

  clear_loss
  stop_multipath
  echo "==== ${name} e2e end ===="
}

run_concurrent_fallback_case() {
  local name="concurrent-fallback"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_CONCURRENT_FALLBACK}" false false 200 600
  expect_ping_fail_for "${name} precheck" 2
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP on both path1 and path2 simultaneously; both lanes must fall back to TCP"
  apply_udp_tunnel_block_path 1 "${PORT_CONCURRENT_FALLBACK}"
  apply_udp_tunnel_block_path 2 "${PORT_CONCURRENT_FALLBACK}"

  wait_ping_ok "${name} dual-tcp-fallback" 25
  wait_log_pattern "${name}" "accept_hello_ack session=[0-9]+ lane=1 .*tcp conn=" 25 "lane=1 reached TCP HELLO_ACK with both UDP paths blocked"
  wait_log_pattern "${name}" "accept_hello_ack session=[0-9]+ lane=2 .*tcp conn=" 25 "lane=2 reached TCP HELLO_ACK with both UDP paths blocked"

  echo "[${name}] restore UDP; both lanes must come back to UDP"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15
  wait_log_pattern "${name}" "target_recovered .*lane=1" 15 "lane=1 probe target recovered after UDP restore"
  wait_log_pattern "${name}" "target_recovered .*lane=2" 15 "lane=2 probe target recovered after UDP restore"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_fallback_dial_error_case() {
  local name="fallback-dial-error"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_FALLBACK_DIAL_ERROR}" false false 200 600
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP on path1 and REJECT TCP at the server; lane must fail closed"
  apply_udp_tunnel_block_path 1 "${PORT_FALLBACK_DIAL_ERROR}"
  apply_tcp_server_reject "${PORT_FALLBACK_DIAL_ERROR}"

  wait_log_pattern "${name}" "fallback_result_err .*lane=1" 15 "client observed fallback_dial_error on lane=1"
  expect_ping_fail_for "${name} no-runnable" 3

  echo "[${name}] restore UDP and TCP; lane must come back online"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_weighted_scheduling_case() {
  local name="weighted-scheduling"
  local client_log
  local path1_weight=4
  local path2_weight=1
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_WEIGHTED}" false false 200 3000 "${path1_weight}" "${path2_weight}"
  start_multipath "${name}"
  client_log="${CURRENT_CLIENT_LOG}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] send a ping sample to exercise the weighted scheduler"
  ip netns exec "${NS_C}" ping -c 200 -i 0.05 -W 1 "${TUN_C_REMOTE}" >/dev/null 2>&1 || true

  stop_multipath

  local lane1_count
  local lane2_count
  local total
  lane1_count="$(grep -E -c "tun_done session=[0-9]+ packet_id=[0-9]+ lane=1 " "${client_log}" 2>/dev/null || true)"
  lane2_count="$(grep -E -c "tun_done session=[0-9]+ packet_id=[0-9]+ lane=2 " "${client_log}" 2>/dev/null || true)"
  lane1_count="${lane1_count:-0}"
  lane2_count="${lane2_count:-0}"
  total=$((lane1_count + lane2_count))
  echo "[${name}] lane1_count=${lane1_count} lane2_count=${lane2_count} total=${total}"

  if (( total < 50 )); then
    fail "${name}" "client emitted only ${total} tun_done lines; expected >=50 (MULTIPATH_DEBUG must be enabled)"
  elif awk -v l1="${lane1_count}" -v l2="${lane2_count}" -v w1="${path1_weight}" -v w2="${path2_weight}" 'BEGIN {
        total = l1 + l2;
        if (total == 0) { exit 1 }
        observed = l1 / total;
        target   = w1 / (w1 + w2);
        diff     = observed - target;
        if (diff < 0) { diff = -diff }
        exit !(diff <= 0.15)
      }'; then
    pass "${name}" "lane1 fraction ${lane1_count}/${total} matches target ${path1_weight}:${path2_weight} within tolerance"
  else
    fail "${name}" "lane1 fraction ${lane1_count}/${total} does not match target ${path1_weight}:${path2_weight}"
  fi

  clear_loss
  echo "==== ${name} e2e end ===="
}

run_mtu_case() {
  local name="mtu"
  local payload=1412
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_MTU}" false false 200 600
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] send near-MTU ping (-s ${payload} -M do) over UDP"
  if ip netns exec "${NS_C}" ping -c 5 -W 2 -s "${payload}" -M do "${TUN_C_REMOTE}" >/dev/null 2>&1; then
    pass "${name}" "near-MTU ping survived UDP transport"
  else
    fail "${name}" "near-MTU ping failed over UDP transport"
  fi

  echo "[${name}] block UDP and force TCP fallback"
  apply_udp_tunnel_block_path 1 "${PORT_MTU}"
  wait_ping_ok "${name} tcp-fallback" 20

  echo "[${name}] send near-MTU ping (-s ${payload} -M do) over TCP fallback"
  if ip netns exec "${NS_C}" ping -c 5 -W 2 -s "${payload}" -M do "${TUN_C_REMOTE}" >/dev/null 2>&1; then
    pass "${name}" "near-MTU ping survived TCP fallback"
  else
    fail "${name}" "near-MTU ping failed over TCP fallback"
  fi

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

build_bin
setup_netns

run_multipath_case
run_per_lane_fallback_case
run_concurrent_fallback_case
run_legacy_tcp_flag_case
run_fallback_case
run_server_restart_reconnect_case
run_fallback_dial_error_case
run_leg_selector_case
run_bandwidth_probe_convergence_case
run_bandwidth_probe_tcp_reference_case
run_nat_case
run_fec_comparison
run_fec_tcp_fallback_case
run_multipath_fec_case
run_fec_loaded_latency_case
run_weighted_scheduling_case
run_mtu_case

if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "real e2e failed (${FAIL_COUNT} failures)"
  exit 1
fi

echo "real e2e ok"
