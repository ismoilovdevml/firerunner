# Configuration

Settings live in `/etc/firerunner/config.yaml`. Change them with the CLI; values are checked and
most take effect for the next job, without a restart:

```bash
sudo firerunner config show
sudo firerunner config set vm.memory_mb 3072
```

## Job VMs and pool

| Key | Default | |
|---|---|---|
| `vm.vcpu` | `2` | per job VM |
| `vm.memory_mb` | `2048` | per job VM |
| `pool.size` | `2` | pre-booted VMs; a job takes one in 0.3 s instead of a 15 s boot |
| `pool.max_idle` | `30m` | idle pool VMs are replaced after this |
| `pool.preload_images` | none | images pulled into pool VMs in advance, e.g. a large SDK |
| `vm.host_reserve_mb` | `1024` | host memory never given to VMs |

Set `pool.size` to the number of jobs that usually start at once.

## Builders (Docker layer cache)

| Key | Default | |
|---|---|---|
| `builder.enabled` | `true` | one builder VM per project that runs `docker build` |
| `builder.vcpu`, `builder.memory_mb` | `4`, `8192` | Node.js frontend builds need 4 GB or more |
| `builder.max` | `4` | the least recently used idle builder makes room for a new one |
| `builder.idle_ttl` | `24h` | an unused builder is deleted with its cache |

A new builder size applies to builders started later; existing caches are kept.

## Sizing a host

A VM starts only when its memory fits. Jobs that do not fit wait, so the host never runs out of memory.

```text
(jobs × vm.memory_mb) + (pool.size × vm.memory_mb) + (builder.max × builder.memory_mb) ≤ RAM − 1 GB
```

For example, 64 GB fits 10 jobs and 4 pool VMs at 1.5 GB plus 4 builders at 8 GB.
Set the parallel jobs with `sudo firerunner runner concurrent 10`.

CPU is shared, not reserved: give VMs more vCPUs than the host has cores.

## Other keys

`firerunner config keys` lists all of them. Worth knowing:

| Key | Default | |
|---|---|---|
| `daemon.metrics_listen` | `127.0.0.1:9477` | Prometheus endpoint |
| `daemon.job_max_age` | `3h` | VMs of jobs older than this are deleted |
| `vm.docker_bip`, `vm.docker_address_pool` | `10.201.0.1/24`, `10.202.0.0/16` | Docker networks in the VM; change them if they clash with your LAN |
