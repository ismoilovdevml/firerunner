# Monitoring

## Metrics endpoint

The daemon exposes Prometheus metrics on `daemon.metrics_listen` (default `127.0.0.1:9477`).
To scrape from another host, install with `FR_METRICS_ALLOW=<prometheus-ip>/32` (opens the port
for that source only and listens on `:9477`).

```yaml
# prometheus.yml
scrape_configs:
  - job_name: firerunner
    static_configs:
      - targets: ["runner-host-01:9477"]
        labels:
          instance: runner-host-01
```

All metrics are listed in the [metrics reference](../reference/metrics.md).

## Grafana dashboard

Import [`deploy/grafana/firerunner.json`](https://github.com/ismoilovdevml/firerunner/blob/main/deploy/grafana/firerunner.json)
(uid `firerunner`). Choose your Prometheus data source in the *Data source* variable.

![Grafana dashboard](../images/grafana.png)

What to look at:

- **Pool hit rate** — share of jobs that got a pre-booted VM. Below ~80 %: raise `pool.size`.
- **Prepare p50 (pool / cold)** — how long jobs waited for a VM.
- **Job failure rate** — compare with other runners before blaming the host.
- **Thin pool data** — at 100 % every VM create fails.
- **Boot failures and cleanups** — boot problems, reconciled orphans, jobs waiting for memory.

## Alerts

[`deploy/prometheus/firerunner-alerts.yml`](https://github.com/ismoilovdevml/firerunner/blob/main/deploy/prometheus/firerunner-alerts.yml)
contains 7 rules:

| Alert | Fires when |
|---|---|
| `FireRunnerDown` | no metrics for 5 min |
| `FireRunnerServiceDown` | a FireRunner systemd service is down for 3 min |
| `FireRunnerFlintlockDown` | flintlock API unreachable for 3 min |
| `FireRunnerThinPoolFull` | thin pool > 85 % for 10 min |
| `FireRunnerBootFailures` | ≥ 3 boot failures in 15 min |
| `FireRunnerJobsFailing` | > 50 % of ≥ 10 jobs failed in 1 h |
| `FireRunnerPoolEmpty` | pool empty and not refilling for 15 min |

## Logs

```bash
journalctl -u firerunner -f          # daemon (JSON)
journalctl -u flintlockd -f          # microVM lifecycle
journalctl -u gitlab-runner -f       # job pickup
```
