#!/usr/bin/env bash
# ephemeral-collapse: Automated Linux Network Namespace Setup & Kernel Tuning
# Creates an isolated netns topology with tuned sysctl seams for bare-metal / VM environments.

set -euo pipefail

NS_SERVER="ns-srv-collapse"
NS_CLIENT="ns-cli-collapse"
VETH_SRV="veth-srv"
VETH_CLI="veth-cli"
IP_SRV="10.200.1.10/24"
IP_CLI="10.200.1.20/24"

echo "==============================================================================="
echo " ephemeral-collapse: Provisioning Network Namespaces and Seam Controls"
echo "==============================================================================="

if [[ $EUID -ne 0 ]]; then
   echo "[FATAL] This script requires root privileges to configure network namespaces and sysctls."
   exit 1
fi

# Clean up any existing namespaces
ip netns del "$NS_SERVER" 2>/dev/null || true
ip netns del "$NS_CLIENT" 2>/dev/null || true

echo "[+] Creating network namespaces: $NS_SERVER and $NS_CLIENT..."
ip netns add "$NS_SERVER"
ip netns add "$NS_CLIENT"

echo "[+] Creating veth pair ($VETH_SRV <-> $VETH_CLI)..."
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

echo "[+] Applying Seam Failure sysctl configuration to $NS_SERVER (target)..."
# Restrict Listen Queue to 128 to reproduce backlog overflow drops
ip netns exec "$NS_SERVER" sysctl -w net.core.somaxconn=128
ip netns exec "$NS_SERVER" sysctl -w net.ipv4.tcp_max_syn_backlog=128
# Restrict Ephemeral Port Range to 512 ports to reproduce port exhaustion
ip netns exec "$NS_SERVER" sysctl -w net.ipv4.ip_local_port_range="32768 33279"
# Disable socket reuse to force TIME_WAIT bucket overflow
ip netns exec "$NS_SERVER" sysctl -w net.ipv4.tcp_tw_reuse=0

# Configure netfilter conntrack limits if kernel module is loaded
if sysctl net.netfilter.nf_conntrack_max >/dev/null 2>&1; then
    echo "[+] Bounding nf_conntrack_max to 2048 entries..."
    sysctl -w net.netfilter.nf_conntrack_max=2048
    sysctl -w net.netfilter.nf_conntrack_tcp_timeout_time_wait=60
fi

echo "-------------------------------------------------------------------------------"
echo " Topology established successfully:"
echo "   Server NS: $NS_SERVER (10.200.1.10) [somaxconn=128, port_range=32768-33279]"
echo "   Client NS: $NS_CLIENT (10.200.1.20)"
echo ""
echo " To launch backend in namespace:"
echo "   ip netns exec $NS_SERVER ./ephemeral-backend"
echo " To launch load generator in namespace:"
echo "   ip netns exec $NS_CLIENT ./traffic-generator --target http://10.200.1.10:8080"
echo "==============================================================================="
