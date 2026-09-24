# Metrics reference

Endpoint: `http://<host>:9477/metrics` (see [Monitoring](../operator-guide/monitoring.md)). Also `/healthz`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `firerunner_build_info` | gauge | `version` | always 1 |
| `firerunner_pool_target` | gauge | | configured `pool.size` |
| `firerunner_pool_ready` | gauge | | pre-booted VMs ready to claim |
| `firerunner_pool_booting` | gauge | | pool VMs booting (incl. image preload) |
| `firerunner_pool_claims_total` | counter | `result` = hit\|miss | jobs that got a pool VM / had to cold-boot |
| `firerunner_pool_preload_seconds` | histogram | | time to preload images into a pool VM |
| `firerunner_job_prepare_seconds` | histogram | `source` = pool\|cold | how long a job waited for its VM |
| `firerunner_job_duration_seconds` | histogram | | prepare to cleanup |
| `firerunner_jobs_total` | counter | `result` = success\|failed | finished jobs |
| `firerunner_vm_boot_seconds` | histogram | `kind` = pool\|cold | create to SSH-ready |
| `firerunner_vm_boot_failures_total` | counter | `kind` = pool\|cold\|preload | VMs that did not become ready |
| `firerunner_microvms` | gauge | `state` | VMs known to flintlock |
| `firerunner_orphans_deleted_total` | counter | `reason` | VMs deleted by reconcile |
| `firerunner_admission_waits_total` | counter | | boots postponed for lack of memory |
| `firerunner_flintlock_up` | gauge | | 1 if the flintlock API answers |
| `firerunner_service_up` | gauge | `service` | 1 if the systemd unit is active |
| `firerunner_thinpool_usage_ratio` | gauge | `type` = data\|metadata | thin pool usage (0–1) |
| `firerunner_host_memory_available_bytes` | gauge | | MemAvailable |
| `firerunner_runner_concurrent` | gauge | | gitlab-runner `concurrent` |

Plus the standard Go and process collectors.
