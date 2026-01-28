#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID:-0} -ne 0 ]]; then
  exec sudo -E bash "$0" "$@"
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${ROOT_DIR}/multipath"
WORKDIR="$(mktemp -d)"

NS_C="mp_ns_c"
NS_S="mp_ns_s"

VETHC1="mpc1"
VETHS1="mps1"
VETHC2="mpc2"
VETHS2="mps2"

PORT_TCP=5000
PORT_UDP=5001

TUN_NAME="mptun0"
TUN_C_LOCAL="172.16.0.1/30"
TUN_S_LOCAL="172.16.0.2/30"
TUN_C_REMOTE="172.16.0.2"
TUN_S_REMOTE="172.16.0.1"

cleanup() {
  set +e
  pkill -f "${BIN} -config ${WORKDIR}/server.json" >/dev/null 2>&1 || true
  pkill -f "${BIN} -config ${WORKDIR}/client.json" >/dev/null 2>&1 || true

  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root >/dev/null 2>&1 || true
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC1}" root >/dev/null 2>&1 || true

  ip netns del "${NS_C}" >/dev/null 2>&1 || true
  ip netns del "${NS_S}" >/dev/null 2>&1 || true

  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

build_bin() {
  (cd "${ROOT_DIR}" && go build -o "${BIN}" ./...)
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

  ip netns exec "${NS_C}" ip addr add 10.0.1.1/24 dev "${VETHC1}"
  ip netns exec "${NS_S}" ip addr add 10.0.1.2/24 dev "${VETHS1}"
  ip netns exec "${NS_C}" ip addr add 10.0.2.1/24 dev "${VETHC2}"
  ip netns exec "${NS_S}" ip addr add 10.0.2.2/24 dev "${VETHS2}"

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

  cat >"${WORKDIR}/server.json" <<EOF
{
  "server": { "listen": "0.0.0.0:${port}" },
  "tun": {
    "name": "${TUN_NAME}",
    "localAddr": "${TUN_S_LOCAL}",
    "remoteAddr": "${TUN_S_REMOTE}",
    "allowedIPs": ["${TUN_C_REMOTE}/32"]
  },
  "promListenAddr": "127.0.0.1:2131",
  "tcp": ${tcp_flag},
  "isServer": true
}
EOF

  cat >"${WORKDIR}/client.json" <<EOF
{
  "client": {
    "remotePaths": [
      { "remoteAddr": "10.0.1.2:${port}", "weight": 1 },
      { "remoteAddr": "10.0.2.2:${port}", "weight": 1 }
    ]
  },
  "tun": {
    "name": "${TUN_NAME}",
    "localAddr": "${TUN_C_LOCAL}",
    "remoteAddr": "${TUN_C_REMOTE}",
    "allowedIPs": ["${TUN_S_REMOTE}/32"]
  },
  "promListenAddr": "127.0.0.1:2132",
  "tcp": ${tcp_flag},
  "isServer": false
}
EOF
}

run_mode() {
  local mode="$1"
  local port="$2"

  write_config "${mode}" "${port}"

  ip netns exec "${NS_S}" "${BIN}" -config "${WORKDIR}/server.json" &
  local server_pid=$!
  ip netns exec "${NS_C}" "${BIN}" -config "${WORKDIR}/client.json" &
  local client_pid=$!

  sleep 2

  local ping_log="${WORKDIR}/ping_${mode}.log"
  ip netns exec "${NS_C}" ping -D -i 0.2 "${TUN_S_REMOTE}" >"${ping_log}" 2>&1 &
  local ping_pid=$!

  echo "[${mode}] ping over TUN (baseline)"
  ip netns exec "${NS_C}" ping -c 3 -W 1 "${TUN_S_REMOTE}"

  if command -v iperf3 >/dev/null 2>&1; then
    ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_REMOTE}" >/dev/null 2>&1 &
    sleep 1
    echo "[${mode}] iperf3 over TUN (baseline)"
    ip netns exec "${NS_C}" iperf3 -c "${TUN_S_REMOTE}" -t 3 -i 1 || true
  else
    echo "[${mode}] iperf3 not found, skip throughput test"
  fi

  echo "[${mode}] simulate loss on path2"
  ip netns exec "${NS_C}" tc qdisc add dev "${VETHC2}" root netem loss 100%
  sleep 2
  ip netns exec "${NS_C}" ping -c 3 -W 1 "${TUN_S_REMOTE}"

  echo "[${mode}] restore path2"
  ip netns exec "${NS_C}" tc qdisc del dev "${VETHC2}" root || true
  sleep 2
  ip netns exec "${NS_C}" ping -c 3 -W 1 "${TUN_S_REMOTE}"

  if command -v iperf3 >/dev/null 2>&1; then
    ip netns exec "${NS_S}" iperf3 -s -1 -B "${TUN_S_REMOTE}" >/dev/null 2>&1 &
    sleep 1
    echo "[${mode}] iperf3 over TUN (post-recovery)"
    ip netns exec "${NS_C}" iperf3 -c "${TUN_S_REMOTE}" -t 3 -i 1 || true
  fi

  kill "${ping_pid}" >/dev/null 2>&1 || true
  wait "${ping_pid}" >/dev/null 2>&1 || true
  echo "[${mode}] ping log: ${ping_log}"

  kill "${client_pid}" "${server_pid}" >/dev/null 2>&1 || true
  wait "${client_pid}" "${server_pid}" >/dev/null 2>&1 || true
}

build_bin
setup_netns

run_mode "udp" "${PORT_UDP}"
run_mode "tcp" "${PORT_TCP}"

echo "e2e ok"
