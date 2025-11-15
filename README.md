# FireRunner

Ephemeral GitLab CI/CD runners with Firecracker microVMs. Open-source alternative to Actuated.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev)
[![Coverage](https://img.shields.io/badge/Coverage-65%25-brightgreen)](pkg/)

## Features

- **Sub-second VM provisioning** - Firecracker boots in <1s
- **True isolation** - KVM-based microVMs per job
- **Ephemeral** - VMs destroyed after each job
- **Dynamic resources** - CPU/RAM per job via tags
- **Production-ready** - 65% test coverage, race-free

## Install

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

Supports: Ubuntu 22.04+, Debian 11+, Rocky Linux 9+, RHEL 9+

## Architecture

```
GitLab (webhook) → FireRunner → Flintlock → Firecracker VM
                                          ↓
                                   Ephemeral Runner
                                          ↓
                                   Job Execution
                                          ↓
                                   VM Destroyed
```

**Workflow:**
1. Push code → GitLab triggers webhook
2. FireRunner registers ephemeral runner (GitLab API)
3. Creates VM via Flintlock (gRPC)
4. GitLab assigns job to runner in VM
5. Job completes → Runner unregistered → VM destroyed

## Configuration

After install, configure webhook in GitLab:

**Project → Settings → Webhooks:**
- URL: `http://your-server:8080/webhook`
- Secret: From `/etc/firerunner/.webhook_secret`
- Trigger: Job events

## Usage

**.gitlab-ci.yml:**

```yaml
build:
  script:
    - make build
  tags:
    - firecracker-4cpu-8gb  # 4 vCPU, 8GB RAM
```

**Tag format:** `firecracker-{CPU}cpu-{RAM}gb`

**Examples:**
- `firecracker-2cpu-4gb` - Small jobs (tests)
- `firecracker-8cpu-16gb` - Large jobs (builds)

## Components

**FireRunner** (this repo)
- Webhook handler (HMAC-SHA256, rate limiting)
- Job scheduler (worker pool, queue-based)
- GitLab API integration (runner registration/cleanup)
- Real-time job monitoring

**Flintlock** (external)
- gRPC-based microVM management
- Installed by script

**Firecracker** (external)
- AWS's microVM technology
- Installed by script

## Files

```
/usr/local/bin/firerunner      # Binary
/etc/firerunner/config.yaml    # Config
/etc/firerunner/.webhook_secret # GitLab webhook secret
```

**Config example:**

```yaml
gitlab:
  url: "https://gitlab.com"
  token: "glpat-xxxxxxxxxxxx"
  webhook_secret: "your-secret"

flintlock:
  endpoint: "localhost:9090"

vm:
  default_vcpu: 2
  default_memory_mb: 4096

scheduler:
  worker_count: 5
  queue_size: 100
  job_timeout: 1h
```

## Commands

```bash
# Status
systemctl status firerunner

# Logs
journalctl -u firerunner -f

# Restart
systemctl restart firerunner

# Metrics
curl localhost:9090/metrics
```

## Development

**Build:**
```bash
make build
```

**Test:**
```bash
make test  # 65% coverage, race detector clean
```

**Run locally:**
```bash
# Terminal 1: Flintlock
flintlockd run --config /etc/flintlock/config.yaml

# Terminal 2: FireRunner
go run cmd/firerunner/main.go --config config.yaml
```

## Production Status

**Ready:**
- Webhook handler ✓
- Job scheduler (86.2% coverage) ✓
- GitLab API (real registration/cleanup) ✓
- Job monitoring (API polling) ✓
- Prometheus metrics ✓

**Requires setup:**
- Flintlock server (installed by script)
- VM images (kernel + rootfs)
- Runner installation in VM (TODO: SSH automation)

## Prerequisites

- Bare metal or nested virtualization
- KVM support (`/dev/kvm`)
- 16GB+ RAM, 4+ cores
- GitLab instance

## Troubleshooting

**Webhook not working:**
```bash
# Check logs
journalctl -u firerunner -n 50

# Test webhook
curl -X POST http://localhost:8080/webhook \
  -H "X-Gitlab-Event: Job Hook" \
  -H "X-Gitlab-Token: $(cat /etc/firerunner/.webhook_secret)"
```

**VM creation fails:**
```bash
# Check Flintlock
systemctl status flintlock
journalctl -u flintlock -f

# Check KVM
ls -l /dev/kvm
```

**Service won't start:**
```bash
# Check config
cat /etc/firerunner/config.yaml

# Validate GitLab token
curl -H "PRIVATE-TOKEN: your-token" https://gitlab.com/api/v4/user
```

## Metrics

```bash
curl localhost:9090/metrics | grep firerunner

# Key metrics:
# firerunner_jobs_total{status="success|failed"}
# firerunner_vms_active
# firerunner_job_duration_seconds
# firerunner_queue_size
```

## Code Structure

```
pkg/
├── config/       # YAML + ENV config (50% coverage)
├── firecracker/  # VM lifecycle (67.7% coverage)
├── gitlab/       # API + webhook (26.4% coverage)
└── scheduler/    # Job orchestration (86.2% coverage)

cmd/firerunner/   # Main entry point
```

## Testing

**Unit tests:**
```bash
go test -v -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**Integration test:**
Requires Flintlock running on localhost:9090

## Monitoring

**Grafana dashboards:** `config/grafana/dashboards/*.json`

**Prometheus config:**
```yaml
scrape_configs:
  - job_name: firerunner
    static_configs:
      - targets: ['localhost:9090']
```

## License

Apache License 2.0

## Credits

- [Actuated](https://actuated.com) - Architecture inspiration
- [Firecracker](https://github.com/firecracker-microvm/firecracker) - microVM technology
- [Flintlock](https://github.com/liquidmetal-dev/flintlock) - VM management
- [GitLab](https://gitlab.com) - CI/CD platform

---

**One-line install. Production-ready orchestration. Ephemeral by design.**
