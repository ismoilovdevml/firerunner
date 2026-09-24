# Capacity and performance

## How many jobs fit on a host

FireRunner never over-commits memory: a microVM boots only when

```text
sum(memory of all microVMs) × 1.05  +  new VM × 1.05  ≤  MemTotal − vm.host_reserve_mb
```

Jobs that do not fit wait (`waiting for host memory …` in the job log) instead of making the host
swap or OOM. Size the host with:

```text
max VMs ≈ (host RAM − reserve) / (vm.memory_mb × 1.05)
runner concurrent + pool.size  ≤  max VMs      (recommended)
```

| Host RAM | VM size | Max VMs | Suggested concurrent / pool |
|---|---|---|---|
| 16 GB | 2 GB | 7 | 4 / 2 |
| 64 GB | 2 GB | 29 | 16 / 8 |
| 64 GB | 4 GB | 14 | 8 / 4 |
| 256 GB | 4 GB | 60 | 32 / 16 |

CPU is over-committed like any hypervisor; plan ~1 physical core per busy job VM for CPU-heavy builds.
Disk: each job VM uses copy-on-write space in the thin pool; 100 GB is comfortable for ~20 parallel jobs.

## Warm pool

`pool.size` VMs are booted ahead of time. A job claims one in ~0.3 s; the daemon boots a
replacement in the background. Set `pool.size` to roughly the number of jobs that typically start
at the same moment (the parallel jobs of one pipeline stage). Pool VMs are never reused — each serves
exactly one job.

`pool.preload_images` pulls images into pool VMs while they are idle. Use it for large images your
jobs use a lot (e.g. .NET or JDK SDKs), especially from registries other than Docker Hub.

## Memory deduplication (KSM)

A microVM keeps every page it has touched: after a build or an image preload its full
`memory_mb` is in use on the host. The installer turns on kernel samepage merging and starts
flintlockd through `firerunner ksm-exec`, so all Firecracker processes are merge candidates:
the same kernel, rootfs and base images in many VMs are stored once. Check the effect with
`cat /sys/kernel/mm/ksm/general_profit` (bytes saved) or `pages_sharing`.

## Builders

Each GitLab project that runs `docker build` gets a builder microVM (`builder.vcpu`,
`builder.memory_mb`), kept for `builder.idle_ttl` after its last job. Budget
`builder.max × builder.memory_mb` on top of jobs and pool, for example on 64 GB:
8 jobs × 2 GB + 2 pool × 2 GB + 4 builders × 8 GB ≈ 52 GB. Builds run in the builders, so
give them the memory your builds need (a Node.js frontend build needs 4+ GB). Builders keep up to
`builder.cache_mb` of layers each in the thin pool.

## Measured results

Test host: 8 vCPU, 16 GB RAM, a VMware VM with nested virtualization. Same commit, same pipeline,
jobs run side by side with the organisation's existing runners.

![Benchmarks](../images/benchmarks.svg)

| | FireRunner | Existing runner |
|---|---|---|
| Job waits for its VM (p50) | 0.25 s (pool) · 16.7 s (cold) | — |
| .NET service, `dotnet test` (44 tests), `image: mcr.microsoft.com/dotnet/sdk:8.0` | **27.7 s** | 31.1 s (docker executor) |
| Same, cold VM, SDK image pulled | 104.3 s | — |
| Same `dotnet test` with NuGet packages in `cache:` | **25.5 s** (32.4 s without) | 23.4 s (docker executor) |
| `docker build` of the same service | 24.6–31 s | 1–2.4 s (shell executor, warm layer cache) |
| Same build with the BuildKit cache in `cache:` | build 10–11 s + 8 s cache transfer (367 MB) | |
| Same build on the project's warm builder | **2 s** build, 4.8 s job | 2.0 s job (shell executor) |
| 6-job pipeline | 60 s no pool · 38 s pool 2 · **19 s** pool 4 | — |

The `docker build` gap is the cost of isolation: shell and docker runners reuse the host's layer
cache between jobs, a FireRunner job has to download and upload its cache
([Writing jobs](../user-guide/writing-jobs.md#docker-builds)). See
[#25](https://github.com/ismoilovdevml/firerunner/issues/25).
