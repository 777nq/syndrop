#!/usr/bin/env bash
# ephemeral-collapse: Automated Linux Network Namespace Setup & Kernel Tuning
# Creates an isolated netns topology with strict client vs server sysctl separation
# and host pollution safety guards.

set -euo pipefail

NS_SERVER="ns-srv-collapse"
NS_CLIENT="ns-cli-collapse"
VETH_SRV="veth-srv"
VETH_CLI="veth-cli"
IP_SRV="10.200.1.10/24"
IP_CLI="10.200.1.20/24"
CLAMP_HOST_CONNTRACK="${1:-no}"

echo "==============================================================================="
echo " ephemeral-collapse: Provisioning Isolated Network Namespaces (netns)"
echo " Safe Isolation: Namespaces prevent host-wide socket pollution"
echo "==============================================================================="

if [[ $EUID -ne 0 ]]; then
   echo "[FATAL] This script requires root privileges to configure network namespaces."
   exit 1
fi

# Clean up any pre-existing namespaces
ip netns del "$NS_SERVER" 2>/dev/null || true
ip netns del "$NS_CLIENT" 2>/dev/null || true

echo "[+] Creating isolated network namespaces: $NS_SERVER and $NS_CLIENT..."
ip netns add "$NS_SERVER"
ip netns add "$NS_CLIENT"

echo "[+] Creating dedicated veth interconnect ($VETH_SRV <-> $VETH_CLI)..."
ip link add "$VETH_SRV" type veth peer name "$VETH_CLI"

echo "[+] Moving interfaces into respective namespaces..."
ip link set "$VETH_SRV" netns "$NS_SERVER"
ip link set "$VETH_CLI" netns "$NS_CLIENT"

echo "[+] Configuring IP addressing and bringing loopback interfaces UP..."
ip netns exec "$NS_SERVER" ip addr add "$IP_SRV" dev "$VETH_SRV"
ip netns exec "$NS_SERVER" ip link set dev "$VETH_SRV" up
ip netns exec "$NS_SERVER" ip link set dev lo up

ip netns exec "$NS_CLIENT" ip addr add "$IP_CLI" dev "$VETH_CLI"
ip netns exec "$NS_CLIENT" ip link set dev "$VETH_CLI" up
ip netns exec "$NS_CLIENT" ip link set dev lo up

echo "[+] [SERVER SEAMS] Applying Server-Side sysctls to $NS_SERVER..."
# Server-side namespaced sysctls: somaxconn and tcp_max_syn_backlog
ip netns exec "$NS_SERVER" sysctl -w net.core.somaxconn=128
ip netns exec "$NS_SERVER" sysctl -w net.ipv4.tcp_max_syn_backlog=128
echo "    -> net.core.somaxconn = 128 (forces accept queue drops under burst)"
echo "    -> net.ipv4.tcp_max_syn_backlog = 128"

echo "[+] [CLIENT SEAMS] Applying Client-Side sysctls to $NS_CLIENT..."
# Client-side namespaced sysctls: ephemeral port range and tw reuse
ip netns exec "$NS_CLIENT" sysctl -w net.ipv4.ip_local_port_range="32768 33279"
ip netns exec "$NS_CLIENT" sysctl -w net.ipv4.tcp_tw_reuse=0
echo "    -> net.ipv4.ip_local_port_range = '32768 33279' (512 ephemeral ports)"
echo "    -> net.ipv4.tcp_tw_reuse = 0 (prohibits socket recycling)"

echo ""
echo "[!] [HOST PROTECTION NOTICE: Netfilter Conntrack & TIME_WAIT Buckets]"
echo "    In Linux, net.netfilter.nf_conntrack_max and net.ipv4.tcp_max_tw_buckets"
echo "    are global to the host kernel in many distributions."
echo "    To protect your developer workstation (Slack, Spotify, browser tabs),"
echo "    host-wide conntrack is NOT modified by default."

if [ "$CLAMP_HOST_CONNTRACK" = "--clamp-conntrack" ]; then
    echo "[!] Explicit flag --clamp-conntrack detected. Clamping host conntrack to 2048..."
    sysctl -w net.netfilter.nf_conntrack_max=2048
    sysctl -w net.netfilter.nf_conntrack_tcp_timeout_time_wait=60
else
    echo "    To test conntrack saturation safely, run the generator with high 5-tuple churn,"
    echo "    or pass '--clamp-conntrack' in an isolated VM/Vagrant instance."
fi

echo "-------------------------------------------------------------------------------"
echo " Topology established successfully:"
echo "   Server NS: $NS_SERVER (10.200.1.10) [somaxconn=128, syn_backlog=128]"
echo "   Client NS: $NS_CLIENT (10.200.1.20) [port_range=32768-33279 (512), tw_reuse=0]"
echo ""
echo " To run backend in Server NS:"
echo "   ip netns exec $NS_SERVER ./ephemeral-backend"
echo " To run load generator in Client NS:"
echo "   ip netns exec $NS_CLIENT ./traffic-generator -target http://10.200.1.10:8080 -scenario time-wait"
echo "==============================================================================="
