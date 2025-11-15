# FireRunner Installation Guide

## One-Line Install (Recommended)

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

### What It Does

1. **Checks Prerequisites:**
   - OS: Ubuntu 22.04+ / Debian
   - KVM support (`/dev/kvm`)
   - CPU: 4+ cores (recommended)
   - RAM: 16GB+ (recommended)

2. **Installs Dependencies:**
   - curl, wget, tar, git, make
   - openssl, jq, ca-certificates

3. **Installs Firecracker:**
   - Version: v1.7.0
   - Binary: `/usr/local/bin/firecracker`
   - Skips if already installed

4. **Installs Flintlock:**
   - Version: v0.6.0
   - Binary: `/usr/local/bin/flintlockd`
   - Config: `/etc/flintlock/config.yaml`
   - Systemd service: `flintlock.service`

5. **Installs Go (if needed):**
   - Version: 1.21.6
   - Path: `/usr/local/go`

6. **Builds & Installs FireRunner:**
   - Clones repo: `https://github.com/ismoilovdevml/firerunner.git`
   - Builds from source
   - Binary: `/usr/local/bin/firerunner`

7. **Interactive Configuration:**
   - Prompts for:
     - GitLab URL
     - GitLab API Token
     - Webhook Secret (auto-generated)
     - Worker count
     - Queue size
   - Creates: `/etc/firerunner/config.yaml`

8. **Creates Systemd Services:**
   - `flintlock.service` - VM management
   - `firerunner.service` - Job orchestration

9. **Starts Services:**
   - Enables auto-start on boot
   - Validates services are running

10. **Shows Completion:**
    - Webhook URL and secret
    - GitLab configuration steps
    - Useful commands

## Example Installation Session

```bash
$ curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash

╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║           🔥 FireRunner Installer v1.0.0 🔥              ║
║                                                           ║
║     Ephemeral GitLab CI/CD Runners with Firecracker      ║
║                                                           ║
╚═══════════════════════════════════════════════════════════╝

[✓] OS: ubuntu 22.04
[→] Checking prerequisites...
[✓] KVM support detected
[✓] CPU cores: 8
[✓] RAM: 32GB
[→] Installing dependencies...
[✓] Dependencies installed
[→] Installing Firecracker v1.7.0...
[✓] Firecracker installed: v1.7.0
[→] Installing Flintlock v0.6.0...
[✓] Flintlock installed
[→] Configuring Flintlock...
[✓] Flintlock configured (interface: ens5)
[→] Installing Go 1.21...
[✓] Go installed: go1.21.6
[→] Installing FireRunner...
[✓] FireRunner installed
[→] Configuring FireRunner...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Configuration
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

GitLab URL [https://gitlab.com]: https://gitlab.example.com
GitLab API Token (glpat-xxx): glpat-xxxxxxxxxxxxxxxxxxxx
Webhook Secret [auto-generate]:
[✓] Generated secret: 4a3b2c1d...
[✓] Server IP: 192.168.1.100
Worker count [5]: 5
Queue size [100]: 100
[✓] Config saved: /etc/firerunner/config.yaml
[→] Creating FireRunner service...
[✓] Service created
[→] Starting services...
[✓] Flintlock: active
[✓] FireRunner: active

╔═══════════════════════════════════════════════════════════╗
║  ✓ Installation Complete!                                 ║
╚═══════════════════════════════════════════════════════════╝

Services:
  • Flintlock:   active
  • FireRunner:  active

Files:
  • Binary:   /usr/local/bin/firerunner
  • Config:   /etc/firerunner/config.yaml
  • Logs:     journalctl -u firerunner -f
  • Metrics:  http://localhost:9090/metrics

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  GitLab Webhook Setup
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

1. Go to: Project → Settings → Webhooks

2. Add webhook:
   URL:    http://192.168.1.100:8080/webhook
   Secret: 4a3b2c1d...
   Trigger: ✓ Job events

3. Test with .gitlab-ci.yml:

   test:
     script:
       - echo "Running in ephemeral VM!"
     tags:
       - firecracker-2cpu-4gb

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Commands:
  • Logs:    journalctl -u firerunner -f
  • Status:  systemctl status firerunner
  • Restart: systemctl restart firerunner
  • Stop:    systemctl stop firerunner

Documentation: https://github.com/ismoilovdevml/firerunner
```

## Alternative: wget

```bash
wget -qO- https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

## Environment Variables

Customize installation:

```bash
# Custom install directory
INSTALL_DIR=/opt/firerunner \
  curl -sfL https://.../install.sh | sudo bash

# Custom config directory
CONFIG_DIR=/opt/firerunner/config \
  curl -sfL https://.../install.sh | sudo bash
```

## Non-Interactive Mode

For automated deployments, pre-create config:

```bash
# 1. Create config directory
sudo mkdir -p /etc/firerunner

# 2. Create config file
sudo tee /etc/firerunner/config.yaml <<EOF
gitlab:
  url: "https://gitlab.com"
  token: "glpat-xxxxxxxxxxxx"
  webhook_secret: "your-secret"
# ... rest of config
EOF

# 3. Run installer (will skip interactive config if file exists)
curl -sfL https://.../install.sh | sudo bash
```

## Post-Installation

### 1. Verify Services

```bash
# Check status
systemctl status firerunner
systemctl status flintlock

# View logs
journalctl -u firerunner -f
journalctl -u flintlock -f
```

### 2. Configure GitLab Webhook

1. Go to GitLab project
2. Navigate to: **Settings → Webhooks**
3. Add webhook:
   - URL: From installer output
   - Secret: From installer output or `/etc/firerunner/.webhook_secret`
   - Trigger: Job events only
4. Click **Add webhook**
5. Click **Test → Job events** to verify

### 3. Test with GitLab CI

Create `.gitlab-ci.yml`:

```yaml
test:
  script:
    - echo "Hello from FireRunner!"
    - echo "CPU cores: $(nproc)"
    - echo "RAM: $(free -h | grep Mem | awk '{print $2}')"
  tags:
    - firecracker-2cpu-4gb
```

Push to GitLab and watch the job run in an ephemeral VM!

### 4. Monitor Metrics

```bash
# Prometheus metrics
curl http://localhost:9090/metrics | grep firerunner

# Key metrics
curl -s http://localhost:9090/metrics | grep -E "firerunner_(jobs|vms|queue)"
```

## Troubleshooting

### Installation Fails

**Check logs:**
```bash
# If during build
journalctl -u firerunner -n 50 --no-pager

# If Flintlock fails
journalctl -u flintlock -n 50 --no-pager
```

**Common issues:**

1. **KVM not available:**
   ```bash
   # Enable KVM module
   sudo modprobe kvm kvm_intel  # Intel
   sudo modprobe kvm kvm_amd    # AMD

   # Verify
   ls -l /dev/kvm
   ```

2. **Insufficient permissions:**
   ```bash
   # Must run with sudo
   curl -sfL https://.../install.sh | sudo bash
   ```

3. **Network issues:**
   ```bash
   # Check connectivity
   curl -I https://github.com
   curl -I https://gitlab.com
   ```

### Service Won't Start

```bash
# Check logs
journalctl -u firerunner -n 50

# Common causes:
# 1. GitLab token invalid
# 2. Port 8080 already in use
# 3. Flintlock not running

# Restart services
systemctl restart flintlock
systemctl restart firerunner
```

### Webhook Not Working

1. **Check GitLab webhook logs:**
   - Project → Settings → Webhooks → Recent Deliveries

2. **Verify secret matches:**
   ```bash
   cat /etc/firerunner/.webhook_secret
   ```

3. **Check firewall:**
   ```bash
   # Allow port 8080
   sudo ufw allow 8080/tcp
   ```

4. **Test webhook manually:**
   ```bash
   curl -X POST http://localhost:8080/webhook \
     -H "X-Gitlab-Event: Job Hook" \
     -H "X-Gitlab-Token: $(cat /etc/firerunner/.webhook_secret)" \
     -d '{}'
   ```

## Manual Installation

If you prefer manual installation, see [README.md](README.md#installation).

## Uninstallation

```bash
# Stop services
sudo systemctl stop firerunner flintlock
sudo systemctl disable firerunner flintlock

# Remove binaries
sudo rm /usr/local/bin/firerunner
sudo rm /usr/local/bin/flintlockd
sudo rm /usr/local/bin/firecracker

# Remove configs
sudo rm -rf /etc/firerunner
sudo rm -rf /etc/flintlock

# Remove systemd services
sudo rm /etc/systemd/system/firerunner.service
sudo rm /etc/systemd/system/flintlock.service
sudo systemctl daemon-reload

# Optional: Remove Go
sudo rm -rf /usr/local/go
```

## Next Steps

- [Read documentation](https://github.com/ismoilovdevml/firerunner)
- [Configure VM images](README.md#vm-image-requirements)
- [Setup monitoring](README.md#monitoring)
- [Production deployment](README.md#production-deployment)

---

**Installation takes ~5 minutes on a 100 Mbps connection**
