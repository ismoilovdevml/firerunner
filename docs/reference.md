# Reference

## CLI

All commands need root.

| Command | |
|---|---|
| `firerunner status` · `doctor` | overview · every check with a hint |
| `firerunner config show` · `keys` · `path` · `get <key>` · `set <key> <value>` | settings; `path` prints the config file |
| `firerunner config unset vm.kernel_cmdline.<arg>` | remove a guest kernel argument |
| `firerunner runner register --url URL --token - [--concurrent N] [--name NAME]` | register this host; the `glrt-...` token is read from stdin (or `FIRERUNNER_RUNNER_TOKEN`), never from the command line; the name shown in GitLab defaults to `firerunner-<hostname>` |
| `firerunner runner status` · `concurrent N` · `unregister` | the GitLab runner |
| `firerunner runner cache [local \| off \| s3 --server H:P --bucket B [--insecure]]` | where `cache:` is stored; `--insecure` uses plain HTTP |
| `firerunner vm list` · `logs <id> [-n N]` · `rm <id> \| --all` | microVMs; `logs` prints the last N lines of a VM's console (default 100) |
| `firerunner pool [refresh]` | pre-booted VMs; refresh replaces them |
| `firerunner builder list` · `rm <project> \| --all [--force]` | builders; a builder in use is kept unless `--force` |
| `firerunner run [--keep] -- <command>` | run a command in a throwaway VM |
| `firerunner upgrade [--version V] [--check]` | update the binary |
| `firerunner proxy` | the corporate proxy forwarder (systemd: `firerunner-proxy`), see [Corporate networks](corporate-network.md) |
| `firerunner version` | |

`firerunner daemon` and `firerunner executor …` are run by systemd and gitlab-runner.

## Metrics

`http://<host>:9477/metrics`. `/healthz` on the same port fails when the daemon's main loop has not
passed for 2 minutes.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `firerunner_build_info` | gauge | `version` | always 1; the running version |
| `firerunner_loop_last_tick_timestamp_seconds` | gauge | | when the daemon's main loop last completed a pass (every 2 s) |
| `firerunner_pool_target` | gauge | | `pool.size`: pre-booted VMs wanted |
| `firerunner_pool_ready` | gauge | | pre-booted VMs ready to be claimed |
| `firerunner_pool_booting` | gauge | | pool VMs booting now |
| `firerunner_pool_claims_total` | counter | `result`: `hit`, `miss` | claim requests; a job waiting for memory asks again every few seconds, so misses are not jobs: the share of jobs that got a pool VM is `firerunner_job_prepare_seconds_count` by `source` |
| `firerunner_pool_preload_seconds` | histogram | | time to pull `pool.preload_images` into a pool VM |
| `firerunner_admission_waits_total` | counter | | pool refill passes (every 2 s) that could not boot every missing pool VM because host memory was short |
| `firerunner_jobs_running` | gauge | | jobs in progress on the host (the state files prepare writes and cleanup removes) |
| `firerunner_job_prepare_seconds` | histogram | `source`: `pool`, `cold` | how long a job waited until its microVM was ready: a claimed pool VM or its own boot; failed prepares are not observed |
| `firerunner_job_admission_wait_seconds` | histogram | | how long a job waited for host memory before its microVM was created. Only jobs that found no ready pool VM are observed: cold boots, and jobs that took a pool VM that became ready while they waited. Jobs that got a ready pool VM at once are not counted |
| `firerunner_jobs_total` | counter | `result`, `reason` | finished jobs: `success`, and `script_failure` (the job's own commands; an exit code 255 from a VM that still answers too), both with reason `none`; `system_failure` (FireRunner or the host) with a reason below |
| `firerunner_job_duration_seconds` | histogram | | job duration from prepare to cleanup |
| `firerunner_stage_duration_seconds` | histogram | `stage`: `prepare_script`, `get_sources`, `restore_cache`, `download_artifacts`, `script`, `after_script`, `archive_cache`, `upload_artifacts`, `cleanup_file_variables`, `other` | time of each gitlab-runner stage of a job; `script` is the job's own steps, the cache and artifact uploads include their variants |
| `firerunner_microvms` | gauge | `state` | every microVM flintlock lists (pool, job, builder, `run`), by flintlock state |
| `firerunner_vm_boot_seconds` | histogram | `kind`: `pool`, `cold`, `builder` | time from create request to SSH ready; a builder's includes waiting for its previous cache copy, host memory and the cache restore |
| `firerunner_vm_boot_failures_total` | counter | `kind`: `pool`, `cold`, `builder`, `preload`, `pool_dead` | microVMs that did not become ready; `pool_dead`: a pool VM that no longer answered when a job claimed it |
| `firerunner_orphans_deleted_total` | counter | `reason`, below | microVMs reconcile deleted |
| `firerunner_builders` | gauge | `state`: `ready`, `booting` | per-project BuildKit builder microVMs |
| `firerunner_builder_slots` | gauge | | builders that may run at once: `builder.max`, 0 while builders are disabled |
| `firerunner_builder_requests_total` | counter | `state`: `ready`, `booting`, `busy`, `disabled` | what a job asking for its project's builder got. Asked at prepare for every shell job of a project that has a builder, and when a stage runs `docker build`; `booting`: the job's `docker build` waits for it; `busy`: no free slot, or the project's builder just failed |
| `firerunner_builds_total` | counter | `builder`: `ready`, `booting`, `busy`, `disabled`, `none` | jobs that ran `docker build` (counted once per job), by the state of the project's builder they got: `ready` warm layer cache, `booting` the builder was starting, `busy` no free slot (built without the cache), `none` no builder |
| `firerunner_builder_removals_total` | counter | `reason`: `lru`, `idle`, `max_age`, `config_changed`, `vm_gone`, `not_answering`, `operator`, `disabled` | builders removed. `lru` (its slot was needed), `idle`, `max_age` and `config_changed` keep their cache for the project's next builder; `operator` is `builder rm` |
| `firerunner_builder_cache_total` | counter | `op`: `save`, `restore`, `evict`; `result`: `ok`, `failed`, `skipped`, `missing`, `stale`, `legacy` | builder caches copied to the host when a builder is removed (`save`; `skipped`: too large, no room, or dropped by `builder rm`), loaded into the project's next builder (`restore`; `missing`: nothing saved, `stale`: saved by another builder image, dropped; `legacy`: saved before caches named their image, loaded for `moby/buildkit:v0.33.0`) and deleted to make room for another (`evict`) |
| `firerunner_memory_committed_bytes` | gauge | `role`: `pool`, `job`, `builder`, `run`, `other` | guest memory plus Firecracker overhead promised to microVMs: what admission counts |
| `firerunner_memory_capacity_bytes` | gauge | | memory microVMs may commit: MemTotal minus `vm.host_reserve_mb` |
| `firerunner_host_memory_available_bytes` | gauge | | MemAvailable on the host |
| `firerunner_host_oom_kills_total` | counter | | processes the host kernel OOM-killed while the daemon ran (usually a firecracker microVM) |
| `firerunner_dhcp_leases` | gauge | `state`: `live`, `stale` | unexpired leases on the microVM bridge; `stale`: not held by a microVM flintlock lists |
| `firerunner_dhcp_leases_capacity` | gauge | | addresses in the microVM DHCP range; 0 until the range has been read |
| `firerunner_thinpool_usage_ratio` | gauge | `type`: `data`, `metadata` | devmapper thin pool usage, 0–1, read with `lvs` every 15 s; absent while `lvs` fails |
| `firerunner_disk_free_bytes`, `firerunner_disk_size_bytes` | gauge | `dir`: `/var/lib/firerunner/builder-cache`, `/var/lib/flintlock/vm` | free and total bytes of the file system holding the saved builder caches and flintlock's microVM state; absent while it cannot be read |
| `firerunner_runner_concurrent` | gauge | | gitlab-runner's `concurrent` limit |
| `firerunner_service_up` | gauge | `service`: `containerd-flintlock`, `firerunner-net`, `firerunner-dnsmasq`, `firerunner-registry`, `firerunner-cache`, `flintlockd`, `gitlab-runner` | 1 if the systemd service is active |
| `firerunner_flintlock_up` | gauge | | 1 if the daemon's last flintlock listing answered |
| `firerunner_flintlock_errors_total` | counter | `op`: `CreateMicroVM`, `DeleteMicroVM`, `ListMicroVMs`; `code`: the gRPC code | failed flintlock calls of the daemon; every failed try of a retried listing counts |

System failure reasons in `firerunner_jobs_total`: `admission_timeout` (no host memory for a
microVM within `vm.boot_timeout`, or other admissions held the admission lock that long), `flintlock_error`, `vm_boot` (no DHCP lease or no SSH in time),
`services` (`services:` containers did not start), `docker_auth`, `state_file`, `ssh_lost` (the VM
died or was killed during a stage), `helper_stage` (sources, cache or artifacts), `canceled`
(cancelled or timed out, in prepare or a stage gitlab-runner stopped; leave it out of failure
rates), `other`.

Reasons in `firerunner_orphans_deleted_total`: `failed` (flintlock reports it failed),
`job_max_age` (its job ran longer than `daemon.job_max_age`), `pool_previous_run` and
`builder_previous_run` (left by a previous daemon run), `pool_orphan`, `builder_orphan` and
`job_orphan` (nobody owns it any more), `run_abandoned` (a `firerunner run` VM older than
`daemon.job_max_age`).

Histogram buckets in seconds, for `le` in queries: prepare 0.5, 1, 2, 3, 4, 5, 6, 8, 10, 12, 14,
16, 18, 20, 25, 30, 45, 60, 90, 120, 180, 300; boot 2, 5, 8, 10, 12, 14, 16, 18, 20, 25, 30, 45,
60, 90, 120, 180, 240, 300, 600; admission wait 0.5, 1, 5, 10, 30, 60, 120, 180, 300; job
duration 10, 20, 30, 45, 60, 90, 120, 180, 300, 450, 600, 900, 1200, 1800, 3600; preload 5, 10,
20, 30, 60, 90, 120, 180, 300, 600, 900.

## Limits

- x86_64 Linux hosts, Linux jobs.
- The layer cache belongs to one project's builder: a project's first build is cold. After that
  the cache survives the builder, up to `builder.saved_cache_gb` of saved caches.
