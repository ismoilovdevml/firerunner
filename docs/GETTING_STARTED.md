# FireRunner - Getting Started Guide

**EASY 5-STEP SETUP** - Complete setup in 30 minutes

## Prerequisites

Before starting, you need:
- ✅ Ubuntu 22.04 LTS server (bare metal or VM with nested virt)
- ✅ Root/sudo access
- ✅ 16GB+ RAM, 4+ CPU cores
- ✅ GitLab instance (self-hosted or GitLab.com)
- ✅ GitLab API token

**Check if your system supports KVM:**
```bash
# This should return a number (CPU cores)
egrep -c '(vmx|svm)' /proc/cpuinfo

# This file should exist
ls /dev/kvm
```

---

## Step 1: Install Firecracker (5 min)

```bash
# Download Firecracker
FIRECRACKER_VERSION="v1.7.0"
curl -LOJ https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz

# Extract and install
tar -xzf firecracker-${FIRECRACKER_VERSION}-x86_64.tgz
sudo cp release-${FIRECRACKER_VERSION}-x86_64/firecracker-${FIRECRACKER_VERSION}-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

# Verify
firecracker --version
```

**Expected output:**
```
Firecracker v1.7.0
```

---

## Step 2: Install Flintlock (5 min)

```bash
# Download Flintlock
FLINTLOCK_VERSION="v0.6.0"
curl -LOJ https://github.com/liquidmetal-dev/flintlock/releases/download/${FLINTLOCK_VERSION}/flintlock-${FLINTLOCK_VERSION}-linux-x86_64.tar.gz

# Extract and install
tar -xzf flintlock-${FLINTLOCK_VERSION}-linux-x86_64.tar.gz
sudo cp flintlockd /usr/local/bin/
sudo chmod +x /usr/local/bin/flintlockd

# Create config directory
sudo mkdir -p /etc/flintlock

# Create config file
sudo tee /etc/flintlock/config.yaml > /dev/null <<EOF
grpc-endpoint: 0.0.0.0:9090
verbosity: info
parent-iface:
  - name: $(ip route | grep default | awk '{print $5}')  # Auto-detect network interface
EOF

# Create systemd service
sudo tee /etc/systemd/system/flintlock.service > /dev/null <<EOF
[Unit]
Description=Flintlock MicroVM Service
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/flintlockd run --config /etc/flintlock/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# Start Flintlock
sudo systemctl daemon-reload
sudo systemctl enable flintlock
sudo systemctl start flintlock

# Verify
sudo systemctl status flintlock
```

**Expected output:**
```
● flintlock.service - Flintlock MicroVM Service
   Active: active (running)
```

---

## Step 3: Install FireRunner (10 min)

### Option A: Download Binary (Recommended)

```bash
# Download latest release
FIRERUNNER_VERSION="v0.1.0"
curl -LOJ https://github.com/ismoilovdevml/firerunner/releases/download/${FIRERUNNER_VERSION}/firerunner-${FIRERUNNER_VERSION}-linux-amd64.tar.gz

# Extract
tar -xzf firerunner-${FIRERUNNER_VERSION}-linux-amd64.tar.gz

# Install
sudo cp firerunner /usr/local/bin/
sudo chmod +x /usr/local/bin/firerunner
```

### Option B: Build from Source

```bash
# Install Go 1.24+
sudo add-apt-repository ppa:longsleep/golang-backports
sudo apt update
sudo apt install -y golang-go

# Clone repository
git clone https://github.com/ismoilovdevml/firerunner.git
cd firerunner

# Build
make build

# Install
sudo cp build/firerunner /usr/local/bin/
```

### Configure FireRunner

```bash
# Create config directory
sudo mkdir -p /etc/firerunner

# Get your GitLab API token
# 1. Go to GitLab → Settings → Access Tokens
# 2. Create token with scopes: api, read_api, write_repository
# 3. Copy the token

# Create config
sudo tee /etc/firerunner/config.yaml > /dev/null <<EOF
server:
  host: "0.0.0.0"
  port: 8080

gitlab:
  url: "https://gitlab.com"  # Or your GitLab instance URL
  token: "YOUR_GITLAB_TOKEN_HERE"  # Replace with your token
  webhook_secret: "$(openssl rand -hex 32)"  # Auto-generated secret

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

# IMPORTANT: Replace YOUR_GITLAB_TOKEN_HERE with your actual token!
sudo nano /etc/firerunner/config.yaml
```

### Create Systemd Service

```bash
sudo tee /etc/systemd/system/firerunner.service > /dev/null <<EOF
[Unit]
Description=FireRunner - GitLab Firecracker Runner
After=network.target flintlock.service
Requires=flintlock.service

[Service]
Type=simple
ExecStart=/usr/local/bin/firerunner --config /etc/firerunner/config.yaml
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

# Start FireRunner
sudo systemctl daemon-reload
sudo systemctl enable firerunner
sudo systemctl start firerunner

# Check status
sudo systemctl status firerunner
```

**Expected output:**
```
● firerunner.service - FireRunner - GitLab Firecracker Runner
   Active: active (running)
```

**View logs:**
```bash
sudo journalctl -u firerunner -f
```

---

## Step 4: Setup GitLab Webhook (5 min)

### 4.1 Find Your Server IP

```bash
# Get public IP
curl ifconfig.me

# Or use local IP if GitLab is on same network
hostname -I | awk '{print $1}'
```

### 4.2 Configure Webhook in GitLab

1. **Go to your GitLab project**
   - Navigate to: `Settings → Webhooks`

2. **Add webhook:**
   - **URL**: `http://YOUR_SERVER_IP:8080/webhook`
   - **Secret Token**: (copy from `/etc/firerunner/config.yaml` → `webhook_secret`)
   - **Trigger**: ☑️ Job events
   - Click **Add webhook**

3. **Test webhook:**
   - Click **Test → Job events**
   - Should see: `HTTP 200` ✅

### 4.3 Verify FireRunner Received Webhook

```bash
# Check logs
sudo journalctl -u firerunner -n 50 | grep webhook

# Should see:
# "Received webhook event" event_type="Job Hook"
```

---

## Step 5: Run Your First Job! (5 min)

### 5.1 Add `.gitlab-ci.yml` to Your Project

Create `.gitlab-ci.yml` in your repository root:

```yaml
# Simple test job
test:
  script:
    - echo "Hello from FireRunner!"
    - echo "Running on $(hostname)"
    - docker --version
  tags:
    - firecracker-2cpu-4gb  # ← FireRunner tag
```

### 5.2 Commit and Push

```bash
git add .gitlab-ci.yml
git commit -m "Add FireRunner CI/CD"
git push
```

### 5.3 Watch the Magic! ✨

1. **Go to GitLab**: `CI/CD → Pipelines`
2. **You should see**:
   - Pipeline running
   - Job pending → running
   - FireRunner creating VM (check logs)

**Monitor FireRunner:**
```bash
# Watch logs in real-time
sudo journalctl -u firerunner -f

# You should see:
# "Creating MicroVM for job"
# "MicroVM created successfully" duration=800ms
# "Job processing completed"
```

---

## Troubleshooting

### Issue: "Flintlock connection refused"

```bash
# Check Flintlock is running
sudo systemctl status flintlock

# Check port is open
sudo netstat -tlnp | grep 9090

# Restart if needed
sudo systemctl restart flintlock
```

### Issue: "Webhook not received"

```bash
# Check FireRunner is listening
sudo netstat -tlnp | grep 8080

# Check firewall
sudo ufw allow 8080/tcp

# Test webhook manually
curl -X POST http://localhost:8080/health
# Should return: {"status":"healthy"}
```

### Issue: "VM creation failed"

```bash
# Check /dev/kvm exists
ls -l /dev/kvm

# Check images are accessible
docker pull ghcr.io/liquidmetal-dev/flintlock-kernel:5.10
docker pull ghcr.io/ismoilovdevml/firerunner-rootfs:latest

# Check Flintlock logs
sudo journalctl -u flintlock -n 100
```

### Issue: "GitLab runner not registered"

This is expected in current version! The runner registration is TODO.

**Workaround for now:**
1. VM is created ✅
2. VM gets IP ✅
3. Runner registration → Coming soon in v0.2.0

---

## Next Steps

### 1. Build Custom VM Images

Follow: [images/BUILD.md](../images/BUILD.md)

### 2. Add More Workers

Edit `/etc/firerunner/config.yaml`:
```yaml
scheduler:
  worker_count: 10  # Increase for more parallel jobs
```

Restart:
```bash
sudo systemctl restart firerunner
```

### 3. Monitor Performance

```bash
# Metrics endpoint
curl http://localhost:9090/metrics

# Setup Prometheus + Grafana (optional)
```

### 4. Production Hardening

- [ ] Enable TLS
- [ ] Setup firewall rules
- [ ] Configure resource limits
- [ ] Add monitoring alerts
- [ ] Backup configuration
- [ ] Setup log rotation

---

## Uninstall

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
```

---

## Support

- 📖 [Documentation](https://github.com/ismoilovdevml/firerunner)
- 🐛 [Report Issues](https://github.com/ismoilovdevml/firerunner/issues)
- 💬 [Discussions](https://github.com/ismoilovdevml/firerunner/discussions)

**Congratulations! FireRunner is now running! 🔥🚀**
