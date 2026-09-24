# Configuration

FireRunner reads `/etc/firerunner/config.yaml` (override with `FIRERUNNER_CONFIG`). The installer
writes it with defaults. Change settings with the CLI — values are validated and unknown keys are
rejected:

```bash
sudo firerunner config show
sudo firerunner config set vm.memory_mb 4096
sudo firerunner config get pool.size
```

Most changes apply to the **next job** without a restart; the daemon reloads the file within 2 s
and replaces pool VMs that were booted with the old settings.

## Keys

### microVM (`vm.*`)

| Key | Default | Notes |
|---|---|---|
| `vm.vcpu` | `2` | 1–32 |
| `vm.memory_mb` | `2048` | 256–65536 |
| `vm.boot_timeout` | `3m` | time to DHCP lease + SSH, min 10s |
| `vm.rootfs_image` | `ghcr.io/ismoilovdevml/firerunner-rootfs:latest` | guest root filesystem (OCI image) |
| `vm.kernel_image` | `ghcr.io/ismoilovdevml/firerunner-kernel:6.18.53` | guest kernel (OCI image, `/boot/vmlinux`), built from [`images/kernel`](https://github.com/ismoilovdevml/firerunner/tree/main/images/kernel) |
| `vm.kernel_cmdline.<arg>` | none | extra kernel arguments; remove with `config unset` |
| `vm.registry_mirror` | `http://<subnet>.1:5000` | Docker Hub mirror written to the guest's daemon.json |
| `vm.docker_bip` | `10.201.0.1/24` | guest Docker default bridge |
| `vm.docker_address_pool` | `10.202.0.0/16` | guest Docker user networks |
| `vm.host_reserve_mb` | `1024` | host memory never promised to microVMs |

### Warm pool (`pool.*`)

| Key | Default | Notes |
|---|---|---|
| `pool.size` | `2` | pre-booted microVMs; 0 disables the pool |
| `pool.max_idle` | `30m` | idle pool VMs are replaced after this |
| `pool.preload_images` | `[]` | images pulled into pool VMs in advance, e.g. `[mcr.microsoft.com/dotnet/sdk:8.0, node:22]` |

```bash
sudo firerunner config set pool.preload_images "[mcr.microsoft.com/dotnet/sdk:8.0, node:22-alpine]"
```

### Docker layer cache (`builder.*`)

| Key | Default | Notes |
|---|---|---|
| `builder.enabled` | `true` | one BuildKit microVM per GitLab project for `docker build` in jobs without `image:` |
| `builder.vcpu` | `4` | builds run here, not in the job VM |
| `builder.memory_mb` | `8192` | counted in memory admission like any microVM; Node.js sizes its heap from RAM, so frontend builds fail with less |
| `builder.max` | `4` | builders kept at once; the least recently used idle one is replaced |
| `builder.idle_ttl` | `24h` | a builder unused this long is deleted with its cache |
| `builder.cache_mb` | `25000` | BuildKit garbage-collects above this |
| `builder.image` | `moby/buildkit:v0.33.0` | pulled through the Docker Hub mirror |
| `builder.port_base` | `20000` | job VMs reach builder *n* at `<subnet>.1:port_base+n` |

A new `builder.vcpu` / `builder.memory_mb` applies to builders started afterwards, so resizing
never throws away warm caches (`firerunner builder rm <project>` applies it at once). A new
`builder.image`, `builder.cache_mb` or guest image replaces each builder after 10 idle minutes.

### Daemon (`daemon.*`)

| Key | Default | Notes |
|---|---|---|
| `daemon.metrics_listen` | `127.0.0.1:9477` | `:9477` when installed with `FR_METRICS_ALLOW` (restart needed) |
| `daemon.socket` | `/run/firerunner/daemon.sock` | local API used by the executor |
| `daemon.reconcile_interval` | `1m` | orphan cleanup period |
| `daemon.job_max_age` | `3h` | VMs of jobs older than this are deleted |

### flintlock and network

| Key | Default |
|---|---|
| `flintlock.endpoint` | `127.0.0.1:9090` |
| `flintlock.token_file` | `/etc/firerunner/flintlock.token` |
| `flintlock.namespace` | `firerunner` |
| `network.leases_file` | `/var/lib/misc/firerunner-dnsmasq.leases` |
| `network.ssh_key` | `/etc/firerunner/executor/id_ed25519` |

## gitlab-runner settings

The runner lives in `/etc/gitlab-runner/config.toml`. Use the CLI for the common setting:

```bash
sudo firerunner runner concurrent 6     # max parallel jobs on this host
```

## Files on the host

| Path | Content |
|---|---|
| `/etc/firerunner/config.yaml` | FireRunner settings |
| `/etc/firerunner/flintlock.token` | flintlock API token (0600) |
| `/etc/opt/flintlockd/config.yaml` | flintlockd settings incl. token (0600) |
| `/etc/firerunner/executor/` | SSH key the executor uses to reach job VMs |
| `/run/firerunner/jobs/` | one state file per running job |
| `/run/firerunner/known_hosts/` | pinned host key per microVM |
| `/run/firerunner/pool.json` | warm pool, kept across daemon restarts |
