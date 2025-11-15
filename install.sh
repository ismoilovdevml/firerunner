#!/bin/bash
set -e

# FireRunner Automated Installer
# This script installs FireRunner and all dependencies on Ubuntu 22.04+

VERSION="v0.2.0"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/firerunner"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Print functions
print_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

print_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_header() {
    echo ""
    echo "=================================="
    echo "  FireRunner Installer ${VERSION}"
    echo "=================================="
    echo ""
}

# Check if running as root
check_root() {
    if [[ $EUID -ne 0 ]]; then
       print_error "This script must be run as root (use sudo)"
       exit 1
    fi
}

# Check system requirements
check_requirements() {
    print_info "Checking system requirements..."

    # Check OS
    if [ ! -f /etc/os-release ]; then
        print_error "Cannot determine OS. Ubuntu 22.04+ required."
        exit 1
    fi

    . /etc/os-release
    if [[ "$ID" != "ubuntu" ]] || [[ "${VERSION_ID%%.*}" -lt 22 ]]; then
        print_warn "Ubuntu 22.04+ recommended. Current: $ID $VERSION_ID"
    fi

    # Check KVM support
    if [ ! -e /dev/kvm ]; then
        print_error "/dev/kvm not found. Virtualization support required."
        print_info "Enable VT-x/AMD-V in BIOS and run: sudo modprobe kvm kvm_intel"
        exit 1
    fi

    # Check CPU cores
    CPU_CORES=$(nproc)
    if [ "$CPU_CORES" -lt 4 ]; then
        print_warn "Only $CPU_CORES CPU cores detected. 4+ recommended."
    fi

    # Check RAM
    TOTAL_RAM=$(free -g | awk '/^Mem:/{print $2}')
    if [ "$TOTAL_RAM" -lt 8 ]; then
        print_warn "Only ${TOTAL_RAM}GB RAM detected. 16GB+ recommended."
    fi

    print_info "✓ System requirements check passed"
}

# Install dependencies
install_dependencies() {
    print_info "Installing dependencies..."

    apt-get update -qq
    apt-get install -y -qq \
        curl \
        wget \
        tar \
        git \
        jq \
        ca-certificates \
        gnupg \
        lsb-release

    print_info "✓ Dependencies installed"
}

# Install Firecracker
install_firecracker() {
    print_info "Installing Firecracker..."

    FC_VERSION="v1.7.0"
    FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz"

    cd /tmp
    wget -q "$FC_URL"
    tar -xzf "firecracker-${FC_VERSION}-x86_64.tgz"
    cp "release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" "${INSTALL_DIR}/firecracker"
    chmod +x "${INSTALL_DIR}/firecracker"
    rm -rf "firecracker-${FC_VERSION}-x86_64.tgz" "release-${FC_VERSION}-x86_64"

    print_info "✓ Firecracker installed: $(firecracker --version)"
}

# Install Flintlock
install_flintlock() {
    print_info "Installing Flintlock..."

    FL_VERSION="v0.9.0"
    FL_URL="https://github.com/liquidmetal-dev/flintlock/releases/download/${FL_VERSION}/flintlock-${FL_VERSION}-linux-x86_64.tar.gz"

    cd /tmp
    wget -q "$FL_URL"
    tar -xzf "flintlock-${FL_VERSION}-linux-x86_64.tar.gz"
    cp flintlockd "${INSTALL_DIR}/"
    chmod +x "${INSTALL_DIR}/flintlockd"
    rm -f "flintlock-${FL_VERSION}-linux-x86_64.tar.gz" flintlockd

    # Create Flintlock config
    mkdir -p /etc/flintlock
    cat > /etc/flintlock/config.yaml <<EOF
grpc-endpoint: 0.0.0.0:9090
verbosity: info
parent-iface:
  - name: $(ip route | grep default | awk '{print $5}')
EOF

    # Create systemd service
    cat > /etc/systemd/system/flintlock.service <<EOF
[Unit]
Description=Flintlock MicroVM Service
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/flintlockd run --config /etc/flintlock/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable flintlock
    systemctl start flintlock

    print_info "✓ Flintlock installed and started"
}

# Install FireRunner
install_firerunner() {
    print_info "Installing FireRunner..."

    FR_VERSION="v0.2.0"
    FR_URL="https://github.com/ismoilovdevml/firerunner/releases/download/${FR_VERSION}/firerunner-${FR_VERSION}-linux-amd64.tar.gz"

    cd /tmp
    # For now, build from source since releases don't exist yet
    if ! command -v go &> /dev/null; then
        print_info "Installing Go..."
        GO_VERSION="1.24.0"
        wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
        rm -rf /usr/local/go
        tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"
        export PATH=$PATH:/usr/local/go/bin
        rm "go${GO_VERSION}.linux-amd64.tar.gz"
    fi

    # Clone and build
    cd /tmp
    rm -rf firerunner
    git clone -q https://github.com/ismoilovdevml/firerunner.git
    cd firerunner
    make build
    cp build/firerunner "${INSTALL_DIR}/"
    chmod +x "${INSTALL_DIR}/firerunner"
    cd /tmp
    rm -rf firerunner

    print_info "✓ FireRunner installed"
}

# Configure FireRunner
configure_firerunner() {
    print_info "Configuring FireRunner..."

    mkdir -p "$CONFIG_DIR"

    # Interactive configuration
    echo ""
    read -p "Enter your GitLab URL (default: https://gitlab.com): " GITLAB_URL
    GITLAB_URL=${GITLAB_URL:-https://gitlab.com}

    read -p "Enter your GitLab API token: " GITLAB_TOKEN
    if [ -z "$GITLAB_TOKEN" ]; then
        print_error "GitLab token is required!"
        exit 1
    fi

    # Generate webhook secret
    WEBHOOK_SECRET=$(openssl rand -hex 32)

    # Create config file
    cat > "${CONFIG_DIR}/config.yaml" <<EOF
server:
  host: "0.0.0.0"
  port: 8080

gitlab:
  url: "${GITLAB_URL}"
  token: "${GITLAB_TOKEN}"
  webhook_secret: "${WEBHOOK_SECRET}"

flintlock:
  endpoint: "localhost:9090"

vm:
  default_vcpu: 2
  default_memory_mb: 4096
  kernel_image: "ghcr.io/liquidmetal-dev/flintlock-kernel:5.10"
  rootfs_image: "ghcr.io/ismoilovdevml/firerunner-rootfs:latest"

scheduler:
  worker_count: 5
  queue_size: 100

logging:
  level: "info"
  format: "json"
EOF

    print_info "✓ Configuration created at ${CONFIG_DIR}/config.yaml"
    print_info "✓ Webhook secret: ${WEBHOOK_SECRET}"
    echo "${WEBHOOK_SECRET}" > "${CONFIG_DIR}/webhook-secret.txt"
    chmod 600 "${CONFIG_DIR}/webhook-secret.txt"
}

# Create systemd service for FireRunner
create_systemd_service() {
    print_info "Creating FireRunner systemd service..."

    cat > /etc/systemd/system/firerunner.service <<EOF
[Unit]
Description=FireRunner - GitLab Firecracker Runner
After=network.target flintlock.service
Requires=flintlock.service

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/firerunner --config ${CONFIG_DIR}/config.yaml
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable firerunner
    systemctl start firerunner

    print_info "✓ FireRunner service created and started"
}

# Print final instructions
print_final_instructions() {
    PUBLIC_IP=$(curl -s ifconfig.me || echo "YOUR_SERVER_IP")
    WEBHOOK_SECRET=$(cat "${CONFIG_DIR}/webhook-secret.txt")

    cat <<EOF

${GREEN}╔════════════════════════════════════════════════════════════╗
║  FireRunner Installation Complete! 🎉                      ║
╚════════════════════════════════════════════════════════════╝${NC}

${GREEN}Next Steps:${NC}

1. Configure GitLab Webhook:
   - Go to your GitLab project → Settings → Webhooks
   - URL: http://${PUBLIC_IP}:8080/webhook
   - Secret Token: ${WEBHOOK_SECRET}
   - Trigger: ☑️ Job events
   - Click "Add webhook"

2. Add .gitlab-ci.yml to your project:
   test:
     script:
       - echo "Hello from FireRunner!"
     tags:
       - firecracker-2cpu-4gb

3. Check status:
   sudo systemctl status firerunner
   sudo journalctl -u firerunner -f

4. View metrics:
   curl http://localhost:9090/metrics

${YELLOW}Important Notes:${NC}
- Webhook secret saved to: ${CONFIG_DIR}/webhook-secret.txt
- Logs: sudo journalctl -u firerunner -f
- Config: ${CONFIG_DIR}/config.yaml

${GREEN}Support:${NC}
- Documentation: https://github.com/ismoilovdevml/firerunner
- Issues: https://github.com/ismoilovdevml/firerunner/issues

EOF
}

# Main installation flow
main() {
    print_header
    check_root
    check_requirements
    install_dependencies
    install_firecracker
    install_flintlock
    install_firerunner
    configure_firerunner
    create_systemd_service
    print_final_instructions
}

# Run main function
main

exit 0
