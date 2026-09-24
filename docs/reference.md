# Reference

## CLI

All commands need root.

| Command | |
|---|---|
| `firerunner status` · `doctor` | overview · every check with a hint |
| `firerunner config show` · `keys` · `get <key>` · `set <key> <value>` | settings |
| `firerunner runner register --url URL --token glrt-... [--concurrent N]` | register this host |
| `firerunner runner status` · `concurrent N` · `unregister` | the GitLab runner |
| `firerunner runner cache [local \| off \| s3 --server H:P --bucket B]` | where `cache:` is stored |
| `firerunner vm list` · `logs <id>` · `rm <id> \| --all` | microVMs |
| `firerunner pool [refresh]` | pre-booted VMs; refresh replaces them |
| `firerunner builder list` · `rm <project> \| --all [--force]` | builders; a builder in use is kept unless `--force` |
| `firerunner run [--keep] -- <command>` | run a command in a throwaway VM |
| `firerunner upgrade [--version V] [--check]` | update the binary |
| `firerunner version` | |

`firerunner daemon` and `firerunner executor …` are run by systemd and gitlab-runner.

## Metrics

`http://<host>:9477/metrics`

| Metric | |
|---|---|
| `firerunner_pool_ready`, `firerunner_pool_target` | pre-booted VMs now and wanted |
| `firerunner_pool_claims_total{result}` | jobs that got a pool VM (`hit`) or booted one (`miss`) |
| `firerunner_job_prepare_seconds{source}` | how long jobs waited for a VM |
| `firerunner_job_duration_seconds`, `firerunner_jobs_total{result}` | finished jobs |
| `firerunner_vm_boot_seconds{kind}`, `firerunner_vm_boot_failures_total{kind}` | boots |
| `firerunner_builders`, `firerunner_builder_requests_total{state}` | builders and their use |
| `firerunner_orphans_deleted_total{reason}` | VMs removed by cleanup |
| `firerunner_admission_waits_total` | boots postponed for lack of memory |
| `firerunner_thinpool_usage_ratio{type}` | disk pool usage, 0–1 |
| `firerunner_service_up{service}`, `firerunner_flintlock_up` | health |

## Limits

- x86_64 Linux hosts, Linux jobs.
- The layer cache belongs to one project's builder: the first build of a project, and the first
  after `builder.idle_ttl`, is cold.
