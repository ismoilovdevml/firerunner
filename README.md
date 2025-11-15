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

## How It Works

```text
┌────────────────────────────────────────────────────────────────────────┐
│                                                                        │
│  Developer                    FireRunner                 Server/KVM    │
│                                                                        │
│                                                                        │
│  ┌─────────┐                                            ┌──────────┐  │
│  │ GitLab  │                                            │ Flintlock│  │
│  │ Project │                                            │  Server  │  │
│  └────┬────┘                                            └────┬─────┘  │
│       │                                                      │         │
│       │ 1) git push                                          │         │
│       │                                                      │         │
│       ▼                                                      │         │
│  ┌─────────────┐                                            │         │
│  │   GitLab    │                                            │         │
│  │   Server    │                                            │         │
│  └──────┬──────┘                                            │         │
│         │                                                    │         │
│         │ 2) Webhook (Job Event)                            │         │
│         │                                                    │         │
│         ▼                                                    │         │
│  ┌──────────────────┐                                       │         │
│  │   FireRunner     │                                       │         │
│  │  (Controller)    │                                       │         │
│  │                  │                                       │         │
│  │ • Webhook Auth   │                                       │         │
│  │ • Job Queue      │                                       │         │
│  │ • Scheduler      │                                       │         │
│  └────┬─────────────┘                                       │         │
│       │                                                      │         │
│       │ 3) Register Runner (GitLab API)                     │         │
│       │────────────────────────────────────────────────────▶│         │
│       │                                                      │         │
│       │ 4) Create VM (gRPC)                                 │         │
│       │──────────────────────────────────────────────────────────────▶│
│       │                                                      │         │
│       │                                    ┌────────────────────────┐ │
│       │                                    │   Firecracker VM       │ │
│       │                                    │                        │ │
│       │                                    │  ┌──────────────────┐  │ │
│       │                                    │  │ GitLab Runner 🔥 │  │ │
│       │                                    │  │                  │  │ │
│       │ 5) Start Job                       │  │ • Execute Job    │  │ │
│       │◀───────────────────────────────────│──│ • Build/Test     │  │ │
│       │                                    │  │ • Report Results │  │ │
│       │ 6) Monitor Status                  │  └──────────────────┘  │ │
│       │◀───────────────────────────────────│                        │ │
│       │                                    │  (Ephemeral - <1s)     │ │
│       │                                    └────────────────────────┘ │
│       │                                                      │         │
│       │ 7) Job Complete                                     │         │
│       │                                                      │         │
│       │ 8) Unregister Runner (GitLab API)                   │         │
│       │────────────────────────────────────────────────────▶│         │
│       │                                                      │         │
│       │ 9) Destroy VM (gRPC)                                │         │
│       │──────────────────────────────────────────────────────────────▶│
│       │                                                      │         │
│       ▼                                                      ▼         │
│   Cleanup Done                                      VM Destroyed      │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘

Key Features:
• Sub-second VM boot (Firecracker)
• True isolation (KVM-based microVMs)
• Ephemeral runners (destroyed after each job)
• Dynamic resources (CPU/RAM via tags)
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

Examples:

- `firecracker-2cpu-4gb` - Small jobs (tests)
- `firecracker-4cpu-8gb` - Medium jobs (builds)
- `firecracker-8cpu-16gb` - Large jobs (heavy builds)

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

## Current Status

**Production Ready:** Core orchestration (webhook, scheduler, GitLab API)

**Development Mode:** VM creation (mock Flintlock for testing)

To enable real VM creation:

1. Install Flintlock on your server
2. Update `pkg/firecracker/client.go` with real gRPC calls
3. Build VM images with GitLab Runner pre-installed

## License

Apache License 2.0
