# FireRunner

Ephemeral GitLab CI/CD runners with Firecracker microVMs.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev)
[![Coverage](https://img.shields.io/badge/Coverage-65%25-brightgreen)](pkg/)

## Install

**Requires bare metal server** (KVM support)

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

Supports: Ubuntu 22.04+, Debian 11+, Rocky Linux 9+, RHEL 9+

## Architecture

```text
┌─────────────────────────────────────────────────────────────────┐
│                    Actuated-style Workflow                       │
│                                                                  │
│  1) git push → GitLab Project                                   │
│                      │                                           │
│                      ▼                                           │
│  2) Webhook → FireRunner (Actuated Controller)                  │
│                      │                                           │
│                      ├─ 3) Add new runner to Project            │
│                      │                                           │
│                      ▼                                           │
│  4) Start runner → Server with KVM                              │
│                      │                                           │
│                      ├─ Flintlock (gRPC)                        │
│                      │                                           │
│                      ▼                                           │
│                   Firecracker                                    │
│                      │                                           │
│                      ▼                                           │
│              Ephemeral Runner 🔥                                 │
│                      │                                           │
│  5) Start job ←──────┘                                          │
│                      │                                           │
│                      ▼                                           │
│              Job Execution                                       │
│                      │                                           │
│                      ▼                                           │
│              VM Destroyed                                        │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

## Configuration

GitLab webhook: `Project → Settings → Webhooks`

- URL: `http://your-server:8080/webhook`
- Secret: From `/etc/firerunner/.webhook_secret`
- Trigger: Job events

## Usage

`.gitlab-ci.yml`:

```yaml
build:
  script:
    - make build
  tags:
    - firecracker-4cpu-8gb
```

Tag format: `firecracker-{CPU}cpu-{RAM}gb`

## Files

```text
/usr/local/bin/firerunner      # Binary
/etc/firerunner/config.yaml    # Config
/etc/firerunner/.webhook_secret # Secret
```

Config example:

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
systemctl status firerunner    # Status
journalctl -u firerunner -f    # Logs
systemctl restart firerunner   # Restart
curl localhost:9090/metrics    # Metrics
```

## Development

```bash
make build  # Build
make test   # Test (65% coverage, race-free)
```

## Prerequisites

**Bare Metal Required:**

- KVM (`/dev/kvm`)
- 16GB+ RAM, 4+ cores
- Ubuntu 22.04+ / Debian 11+ / Rocky 9+ / RHEL 9+

**Supported:**

- ✅ Hetzner Dedicated, OVH Bare Metal, AWS i3.metal

**NOT Supported:**

- ❌ VPS/Cloud VMs (no KVM)

## License

Apache License 2.0
