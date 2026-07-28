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
E2E_DEBUG=1
FEC_PING_COUNT=1000
FEC_PING_INTERVAL=0.02
FEC_HIGH_RTT_DELAY=50ms
LINK_STATUS_QOS_LOSS=50%
LINK_STATUS_QOS_WAIT=35
LINK_STATUS_QOS_CLEAR_WAIT=120

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
    bash "$0" "$@"
fi

echo "real e2e workdir: ${WORKDIR}"
mkdir -p "${WORKDIR}"

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
PORT_BW_PROBE_DEFAULT_CAP=5018
PORT_LINK_STATUS_QOS=5019
PORT_UNKNOWN_SESSION_REBOOTSTRAP=5020
PORT_TCP_ESTABLISHED_REDIAL=5021
PORT_FEC_DISABLED_NEGOTIATION=5022
PORT_LINK_STATUS_QOS_REVERSE=5023
PORT_MULTILANE_REBOOTSTRAP=5025
PORT_NAT_TCP_FALLBACK=5026
PORT_MTU_FEC=5027
PORT_BW_PROBE_DISABLED=5028
PORT_TCP_CORRECTNESS=5029
PORT_UDP_CORRECTNESS=5030
PORT_LINK_STATUS_QOS_IPERF=5031
PORT_LINK_STATUS_QOS_IPERF_REPEAT=5032
PORT_LINK_STATUS_QOS_IPERF_JITTER=5033
PORT_LINK_STATUS_QOS_IPERF_RTT200=5034
PORT_TCP_FALLBACK_RATE_DYNAMIC=5035
PORT_LINK_STATUS_QOS_JITTER_NO_QOS=5036
PORT_FEC_ADAPTIVE_75=5037
PORT_BW_PROBE_UDP_RESTORE=5038

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
  require_command python3

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
  local bandwidth_probe_cap_bps="${7:-}"
  local bandwidth_probe_cap_json=""
  if [[ -n "${bandwidth_probe_cap_bps}" ]]; then
    bandwidth_probe_cap_json=",
  \"bandwidthProbeCapBps\": ${bandwidth_probe_cap_bps}"
  fi

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
  "probeTimeoutMS": ${probe_timeout_ms}${bandwidth_probe_cap_json}
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
  "probeTimeoutMS": ${probe_timeout_ms}${bandwidth_probe_cap_json}
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
  local min_bps="${2:-1000000}"
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${label}] iperf3 not found, skip throughput smoke"
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${label}] timeout not found, skip throughput smoke"
    return 0
  fi

  local server_log="${WORKDIR}/${label}.iperf-server.log"
  local client_log="${WORKDIR}/${label}.iperf-client.log"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >"${server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1
  echo "[${label}] iperf3 over TUN"
  timeout 8s ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t 3 -i 1 >"${client_log}" 2>&1 || true
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true
  echo "[${label}] iperf3 client log: ${client_log}"
  echo "[${label}] iperf3 server log: ${server_log}"
  tail -n 6 "${client_log}" || true

  local bps
  bps="$(parse_iperf_receiver_bps "${client_log}")"
  if [[ -z "${bps}" ]]; then
    fail "${label}" "iperf3 receiver bitrate was not parsed"
    return 0
  fi
  echo "[${label}] iperf3 receiver_bps=${bps}"
  if awk -v got="${bps}" -v min="${min_bps}" 'BEGIN { exit !(got >= min) }'; then
    pass "${label}" "iperf3 receiver bitrate ${bps} bps >= ${min_bps} bps"
  else
    fail "${label}" "iperf3 receiver bitrate ${bps} bps < ${min_bps} bps"
  fi
}

parse_iperf_receiver_bps() {
  local log_file="$1"
  if [[ ! -f "${log_file}" ]]; then
    return 0
  fi
  awk '
    /receiver/ && /bits\/sec/ {
      value = "";
      unit = "";
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^[0-9]+([.][0-9]+)?$/ && (i + 1) <= NF && $(i + 1) ~ /^[KMG]?bits\/sec$/) {
          value = $i + 0;
          unit = $(i + 1);
        }
      }
      if (value != "") {
        scale = 1;
        if (unit == "Kbits/sec") scale = 1000;
        if (unit == "Mbits/sec") scale = 1000000;
        if (unit == "Gbits/sec") scale = 1000000000;
        bps = value * scale;
      }
    }
    END {
      if (bps != "") {
        printf "%.0f\n", bps;
      }
    }
  ' "${log_file}"
}

parse_iperf_json_window_avg_bps() {
  local log_file="$1"
  local window_start="$2"
  local window_end="$3"
  if [[ ! -f "${log_file}" ]]; then
    return 0
  fi
  python3 - "${log_file}" "${window_start}" "${window_end}" <<'PY'
import json
import sys

path = sys.argv[1]
window_start = float(sys.argv[2])
window_end = float(sys.argv[3])
try:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception:
    sys.exit(0)

weighted_bps = 0.0
weighted_seconds = 0.0
for interval in data.get("intervals", []):
    summary = interval.get("sum") or interval.get("sum_received") or interval.get("sum_sent")
    if not summary:
        continue
    start = float(summary.get("start", 0.0))
    end = float(summary.get("end", start))
    bps = float(summary.get("bits_per_second", 0.0))
    overlap = max(0.0, min(end, window_end) - max(start, window_start))
    if overlap <= 0:
        continue
    weighted_bps += bps * overlap
    weighted_seconds += overlap

if weighted_seconds > 0:
    print(f"{weighted_bps / weighted_seconds:.0f}")
PY
}

parse_iperf_json_end_bps() {
  local log_file="$1"
  if [[ ! -f "${log_file}" ]]; then
    return 0
  fi
  python3 - "${log_file}" <<'PY'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception:
    sys.exit(0)

end = data.get("end") or {}
for key in ("sum_received", "sum", "sum_sent"):
    summary = end.get(key)
    if isinstance(summary, dict) and summary.get("bits_per_second") is not None:
        print(f'{float(summary["bits_per_second"]):.0f}')
        break
PY
}

assert_iperf_window_recovered() {
  local label="$1"
  local baseline="$2"
  local limited="$3"
  local recovered="$4"
  local min_recovered_vs_baseline="$5"
  local min_recovered_vs_limited="$6"
  local min_limited_vs_baseline="$7"

  echo "[${label}] iperf_window_bps baseline=${baseline} limited=${limited} recovered=${recovered}"
  if [[ -z "${baseline}" || -z "${limited}" || -z "${recovered}" ]]; then
    fail "${label}" "could not parse staged iperf window rates"
    return 1
  fi
  if awk \
      -v baseline="${baseline}" \
      -v limited="${limited}" \
      -v recovered="${recovered}" \
      -v min_rb="${min_recovered_vs_baseline}" \
      -v min_rl="${min_recovered_vs_limited}" \
      -v min_lb="${min_limited_vs_baseline}" '
        BEGIN {
          ok = baseline > 0 &&
               limited >= baseline * min_lb &&
               recovered >= baseline * min_rb &&
               recovered >= limited * min_rl
          exit !ok
        }'; then
    pass "${label}" "iperf throughput stayed usable and recovered after shaping cleared"
  else
    fail "${label}" "bad staged iperf rates: baseline=${baseline} limited=${limited} recovered=${recovered}"
    return 1
  fi
}

assert_iperf_window_limited_drop() {
  local label="$1"
  local baseline="$2"
  local limited="$3"
  local max_limited_vs_baseline="$4"

  if [[ -z "${baseline}" || -z "${limited}" ]]; then
    fail "${label}" "could not parse iperf drop windows"
    return 1
  fi
  if awk \
      -v baseline="${baseline}" \
      -v limited="${limited}" \
      -v max_lb="${max_limited_vs_baseline}" '
        BEGIN {
          ok = baseline > 0 && limited <= baseline * max_lb
          exit !ok
        }'; then
    pass "${label}" "limited window dropped as expected"
  else
    fail "${label}" "limited window did not drop enough: baseline=${baseline} limited=${limited}"
    return 1
  fi
}

assert_iperf_window_not_degraded() {
  local label="$1"
  local before="$2"
  local after="$3"
  local min_after_vs_before="$4"

  echo "[${label}] iperf_window_bps before=${before} after=${after}"
  if [[ -z "${before}" || -z "${after}" ]]; then
    fail "${label}" "could not parse staged iperf stability windows"
    return 1
  fi
  if awk \
      -v before="${before}" \
      -v after="${after}" \
      -v min_ab="${min_after_vs_before}" '
        BEGIN {
          ok = before > 0 && after >= before * min_ab
          exit !ok
        }'; then
    pass "${label}" "iperf throughput did not materially drop after UDP restore"
  else
    fail "${label}" "iperf throughput dropped after UDP restore: before=${before} after=${after}"
    return 1
  fi
}

assert_selector_switches_since_le() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local max_count="$4"
  local count
  count="$(count_log_file_pattern_since "${log_file}" "${start_line}" "selector action=qos_data_leg")"
  count="${count:-0}"
  echo "[${label}] selector_switch_count=${count} max=${max_count}"
  if (( count <= max_count )); then
    pass "${label}" "selector switch count stayed bounded"
  else
    fail "${label}" "selector switch count ${count} > ${max_count}"
  fi
}

assert_link_status_since_ge() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local pattern="$4"
  local min_count="$5"
  local message="$6"
  local count
  count="$(count_log_file_pattern_since "${log_file}" "${start_line}" "${pattern}")"
  count="${count:-0}"
  if (( count >= min_count )); then
    pass "${label}" "${message}: count=${count}"
  else
    fail "${label}" "${message}: count=${count}, want >=${min_count}"
  fi
}

write_tcp_correctness_tool() {
  local tool="${WORKDIR}/tcp_correctness.py"
  if [[ -f "${tool}" ]]; then
    printf '%s\n' "${tool}"
    return 0
  fi

  cat >"${tool}" <<'PY'
#!/usr/bin/env python3
import argparse
import hashlib
import socket
import struct
import sys
import time


def recv_exact(conn, size):
    data = bytearray()
    while len(data) < size:
        chunk = conn.recv(size - len(data))
        if not chunk:
            raise EOFError(f"short read: got {len(data)} want {size}")
        data.extend(chunk)
    return bytes(data)


def payload_for(size, seed):
    out = bytearray()
    block = hashlib.sha256(f"{seed}:{size}".encode("ascii")).digest()
    counter = 0
    while len(out) < size:
        block = hashlib.sha256(block + counter.to_bytes(8, "big")).digest()
        out.extend(block)
        counter += 1
    return bytes(out[:size])


def send_record(conn, payload):
    conn.sendall(struct.pack("!I", len(payload)))
    conn.sendall(payload)


def recv_record(conn, max_size):
    size = struct.unpack("!I", recv_exact(conn, 4))[0]
    if size > max_size:
        raise ValueError(f"record too large: {size} > {max_size}")
    return recv_exact(conn, size)


def run_server(args):
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.settimeout(args.timeout)
    listener.bind((args.bind, args.port))
    listener.listen(1)
    print(f"server listening bind={args.bind} port={args.port}", flush=True)
    conn, addr = listener.accept()
    with conn:
        conn.settimeout(args.timeout)
        count = struct.unpack("!I", recv_exact(conn, 4))[0]
        if count > args.max_records:
            raise ValueError(f"too many records: {count} > {args.max_records}")
        print(f"server accepted addr={addr} records={count}", flush=True)
        for idx in range(count):
            payload = recv_record(conn, args.max_size)
            digest = hashlib.sha256(payload).hexdigest()
            send_record(conn, payload)
            print(f"server echoed index={idx} size={len(payload)} sha256={digest}", flush=True)
    listener.close()


def run_client(args):
    sizes = [int(part) for part in args.sizes.split(",") if part]
    conn = socket.create_connection((args.host, args.port), timeout=args.timeout)
    with conn:
        conn.settimeout(args.timeout)
        conn.sendall(struct.pack("!I", len(sizes)))
        for idx, size in enumerate(sizes):
            payload = payload_for(size, idx + 1)
            digest = hashlib.sha256(payload).hexdigest()
            send_record(conn, payload)
            echoed = recv_record(conn, args.max_size)
            if echoed != payload:
                got = hashlib.sha256(echoed).hexdigest()
                raise ValueError(
                    f"echo mismatch index={idx} size={size} want_sha256={digest} got_size={len(echoed)} got_sha256={got}"
                )
            print(f"client verified index={idx} size={size} sha256={digest}", flush=True)


UDP_MAGIC = b"MPU1"
UDP_DONE = UDP_MAGIC + b"DONE"


def udp_packet_for(size, index):
    payload = payload_for(size, index + 1)
    return UDP_MAGIC + struct.pack("!II", index, size) + payload


def parse_udp_packet(packet):
    if len(packet) < 12 or packet[:4] != UDP_MAGIC:
        return None
    index, size = struct.unpack("!II", packet[4:12])
    payload = packet[12:]
    if len(payload) != size:
        return None
    return index, payload


def run_udp_server(args):
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.settimeout(args.idle_timeout)
    sock.bind((args.bind, args.port))
    print(f"udp server listening bind={args.bind} port={args.port}", flush=True)
    while True:
        try:
            packet, addr = sock.recvfrom(args.max_size + 12)
        except socket.timeout:
            print("udp server idle timeout", flush=True)
            break
        if packet == UDP_DONE:
            print("udp server done", flush=True)
            break
        parsed = parse_udp_packet(packet)
        if parsed is None:
            print(f"udp server ignored malformed size={len(packet)}", flush=True)
            continue
        index, payload = parsed
        sock.sendto(packet, addr)
        digest = hashlib.sha256(payload).hexdigest()
        print(f"udp server echoed index={index} size={len(payload)} sha256={digest}", flush=True)
    sock.close()


def run_udp_client(args):
    sizes = [int(part) for part in args.sizes.split(",") if part]
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(args.attempt_timeout)
    remote = (args.host, args.port)
    for index, size in enumerate(sizes):
        expected = udp_packet_for(size, index)
        expected_payload = expected[12:]
        digest = hashlib.sha256(expected_payload).hexdigest()
        verified = False
        for attempt in range(1, args.attempts + 1):
            sock.sendto(expected, remote)
            deadline = time.monotonic() + args.attempt_timeout
            while time.monotonic() < deadline:
                try:
                    echoed, _ = sock.recvfrom(args.max_size + 12)
                except socket.timeout:
                    break
                if echoed == expected:
                    print(
                        f"udp client verified index={index} size={size} attempt={attempt} sha256={digest}",
                        flush=True,
                    )
                    verified = True
                    break
                parsed = parse_udp_packet(echoed)
                if parsed is not None and parsed[0] < index:
                    continue
                got = hashlib.sha256(echoed[12:] if len(echoed) >= 12 else echoed).hexdigest()
                raise ValueError(
                    f"udp echo mismatch index={index} size={size} want_sha256={digest} "
                    f"got_size={len(echoed)} got_sha256={got}"
                )
            if verified:
                break
        if not verified:
            raise TimeoutError(f"udp echo timeout index={index} size={size} attempts={args.attempts}")
    for _ in range(3):
        sock.sendto(UDP_DONE, remote)
    sock.close()


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="mode", required=True)

    server = sub.add_parser("server")
    server.add_argument("--bind", required=True)
    server.add_argument("--port", required=True, type=int)
    server.add_argument("--timeout", type=float, default=60)
    server.add_argument("--max-records", type=int, default=64)
    server.add_argument("--max-size", type=int, default=8 * 1024 * 1024)

    client = sub.add_parser("client")
    client.add_argument("--host", required=True)
    client.add_argument("--port", required=True, type=int)
    client.add_argument("--sizes", required=True)
    client.add_argument("--timeout", type=float, default=60)
    client.add_argument("--max-size", type=int, default=8 * 1024 * 1024)

    udp_server = sub.add_parser("udp-server")
    udp_server.add_argument("--bind", required=True)
    udp_server.add_argument("--port", required=True, type=int)
    udp_server.add_argument("--idle-timeout", type=float, default=5)
    udp_server.add_argument("--max-size", type=int, default=65500)

    udp_client = sub.add_parser("udp-client")
    udp_client.add_argument("--host", required=True)
    udp_client.add_argument("--port", required=True, type=int)
    udp_client.add_argument("--sizes", required=True)
    udp_client.add_argument("--attempts", type=int, default=3)
    udp_client.add_argument("--attempt-timeout", type=float, default=2)
    udp_client.add_argument("--max-size", type=int, default=65500)

    args = parser.parse_args()
    if args.mode == "server":
        run_server(args)
    elif args.mode == "udp-server":
        run_udp_server(args)
    elif args.mode == "udp-client":
        run_udp_client(args)
    else:
        run_client(args)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr, flush=True)
        sys.exit(1)
PY
  chmod +x "${tool}"
  printf '%s\n' "${tool}"
}

run_tcp_correctness_transfer() {
  local label="$1"
  local sizes="$2"
  local timeout_s="${3:-60}"
  local app_port="${4:-16029}"
  local tool
  tool="$(write_tcp_correctness_tool)"

  local server_log="${WORKDIR}/${label}.tcp-server.log"
  local client_log="${WORKDIR}/${label}.tcp-client.log"
  echo "[${label}] TCP correctness over TUN: sizes=${sizes} timeout=${timeout_s}s"
  ip netns exec "${NS_S}" python3 -u "${tool}" server \
    --bind "${TUN_S_LOCAL}" \
    --port "${app_port}" \
    --timeout "${timeout_s}" \
    >"${server_log}" 2>&1 &
  local tcp_server_pid=$!
  sleep 0.5

  local client_status=0
  if ip netns exec "${NS_C}" python3 -u "${tool}" client \
      --host "${TUN_C_REMOTE}" \
      --port "${app_port}" \
      --sizes "${sizes}" \
      --timeout "${timeout_s}" \
      >"${client_log}" 2>&1; then
    client_status=0
  else
    client_status=$?
    kill "${tcp_server_pid}" >/dev/null 2>&1 || true
  fi

  local server_status=0
  if wait "${tcp_server_pid}"; then
    server_status=0
  else
    server_status=$?
  fi

  echo "[${label}] TCP correctness client log: ${client_log}"
  echo "[${label}] TCP correctness server log: ${server_log}"
  tail -n 6 "${client_log}" || true
  tail -n 6 "${server_log}" || true

  if (( client_status == 0 && server_status == 0 )); then
    pass "${label}" "TCP byte stream echoed correctly over TUN"
  else
    fail "${label}" "TCP correctness failed client_status=${client_status} server_status=${server_status}"
  fi
}

run_udp_correctness_transfer() {
  local label="$1"
  local sizes="$2"
  local attempts="${3:-3}"
  local attempt_timeout_s="${4:-2}"
  local app_port="${5:-16030}"
  local tool
  tool="$(write_tcp_correctness_tool)"

  local server_log="${WORKDIR}/${label}.udp-server.log"
  local client_log="${WORKDIR}/${label}.udp-client.log"
  echo "[${label}] UDP correctness over TUN: sizes=${sizes} attempts=${attempts} attempt_timeout=${attempt_timeout_s}s"
  ip netns exec "${NS_S}" python3 -u "${tool}" udp-server \
    --bind "${TUN_S_LOCAL}" \
    --port "${app_port}" \
    --idle-timeout 5 \
    >"${server_log}" 2>&1 &
  local udp_server_pid=$!
  sleep 0.5

  local client_status=0
  if ip netns exec "${NS_C}" python3 -u "${tool}" udp-client \
      --host "${TUN_C_REMOTE}" \
      --port "${app_port}" \
      --sizes "${sizes}" \
      --attempts "${attempts}" \
      --attempt-timeout "${attempt_timeout_s}" \
      >"${client_log}" 2>&1; then
    client_status=0
  else
    client_status=$?
    kill "${udp_server_pid}" >/dev/null 2>&1 || true
  fi

  local server_status=0
  if wait "${udp_server_pid}"; then
    server_status=0
  else
    server_status=$?
  fi

  echo "[${label}] UDP correctness client log: ${client_log}"
  echo "[${label}] UDP correctness server log: ${server_log}"
  tail -n 6 "${client_log}" || true
  tail -n 6 "${server_log}" || true

  if (( client_status == 0 && server_status == 0 )); then
    pass "${label}" "UDP datagrams echoed correctly over TUN"
  else
    fail "${label}" "UDP correctness failed client_status=${client_status} server_status=${server_status}"
  fi
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
  ip netns exec "${NS_C}" env MULTIPATH_DEBUG="${E2E_DEBUG}" "${CURRENT_EXTRA_ENV[@]}" "${BIN}" -config "${client_config}" >>"${client_log}" 2>&1 &
  CLIENT_PID=$!

  echo "[${name}] client log: ${client_log}"
  echo "[${name}] server log: ${server_log} (MULTIPATH_DEBUG=${E2E_DEBUG})"
}

start_server_process() {
  local name="$1"
  local server_config="${WORKDIR}/server-${name}.json"
  local server_log="${WORKDIR}/${name}.server.log"

  CURRENT_SERVER_LOG="${server_log}"
  ip netns exec "${NS_S}" env MULTIPATH_DEBUG="${E2E_DEBUG}" "${CURRENT_EXTRA_ENV[@]}" "${BIN}" -config "${server_config}" >>"${server_log}" 2>&1 &
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
  ip netns exec "${NS_C}" iptables -F OUTPUT >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" iptables -t mangle -F OUTPUT >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" iptables -t mangle -F OUTPUT >/dev/null 2>&1 || true
  ip netns exec "${NS_N}" iptables -t mangle -F OUTPUT >/dev/null 2>&1 || true
  ip netns exec "${NS_S}" iptables -F INPUT >/dev/null 2>&1 || true
}

setup_prio_qdisc() {
  local ns="$1"
  local dev="$2"
  ip netns exec "${ns}" tc qdisc del dev "${dev}" root >/dev/null 2>&1 || true
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

add_delay_jitter_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local delay="$5"
  local jitter="$6"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem delay "${delay}" "${jitter}" distribution normal
}

add_loss_delay_jitter_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local loss="$5"
  local delay="$6"
  local jitter="$7"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem delay "${delay}" "${jitter}" distribution normal loss "${loss}"
}

add_rate_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local rate="$5"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" tbf rate "${rate}" burst 1mbit latency 100ms
}

add_delay_jitter_rate_band() {
  local ns="$1"
  local dev="$2"
  local band="$3"
  local handle="$4"
  local delay="$5"
  local jitter="$6"
  local rate="$7"
  ip netns exec "${ns}" tc qdisc replace dev "${dev}" parent "1:${band}" handle "${handle}:" netem delay "${delay}" "${jitter}" distribution normal rate "${rate}"
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

add_mark_filter() {
  local ns="$1"
  local dev="$2"
  local prio="$3"
  local mark="$4"
  local band="$5"
  ip netns exec "${ns}" tc filter replace dev "${dev}" protocol ip parent 1:0 prio "${prio}" handle "${mark}" fw flowid "1:${band}"
}

add_udp_output_mark() {
  local ns="$1"
  local dev="$2"
  local field="$3"
  local port="$4"
  local mark="$5"

  case "${field}" in
  dport | sport)
    ;;
  *)
    echo "unsupported udp mark field: ${field}"
    exit 1
    ;;
  esac

  ip netns exec "${ns}" iptables -t mangle -A OUTPUT -o "${dev}" -p udp "--${field}" "${port}" -j MARK --set-mark "${mark}"
}

add_udp_port_large_packet_filter() {
  local ns="$1"
  local dev="$2"
  local prio="$3"
  local field="$4"
  local port="$5"
  local band="$6"

  ip netns exec "${ns}" tc filter replace dev "${dev}" protocol ip parent 1:0 prio "${prio}" u32 \
    match ip protocol 17 0xff \
    match ip "${field}" "${port}" 0xffff \
    match u16 0x0400 0xfc00 at 2 \
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

apply_udp_partial_loss_client_to_server_path() {
  local path="$1"
  local port="$2"
  local loss="$3"
  local client_dev
  client_dev="$(path_client_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 "${loss}"
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3
}

apply_udp_data_keep_one_of_four_client_to_server_path() {
  local path="$1"
  local port="$2"
  local client_dev
  client_dev="$(path_client_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 100%
  ip netns exec "${NS_C}" tc filter replace dev "${client_dev}" protocol ip parent 1:0 prio 1 handle 75 fw flowid 1:3

  ip netns exec "${NS_C}" iptables -t mangle -A OUTPUT -o "${client_dev}" \
    -p udp --dport "${port}" -m length --length 80:65535 \
    -m statistic --mode nth --every 4 --packet 0 \
    -j ACCEPT
  ip netns exec "${NS_C}" iptables -t mangle -A OUTPUT -o "${client_dev}" \
    -p udp --dport "${port}" -m length --length 80:65535 \
    -j MARK --set-mark 75
}

apply_udp_partial_loss_server_to_client_path() {
  local path="$1"
  local port="$2"
  local loss="$3"
  local server_dev
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 "${loss}"
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
}

apply_udp_large_packet_partial_loss() {
  local path="$1"
  local port="$2"
  local loss="$3"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 "${loss}"
  add_udp_port_large_packet_filter "${NS_C}" "${client_dev}" 1 dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 "${loss}"
  add_udp_port_large_packet_filter "${NS_S}" "${server_dev}" 1 sport "${port}" 3
}

apply_udp_tunnel_rate_path() {
  local path="$1"
  local port="$2"
  local rate="$3"
  local mark="0x301"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_rate_band "${NS_C}" "${client_dev}" 3 30 "${rate}"
  add_udp_output_mark "${NS_C}" "${client_dev}" dport "${port}" "${mark}"
  add_mark_filter "${NS_C}" "${client_dev}" 1 "${mark}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_rate_band "${NS_S}" "${server_dev}" 3 30 "${rate}"
  add_udp_output_mark "${NS_S}" "${server_dev}" sport "${port}" "${mark}"
  add_mark_filter "${NS_S}" "${server_dev}" 1 "${mark}" 3
}

apply_udp_tunnel_rate_with_delay_jitter_path() {
  local path="$1"
  local port="$2"
  local rate="$3"
  local delay="$4"
  local jitter="$5"
  local mark="0x301"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_delay_jitter_rate_band "${NS_C}" "${client_dev}" 3 30 "${delay}" "${jitter}" "${rate}"
  add_udp_output_mark "${NS_C}" "${client_dev}" dport "${port}" "${mark}"
  add_mark_filter "${NS_C}" "${client_dev}" 1 "${mark}" 3
  add_delay_jitter_band "${NS_C}" "${client_dev}" 4 40 "${delay}" "${jitter}"
  add_port_filter "${NS_C}" "${client_dev}" 2 tcp dport "${port}" 4

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_delay_jitter_rate_band "${NS_S}" "${server_dev}" 3 30 "${delay}" "${jitter}" "${rate}"
  add_udp_output_mark "${NS_S}" "${server_dev}" sport "${port}" "${mark}"
  add_mark_filter "${NS_S}" "${server_dev}" 1 "${mark}" 3
  add_delay_jitter_band "${NS_S}" "${server_dev}" 4 40 "${delay}" "${jitter}"
  add_port_filter "${NS_S}" "${server_dev}" 2 tcp sport "${port}" 4
}

apply_udp_block_tcp_rate_path() {
  local path="$1"
  local port="$2"
  local rate="$3"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_band "${NS_C}" "${client_dev}" 3 30 100%
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3
  add_rate_band "${NS_C}" "${client_dev}" 4 40 "${rate}"
  add_port_filter "${NS_C}" "${client_dev}" 2 tcp dport "${port}" 4

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_band "${NS_S}" "${server_dev}" 3 30 100%
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
  add_rate_band "${NS_S}" "${server_dev}" 4 40 "${rate}"
  add_port_filter "${NS_S}" "${server_dev}" 2 tcp sport "${port}" 4
}

apply_tunnel_delay_jitter_path() {
  local path="$1"
  local port="$2"
  local delay="$3"
  local jitter="$4"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_delay_jitter_band "${NS_C}" "${client_dev}" 3 30 "${delay}" "${jitter}"
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3
  add_port_filter "${NS_C}" "${client_dev}" 2 tcp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_delay_jitter_band "${NS_S}" "${server_dev}" 3 30 "${delay}" "${jitter}"
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
  add_port_filter "${NS_S}" "${server_dev}" 2 tcp sport "${port}" 3
}

apply_udp_tunnel_loss_delay_jitter_path() {
  local path="$1"
  local port="$2"
  local loss="$3"
  local delay="$4"
  local jitter="$5"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_loss_delay_jitter_band "${NS_C}" "${client_dev}" 3 30 "${loss}" "${delay}" "${jitter}"
  add_port_filter "${NS_C}" "${client_dev}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_loss_delay_jitter_band "${NS_S}" "${server_dev}" 3 30 "${loss}" "${delay}" "${jitter}"
  add_port_filter "${NS_S}" "${server_dev}" 1 udp sport "${port}" 3
}

apply_tcp_tunnel_delay_path() {
  local path="$1"
  local port="$2"
  local delay="$3"
  local client_dev server_dev
  client_dev="$(path_client_dev "${path}")"
  server_dev="$(path_server_dev "${path}")"

  setup_prio_qdisc "${NS_C}" "${client_dev}"
  add_delay_band "${NS_C}" "${client_dev}" 3 30 "${delay}"
  add_port_filter "${NS_C}" "${client_dev}" 1 tcp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${server_dev}"
  add_delay_band "${NS_S}" "${server_dev}" 3 30 "${delay}"
  add_port_filter "${NS_S}" "${server_dev}" 1 tcp sport "${port}" 3
}

apply_nat_tcp_block() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHCN}"
  add_loss_band "${NS_C}" "${VETHCN}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHCN}" 1 tcp dport "${port}" 3
}

apply_nat_udp_tunnel_block() {
  local port="$1"
  setup_prio_qdisc "${NS_C}" "${VETHCN}"
  add_loss_band "${NS_C}" "${VETHCN}" 3 30 100%
  add_port_filter "${NS_C}" "${VETHCN}" 1 udp dport "${port}" 3

  setup_prio_qdisc "${NS_S}" "${VETHSN}"
  add_loss_band "${NS_S}" "${VETHSN}" 3 30 100%
  add_port_filter "${NS_S}" "${VETHSN}" 1 udp sport "${port}" 3
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
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "send/dialer: dial err remote=" 20 "client observed TCP fallback dial failure while server was down"

  echo "[${name}] restart server with UDP still blocked; client must retry TCP fallback and reconnect"
  start_server_process "${name}"
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "send: tcp_dialed session=[0-9]+ lane=1 conn=" 20 "client retried TCP fallback after server restart"
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "client accepted TCP HELLO_ACK after server restart"
  wait_ping_ok "${name} post-restart-tcp" 20

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_unknown_session_rebootstrap_case() {
  local name="unknown-session-rebootstrap"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_UNKNOWN_SESSION_REBOOTSTRAP}" false false 100 300
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  local server_close_line
  local client_rebootstrap_line
  server_close_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_rebootstrap_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  echo "[${name}] restart server only; stale client PING must get CLOSE{UnknownSession} and rebuild a fresh session"
  restart_server_process "${name}"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "runtime: close_unknown_session session=[0-9]+ kind=1" 20 "server rejected stale session with CLOSE unknown_session" "${server_close_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: rebootstrap old_session=[0-9]+" 20 "client rebuilt after unknown-session CLOSE" "${client_rebootstrap_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=1 kind=1" 20 "client accepted HELLO_ACK for fresh session" "${client_rebootstrap_line}"
  wait_ping_ok "${name} post-rebootstrap" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_multilane_rebootstrap_case() {
  local name="multilane-rebootstrap"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_MULTILANE_REBOOTSTRAP}" false false 100 300
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=1" 12 "lane=1 UDP active before restart"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=2 kind=1" 12 "lane=2 UDP active before restart"

  local server_close_line
  local client_rebootstrap_line
  server_close_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_rebootstrap_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  echo "[${name}] restart server only; all bootstrap lanes must rebuild under a fresh session"
  restart_server_process "${name}"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "runtime: close_unknown_session session=[0-9]+ kind=1" 20 "server rejected stale multi-lane session with CLOSE unknown_session" "${server_close_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: rebootstrap old_session=[0-9]+" 20 "client rebuilt multi-lane session after unknown-session CLOSE" "${client_rebootstrap_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=1 kind=1" 20 "lane=1 accepted HELLO_ACK for fresh session" "${client_rebootstrap_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=2 kind=1" 20 "lane=2 accepted HELLO_ACK for fresh session" "${client_rebootstrap_line}"
  wait_ping_ok "${name} post-rebootstrap" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_leg_selector_case() {
  local name="leg-selector"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_LEG_SELECTOR}" false false 200 600 -1
  echo "[${name}] apply 50% large-packet UDP loss before startup; bandwidth probes should mark UDP QoS-limited without tripping PING liveness"
  apply_udp_large_packet_partial_loss 1 "${PORT_LEG_SELECTOR}" 50%
  local client_qos_start_line
  local server_qos_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_qos_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_qos_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "warm TCP fallback leg reached HELLO_ACK"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 45 "client produced UDP bandwidth sample" "${client_qos_start_line}"
  local client_select_start_line
  client_select_start_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*leg=\\{tcp .*frame=type=DATA" 20 "client leg selector chose TCP for DATA after UDP QoS detection" "${client_select_start_line}" "${NS_C}" "${TUN_C_REMOTE}"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 45 "server produced UDP bandwidth sample" "${server_qos_start_line}"
  local server_select_start_line
  server_select_start_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_SERVER_LOG}" "schedule_select.*leg=\\{tcp .*frame=type=DATA" 20 "server leg selector chose TCP for DATA after UDP QoS detection" "${server_select_start_line}" "${NS_S}" "${TUN_S_REMOTE}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_tcp_correctness_case() {
  local name="tcp-correctness"
  local app_port=16029
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_TCP_CORRECTNESS}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "lane=1 TCP shadow leg reached HELLO_ACK"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=2 kind=2" 20 "lane=2 TCP shadow leg reached HELLO_ACK"

  run_tcp_correctness_transfer "${name}-baseline" "1,17,257,1200,4096,65536,524288,1048576" 60 "${app_port}"

  echo "[${name}] apply medium RTT jitter on both tunnel paths"
  clear_loss
  apply_tunnel_delay_jitter_path 1 "${PORT_TCP_CORRECTNESS}" 40ms 20ms
  apply_tunnel_delay_jitter_path 2 "${PORT_TCP_CORRECTNESS}" 70ms 35ms
  run_tcp_correctness_transfer "${name}-medium-rtt-jitter" "1,64,1200,32768,262144" 90 "${app_port}"

  echo "[${name}] apply large RTT jitter on both tunnel paths"
  clear_loss
  apply_tunnel_delay_jitter_path 1 "${PORT_TCP_CORRECTNESS}" 120ms 60ms
  apply_tunnel_delay_jitter_path 2 "${PORT_TCP_CORRECTNESS}" 180ms 90ms
  run_tcp_correctness_transfer "${name}-large-rtt-jitter" "1,1200,32768,131072" 120 "${app_port}"

  echo "[${name}] apply UDP QoS loss plus RTT jitter while TCP shadow stays clean"
  clear_loss
  apply_udp_tunnel_loss_delay_jitter_path 1 "${PORT_TCP_CORRECTNESS}" 20% 50ms 25ms
  apply_udp_tunnel_loss_delay_jitter_path 2 "${PORT_TCP_CORRECTNESS}" 20% 80ms 40ms
  run_tcp_correctness_transfer "${name}-udp-qos-jitter" "1,64,1200,16384,131072" 120 "${app_port}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_udp_correctness_case() {
  local name="udp-correctness"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_two_lane_config "${name}" "${PORT_UDP_CORRECTNESS}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "lane=1 TCP shadow leg reached HELLO_ACK"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=2 kind=2" 20 "lane=2 TCP shadow leg reached HELLO_ACK"

  run_udp_correctness_transfer "${name}-baseline" "1,17,257,1200,1400" 3 2 16030

  echo "[${name}] apply medium RTT jitter on both tunnel paths"
  clear_loss
  apply_tunnel_delay_jitter_path 1 "${PORT_UDP_CORRECTNESS}" 40ms 20ms
  apply_tunnel_delay_jitter_path 2 "${PORT_UDP_CORRECTNESS}" 70ms 35ms
  run_udp_correctness_transfer "${name}-medium-rtt-jitter" "1,64,1200,1400" 5 3 16031

  echo "[${name}] apply large RTT jitter on both tunnel paths"
  clear_loss
  apply_tunnel_delay_jitter_path 1 "${PORT_UDP_CORRECTNESS}" 120ms 60ms
  apply_tunnel_delay_jitter_path 2 "${PORT_UDP_CORRECTNESS}" 180ms 90ms
  run_udp_correctness_transfer "${name}-large-rtt-jitter" "1,1200,1400" 5 4 16032

  echo "[${name}] apply UDP QoS loss plus RTT jitter while TCP shadow stays clean"
  clear_loss
  apply_udp_tunnel_loss_delay_jitter_path 1 "${PORT_UDP_CORRECTNESS}" 20% 50ms 25ms
  apply_udp_tunnel_loss_delay_jitter_path 2 "${PORT_UDP_CORRECTNESS}" 20% 80ms 40ms
  run_udp_correctness_transfer "${name}-udp-qos-jitter" "1,64,1200" 20 4 16033

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_convergence_case() {
  local name="bandwidth-probe-convergence"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_CONVERGENCE}" false false 200 1000 -1
  echo "[${name}] rate-limit UDP tunnel before startup; bandwidth probe should classify UDP relative to TCP"
  apply_udp_tunnel_rate_path 1 "${PORT_BW_PROBE_CONVERGENCE}" 80mbit
  local client_start_line
  local server_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"

  wait_ping_ok "${name} baseline-under-rate-limit" 12
  wait_bandwidth_probe_train_budget "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 25 "client TCP BW_PROBE used train-level budget" 20000000 32768 32768
  wait_client_tcp_reference_probe "${name}" "${client_start_line}"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" "client BW gate did not rely on remote timeout"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_SERVER_LOG}" "${server_start_line}" "server BW gate did not rely on remote timeout"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 35 "client measured UDP bandwidth after TCP reference" "${client_start_line}"
  wait_ping_ok "${name} post-convergence" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_tcp_reference_case() {
  local name="bandwidth-probe-tcp-reference"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_GUARD}" false false 200 1000 -1
  echo "[${name}] apply 20mbit UDP tunnel bottleneck; bandwidth probe should classify UDP relative to TCP reference"
  apply_udp_tunnel_rate_path 1 "${PORT_BW_PROBE_GUARD}" 20mbit
  local client_start_line
  local server_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"
  wait_ping_ok "${name} baseline" 12
  wait_bandwidth_probe_train_budget "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 25 "client TCP BW_PROBE used train-level budget" 20000000 32768 32768
  wait_client_tcp_reference_probe "${name}" "${client_start_line}"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" "client BW gate did not rely on remote timeout"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_SERVER_LOG}" "${server_start_line}" "server BW gate did not rely on remote timeout"
  wait_bandwidth_probe_udp_rate_window "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 40 "client UDP probe measured veth throughput" 10000000 60000000 200000000
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 35 "client measured UDP bandwidth after TCP reference" "${client_start_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision side=send .*prefer_tcp=true selected_leg=tcp" 10 "client classified UDP below TCP reference and selected TCP" "${client_start_line}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_udp_restore_iperf_case() {
  local name="bandwidth-probe-udp-restore-iperf"
  local port="${PORT_BW_PROBE_UDP_RESTORE}"
  local rate="20mbit"
  local duration=56
  local clear_at=28

  echo "==== ${name} e2e start ===="
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${name}] iperf3 not found, skip bandwidth-probe UDP restore case"
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${name}] timeout not found, skip bandwidth-probe UDP restore case"
    echo "==== ${name} e2e end ===="
    return 0
  fi

  clear_loss
  write_one_lane_config "${name}" "${port}" false true 200 3000 -1
  echo "[${name}] apply ${rate} UDP tunnel bottleneck before startup; bandwidth probe should prefer TCP"
  apply_udp_tunnel_rate_path 1 "${port}" "${rate}"

  local client_start_line
  local server_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"

  wait_ping_ok "${name} baseline-under-rate-limit" 12
  wait_bandwidth_probe_train_budget "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 25 "client TCP BW_PROBE used train-level budget" 20000000 32768 32768
  wait_client_tcp_reference_probe "${name}" "${client_start_line}"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" "client BW gate did not rely on remote timeout"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_SERVER_LOG}" "${server_start_line}" "server BW gate did not rely on remote timeout"
  wait_bandwidth_probe_udp_rate_window "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 40 "client UDP probe recorded the startup bottleneck" 10000000 120000000 200000000
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision side=send .*prefer_tcp=true selected_leg=tcp" 10 "client classified UDP below TCP reference and selected TCP" "${client_start_line}"

  local client_tcp_line
  client_tcp_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client sent DATA over TCP after bandwidth probe preference" "${client_tcp_line}" "${NS_C}" "${TUN_C_REMOTE}"

  local iperf_server_log="${WORKDIR}/${name}.iperf-server.log"
  local iperf_client_json="${WORKDIR}/${name}.iperf-client.json"
  local iperf_client_err="${WORKDIR}/${name}.iperf-client.err"
  echo "[${name}] start iperf3 over TCP-selected TUN path: duration=${duration}s"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >"${iperf_server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1
  local iperf_start
  iperf_start="${SECONDS}"
  timeout "$((duration + 8))s" ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t "${duration}" -i 1 -J \
    >"${iperf_client_json}" 2>"${iperf_client_err}" &
  local iperf_client=$!

  if ! wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "bandwidth_probe_decision side=recv .*prefer_tcp=true selected_leg=tcp" 10 "server receive-side bandwidth decision classified client UDP below TCP reference" "${server_start_line}"; then
    kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
    wait "${iperf_client}" >/dev/null 2>&1 || true
    wait "${iperf_server}" >/dev/null 2>&1 || true
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "bandwidth_probe_decision side=recv .*prefer_tcp=true selected_leg=tcp" 10 "client receive-side bandwidth decision classified server UDP below TCP reference" "${client_start_line}"; then
    kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
    wait "${iperf_client}" >/dev/null 2>&1 || true
    wait "${iperf_server}" >/dev/null 2>&1 || true
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  while (( SECONDS < iperf_start + clear_at - 3 )); do
    sleep 1
  done
  local before_clear_line
  before_clear_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  if ! wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 3 "client kept sending DATA over TCP before UDP restore" "${before_clear_line}" "${NS_C}" "${TUN_C_REMOTE}"; then
    kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
    wait "${iperf_client}" >/dev/null 2>&1 || true
    wait "${iperf_server}" >/dev/null 2>&1 || true
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  while (( SECONDS < iperf_start + clear_at )); do
    sleep 1
  done
  echo "[${name}] clear UDP rate limit at iperf_elapsed=${clear_at}s while iperf3 is still running"
  clear_loss
  local after_clear_line
  after_clear_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  local client_status=0
  set +e
  wait "${iperf_client}"
  client_status=$?
  set -e
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true

  echo "[${name}] iperf3 client json: ${iperf_client_json}"
  echo "[${name}] iperf3 client err: ${iperf_client_err}"
  echo "[${name}] iperf3 server log: ${iperf_server_log}"
  if (( client_status != 0 )); then
    fail "${name}" "iperf3 client failed status=${client_status}"
  fi

  local tcp_selected_bps
  local udp_restored_bps
  tcp_selected_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 5 20)"
  udp_restored_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 38 52)"
  if ! assert_iperf_window_not_degraded "${name}" "${tcp_selected_bps}" "${udp_restored_bps}" 0.75; then
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  if ! wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{udp .*frame=type=DATA" 20 "client sent DATA over UDP after UDP restore" "${after_clear_line}" "${NS_C}" "${TUN_C_REMOTE}"; then
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! wait_ping_ok "${name} post-udp-restore" 12; then
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_default_cap_case() {
  local name="bandwidth-probe-default-cap"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_DEFAULT_CAP}" false false 200 600
  echo "[${name}] apply 50% large-packet UDP loss before startup; default 200mbit cap should classify UDP without TCP reference probe"
  apply_udp_large_packet_partial_loss 1 "${PORT_BW_PROBE_DEFAULT_CAP}" 50%
  local client_start_line
  local server_start_line
  local client_log_file="${WORKDIR}/${name}.client.log"
  local server_log_file="${WORKDIR}/${name}.server.log"
  client_start_line="$(current_log_file_line_count "${client_log_file}")"
  server_start_line="$(current_log_file_line_count "${server_log_file}")"
  start_multipath "${name}"

  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "warm TCP fallback leg reached HELLO_ACK"
  wait_bandwidth_probe_train_budget "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" 20 "client UDP BW_PROBE used default-cap train-level budget" 20000000 1200 1400
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 20 "client started default-capped UDP bandwidth probe" "${client_start_line}"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_CLIENT_LOG}" "${client_start_line}" "client BW gate did not rely on remote timeout"
  assert_no_bandwidth_probe_remote_timeout_since "${name}" "${CURRENT_SERVER_LOG}" "${server_start_line}" "server BW gate did not rely on remote timeout"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=1" 35 "client measured UDP bandwidth against default cap without TCP probe sample" "${client_start_line}"
  local client_select_start_line
  client_select_start_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*leg=\\{tcp .*frame=type=DATA" 20 "client leg selector chose TCP after default-cap QoS detection" "${client_select_start_line}" "${NS_C}" "${TUN_C_REMOTE}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_bandwidth_probe_disabled_case() {
  local name="bandwidth-probe-disabled"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_BW_PROBE_DISABLED}" false false 100 300 -1
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  run_short_ping_load "${name}-load" 180 0.02

  assert_log_file_not_contains "${name}" "${CURRENT_CLIENT_LOG}" "send/bw:" "client did not run bandwidth probe when disabled"
  assert_log_file_not_contains "${name}" "${CURRENT_SERVER_LOG}" "send/bw:" "server did not run bandwidth probe when disabled"
  wait_ping_ok "${name} post-load" 12

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

run_nat_tcp_fallback_case() {
  local name="nat-tcp-fallback"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_nat_config "${name}" "${PORT_NAT_TCP_FALLBACK}" 200 600
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"
  wait_ping_ok "${name} udp-through-snat" 15
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK through NAT"

  local client_fallback_line
  client_fallback_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  echo "[${name}] block UDP tunnel through NAT; TCP fallback must reconnect over the same SNAT path"
  apply_nat_udp_tunnel_block "${PORT_NAT_TCP_FALLBACK}"
  wait_ping_ok "${name} tcp-fallback-through-snat" 25
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 25 "client sent DATA over TCP fallback through NAT" "${client_fallback_line}"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_ping_sample() {
  run_ping_sample_from "$1" "${NS_C}" "${TUN_C_REMOTE}"
}

run_ping_sample_from() {
  local label="$1"
  local ns="$2"
  local remote="$3"
  local count="${FEC_PING_COUNT}"
  local interval="${FEC_PING_INTERVAL}"
  local output
  echo "[${label}] ping sample: ns=${ns} remote=${remote} count=${count} interval=${interval}s"
  output="$(ip netns exec "${ns}" ping -c "${count}" -i "${interval}" -W 1 "${remote}" 2>&1 || true)"
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

run_short_ping_load() {
  run_short_ping_load_from "$1" "${NS_C}" "${TUN_C_REMOTE}" "${2:-180}" "${3:-0.02}"
}

run_short_ping_load_from() {
  local label="$1"
  local ns="$2"
  local remote="$3"
  local count="${4:-180}"
  local interval="${5:-0.02}"
  echo "[${label}] short ping load: ns=${ns} remote=${remote} count=${count} interval=${interval}s"
  ip netns exec "${ns}" ping -c "${count}" -i "${interval}" -W 1 "${remote}" >/dev/null 2>&1 || true
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

count_log_file_pattern_since() {
  local log_file="$1"
  local start_line="$2"
  local pattern="$3"
  if [[ ! -f "${log_file}" ]]; then
    printf '0\n'
    return 0
  fi
  tail -n "+$((start_line + 1))" "${log_file}" | grep -E -c "${pattern}" || true
}

assert_log_file_pattern_count_since_ge() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local pattern="$4"
  local min_count="$5"
  local message="$6"
  local count
  count="$(count_log_file_pattern_since "${log_file}" "${start_line}" "${pattern}")"
  count="${count:-0}"
  if (( count >= min_count )); then
    pass "${label}" "${message}: count=${count}"
  else
    fail "${label}" "${message}: count=${count}, want >=${min_count}; pattern=${pattern}"
  fi
}

assert_log_file_pattern_count_since_le() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local pattern="$4"
  local max_count="$5"
  local message="$6"
  local count
  count="$(count_log_file_pattern_since "${log_file}" "${start_line}" "${pattern}")"
  count="${count:-0}"
  if (( count <= max_count )); then
    pass "${label}" "${message}: count=${count}"
  else
    fail "${label}" "${message}: count=${count}, want <=${max_count}; pattern=${pattern}"
  fi
}

assert_fec_groups_scaled_tcp_repairs_since() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local full_repair_count="$4"
  local output status

  set +e
  output="$(python3 - "${log_file}" "${start_line}" "${full_repair_count}" <<'PY'
import re
import sys

path = sys.argv[1]
start = int(sys.argv[2])
full_repair_count = int(sys.argv[3])
data = {}
repairs = {}

field_re = {}


def field(line, name):
    regex = field_re.get(name)
    if regex is None:
        regex = re.compile(r"\b" + re.escape(name) + r"=([0-9]+)")
        field_re[name] = regex
    match = regex.search(line)
    if not match:
        return None
    return int(match.group(1))


def scaled_count(repair_count, source_span):
    if repair_count <= 0:
        repair_count = 1
    if repair_count > 4:
        repair_count = 4
    count = (repair_count * source_span + 3) // 4
    if count < 1:
        return 1
    if count > source_span:
        return source_span
    return count


try:
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line_no, line in enumerate(f, 1):
            if line_no <= start:
                continue
            if "recv: frame_in type=DATA " in line and "leg={udp " in line:
                lane = field(line, "lane")
                group_id = field(line, "group_id")
                source_index = field(line, "source_index")
                if None not in (lane, group_id, source_index):
                    data.setdefault(lane, set()).add((group_id, source_index))
                continue
            if "recv: frame_in type=REPAIR " in line and "leg={tcp " in line:
                lane = field(line, "lane")
                group_id = field(line, "group_id")
                key = field(line, "key")
                span = field(line, "source_span")
                if None not in (lane, group_id, key, span):
                    repairs.setdefault((lane, group_id, span), set()).add(key)
except FileNotFoundError:
    print(f"log file missing: {path}")
    sys.exit(2)

best = None
match = None
oversent = None
for (lane, group_id, span), keys in sorted(repairs.items()):
    udp_data = sum(1 for source_index in range(span) if (group_id, source_index) in data.get(lane, set()))
    got = len(keys)
    want = scaled_count(full_repair_count, span)
    candidate = (got, udp_data, lane, group_id, span, want)
    if best is None or candidate > best:
        best = candidate
    if got > want:
        oversent = candidate
    elif got == want and udp_data < span and match is None:
        match = candidate

if oversent is not None:
    got, udp_data, lane, group_id, span, want = oversent
    print(f"oversent lane={lane} group_id={group_id} source_span={span} udp_data={udp_data} tcp_repairs={got} want={want}")
    sys.exit(1)

if match is not None:
    got, udp_data, lane, group_id, span, want = match
    print(f"ok lane={lane} group_id={group_id} source_span={span} udp_data={udp_data} tcp_repairs={got} want={want}")
    sys.exit(0)

if best is None:
    print("no TCP REPAIR group found")
else:
    got, udp_data, lane, group_id, span, want = best
    print(f"best lane={lane} group_id={group_id} source_span={span} udp_data={udp_data} tcp_repairs={got} want={want}")
sys.exit(1)
PY
)"
  status=$?
  set -e

  if (( status == 0 )); then
    pass "${label}" "server observed scaled TCP REPAIR count for an actually observed FEC group: ${output#ok }"
  else
    fail "${label}" "server did not observe a correctly scaled TCP REPAIR group: ${output}"
  fi
}

assert_fec_span_two_scaled_tcp_repairs_since() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local full_repair_count="$4"
  local output status

  set +e
  output="$(python3 - "${log_file}" "${start_line}" "${full_repair_count}" <<'PY'
import re
import sys

path = sys.argv[1]
start = int(sys.argv[2])
full_repair_count = int(sys.argv[3])
repairs = {}
field_re = {}


def field(line, name):
    regex = field_re.get(name)
    if regex is None:
        regex = re.compile(r"\b" + re.escape(name) + r"=([0-9]+)")
        field_re[name] = regex
    match = regex.search(line)
    if not match:
        return None
    return int(match.group(1))


def scaled_count(repair_count, source_span):
    if repair_count <= 0:
        repair_count = 1
    if repair_count > 4:
        repair_count = 4
    count = (repair_count * source_span + 3) // 4
    if count < 1:
        return 1
    if count > source_span:
        return source_span
    return count


try:
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line_no, line in enumerate(f, 1):
            if line_no <= start:
                continue
            if "recv: frame_in type=REPAIR " not in line or "leg={tcp " not in line:
                continue
            lane = field(line, "lane")
            group_id = field(line, "group_id")
            key = field(line, "key")
            span = field(line, "source_span")
            if None not in (lane, group_id, key, span):
                repairs.setdefault((lane, group_id, span), set()).add(key)
except FileNotFoundError:
    print(f"log file missing: {path}")
    sys.exit(2)

best = None
match = None
oversent = None
for (lane, group_id, span), keys in sorted(repairs.items()):
    if span != 2:
        continue
    got = len(keys)
    want = scaled_count(full_repair_count, span)
    candidate = (got, lane, group_id, span, want)
    if best is None or candidate > best:
        best = candidate
    if got > want:
        oversent = candidate
    elif got == want and match is None:
        match = candidate

if oversent is not None:
    got, lane, group_id, span, want = oversent
    print(f"oversent lane={lane} group_id={group_id} source_span={span} tcp_repairs={got} want={want}")
    sys.exit(1)

if match is not None:
    got, lane, group_id, span, want = match
    print(f"ok lane={lane} group_id={group_id} source_span={span} tcp_repairs={got}")
    sys.exit(0)

if best is None:
    print("no source_span=2 TCP REPAIR group found")
else:
    got, lane, group_id, span, want = best
    print(f"best lane={lane} group_id={group_id} source_span={span} tcp_repairs={got} want={want}")
sys.exit(1)
PY
)"
  status=$?
  set -e

  if (( status == 0 )); then
    pass "${label}" "server observed scaled TCP REPAIR count for source_span=2 FEC group: ${output#ok }"
  else
    fail "${label}" "server did not observe scaled TCP REPAIR count for source_span=2 FEC group: ${output}"
  fi
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
        /send\/bw: sample/ && /kind=1/ {
          for (i = 1; i <= NF; i++) {
            if ($i ~ /^bps=/) {
              split($i, parts, "=")
              window_bps = parts[2] + 0
              finish = 1
            }
          }
        }
        END {
          if (!finish) {
            print "need udp sample"
            exit 2
          }
          if (window_bps < min_window || window_bps > max_window) {
            printf("bad-window window_bps=%d\n", window_bps)
            exit 1
          }
          if (window_bps > max_target) {
            printf("bad-target window_bps=%d max_target_bps=%d\n", window_bps, max_target)
            exit 1
          }
          printf("ok window_bps=%d\n", window_bps)
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
  fail "${label}" "${message}: UDP sample not observed within ${timeout}s"
  return 1
}

wait_bandwidth_probe_train_budget() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local timeout="${4:-25}"
  local message="$5"
  local min_total="$6"
  local min_payload="$7"
  local max_payload="$8"
  local deadline=$((SECONDS + timeout))
  local output status

  while (( SECONDS < deadline )); do
    set +e
    output="$(awk \
      -v start="${start_line}" \
      -v min_total="${min_total}" \
      -v min_payload="${min_payload}" \
      -v max_payload="${max_payload}" '
        NR <= start { next }
        /protocol: encode type=BW_PROBE/ {
          train_total = -1
          train_remaining = -1
          payload_len = -1
          for (i = 1; i <= NF; i++) {
            if ($i ~ /^train_total=/) {
              split($i, parts, "=")
              train_total = parts[2] + 0
            } else if ($i ~ /^train_remaining=/) {
              split($i, parts, "=")
              train_remaining = parts[2] + 0
            } else if ($i ~ /^payload_len=/) {
              split($i, parts, "=")
              payload_len = parts[2] + 0
            }
          }
          if (payload_len < min_payload || payload_len > max_payload) {
            next
          }
          if (train_total < min_total) {
            message = sprintf("bad-train-budget train_total=%d train_remaining=%d payload_len=%d min_total=%d", train_total, train_remaining, payload_len, min_total)
            status = 1
            done = 1
            exit
          }
          if (train_remaining <= 0 || train_remaining >= train_total) {
            message = sprintf("bad-train-remaining train_total=%d train_remaining=%d payload_len=%d", train_total, train_remaining, payload_len)
            status = 1
            done = 1
            exit
          }
          message = sprintf("ok train_total=%d train_remaining=%d payload_len=%d", train_total, train_remaining, payload_len)
          status = 0
          done = 1
          exit
        }
        END {
          if (!done) {
            message = "need matching BW_PROBE encode"
            status = 2
          }
          print message
          exit status
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
  fail "${label}" "${message}: matching BW_PROBE encode not observed within ${timeout}s"
  return 1
}

assert_no_bandwidth_probe_remote_timeout_since() {
  local label="$1"
  local log_file="$2"
  local start_line="$3"
  local message="$4"
  local pattern="bw action=remote_timeout|gate_remote_timeout"
  if [[ -z "${log_file}" || ! -f "${log_file}" ]]; then
    fail "${label}" "${message}: log file missing"
    return 1
  fi
  if tail -n "+$((start_line + 1))" "${log_file}" | grep -E "${pattern}" >/dev/null; then
    fail "${label}" "${message}: unexpected BW remote timeout found"
    return 1
  fi
  pass "${label}" "${message}"
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

assert_log_file_not_contains() {
  local label="$1"
  local log_file="$2"
  local pattern="$3"
  local message="$4"
  if [[ -z "${log_file}" || ! -f "${log_file}" ]]; then
    fail "${label}" "${message}: log file missing"
    return
  fi
  if grep -E -q "${pattern}" "${log_file}"; then
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

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 25 "client warmed TCP reference leg" "${start_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send/bw: sample session=[0-9]+ lane=1 kind=2" 25 "client finished TCP reference probe" "${start_line}"
  assert_client_tcp_reference_before_udp_probe "${name}" "${start_line}"
}

assert_client_tcp_reference_before_udp_probe() {
  local label="$1"
  local start_line="$2"
  local output status

  set +e
  output="$(awk -v start="${start_line}" '
    NR <= start { next }
    /send\/bw: sample/ && /kind=1/ {
      message = "udp sample before tcp sample: " $0
      status = 1
      done = 1
      exit
    }
    /send\/bw: sample/ && /kind=2/ {
      message = "tcp sample before udp sample"
      status = 0
      done = 1
      exit
    }
    END {
      if (!done) {
        message = "need tcp sample"
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
    fail "${label}" "client UDP bandwidth probe completed before TCP reference completed: ${output}"
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
  if log_file_has_any_pattern_since "${log_file}" "${start_line}" "$@"; then
    pass "${label}" "${message}"
    return 0
  fi
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
    if tail -n "+$((start_line + 1))" "${log_file}" | grep -E "${pattern}" >/dev/null; then
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
  local grep_cmd
  if [[ -z "${start_line}" || "${start_line}" == "0" ]]; then
    grep_cmd() { grep -E -q "${pattern}" "${log_file}"; }
  else
    grep_cmd() { tail -n "+$((start_line + 1))" "${log_file}" | grep -E "${pattern}" >/dev/null; }
  fi
  while (( SECONDS < deadline )); do
    if [[ -f "${log_file}" ]] && grep_cmd; then
      pass "${label}" "${message}"
      return 0
    fi
    if ! check_multipath_alive "${label}"; then
      return 1
    fi
    ping_once_from "${ping_ns}" "${ping_remote}" || true
    if [[ -f "${log_file}" ]] && grep_cmd; then
      pass "${label}" "${message}"
      return 0
    fi
    sleep 0.2
  done
  if [[ -f "${log_file}" ]] && grep_cmd; then
    pass "${label}" "${message}"
    return 0
  fi
  echo "[${label}] traffic probe debug: ns=${ping_ns} remote=${ping_remote}"
  ip netns exec "${ping_ns}" ip -4 route get "${ping_remote}" || true
  ip netns exec "${ping_ns}" ping -c 1 -W 1 "${ping_remote}" || true
  if [[ -f "${log_file}" ]] && grep_cmd; then
    pass "${label}" "${message}"
    return 0
  fi
  fail "${label}" "${message}: pattern not seen within ${timeout}s: ${pattern}"
  return 1
}

wait_log_file_pattern_while_iperf_from() {
  local label="$1"
  local log_file="$2"
  local pattern="$3"
  local timeout="${4:-15}"
  local message="$5"
  local start_line="${6:-0}"
  local server_ns="$7"
  local server_bind="$8"
  local client_ns="$9"
  local client_remote="${10}"
  if [[ -z "${log_file}" ]]; then
    fail "${label}" "${message}: log file unset"
    return 1
  fi
  if ! command -v iperf3 >/dev/null 2>&1; then
    fail "${label}" "${message}: iperf3 not found"
    return 1
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    fail "${label}" "${message}: timeout not found"
    return 1
  fi

  local iperf_server_log="${WORKDIR}/${label}.wait-iperf-server.log"
  local iperf_client_log="${WORKDIR}/${label}.wait-iperf-client.log"
  echo "[${label}] drive iperf3 while waiting: ${client_ns}->${client_remote}, server=${server_ns}/${server_bind}"
  ip netns exec "${server_ns}" iperf3 -s -1 -B "${server_bind}" >"${iperf_server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1
  timeout "$((timeout + 5))s" ip netns exec "${client_ns}" iperf3 -c "${client_remote}" -t "${timeout}" -i 1 >"${iperf_client_log}" 2>&1 &
  local iperf_client=$!

  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if log_file_has_any_pattern_since "${log_file}" "${start_line}" "${pattern}"; then
      kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
      wait "${iperf_client}" >/dev/null 2>&1 || true
      wait "${iperf_server}" >/dev/null 2>&1 || true
      pass "${label}" "${message}"
      return 0
    fi
    if ! check_multipath_alive "${label}"; then
      kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
      wait "${iperf_client}" >/dev/null 2>&1 || true
      wait "${iperf_server}" >/dev/null 2>&1 || true
      return 1
    fi
    if ! kill -0 "${iperf_client}" >/dev/null 2>&1; then
      break
    fi
    sleep 0.2
  done
  if log_file_has_any_pattern_since "${log_file}" "${start_line}" "${pattern}"; then
    kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
    wait "${iperf_client}" >/dev/null 2>&1 || true
    wait "${iperf_server}" >/dev/null 2>&1 || true
    pass "${label}" "${message}"
    return 0
  fi

  kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_client}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true
  echo "[${label}] iperf3 client log: ${iperf_client_log}"
  tail -n 12 "${iperf_client_log}" || true
  echo "[${label}] iperf3 server log: ${iperf_server_log}"
  tail -n 12 "${iperf_server_log}" || true
  echo "[${label}] traffic probe debug: ns=${client_ns} remote=${client_remote}"
  ip netns exec "${client_ns}" ip -4 route get "${client_remote}" || true
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
  wait_log_pattern "${name}" "send: ping_up session=[0-9]+ lane=2 kind=1" 15 "lane=2 probe target recovered after UDP restore"

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
  wait_log_pattern "${name}" "type=HELLO_ACK .*accepted=1 caps=0x6 fec_profile=1" 20 "TCP shadow HELLO_ACK preserved FEC and LINK_STATUS negotiation"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  echo "[${name}] block UDP on path1; lane must fall back to TCP while preserving FEC capability"
  local client_fallback_line
  client_fallback_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  apply_udp_tunnel_block_path 1 "${PORT_FEC_TCP_FALLBACK}"

  wait_ping_ok "${name} tcp-fallback" 20
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client sent DATA over TCP fallback with negotiated FEC" "${client_fallback_line}"

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

run_fec_adaptive_75_case() {
  local name="fec-adaptive-75"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_FEC_ADAPTIVE_75}" false true 200 3000
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local server_loss_line
  local client_apply_line
  server_loss_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_apply_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  echo "[${name}] deterministically drop 3 of every 4 client-to-server UDP DATA frames; TCP REPAIR stays clean"
  apply_udp_data_keep_one_of_four_client_to_server_path 1 "${PORT_FEC_ADAPTIVE_75}"
  run_short_ping_load "${name}-warmup" 200 0.001
  wait_log_file_pattern "${name}" "${CURRENT_CLIENT_LOG}" "runtime: link_status_apply session=[0-9]+ lane=1 .*udp_limited=false .*tcp_limited=false .*repair_count=[34]" 5 "client applied FEC repair count >=3 before QoS selector switch"

  local adaptive_repair_count
  adaptive_repair_count="$(python3 - "${CURRENT_CLIENT_LOG}" "${client_apply_line}" <<'PY'
import re
import sys

path = sys.argv[1]
start = int(sys.argv[2])
pattern = re.compile(r"runtime: link_status_apply .*udp_limited=false .*tcp_limited=false .*repair_count=([34])\b")

with open(path, "r", encoding="utf-8", errors="replace") as f:
    for line_no, line in enumerate(f, 1):
        if line_no <= start:
            continue
        match = pattern.search(line)
        if match:
            print(match.group(1))
            break
PY
)"
  if [[ -z "${adaptive_repair_count}" ]]; then
    fail "${name}" "could not read adaptive FEC repair count from LINK_STATUS"
  fi

  local server_partial_line
  server_partial_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  run_short_ping_load "${name}-partial-group-check" 2 0.001
  sleep 0.2
  assert_fec_span_two_scaled_tcp_repairs_since "${name}" "${CURRENT_SERVER_LOG}" "${server_partial_line}" "${adaptive_repair_count}"

  local server_group_line
  server_group_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  run_short_ping_load "${name}-group-check" 400 0.001
  sleep 0.2
  assert_fec_groups_scaled_tcp_repairs_since "${name}" "${CURRENT_SERVER_LOG}" "${server_group_line}" "${adaptive_repair_count}"

  run_ping_sample "${name}-steady"

  if awk -v loss="${PING_SAMPLE_LOSS}" 'BEGIN { exit !(loss < 1) }'; then
    pass "${name}" "observed packet loss ${PING_SAMPLE_LOSS}% < 1% after adaptive FEC"
  else
    fail "${name}" "observed packet loss ${PING_SAMPLE_LOSS}% >= 1% after adaptive FEC"
  fi

  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_SERVER_LOG}" "${server_loss_line}" "recv: recover_err" 0 "server observed no FEC recovery errors"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "runtime/qos: link_status_send session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "server emitted UDP limited LINK_STATUS with reset repair count" "${server_loss_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "runtime: link_status_apply session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "client applied UDP limited LINK_STATUS with reset repair count" "${client_apply_line}"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client sent DATA over TCP after reset-count QoS switch" "${client_apply_line}" "${NS_C}" "${TUN_C_REMOTE}"
  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_SERVER_LOG}" "${server_loss_line}" "runtime/qos: link_status_send session=[0-9]+ lane=1 status=0x54" 0 "server did not emit UDP limited LINK_STATUS with stale repair count 3"
  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_CLIENT_LOG}" "${client_apply_line}" "runtime: link_status_apply session=[0-9]+ lane=1 status=0x54" 0 "client did not apply UDP limited LINK_STATUS with stale repair count 3"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_fec_disabled_negotiation_case() {
  local name="fec-disabled-negotiation"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_FEC_DISABLED_NEGOTIATION}" false false 200 600
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  run_short_ping_load "${name}-load" 180 0.02

  assert_log_file_not_contains "${name}" "${CURRENT_CLIENT_LOG}" "type=HELLO .*caps=0x[1-9a-f]" "client HELLO did not advertise FEC/LinkStatus when fec=false"
  assert_log_file_not_contains "${name}" "${CURRENT_SERVER_LOG}" "type=HELLO_ACK .*caps=0x[1-9a-f]" "server HELLO_ACK did not negotiate FEC/LinkStatus when fec=false"
  assert_log_file_not_contains "${name}" "${CURRENT_CLIENT_LOG}" "type=REPAIR|runtime/qos: link_status_send|runtime: link_status_apply" "client emitted no FEC repair or LINK_STATUS when fec=false"
  assert_log_file_not_contains "${name}" "${CURRENT_SERVER_LOG}" "type=REPAIR|runtime/qos: link_status_send|runtime: link_status_apply" "server emitted no FEC repair or LINK_STATUS when fec=false"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_link_status_qos_case() {
  local name="link-status-qos"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_LINK_STATUS_QOS}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local server_repair_line
  server_repair_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  run_short_ping_load "${name}-repair-shadow" 120 0.02
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "recv: frame_in type=REPAIR .*leg=\\{tcp" 10 "server received FEC REPAIR on TCP shadow" "${server_repair_line}"

  local server_status_line
  local client_apply_line
  local client_select_line
  server_status_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_apply_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  echo "[${name}] apply ${LINK_STATUS_QOS_LOSS} client-to-server UDP data loss while TCP shadow stays clean; recv-side FEC differential QoS should notify the sender"
  apply_udp_partial_loss_client_to_server_path 1 "${PORT_LINK_STATUS_QOS}" "${LINK_STATUS_QOS_LOSS}"

  run_ping_sample "${name}-qos"

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" "runtime/qos: link_status_send session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "server emitted UDP limited LINK_STATUS from receive-side QoS" "${server_status_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "runtime: link_status_apply session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "client applied server LINK_STATUS to lane selector" "${client_apply_line}"
  client_select_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client selector sent DATA over TCP after receive-side QoS feedback" "${client_select_line}" "${NS_C}" "${TUN_C_REMOTE}"

  echo "[${name}] clear UDP loss; receiver should send clear LINK_STATUS and selector should return DATA to UDP"
  clear_loss
  local client_return_line
  client_return_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  wait_log_file_pattern_while_iperf_from "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{udp .*frame=type=DATA" "${LINK_STATUS_QOS_CLEAR_WAIT}" "client selector returned DATA to UDP after QoS clear" "${client_return_line}" "${NS_S}" "${TUN_S_LOCAL}" "${NS_C}" "${TUN_C_REMOTE}"
  wait_ping_ok "${name} post-qos-clear" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_link_status_qos_reverse_case() {
  local name="link-status-qos-reverse"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_LINK_STATUS_QOS_REVERSE}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local client_repair_line
  client_repair_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  run_short_ping_load_from "${name}-repair-shadow" "${NS_S}" "${TUN_S_REMOTE}" 120 0.02
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "recv: frame_in type=REPAIR .*leg=\\{tcp" 10 "client received FEC REPAIR on TCP shadow" "${client_repair_line}" "${NS_S}" "${TUN_S_REMOTE}"

  local client_status_line
  local server_apply_line
  client_status_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  server_apply_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"

  echo "[${name}] apply ${LINK_STATUS_QOS_LOSS} server-to-client UDP data loss and generate server-to-client DATA; client receive-side QoS should notify the server"
  apply_udp_partial_loss_server_to_client_path 1 "${PORT_LINK_STATUS_QOS_REVERSE}" "${LINK_STATUS_QOS_LOSS}"

  run_ping_sample_from "${name}-qos" "${NS_S}" "${TUN_S_REMOTE}"

  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_CLIENT_LOG}" "runtime/qos: link_status_send session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "client emitted UDP limited LINK_STATUS from receive-side QoS" "${client_status_line}" "${NS_S}" "${TUN_S_REMOTE}"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_SERVER_LOG}" "runtime: link_status_apply session=[0-9]+ lane=1 status=0x10 .*udp_limited=true" "${LINK_STATUS_QOS_WAIT}" "server applied client LINK_STATUS to lane selector" "${server_apply_line}" "${NS_S}" "${TUN_S_REMOTE}"

  local server_select_line
  server_select_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  wait_log_file_pattern_while_ping_from "${name}" "${CURRENT_SERVER_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "server selector sent DATA over TCP after receive-side QoS feedback" "${server_select_line}" "${NS_S}" "${TUN_S_REMOTE}"

  echo "[${name}] clear UDP loss; server selector should return DATA to UDP"
  clear_loss
  local server_return_line
  server_return_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  wait_log_file_pattern_while_iperf_from "${name}" "${CURRENT_SERVER_LOG}" "schedule_select.*lane=1 .*leg=\\{udp .*frame=type=DATA" "${LINK_STATUS_QOS_CLEAR_WAIT}" "server selector returned DATA to UDP after QoS clear" "${server_return_line}" "${NS_C}" "${TUN_C_LOCAL}" "${NS_S}" "${TUN_S_REMOTE}"
  wait_ping_ok "${name} post-qos-clear" 12

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_link_status_qos_jitter_no_qos_case() {
  local name="link-status-qos-jitter-no-qos"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_LINK_STATUS_QOS_JITTER_NO_QOS}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local server_status_line
  local client_select_line
  server_status_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_select_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  echo "[${name}] apply shared high RTT/jitter without UDP QoS; low-rate traffic should stay on UDP"
  apply_tunnel_delay_jitter_path 1 "${PORT_LINK_STATUS_QOS_JITTER_NO_QOS}" 100ms 100ms
  run_short_ping_load "${name}-shared-jitter-low-rate" 120 0.05
  wait_ping_ok "${name} post-shared-jitter" 20

  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_SERVER_LOG}" "${server_status_line}" "runtime/qos: link_status_send .*udp_limited=true" 0 "server did not report UDP limited under shared jitter"
  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_CLIENT_LOG}" "${client_select_line}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 0 "client kept DATA on UDP under shared jitter"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_direct_udp_iperf_json() {
  local label="$1"
  local port="$2"
  local duration="$3"
  local rate="$4"
  local client_json="$5"
  local client_err="$6"
  local server_log="$7"

  echo "[${label}] direct UDP iperf3: ${NS_C}->${PATH1_REMOTE}:${port} rate=${rate} duration=${duration}s"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${PATH1_REMOTE}" -p "${port}" >"${server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1

  local client_status=0
  set +e
  timeout "$((duration + 8))s" ip netns exec "${NS_C}" iperf3 -c "${PATH1_REMOTE}" -p "${port}" -u -b "${rate}" -t "${duration}" -i 1 -J \
    >"${client_json}" 2>"${client_err}"
  client_status=$?
  set -e

  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true

  echo "[${label}] direct UDP iperf3 json=${client_json} err=${client_err} server_log=${server_log}"
  if (( client_status != 0 )); then
    tail -n 20 "${client_err}" || true
    fail "${label}" "direct UDP iperf3 failed status=${client_status}"
    return 1
  fi
}

run_link_status_qos_udp_rate_sanity_case() {
  local name="link-status-qos-udp-rate-sanity"
  local port="${PORT_LINK_STATUS_QOS_IPERF}"
  local rate="5mbit"
  echo "==== ${name} e2e start ===="
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${name}] iperf3 not found, skip UDP rate sanity case"
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${name}] timeout not found, skip UDP rate sanity case"
    echo "==== ${name} e2e end ===="
    return 0
  fi

  clear_loss

  local baseline_json="${WORKDIR}/${name}.baseline-client.json"
  local baseline_err="${WORKDIR}/${name}.baseline-client.err"
  local baseline_server_log="${WORKDIR}/${name}.baseline-server.log"
  if ! run_direct_udp_iperf_json "${name}-baseline" "${port}" 6 120M "${baseline_json}" "${baseline_err}" "${baseline_server_log}"; then
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  echo "[${name}] apply UDP rate limit: ${rate}"
  apply_udp_tunnel_rate_path 1 "${port}" "${rate}"

  local limited_json="${WORKDIR}/${name}.limited-client.json"
  local limited_err="${WORKDIR}/${name}.limited-client.err"
  local limited_server_log="${WORKDIR}/${name}.limited-server.log"
  if ! run_direct_udp_iperf_json "${name}-limited" "${port}" 10 120M "${limited_json}" "${limited_err}" "${limited_server_log}"; then
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
  fi

  echo "[${name}] client qdisc after limited UDP iperf3"
  ip netns exec "${NS_C}" tc -s qdisc show dev "${VETHC1}" || true
  echo "[${name}] client mangle OUTPUT marks after limited UDP iperf3"
  ip netns exec "${NS_C}" iptables -t mangle -nvL OUTPUT || true

  local baseline_bps
  local limited_bps
  baseline_bps="$(parse_iperf_json_end_bps "${baseline_json}")"
  limited_bps="$(parse_iperf_json_end_bps "${limited_json}")"
  echo "[${name}] udp_direct_bps baseline=${baseline_bps} limited=${limited_bps}"

  if [[ -z "${baseline_bps}" || -z "${limited_bps}" ]]; then
    fail "${name}" "could not parse direct UDP iperf3 bitrate"
  elif awk -v baseline="${baseline_bps}" -v limited="${limited_bps}" 'BEGIN { exit !(baseline > 30000000 && limited < 10000000) }'; then
    pass "${name}" "direct UDP path was rate-limited"
  else
    fail "${name}" "direct UDP path was not rate-limited enough"
  fi

  clear_loss
  echo "==== ${name} e2e end ===="
}

run_link_status_qos_iperf_dynamic_case() {
  local name="$1"
  local port="$2"
  local shape_mode="$3"
  local repeat_limit="$4"
  local rate="$5"
  local delay="${6:-}"
  local jitter="${7:-}"
  local min_recovered_vs_baseline="${8:-0.40}"
  local min_limited_vs_baseline="${9:-0.03}"
  local min_recovered_vs_limited="0.75"
  local cycles=1
  local duration=38
  if [[ "${repeat_limit}" == "true" ]]; then
    cycles=2
    # Keep traffic running after the second clear so selector recovery can be
    # observed instead of racing the iperf process exit.
    duration=75
  fi

  # This case is intentionally stronger than a throughput recovery smoke test:
  # DATA starts on UDP, UDP tunnel traffic is rate-limited, receive-side QoS is
  # expected to emit UDP-limited LINK_STATUS, and the sender must switch DATA to
  # TCP until the UDP rate limit is cleared.
  echo "==== ${name} e2e start ===="
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${name}] iperf3 not found, skip dynamic QoS throughput case"
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${name}] timeout not found, skip dynamic QoS throughput case"
    echo "==== ${name} e2e end ===="
    return 0
  fi

  clear_loss
  write_one_lane_config "${name}" "${port}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  if [[ "${shape_mode}" == "udp-rate-jitter" ]]; then
    echo "[${name}] apply baseline RTT/jitter: delay=${delay} jitter=${jitter}"
    apply_tunnel_delay_jitter_path 1 "${port}" "${delay}" "${jitter}"
    wait_ping_ok "${name} jitter-baseline" 20
  fi

  local server_status_line
  local client_selector_line
  server_status_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  client_selector_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"

  local iperf_server_log="${WORKDIR}/${name}.iperf-server.log"
  local iperf_client_json="${WORKDIR}/${name}.iperf-client.json"
  local iperf_client_err="${WORKDIR}/${name}.iperf-client.err"
  echo "[${name}] start staged iperf3 over TUN: duration=${duration}s rate_limit=${rate} mode=${shape_mode} repeat=${repeat_limit}"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >"${iperf_server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1
  timeout "$((duration + 8))s" ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t "${duration}" -i 1 -J \
    >"${iperf_client_json}" 2>"${iperf_client_err}" &
  local iperf_client=$!

  sleep 7
  echo "[${name}] apply UDP rate limit: ${rate}"
  case "${shape_mode}" in
  udp-rate)
    apply_udp_tunnel_rate_path 1 "${port}" "${rate}"
    ;;
  udp-rate-jitter)
    apply_udp_tunnel_rate_with_delay_jitter_path 1 "${port}" "${rate}" "${delay}" "${jitter}"
    ;;
  *)
    fail "${name}" "unsupported dynamic QoS shape mode: ${shape_mode}"
    kill "${iperf_client}" "${iperf_server}" >/dev/null 2>&1 || true
    wait "${iperf_client}" >/dev/null 2>&1 || true
    wait "${iperf_server}" >/dev/null 2>&1 || true
    stop_multipath
    clear_loss
    echo "==== ${name} e2e end ===="
    return 0
    ;;
  esac

  sleep 14
  echo "[${name}] clear UDP rate limit"
  if [[ "${shape_mode}" == "udp-rate-jitter" ]]; then
    apply_tunnel_delay_jitter_path 1 "${port}" "${delay}" "${jitter}"
  else
    clear_loss
  fi

  if [[ "${repeat_limit}" == "true" ]]; then
    sleep 10
    echo "[${name}] re-apply UDP rate limit: ${rate}"
    if [[ "${shape_mode}" == "udp-rate-jitter" ]]; then
      apply_udp_tunnel_rate_with_delay_jitter_path 1 "${port}" "${rate}" "${delay}" "${jitter}"
    else
      apply_udp_tunnel_rate_path 1 "${port}" "${rate}"
    fi
    sleep 10
    echo "[${name}] clear UDP rate limit again"
    if [[ "${shape_mode}" == "udp-rate-jitter" ]]; then
      apply_tunnel_delay_jitter_path 1 "${port}" "${delay}" "${jitter}"
    else
      clear_loss
    fi
  fi

  local client_status=0
  set +e
  wait "${iperf_client}"
  client_status=$?
  set -e
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true

  echo "[${name}] iperf3 client json: ${iperf_client_json}"
  echo "[${name}] iperf3 client err: ${iperf_client_err}"
  echo "[${name}] iperf3 server log: ${iperf_server_log}"
  if (( client_status != 0 )); then
    fail "${name}" "iperf3 client failed status=${client_status}"
  fi

  local baseline_bps
  local limited_bps
  local recovered_bps
  baseline_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 2 6)"
  limited_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 13 20)"
  recovered_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 26 36)"
  assert_iperf_window_recovered "${name}" "${baseline_bps}" "${limited_bps}" "${recovered_bps}" "${min_recovered_vs_baseline}" "${min_recovered_vs_limited}" "${min_limited_vs_baseline}"

  if [[ "${repeat_limit}" == "true" ]]; then
    local limited2_bps
    local recovered2_bps
    limited2_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 34 40)"
    recovered2_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 58 68)"
    assert_iperf_window_recovered "${name}-repeat" "${baseline_bps}" "${limited2_bps}" "${recovered2_bps}" "${min_recovered_vs_baseline}" "${min_recovered_vs_limited}" "${min_limited_vs_baseline}"
  fi

  assert_link_status_since_ge "${name}" "${CURRENT_SERVER_LOG}" "${server_status_line}" "runtime/qos: link_status_send .*udp_limited=true" "${cycles}" "server sent UDP-limited LINK_STATUS snapshots"
  assert_link_status_since_ge "${name}" "${CURRENT_SERVER_LOG}" "${server_status_line}" "runtime/qos: link_status_send .*udp_limited=false" "${cycles}" "server sent UDP-clear LINK_STATUS snapshots"
  assert_link_status_since_ge "${name}" "${CURRENT_CLIENT_LOG}" "${client_selector_line}" "selector action=qos_data_leg .*from=udp to=tcp" "${cycles}" "client switched DATA selector to TCP"
  assert_link_status_since_ge "${name}" "${CURRENT_CLIENT_LOG}" "${client_selector_line}" "selector action=qos_data_leg .*from=tcp to=udp" "${cycles}" "client switched DATA selector back to UDP"
  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_SERVER_LOG}" "${server_status_line}" "runtime/qos: link_status_send .*tcp_limited=true" 0 "server did not falsely report TCP limited during UDP rate shaping"
  assert_log_file_pattern_count_since_le "${name}" "${CURRENT_CLIENT_LOG}" "${client_selector_line}" "runtime: link_status_apply .*tcp_limited=true" 0 "client did not apply false TCP limited status during UDP rate shaping"
  assert_selector_switches_since_le "${name}" "${CURRENT_CLIENT_LOG}" "${client_selector_line}" "$((cycles * 4))"

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_tcp_fallback_rate_dynamic_case() {
  local name="tcp-fallback-rate-dynamic"
  local port="${PORT_TCP_FALLBACK_RATE_DYNAMIC}"
  local rate="8mbit"
  local duration=38

  echo "==== ${name} e2e start ===="
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "[${name}] iperf3 not found, skip TCP fallback rate case"
    echo "==== ${name} e2e end ===="
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    echo "[${name}] timeout not found, skip TCP fallback rate case"
    echo "==== ${name} e2e end ===="
    return 0
  fi

  clear_loss
  write_one_lane_config "${name}" "${port}" false true 200 3000
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"
  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local client_selector_line
  client_selector_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  echo "[${name}] block UDP so DATA uses TCP fallback"
  apply_udp_tunnel_block_path 1 "${port}"
  wait_ping_ok "${name} tcp-fallback" 25
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client sent DATA over TCP fallback before TCP shaping" "${client_selector_line}"

  local iperf_server_log="${WORKDIR}/${name}.iperf-server.log"
  local iperf_client_json="${WORKDIR}/${name}.iperf-client.json"
  local iperf_client_err="${WORKDIR}/${name}.iperf-client.err"
  echo "[${name}] start staged iperf3 over TCP fallback: duration=${duration}s tcp_rate=${rate}"
  ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_LOCAL}" >"${iperf_server_log}" 2>&1 &
  local iperf_server=$!
  sleep 1
  timeout "$((duration + 8))s" ip netns exec "${NS_C}" iperf3 -c "${TUN_C_REMOTE}" -t "${duration}" -i 1 -J \
    >"${iperf_client_json}" 2>"${iperf_client_err}" &
  local iperf_client=$!

  sleep 7
  echo "[${name}] apply TCP fallback rate limit: ${rate}"
  apply_udp_block_tcp_rate_path 1 "${port}" "${rate}"
  sleep 14
  echo "[${name}] clear TCP fallback rate limit while keeping UDP blocked"
  clear_loss
  apply_udp_tunnel_block_path 1 "${port}"

  local client_status=0
  set +e
  wait "${iperf_client}"
  client_status=$?
  set -e
  kill "${iperf_server}" >/dev/null 2>&1 || true
  wait "${iperf_server}" >/dev/null 2>&1 || true

  echo "[${name}] iperf3 client json: ${iperf_client_json}"
  echo "[${name}] iperf3 client err: ${iperf_client_err}"
  echo "[${name}] iperf3 server log: ${iperf_server_log}"
  if (( client_status != 0 )); then
    fail "${name}" "iperf3 client failed status=${client_status}"
  fi

  local baseline_bps
  local limited_bps
  local recovered_bps
  baseline_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 2 6)"
  limited_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 13 20)"
  recovered_bps="$(parse_iperf_json_window_avg_bps "${iperf_client_json}" 26 36)"
  echo "[${name}] iperf_window_bps baseline=${baseline_bps} limited=${limited_bps} recovered=${recovered_bps}"
  assert_iperf_window_limited_drop "${name}" "${baseline_bps}" "${limited_bps}" 0.80
  assert_iperf_window_recovered "${name}" "${baseline_bps}" "${limited_bps}" "${recovered_bps}" 0.50 1.20 0.01

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_fec_loaded_latency_case() {
  local name="fec-loaded-latency"
  local iperf_rate="10M"
  local iperf_duration="25"
  local ping_count="400"
  local ping_interval="0.05"

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
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 25 "lane=1 reached TCP HELLO_ACK with both UDP paths blocked"
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=2 kind=2" 25 "lane=2 reached TCP HELLO_ACK with both UDP paths blocked"

  echo "[${name}] restore UDP; both lanes must come back to UDP"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15
  wait_log_pattern "${name}" "send: ping_up session=[0-9]+ lane=1 kind=1" 15 "lane=1 probe target recovered after UDP restore"
  wait_log_pattern "${name}" "send: ping_up session=[0-9]+ lane=2 kind=1" 15 "lane=2 probe target recovered after UDP restore"

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

  wait_log_pattern "${name}" "send/dialer: dial err remote=" 15 "client observed fallback_dial_error on lane=1"
  expect_ping_fail_for "${name} no-runnable" 3

  echo "[${name}] restore UDP and TCP; lane must come back online"
  clear_loss
  wait_ping_ok "${name} post-recovery" 15

  stop_multipath
  clear_loss
  echo "==== ${name} e2e end ===="
}

run_tcp_established_redial_case() {
  local name="tcp-established-redial"
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_TCP_ESTABLISHED_REDIAL}" false false 200 600
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12

  echo "[${name}] block UDP; establish TCP fallback first"
  apply_udp_tunnel_block_path 1 "${PORT_TCP_ESTABLISHED_REDIAL}"
  wait_ping_ok "${name} tcp-fallback" 20
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "client established TCP fallback before failure"

  local client_failure_line
  client_failure_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  echo "[${name}] kill server with TCP fallback established; client must mark TCP failed and redial"
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi

  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: tcp_leg_failure conn=" 20 "client observed established TCP leg failure" "${client_failure_line}"

  local client_redial_line
  client_redial_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  echo "[${name}] restart server with UDP still blocked; TCP redial must restore the lane"
  start_server_process "${name}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: tcp_dialed session=[0-9]+ lane=1 conn=" 20 "client redialed TCP after established failure" "${client_redial_line}"
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "client accepted TCP HELLO_ACK after redial" "${client_redial_line}"
  wait_ping_ok "${name} post-redial" 20

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
  lane1_count="$(grep -E -c "schedule_select session=[0-9]+ lane=1 .*frame=type=DATA" "${client_log}" 2>/dev/null || true)"
  lane2_count="$(grep -E -c "schedule_select session=[0-9]+ lane=2 .*frame=type=DATA" "${client_log}" 2>/dev/null || true)"
  lane1_count="${lane1_count:-0}"
  lane2_count="${lane2_count:-0}"
  total=$((lane1_count + lane2_count))
  echo "[${name}] lane1_count=${lane1_count} lane2_count=${lane2_count} total=${total}"

  if (( total < 50 )); then
    fail "${name}" "client emitted only ${total} DATA schedule_select lines; expected >=50 (MULTIPATH_DEBUG must be enabled)"
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

run_mtu_fec_case() {
  local name="mtu-fec"
  local payload=1412
  echo "==== ${name} e2e start ===="
  clear_loss
  write_one_lane_config "${name}" "${PORT_MTU_FEC}" false true 200 600
  CURRENT_EXTRA_ENV=(MULTIPATH_DISABLE_BW_PROBE=1)
  start_multipath "${name}"

  wait_ping_ok "${name} baseline" 12
  wait_log_pattern "${name}" "send: hello_ack_active session=[0-9]+ lane=1 kind=2" 20 "TCP shadow leg reached HELLO_ACK"

  local server_repair_line
  server_repair_line="$(current_log_file_line_count "${CURRENT_SERVER_LOG}")"
  echo "[${name}] send near-MTU ping (-s ${payload} -M do) over UDP with FEC enabled"
  if ip netns exec "${NS_C}" ping -c 12 -i 0.05 -W 2 -s "${payload}" -M do "${TUN_C_REMOTE}" >/dev/null 2>&1; then
    pass "${name}" "near-MTU ping survived UDP transport with FEC"
  else
    fail "${name}" "near-MTU ping failed over UDP transport with FEC"
  fi
  wait_log_file_any_pattern_while_ping "${name}" "${CURRENT_SERVER_LOG}" 10 "server received near-MTU FEC REPAIR on TCP shadow" "${server_repair_line}" \
    "recv: frame_in type=REPAIR .*symbol_len=[1-9][0-9]{3} .*leg=\\{tcp" \
    "recv: frame_in type=REPAIR .*leg=\\{tcp .*symbol_len=[1-9][0-9]{3}"

  echo "[${name}] block UDP and force TCP fallback with FEC still enabled"
  local client_fallback_line
  client_fallback_line="$(current_log_file_line_count "${CURRENT_CLIENT_LOG}")"
  apply_udp_tunnel_block_path 1 "${PORT_MTU_FEC}"
  wait_ping_ok "${name} tcp-fallback" 20
  wait_log_file_pattern_while_ping "${name}" "${CURRENT_CLIENT_LOG}" "schedule_select.*lane=1 .*leg=\\{tcp .*frame=type=DATA" 20 "client sent DATA over TCP fallback for FEC MTU path" "${client_fallback_line}"

  echo "[${name}] send near-MTU ping (-s ${payload} -M do) over TCP fallback with FEC enabled"
  if ip netns exec "${NS_C}" ping -c 8 -i 0.05 -W 2 -s "${payload}" -M do "${TUN_C_REMOTE}" >/dev/null 2>&1; then
    pass "${name}" "near-MTU ping survived TCP fallback with FEC"
  else
    fail "${name}" "near-MTU ping failed over TCP fallback with FEC"
  fi

  assert_log_file_not_contains "${name}" "${CURRENT_CLIENT_LOG}" "recv: recover_err" "client observed no FEC recovery errors during MTU case"
  assert_log_file_not_contains "${name}" "${CURRENT_SERVER_LOG}" "recv: recover_err" "server observed no FEC recovery errors during MTU case"

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
run_unknown_session_rebootstrap_case
run_multilane_rebootstrap_case
run_fallback_dial_error_case
run_tcp_established_redial_case
run_leg_selector_case
run_tcp_correctness_case
run_udp_correctness_case
run_bandwidth_probe_convergence_case
run_bandwidth_probe_tcp_reference_case
run_bandwidth_probe_udp_restore_iperf_case
run_bandwidth_probe_default_cap_case
run_bandwidth_probe_disabled_case
run_nat_case
run_nat_tcp_fallback_case
run_fec_comparison
run_fec_disabled_negotiation_case
run_fec_tcp_fallback_case
run_multipath_fec_case
run_fec_adaptive_75_case
run_link_status_qos_case
run_link_status_qos_reverse_case
run_link_status_qos_jitter_no_qos_case
run_link_status_qos_udp_rate_sanity_case
run_link_status_qos_iperf_dynamic_case "link-status-qos-iperf" "${PORT_LINK_STATUS_QOS_IPERF}" udp-rate false 5mbit "" "" 0.40 0.03
run_link_status_qos_iperf_dynamic_case "link-status-qos-iperf-repeat" "${PORT_LINK_STATUS_QOS_IPERF_REPEAT}" udp-rate true 5mbit "" "" 0.40 0.03
run_link_status_qos_iperf_dynamic_case "link-status-qos-iperf-jitter" "${PORT_LINK_STATUS_QOS_IPERF_JITTER}" udp-rate-jitter false 500kbit 60ms 80ms 0.25 0.03
run_link_status_qos_iperf_dynamic_case "link-status-qos-iperf-rtt200-jitter" "${PORT_LINK_STATUS_QOS_IPERF_RTT200}" udp-rate-jitter false 500kbit 100ms 100ms 0.20 0.03
run_tcp_fallback_rate_dynamic_case
run_fec_loaded_latency_case
run_weighted_scheduling_case
run_mtu_case
run_mtu_fec_case

if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "real e2e failed (${FAIL_COUNT} failures)"
  exit 1
fi

echo "real e2e ok"
