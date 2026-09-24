# FireRunner

**Every GitLab CI job in its own fresh Firecracker microVM, on your own hardware.**

FireRunner is a [GitLab Runner custom executor](https://docs.gitlab.com/runner/executors/custom/).
gitlab-runner picks up jobs as usual. For each job FireRunner hands out a clean microVM — its
own kernel, root filesystem, Docker daemon and network identity — runs every stage of the job
inside it, and deletes it when the job ends. Nothing survives between jobs.

A daemon keeps a pool of pre-booted microVMs, so a job normally gets its VM in **~0.3 s**
instead of a 15–17 s cold boot.

![FireRunner Grafana dashboard](images/grafana.png)

## Who this documentation is for

<div class="grid cards" markdown>

- **CI users** — engineers writing `.gitlab-ci.yml`.
  Start with [Writing jobs](user-guide/writing-jobs.md).
- **Operators** — the people who run the runner hosts.
  Start with [Requirements](getting-started/requirements.md) and [Install](getting-started/install.md).

</div>

## Quick start

```bash
# 1. On a Linux host with /dev/kvm (bare metal, or a VM with nested virtualization)
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash

# 2. Create a runner in GitLab (tag: firecracker) and register it
sudo firerunner runner register --url https://gitlab.example.com --token glrt-...

# 3. Check everything
sudo firerunner doctor
```

```yaml
# 4. Use it in .gitlab-ci.yml
test:
  tags: [firecracker]
  image: python:3.12-alpine
  script:
    - python -m unittest -v
```

## Why microVMs

| | Shell executor | Docker executor | **FireRunner** |
|---|---|---|---|
| Job isolation | none (shared host) | containers on a shared kernel and Docker daemon | **separate VM and kernel per job** |
| Leftovers between jobs | files, processes, Docker cache | Docker cache, volumes | **none — the VM is deleted** |
| `docker build` / Docker-in-Docker | host Docker | privileged DinD or socket mount | **native Docker in the VM, no privileged mode** |
| Start time | instant | image pull | **~0.3 s** from the warm pool |

Measured results are on the [Capacity and performance](operator-guide/capacity.md) page.
