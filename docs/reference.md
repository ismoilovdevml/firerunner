# Reference

## CLI

All commands need root.

| Command | |
|---|---|
| `firerunner status` · `doctor` | overview · every check with a hint |
| `firerunner config show` · `keys` · `path` · `get <key>` · `set <key> <value>` | settings; `path` prints the config file |
| `firerunner config unset vm.kernel_cmdline.<arg>` | remove a guest kernel argument |
| `firerunner runner register --url URL --token glrt-... [--concurrent N] [--name NAME]` | register this host; the name shown in GitLab defaults to `firerunner-<hostname>` |
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

`http://<host>:9477/metrics`

| Metric | |
|---|---|
| `firerunner_pool_ready`, `firerunner_pool_target` | pre-booted VMs now and wanted |
| `firerunner_pool_claims_total{result}` | claim requests; a job waiting for memory asks again, so use `firerunner_job_prepare_seconds_count{source}` for the hit rate |
| `firerunner_job_prepare_seconds{source}` | how long jobs waited for a ready VM (`pool` or `cold`) |
| `firerunner_job_admission_wait_seconds` | how long jobs waited for host memory |
| `firerunner_jobs_total{result,reason}` | finished jobs: `success`, `script_failure` (the job's commands), `system_failure` (FireRunner or the host) with a `reason` such as `ssh_lost`, `vm_boot`, `flintlock_error`, `admission_timeout` |
| `firerunner_job_duration_seconds` | job duration |
| `firerunner_vm_boot_seconds{kind}`, `firerunner_vm_boot_failures_total{kind}` | boots |
| `firerunner_builders`, `firerunner_builder_requests_total{state}` | builders and their use |
| `firerunner_builder_cache_total{op,result}` | caches saved from deleted builders and restored into new ones |
| `firerunner_orphans_deleted_total{reason}` | VMs removed by cleanup |
| `firerunner_admission_waits_total` | pool refill passes postponed for lack of memory |
| `firerunner_memory_committed_bytes{role}`, `firerunner_memory_capacity_bytes` | memory promised to VMs by role, and the limit |
| `firerunner_dhcp_leases{state}`, `firerunner_dhcp_leases_capacity` | leased VM addresses (`stale`: not held by a VM) |
| `firerunner_host_oom_kills_total` | processes the host kernel OOM-killed |
| `firerunner_loop_last_tick_timestamp_seconds` | last pass of the daemon loop; `/healthz` fails after 2 min |
| `firerunner_thinpool_usage_ratio{type}` | disk pool usage, 0–1 |
| `firerunner_service_up{service}`, `firerunner_flintlock_up` | health |

## Limits

- x86_64 Linux hosts, Linux jobs.
- The layer cache belongs to one project's builder: a project's first build is cold. After that
  the cache survives the builder, up to `builder.saved_cache_gb` of saved caches.
