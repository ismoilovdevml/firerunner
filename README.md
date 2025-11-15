# FireRunner 🔥

**Ephemeral GitLab CI/CD runners using Firecracker microVMs**

Open-source alternative to Actuated. Each CI job runs in an isolated, ephemeral Firecracker VM that boots in <1 second and destroys immediately after completion.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Test Coverage](https://img.shields.io/badge/Coverage-65%25-brightgreen)](pkg/)
![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)

## Quick Install

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

This installs:
- Firecracker v1.7.0
- Flintlock v0.6.0
- FireRunner v1.0.0
- Systemd services (auto-start)
- Interactive configuration

**Manual installation:** See [Installation](#installation) section below.

## Architecture

Matches the [Actuated workflow](https://actuated.com):

```
┌─────────────────┐      2) Webhook       ┌──────────────────────┐
│  Self-hosted    │───────────────────────>│  FireRunner Service  │
│  GitLab         │<───────────────────────│  (Controller)        │
│                 │  3) Register runner    │                      │
└────────┬────────┘                        └──────────┬───────────┘
         │                                            │
         │ 1) git push                                │ 4) Create VM
         │                                            ↓
    ┌────┴─────┐                            ┌─────────────────────┐
    │ Project  │                            │  Server with KVM    │
    └──────────┘                            │  ┌───────────────┐  │
         ↑                                  │  │  Ephemeral    │  │
         │                                  │  │  Runner 🔥    │  │
         └──────────────5) Start job────────┤  └───────────────┘  │
                                            └─────────────────────┘
```

**Workflow:**
1. Developer pushes code → GitLab
2. GitLab sends webhook → FireRunner
3. FireRunner registers ephemeral runner → GitLab API
4. FireRunner creates VM via Flintlock → Firecracker
5. GitLab starts job → ephemeral runner in VM
6. Job completes → Runner unregistered → VM destroyed

## Production Status

### ✅ Production Ready (v1.0.0)

**Core orchestration layer:**
- Webhook handler (HMAC-SHA256, rate limiting, IP whitelist)
- Job scheduler (worker pool, context-based cancellation)
- GitLab API integration (real runner registration/cleanup)
- Job monitoring (GitLab API polling, 5s intervals)
- Configuration management (YAML + ENV validation)
- Prometheus metrics
- 65% test coverage, race detector clean

**Code references:**
- `pkg/gitlab/webhook_handler.go` - Secure webhook processing
- `pkg/scheduler/scheduler.go` - Job scheduling, worker pool
- `pkg/gitlab/service.go` - Real GitLab API (RegisterRunner, UnregisterRunner)
- `pkg/gitlab/job_monitor.go` - Real-time job status monitoring

### ⚠️ Requires Setup

**Infrastructure dependencies:**
1. **Flintlock server** - VM provisioning backend (install separately)
2. **VM images** - Kernel + rootfs with GitLab Runner pre-installed
3. **SSH automation** - Runner installation/configuration (TODO: `scheduler.go:434`)

**Current limitation:**
- FireRunner registers runners via GitLab API ✅
- Assumes runner binary is pre-installed in VM image
- Production needs: SSH into VM → install runner → configure token

## Quick Start

### Prerequisites

- Bare metal server or VM with nested virtualization
- Ubuntu 22.04 LTS
- KVM support (`/dev/kvm`)
- 16GB+ RAM, 4+ cores
- GitLab instance (self-hosted or GitLab.com)

### Installation

**1. Install Firecracker**
```bash
curl -LOJ https://github.com/firecracker-microvm/firecracker/releases/download/v1.7.0/firecracker-v1.7.0-x86_64.tgz
tar -xzf firecracker-v1.7.0-x86_64.tgz
sudo cp release-v1.7.0-x86_64/firecracker-v1.7.0-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker
```

**2. Install Flintlock**
```bash
curl -LOJ https://github.com/liquidmetal-dev/flintlock/releases/download/v0.6.0/flintlock-v0.6.0-linux-x86_64.tar.gz
tar -xzf flintlock-v0.6.0-linux-x86_64.tar.gz
sudo cp flintlockd /usr/local/bin/
sudo chmod +x /usr/local/bin/flintlockd
```

**3. Configure Flintlock**
```bash
sudo mkdir -p /etc/flintlock
cat <<EOF | sudo tee /etc/flintlock/config.yaml
grpc-endpoint: 0.0.0.0:9090
verbosity: debug
parent-iface:
  - name: eth0
EOF
```

**4. Start Flintlock**
```bash
sudo flintlockd run --config /etc/flintlock/config.yaml &
```

**5. Install FireRunner**
```bash
git clone https://github.com/ismoilovdevml/firerunner.git
cd firerunner
make build
sudo cp build/firerunner /usr/local/bin/
```

**6. Configure FireRunner**
```bash
sudo mkdir -p /etc/firerunner
cat <<EOF | sudo tee /etc/firerunner/config.yaml
server:
  host: "0.0.0.0"
  port: 8080

gitlab:
  url: "https://gitlab.example.com"
  token: "glpat-xxxxxxxxxxxx"  # GitLab API token
  webhook_secret: "your-secret-token"

flintlock:
  endpoint: "localhost:9090"
  timeout: 30s

vm:
  default_vcpu: 2
  default_memory_mb: 4096
  kernel_image: "ghcr.io/firerunner/kernel:latest"
  rootfs_image: "ghcr.io/firerunner/ubuntu-runner:latest"

scheduler:
  worker_count: 5
  queue_size: 100
  job_timeout: 1h
EOF
```

**7. Start FireRunner**
```bash
firerunner --config /etc/firerunner/config.yaml
```

### GitLab Webhook Setup

1. Navigate to: **Project → Settings → Webhooks**
2. Add webhook:
   - **URL**: `http://your-server-ip:8080/webhook`
   - **Secret Token**: (same as `webhook_secret` in config)
   - **Trigger**: ✅ Job events only
3. Save webhook

## Usage

### Basic Pipeline

```yaml
# .gitlab-ci.yml
test:
  script:
    - make test
  tags:
    - firecracker-2cpu-4gb
```

### Resource Allocation

Tag format: `firecracker-{CPU}cpu-{RAM}gb`

```yaml
build:
  script:
    - cargo build --release
  tags:
    - firecracker-8cpu-16gb  # Large build
```

### How It Works

1. You push code → GitLab
2. GitLab webhook → FireRunner at `/webhook`
3. FireRunner parses job requirements from tags
4. Creates VM via Flintlock (2 vCPU, 4GB RAM)
5. Registers ephemeral runner → GitLab API
6. GitLab assigns job → runner in VM
7. Job executes → completes
8. FireRunner unregisters runner → destroys VM

**Job lifecycle (scheduler.go:341-387):**
```go
processJob() {
    createVM()           // pkg/firecracker/manager.go
    registerRunner()     // pkg/gitlab/service.go:35
    waitForJobCompletion() // pkg/gitlab/job_monitor.go:32
    cleanupVM()          // Unregister + destroy
}
```

## Development

**Build**
```bash
make build
```

**Test**
```bash
make test  # 65% coverage, race detector clean
```

**Run locally**
```bash
# Terminal 1: Flintlock
flintlockd run --config /etc/flintlock/config.yaml

# Terminal 2: FireRunner
go run cmd/firerunner/main.go --config /etc/firerunner/config.yaml
```

## Monitoring

Prometheus metrics on `:9090/metrics`:

```bash
curl http://localhost:9090/metrics | grep firerunner
```

Key metrics:
- `firerunner_jobs_total{status="success|failed"}`
- `firerunner_vms_active`
- `firerunner_job_duration_seconds`
- `firerunner_queue_size`

Grafana dashboards: `config/grafana/dashboards/*.json`

## Production Deployment

### Systemd Service

```bash
sudo tee /etc/systemd/system/firerunner.service <<EOF
[Unit]
Description=FireRunner - Ephemeral GitLab Runners
After=network.target flintlock.service

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/firerunner --config /etc/firerunner/config.yaml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now firerunner
```

### VM Image Requirements

Your rootfs image must include:
- Ubuntu 22.04 base
- GitLab Runner binary pre-installed
- SSH server (for future automation)

**Current assumption**: Runner auto-configures with token from cloud-init

**TODO (scheduler.go:434-443)**:
```go
// Production implementation should:
// - SSH into VM (job.VM.IPAddress)
// - Install gitlab-runner (if not in image)
// - Configure: gitlab-runner register --token $TOKEN
// - Start: systemctl start gitlab-runner
```

## Testing

**Unit tests**
```bash
make test
# Output:
# pkg/scheduler: 86.2% coverage
# pkg/firecracker: 67.7% coverage
# pkg/gitlab: 26.4% coverage
# pkg/config: 50.0% coverage
```

**Integration test** (requires Flintlock running)
```bash
# 1. Start Flintlock
flintlockd run --config /etc/flintlock/config.yaml &

# 2. Start FireRunner
firerunner --config /etc/firerunner/config.yaml &

# 3. Trigger webhook manually
curl -X POST http://localhost:8080/webhook \
  -H "X-Gitlab-Event: Job Hook" \
  -H "X-Gitlab-Token: your-webhook-secret" \
  -d @test/fixtures/job_event.json

# 4. Check logs
tail -f /var/log/firerunner.log
```

## Troubleshooting

**Webhook not triggering**
- Check GitLab webhook logs: Project → Settings → Webhooks → Recent Deliveries
- Verify secret token matches config
- Check firewall allows port 8080

**VM creation fails**
- Verify Flintlock is running: `curl localhost:9090`
- Check KVM support: `ls -l /dev/kvm`
- Review Flintlock logs: `journalctl -u flintlock -f`

**Runner not picking up jobs**
- Check runner registered: GitLab → Settings → CI/CD → Runners
- Verify tags match: `firecracker-*` tags in .gitlab-ci.yml
- Check VM has network connectivity

**High memory usage**
- Reduce `scheduler.worker_count` in config
- Lower `vm.default_memory_mb` for jobs
- Implement VM cleanup: `scheduler.cleanup_interval`

## Roadmap

- [ ] SSH automation for runner installation (scheduler.go:434)
- [ ] Cloud-init integration for token injection
- [ ] Multi-tenancy (namespace isolation)
- [ ] Custom VM image builder
- [ ] High availability (leader election)
- [ ] Metrics dashboard (pre-built Grafana)

## License

Apache License 2.0 - see [LICENSE](LICENSE)

## Credits

- [Actuated](https://actuated.com) - Architecture inspiration
- [Firecracker](https://github.com/firecracker-microvm/firecracker) - microVM technology
- [Flintlock](https://github.com/liquidmetal-dev/flintlock) - VM management
- [GitLab](https://gitlab.com) - CI/CD platform

---

**Production-ready orchestration layer. VM provisioning requires Flintlock server setup.**
