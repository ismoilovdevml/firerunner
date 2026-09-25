#!/usr/bin/env bash
# Functional test of the firewall install.sh writes (net-up.sh): network
# namespaces stand in for microVMs on the bridge (one tap not named fltap*),
# a builder, the host, a cloud metadata server and outside hosts (also used as
# a LAN host that routes the microVM subnet through this host).
#
# It changes the network of the machine it runs on, so run it in a privileged
# throwaway container, once with and once without br_netfilter:
#   docker run --rm --privileged -e BRNF=1 -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/network/run.sh
set -uo pipefail
apt-get update -qq >/dev/null && apt-get install -y -qq nftables iproute2 netcat-openbsd iputils-ping >/dev/null || exit 99

WORK=/work; mkdir -p $WORK
# BRNF=1: br_netfilter active (bridged IPv4 also passes the inet hooks); 0: pure bridging.
sysctl -qw net.bridge.bridge-nf-call-iptables="${BRNF:-1}" 2>/dev/null
echo "MODE bridge-nf-call-iptables=$(cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null || echo absent)"
# Render net-up.sh with install.sh's own code (everything but the final `main "$@"`).
INSTALL_SH=${INSTALL_SH:-/src/install.sh}
[[ $(tail -n1 "$INSTALL_SH") == 'main "$@"' ]] || { echo "install.sh no longer ends with main \"\$@\""; exit 95; }
sed '$d' "$INSTALL_SH" > $WORK/install-lib.sh
(
    # shellcheck disable=SC1091
    source $WORK/install-lib.sh
    # shellcheck disable=SC2034  # read by the install.sh functions
    LIB_DIR=$WORK/lib CONF_DIR=$WORK/conf
    # shellcheck disable=SC2034
    FR_METRICS_ALLOW=192.0.2.10 FR_EGRESS_DENY=198.51.100.0/24
    # A corporate proxy is configured: microVMs may use the forwarder on .1:3128.
    PROXY_FILE=$WORK/conf/proxy-upstream
    mkdir -p "$WORK/conf"; echo "http://proxy.corp:3128" > "$PROXY_FILE"
    systemctl() { return 1; }      # no firewalld
    put() { local f=$1 mode=${2:-0644}; mkdir -p "$(dirname "$f")"; cat >"$f"; chmod "$mode" "$f"; }
    setup_network >/dev/null
)
NETUP=$WORK/lib/net-up.sh
[[ -x $NETUP ]] || { echo "net-up.sh not rendered"; exit 98; }
bash -n $NETUP || exit 97

pass=0 fail=0
check() { # check "name" expected(ok|blocked) command...
    local name=$1 want=$2; shift 2
    if timeout 3 "$@" >/dev/null 2>&1; then got=ok; else got=blocked; fi
    if [[ $got == "$want" ]]; then pass=$((pass+1)); echo "PASS  $name ($got)"; else fail=$((fail+1)); echo "FAIL  $name (want $want, got $got)"; fi
}

# ---- topology
$NETUP || { echo "net-up.sh failed"; cat $NETUP; exit 96; }
vm() { # vm NAME HOSTIF IP
    ip netns add "$1"
    ip link add "$2" type veth peer name eth0 netns "$1"
    ip link set "$2" master br-fc up
    ip -n "$1" addr add "$3/24" dev eth0
    ip -n "$1" link set eth0 up; ip -n "$1" link set lo up
    ip -n "$1" route add default via 10.200.0.1
}
vm vmA fltapA 10.200.0.11
vm vmB fltapB 10.200.0.12
vm bld tapbld  10.200.0.13        # a tap NOT matching fltap*
# outside world: one netns reachable via a veth from the host
ip netns add out
ip link add up0 type veth peer name eth0 netns out
ip addr add 192.0.2.1/24 dev up0; ip link set up0 up
ip -n out addr add 192.0.2.10/24 dev eth0; ip -n out addr add 192.0.2.50/24 dev eth0
ip -n out addr add 169.254.169.254/32 dev eth0; ip -n out addr add 198.51.100.7/32 dev eth0
ip -n out link set eth0 up; ip -n out link set lo up
ip -n out route add default via 192.0.2.1
ip route add 169.254.169.254/32 via 192.0.2.10
ip route add 198.51.100.0/24 via 192.0.2.10

# listeners
listen() { ( while true; do "$@" -l -k -n 2>/dev/null; sleep 0.1; done ) & }
listen ip netns exec vmB nc 10.200.0.12 1234
listen ip netns exec bld nc 10.200.0.13 1234
listen nc 10.200.0.1 53
listen nc 10.200.0.1 22
listen nc 10.200.0.1 5000
listen nc 10.200.0.1 3128
listen nc 10.200.0.1 3129
listen nc 0.0.0.0 9477
listen ip netns exec out nc 169.254.169.254 80
listen ip netns exec out nc 198.51.100.7 80
listen ip netns exec out nc 192.0.2.50 80
sleep 1

# builder port map (what the daemon does)
nft add element inet firerunner builders '{ 20001 : 10.200.0.13 . 1234 }'

A="ip netns exec vmA"
check "VM->VM same bridge (fltap names)"          blocked $A ping -c1 -W1 10.200.0.12
check "VM->VM tap without fltap name"             blocked $A ping -c1 -W1 10.200.0.13
check "VM->host DNS tcp/53 on .1"                 ok      $A nc -z -w2 10.200.0.1 53
check "VM->host sshd tcp/22"                      blocked $A nc -z -w2 10.200.0.1 22
check "VM->host registry tcp/5000"                ok      $A nc -z -w2 10.200.0.1 5000
check "VM->host metrics tcp/9477"                 blocked $A nc -z -w2 10.200.0.1 9477
check "VM->builder through DNAT .1:20001"         ok      $A nc -z -w2 10.200.0.1 20001
ip -n vmA route add 10.200.0.13/32 via 10.200.0.1
check "VM->builder routed via host, no DNAT"      blocked $A nc -z -w2 10.200.0.13 1234
ip -n vmA route del 10.200.0.13/32
check "VM->cloud metadata 169.254.169.254"        blocked $A nc -z -w2 169.254.169.254 80
check "VM->FR_EGRESS_DENY 198.51.100.7"           blocked $A nc -z -w2 198.51.100.7 80
check "VM->internet-like 192.0.2.50 (NAT)"        ok      $A nc -z -w2 192.0.2.50 80
check "metrics from allowed 192.0.2.10"           ok      ip netns exec out nc -z -w2 -s 192.0.2.10 192.0.2.1 9477
check "metrics from other 192.0.2.50"             blocked ip netns exec out nc -z -w2 -s 192.0.2.50 192.0.2.1 9477
check "metrics from localhost"                    ok      nc -z -w2 127.0.0.1 9477

# reload keeps the daemon's builder mapping
$NETUP || { echo "reload failed"; fail=$((fail+1)); }
check "builder mapping survives a reload"          ok      bash -c "nft list map inet firerunner builders | grep -q '20001 : 10.200.0.13 . 1234'"
check "VM->builder through DNAT after reload"      ok      $A nc -z -w2 10.200.0.1 20001
check "VM->VM TCP to vmB blocked"                   blocked $A nc -z -w2 10.200.0.12 1234
check "VM->VM still blocked after reload"          blocked $A ping -c1 -W1 10.200.0.12


# ingress from the uplink: a LAN host that routes the microVM subnet via this host
ip -n out route add 10.200.0.0/24 via 192.0.2.1
L="ip netns exec out"
check "LAN->VM tcp/1234 (forwarded into the subnet)" blocked $L nc -z -w2 10.200.0.12 1234
check "LAN->bridge address registry .1:5000"        blocked $L nc -z -w2 10.200.0.1 5000
check "LAN->bridge address DNS tcp .1:53"           blocked $L nc -z -w2 10.200.0.1 53
check "host itself->bridge address .1:5000"         ok      nc -z -w2 10.200.0.1 5000
check "host->VM (how the executor reaches sshd)"   ok      nc -z -w2 10.200.0.12 1234
check "VM egress still gets replies"                ok      $A nc -z -w2 192.0.2.50 80

# flintlock gives every microVM a second tap for the metadata service (eth0 in the
# guest, 169.254.0.1/16). It is up on the host but not on the bridge; Firecracker
# answers the metadata address itself, so nothing sent on it may reach the host.
sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0 2>/dev/null
ip link add fltapM type veth peer name mds0 netns vmA
ip link set fltapM up
ip -n vmA addr add 169.254.0.1/16 dev mds0; ip -n vmA link set mds0 up
listen nc -6 :: 2222
listen nc 0.0.0.0 2223
sleep 3   # IPv6 link-local addresses finish DAD
HLL=$(ip -6 addr show dev fltapM scope link | awk '/inet6/{sub("/.*","",$2); print $2; exit}')
# IPv4 through the metadata tap, sourced from the VM's own bridge address so the
# host's reply comes back over the bridge (loose rp_filter, the Ubuntu default).
sysctl -qw net.ipv4.conf.all.rp_filter=2 net.ipv4.conf.fltapM.rp_filter=2
ip -n vmA route add 192.0.2.1/32 dev mds0 src 10.200.0.11
check "metadata tap->host over IPv6 link-local"     blocked $A nc -6 -z -w2 "$HLL%mds0" 2222
check "metadata tap->host uplink address (IPv4)"    blocked $A nc -z -w2 -s 10.200.0.11 192.0.2.1 2223
check "metadata tap->other VM"                      blocked $A nc -z -w2 -s 169.254.0.1 10.200.0.12 1234
# the proxy forwarder on the bridge address: for microVMs, not for the LAN
check "VM->proxy forwarder .1:3128"                 ok      $A nc -z -w2 10.200.0.1 3128
check "VM->other host port .1:3129"                 blocked $A nc -z -w2 10.200.0.1 3129
check "LAN->proxy forwarder .1:3128"                blocked $L nc -z -w2 10.200.0.1 3128
echo "---- rendered rules"; nft list table inet firerunner | sed -n '/chain input/,/^}/p'
echo "RESULT pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
