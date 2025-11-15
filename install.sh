#!/bin/bash
set -e

# FireRunner One-Click Installer
# Usage: curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
# Or: wget -qO- https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash

VERSION="v1.0.0"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/firerunner"
FLINTLOCK_CONFIG_DIR="/etc/flintlock"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Logging
log_info() { echo -e "${GREEN}[✓]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[!]${NC} $1"; }
log_error() { echo -e "${RED}[✗]${NC} $1"; }
log_step() { echo -e "${BLUE}[→]${NC} $1"; }

# Banner
show_banner() {
    cat <<'EOF'
╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║           🔥 FireRunner Installer v1.0.0 🔥              ║
║                                                           ║
║     Ephemeral GitLab CI/CD Runners with Firecracker      ║
║                                                           ║
╚═══════════════════════════════════════════════════════════╝
EOF
    echo ""
}

# Check root
check_root() {
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root"
        echo "Usage: curl -sfL https://... | sudo bash"
        exit 1
    fi
}

# Check OS
check_os() {
    log_step "Checking operating system..."

    if [[ ! -f /etc/os-release ]]; then
        log_error "Cannot detect OS"
        exit 1
    fi

    source /etc/os-release

    if [[ "$ID" != "ubuntu" ]] && [[ "$ID" != "debian" ]]; then
        log_warn "OS: $ID $VERSION_ID (tested on Ubuntu 22.04+)"
        read -p "Continue anyway? [y/N] " -n 1 -r
        echo
        [[ ! $REPLY =~ ^[Yy]$ ]] && exit 1
    else
        log_info "OS: $ID $VERSION_ID"
    fi
}

# Check prerequisites
check_prerequisites() {
    log_step "Checking prerequisites..."

    # KVM support
    if [[ ! -e /dev/kvm ]]; then
        log_error "KVM not available (/dev/kvm not found)"
        echo ""
        echo "Enable virtualization in BIOS and run:"
        echo "  sudo modprobe kvm kvm_intel  # Intel"
        echo "  sudo modprobe kvm kvm_amd    # AMD"
        exit 1
    fi
    log_info "KVM support detected"

    # CPU cores
    local cpu_cores=$(nproc)
    if [[ $cpu_cores -lt 4 ]]; then
        log_warn "Only $cpu_cores CPU cores (4+ recommended)"
    else
        log_info "CPU cores: $cpu_cores"
    fi

    # RAM
    local ram_gb=$(free -g | awk '/^Mem:/{print $2}')
    if [[ $ram_gb -lt 16 ]]; then
        log_warn "Only ${ram_gb}GB RAM (16GB+ recommended)"
    else
        log_info "RAM: ${ram_gb}GB"
    fi
}

# Install dependencies
install_dependencies() {
    log_step "Installing dependencies..."

    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq \
        curl \
        wget \
        tar \
        git \
        make \
        jq \
        openssl \
        ca-certificates \
        gnupg >/dev/null 2>&1

    log_info "Dependencies installed"
}

# Install Firecracker
install_firecracker() {
    log_step "Installing Firecracker v1.7.0..."

    if command -v firecracker &>/dev/null; then
        log_info "Firecracker already installed ($(firecracker --version | head -n1))"
        return 0
    fi

    cd /tmp
    curl -sLO https://github.com/firecracker-microvm/firecracker/releases/download/v1.7.0/firecracker-v1.7.0-x86_64.tgz
    tar -xzf firecracker-v1.7.0-x86_64.tgz
    cp release-v1.7.0-x86_64/firecracker-v1.7.0-x86_64 $INSTALL_DIR/firecracker
    chmod +x $INSTALL_DIR/firecracker
    rm -rf firecracker-v1.7.0-x86_64.tgz release-v1.7.0-x86_64

    log_info "Firecracker installed: $(firecracker --version | head -n1)"
}

# Install Flintlock
install_flintlock() {
    log_step "Installing Flintlock v0.6.0..."

    if command -v flintlockd &>/dev/null; then
        log_info "Flintlock already installed"
        return 0
    fi

    cd /tmp
    curl -sLO https://github.com/liquidmetal-dev/flintlock/releases/download/v0.6.0/flintlock-v0.6.0-linux-x86_64.tar.gz
    tar -xzf flintlock-v0.6.0-linux-x86_64.tar.gz
    cp flintlockd $INSTALL_DIR/
    chmod +x $INSTALL_DIR/flintlockd
    rm -rf flintlock-v0.6.0-linux-x86_64.tar.gz flintlockd

    log_info "Flintlock installed"
}

# Configure Flintlock
configure_flintlock() {
    log_step "Configuring Flintlock..."

    mkdir -p $FLINTLOCK_CONFIG_DIR

    # Detect network interface
    local iface=$(ip route | grep default | awk '{print $5}' | head -n1)
    [[ -z "$iface" ]] && iface="eth0"

    cat > $FLINTLOCK_CONFIG_DIR/config.yaml <<EOF
grpc-endpoint: 0.0.0.0:9090
verbosity: debug
parent-iface:
  - name: $iface
EOF

    # Create systemd service
    cat > /etc/systemd/system/flintlock.service <<EOF
[Unit]
Description=Flintlock - MicroVM Management Service
Documentation=https://github.com/liquidmetal-dev/flintlock
After=network.target

[Service]
Type=simple
User=root
ExecStart=$INSTALL_DIR/flintlockd run --config $FLINTLOCK_CONFIG_DIR/config.yaml
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    log_info "Flintlock configured (interface: $iface)"
}

# Install Go
install_go() {
    if command -v go &>/dev/null; then
        log_info "Go already installed: $(go version | awk '{print $3}')"
        return 0
    fi

    log_step "Installing Go 1.21..."

    cd /tmp
    curl -sLO https://go.dev/dl/go1.21.6.linux-amd64.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf go1.21.6.linux-amd64.tar.gz
    rm go1.21.6.linux-amd64.tar.gz

    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile

    log_info "Go installed: $(go version | awk '{print $3}')"
}

# Install FireRunner
install_firerunner() {
    log_step "Installing FireRunner..."

    cd /tmp
    [[ -d firerunner ]] && rm -rf firerunner

    git clone -q https://github.com/ismoilovdevml/firerunner.git
    cd firerunner

    export PATH=$PATH:/usr/local/go/bin
    make build >/dev/null 2>&1

    cp build/firerunner $INSTALL_DIR/
    chmod +x $INSTALL_DIR/firerunner

    log_info "FireRunner installed"

    cd /tmp
    rm -rf firerunner
}

# Interactive configuration
configure_firerunner() {
    log_step "Configuring FireRunner..."

    mkdir -p $CONFIG_DIR

    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  Configuration"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""

    # GitLab URL
    read -p "GitLab URL [https://gitlab.com]: " GITLAB_URL
    GITLAB_URL=${GITLAB_URL:-https://gitlab.com}

    # GitLab Token
    while true; do
        read -p "GitLab API Token (glpat-xxx): " GITLAB_TOKEN
        [[ -n "$GITLAB_TOKEN" ]] && break
        log_error "Token required!"
    done

    # Webhook secret
    read -p "Webhook Secret [auto-generate]: " WEBHOOK_SECRET
    if [[ -z "$WEBHOOK_SECRET" ]]; then
        WEBHOOK_SECRET=$(openssl rand -hex 32)
        log_info "Generated secret: $WEBHOOK_SECRET"
    fi

    # Server IP
    local server_ip=$(curl -s ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}')
    log_info "Server IP: $server_ip"

    # Workers
    read -p "Worker count [5]: " WORKER_COUNT
    WORKER_COUNT=${WORKER_COUNT:-5}

    # Queue size
    read -p "Queue size [100]: " QUEUE_SIZE
    QUEUE_SIZE=${QUEUE_SIZE:-100}

    # Create config
    cat > $CONFIG_DIR/config.yaml <<EOF
server:
  host: "0.0.0.0"
  port: 8080

gitlab:
  url: "$GITLAB_URL"
  token: "$GITLAB_TOKEN"
  webhook_secret: "$WEBHOOK_SECRET"
  runner_tags:
    - firerunner
    - firecracker

flintlock:
  endpoint: "localhost:9090"
  timeout: 30s

vm:
  default_vcpu: 2
  default_memory_mb: 4096
  kernel_image: "ghcr.io/firerunner/kernel:latest"
  rootfs_image: "ghcr.io/firerunner/ubuntu-runner:latest"

scheduler:
  worker_count: $WORKER_COUNT
  queue_size: $QUEUE_SIZE
  job_timeout: 1h
  vm_start_timeout: 5m
  vm_shutdown_timeout: 1m
  cleanup_interval: 5m

logging:
  level: "info"
  format: "json"

metrics:
  enabled: true
  port: 9090
  path: "/metrics"
EOF

    # Save webhook info
    echo "$WEBHOOK_SECRET" > $CONFIG_DIR/.webhook_secret
    echo "http://$server_ip:8080/webhook" > $CONFIG_DIR/.webhook_url
    chmod 600 $CONFIG_DIR/.webhook_secret

    log_info "Config saved: $CONFIG_DIR/config.yaml"
}

# Create FireRunner service
create_firerunner_service() {
    log_step "Creating FireRunner service..."

    cat > /etc/systemd/system/firerunner.service <<EOF
[Unit]
Description=FireRunner - Ephemeral GitLab CI/CD Runners
Documentation=https://github.com/ismoilovdevml/firerunner
After=network.target flintlock.service
Requires=flintlock.service

[Service]
Type=simple
User=root
ExecStart=$INSTALL_DIR/firerunner --config $CONFIG_DIR/config.yaml
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    log_info "Service created"
}

# Start services
start_services() {
    log_step "Starting services..."

    # Start Flintlock
    systemctl enable flintlock >/dev/null 2>&1
    systemctl restart flintlock
    sleep 2

    if systemctl is-active --quiet flintlock; then
        log_info "Flintlock: active"
    else
        log_error "Flintlock failed to start"
        journalctl -u flintlock -n 20 --no-pager
        exit 1
    fi

    # Start FireRunner
    systemctl enable firerunner >/dev/null 2>&1
    systemctl restart firerunner
    sleep 2

    if systemctl is-active --quiet firerunner; then
        log_info "FireRunner: active"
    else
        log_error "FireRunner failed to start"
        journalctl -u firerunner -n 20 --no-pager
        exit 1
    fi
}

# Show completion
show_completion() {
    local webhook_url=$(cat $CONFIG_DIR/.webhook_url 2>/dev/null)
    local webhook_secret=$(cat $CONFIG_DIR/.webhook_secret 2>/dev/null)

    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo -e "║  ${GREEN}✓ Installation Complete!${NC}                              ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
    echo "Services:"
    echo "  • Flintlock:   $(systemctl is-active flintlock)"
    echo "  • FireRunner:  $(systemctl is-active firerunner)"
    echo ""
    echo "Files:"
    echo "  • Binary:   $INSTALL_DIR/firerunner"
    echo "  • Config:   $CONFIG_DIR/config.yaml"
    echo "  • Logs:     journalctl -u firerunner -f"
    echo "  • Metrics:  http://localhost:9090/metrics"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  GitLab Webhook Setup"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "1. Go to: Project → Settings → Webhooks"
    echo ""
    echo "2. Add webhook:"
    echo "   URL:    $webhook_url"
    echo "   Secret: $webhook_secret"
    echo "   Trigger: ✓ Job events"
    echo ""
    echo "3. Test with .gitlab-ci.yml:"
    echo ""
    cat <<'YAML'
   test:
     script:
       - echo "Running in ephemeral VM!"
     tags:
       - firecracker-2cpu-4gb
YAML
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "Commands:"
    echo "  • Logs:    journalctl -u firerunner -f"
    echo "  • Status:  systemctl status firerunner"
    echo "  • Restart: systemctl restart firerunner"
    echo "  • Stop:    systemctl stop firerunner"
    echo ""
    echo "Documentation: https://github.com/ismoilovdevml/firerunner"
    echo ""
}

# Main
main() {
    show_banner
    check_root
    check_os
    check_prerequisites
    install_dependencies

    install_firecracker
    install_flintlock
    configure_flintlock

    install_go
    install_firerunner

    configure_firerunner
    create_firerunner_service

    start_services
    show_completion
}

# Run
main "$@"
