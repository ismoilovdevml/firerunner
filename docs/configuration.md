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
| `vm.vcpu` | host CPUs / runner `concurrent` (2 to 16), set by the installer | per job VM |
| `vm.memory_mb` | `2048` | per job VM |
| `vm.job_max_vcpu`, `vm.job_max_memory_mb` | `0` | the largest VM a job may ask for (see below); `0` = `vm.vcpu` and `vm.memory_mb`, so jobs cannot ask for more |
| `pool.size` | runner `concurrent`, set by the installer | pre-booted VMs; a job that finds one does not wait for a boot (about 10 s) |
| `pool.max_idle` | `30m` | idle pool VMs are replaced after this |
| `pool.preload_images` | none | images pulled into pool VMs in advance, e.g. a large SDK |
| `pool.preload_top` | `0` (off) | also pull the N images the recent jobs used most (at least 2 of the last 200 jobs), see below |
| `vm.host_reserve_mb` | `1024` | host memory never given to VMs |

Set `pool.size` to the number of jobs that usually start at once: a job that finds the pool empty
waits for a boot. Each pool VM holds `vm.memory_mb` of host memory while it waits.

A shell executor's job gets every CPU of the host. vCPUs are threads on the host and an idle one
costs nothing, so the installer gives each VM the host's CPUs divided by the jobs that run at once.

A job whose `image:` is already in its pool VM skips the pull (10 to 20 s for a large image).
`pool.preload_top` picks those images from what the recent jobs ran in, so they need no list:

```bash
sudo firerunner config set pool.preload_top 3
```

Pool VMs pull without the jobs' credentials, so an image that needs a login is never preloaded
(it is skipped for a day after its first failed pull), and a job gets no image it could not pull
itself. A job can see the names of the public images other projects use, though: leave it off
where that matters. Preloading uses the pool VM's disk and makes its boot longer; a job that
arrives meanwhile takes the VM and the preload stops.

Keep `vm.vcpu` and `vm.memory_mb` at what most jobs need and let the few heavy jobs (static analysis,
large test suites) ask for more with job variables, up to `vm.job_max_vcpu` and
`vm.job_max_memory_mb`:

```bash
sudo firerunner config set vm.job_max_memory_mb 4096
sudo firerunner config set vm.job_max_vcpu 4
```

Such a job boots its own VM (pool VMs have the default size), so it waits for memory like any cold
boot and counts its own size.

## Builders (Docker layer cache)

| Key | Default | |
|---|---|---|
| `builder.enabled` | `true` | one builder VM per project that runs `docker build` |
| `builder.vcpu`, `builder.memory_mb` | `4`, `8192` | Node.js frontend builds need 4 GB or more |
| `builder.max` | `4` | the least recently used builder idle for 5 minutes makes room for a new one; while none is, a build goes without a cache |
| `builder.idle_ttl` | `24h` | an unused builder is deleted; its cache is saved |
| `builder.saved_cache_gb` | `100` | host disk for the caches of deleted builders; `0` saves none |

When a builder is deleted because it was idle, its slot was needed, it was a week old, or its
settings changed (not its size), its cache is copied to
`/var/lib/firerunner/builder-cache/<project>.tar` and loaded into the project's next builder. So
every project keeps a warm cache while only `builder.max` builders use memory. When the saved
caches pass `builder.saved_cache_gb`, or the disk has less than 10% free, the least recently used
are deleted. `firerunner builder rm` deletes a project's saved cache too.

A saved cache names the builder image it came from and is only loaded into a builder of the same
image. A builder deleted because `builder.image` changed is not copied out, and a cache of another
image is dropped when the next builder starts. Caches saved by versions that did not record the
image were all written by `moby/buildkit:v0.33.0`: they are still loaded while `builder.image` is that
image, and dropped otherwise.

A new builder size applies to builders started later; existing caches are kept.

## Sizing a host

A VM starts only when its memory fits. Jobs that do not fit wait, so the host never runs out of memory.

```text
((jobs × vm.memory_mb) + (pool.size × vm.memory_mb) + (builder.max × builder.memory_mb)) × 1.05 ≤ RAM − 1 GB
```

`1.05` adds Firecracker's own memory, 5% per VM; 1 GB is `vm.host_reserve_mb`.

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
| `vm.registry_mirror` | `http://<bridge>:5000` | Docker Hub's pull-through mirror on the host |
| `vm.registry_mirrors` | one per registry in `FR_REGISTRY_MIRRORS` | pull-through mirrors of other registries, e.g. `{"ghcr.io": "http://10.200.0.1:5001"}`; the installer writes it on every run |

## Registry mirrors

Every job starts with an empty image store, so each `image:`, `services:` and `docker pull`
downloads its image again. The host keeps a pull-through mirror of Docker Hub and, by default, of
`ghcr.io`, `quay.io`, `registry.k8s.io` and `mcr.microsoft.com` (`FR_REGISTRY_MIRRORS` at install
time), so a layer crosses the internet once per host instead of once per job. Job VMs and builders
use them without any change to `.gitlab-ci.yml`.

A pull the mirror cannot serve goes to the registry itself: private images (the mirror has no
credentials, so it never holds another project's private image), a registry the mirror cannot
reach, or a mirror that is down. Other registries' mirrors need the guest's Docker to use the
containerd image store, the default of Docker 29 in the FireRunner image.

## Corporate networks

Set these with the installer options described in [Corporate networks](corporate-network.md);
the keys are:

| Key | Default | |
|---|---|---|
| `proxy.enabled` | `false` | microVMs and host services use the corporate proxy through the forwarder |
| `proxy.listen` | `10.200.0.1:3128` | the forwarder on the bridge address (and 127.0.0.1) |
| `proxy.upstream_file` | `/etc/firerunner/proxy-upstream` | root-only file with `http://[user:password@]host:port` |
| `proxy.no_proxy` | none | hosts, `.domains` and CIDRs reached directly; local addresses are always added |
| `proxy.connect_ports` | `[443]` | ports HTTPS tunnels through the proxy may go to |
| `network.egress_deny` | none | networks jobs must not reach, directly or through the proxy (installer: `FR_EGRESS_DENY`) |
| `vm.ca_file` | none | PEM root CA trusted by microVMs, their Docker, builders and job containers |
| `vm.insecure_registries` | none | `host:port` (TLS without a check) or `http://host:port` (plain HTTP) |
