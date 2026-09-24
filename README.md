# FireRunner

Run every GitLab CI job in its own Firecracker microVM, on your own hardware.

[![CI](https://github.com/ismoilovdevml/firerunner/actions/workflows/ci.yml/badge.svg)](https://github.com/ismoilovdevml/firerunner/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-read-orange)](https://ismoilovdevml.github.io/firerunner/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

- **Isolated.** Each job gets a new VM with its own kernel and Docker daemon. The VM is deleted when the job ends.
- **Fast.** Pre-booted VMs start a job in 0.3 s. `docker build` keeps a per-project layer cache.
- **Drop-in.** A GitLab Runner [custom executor](https://docs.gitlab.com/runner/executors/custom/): `image:`, `services:`, `cache:` and artifacts work as usual.

![Architecture](docs/images/architecture.svg)

## Install

On a Linux host with `/dev/kvm` and a blank disk:

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
sudo firerunner runner register --url https://gitlab.example.com --token glrt-...
```

## Use

```yaml
test:
  tags: [firecracker]
  image: node:22        # optional; without it the script runs on the VM
  services: [postgres:16]
  script:
    - npm test
```

## Measured

Same commit, same pipeline, next to the existing runners:

| | FireRunner | Existing runner |
|---|---|---|
| Job waits for its VM | 0.3 s | — |
| `dotnet test` (44 tests) | 27.7 s | 31.1 s (docker) |
| `docker build`, warm cache | 4.8 s | 2.0 s (shell) |

## Documentation

[ismoilovdevml.github.io/firerunner](https://ismoilovdevml.github.io/firerunner/): install, writing jobs, configuration, operations and security.

Apache 2.0
