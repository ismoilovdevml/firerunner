#!/usr/bin/env bash
# FireRunner host installer.
#
# Prepares a KVM host (bare metal or a VM with nested virtualization) to run
# Firecracker microVMs through Flintlock:
#   containerd (devmapper thin pool) + Firecracker/jailer + flintlockd + bridge/DHCP/NAT
#
# Usage:
#   curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
#   curl -sfL .../install.sh | sudo FR_DISK=/dev/sdb bash      # choose the thin-pool disk
#   curl -sfL .../install.sh | sudo bash -s -- uninstall        # remove services (keeps data)
#
# Settings (environment variables):
#   FR_DISK                 empty block device for the thin pool (default: auto-detect a blank disk)
#   FR_BRIDGE               bridge for microVM taps          (default: br-fc)
#   FR_SUBNET               /24 prefix for microVMs           (default: 10.200.0)
#   FR_VM_DISK              root disk of every microVM, thin-provisioned (default: 40GB)
#   CONTAINERD_VERSION      (default: 1.7.35)
#   FIRECRACKER_VERSION     (default: 1.17.0)
#   FLINTLOCK_VERSION       (default: 0.15.2)
#
#   FR_VERSION              firerunner release to install (default: edge = latest main)
#   FR_POOL_SIZE            pre-booted microVMs kept ready (default: 2)
#   FR_METRICS_ALLOW        source IPv4/CIDR allowed to scrape :9477/metrics (default: none, localhost only;
#                           remembered for later re-runs)
#   FR_EGRESS_DENY          comma-separated IPv4 CIDRs jobs must not reach, e.g. 192.168.0.0/16
#                           (default: none; link-local 169.254.0.0/16 is always blocked)
#   REGISTRY_VERSION        Docker Hub pull-through mirror for microVMs (default: 3.1.1)
#   VERSITYGW_VERSION       S3 server for the runner's cache: (default: 1.8.0)
#   FR_CACHE_DAYS           delete cache: archives not written for this many days (default: 14)
#   GITLAB_RUNNER_VERSION   (default: 19.4.0)
#
# Register a GitLab runner right away (optional, can be done later with
# `firerunner runner register`):
#   FR_GITLAB_URL           e.g. https://gitlab.example.com
#   FR_RUNNER_TOKEN         runner authentication token (glrt-...) from
#                           Settings > CI/CD > Runners > New project/group/instance runner
#   FR_RUNNER_CONCURRENT    max parallel jobs / microVMs (default: 4)
#
# Safe to re-run: every step checks current state first.

set -euo pipefail

CONTAINERD_VERSION="${CONTAINERD_VERSION:-1.7.35}"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-1.17.0}"
FLINTLOCK_VERSION="${FLINTLOCK_VERSION:-0.15.2}"
REGISTRY_VERSION="${REGISTRY_VERSION:-3.1.1}"
VERSITYGW_VERSION="${VERSITYGW_VERSION:-1.8.0}"
FR_CACHE_DAYS="${FR_CACHE_DAYS:-14}"
GITLAB_RUNNER_VERSION="${GITLAB_RUNNER_VERSION:-19.4.0}"
FR_VERSION="${FR_VERSION:-edge}"
FR_BINARY="${FR_BINARY:-}"            # local firerunner binary instead of a release (development)

FR_DISK="${FR_DISK:-}"
FR_BRIDGE="${FR_BRIDGE:-br-fc}"
FR_SUBNET="${FR_SUBNET:-10.200.0}"
FR_VM_DISK="${FR_VM_DISK:-40GB}"

FR_GITLAB_URL="${FR_GITLAB_URL:-}"
FR_RUNNER_TOKEN="${FR_RUNNER_TOKEN:-}"
FR_RUNNER_CONCURRENT="${FR_RUNNER_CONCURRENT:-4}"
FR_POOL_SIZE="${FR_POOL_SIZE:-2}"
FR_METRICS_ALLOW="${FR_METRICS_ALLOW:-}"
FR_EGRESS_DENY="${FR_EGRESS_DENY:-}"
FR_REPO=ismoilovdevml/firerunner

BIN_DIR=/usr/local/bin
LIB_DIR=/usr/local/lib/firerunner
CONF_DIR=/etc/firerunner
VG=flintlock
THINPOOL="${VG}-thinpool"          # device-mapper name of ${VG}/thinpool
CONTAINERD_ROOT=/var/lib/containerd-flintlock
CONTAINERD_STATE=/run/containerd-flintlock
CONTAINERD_SOCK="${CONTAINERD_STATE}/containerd.sock"
FLINTLOCK_ENDPOINT=127.0.0.1:9090

TMP_DIR=""

log()  { printf '[firerunner] %s\n' "$*"; }
warn() { printf '[firerunner] WARNING: %s\n' "$*" >&2; }
die()  { printf '[firerunner] ERROR: %s\n' "$*" >&2; exit 1; }

cleanup() { [[ -n "$TMP_DIR" ]] && rm -rf "$TMP_DIR"; return 0; }
trap cleanup EXIT
# set -e exits silently; say where and why.
trap 'printf "[firerunner] ERROR: line %s: %s (exit %s)\n" "$LINENO" "$BASH_COMMAND" "$?" >&2' ERR

# --------------------------------------------------------------------------
# Preflight
# --------------------------------------------------------------------------

preflight() {
    [[ $EUID -eq 0 ]] || die "run as root (curl ... | sudo bash)"
    command -v systemctl >/dev/null || die "systemd is required"

    # Both end up in firewall rules.
    if [[ -n $FR_METRICS_ALLOW ]]; then
        is_ipv4_cidr "$FR_METRICS_ALLOW" || die "FR_METRICS_ALLOW=$FR_METRICS_ALLOW is not an IPv4 address or CIDR"
    fi
    local cidr
    for cidr in ${FR_EGRESS_DENY//,/ }; do
        is_ipv4_cidr "$cidr" || die "FR_EGRESS_DENY: $cidr is not an IPv4 address or CIDR"
    done

    # Only x86_64 is built and tested for now.
    [[ "$(uname -m)" == x86_64 ]] || die "unsupported architecture: $(uname -m) (x86_64 only)"
    ARCH=amd64
    FC_ARCH=x86_64

    # shellcheck disable=SC1091
    . /etc/os-release
    case "${ID_LIKE:-} ${ID}" in
        *debian*|*ubuntu*) PKG=apt ;;
        *rhel*|*fedora*|*centos*|*rocky*|*almalinux*) PKG=dnf ;;
        *) die "unsupported OS: ${PRETTY_NAME:-unknown} (need Ubuntu/Debian or RHEL-family)" ;;
    esac
    log "OS: ${PRETTY_NAME}, arch: ${ARCH}"

    if [[ ! -c /dev/kvm ]]; then
        if grep -qE 'vmx|svm' /proc/cpuinfo; then
            modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
        fi
        [[ -c /dev/kvm ]] || die "/dev/kvm not found. On bare metal enable VT-x/AMD-V in BIOS.
  In a VM enable nested virtualization (VMware: 'Expose hardware assisted
  virtualization to the guest OS'; KVM: nested=1; cloud: a nested-virt instance type)."
    fi
    if systemd-detect-virt -q 2>/dev/null; then
        log "running inside a VM ($(systemd-detect-virt)); nested virtualization detected via /dev/kvm"
    fi
}

install_packages() {
    log "installing packages"
    if [[ $PKG == apt ]]; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq curl tar lvm2 thin-provisioning-tools dnsmasq-base dnsmasq-utils nftables iproute2 openssh-client >/dev/null
    else
        dnf install -y -q curl tar lvm2 device-mapper-persistent-data dnsmasq dnsmasq-utils nftables iproute openssh-clients >/dev/null
    fi
}

# --------------------------------------------------------------------------
# Downloads (always checksum-verified)
# --------------------------------------------------------------------------

# fetch URL DEST
fetch() { curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1" || die "download failed: $1"; }

# verify FILE SHA256
verify() {
    local got
    got=$(sha256sum "$1" | awk '{print $1}')
    [[ "$got" == "$2" ]] || die "checksum mismatch for $(basename "$1"): got $got, want $2"
}

# Files that changed in this run; a service restarts only if one of its
# files changed, so re-running the installer does not kill running jobs.
CHANGED=" "

# put FILE [MODE]: write stdin to FILE only when the content differs.
put() {
    local f=$1 mode=${2:-0644} tmp
    tmp=$(mktemp)
    cat >"$tmp"
    if [[ -f $f ]] && cmp -s "$tmp" "$f"; then
        rm -f "$tmp"
        chmod "$mode" "$f"
        return 0
    fi
    mkdir -p "$(dirname "$f")"
    install -m "$mode" "$tmp" "$f"
    rm -f "$tmp"
    CHANGED+="$f "
}

# installed FILE: record that a binary was (re)installed.
installed() { CHANGED+="$1 "; }

changed() {
    local f
    for f in "$@"; do [[ $CHANGED == *" $f "* ]] && return 0; done
    return 1
}

# --------------------------------------------------------------------------
# containerd with devmapper snapshotter
# --------------------------------------------------------------------------

install_containerd() {
    local have=""
    [[ -x $BIN_DIR/containerd ]] && have=$($BIN_DIR/containerd --version | awk '{print $3}' || true)
    if [[ "$have" != "v${CONTAINERD_VERSION}" ]]; then
        log "installing containerd v${CONTAINERD_VERSION}"
        local base="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}"
        local tgz="containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz"
        fetch "$base/$tgz" "$TMP_DIR/$tgz"
        fetch "$base/$tgz.sha256sum" "$TMP_DIR/$tgz.sha256sum"
        verify "$TMP_DIR/$tgz" "$(awk '{print $1}' "$TMP_DIR/$tgz.sha256sum")"
        tar -xzf "$TMP_DIR/$tgz" -C /usr/local
        installed "$BIN_DIR/containerd"
    else
        log "containerd v${CONTAINERD_VERSION} already installed"
    fi

    mkdir -p "$CONF_DIR" "$CONTAINERD_ROOT/snapshotter/devmapper"
    OLD_VM_DISK=$(sed -n 's/^ *base_image_size = "\(.*\)"/\1/p' "$CONF_DIR/containerd.toml" 2>/dev/null || true)
    put "$CONF_DIR/containerd.toml" <<EOF
version = 2
root = "${CONTAINERD_ROOT}"
state = "${CONTAINERD_STATE}"

[grpc]
  address = "${CONTAINERD_SOCK}"

[metrics]
  address = "127.0.0.1:1338"

[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "${THINPOOL}"
  root_path = "${CONTAINERD_ROOT}/snapshotter/devmapper"
  base_image_size = "${FR_VM_DISK}"
  discard_blocks = true
EOF

    # Dedicated instance so it never clashes with a Docker/Kubernetes containerd.
    put /etc/systemd/system/containerd-flintlock.service <<EOF
[Unit]
Description=containerd for flintlock microVMs
After=network.target local-fs.target lvm2-monitor.service

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStartPre=-/sbin/modprobe dm_thin_pool
ExecStart=${BIN_DIR}/containerd --config ${CONF_DIR}/containerd.toml
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=1048576
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
EOF
}

# --------------------------------------------------------------------------
# LVM thin pool
# --------------------------------------------------------------------------

# Prints the first whole disk that has no partitions, filesystem, LVM or mount.
find_blank_disk() {
    local name type
    while read -r name type; do
        [[ $type == disk ]] || continue
        [[ $name == sr* || $name == loop* || $name == zram* ]] && continue
        [[ $(lsblk -nro NAME "/dev/$name" | wc -l) -eq 1 ]] || continue   # has children
        [[ -z $(blkid -p "/dev/$name" 2>/dev/null) ]] || continue           # has a signature
        findmnt -rn -S "/dev/$name" >/dev/null && continue
        echo "/dev/$name"
        return 0
    done < <(lsblk -dnro NAME,TYPE)
    return 1
}

setup_thinpool() {
    if lvs "$VG/thinpool" >/dev/null 2>&1; then
        log "thin pool $VG/thinpool already exists"
    else
        if [[ -z $FR_DISK ]]; then
            FR_DISK=$(find_blank_disk) || die "no blank disk found for the thin pool.
  Attach an empty disk (e.g. 100G+) and re-run, or set FR_DISK=/dev/sdX (it will be wiped)."
            log "using blank disk $FR_DISK for the thin pool"
        fi
        [[ -b $FR_DISK ]] || die "FR_DISK=$FR_DISK is not a block device"
        findmnt -rn -S "$FR_DISK" >/dev/null && die "$FR_DISK is mounted"

        log "creating LVM thin pool on $FR_DISK"
        pvcreate -qy "$FR_DISK"
        vgcreate -q "$VG" "$FR_DISK"
        lvcreate -q --wipesignatures y -n thinpool "$VG" -l 95%VG
        lvcreate -q --wipesignatures y -n thinpoolmeta "$VG" -l 1%VG
        lvconvert -qy --zero n -c 512K --thinpool "$VG/thinpool" --poolmetadata "$VG/thinpoolmeta"
    fi

    mkdir -p /etc/lvm/profile
    put /etc/lvm/profile/${THINPOOL}.profile <<'EOF'
activation {
  thin_pool_autoextend_threshold=80
  thin_pool_autoextend_percent=20
}
EOF
    lvchange -q --metadataprofile "$THINPOOL" "$VG/thinpool"
    lvchange -q --monitor y "$VG/thinpool" || warn "could not enable thin pool monitoring"
}

# --------------------------------------------------------------------------
# Firecracker + jailer
# --------------------------------------------------------------------------

install_firecracker() {
    local have=""
    [[ -x $BIN_DIR/firecracker ]] && have=$($BIN_DIR/firecracker --version 2>/dev/null | head -1 | awk '{print $2}' || true)
    if [[ "$have" == "v${FIRECRACKER_VERSION}" ]]; then
        log "firecracker v${FIRECRACKER_VERSION} already installed"
        return
    fi
    log "installing firecracker v${FIRECRACKER_VERSION}"
    local base="https://github.com/firecracker-microvm/firecracker/releases/download/v${FIRECRACKER_VERSION}"
    local tgz="firecracker-v${FIRECRACKER_VERSION}-${FC_ARCH}.tgz"
    fetch "$base/$tgz" "$TMP_DIR/$tgz"
    fetch "$base/$tgz.sha256.txt" "$TMP_DIR/$tgz.sha256.txt"
    verify "$TMP_DIR/$tgz" "$(awk '{print $1}' "$TMP_DIR/$tgz.sha256.txt")"
    tar -xzf "$TMP_DIR/$tgz" -C "$TMP_DIR"
    local rel="$TMP_DIR/release-v${FIRECRACKER_VERSION}-${FC_ARCH}"
    install -m 0755 "$rel/firecracker-v${FIRECRACKER_VERSION}-${FC_ARCH}" "$BIN_DIR/firecracker"
    install -m 0755 "$rel/jailer-v${FIRECRACKER_VERSION}-${FC_ARCH}" "$BIN_DIR/jailer"
    installed "$BIN_DIR/firecracker"
}

# --------------------------------------------------------------------------
# microVM network: bridge + DHCP/DNS + NAT
# --------------------------------------------------------------------------

is_ipv4_cidr() { [[ $1 =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}(/[0-9]{1,2})?$ ]]; }

# The source allowed to scrape metrics. It is remembered, so a re-run without
# FR_METRICS_ALLOW keeps the rule instead of opening :9477 to everyone.
metrics_allow() {
    if [[ -n $FR_METRICS_ALLOW ]]; then
        echo "$FR_METRICS_ALLOW"
    elif [[ -s $CONF_DIR/metrics-allow ]]; then
        cat "$CONF_DIR/metrics-allow"
    fi
}

# nft rules that keep :9477 to the allowed source, also on hosts without
# firewalld or ufw. Nothing when metrics were never opened.
metrics_input_rules() {
    local allow
    allow=$(metrics_allow)
    [[ -n $allow ]] || return 0
    printf '    tcp dport 9477 iifname "lo" accept\n'
    printf '    tcp dport 9477 ip saddr %s accept\n' "$allow"
    printf '    tcp dport 9477 drop\n'
}

# FR_EGRESS_DENY: extra destinations jobs must not reach, e.g. internal ranges.
egress_deny_rules() {
    [[ -n $FR_EGRESS_DENY ]] || return 0
    printf '    iifname "%s" ip daddr { %s } drop\n' "$FR_BRIDGE" "${FR_EGRESS_DENY//,/, }"
}

setup_network() {
    log "configuring bridge ${FR_BRIDGE} (${FR_SUBNET}.0/24) with DHCP and NAT"
    mkdir -p "$LIB_DIR" "$CONF_DIR"

    put "$LIB_DIR/net-up.sh" 0755 <<EOF
#!/usr/bin/env bash
set -euo pipefail
ip link show ${FR_BRIDGE} >/dev/null 2>&1 || ip link add ${FR_BRIDGE} type bridge
ip addr replace ${FR_SUBNET}.1/24 dev ${FR_BRIDGE}
# microVMs get IPv4 only; with IPv6 on the bridge they could reach host services via fe80::.
sysctl -qw net.ipv6.conf.${FR_BRIDGE}.disable_ipv6=1 2>/dev/null || true
ip link set ${FR_BRIDGE} up
sysctl -qw net.ipv4.ip_forward=1
# Keep the daemon's builder port mappings across a reload of these rules.
builders=\$(nft list map inet firerunner builders 2>/dev/null | tr -d '\n\t' | sed -n 's/.*elements = {\([^}]*\)}.*/\1/p' || true)
nft -f - <<'NFT'
table ip firerunner
delete table ip firerunner
table inet firerunner
delete table inet firerunner
table bridge firerunner
delete table bridge firerunner
table inet firerunner {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr ${FR_SUBNET}.0/24 oifname != "${FR_BRIDGE}" masquerade
    # builder hairpin: a builder answers the bridge address, not the job VM (iifname
    # is not set here for bridged packets under br_netfilter, so match the source)
    ip saddr ${FR_SUBNET}.0/24 oifname "${FR_BRIDGE}" ct status dnat masquerade
  }
  chain input {
    type filter hook input priority filter; policy accept;
    # microVMs may only use DHCP, DNS, the registry mirror and the cache server on
    # the host (IPv4 and IPv6). DHCP requests are broadcasts, everything else must
    # be addressed to the bridge address.
    iifname "${FR_BRIDGE}" udp dport 67 accept
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 udp dport 53 accept
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 tcp dport { 53, 5000, 9000 } accept
    iifname "${FR_BRIDGE}" ct state established,related accept
    iifname "${FR_BRIDGE}" drop
$(metrics_input_rules)
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    # A job VM reaches its project's builder through the bridge address (DNAT
    # below); any other traffic routed from one microVM to another is dropped.
    iifname "${FR_BRIDGE}" oifname "${FR_BRIDGE}" ct status dnat accept
    iifname "${FR_BRIDGE}" oifname "${FR_BRIDGE}" drop
    # Link-local addresses (cloud instance metadata, 169.254.169.254) are never
    # reachable from jobs. Firecracker answers the VM's own MMDS before the tap.
    iifname "${FR_BRIDGE}" ip daddr 169.254.0.0/16 drop
$(egress_deny_rules)
  }
  # Job VMs reach their project's BuildKit builder at ${FR_SUBNET}.1:<port>; the
  # firerunner daemon fills the map. Routed through the host, never VM to VM.
  map builders { type inet_service : ipv4_addr . inet_service; }
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 dnat ip to tcp dport map @builders
  }
  # Every packet addressed to the bridge address is marked (before DNAT) for the
  # bridge table below; it only reaches that table with br_netfilter.
  chain tag_bridge_address {
    type filter hook prerouting priority mangle; policy accept;
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 meta mark set meta mark | 0x10000000
  }
}
# Jobs of different projects share the bridge: no frame may pass between two
# microVMs. Only frames from one bridge port to another reach this hook (traffic
# to and from the host does not), so everything is dropped, whatever flintlock
# names the taps. The exception is builder traffic: with br_netfilter loaded, a
# connection DNAT'ed from the bridge address to a builder is bridged, not routed;
# prerouting marks everything addressed to the bridge address. Without
# br_netfilter the mark never reaches this table and nothing is bridged anyway.
table bridge firerunner {
  chain forward {
    type filter hook forward priority filter; policy drop;
    meta mark & 0x10000000 == 0x10000000 accept
  }
}
NFT
if [[ -n \$builders ]]; then
    nft add element inet firerunner builders "{ \$builders }" || true
fi
EOF

    put "$LIB_DIR/net-down.sh" 0755 <<EOF
#!/usr/bin/env bash
nft delete table ip firerunner 2>/dev/null || true
nft delete table inet firerunner 2>/dev/null || true
nft delete table bridge firerunner 2>/dev/null || true
ip link del ${FR_BRIDGE} 2>/dev/null || true
EOF

    put /etc/sysctl.d/90-firerunner.conf <<'EOF'
net.ipv4.ip_forward = 1
EOF

    put /etc/systemd/system/firerunner-net.service <<EOF
[Unit]
Description=FireRunner microVM bridge and NAT
After=network-online.target firewalld.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=${LIB_DIR}/net-up.sh
# Reload re-applies the rules in place; stop removes the bridge under running microVMs.
ExecReload=${LIB_DIR}/net-up.sh
ExecStop=${LIB_DIR}/net-down.sh

[Install]
WantedBy=multi-user.target
EOF

    put "$CONF_DIR/dnsmasq.conf" <<EOF
interface=${FR_BRIDGE}
bind-interfaces
except-interface=lo
# Short leases: every job gets a new VM with a new MAC, and firerunner releases
# a VM's lease when it deletes the VM.
dhcp-range=${FR_SUBNET}.10,${FR_SUBNET}.250,255.255.255.0,15m
dhcp-option=option:router,${FR_SUBNET}.1
dhcp-option=option:dns-server,${FR_SUBNET}.1
dhcp-leasefile=/var/lib/misc/firerunner-dnsmasq.leases
no-hosts
log-dhcp
EOF

    mkdir -p /var/lib/misc
    put /etc/systemd/system/firerunner-dnsmasq.service <<EOF
[Unit]
Description=FireRunner DHCP/DNS for microVMs
Requires=firerunner-net.service
After=firerunner-net.service

[Service]
ExecStart=$(command -v dnsmasq) --keep-in-foreground --conf-file=${CONF_DIR}/dnsmasq.conf
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    # Host firewalls: let bridge traffic through and masquerade it out.
    if systemctl is-active -q firewalld 2>/dev/null; then
        log "firewalld active: adding ${FR_BRIDGE} to the trusted zone"
        firewall-cmd -q --permanent --zone=trusted --add-interface="${FR_BRIDGE}" || true
        firewall-cmd -q --permanent --zone="$(firewall-cmd --get-default-zone)" --add-masquerade
        firewall-cmd -q --reload
    fi
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        log "ufw active: allowing routed traffic from ${FR_BRIDGE}"
        ufw route allow in on "${FR_BRIDGE}" >/dev/null
        ufw allow in on "${FR_BRIDGE}" to any port 67 proto udp >/dev/null
        ufw allow in on "${FR_BRIDGE}" to any port 53 >/dev/null
    fi
}

# --------------------------------------------------------------------------
# flintlockd
# --------------------------------------------------------------------------

install_flintlock() {
    local have=""
    [[ -x $BIN_DIR/flintlockd ]] && have=$($BIN_DIR/flintlockd version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
    if [[ "$have" != "${FLINTLOCK_VERSION}" ]]; then
        log "installing flintlockd v${FLINTLOCK_VERSION}"
        local base="https://github.com/liquidmetal-dev/flintlock/releases/download/v${FLINTLOCK_VERSION}"
        local bin="flintlockd_${ARCH}"
        fetch "$base/$bin" "$TMP_DIR/$bin"
        fetch "$base/checksums.txt" "$TMP_DIR/checksums.txt"
        verify "$TMP_DIR/$bin" "$(awk -v f="$bin" '$2==f {print $1}' "$TMP_DIR/checksums.txt")"
        install -m 0755 "$TMP_DIR/$bin" "$BIN_DIR/flintlockd"
        installed "$BIN_DIR/flintlockd"
    else
        log "flintlockd v${FLINTLOCK_VERSION} already installed"
    fi

    # API token: generated once, root-only. Clients send it as a bearer token.
    if [[ ! -s $CONF_DIR/flintlock.token ]]; then
        (umask 077; head -c 32 /dev/urandom | base64 | tr -d '/+=\n' >"$CONF_DIR/flintlock.token")
    fi
    chmod 0600 "$CONF_DIR/flintlock.token"
    rm -f "$CONF_DIR/flintlock.env"   # older installs passed the token on the command line
    # flintlockd reads every flag from this file (viper); the token never appears in argv.
    put /etc/opt/flintlockd/config.yaml 0600 <<EOF
containerd-socket: ${CONTAINERD_SOCK}
grpc-endpoint: ${FLINTLOCK_ENDPOINT}
bridge-name: ${FR_BRIDGE}
firecracker-bin: ${BIN_DIR}/firecracker
basic-auth-token: $(cat "$CONF_DIR/flintlock.token")
insecure: true
log-format: json
verbosity: 1
EOF

    put /etc/systemd/system/flintlockd.service <<EOF
[Unit]
Description=flintlock microVM service
Requires=containerd-flintlock.service firerunner-net.service
After=containerd-flintlock.service firerunner-net.service

[Service]
# All settings, including the API token, come from /etc/opt/flintlockd/config.yaml (0600).
ExecStart=${BIN_DIR}/flintlockd run
Restart=always
RestartSec=5
KillMode=process
# An OOM-killed microVM must not take the whole service (and every other VM) down.
OOMPolicy=continue
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
}

# --------------------------------------------------------------------------
# firerunner CLI + GitLab Runner
# --------------------------------------------------------------------------

install_registry_mirror() {
    local have=""
    [[ -x $BIN_DIR/registry ]] && have=$($BIN_DIR/registry --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
    if [[ "$have" != "${REGISTRY_VERSION}" ]]; then
        log "installing Docker Hub mirror (distribution v${REGISTRY_VERSION})"
        local base="https://github.com/distribution/distribution/releases/download/v${REGISTRY_VERSION}"
        local tgz="registry_${REGISTRY_VERSION}_linux_amd64.tar.gz"
        fetch "$base/$tgz" "$TMP_DIR/$tgz"
        fetch "$base/$tgz.sha256" "$TMP_DIR/$tgz.sha256"
        verify "$TMP_DIR/$tgz" "$(awk '{print $1}' "$TMP_DIR/$tgz.sha256")"
        tar -xzf "$TMP_DIR/$tgz" -C "$TMP_DIR" registry
        install -m 0755 "$TMP_DIR/registry" "$BIN_DIR/registry"
        installed "$BIN_DIR/registry"
    fi
    mkdir -p /var/lib/firerunner/registry
    put "$CONF_DIR/registry.yml" <<EOF
version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: /var/lib/firerunner/registry
  delete:
    enabled: false
http:
  addr: ${FR_SUBNET}.1:5000
proxy:
  remoteurl: https://registry-1.docker.io
  ttl: 168h
EOF
    put /etc/systemd/system/firerunner-registry.service <<EOF
[Unit]
Description=FireRunner Docker Hub pull-through mirror for microVMs
Requires=firerunner-net.service
After=firerunner-net.service

[Service]
ExecStart=${BIN_DIR}/registry serve ${CONF_DIR}/registry.yml
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/firerunner/registry
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
}

# S3 server for gitlab-runner's cache: (versitygw, plain files on disk). Jobs only get
# presigned URLs from the runner, so the keys stay on the host.
install_cache_server() {
    local have=""
    [[ -x $BIN_DIR/versitygw ]] && have=$($BIN_DIR/versitygw --version 2>/dev/null | awk '/^Version/ {print $3}' || true)
    if [[ "$have" != "${VERSITYGW_VERSION}" ]]; then
        log "installing cache server (versitygw v${VERSITYGW_VERSION})"
        local base="https://github.com/versity/versitygw/releases/download/v${VERSITYGW_VERSION}"
        local dir="versitygw_v${VERSITYGW_VERSION}_Linux_x86_64"
        fetch "$base/$dir.tar.gz" "$TMP_DIR/$dir.tar.gz"
        fetch "$base/checksums.txt" "$TMP_DIR/versitygw.sums"
        verify "$TMP_DIR/$dir.tar.gz" "$(awk -v f="$dir.tar.gz" '$2==f {print $1}' "$TMP_DIR/versitygw.sums")"
        tar -xzf "$TMP_DIR/$dir.tar.gz" -C "$TMP_DIR" "$dir/versitygw"
        install -m 0755 "$TMP_DIR/$dir/versitygw" "$BIN_DIR/versitygw"
        installed "$BIN_DIR/versitygw"
    fi
    # The top-level directory is the S3 root; each sub-directory is a bucket.
    mkdir -p /var/lib/firerunner/cache/runner-cache
    local key secret
    key=$(sed -n 's/^ROOT_ACCESS_KEY_ID=//p' "$CONF_DIR/cache.env" 2>/dev/null || true)
    secret=$(sed -n 's/^ROOT_SECRET_ACCESS_KEY=//p' "$CONF_DIR/cache.env" 2>/dev/null || true)
    # Read a fixed amount (no SIGPIPE under pipefail), then cut: ~70 and ~120 chars survive tr.
    [[ -n $key ]] || { key=$(head -c 512 /dev/urandom | tr -dc 'A-Z0-9'); key=${key:0:20}; }
    [[ -n $secret ]] || { secret=$(head -c 512 /dev/urandom | tr -dc 'A-Za-z0-9'); secret=${secret:0:40}; }
    put "$CONF_DIR/cache.env" 0600 <<EOF
VGW_PORT=${FR_SUBNET}.1:9000
ROOT_ACCESS_KEY_ID=${key}
ROOT_SECRET_ACCESS_KEY=${secret}
FR_CACHE_BUCKET=runner-cache
EOF
    put /etc/systemd/system/firerunner-cache.service <<EOF
[Unit]
Description=FireRunner S3 cache server for GitLab cache:
Requires=firerunner-net.service
After=firerunner-net.service

[Service]
EnvironmentFile=${CONF_DIR}/cache.env
ExecStart=${BIN_DIR}/versitygw posix /var/lib/firerunner/cache
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/firerunner/cache
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
    put /etc/systemd/system/firerunner-cache-clean.service <<EOF
[Unit]
Description=Delete FireRunner cache: archives older than ${FR_CACHE_DAYS} days

[Service]
Type=oneshot
ExecStart=/usr/bin/find /var/lib/firerunner/cache/runner-cache -mindepth 1 -type f -mtime +${FR_CACHE_DAYS} -delete
ExecStart=/usr/bin/find /var/lib/firerunner/cache/runner-cache -mindepth 1 -type d -empty -delete
EOF
    put /etc/systemd/system/firerunner-cache-clean.timer <<EOF
[Unit]
Description=Daily cleanup of the FireRunner cache

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
EOF
}

# Point the registered runner's cache: at the local cache server, unless an
# operator already configured one (firerunner runner cache s3 ...).
configure_runner_cache() {
    $BIN_DIR/firerunner runner status 2>/dev/null | grep -q '^cache *none' || return 0
    log "storing cache: on this host (firerunner runner cache)"
    $BIN_DIR/firerunner runner cache local >/dev/null
}

# A new root disk size only applies to images unpacked after the change:
# drop the unpacked images so flintlock pulls them again (running VMs keep theirs).
apply_vm_disk_size() {
    [[ -n ${OLD_VM_DISK:-} && $OLD_VM_DISK != "$FR_VM_DISK" ]] || return 0
    log "microVM disk size ${OLD_VM_DISK} -> ${FR_VM_DISK}: images are pulled again for new VMs"
    local ctr=("$BIN_DIR/ctr" -a "$CONTAINERD_SOCK" -n flintlock) img
    for img in $("${ctr[@]}" images ls -q 2>/dev/null); do
        "${ctr[@]}" images rm "$img" >/dev/null 2>&1 || warn "could not remove image $img"
    done
}

open_metrics_port() {
    if [[ -z $FR_METRICS_ALLOW ]]; then
        if [[ ! -s $CONF_DIR/metrics-allow && "$($BIN_DIR/firerunner config get daemon.metrics_listen 2>/dev/null)" == :* ]]; then
            warn "metrics listen on all interfaces without an allowed source; re-run with FR_METRICS_ALLOW=<prometheus address>"
        fi
        return 0
    fi
    log "allowing ${FR_METRICS_ALLOW} to scrape :9477/metrics"
    # net-up.sh reads it to keep the port closed to anyone else (setup_network).
    put "$CONF_DIR/metrics-allow" <<<"$FR_METRICS_ALLOW"
    # metrics listen on 127.0.0.1 by default; open them only together with the firewall rule
    if [[ "$($BIN_DIR/firerunner config get daemon.metrics_listen)" != ":9477" ]]; then
        $BIN_DIR/firerunner config set daemon.metrics_listen ":9477" >/dev/null
        systemctl restart firerunner
    fi
    if systemctl is-active -q firewalld 2>/dev/null; then
        firewall-cmd -q --permanent --add-rich-rule="rule family=ipv4 source address=${FR_METRICS_ALLOW} port port=9477 protocol=tcp accept"
        firewall-cmd -q --reload
    elif command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        ufw allow from "${FR_METRICS_ALLOW}" to any port 9477 proto tcp >/dev/null
    fi
}

install_firerunner() {
    if [[ -n $FR_BINARY ]]; then
        log "installing firerunner from $FR_BINARY"
        put "$BIN_DIR/firerunner" 0755 <"$FR_BINARY"
    else
        log "installing firerunner ${FR_VERSION}"
        local base="https://github.com/${FR_REPO}/releases/download/${FR_VERSION}"
        local bin="firerunner-linux-${ARCH}"
        fetch "$base/$bin" "$TMP_DIR/$bin"
        fetch "$base/checksums.txt" "$TMP_DIR/fr.sums"
        verify "$TMP_DIR/$bin" "$(awk -v f="$bin" '$2==f {print $1}' "$TMP_DIR/fr.sums")"
        put "$BIN_DIR/firerunner" 0755 <"$TMP_DIR/$bin"
    fi
    log "  $($BIN_DIR/firerunner version)"

    mkdir -p "$CONF_DIR/executor"
    chmod 0700 "$CONF_DIR/executor"
    [[ -f $CONF_DIR/executor/id_ed25519 ]] ||
        ssh-keygen -q -t ed25519 -N "" -C firerunner-executor -f "$CONF_DIR/executor/id_ed25519"
    # Write the default config once so it is visible and editable.
    if [[ ! -f $CONF_DIR/config.yaml ]]; then
        $BIN_DIR/firerunner config set pool.size "$FR_POOL_SIZE" >/dev/null
        $BIN_DIR/firerunner config set vm.registry_mirror "http://${FR_SUBNET}.1:5000" >/dev/null
    fi

    put /etc/systemd/system/firerunner.service <<EOF
[Unit]
Description=FireRunner daemon (microVM pool, reconcile, metrics)
Requires=flintlockd.service
After=flintlockd.service

[Service]
ExecStart=${BIN_DIR}/firerunner daemon
Restart=always
RestartSec=5
TimeoutStopSec=60
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
RuntimeDirectory=firerunner
RuntimeDirectoryPreserve=yes
# /var/lib/flintlock/vm: reconcile removes the state dirs flintlockd leaves for deleted VMs.
ReadWritePaths=/run/firerunner -/run/lock -/run/lvm -/var/lib/flintlock/vm
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
EOF

    local have=""
    [[ -x $BIN_DIR/gitlab-runner ]] && have=$($BIN_DIR/gitlab-runner --version 2>/dev/null | awk '/^Version:/ {print $2}' || true)
    if [[ "$have" != "${GITLAB_RUNNER_VERSION}" ]]; then
        log "installing gitlab-runner v${GITLAB_RUNNER_VERSION}"
        local base="https://gitlab-runner-downloads.s3.amazonaws.com/v${GITLAB_RUNNER_VERSION}"
        local bin="gitlab-runner-linux-${ARCH}"
        fetch "$base/binaries/$bin" "$TMP_DIR/$bin"
        fetch "$base/release.sha256" "$TMP_DIR/runner.sums"
        verify "$TMP_DIR/$bin" "$(awk -v f="binaries/$bin" '$2==f {print $1}' "$TMP_DIR/runner.sums")"
        systemctl stop gitlab-runner 2>/dev/null || true
        install -m 0755 "$TMP_DIR/$bin" "$BIN_DIR/gitlab-runner"
        systemctl start gitlab-runner 2>/dev/null || true
    fi
}

register_runner() {
    [[ -n $FR_GITLAB_URL || -n $FR_RUNNER_TOKEN ]] || return 0
    [[ -n $FR_GITLAB_URL && -n $FR_RUNNER_TOKEN ]] || die "set both FR_GITLAB_URL and FR_RUNNER_TOKEN"
    if $BIN_DIR/firerunner runner status >/dev/null 2>&1; then
        log "a GitLab runner is already registered (firerunner runner status)"
        return 0
    fi
    log "registering GitLab runner at ${FR_GITLAB_URL}"
    FIRERUNNER_RUNNER_TOKEN="$FR_RUNNER_TOKEN" $BIN_DIR/firerunner runner register \
        --url "$FR_GITLAB_URL" --concurrent "$FR_RUNNER_CONCURRENT"
}

# --------------------------------------------------------------------------
# Start and verify
# --------------------------------------------------------------------------

start_services() {
    systemctl daemon-reload
    local svc files
    for svc in containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd; do
        systemctl enable -q "$svc"
        case $svc in
            containerd-flintlock) files="$CONF_DIR/containerd.toml $BIN_DIR/containerd" ;;
            firerunner-net)       files="$LIB_DIR/net-up.sh" ;;
            firerunner-dnsmasq)   files="$CONF_DIR/dnsmasq.conf" ;;
            firerunner-registry)  files="$CONF_DIR/registry.yml $BIN_DIR/registry" ;;
            firerunner-cache)     files="$CONF_DIR/cache.env $BIN_DIR/versitygw" ;;
            flintlockd)           files="/etc/opt/flintlockd/config.yaml $BIN_DIR/flintlockd $BIN_DIR/firecracker" ;;
        esac
        # shellcheck disable=SC2086
        if ! systemctl is-active -q "$svc"; then
            log "  starting $svc"
            systemctl restart "$svc"
        elif changed "/etc/systemd/system/$svc.service" $files; then
            if [[ $svc == firerunner-net ]]; then
                # A restart would delete the bridge and cut every running microVM off.
                log "  reloading $svc rules"
                systemctl reload "$svc"
            else
                log "  restarting $svc"
                systemctl restart "$svc"
            fi
        fi
    done
    systemctl enable -q --now firerunner-cache-clean.timer
}

verify_install() {
    log "verifying"
    local ok=1 svc
    for svc in containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd firerunner; do
        if systemctl is-active -q "$svc"; then
            log "  $svc: active"
        else
            warn "  $svc: NOT active (journalctl -u $svc)"; ok=0
        fi
    done
    local _
    for _ in $(seq 1 15); do
        ss -ltn | grep -q " ${FLINTLOCK_ENDPOINT} " && break
        sleep 1
    done
    if ss -ltn | grep -q " ${FLINTLOCK_ENDPOINT} "; then
        log "  flintlock gRPC listening on ${FLINTLOCK_ENDPOINT}"
    else
        warn "  flintlock gRPC not listening on ${FLINTLOCK_ENDPOINT}"; ok=0
    fi
    dmsetup status "$THINPOOL" >/dev/null 2>&1 && log "  thin pool ${THINPOOL}: ok" || { warn "  thin pool ${THINPOOL} missing"; ok=0; }
    [[ $ok -eq 1 ]] || die "installation finished with errors"

    cat <<EOF

FireRunner host is ready.
  flintlock API : ${FLINTLOCK_ENDPOINT} (localhost only)
  API token     : ${CONF_DIR}/flintlock.token (root only)
  microVM net   : ${FR_BRIDGE} ${FR_SUBNET}.0/24, DHCP + NAT
  versions      : containerd ${CONTAINERD_VERSION}, firecracker ${FIRECRACKER_VERSION}, flintlock ${FLINTLOCK_VERSION}

Next steps:
  firerunner doctor                  check the host
  firerunner run -- uname -a         boot a throwaway microVM
  firerunner runner register --url https://gitlab.example.com --token glrt-...
  firerunner config set vm.vcpu 4    tune microVM size
EOF
}

uninstall() {
    [[ $EUID -eq 0 ]] || die "run as root"
    log "stopping and removing services (thin pool and images are kept)"
    # The daemon keeps its warm pool across restarts, so delete microVMs explicitly.
    if [[ -x $BIN_DIR/firerunner ]] && systemctl is-active -q flintlockd; then
        $BIN_DIR/firerunner vm rm --all >/dev/null 2>&1 || warn "could not delete all microVMs"
    fi
    local svc
    if [[ -x $BIN_DIR/gitlab-runner ]]; then
        # The runner stays registered in GitLab; delete it there if it is no longer needed.
        $BIN_DIR/gitlab-runner uninstall --service gitlab-runner >/dev/null 2>&1 || true
        rm -f "$BIN_DIR/gitlab-runner"
    fi
    systemctl disable --now -q firerunner-cache-clean.timer 2>/dev/null || true
    rm -f /etc/systemd/system/firerunner-cache-clean.{service,timer}
    for svc in firerunner flintlockd firerunner-cache firerunner-registry firerunner-dnsmasq firerunner-net containerd-flintlock; do
        systemctl disable --now -q "$svc" 2>/dev/null || true
        rm -f "/etc/systemd/system/$svc.service"
    done
    systemctl daemon-reload
    rm -f /etc/sysctl.d/90-firerunner.conf "$BIN_DIR/flintlockd" "$BIN_DIR/firerunner" "$BIN_DIR/registry" "$BIN_DIR/versitygw"
    rm -rf "$LIB_DIR"
    log "done. To also drop data: vgremove $VG && rm -rf $CONTAINERD_ROOT /var/lib/flintlock $CONF_DIR"
}

main() {
    case "${1:-install}" in
        install) ;;
        uninstall) uninstall; exit 0 ;;
        *) die "usage: install.sh [install|uninstall]" ;;
    esac
    preflight
    TMP_DIR=$(mktemp -d)
    install_packages
    install_containerd
    setup_thinpool
    install_firecracker
    setup_network
    install_flintlock
    install_registry_mirror
    install_cache_server
    start_services
    apply_vm_disk_size
    install_firerunner
    systemctl daemon-reload
    systemctl enable -q firerunner
    if ! systemctl is-active -q firerunner || changed /etc/systemd/system/firerunner.service "$BIN_DIR/firerunner"; then
        systemctl restart firerunner
    fi
    open_metrics_port
    verify_install
    register_runner
    configure_runner_cache
}

main "$@"
