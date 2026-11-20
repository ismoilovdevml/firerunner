#!/usr/bin/env bash
# FireRunner host installer.
#
# Prepares a KVM host (bare metal or a VM with nested virtualization) to run
# GitLab CI jobs in Firecracker microVMs:
#   containerd (devmapper thin pool) + Firecracker + flintlockd + bridge/DHCP/NAT/firewall
#   + Docker Hub pull-through mirror + S3 store for cache: + firerunner + gitlab-runner
#
# Usage:
#   curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
#   curl -sfL .../install.sh | sudo FR_DISK=/dev/sdb bash      # choose the thin-pool disk
#   curl -sfL .../install.sh | sudo bash -s -- uninstall        # remove services (keeps data)
#
# Settings (environment variables):
#   FR_DISK                 empty block device for the thin pool, wiped (default: the blank disk,
#                           if there is exactly one; never a disk in use, see thinpool_disk)
#   FR_BRIDGE               bridge for microVM taps          (default: br-fc)
#   FR_SUBNET               /24 prefix for microVMs           (default: 10.200.0)
#   FR_VM_DISK              root disk of every microVM, thin-provisioned (default: 40GB)
#   FR_THIN_CHUNK           thin pool chunk size, only for a new pool (default: 64K). Every
#                           first write to a chunk of a VM's disk copies or zeroes the whole
#                           chunk: a larger one costs more per small write, less pool metadata
#   CONTAINERD_VERSION      (default: 1.7.35)
#   FIRECRACKER_VERSION     (default: 1.17.0)
#   FLINTLOCK_VERSION       (default: 0.15.2)
#
#   FR_VERSION              firerunner release to install: edge (latest main), latest (newest
#                           stable release) or a tag like v1.2.0 (default: edge)
#   FR_ALLOW_UNSIGNED=1     install a release without a signature (published before releases were
#                           signed); a signature that does not verify is never accepted
#   FR_POOL_SIZE            pre-booted microVMs kept ready (default: FR_RUNNER_CONCURRENT, so a
#                           burst of that many jobs starts without a boot)
#   FR_VM_VCPU              vCPUs of each job microVM (default: host CPUs / FR_RUNNER_CONCURRENT,
#                           at least 2, at most 16)
#   FR_METRICS_ALLOW        source IPv4/CIDR allowed to scrape :9477/metrics (default: none, localhost only;
#                           remembered for later re-runs)
#   FR_EGRESS_DENY          comma-separated IPv4 CIDRs jobs must not reach, e.g. 192.168.0.0/16,
#                           directly or through the proxy (default: none; remembered for later
#                           re-runs, FR_EGRESS_DENY=none clears it; link-local is always blocked)
#   REGISTRY_VERSION        Docker Hub pull-through mirror for microVMs (default: 3.1.1)
#   FR_REGISTRY_MIRRORS     comma-separated registries mirrored on the host besides Docker Hub
#                           (default: ghcr.io,quay.io,registry.k8s.io,mcr.microsoft.com; remembered
#                           for later re-runs, none for no others). A pull the mirror cannot serve
#                           (a private image) goes to the registry itself
#   FR_REGISTRY_GB          most the registry mirrors may store, in a file system of its own on the
#                           root disk (default: auto, a fifth of the root file system, at least 5).
#                           Only a larger value changes an existing one (grown online)
#   VERSITYGW_VERSION      S3 server for the runner's cache: (default: 1.8.0)
#   FR_CACHE_DAYS           delete cache: archives not written for this many days (default: 14)
#   FR_CACHE_MAX_GB         above this size the oldest cache: archives are deleted, checked hourly
#                           (default: auto, a fifth of the file system the cache is on)
#   GITLAB_RUNNER_VERSION   (default: 19.4.0)
#
# Corporate networks (all remembered for later re-runs):
#   FR_PROXY                corporate HTTP proxy, http://host:port. With a password, write
#                           http://user:password@host:port into /etc/firerunner/proxy-upstream
#                           (root only, 0600) before running the installer instead: a value here
#                           is visible in the process list while the installer runs. microVMs
#                           and host services use a local forwarder and never see the password.
#   FR_NO_PROXY             comma-separated hosts, .domains and CIDRs reached without the proxy,
#                           e.g. gitlab.corp.example,.corp.example,10.0.0.0/8
#   FR_CA_FILE              PEM file with a company root CA (internal registries, S3, or a
#                           TLS-inspecting proxy): trusted by the host, microVMs and job containers
#   FR_INSECURE_REGISTRIES  comma-separated registries without a checkable certificate (host:port)
#                           or on plain HTTP (http://host:port)
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
FR_CACHE_MAX_GB="${FR_CACHE_MAX_GB:-auto}"
GITLAB_RUNNER_VERSION="${GITLAB_RUNNER_VERSION:-19.4.0}"
FR_VERSION="${FR_VERSION:-edge}"
FR_BINARY="${FR_BINARY:-}"            # local firerunner binary instead of a release (development)
FR_ALLOW_UNSIGNED="${FR_ALLOW_UNSIGNED:-}"
# The key firerunner releases are signed with (internal/upgrade/release-signing.pub):
# checksums.txt.sig is an Ed25519 signature of checksums.txt.
RELEASE_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEApi5crvwgIWd7zeolIaclDHYnEcLcM9xidviuvmeV8PA=
-----END PUBLIC KEY-----'

FR_DISK="${FR_DISK:-}"
FR_BRIDGE="${FR_BRIDGE:-br-fc}"
FR_SUBNET="${FR_SUBNET:-10.200.0}"
FR_VM_DISK="${FR_VM_DISK:-40GB}"
FR_THIN_CHUNK="${FR_THIN_CHUNK:-64K}"

FR_GITLAB_URL="${FR_GITLAB_URL:-}"
FR_RUNNER_TOKEN="${FR_RUNNER_TOKEN:-}"
FR_RUNNER_CONCURRENT="${FR_RUNNER_CONCURRENT:-4}"
# Set on the command line: applied to an existing config too (else only to a new one).
FR_POOL_SIZE_GIVEN="${FR_POOL_SIZE:+1}"
FR_POOL_SIZE="${FR_POOL_SIZE:-$FR_RUNNER_CONCURRENT}"
FR_VM_VCPU_GIVEN="${FR_VM_VCPU:+1}"
FR_VM_VCPU="${FR_VM_VCPU:-}"
FR_METRICS_ALLOW="${FR_METRICS_ALLOW:-}"
FR_EGRESS_DENY="${FR_EGRESS_DENY:-}"
FR_PROXY="${FR_PROXY:-}"
FR_NO_PROXY="${FR_NO_PROXY:-}"
FR_CA_FILE="${FR_CA_FILE:-}"
FR_INSECURE_REGISTRIES="${FR_INSECURE_REGISTRIES:-}"
FR_REGISTRY_MIRRORS="${FR_REGISTRY_MIRRORS:-}"
FR_REGISTRY_GB="${FR_REGISTRY_GB:-auto}"
PROXY_PORT=3128                       # the forwarder (proxy.listen); opened on the bridge
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
FLINTLOCK_TLS_DIR=$CONF_DIR/flintlock-tls  # CA, flintlockd's and firerunner's certificates
FLINTLOCK_TLS_LATER=""                      # why flintlockd's TLS waits for another run
FLINTLOCK_TLS_WAIT=20                       # seconds firerunner gets to reach flintlockd over TLS
FLINTLOCK_TLS_SWITCHING=0                   # inside flintlock_tls_switch (the EXIT trap finishes it)
FLINTLOCK_TLS_MARKER=/var/lib/firerunner/flintlock-tls-switching  # the same, for a run killed outright
FLINTLOCK_TLS_RESET=0                       # flintlockd and firerunner restart now, jobs or not
FLINTLOCK_TLS_DAEMON=0                      # firerunner's daemon restarts (it may hold the old setting)

TMP_DIR=""
BIN_TMP=""   # a binary being installed (install_binary), removed if the run ends first

log()  { printf '[firerunner] %s\n' "$*"; }
warn() { printf '[firerunner] WARNING: %s\n' "$*" >&2; }
die()  { printf '[firerunner] ERROR: %s\n' "$*" >&2; exit 1; }

cleanup() {
    # Leaving: a second Ctrl-C must not cut the put-back below short.
    trap '' INT HUP TERM
    # A run cut off while switching flintlockd to TLS puts it back first.
    [[ $FLINTLOCK_TLS_SWITCHING == 1 ]] && flintlock_tls_revert
    [[ -n "$TMP_DIR" ]] && rm -rf "$TMP_DIR"
    [[ -n "$BIN_TMP" ]] && rm -f "$BIN_TMP"
    return 0
}
trap cleanup EXIT
# Ctrl-C, a closed terminal or a kill end the run through the EXIT trap too.
trap 'exit 130' INT HUP TERM
# set -e exits silently; say where and why.
trap 'printf "[firerunner] ERROR: line %s: %s (exit %s)\n" "$LINENO" "$BASH_COMMAND" "$?" >&2' ERR

# --------------------------------------------------------------------------
# Preflight
# --------------------------------------------------------------------------

preflight() {
    [[ $EUID -eq 0 ]] || die "run as root (curl ... | sudo bash)"
    command -v systemctl >/dev/null || die "systemd is required"
    preflight_settings

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

# default_vcpu: the host's CPUs shared by the jobs that run at once (a shell
# executor's job gets every CPU of the host), at least 2 and at most 16. vCPUs
# are threads on the host: an idle one costs nothing, so they are not reserved.
default_vcpu() {
    local n=$(( $(nproc) / FR_RUNNER_CONCURRENT ))
    ((n < 2)) && n=2
    ((n > 16)) && n=16
    echo "$n"
}

# preflight_settings checks the FR_* settings that end up in firewall rules,
# systemd units and the microVM config, before anything is changed.
preflight_settings() {
    [[ $FR_CACHE_DAYS =~ ^[0-9]{1,4}$ ]] || die "FR_CACHE_DAYS must be a whole number of days"
    [[ $FR_CACHE_MAX_GB == auto || $FR_CACHE_MAX_GB =~ ^[1-9][0-9]{0,5}$ ]] || die "FR_CACHE_MAX_GB must be auto or a whole number of GB from 1"
    [[ $FR_REGISTRY_GB == auto ]] || { [[ $FR_REGISTRY_GB =~ ^[1-9][0-9]{0,5}$ ]] && ((FR_REGISTRY_GB >= REGISTRY_MIN_GB)); } ||
        die "FR_REGISTRY_GB must be auto or a whole number of GB from ${REGISTRY_MIN_GB}"
    [[ $FR_THIN_CHUNK =~ ^(64|128|256|512|1024)[Kk]$ ]] || die "FR_THIN_CHUNK must be 64K, 128K, 256K, 512K or 1024K"
    [[ $FR_RUNNER_CONCURRENT =~ ^[1-9][0-9]{0,2}$ ]] || die "FR_RUNNER_CONCURRENT must be a whole number from 1"
    [[ -n $FR_POOL_SIZE_GIVEN ]] || ((FR_POOL_SIZE <= 32)) || FR_POOL_SIZE=32
    [[ $FR_POOL_SIZE =~ ^[0-9]+$ ]] && ((FR_POOL_SIZE <= 32)) || die "FR_POOL_SIZE must be a whole number from 0 to 32"
    [[ -n $FR_VM_VCPU ]] || FR_VM_VCPU=$(default_vcpu)
    [[ $FR_VM_VCPU =~ ^[0-9]+$ ]] && ((FR_VM_VCPU >= 1 && FR_VM_VCPU <= 32)) || die "FR_VM_VCPU must be a whole number from 1 to 32"
    if [[ -n $FR_METRICS_ALLOW ]]; then
        is_ipv4_cidr "$FR_METRICS_ALLOW" || die "FR_METRICS_ALLOW=$FR_METRICS_ALLOW is not an IPv4 address or CIDR"
    fi
    local cidr
    if [[ $FR_EGRESS_DENY != none ]]; then
        for cidr in ${FR_EGRESS_DENY//,/ }; do
            is_ipv4_cidr "$cidr" || die "FR_EGRESS_DENY: $cidr is not an IPv4 address or CIDR"
        done
    fi
    # user and password: unreserved characters and %XX only, as the forwarder parses them.
    local ui='([A-Za-z0-9._~!$&()*+,;=-]|%[0-9A-Fa-f]{2})'
    local proxy_re="^http://(${ui}+(:${ui}*)?@)?([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?):([0-9]{1,5})/?\$"
    if [[ -n $FR_PROXY ]]; then
        [[ $FR_PROXY =~ $proxy_re ]] ||
            die "FR_PROXY must be http://[user:password@]host:port (%-encode other characters in the password)"
        local phost=${BASH_REMATCH[5]} pport=${BASH_REMATCH[7]}
        if [[ $pport == "$PROXY_PORT" && $phost =~ ^(localhost|127\.[0-9.]+|0\.0\.0\.0|${FR_SUBNET//./\\.}\.1)$ ]]; then
            die "FR_PROXY points at the FireRunner forwarder itself (port $PROXY_PORT): give a local proxy (CNTLM, px) another port"
        fi
    fi
    if [[ -n $FR_NO_PROXY && ! $FR_NO_PROXY =~ ^[A-Za-z0-9.,:/*_-]+$ ]]; then
        die "FR_NO_PROXY: only hosts, .domains and CIDRs, comma-separated"
    fi
    if [[ -n $FR_CA_FILE && $FR_CA_FILE != none ]]; then
        grep -q -- "-----BEGIN CERTIFICATE-----" "$FR_CA_FILE" 2>/dev/null ||
            die "FR_CA_FILE=$FR_CA_FILE is not a PEM file with a certificate"
    fi
    [[ -n $FR_REGISTRY_MIRRORS ]] || FR_REGISTRY_MIRRORS=$(cat "$MIRRORS_FILE" 2>/dev/null || echo "$DEFAULT_REGISTRY_MIRRORS")
    local host
    for host in $(mirror_hosts); do
        [[ $host =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && $host == *.* && $host != docker.io ]] ||
            die "FR_REGISTRY_MIRRORS: $host is not a registry host name (Docker Hub has its own mirror)"
    done
    if [[ -n $FR_INSECURE_REGISTRIES && ! $FR_INSECURE_REGISTRIES =~ ^(http://)?[A-Za-z0-9.:-]+(,(http://)?[A-Za-z0-9.:-]+)*$ ]]; then
        die "FR_INSECURE_REGISTRIES: host:port or http://host:port entries, comma-separated"
    fi
    return 0
}

# --------------------------------------------------------------------------
# Corporate proxy and company CA
# --------------------------------------------------------------------------

PROXY_FILE="$CONF_DIR/proxy-upstream"
CA_FILE="$CONF_DIR/ca.pem"
EGRESS_FILE="$CONF_DIR/egress-deny"

# jobs_running: microVM jobs are in progress on this host.
jobs_running() { compgen -G "/run/firerunner/jobs/*.json" >/dev/null; }

# drained: `firerunner drain` stopped (or is stopping) gitlab-runner on
# purpose; only `firerunner undrain` starts it again. The installer never
# starts or restarts it then: its next start picks up what changed.
DRAIN_FILE=/run/firerunner/drain.json
drained() { [[ -f $DRAIN_FILE ]]; }

# restart_when_idle SERVICE: restart now or, while jobs run, remember it in
# PENDING_FILE, so a later run of the installer when idle does it
# (containerd, flintlockd, the registry mirror and gitlab-runner serve jobs).
PENDING_FILE=/var/lib/firerunner/pending-restarts
PENDING=""
restart_when_idle() {
    if [[ $1 == gitlab-runner ]] && drained; then
        log "  gitlab-runner not restarted: the host is drained (firerunner undrain starts it with the change)"
        [[ -f $PENDING_FILE ]] && sed -i "/^restart $1\$/d" "$PENDING_FILE"
        return 0
    fi
    if systemctl is-active -q "$1" && jobs_running; then
        warn "  $1 not restarted: jobs are running (re-run the installer when the runner is idle)"
        PENDING+="$1 "
        mkdir -p "$(dirname "$PENDING_FILE")"
        grep -qx "restart $1" "$PENDING_FILE" 2>/dev/null || echo "restart $1" >> "$PENDING_FILE"
        return 0
    fi
    log "  restarting $1"
    systemctl restart "$1"
    [[ -f $PENDING_FILE ]] && sed -i "/^restart $1\$/d" "$PENDING_FILE"
    return 0
}

# apply_pending runs, when no job runs, what earlier runs had to leave:
# restarts, and stopping the forwarder once nothing uses it.
apply_pending() {
    [[ -s $PENDING_FILE ]] || return 0
    if jobs_running; then
        warn "still pending while jobs run: $(tr '\n' ' ' <"$PENDING_FILE")- re-run the installer when the runner is idle"
        return 0
    fi
    local action svc
    while read -r action svc; do
        if [[ $svc == gitlab-runner ]] && drained; then
            log "  gitlab-runner not restarted (pending): the host is drained (firerunner undrain starts it with the change)"
            continue
        fi
        case $action in
            restart) log "  restarting $svc (pending)"; systemctl restart "$svc" ;;
            stop)    log "  stopping $svc (pending)"; systemctl disable --now -q "$svc" ;;
        esac
    done <"$PENDING_FILE"
    rm -f "$PENDING_FILE"
}

# The networks jobs must not reach. Remembered, so a re-run without
# FR_EGRESS_DENY keeps them; FR_EGRESS_DENY=none clears them.
egress_deny() {
    if [[ $FR_EGRESS_DENY == none ]]; then
        return 0
    elif [[ -n $FR_EGRESS_DENY ]]; then
        echo "$FR_EGRESS_DENY"
    elif [[ -s $EGRESS_FILE ]]; then
        cat "$EGRESS_FILE"
    fi
}

# proxy_on: a corporate proxy is configured (now or by an earlier run).
proxy_on() { [[ -s $PROXY_FILE ]]; }

# setup_proxy runs first: every later download may need the proxy and the CA.
setup_proxy() {
    mkdir -p "$CONF_DIR"
    if [[ $FR_CA_FILE == none ]]; then
        log "dropping the company CA"
        rm -f "$CA_FILE"
        if [[ -f /etc/pki/ca-trust/source/anchors/firerunner-ca.pem ]]; then
            rm -f /etc/pki/ca-trust/source/anchors/firerunner-ca.pem && update-ca-trust
        fi
        if [[ -f /usr/local/share/ca-certificates/firerunner-ca.crt ]]; then
            rm -f /usr/local/share/ca-certificates/firerunner-ca.crt && update-ca-certificates >/dev/null
        fi
    elif [[ -n $FR_CA_FILE ]]; then
        log "trusting the company CA from $FR_CA_FILE"
        put "$CA_FILE" 0644 <"$FR_CA_FILE"
    fi
    if [[ -f $CA_FILE ]]; then
        # The host's own clients (curl, dnf/apt, containerd, the registry mirror) trust it too.
        if [[ -d /etc/pki/ca-trust/source/anchors ]]; then
            put /etc/pki/ca-trust/source/anchors/firerunner-ca.pem 0644 <"$CA_FILE"
            changed /etc/pki/ca-trust/source/anchors/firerunner-ca.pem && update-ca-trust
        elif [[ -d /usr/local/share/ca-certificates ]]; then
            put /usr/local/share/ca-certificates/firerunner-ca.crt 0644 <"$CA_FILE"
            changed /usr/local/share/ca-certificates/firerunner-ca.crt && update-ca-certificates >/dev/null
        fi
    fi
    if [[ -n $FR_PROXY ]]; then
        log "using the corporate proxy (stored in $PROXY_FILE, root only)"
        (umask 077; printf '%s\n' "${FR_PROXY%/}" | put "$PROXY_FILE" 0600)
    elif proxy_on; then
        chmod 0600 "$PROXY_FILE"
        log "using the corporate proxy from $PROXY_FILE"
    fi
    if [[ $FR_EGRESS_DENY == none ]]; then
        rm -f "$EGRESS_FILE"
    elif [[ -n $FR_EGRESS_DENY ]]; then
        echo "$FR_EGRESS_DENY" | put "$EGRESS_FILE" 0644
    elif [[ ! -f $EGRESS_FILE && -f $LIB_DIR/net-up.sh ]]; then
        # Given to a run from before the list was remembered: keep it.
        local old
        old=$(sed -n 's/.*ip daddr { \(.*\) } drop$/\1/p' "$LIB_DIR/net-up.sh" | head -1 | tr -d ' ')
        [[ -n $old ]] && echo "$old" | put "$EGRESS_FILE" 0644
    fi
    proxy_on || return 0
    # The installer's own downloads go straight to the corporate proxy.
    local upstream no
    upstream=$(<"$PROXY_FILE")
    no="localhost,127.0.0.1,${FR_SUBNET}.1${FR_NO_PROXY:+,$FR_NO_PROXY}"
    export http_proxy="$upstream" https_proxy="$upstream" no_proxy="$no"
    export HTTP_PROXY="$upstream" HTTPS_PROXY="$upstream" NO_PROXY="$no"
}

# proxy_dropin SERVICE: host services reach the internet through the local
# forwarder (the password stays in one root-only file).
proxy_dropin() {
    local d="/etc/systemd/system/$1.service.d/firerunner-proxy.conf"
    if ! proxy_on; then
        [[ -f $d ]] && { rm -f "$d"; CHANGED+="$d "; }
        return 0
    fi
    local p="http://127.0.0.1:${PROXY_PORT}" no
    no="localhost,127.0.0.1,::1,${FR_SUBNET}.1,${FR_SUBNET}.0/24$(no_proxy_extra)"
    put "$d" 0644 <<EOF
[Service]
Environment="HTTP_PROXY=$p" "HTTPS_PROXY=$p" "NO_PROXY=$no" "http_proxy=$p" "https_proxy=$p" "no_proxy=$no"
EOF
}

# no_proxy_extra: ",<proxy.no_proxy>" as configured now or by an earlier run.
no_proxy_extra() {
    local extra=$FR_NO_PROXY
    if [[ -z $extra && -x $BIN_DIR/firerunner ]]; then
        extra=$($BIN_DIR/firerunner config get proxy.no_proxy 2>/dev/null | tr -d "\"' " || true)
    fi
    [[ -n $extra ]] && printf ',%s' "$extra"
    return 0
}

proxy_input_rules() {
    proxy_on || return 0
    printf '    iifname "%s" ip daddr %s.1 tcp dport %s accept\n' "$FR_BRIDGE" "$FR_SUBNET" "$PROXY_PORT"
}

# configure_proxy writes the proxy, CA and registry settings into the
# firerunner config and runs the forwarder (after firerunner is installed).
configure_proxy() {
    local fr=$BIN_DIR/firerunner
    if [[ -f $CA_FILE ]]; then
        $fr config set vm.ca_file "$CA_FILE" >/dev/null
    elif [[ $FR_CA_FILE == none ]]; then
        $fr config set vm.ca_file '""' >/dev/null
    fi
    # Written every run: the ports are the ones this run's mirrors listen on.
    $fr config set vm.registry_mirrors "$(mirrors_json)" >/dev/null
    if [[ -n $FR_INSECURE_REGISTRIES ]]; then
        $fr config set vm.insecure_registries "[${FR_INSECURE_REGISTRIES}]" >/dev/null
    fi
    if ! proxy_on; then
        # Only the forwarder reads network.egress_deny (the firewall has the list
        # itself): without it the key stays out, and v0.1.0 can still read the config.
        $fr config set network.egress_deny "[]" >/dev/null
        if systemctl is-enabled -q firerunner-proxy 2>/dev/null; then
            $fr config set proxy.enabled false >/dev/null
            STOP_FORWARDER=1 # after the services that use it are restarted (main)
        fi
        return 0
    fi
    # The forwarder refuses these too: proxied traffic leaves from the host.
    $fr config set network.egress_deny "[$(egress_deny)]" >/dev/null
    $fr config set proxy.listen "${FR_SUBNET}.1:${PROXY_PORT}" >/dev/null
    $fr config set proxy.upstream_file "$PROXY_FILE" >/dev/null
    if [[ -n $FR_NO_PROXY ]]; then
        $fr config set proxy.no_proxy "$FR_NO_PROXY" >/dev/null
    fi
    $fr config set proxy.enabled true >/dev/null
    put /etc/systemd/system/firerunner-proxy.service <<EOF
[Unit]
Description=FireRunner proxy forwarder (microVMs and host services to the corporate proxy)
Wants=firerunner-net.service
After=firerunner-net.service

[Service]
ExecStart=${BIN_DIR}/firerunner proxy
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictSUIDSGID=yes
LockPersonality=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable -q firerunner-proxy
    if ! systemctl is-active -q firerunner-proxy; then
        systemctl restart firerunner-proxy
    elif changed /etc/systemd/system/firerunner-proxy.service "$BIN_DIR/firerunner"; then
        # A restart cuts the open tunnels (clones, pulls) of running jobs.
        restart_when_idle firerunner-proxy
    fi
}

# stop_forwarder disables the forwarder once no host service still points at
# it; while one's restart is pending, the stop waits in PENDING_FILE too.
STOP_FORWARDER=0
stop_forwarder() {
    if grep -qE '^restart (containerd-flintlock|firerunner-registry(@[^ ]+)?|gitlab-runner)$' "$PENDING_FILE" 2>/dev/null; then
        grep -qx "stop firerunner-proxy" "$PENDING_FILE" || echo "stop firerunner-proxy" >> "$PENDING_FILE"
        PENDING+="firerunner-proxy(stop) "
        return 0
    fi
    log "  stopping firerunner-proxy"
    systemctl disable --now -q firerunner-proxy
}

# proxy_dropins_apply writes or removes the host services' proxy drop-ins and
# restarts the services whose drop-in changed (later, while jobs run).
# firerunner-registry@ is the template of every registry mirror: its drop-in
# applies to all of them, and restarting firerunner-registry restarts them all
# (PartOf=). The mirrors' fronts only talk to 127.0.0.1 and need none.
proxy_dropins_apply() {
    local svc
    # Until its mirrors move to firerunner-registry@ (setup_registries), the
    # single service of earlier versions keeps its own drop-in.
    if [[ $REGISTRY_DEFERRED == 1 ]]; then
        proxy_dropin firerunner-registry
    fi
    for svc in containerd-flintlock firerunner-registry@ gitlab-runner; do
        proxy_dropin "$svc"
    done
    systemctl daemon-reload
    for svc in containerd-flintlock firerunner-registry@ gitlab-runner; do
        [[ $svc == firerunner-registry@ && $REGISTRY_DEFERRED == 1 ]] && continue
        if changed "/etc/systemd/system/$svc.service.d/firerunner-proxy.conf" && systemctl is-active -q "${svc%@}"; then
            restart_when_idle "${svc%@}"
        fi
    done
    if [[ $REGISTRY_DEFERRED == 1 ]] && changed /etc/systemd/system/firerunner-registry.service.d/firerunner-proxy.conf &&
        systemctl is-active -q firerunner-registry; then
        restart_when_idle firerunner-registry
    fi
}

install_packages() {
    log "installing packages"
    if [[ $PKG == apt ]]; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq curl tar openssl lvm2 thin-provisioning-tools dnsmasq-base dnsmasq-utils nftables iproute2 openssh-client e2fsprogs >/dev/null
    else
        dnf install -y -q curl tar openssl lvm2 device-mapper-persistent-data dnsmasq dnsmasq-utils nftables iproute openssh-clients e2fsprogs >/dev/null
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

# SHA256 of the default versions' downloads, by "<release tag>/<file>". Pinned
# here, a file changed on the download host is refused even when its checksum
# file was changed with it. Copied on 2026-11-20 from each release's own
# checksum file (named below); the GitHub ones also match the digest GitHub
# records for the asset (api.github.com .../releases/tags/<tag>, "digest").
# Another version (CONTAINERD_VERSION=... etc.) has no pin and is checked
# against its release's checksum file, from the same host as the file.
declare -A PINNED_SHA256=(
    # containerd-1.7.35-linux-amd64.tar.gz.sha256sum
    [v1.7.35/containerd-1.7.35-linux-amd64.tar.gz]=5adff3dba002f0aa0eec823d2340b695f024736503043b9ebc5d0aa1ebbf8c49
    # firecracker-v1.17.0-x86_64.tgz.sha256.txt
    [v1.17.0/firecracker-v1.17.0-x86_64.tgz]=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
    # flintlock checksums.txt
    [v0.15.2/flintlockd_amd64]=d08a4bd8eef185d948dd35d2857c0a3bb8db35195f88d2aa437e1e8667b0a1a8
    # registry_3.1.1_linux_amd64.tar.gz.sha256
    [v3.1.1/registry_3.1.1_linux_amd64.tar.gz]=6f330a3ba9ea1d23a6ee189f449d792595240585bb2f159123d76ac594f70dd8
    # versitygw checksums.txt
    [v1.8.0/versitygw_v1.8.0_Linux_x86_64.tar.gz]=2ba2c734d10d2c4e651d03182cb4b246656bc735a2f282db7b0b73fba6073467
    # gitlab-runner-downloads.s3.amazonaws.com/v19.4.0/release.sha256 (not on GitHub: no second source)
    [v19.4.0/binaries/gitlab-runner-linux-amd64]=aeebda6435d006622f135e4b095450446e1077d8980227a44b53fc404fc22157
)

# pinned TAG/FILE: prints the pinned SHA256 of FILE, or fails when it has none.
pinned() {
    [[ -n ${PINNED_SHA256[$1]:-} ]] || { log "  no pinned checksum for $1: using the release's checksum file" >&2; return 1; }
    echo "${PINNED_SHA256[$1]}"
}

# verify_release SUMS URL: SUMS (a firerunner release's checksums.txt) carries
# the release signature, downloaded from URL. Only the releases published
# before signing may lack one, and only with FR_ALLOW_UNSIGNED=1.
verify_release() {
    local sums=$1 url=$2 code out
    code=$(curl -sSL --retry 3 --retry-delay 2 -o "$sums.sig" -w '%{http_code}' "$url") || die "download failed: $url"
    case $code in
        200) ;;
        404)
            case $FR_VERSION in
                v0.1.0|v0.1.1) ;;
                *) die "firerunner ${FR_VERSION} has no signature (checksums.txt.sig): it is being published (try again in a minute) or was tampered with; not installing it" ;;
            esac
            [[ $FR_ALLOW_UNSIGNED == 1 ]] ||
                die "firerunner ${FR_VERSION} was published before releases were signed; FR_ALLOW_UNSIGNED=1 installs it anyway"
            warn "firerunner ${FR_VERSION} is not signed; installing it because FR_ALLOW_UNSIGNED=1"
            return 0 ;;
        *) die "download failed: $url (HTTP $code)" ;;
    esac
    openssl pkeyutl -help 2>&1 | grep -q -- -rawin ||
        die "checking the release signature needs OpenSSL 3 (openssl pkeyutl -rawin)"
    printf '%s\n' "$RELEASE_KEY" >"$sums.key"
    out=$(openssl pkeyutl -verify -pubin -inkey "$sums.key" -rawin -in "$sums" -sigfile "$sums.sig" 2>&1) ||
        die "checksums.txt of firerunner ${FR_VERSION} does not match its signature, not installing it (${out//$'\n'/ }); while a release is being published this can happen for a minute: try again"
}

# release_matches VERSION: the downloaded firerunner says it is FR_VERSION, so
# an older signed release served in its place is not installed.
release_matches() {
    case $FR_VERSION in
        edge)   [[ $1 == edge-* ]] ;;
        latest) [[ $1 == v* ]] ;;
        *)      [[ $1 == "$FR_VERSION" ]] ;;
    esac
}

# release_base: where FR_VERSION's files are; latest is the newest stable
# release, as for `firerunner upgrade --version latest` (internal/upgrade).
release_base() {
    if [[ $FR_VERSION == latest ]]; then
        echo "https://github.com/${FR_REPO}/releases/latest/download"
    else
        echo "https://github.com/${FR_REPO}/releases/download/${FR_VERSION}"
    fi
}

# version_compare A B: prints -1, 0 or 1 as A is older than, the same as or
# newer than B, for versions vMAJOR[.MINOR[.PATCH]][-PRE][+BUILD] (SemVer
# order: a pre-release is older than its release, its dot-separated
# identifiers compare as numbers when both are numeric, numbers before text,
# and a shorter list of equal identifiers first). Fails for anything else
# (edge-<sha>, dev): those are not compared.
version_compare() {
    local LC_ALL=C re='^v([0-9]{1,15})(\.([0-9]{1,15}))?(\.([0-9]{1,15}))?(-([0-9A-Za-z.-]+))?(\+[0-9A-Za-z.-]+)?$'
    local an bn ap bp i x y
    [[ $1 =~ $re ]] || return 1
    an=("${BASH_REMATCH[1]}" "${BASH_REMATCH[3]:-0}" "${BASH_REMATCH[5]:-0}") ap=${BASH_REMATCH[7]}
    [[ $2 =~ $re ]] || return 1
    bn=("${BASH_REMATCH[1]}" "${BASH_REMATCH[3]:-0}" "${BASH_REMATCH[5]:-0}") bp=${BASH_REMATCH[7]}
    for i in 0 1 2; do
        if ((10#${an[i]} != 10#${bn[i]})); then
            ((10#${an[i]} < 10#${bn[i]})) && echo -1 || echo 1
            return 0
        fi
    done
    if [[ $ap == "$bp" ]]; then echo 0; return 0; fi
    [[ -z $ap ]] && { echo 1; return 0; }
    [[ -z $bp ]] && { echo -1; return 0; }
    local -a aid bid
    IFS=. read -ra aid <<<"$ap"
    IFS=. read -ra bid <<<"$bp"
    for ((i = 0; i < ${#aid[@]} && i < ${#bid[@]}; i++)); do
        x=${aid[i]} y=${bid[i]}
        [[ $x == "$y" ]] && continue
        if [[ $x =~ ^[0-9]{1,15}$ && $y =~ ^[0-9]{1,15}$ ]]; then
            ((10#$x < 10#$y)) && echo -1 || echo 1
        elif [[ $x =~ ^[0-9]+$ ]]; then
            echo -1
        elif [[ $y =~ ^[0-9]+$ ]]; then
            echo 1
        elif [[ $x < "$y" ]]; then
            echo -1
        else
            echo 1
        fi
        return 0
    done
    ((${#aid[@]} < ${#bid[@]})) && echo -1 || echo 1
}

# refuse_downgrade NEW: FR_VERSION=latest moves a host forward only, as
# `firerunner upgrade --version latest` does: a release older than the
# installed one (a backport GitHub marks latest) may not know its config.
# It is installed only when asked for by its tag. edge and dev installs are
# not compared: leaving edge for the stable line is a choice.
refuse_downgrade() {
    [[ $FR_VERSION == latest && -x $BIN_DIR/firerunner ]] || return 0
    local cur
    cur=$("$BIN_DIR/firerunner" version 2>/dev/null | awk '{print $2}' || true)
    if [[ $(version_compare "$1" "$cur" 2>/dev/null) == -1 ]]; then
        die "the latest release is firerunner $1, older than the installed $cur; not installing it (FR_VERSION=$1 installs it anyway)"
    fi
    return 0
}

# release_runs FILE: the downloaded firerunner runs, is FR_VERSION and, for
# latest, is not older than the installed one.
release_runs() {
    local got
    got=$("$1" version 2>/dev/null | awk '{print $2}' || true)
    release_matches "$got" || die "firerunner ${FR_VERSION} contains firerunner ${got:-(does not run)}; not installing it"
    refuse_downgrade "$got"
}

# install_binary SRC DEST [CHECK]: DEST becomes a copy of SRC, mode 0755, in
# one rename: never missing or half written, also not after a crash. The copy
# is written next to DEST (the same file system; /tmp may be noexec), and
# CHECK, given its path, may refuse it first. Recorded as changed if it differs.
install_binary() {
    local src=$1 dest=$2 check=${3:-}
    BIN_TMP=$(mktemp "$(dirname "$dest")/.$(basename "$dest")-new.XXXXXX")
    cp -- "$src" "$BIN_TMP"
    chmod 0755 "$BIN_TMP"
    [[ -z $check ]] || "$check" "$BIN_TMP"
    if [[ -f $dest ]] && cmp -s "$BIN_TMP" "$dest"; then
        rm -f "$BIN_TMP"
        BIN_TMP=""
        chmod 0755 "$dest"
        return 0
    fi
    sync -- "$BIN_TMP" 2>/dev/null || sync
    mv -f -- "$BIN_TMP" "$dest"
    BIN_TMP=""
    sync -- "$(dirname "$dest")" 2>/dev/null || true
    installed "$dest"
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
        local want
        fetch "$base/$tgz" "$TMP_DIR/$tgz"
        want=$(pinned "v${CONTAINERD_VERSION}/$tgz") || {
            fetch "$base/$tgz.sha256sum" "$TMP_DIR/$tgz.sha256sum"
            want=$(awk '{print $1}' "$TMP_DIR/$tgz.sha256sum")
        }
        verify "$TMP_DIR/$tgz" "$want"
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

[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "${THINPOOL}"
  root_path = "${CONTAINERD_ROOT}/snapshotter/devmapper"
  base_image_size = "${FR_VM_DISK}"
  # Discarding a deleted VM's blocks runs inside the snapshotter's write
  # transaction and stalls the next VM's snapshot. It is off when the thin pool
  # zeroes every chunk it hands out again (setup_thinpool), so no VM reads what
  # an earlier one wrote; on for an older pool that does not.
  discard_blocks = $(discard_blocks)
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

# discard_blocks: containerd's setting for the thin pool (see containerd.toml).
discard_blocks() {
    if lvs "$VG/thinpool" >/dev/null 2>&1 && [[ $(lvs --noheadings -o zero "$VG/thinpool" | tr -d ' ') != zero ]]; then
        echo true
    else
        echo false
    fi
}

DISK_MIN_GB=10   # a smaller disk is never taken for the thin pool on its own
SWAPS_FILE=/proc/swaps

# canonical DEV: the device node DEV is (a /dev/disk/by-id link, /dev/mapper name).
canonical() { readlink -f -- "$1" 2>/dev/null || echo "$1"; }

# disk_has_holders DEV: a device-mapper or md device sits on DEV.
disk_has_holders() { compgen -G "/sys/class/block/$(basename "$(canonical "$1")")/holders/*" >/dev/null; }

# disk_multipath_member DEV: DEV is one path of a multipath device, assembled or not.
disk_multipath_member() {
    if command -v multipath >/dev/null && multipath -c "$1" >/dev/null 2>&1; then
        return 0
    fi
    udevadm info --query=property --name="$1" 2>/dev/null | grep -qx 'DM_MULTIPATH_DEVICE_PATH=1'
}

# disk_head_zero DEV: the first MiB of DEV is zeros, as on a disk never written.
# Data without a signature (raw, Ceph, LUKS with a detached header) is not.
disk_head_zero() { cmp -s -n 1048576 "$1" /dev/zero; }

# disk_in_use DEV: prints why DEV must not be wiped, if anything on it is in
# use: mounted or swap (DEV or a partition, LV or mapping on it), an LVM
# physical volume, held by device-mapper or md, or a multipath path.
disk_in_use() {
    local disk=$1 dev mnt pv swap
    while read -r dev mnt; do
        if [[ $mnt == "[SWAP]" ]]; then
            echo "$dev is swap"
            return 0
        elif [[ -n $mnt ]]; then
            echo "$dev is mounted on $mnt"
            return 0
        fi
        while read -r swap; do
            [[ -n $swap && $(canonical "$swap") == "$(canonical "$dev")" ]] && { echo "$dev is swap"; return 0; }
        done < <(awk 'NR > 1 {print $1}' "$SWAPS_FILE" 2>/dev/null)
        while read -r pv; do
            [[ -n $pv && $(canonical "$pv") == "$(canonical "$dev")" ]] && { echo "$dev is an LVM physical volume"; return 0; }
        done < <(pvs --noheadings -o pv_name 2>/dev/null)
    done < <(lsblk -nrpo NAME,MOUNTPOINT "$disk" 2>/dev/null)
    if [[ $(lsblk -nrpo NAME "$disk" 2>/dev/null | wc -l) -gt 1 ]]; then
        echo "$disk has partitions or devices on it: $(lsblk -nrpo NAME "$disk" | tail -n +2 | paste -sd ' ' -)"
        return 0
    fi
    if disk_has_holders "$disk"; then
        echo "$disk is held by another device (/sys/class/block/$(basename "$(canonical "$disk")")/holders)"
        return 0
    fi
    if disk_multipath_member "$disk"; then
        echo "$disk is a path of a multipath device"
        return 0
    fi
    return 1
}

# blank_disks prints every whole disk that looks never used, one per line:
# not removable or read-only, at least DISK_MIN_GB, nothing on it or in use
# (disk_in_use), no signature blkid knows and a first MiB of zeros.
blank_disks() {
    local name type rm ro size dev
    while read -r name type rm ro size; do
        [[ $type == disk && $rm == 0 && $ro == 0 ]] || continue
        [[ $name == sr* || $name == loop* || $name == zram* || $name == ram* || $name == nbd* ]] && continue
        [[ $size =~ ^[0-9]+$ ]] && ((size >= DISK_MIN_GB * 1024 * 1024 * 1024)) || continue
        dev=/dev/$name
        disk_in_use "$dev" >/dev/null && continue
        [[ -z $(blkid -p "$dev" 2>/dev/null) ]] || continue
        disk_head_zero "$dev" || continue
        echo "$dev"
    done < <(lsblk -dnbro NAME,TYPE,RM,RO,SIZE)
}

# thinpool_disk sets FR_DISK, the disk the thin pool is made on, and checks
# it is safe to wipe. Without FR_DISK only the one blank disk is taken: with
# none or several, the operator chooses.
thinpool_disk() {
    if [[ -z $FR_DISK ]]; then
        local disks
        mapfile -t disks < <(blank_disks)
        case ${#disks[@]} in
            1)
                FR_DISK=${disks[0]}
                log "using blank disk $FR_DISK ($(lsblk -dno SIZE,MODEL "$FR_DISK" 2>/dev/null | tr -s ' ')) for the thin pool" ;;
            0)
                die "no blank disk found for the thin pool. A disk counts as blank when it is at least
  ${DISK_MIN_GB} GB, has no partitions, signatures or holders, is not mounted, swap, an LVM or
  multipath member, and its first MiB is zeros. Attach an empty disk (e.g. 100G+) and re-run,
  or choose one with FR_DISK=/dev/sdX (it will be wiped)." ;;
            *)
                die "found ${#disks[@]} blank disks (${disks[*]}): choose the one to wipe for the
  thin pool with FR_DISK=/dev/sdX" ;;
        esac
    fi
    [[ -b $FR_DISK ]] || die "FR_DISK=$FR_DISK is not a block device"
    local why
    if why=$(disk_in_use "$FR_DISK"); then
        die "FR_DISK=$FR_DISK is not wiped: $why. Choose another disk, or free this one first
  (unmount, swapoff, vgremove/pvremove, wipefs -a)."
    fi
    return 0
}

setup_thinpool() {
    if lvs "$VG/thinpool" >/dev/null 2>&1; then
        log "thin pool $VG/thinpool already exists"
    else
        thinpool_disk
        log "creating LVM thin pool on $FR_DISK"
        pvcreate -qy "$FR_DISK"
        vgcreate -q "$VG" "$FR_DISK"
        lvcreate -q --wipesignatures y -n thinpool "$VG" -l 95%VG
        lvcreate -q --wipesignatures y -n thinpoolmeta "$VG" -l 1%VG
        # --zero y: a chunk freed by a deleted VM may be handed to a VM of
        # another project; zeroing on first use means it never reads the old
        # data (discards do not guarantee zeros on every disk).
        # -c: a VM's first write to a chunk copies it from the image or zeroes
        # it whole. 64K chunks copy 8x less than 512K for the small scattered
        # writes of jobs (package installs, layer extraction, git checkouts);
        # their metadata still fits the 1% above (8 TB: 3.8 GiB, thin_metadata_size).
        lvconvert -qy --zero y -c "$FR_THIN_CHUNK" --thinpool "$VG/thinpool" --poolmetadata "$VG/thinpoolmeta"
    fi
    if [[ $(lvs --noheadings -o zero "$VG/thinpool" 2>/dev/null | tr -d ' ') != zero ]]; then
        warn "thin pool $VG/thinpool does not zero new chunks: a microVM may read data a deleted
  microVM left on disk. Turn it on (online, no restart): lvchange --zero y $VG/thinpool"
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
    local want
    fetch "$base/$tgz" "$TMP_DIR/$tgz"
    want=$(pinned "v${FIRECRACKER_VERSION}/$tgz") || {
        fetch "$base/$tgz.sha256.txt" "$TMP_DIR/$tgz.sha256.txt"
        want=$(awk '{print $1}' "$TMP_DIR/$tgz.sha256.txt")
    }
    verify "$TMP_DIR/$tgz" "$want"
    tar -xzf "$TMP_DIR/$tgz" -C "$TMP_DIR"
    local rel="$TMP_DIR/release-v${FIRECRACKER_VERSION}-${FC_ARCH}"
    install -m 0755 "$rel/firecracker-v${FIRECRACKER_VERSION}-${FC_ARCH}" "$BIN_DIR/firecracker"
    install -m 0755 "$rel/jailer-v${FIRECRACKER_VERSION}-${FC_ARCH}" "$BIN_DIR/jailer"
    installed "$BIN_DIR/firecracker"
}

# --------------------------------------------------------------------------
# microVM network: bridge + DHCP/DNS + NAT
# --------------------------------------------------------------------------

is_ipv4_cidr() {
    [[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})(/([0-9]{1,2}))?$ ]] || return 1
    local i
    for i in 1 2 3 4; do (( BASH_REMATCH[i] <= 255 )) || return 1; done
    [[ -z ${BASH_REMATCH[6]} ]] || (( BASH_REMATCH[6] <= 32 ))
}

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
    local deny
    deny=$(egress_deny)
    [[ -n $deny ]] || return 0
    printf '    iifname "%s" ip daddr { %s } drop\n' "$FR_BRIDGE" "${deny//,/, }"
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
# Keep what the daemon added (builder port mappings, microVM bindings) across a
# reload of these rules: they go back in the transaction that re-creates the
# tables, so no microVM is unchecked in between. One listing per table.
elements() { sed -n "s/.*\$1 {[^}]*elements = {\([^}]*\)}.*/\1/p"; }
inet=\$(nft list table inet firerunner 2>/dev/null | tr -d '\n\t' || true)
bridge=\$(nft list table bridge firerunner 2>/dev/null | tr -d '\n\t' || true)
builders=\$(elements "map builders" <<<"\$inet")
vm_taps=\$(elements "set vm_taps" <<<"\$bridge")
vm_macs=\$(elements "set vm_macs" <<<"\$bridge")
vm_addrs=\$(elements "set vm_addrs" <<<"\$bridge")
restore=""
if [[ -n \$builders ]]; then
    restore+="add element inet firerunner builders { \$builders }"\$'\n'
fi
if [[ -n \$vm_taps && -n \$vm_macs && -n \$vm_addrs ]]; then
    restore+="add element bridge firerunner vm_macs { \$vm_macs }"\$'\n'
    restore+="add element bridge firerunner vm_addrs { \$vm_addrs }"\$'\n'
    restore+="add element bridge firerunner vm_taps { \$vm_taps }"\$'\n'
fi
rules=\$(cat <<'NFT'
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
    # The services on the bridge address are for microVMs and the host only,
    # never for the uplink (a LAN host routing the microVM subnet here).
    ip daddr ${FR_SUBNET}.1 iifname != { "${FR_BRIDGE}", "lo" } drop
    # flintlock also gives every microVM a metadata tap (eth0 in the guest). It is
    # up on the host but not on the bridge, and Firecracker answers the metadata
    # address itself, so nothing that arrives on it is for the host (IPv6 link-local
    # would otherwise reach every host service listening on ::).
    iifname "fltap*" drop
    iifname "${FR_BRIDGE}" udp dport 67 accept
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 udp dport 53 accept
    iifname "${FR_BRIDGE}" ip daddr ${FR_SUBNET}.1 tcp dport { 53, $(registry_ports), 9000 } accept
$(proxy_input_rules)
    iifname "${FR_BRIDGE}" ct state established,related accept
    iifname "${FR_BRIDGE}" drop
$(metrics_input_rules)
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    # Nothing is routed from a microVM's metadata tap (see the input chain).
    iifname "fltap*" drop
    # A job VM reaches its project's builder through the bridge address (DNAT
    # below); any other traffic routed from one microVM to another is dropped.
    iifname "${FR_BRIDGE}" oifname "${FR_BRIDGE}" ct status dnat accept
    iifname "${FR_BRIDGE}" oifname "${FR_BRIDGE}" drop
    # From outside, microVMs only get replies to connections they opened.
    oifname "${FR_BRIDGE}" ct state established,related accept
    oifname "${FR_BRIDGE}" drop
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
  # A microVM sends only from its own MAC and address, so it cannot take over
  # another microVM's address on the host (ARP, the bridge's MAC table) and get
  # that microVM's traffic. The daemon adds each microVM's tap, MAC and leased
  # address once it has them; frames from a tap it did not add are not checked,
  # so a microVM whose tap is unknown keeps working.
  set vm_taps { type ifname; }
  set vm_macs { type ifname . ether_addr; }
  set vm_addrs { type ifname . ether_addr . ipv4_addr; }
  chain prerouting {
    type filter hook prerouting priority filter; policy accept;
    iifname != @vm_taps accept
    iifname . ether saddr != @vm_macs drop
    iifname . ether saddr . ip saddr @vm_addrs accept
    # DHCP sent before the VM uses its address (from 0.0.0.0), and ARP probes (RFC 5227).
    ip saddr 0.0.0.0 ip daddr 255.255.255.255 udp sport 68 udp dport 67 accept
    arp htype 1 arp ptype ip arp hlen 6 arp plen 4 iifname . arp saddr ether . arp saddr ip @vm_addrs accept
    arp htype 1 arp ptype ip arp hlen 6 arp plen 4 arp saddr ip 0.0.0.0 iifname . arp saddr ether @vm_macs accept
    # Anything else: other addresses, IPv6 (off on the bridge), VLAN tags.
    drop
  }
}
NFT
)
if ! printf '%s\n%s' "\$rules" "\$restore" | nft -f -; then
    [[ -n \$restore ]] || exit 1
    # The daemon adds them again on its next pass.
    echo "net-up.sh: could not restore the builder ports and microVM bindings; loading the rules without them" >&2
    printf '%s\n' "\$rules" | nft -f -
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
# Do not ping an address before offering it: dnsmasq stops answering DHCP for
# ~3 s per ping, which serialises boots. Only VMs on this bridge use the range.
no-ping
# Every microVM starts with an empty resolver cache: keep the answers here
# (the default holds 150 names).
cache-size=10000
# The host's resolvers answer every query at once and the first reply wins: one
# that is slow or down does not fail lookups in jobs (getaddrinfo EAI_AGAIN).
all-servers
# Never keep a negative answer: an upstream that returns a name without an
# address for a moment would otherwise fail every job's lookups of that name
# until the answer expires (seen as "No address associated with hostname" for
# deb.debian.org in builds of several projects at once).
no-negcache
# Hostnames sent by guests are not published in DNS.
dhcp-ignore-names
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
# No pid file: systemd tracks the process, and the default /run/dnsmasq.pid
# belongs to the host's own dnsmasq, if any.
ExecStart=$(command -v dnsmasq) --keep-in-foreground --pid-file --conf-file=${CONF_DIR}/dnsmasq.conf
Restart=always
RestartSec=5
# Serves untrusted microVMs. It binds 53 and 67 on the bridge as root, then
# runs as nobody and keeps NET_ADMIN for DHCP (and NET_RAW, SO_BINDTODEVICE on
# older kernels); SETUID and SETGID drop root. Only the lease file is written.
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/misc
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_ADMIN CAP_NET_RAW CAP_SETUID CAP_SETGID

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
        ufw_bridge_ports
    fi
}

# ufw_bridge_ports: ufw's own input chain drops what its rules do not allow,
# whatever the firerunner table accepts: allow the registry mirrors, the cache
# server and the forwarder on the bridge address (the ports net-up.sh opens),
# and delete the rules an earlier run added for ports no longer used (a
# dropped mirror, the proxy turned off). UFW_PORTS_FILE lists the rules this
# installer added, so no rule anyone else made is deleted.
UFW_PORTS_FILE=$CONF_DIR/ufw-bridge-ports   # "bridge address port" per rule
ufw_bridge_ports() {
    local port bridge addr want=""
    for port in $(bridge_tcp_ports); do
        ufw allow in on "${FR_BRIDGE}" to "${FR_SUBNET}.1" port "$port" proto tcp >/dev/null
        want+="${FR_BRIDGE} ${FR_SUBNET}.1 ${port}"$'\n'
    done
    if [[ -f $UFW_PORTS_FILE ]]; then
        while read -r bridge addr port; do
            [[ -n $port ]] || continue
            grep -qxF "$bridge $addr $port" <<<"$want" && continue
            log "  ufw: removing the rule for port $port on $bridge (no longer used)"
            ufw delete allow in on "$bridge" to "$addr" port "$port" proto tcp >/dev/null ||
                warn "could not delete the ufw rule for port $port on $bridge"
        done <"$UFW_PORTS_FILE"
    fi
    printf '%s' "$want" | put "$UFW_PORTS_FILE" 0644
}

# bridge_tcp_ports: the TCP ports microVMs use on the bridge address besides
# DNS: the registry mirrors, the cache server (9000) and, with a corporate
# proxy, the forwarder (proxy.listen).
bridge_tcp_ports() {
    local host
    echo 5000
    for host in $(mirror_hosts); do echo "${MIRROR_PORT[$host]}"; done
    echo 9000
    if proxy_on; then echo "$PROXY_PORT"; fi
}

# --------------------------------------------------------------------------
# flintlockd
# --------------------------------------------------------------------------

# flintlock_tls_issue NAME SUBJECT EXTENSION...: NAME.key and NAME.crt signed by
# the host's flintlock CA, made again when missing, not from that CA, not
# matching each other or expiring within 90 days.
flintlock_tls_issue() {
    local d=$FLINTLOCK_TLS_DIR name=$1 subj=$2; shift 2
    if openssl verify -CAfile "$d/ca.crt" "$d/$name.crt" >/dev/null 2>&1 &&
        openssl x509 -checkend 7776000 -noout -in "$d/$name.crt" >/dev/null 2>&1 &&
        [[ "$(openssl pkey -in "$d/$name.key" -pubout 2>/dev/null)" == "$(openssl x509 -in "$d/$name.crt" -noout -pubkey 2>/dev/null)" ]]; then
        return 0
    fi
    log "  issuing the flintlock TLS certificate $name"
    printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" "$@" >"$TMP_DIR/$name.ext"
    (umask 077 &&
        openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "$subj" \
            -keyout "$TMP_DIR/$name.key" -out "$TMP_DIR/$name.csr" 2>/dev/null &&
        openssl x509 -req -in "$TMP_DIR/$name.csr" -CA "$d/ca.crt" -CAkey "$d/ca.key" \
            -set_serial "0x$(openssl rand -hex 16)" -days 3650 -extfile "$TMP_DIR/$name.ext" \
            -out "$TMP_DIR/$name.crt" 2>/dev/null) || die "could not issue the flintlock TLS certificate $name"
    mv -f "$TMP_DIR/$name.key" "$d/$name.key"
    mv -f "$TMP_DIR/$name.crt" "$d/$name.crt"
    installed "$d/$name.crt"
}

# flintlock_tls_certs: a CA of this host's own, a certificate for flintlockd
# (127.0.0.1) and one for firerunner, the only client flintlockd accepts once
# TLS is on (flintlock_tls_switch). Certificates are valid for 10 years and
# made again by a run within 90 days of that; the CA for 20.
flintlock_tls_certs() {
    local d=$FLINTLOCK_TLS_DIR
    mkdir -p "$d"
    chmod 0700 "$d"
    if ! openssl x509 -noout -in "$d/ca.crt" >/dev/null 2>&1; then
        log "  creating the flintlock TLS certificate authority"
        (umask 077 &&
            openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7300 \
                -subj "/CN=FireRunner flintlock CA" -addext "basicConstraints=critical,CA:TRUE" \
                -addext "keyUsage=critical,keyCertSign,cRLSign" \
                -keyout "$TMP_DIR/ca.key" -out "$TMP_DIR/ca.crt" 2>/dev/null) ||
            die "could not create the flintlock TLS certificate authority"
        mv -f "$TMP_DIR/ca.key" "$d/ca.key"
        mv -f "$TMP_DIR/ca.crt" "$d/ca.crt"
        installed "$d/ca.crt"
        # Without a CA, a firerunner on TLS reaches no flintlockd already: both
        # restart with the new one now, whether jobs run or not.
        if firerunner_uses_tls; then
            FLINTLOCK_TLS_RESET=1
        fi
    elif [[ ! -s $d/ca.key ]]; then
        # The running flintlockd and firerunner trust this CA: never replaced while it works.
        warn "flintlock TLS: $d/ca.key is missing, so its certificates cannot be renewed; to start over, move $d away and re-run the installer when no job runs"
        return 0
    fi
    flintlock_tls_issue server "/CN=flintlockd" extendedKeyUsage=serverAuth "subjectAltName=IP:${FLINTLOCK_ENDPOINT%:*},DNS:localhost"
    flintlock_tls_issue client "/CN=firerunner" extendedKeyUsage=clientAuth
}

# flintlock_tls_on: flintlockd's config has TLS on.
flintlock_tls_on() { grep -qx "insecure: false" /etc/opt/flintlockd/config.yaml 2>/dev/null; }

# firerunner_uses_tls: firerunner's config has the client certificate. Read
# from the file, not through firerunner: a binary that is missing, older or
# refuses the config must not make it look like "no TLS".
firerunner_uses_tls() {
    grep -Eq "^[[:space:]]+tls_cert_file:[[:space:]]*\"?${FLINTLOCK_TLS_DIR}/client\.crt\"?[[:space:]]*\$" "$CONF_DIR/config.yaml" 2>/dev/null
}

# flintlock_config TLS: flintlockd's settings; TLS=1 serves the API over mutual TLS.
flintlock_config() {
    local tls="insecure: true"
    if [[ $1 == 1 ]]; then
        tls="insecure: false
tls-cert: ${FLINTLOCK_TLS_DIR}/server.crt
tls-key: ${FLINTLOCK_TLS_DIR}/server.key
tls-client-validate: true
tls-client-ca: ${FLINTLOCK_TLS_DIR}/ca.crt"
    fi
    # flintlockd reads every flag from this file (viper); the token never appears in argv.
    put /etc/opt/flintlockd/config.yaml 0600 <<EOF
containerd-socket: ${CONTAINERD_SOCK}
grpc-endpoint: ${FLINTLOCK_ENDPOINT}
bridge-name: ${FR_BRIDGE}
firecracker-bin: ${BIN_DIR}/firecracker
basic-auth-token: $(cat "$CONF_DIR/flintlock.token")
${tls}
log-format: json
verbosity: 1
EOF
}

# flintlock_config_follow: flintlockd's config uses TLS exactly when firerunner
# does. TLS is turned on by flintlock_tls_switch.
flintlock_config_follow() {
    if firerunner_uses_tls; then flintlock_config 1; else flintlock_config 0; fi
}

# flintlock_tls_leftover: a run was killed while switching flintlockd to TLS
# (the marker is still there): which setting the two run with is unknown, so
# both restart on what firerunner's config says.
flintlock_tls_leftover() {
    [[ -e $FLINTLOCK_TLS_MARKER ]] || return 0
    rm -f /etc/opt/flintlockd/config.yaml.plain
    if [[ -x $BIN_DIR/firerunner ]] && timeout 10 "$BIN_DIR/firerunner" vm list >/dev/null 2>&1; then
        # They talk: only the daemon may still hold the setting from before.
        FLINTLOCK_TLS_DAEMON=1
        rm -f "$FLINTLOCK_TLS_MARKER"
        return 0
    fi
    warn "an earlier run was cut off while turning on flintlock TLS; restarting flintlockd and firerunner"
    FLINTLOCK_TLS_RESET=1
}

# flintlock_tls_match: after firerunner is installed, flintlockd's config must
# follow it once more (a reinstall keeps firerunner's config but not
# flintlockd's). A change restarts flintlockd at once: the two cannot talk.
flintlock_tls_match() {
    local before=0 after=0
    flintlock_tls_on && before=1
    flintlock_config_follow
    flintlock_tls_on && after=1
    if [[ $before != "$after" ]] && systemctl is-active -q flintlockd; then
        log "  restarting flintlockd: its TLS setting follows firerunner's"
        systemctl restart flintlockd
    fi
}

# flintlock_tls_switch turns TLS on for flintlockd's API and firerunner in one
# step: only when no job runs and the installed firerunner can take the keys.
# flintlockd restarts with TLS, firerunner itself must then list the microVMs
# with the client certificate, and only then gets the keys and restarts; if it
# cannot, flintlockd goes back to no TLS. The EXIT trap finishes a switch cut
# off, and a marker file lets the next run restart both after a kill or a
# power cut in between. Everywhere else flintlockd's config follows what
# firerunner uses.
flintlock_tls_switch() {
    local d=$FLINTLOCK_TLS_DIR fr=$BIN_DIR/firerunner try end
    local keys=(flintlock.tls_ca_file "$d/ca.crt" flintlock.tls_cert_file "$d/client.crt" flintlock.tls_key_file "$d/client.key")
    [[ -x $fr ]] || return 0
    firerunner_uses_tls && return 0
    try=$TMP_DIR/firerunner-tls.yaml
    if [[ -f $("$fr" config path) ]]; then cp "$("$fr" config path)" "$try"; else : >"$try"; fi
    # Also tells whether this firerunner knows the keys and sets them together.
    if ! FIRERUNNER_CONFIG=$try "$fr" config set "${keys[@]}" >/dev/null 2>&1; then
        FLINTLOCK_TLS_LATER="the installed firerunner does not know it"
        return 0
    fi
    if jobs_running; then
        FLINTLOCK_TLS_LATER="jobs are running"
        return 0
    fi
    if ! systemctl is-active -q flintlockd; then
        FLINTLOCK_TLS_LATER="flintlockd is not running"
        return 0
    fi
    log "  turning on mutual TLS for flintlockd's API"
    mkdir -p "$(dirname "$FLINTLOCK_TLS_MARKER")"
    : >"$FLINTLOCK_TLS_MARKER"
    FLINTLOCK_TLS_SWITCHING=1
    # To put back without writing: a full disk must not stop the put-back.
    cp -p /etc/opt/flintlockd/config.yaml /etc/opt/flintlockd/config.yaml.plain
    flintlock_config 1
    systemctl restart flintlockd
    [[ -f $PENDING_FILE ]] && sed -i '/^restart flintlockd$/d' "$PENDING_FILE"
    PENDING=${PENDING//flintlockd /}
    end=$((SECONDS + FLINTLOCK_TLS_WAIT))
    while ((SECONDS < end)); do
        if FIRERUNNER_CONFIG=$try timeout 5 "$fr" vm list >/dev/null 2>"$TMP_DIR/tls-check.err"; then
            "$fr" config set "${keys[@]}" >/dev/null
            log "  firerunner and flintlockd talk over mutual TLS"
            flintlock_tls_revert   # with the keys set: restarts the daemon, keeps TLS
            return 0
        fi
        sleep 1
    done
    flintlock_tls_revert
    warn "firerunner could not reach flintlockd over TLS ($(tail -n1 "$TMP_DIR/tls-check.err")); flintlockd runs without TLS again"
    FLINTLOCK_TLS_LATER="firerunner could not reach it over TLS"
}

# flintlock_tls_revert ends a switch (done, failed or cut off): flintlockd and
# firerunner's daemon restart on what firerunner's config says.
flintlock_tls_revert() {
    trap '' INT HUP TERM   # finished once begun
    if firerunner_uses_tls; then
        # The daemon dials flintlockd once: it must load the keys.
        if systemctl is-active -q firerunner; then systemctl restart firerunner; fi
        rm -f /etc/opt/flintlockd/config.yaml.plain
    else
        if [[ -f /etc/opt/flintlockd/config.yaml.plain ]]; then
            mv -f /etc/opt/flintlockd/config.yaml.plain /etc/opt/flintlockd/config.yaml
        else
            flintlock_config_follow
        fi
        systemctl restart flintlockd
    fi
    rm -f "$FLINTLOCK_TLS_MARKER"
    FLINTLOCK_TLS_SWITCHING=0
    trap 'exit 130' INT HUP TERM
}

install_flintlock() {
    local have=""
    [[ -x $BIN_DIR/flintlockd ]] && have=$($BIN_DIR/flintlockd version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
    if [[ "$have" != "${FLINTLOCK_VERSION}" ]]; then
        log "installing flintlockd v${FLINTLOCK_VERSION}"
        local base="https://github.com/liquidmetal-dev/flintlock/releases/download/v${FLINTLOCK_VERSION}"
        local bin="flintlockd_${ARCH}"
        local want
        fetch "$base/$bin" "$TMP_DIR/$bin"
        want=$(pinned "v${FLINTLOCK_VERSION}/$bin") || {
            fetch "$base/checksums.txt" "$TMP_DIR/checksums.txt"
            want=$(awk -v f="$bin" '$2==f {print $1}' "$TMP_DIR/checksums.txt")
        }
        verify "$TMP_DIR/$bin" "$want"
        install -m 0755 "$TMP_DIR/$bin" "$BIN_DIR/flintlockd"
        installed "$BIN_DIR/flintlockd"
    else
        log "flintlockd v${FLINTLOCK_VERSION} already installed"
    fi

    # API token: generated once, root-only. Clients send it as basic auth (base64 of the token).
    if [[ ! -s $CONF_DIR/flintlock.token ]]; then
        (umask 077; head -c 32 /dev/urandom | base64 | tr -d '/+=\n' >"$CONF_DIR/flintlock.token")
    fi
    chmod 0600 "$CONF_DIR/flintlock.token"
    rm -f "$CONF_DIR/flintlock.env"   # older installs passed the token on the command line
    flintlock_tls_certs
    flintlock_tls_leftover
    flintlock_config_follow

    # flintlockd writes each VM's user-data, which holds the VM's SSH host
    # private key, to metadata.json with mode 0644: keep its state root-only.
    if [[ -d /var/lib/flintlock ]]; then
        chmod -R go-rwx /var/lib/flintlock
    fi

    put /etc/systemd/system/flintlockd.service <<EOF
[Unit]
Description=flintlock microVM service
Requires=containerd-flintlock.service firerunner-net.service
After=containerd-flintlock.service firerunner-net.service

[Service]
# All settings, including the API token, come from /etc/opt/flintlockd/config.yaml (0600).
ExecStart=${BIN_DIR}/flintlockd run
# VM state (metadata.json with the SSH host keys, console logs) for root only.
UMask=0077
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
# Shared services for microVMs: registry mirrors, S3 store for cache:
# --------------------------------------------------------------------------

DEFAULT_REGISTRY_MIRRORS=ghcr.io,quay.io,registry.k8s.io,mcr.microsoft.com
MIRRORS_FILE=$CONF_DIR/registry-mirrors          # FR_REGISTRY_MIRRORS of the last run
MIRROR_PORTS_FILE=$CONF_DIR/registry-mirror-ports # "host port" lines, see assign_mirror_ports
declare -A MIRROR_PORT=()

# Every mirror is a pair of services: firerunner-registry@NAME (the registry,
# on 127.0.0.1) and firerunner-registry-front@NAME (`firerunner registry-front`
# on the mirror's bridge port, the pull API only). NAME is docker.io for Docker
# Hub, else the mirrored registry's host. A mirror that crashes stops no other,
# and each one can write only its own data. firerunner-registry.service starts
# them all, and stops or restarts them all (their PartOf=); `firerunner status`
# and firerunner_service_up report it.
UNIT_DIR=/etc/systemd/system
REGISTRY_CONF_DIR=$CONF_DIR/registries.d   # NAME.yml (the registry), NAME.env (its front)
REGISTRY_USER=firerunner-registry           # runs the registries and their upkeep
CACHE_USER=firerunner-cache                 # runs the cache: server
REGISTRY_LOCAL_OFFSET=10000                 # behind a front, a registry listens on 127.0.0.1:<bridge port + this>
REGISTRY_FRONT=0                            # the installed firerunner has `registry-front` (setup_registries)
REGISTRY_DEFERRED=0                         # leaving the single service of earlier versions waits for no job to run
# The mirrors' data is an ext4 file system in REGISTRY_IMG, mounted on
# REGISTRY_DIR. It takes at most its size (FR_REGISTRY_GB) of the root disk:
# the image is sparse, and the space of blobs deleted in it goes back to the
# root disk (discard). When it is full only the mirrors fail, and microVMs pull
# from the registries themselves (internal/vm: hosts.toml, registry-mirrors).
REGISTRY_DIR=/var/lib/firerunner/registries
REGISTRY_IMG=/var/lib/firerunner/registries.img
REGISTRY_MOUNT_UNIT=var-lib-firerunner-registries.mount  # systemd-escape -p --suffix=mount $REGISTRY_DIR
REGISTRY_MIN_GB=5
REGISTRY_STATE_DIR=/var/lib/firerunner/registry-maint    # when each mirror was last garbage-collected
REGISTRY_EVICT_HIGH=90   # % of the file system in use that starts an eviction
REGISTRY_EVICT_LOW=80    # % in use the eviction stops at
# Earlier versions: one firerunner-registry.service ran every mirror from
# registries.sh, as root, its data on the root disk without a limit.
LEGACY_REGISTRIES_SH=$LIB_DIR/registries.sh
LEGACY_REGISTRY_CONF=$CONF_DIR/registry.yml
LEGACY_MIRROR_CONF_DIR=$CONF_DIR/registry-mirrors.d
LEGACY_REGISTRY_DATA=/var/lib/firerunner/registry
LEGACY_MIRROR_DATA_DIR=/var/lib/firerunner/registry-mirrors

# mirror_hosts: the registries mirrored besides Docker Hub, one per line.
mirror_hosts() {
    [[ $FR_REGISTRY_MIRRORS == none ]] && return 0
    tr ',' '\n' <<<"$FR_REGISTRY_MIRRORS" | sed '/^$/d' | sort -u
}

# assign_mirror_ports gives every mirrored registry its port on the bridge
# address (5001-5099). A port is never given to another registry: a microVM
# still booted with the old mapping would get that registry's image of the
# same name.
assign_mirror_ports() {
    local host port next=5001
    mkdir -p "$CONF_DIR"
    [[ -f $MIRROR_PORTS_FILE ]] || : >"$MIRROR_PORTS_FILE"
    while read -r host port; do
        [[ -n $host ]] || continue
        MIRROR_PORT[$host]=$port
        ((port >= next)) && next=$((port + 1))
    done <"$MIRROR_PORTS_FILE"
    for host in $(mirror_hosts); do
        [[ -n ${MIRROR_PORT[$host]:-} ]] && continue
        ((next <= 5099)) || die "no port left for the mirror of $host (5001-5099 were all given out; see $MIRROR_PORTS_FILE)"
        MIRROR_PORT[$host]=$next
        echo "$host $next" >>"$MIRROR_PORTS_FILE"
        next=$((next + 1))
    done
    echo "$FR_REGISTRY_MIRRORS" >"$MIRRORS_FILE"
}

# registry_ports: the mirrors' ports on the bridge address, for the firewall.
registry_ports() {
    local ports="5000" host
    for host in $(mirror_hosts); do ports+=", ${MIRROR_PORT[$host]}"; done
    echo "$ports"
}

# mirrors_json: vm.registry_mirrors for the firerunner config.
mirrors_json() {
    local out="" host
    for host in $(mirror_hosts); do
        out+="${out:+, }\"$host\": \"http://${FR_SUBNET}.1:${MIRROR_PORT[$host]}\""
    done
    echo "{$out}"
}

# registry_names: every mirror's instance name, Docker Hub's first.
registry_names() {
    echo docker.io
    mirror_hosts
}

# is_registry_name NAME: NAME is mirrored by this run (no pipe: grep -q could
# end it early, a SIGPIPE under pipefail).
is_registry_name() { [[ $'\n'$(registry_names)$'\n' == *$'\n'"$1"$'\n'* ]]; }

# registry_bridge_port NAME: the port microVMs pull NAME's images from.
registry_bridge_port() {
    if [[ $1 == docker.io ]]; then echo 5000; else echo "${MIRROR_PORT[$1]}"; fi
}

# registry_upstream NAME: the registry NAME's mirror pulls from.
registry_upstream() {
    if [[ $1 == docker.io ]]; then echo https://registry-1.docker.io; else echo "https://$1"; fi
}

# registry_front_available: the installed firerunner has `registry-front`.
# Without it (an older release) each registry listens on its bridge port
# itself, as before, and a later run with a newer firerunner adds the fronts.
registry_front_available() { "$BIN_DIR/firerunner" registry-front --help >/dev/null 2>&1; }

# system_user NAME DESCRIPTION: a system user and group NAME, no login, no home.
system_user() {
    getent passwd "$1" >/dev/null && return 0
    log "  adding system user $1"
    useradd --system --user-group --no-create-home --home-dir / \
        --shell "$(command -v nologin || echo /usr/sbin/nologin)" --comment "$2" "$1"
}

# own_tree DIR USER: DIR and everything in it belong to USER. DIR changes
# last and is what is checked, so a run cut short does all of it again.
own_tree() {
    [[ $(stat -c %U "$1") == "$2" ]] && return 0
    find "$1" -mindepth 1 -exec chown -h "$2:$2" {} + && chown "$2:$2" "$1"
}

# pending_add "ACTION UNIT": ACTION (restart, stop) waits in PENDING_FILE
# for a run when no job runs (apply_pending), after the ones already there.
pending_add() {
    PENDING+="${1#* } "
    mkdir -p "$(dirname "$PENDING_FILE")"
    grep -qxF "$1" "$PENDING_FILE" 2>/dev/null || echo "$1" >>"$PENDING_FILE"
}

# install_registry_binary installs distribution's registry. The mirrors are
# set up by setup_registries, once firerunner (their fronts) is installed.
install_registry_binary() {
    local have=""
    [[ -x $BIN_DIR/registry ]] && have=$($BIN_DIR/registry --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
    [[ "$have" != "${REGISTRY_VERSION}" ]] || return 0
    log "installing the registry mirrors (distribution v${REGISTRY_VERSION})"
    local base="https://github.com/distribution/distribution/releases/download/v${REGISTRY_VERSION}"
    local tgz="registry_${REGISTRY_VERSION}_linux_amd64.tar.gz"
    local want
    fetch "$base/$tgz" "$TMP_DIR/$tgz"
    want=$(pinned "v${REGISTRY_VERSION}/$tgz") || {
        fetch "$base/$tgz.sha256" "$TMP_DIR/$tgz.sha256"
        want=$(awk '{print $1}' "$TMP_DIR/$tgz.sha256")
    }
    verify "$TMP_DIR/$tgz" "$want"
    tar -xzf "$TMP_DIR/$tgz" -C "$TMP_DIR" registry
    install -m 0755 "$TMP_DIR/registry" "$BIN_DIR/registry"
    installed "$BIN_DIR/registry"
}

# registry_gb: the size, in GB, of a new mirrors' file system: FR_REGISTRY_GB,
# or a fifth of the file system the image is on (as FR_CACHE_MAX_GB=auto), at
# least REGISTRY_MIN_GB.
registry_gb() {
    if [[ $FR_REGISTRY_GB != auto ]]; then
        echo "$FR_REGISTRY_GB"
        return 0
    fi
    local kb gb
    kb=$(df -Pk "$(dirname "$REGISTRY_IMG")" | awk 'NR == 2 {print $2}')
    gb=$((kb / 5 / 1024 / 1024))
    ((gb >= REGISTRY_MIN_GB)) || gb=$REGISTRY_MIN_GB
    echo "$gb"
}

# registry_storage mounts the mirrors' file system on REGISTRY_DIR: an image
# made on the first run, or a file system the operator put in /etc/fstab for
# REGISTRY_DIR (another disk; then it is used as it is, not grown).
registry_storage() {
    mkdir -p "$(dirname "$REGISTRY_IMG")" "$REGISTRY_DIR"
    if findmnt -s -n -M "$REGISTRY_DIR" >/dev/null 2>&1; then
        log "  registry mirrors' file system: the one /etc/fstab mounts on $REGISTRY_DIR"
        if [[ -f $UNIT_DIR/$REGISTRY_MOUNT_UNIT ]]; then
            warn "$UNIT_DIR/$REGISTRY_MOUNT_UNIT (the image an earlier run made) hides the /etc/fstab entry from systemd: once its data is copied, remove it (docs/operations.md, Registry mirrors > Storage)"
        fi
        mountpoint -q "$REGISTRY_DIR" || mount "$REGISTRY_DIR" || die "could not mount $REGISTRY_DIR (its /etc/fstab entry)"
    else
        registry_image
    fi
    mountpoint -q "$REGISTRY_DIR" || die "$REGISTRY_DIR is not mounted (systemctl status $REGISTRY_MOUNT_UNIT)"
    # Root's, so no registry can write to it, mounted or not.
    chown root:root "$REGISTRY_DIR"
    chmod 0755 "$REGISTRY_DIR"
}

# registry_image makes REGISTRY_IMG once, mounts it with REGISTRY_MOUNT_UNIT
# and grows it to a larger FR_REGISTRY_GB.
registry_image() {
    local gb
    if [[ ! -f $REGISTRY_IMG ]]; then
        gb=$(registry_gb)
        log "  making the registry mirrors' file system: at most ${gb} GB of the root disk, in $REGISTRY_IMG"
        # Made under another name: REGISTRY_IMG always holds a whole file system.
        rm -f "$REGISTRY_IMG.new"
        truncate -s "${gb}G" "$REGISTRY_IMG.new"
        # -m 0: no blocks kept for root (the registries are not root); -i 64K:
        # one inode per 64 KiB, registry blobs are mostly much larger.
        mkfs.ext4 -q -F -m 0 -i 65536 -L fr-registries "$REGISTRY_IMG.new"
        chmod 0600 "$REGISTRY_IMG.new"
        mv -f "$REGISTRY_IMG.new" "$REGISTRY_IMG"
    fi
    put "$UNIT_DIR/$REGISTRY_MOUNT_UNIT" <<EOF
[Unit]
Description=FireRunner registry mirrors' file system (at most the size of ${REGISTRY_IMG})

[Mount]
What=${REGISTRY_IMG}
Where=${REGISTRY_DIR}
Type=ext4
# discard: the space of deleted blobs goes back to the root disk. nofail, and
# wanted by multi-user.target rather than local-fs.target: a damaged image
# does not hold up the boot (the mirrors stay down, microVMs pull from the
# registries themselves). relatime (the default) keeps the atimes the
# eviction goes by (registry-maint.sh).
Options=loop,discard,nodev,nosuid,noexec,nofail

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable -q "$REGISTRY_MOUNT_UNIT"
    if ! mountpoint -q "$REGISTRY_DIR"; then
        systemctl start "$REGISTRY_MOUNT_UNIT" ||
            die "could not mount $REGISTRY_IMG on $REGISTRY_DIR (journalctl -u $REGISTRY_MOUNT_UNIT)"
    fi
    registry_grow
}

# registry_grow grows the image to a larger FR_REGISTRY_GB, online. It is
# never shrunk: ext4 shrinks only unmounted (docs/operations.md).
registry_grow() {
    [[ $FR_REGISTRY_GB != auto ]] || return 0
    local have want dev
    have=$(stat -c %s "$REGISTRY_IMG")
    want=$((FR_REGISTRY_GB * 1024 * 1024 * 1024))
    if ((want < have)); then
        warn "FR_REGISTRY_GB=$FR_REGISTRY_GB is below the mirrors' $((have / 1024 / 1024 / 1024)) GB: not shrunk (docs/operations.md, Registry mirrors > Storage, says how)"
        return 0
    fi
    ((want > have)) || return 0
    dev=$(losetup -j "$REGISTRY_IMG" | head -n 1 | cut -d: -f1)
    [[ -n $dev ]] || die "no loop device for $REGISTRY_IMG: is $REGISTRY_DIR mounted?"
    log "  growing the registry mirrors' file system to ${FR_REGISTRY_GB} GB"
    truncate -s "${FR_REGISTRY_GB}G" "$REGISTRY_IMG"
    losetup -c "$dev"
    resize2fs "$dev" >/dev/null 2>&1 || die "resize2fs $dev failed (run it by hand to see why); $REGISTRY_IMG is ${FR_REGISTRY_GB} GB, its file system not yet"
}

# legacy_registry_data NAME: where earlier versions kept mirror NAME's data.
legacy_registry_data() {
    if [[ $1 == docker.io ]]; then echo "$LEGACY_REGISTRY_DATA"; else echo "$LEGACY_MIRROR_DATA_DIR/$1"; fi
}

# registry_legacy: earlier versions' mirrors are on this host: their service,
# or data of a mirror this run serves.
registry_legacy() {
    local name
    if [[ -f $UNIT_DIR/firerunner-registry.service ]] && ! grep -qx 'Type=oneshot' "$UNIT_DIR/firerunner-registry.service"; then
        return 0
    fi
    for name in $(registry_names); do
        [[ -d $(legacy_registry_data "$name") ]] && return 0
    done
    return 1
}

# registry_fits KB: KB more data keeps the mirrors' file system at most
# REGISTRY_EVICT_LOW % full, and a copy of it fits on the root disk with 1 GiB to spare.
registry_fits() {
    local kb=$1 size used avail
    read -r size used < <(df -Pk "$REGISTRY_DIR" | awk 'NR == 2 {print $2, $3}')
    avail=$(df -Pk "$(dirname "$REGISTRY_IMG")" | awk 'NR == 2 {print $4}')
    (( (used + kb) * 100 <= size * REGISTRY_EVICT_LOW && kb + 1048576 <= avail ))
}

# legacy_keep DIR: renames DIR to DIR.migrated (DIR.migrated-<time> if that is
# taken) and prints the new name.
legacy_keep() {
    local to=$1.migrated
    [[ ! -e $to ]] || to=$1.migrated-$(date +%Y%m%d%H%M%S)
    mv -T -- "$1" "$to"
    echo "$to"
}

# registry_migrate moves earlier versions' mirrors (one root service, data on
# the root disk) to firerunner-registry@ instances; setup_registries runs it
# only while no job runs, as the mirrors are down meanwhile (microVMs pull
# from the registries themselves). Each mirror's data is copied under a
# temporary name and renamed when complete, so a run cut short copies it
# again; the old directory is then renamed to <dir>.migrated and kept until the
# operator deletes it. Data that does not fit (FR_REGISTRY_GB, or the root
# disk's free space for the copy) is not copied: that mirror starts empty and
# fills from its registry again.
registry_migrate() {
    local name src dest tmp kb kept=""
    log "  moving the registry mirrors to a service each (firerunner-registry@), data into $REGISTRY_DIR"
    if systemctl is-active -q firerunner-registry; then
        log "  stopping the single firerunner-registry service of earlier versions"
        systemctl stop firerunner-registry
    fi
    for name in $(registry_names); do
        src=$(legacy_registry_data "$name")
        [[ -d $src ]] || continue
        dest=$REGISTRY_DIR/$name tmp=$REGISTRY_DIR/.$name.migrating
        if [[ -e $dest ]]; then
            log "  $name: already in $dest"
        else
            rm -rf -- "$tmp"
            kb=$(du -sk -- "$src" | awk '{print $1}')
            if ! registry_fits "$kb"; then
                warn "  $name: its $((kb / 1024)) MiB do not fit (FR_REGISTRY_GB, or the root disk's free space for a copy): this mirror starts empty"
            elif mkdir -- "$tmp" && cp -a -- "$src/." "$tmp/" && own_tree "$tmp" "$REGISTRY_USER" && mv -T -- "$tmp" "$dest"; then
                log "  $name: $((kb / 1024)) MiB copied from $src"
            else
                rm -rf -- "$tmp"
                warn "  $name: copying $src failed: this mirror starts empty"
            fi
        fi
        kept+=" $(legacy_keep "$src")"
    done
    for src in "$LEGACY_MIRROR_DATA_DIR"/*; do
        [[ -d $src && $src != *.migrated* ]] || continue
        log "  $(basename "$src") is no longer mirrored: its old data can go (rm -rf $src)"
    done
    # Last: until here a run cut short still finds the old service and goes on.
    rm -f "$LEGACY_REGISTRIES_SH" "$LEGACY_REGISTRY_CONF" "$UNIT_DIR/firerunner-registry.service.d/firerunner-proxy.conf"
    rm -rf "$LEGACY_MIRROR_CONF_DIR"
    rmdir "$UNIT_DIR/firerunner-registry.service.d" 2>/dev/null || true
    if [[ -n $kept ]]; then
        log "  the old data stays in:$kept"
        log "  once the mirrors serve pulls (docs/operations.md), free the root disk: rm -rf$kept"
    fi
}

# registry_files writes every mirror's registry config and front settings,
# the units and the upkeep, and removes a dropped mirror's config.
registry_files() {
    local name port lport addr f keep=" "
    mkdir -p "$REGISTRY_CONF_DIR" "$REGISTRY_STATE_DIR"
    # The registries read their config as REGISTRY_USER: the directories may
    # be searched (the root-only files in CONF_DIR stay 0600).
    chmod 0755 "$REGISTRY_CONF_DIR"
    chmod go+x "$CONF_DIR"
    for name in $(registry_names); do
        keep+="$name.yml $name.env "
        port=$(registry_bridge_port "$name")
        lport=$((port + REGISTRY_LOCAL_OFFSET))
        if [[ $REGISTRY_FRONT == 1 ]]; then addr=127.0.0.1:$lport; else addr=${FR_SUBNET}.1:$port; fi
        mkdir -p "$REGISTRY_DIR/$name"
        chmod 0700 "$REGISTRY_DIR/$name"
        own_tree "$REGISTRY_DIR/$name" "$REGISTRY_USER"
        put "$REGISTRY_CONF_DIR/$name.yml" <<EOF
version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: $REGISTRY_DIR/$name
  # So the ttl deletes what expires (a proxy refuses DELETE from clients all
  # the same), and registry garbage-collect the rest (registry-maint.sh).
  delete:
    enabled: true
http:
  addr: $addr
proxy:
  remoteurl: $(registry_upstream "$name")
  ttl: 168h
EOF
        put "$REGISTRY_CONF_DIR/$name.env" <<EOF
FR_FRONT_LISTEN=${FR_SUBNET}.1:$port
FR_FRONT_UPSTREAM=http://127.0.0.1:$lport
EOF
    done
    for f in "$REGISTRY_CONF_DIR"/*; do
        [[ -f $f && $keep != *" $(basename "$f") "* ]] || continue
        rm -f "$f"
    done

    put "$UNIT_DIR/firerunner-registry.service" <<EOF
[Unit]
Description=FireRunner registry mirrors for microVMs (every firerunner-registry@ and firerunner-registry-front@)
# Starting it starts every mirror enabled for it; stopping or restarting it
# stops or restarts them all (their PartOf=).

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true

[Install]
WantedBy=multi-user.target
EOF
    put "$UNIT_DIR/firerunner-registry@.service" <<EOF
[Unit]
Description=FireRunner registry mirror %i (pull-through cache for microVMs)
Requires=firerunner-net.service
After=firerunner-net.service
# Its data is on the mirrors' file system only, never on the disk below it.
RequiresMountsFor=${REGISTRY_DIR}
PartOf=firerunner-registry.service

[Service]
ExecStart=${BIN_DIR}/registry serve ${REGISTRY_CONF_DIR}/%i.yml
User=${REGISTRY_USER}
Group=${REGISTRY_USER}
UMask=0077
Restart=always
RestartSec=5
# Serves untrusted microVMs: not root, no capabilities (ports above 1024),
# only its own mirror's data writable, no devices, kernel knobs or namespaces.
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=${REGISTRY_DIR}/%i
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
CapabilityBoundingSet=

[Install]
WantedBy=firerunner-registry.service
EOF
    # \${...}: expanded by systemd from the instance's .env file, not here.
    put "$UNIT_DIR/firerunner-registry-front@.service" <<EOF
[Unit]
Description=FireRunner registry mirror %i: the pull API on the bridge address
Requires=firerunner-net.service
Wants=firerunner-registry@%i.service
After=firerunner-net.service firerunner-registry@%i.service
PartOf=firerunner-registry.service

[Service]
EnvironmentFile=${REGISTRY_CONF_DIR}/%i.env
ExecStart=${BIN_DIR}/firerunner registry-front --listen \${FR_FRONT_LISTEN} --upstream \${FR_FRONT_UPSTREAM}
Restart=always
RestartSec=5
# On SIGTERM it takes no new connections and gives running pulls 60 s.
TimeoutStopSec=75s
# Serves untrusted microVMs and keeps no data: a user of its own for each
# run, no capabilities (ports above 1024), no devices, kernel knobs or namespaces.
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
CapabilityBoundingSet=

[Install]
WantedBy=firerunner-registry.service
EOF

    registry_maint_script | put "$LIB_DIR/registry-maint.sh" 0755
    put "$UNIT_DIR/firerunner-registry-evict.service" <<EOF
[Unit]
Description=Evict the FireRunner registry mirrors' least read layers above ${REGISTRY_EVICT_HIGH}% of their file system
RequiresMountsFor=${REGISTRY_DIR}

[Service]
Type=oneshot
User=${REGISTRY_USER}
Group=${REGISTRY_USER}
ExecStart=${LIB_DIR}/registry-maint.sh evict ${REGISTRY_EVICT_HIGH} ${REGISTRY_EVICT_LOW}
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=${REGISTRY_DIR}
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
PrivateNetwork=yes
CapabilityBoundingSet=
EOF
    put "$UNIT_DIR/firerunner-registry-evict.timer" <<EOF
[Unit]
Description=Check the FireRunner registry mirrors' file system every 10 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
EOF
    put "$UNIT_DIR/firerunner-registry-gc.service" <<EOF
[Unit]
Description=Garbage-collect the FireRunner registry mirrors (once a day each, only while no job runs)
RequiresMountsFor=${REGISTRY_DIR}

[Service]
Type=oneshot
ExecStart=${LIB_DIR}/registry-maint.sh gc
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=${REGISTRY_DIR} ${REGISTRY_STATE_DIR}
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
PrivateNetwork=yes
# Root only to stop and start a mirror; garbage-collect runs as
# ${REGISTRY_USER} (setpriv).
CapabilityBoundingSet=CAP_SETUID CAP_SETGID
EOF
    put "$UNIT_DIR/firerunner-registry-gc.timer" <<EOF
[Unit]
Description=Hourly chance to garbage-collect the FireRunner registry mirrors

[Timer]
OnCalendar=hourly
RandomizedDelaySec=10min
Persistent=true

[Install]
WantedBy=timers.target
EOF
}

# registry_start starts what does not run, restarts (when no job runs) the
# mirrors whose config, unit or binary changed, and stops a mirror no longer
# in FR_REGISTRY_MIRRORS (its data stays until evicted).
registry_start() {
    local name reg front w all_reg=0 all_front=0 timer
    systemctl daemon-reload
    changed "$UNIT_DIR/firerunner-registry@.service" "$BIN_DIR/registry" && all_reg=1
    changed "$UNIT_DIR/firerunner-registry-front@.service" "$BIN_DIR/firerunner" && all_front=1
    for w in "$UNIT_DIR"/firerunner-registry.service.wants/firerunner-registry{,-front}@*.service; do
        [[ -L $w || -e $w ]] || continue
        name=${w##*@}
        name=${name%.service}
        is_registry_name "$name" && continue
        log "  stopping $(basename "$w" .service) (no longer mirrored)"
        systemctl disable --now -q "$(basename "$w")"
    done
    systemctl enable -q firerunner-registry
    for name in $(registry_names); do
        reg=firerunner-registry@$name front=firerunner-registry-front@$name
        if [[ $REGISTRY_FRONT != 1 ]] && systemctl is-enabled -q "$front" 2>/dev/null; then
            # The front leaves the bridge port before its registry takes it back.
            log "  stopping $front (this firerunner has no registry-front)"
            systemctl disable --now -q "$front"
        fi
        systemctl enable -q "$reg"
        if ! systemctl is-active -q "$reg"; then
            log "  starting $reg"
            systemctl restart "$reg" || warn "  $reg did not start (journalctl -u $reg)"
        elif [[ $all_reg == 1 ]] || changed "$REGISTRY_CONF_DIR/$name.yml"; then
            restart_when_idle "$reg"
        fi
        [[ $REGISTRY_FRONT == 1 ]] || continue
        systemctl enable -q "$front"
        if ! systemctl is-active -q "$front"; then
            if [[ " $PENDING " == *" $reg "* ]]; then
                # Its registry may still hold the bridge port: the front starts
                # after the registry's restart, when no job runs.
                pending_add "restart $front"
            else
                log "  starting $front"
                systemctl restart "$front" || warn "  $front did not start (journalctl -u $front)"
            fi
        elif [[ $all_front == 1 ]] || changed "$REGISTRY_CONF_DIR/$name.env"; then
            restart_when_idle "$front"
        fi
    done
    systemctl start firerunner-registry
    for timer in firerunner-registry-evict firerunner-registry-gc; do
        systemctl enable -q --now "$timer.timer"
        # A changed schedule applies from the timer's next start.
        if changed "$UNIT_DIR/$timer.timer"; then
            systemctl restart "$timer.timer"
        fi
    done
}

# setup_registries runs the registry mirrors, after install_firerunner (the
# fronts are firerunner). Leaving the single service of earlier versions
# stops every mirror while their data is copied: it waits for a run when no
# job runs, and until then that service keeps serving as it is.
setup_registries() {
    if registry_legacy && systemctl is-active -q firerunner-registry && jobs_running; then
        warn "  registry mirrors: moving to a service each waits until no job runs (re-run the installer then)"
        REGISTRY_DEFERRED=1
        PENDING+="firerunner-registry(move) "
        return 0
    fi
    REGISTRY_FRONT=0
    if registry_front_available; then
        REGISTRY_FRONT=1
    else
        warn "  firerunner $("$BIN_DIR/firerunner" version 2>/dev/null | awk '{print $2}') has no registry-front: each registry mirror listens on the bridge itself"
    fi
    log "registry mirrors: $(registry_names | paste -sd ' ' -)"
    system_user "$REGISTRY_USER" "FireRunner registry mirrors"
    registry_storage
    if registry_legacy; then
        registry_migrate
    fi
    registry_files
    registry_start
}

# registry_maint_script: the mirrors' upkeep the evict and gc timers run.
registry_maint_script() {
    cat <<'EOF'
#!/bin/bash
# registry-maint.sh evict HIGH LOW | gc: upkeep of the registry mirrors' file
# system (written by install.sh).
#
# evict (every 10 minutes, as the registry user): above HIGH % in use, deletes
#   the layer blobs read longest ago until LOW % is in use. The file system is
#   mounted relatime, so a blob's atime is its last read to within a day. Safe
#   while the mirrors run: a mirror asked for a blob it no longer has pulls it
#   from its registry again. Manifests stay: registry garbage-collect stops at
#   a manifest whose blob is gone, and the mirrors' ttl expires them.
# gc (hourly, as root): at most once a day per mirror, and only while no job
#   runs, stops the mirror, runs registry garbage-collect as the registry user
#   (it must not run next to a registry that writes) and starts it again.
#   Meanwhile microVMs pull from the registries themselves.
set -euo pipefail
export LC_ALL=C
dir=${FR_REGISTRY_DIR:-/var/lib/firerunner/registries}
conf=${FR_REGISTRY_CONF_DIR:-/etc/firerunner/registries.d}
state=${FR_REGISTRY_STATE_DIR:-/var/lib/firerunner/registry-maint}
jobs=${FR_JOBS_DIR:-/run/firerunner/jobs}
user=${FR_REGISTRY_USER:-firerunner-registry}
registry=${FR_REGISTRY_BIN:-/usr/local/bin/registry}
day=$((24 * 3600 - 600))   # an hourly timer that fires a little early still counts
# A stop or Ctrl-C ends the script through the EXIT trap (gc starts the mirror again).
trap 'exit 143' TERM INT HUP

jobs_running() { compgen -G "$jobs/*.json" >/dev/null; }

evict() {
    local high=$1 low=$2 size used target d b kb path deleted=0 freed=0
    read -r size used < <(df -Pk "$dir" | awk 'NR == 2 {print $2, $3}')
    [[ $size =~ ^[0-9]+$ && $used =~ ^[0-9]+$ ]] && ((size > 0)) || { echo "cannot read the size of $dir" >&2; return 1; }
    ((used * 100 > size * high)) || return 0
    target=$((size * low / 100))
    local -A manifest=()
    while IFS= read -r -d '' d; do
        manifest[${d##*/}]=1
    done < <(find "$dir" -path '*/_manifests/revisions/sha256/*' -name link -type f -printf '%h\0')
    # "atime KiB blob-directory", read longest ago first; KiB is what it takes on disk.
    local -a blobs=()
    mapfile -d '' blobs < <(find "$dir" -path '*/docker/registry/v2/blobs/sha256/*' -name data -type f -printf '%A@ %k %h\0' | sort -z -n)
    for b in "${blobs[@]}"; do
        ((used > target)) || break
        read -r _ kb _ <<<"$b"
        path=${b#* * }
        [[ -z ${manifest[${path##*/}]:-} ]] || continue
        rm -rf -- "$path"
        used=$((used - kb)) deleted=$((deleted + 1)) freed=$((freed + kb))
    done
    echo "evicted the $deleted layers read longest ago ($((freed / 1024)) MiB): about $((used * 100 / size))% of $((size / 1024)) MiB in use"
}

gc() {
    local c name stamp unit out failed=0 stopped
    mkdir -p "$state"
    for c in "$conf"/*.yml; do
        [[ -f $c ]] || continue
        name=$(basename "$c" .yml)
        stamp=$state/gc-$name unit=firerunner-registry@$name
        if [[ -f $stamp ]] && (($(date +%s) - $(stat -c %Y "$stamp") < day)); then
            continue
        fi
        if jobs_running; then
            echo "garbage collection waits while jobs run"
            return "$failed"
        fi
        stopped=0
        if systemctl is-active -q "$unit"; then
            systemctl stop "$unit"
            stopped=1
            # shellcheck disable=SC2064
            trap "systemctl start '$unit'" EXIT
        fi
        # Tried once a day, whatever the outcome: a failing one does not stop
        # the mirror every hour.
        touch "$stamp"
        out=$(mktemp)
        if setpriv --reuid="$user" --regid="$user" --clear-groups -- "$registry" garbage-collect "$c" >"$out" 2>&1; then
            echo "$name: garbage-collected ($(grep -E 'eligible for deletion' "$out" | tail -n 1 | sed 's/^[[:space:]]*//' || true))"
        else
            echo "$name: registry garbage-collect failed: $(tail -n 5 "$out" | tr '\n' ' ')" >&2
            failed=1
        fi
        rm -f "$out"
        if ((stopped)); then
            trap - EXIT
            systemctl start "$unit"
        fi
    done
    return "$failed"
}

# Never on the disk below an unmounted file system.
if ! mountpoint -q "$dir"; then
    echo "$dir is not mounted: nothing to do"
    exit 0
fi
# One at a time: evict must not delete a blob garbage-collect is sweeping.
exec 9<"$dir"
flock 9
case ${1:-} in
    evict) evict "${2:?HIGH}" "${3:?LOW}" ;;
    gc)    gc ;;
    *)     echo "usage: registry-maint.sh evict HIGH LOW | gc" >&2; exit 2 ;;
esac
EOF
}

# cache_clean_script: the cache cleanup the timer runs (install_cache_server).
cache_clean_script() {
    cat <<'EOF'
#!/bin/bash
# cache-clean.sh DAYS MAX_GB: deletes cache: archives not written for DAYS
# days, then the oldest ones until the cache takes at most MAX_GB (auto: a
# fifth of the file system it is on), so jobs cannot fill the host's disk.
set -euo pipefail
export LC_ALL=C
dir=${FR_CACHE_DIR:-/var/lib/firerunner/cache/runner-cache}
days=$1 max_gb=$2
[[ -d $dir ]] || exit 0
find "$dir" -mindepth 1 -type f -mtime +"$days" -delete
if [[ $max_gb == auto ]]; then
    max_kb=$(( $(df -Pk "$dir" | awk 'NR == 2 {print $2}') / 5 ))
else
    max_kb=$(( max_gb * 1024 * 1024 ))
fi
# "mtime KiB path", oldest first; KiB is what the file takes on disk.
mapfile -d '' files < <(find "$dir" -mindepth 1 -type f -printf '%T@ %k %p\0' | sort -z -n)
total=0
for f in "${files[@]}"; do
    read -r _ kb _ <<<"$f"
    total=$((total + kb))
done
deleted=0 freed=0
for f in "${files[@]}"; do
    ((total > max_kb)) || break
    read -r _ kb _ <<<"$f"
    rm -f -- "${f#* * }"
    total=$((total - kb)) deleted=$((deleted + 1)) freed=$((freed + kb))
done
if ((deleted > 0)); then
    echo "deleted the $deleted oldest cache archives ($((freed / 1024)) MiB) to stay under $((max_kb / 1024)) MiB"
fi
find "$dir" -mindepth 1 -type d -empty -delete
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
        local want
        fetch "$base/$dir.tar.gz" "$TMP_DIR/$dir.tar.gz"
        want=$(pinned "v${VERSITYGW_VERSION}/$dir.tar.gz") || {
            fetch "$base/checksums.txt" "$TMP_DIR/versitygw.sums"
            want=$(awk -v f="$dir.tar.gz" '$2==f {print $1}' "$TMP_DIR/versitygw.sums")
        }
        verify "$TMP_DIR/$dir.tar.gz" "$want"
        tar -xzf "$TMP_DIR/$dir.tar.gz" -C "$TMP_DIR" "$dir/versitygw"
        install -m 0755 "$TMP_DIR/$dir/versitygw" "$BIN_DIR/versitygw"
        installed "$BIN_DIR/versitygw"
    fi
    # The top-level directory is the S3 root; each sub-directory is a bucket.
    mkdir -p /var/lib/firerunner/cache/runner-cache
    system_user "$CACHE_USER" "FireRunner cache: server"
    # Every project's cache: archives, which may hold secrets: the cache user only.
    chmod 0700 /var/lib/firerunner/cache
    if [[ $(stat -c %U /var/lib/firerunner/cache) != "$CACHE_USER" ]]; then
        # Earlier versions ran it as root. Its files change hands with it
        # stopped (start_services starts it again): an upload or download of a
        # running job fails, which a job only warns about (cache: is best effort).
        if systemctl is-active -q firerunner-cache 2>/dev/null; then
            log "  stopping firerunner-cache to give its files to $CACHE_USER"
            systemctl stop firerunner-cache
        fi
        own_tree /var/lib/firerunner/cache "$CACHE_USER" || die "could not give /var/lib/firerunner/cache to $CACHE_USER"
    fi
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
User=${CACHE_USER}
Group=${CACHE_USER}
UMask=0077
Restart=always
RestartSec=5
# Serves untrusted microVMs: not root, no capabilities (port 9000), only the
# cache writable, no devices, kernel knobs or namespaces.
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/firerunner/cache
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF
    cache_clean_script | put "$LIB_DIR/cache-clean.sh" 0755
    put /etc/systemd/system/firerunner-cache-clean.service <<EOF
[Unit]
Description=Delete FireRunner cache: archives older than ${FR_CACHE_DAYS} days, then the oldest above the size cap

[Service]
Type=oneshot
ExecStart=${LIB_DIR}/cache-clean.sh ${FR_CACHE_DAYS} ${FR_CACHE_MAX_GB}
User=${CACHE_USER}
Group=${CACHE_USER}
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/firerunner/cache
ProtectHome=yes
PrivateTmp=yes
PrivateNetwork=yes
EOF
    # Hourly: a day of uploads above the size cap could fill the disk first.
    put /etc/systemd/system/firerunner-cache-clean.timer <<EOF
[Unit]
Description=Hourly cleanup of the FireRunner cache

[Timer]
OnCalendar=hourly
RandomizedDelaySec=10min
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

# link_on_sudo_path makes `sudo firerunner` work where sudo's secure_path
# leaves out /usr/local/bin (RHEL, Rocky). An existing file is never replaced.
link_on_sudo_path() {
    local link=/usr/bin/firerunner
    if [[ -L $link || ! -e $link ]]; then
        ln -sfn "$BIN_DIR/firerunner" "$link"
    fi
}

install_firerunner() {
    if [[ -n $FR_BINARY ]]; then
        log "installing firerunner from $FR_BINARY"
        install_binary "$FR_BINARY" "$BIN_DIR/firerunner"
    else
        log "installing firerunner ${FR_VERSION}"
        local base bin="firerunner-linux-${ARCH}"
        base=$(release_base)
        fetch "$base/$bin" "$TMP_DIR/$bin"
        fetch "$base/checksums.txt" "$TMP_DIR/fr.sums"
        verify_release "$TMP_DIR/fr.sums" "$base/checksums.txt.sig"
        verify "$TMP_DIR/$bin" "$(awk -v f="$bin" '$2==f {print $1}' "$TMP_DIR/fr.sums")"
        # The binary that ran is the one renamed into place.
        install_binary "$TMP_DIR/$bin" "$BIN_DIR/firerunner" release_runs
    fi
    log "  $($BIN_DIR/firerunner version)"
    link_on_sudo_path

    mkdir -p "$CONF_DIR/executor" /var/lib/firerunner/builder-cache
    chmod 0700 "$CONF_DIR/executor" /var/lib/firerunner/builder-cache
    [[ -f $CONF_DIR/executor/id_ed25519 ]] ||
        ssh-keygen -q -t ed25519 -N "" -C firerunner-executor -f "$CONF_DIR/executor/id_ed25519"
    # Write the default config once so it is visible and editable.
    if [[ ! -f $CONF_DIR/config.yaml ]]; then
        $BIN_DIR/firerunner config set pool.size "$FR_POOL_SIZE" >/dev/null
        $BIN_DIR/firerunner config set vm.vcpu "$FR_VM_VCPU" >/dev/null
        $BIN_DIR/firerunner config set vm.registry_mirror "http://${FR_SUBNET}.1:5000" >/dev/null
        log "  microVMs: $FR_VM_VCPU vCPU each, $FR_POOL_SIZE kept booted"
    else
        # A re-run keeps the operator's settings, unless they are given again.
        [[ -z $FR_POOL_SIZE_GIVEN ]] || $BIN_DIR/firerunner config set pool.size "$FR_POOL_SIZE" >/dev/null
        [[ -z $FR_VM_VCPU_GIVEN ]] || $BIN_DIR/firerunner config set vm.vcpu "$FR_VM_VCPU" >/dev/null
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
# The daemon's socket, job states, pinned host keys and SSH control sockets:
# root only, also in the moment before the socket is chmod 0600.
RuntimeDirectoryMode=0700
UMask=0077
# /var/lib/flintlock/vm: reconcile removes the state dirs flintlockd leaves for deleted VMs.
# builder-cache: layer caches of deleted builders, restored into the project's next one.
ReadWritePaths=/run/firerunner -/run/lock -/run/lvm -/var/lib/flintlock/vm /var/lib/firerunner/builder-cache
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
    if [[ "$have" == "${GITLAB_RUNNER_VERSION}" ]]; then
        :
    elif drained && [[ $(systemctl is-active gitlab-runner 2>/dev/null || true) =~ ^(active|activating|deactivating|reloading)$ ]]; then
        # Draining: gitlab-runner finishes its jobs and stops by itself (a stop
        # now would wait for them, a start would undo the drain).
        warn "gitlab-runner $have not upgraded to v${GITLAB_RUNNER_VERSION}: the host is being drained (re-run the installer once it is)"
        PENDING+="gitlab-runner(upgrade) "
    elif [[ -n $have ]] && systemctl is-active -q gitlab-runner && jobs_running; then
        # Stopping gitlab-runner would abort the running jobs; the next run when idle upgrades it.
        warn "gitlab-runner $have not upgraded to v${GITLAB_RUNNER_VERSION}: jobs are running"
        PENDING+="gitlab-runner(upgrade) "
    else
        log "installing gitlab-runner v${GITLAB_RUNNER_VERSION}"
        local base="https://gitlab-runner-downloads.s3.amazonaws.com/v${GITLAB_RUNNER_VERSION}"
        local bin="gitlab-runner-linux-${ARCH}"
        local want
        fetch "$base/binaries/$bin" "$TMP_DIR/$bin"
        want=$(pinned "v${GITLAB_RUNNER_VERSION}/binaries/$bin") || {
            fetch "$base/release.sha256" "$TMP_DIR/runner.sums"
            want=$(awk -v f="binaries/$bin" '$2==f {print $1}' "$TMP_DIR/runner.sums")
        }
        verify "$TMP_DIR/$bin" "$want"
        if drained; then
            # Stopped by the drain: the new binary runs from `firerunner undrain`.
            install -m 0755 "$TMP_DIR/$bin" "$BIN_DIR/gitlab-runner"
            log "  gitlab-runner stays stopped: the host is drained (firerunner undrain starts it)"
        else
            systemctl stop gitlab-runner 2>/dev/null || true
            install -m 0755 "$TMP_DIR/$bin" "$BIN_DIR/gitlab-runner"
            systemctl start gitlab-runner 2>/dev/null || true
        fi
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
    # The registry mirrors start later, in setup_registries.
    for svc in containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-cache flintlockd; do
        systemctl enable -q "$svc"
        case $svc in
            containerd-flintlock) files="$CONF_DIR/containerd.toml $BIN_DIR/containerd" ;;
            firerunner-net)       files="$LIB_DIR/net-up.sh" ;;
            firerunner-dnsmasq)   files="$CONF_DIR/dnsmasq.conf" ;;
            firerunner-cache)     files="$CONF_DIR/cache.env $BIN_DIR/versitygw" ;;
            flintlockd)
                files="/etc/opt/flintlockd/config.yaml $BIN_DIR/flintlockd $BIN_DIR/firecracker"
                # Its certificates only matter while it serves TLS.
                flintlock_tls_on && files+=" $FLINTLOCK_TLS_DIR/ca.crt $FLINTLOCK_TLS_DIR/server.crt" ;;
        esac
        # shellcheck disable=SC2086
        if ! systemctl is-active -q "$svc"; then
            log "  starting $svc"
            systemctl restart "$svc"
        elif [[ $svc == flintlockd && $FLINTLOCK_TLS_RESET == 1 ]]; then
            # A new CA, or a switch to TLS cut off: firerunner and flintlockd
            # may not be able to talk, jobs or not.
            log "  restarting flintlockd (TLS)"
            systemctl restart flintlockd
        elif changed "/etc/systemd/system/$svc.service" $files; then
            case $svc in
                firerunner-net)
                    # A restart would delete the bridge and cut every running microVM off.
                    log "  reloading $svc rules"
                    systemctl reload "$svc" ;;
                containerd-flintlock|flintlockd)
                    restart_when_idle "$svc" ;;
                *)
                    log "  restarting $svc"
                    systemctl restart "$svc" ;;
            esac
        fi
    done
    systemctl enable -q --now firerunner-cache-clean.timer
    # A changed schedule applies from the timer's next start.
    if changed /etc/systemd/system/firerunner-cache-clean.timer; then
        systemctl restart firerunner-cache-clean.timer
    fi
}

verify_install() {
    log "verifying"
    local ok=1 svc name
    local services="containerd-flintlock firerunner-net firerunner-dnsmasq firerunner-registry firerunner-cache flintlockd firerunner"
    proxy_on && services+=" firerunner-proxy"
    if [[ $REGISTRY_DEFERRED != 1 ]]; then
        for name in $(registry_names); do
            services+=" firerunner-registry@$name"
            [[ $REGISTRY_FRONT == 1 ]] && services+=" firerunner-registry-front@$name"
        done
    fi
    for svc in $services; do
        if systemctl is-active -q "$svc"; then
            log "  $svc: active"
        elif [[ " $PENDING " == *" $svc "* ]]; then
            log "  $svc: starts when no job runs (pending)"
        else
            warn "  $svc: NOT active (journalctl -u $svc)"; ok=0
        fi
    done
    if [[ $REGISTRY_DEFERRED != 1 ]]; then
        if mountpoint -q "$REGISTRY_DIR"; then
            log "  registry mirrors' file system: $(df -h --output=used,size "$REGISTRY_DIR" | tail -n 1 | awk '{print $1 " of " $2}') used"
        else
            warn "  $REGISTRY_DIR is not mounted (systemctl status $REGISTRY_MOUNT_UNIT)"; ok=0
        fi
        # A mirror that does not answer only costs pulls from Docker Hub itself.
        for _ in $(seq 1 10); do
            curl -fsS -m 3 -o /dev/null "http://${FR_SUBNET}.1:5000/v2/" 2>/dev/null && break
            sleep 1
        done
        curl -fsS -m 3 -o /dev/null "http://${FR_SUBNET}.1:5000/v2/" 2>/dev/null ||
            warn "  the Docker Hub mirror does not answer on ${FR_SUBNET}.1:5000 (journalctl -u firerunner-registry@docker.io -u firerunner-registry-front@docker.io)"
    fi
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
    if dmsetup status "$THINPOOL" >/dev/null 2>&1; then
        log "  thin pool ${THINPOOL}: ok"
    else
        warn "  thin pool ${THINPOOL} missing"; ok=0
    fi
    [[ $ok -eq 1 ]] || die "installation finished with errors"

    cat <<EOF

FireRunner host is ready.
  flintlock API : ${FLINTLOCK_ENDPOINT} (localhost only$(firerunner_uses_tls && echo ", mutual TLS"))
  API token     : ${CONF_DIR}/flintlock.token (root only)
  microVM net   : ${FR_BRIDGE} ${FR_SUBNET}.0/24, DHCP + NAT
  versions      : containerd ${CONTAINERD_VERSION}, firecracker ${FIRECRACKER_VERSION}, flintlock ${FLINTLOCK_VERSION}

Next steps:
  firerunner doctor                  check the host
  firerunner run -- uname -a         boot a throwaway microVM
  firerunner runner register --url https://gitlab.example.com --token -   # paste the glrt-... token
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
        # Stop it first: removing the service leaves a running process that
        # still asks GitLab for jobs.
        systemctl stop gitlab-runner 2>/dev/null || true
        $BIN_DIR/gitlab-runner uninstall --service gitlab-runner >/dev/null 2>&1 || true
        rm -f "$BIN_DIR/gitlab-runner"
    fi
    systemctl disable --now -q firerunner-cache-clean.timer 2>/dev/null || true
    rm -f /etc/systemd/system/firerunner-cache-clean.{service,timer}
    # The registry mirrors, their upkeep and their file system (its image stays).
    systemctl disable --now -q firerunner-registry-evict.timer firerunner-registry-gc.timer 2>/dev/null || true
    systemctl stop 'firerunner-registry-front@*' 'firerunner-registry@*' 2>/dev/null || true
    rm -rf "$UNIT_DIR"/firerunner-registry.service.wants "$UNIT_DIR"/firerunner-registry{,-front}@.service{,.d} \
        "$UNIT_DIR"/firerunner-registry-{evict,gc}.{service,timer}
    for svc in firerunner-proxy firerunner flintlockd firerunner-cache firerunner-registry firerunner-dnsmasq firerunner-net containerd-flintlock; do
        systemctl disable --now -q "$svc" 2>/dev/null || true
        rm -f "/etc/systemd/system/$svc.service" "/etc/systemd/system/$svc.service.d/firerunner-proxy.conf"
    done
    systemctl disable --now -q "$REGISTRY_MOUNT_UNIT" 2>/dev/null || true
    rm -f "$UNIT_DIR/$REGISTRY_MOUNT_UNIT"
    rm -f /etc/systemd/system/gitlab-runner.service.d/firerunner-proxy.conf
    # The host stops trusting the company CA it was given.
    if [[ -f /etc/pki/ca-trust/source/anchors/firerunner-ca.pem ]]; then
        rm -f /etc/pki/ca-trust/source/anchors/firerunner-ca.pem && update-ca-trust
    fi
    if [[ -f /usr/local/share/ca-certificates/firerunner-ca.crt ]]; then
        rm -f /usr/local/share/ca-certificates/firerunner-ca.crt && update-ca-certificates >/dev/null
    fi
    systemctl daemon-reload
    [[ -L /usr/bin/firerunner ]] && rm -f /usr/bin/firerunner
    rm -f /etc/sysctl.d/90-firerunner.conf "$BIN_DIR/flintlockd" "$BIN_DIR/firerunner" "$BIN_DIR/registry" \
        "$BIN_DIR/versitygw" "$BIN_DIR/firecracker" "$BIN_DIR/jailer"
    rm -rf "$LIB_DIR" /etc/opt/flintlockd   # flintlockd's config holds the API token
    rm -f "$FLINTLOCK_TLS_MARKER"
    log "done. containerd binaries in $BIN_DIR are kept (another containerd may use them)."
    log "The users $REGISTRY_USER and $CACHE_USER stay (userdel removes them); the registry mirrors' data is in $REGISTRY_IMG."
    log "To also drop data: vgremove $VG && rm -rf $CONTAINERD_ROOT /var/lib/flintlock /var/lib/firerunner $CONF_DIR /etc/lvm/profile/${THINPOOL}.profile"
}

main() {
    case "${1:-install}" in
        install) ;;
        uninstall) uninstall; exit 0 ;;
        *) die "usage: install.sh [install|uninstall]" ;;
    esac
    preflight
    TMP_DIR=$(mktemp -d)
    setup_proxy
    install_packages
    install_containerd
    setup_thinpool
    install_firecracker
    assign_mirror_ports
    setup_network
    install_flintlock
    install_registry_binary
    install_cache_server
    start_services
    apply_vm_disk_size
    install_firerunner
    flintlock_tls_match
    systemctl daemon-reload
    systemctl enable -q firerunner
    if ! systemctl is-active -q firerunner || changed /etc/systemd/system/firerunner.service "$BIN_DIR/firerunner" ||
        [[ $FLINTLOCK_TLS_RESET == 1 || $FLINTLOCK_TLS_DAEMON == 1 ]]; then
        systemctl restart firerunner
    fi
    [[ $FLINTLOCK_TLS_RESET == 1 ]] && rm -f "$FLINTLOCK_TLS_MARKER"
    # After install_firerunner: the mirrors' fronts are `firerunner registry-front`.
    setup_registries
    flintlock_tls_switch
    configure_proxy
    open_metrics_port
    verify_install
    register_runner
    # Host services use the forwarder only once it runs, and gitlab-runner
    # exists only after the registration: their drop-ins come last.
    proxy_dropins_apply
    [[ $STOP_FORWARDER == 1 ]] && stop_forwarder
    configure_runner_cache
    if [[ -n $PENDING ]]; then
        warn "left for a run when no job runs: ${PENDING}- re-run the installer then"
    else
        apply_pending
    fi
    if [[ -n $FLINTLOCK_TLS_LATER ]]; then
        warn "flintlockd's API stays without TLS for now (${FLINTLOCK_TLS_LATER}); re-run the installer when the runner is idle"
    fi
}

main "$@"
