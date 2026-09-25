# Operations

![firerunner status](images/cli-status.png)

## Health

```bash
sudo firerunner status     # one screen: services, disk, VMs, runner
sudo firerunner doctor     # every check, with a hint when one fails
```

## Upgrade

```bash
sudo firerunner upgrade            # latest build of main (edge)
sudo firerunner upgrade --version v1.2.0
```

The binary is checksum-verified and replaced in place. Running jobs, pre-booted VMs and builders
keep running. To upgrade the host stack (containerd, Firecracker, flintlock, gitlab-runner, network),
run the installer again.

After a new guest image is published, replace the idle VMs: `sudo firerunner pool refresh`.

To roll back, restore the previous binary and the `config.yaml` saved before the upgrade: an older
version refuses keys it does not know.

## microVMs

```bash
sudo firerunner vm list            # role: job, pool, builder or run
sudo firerunner vm logs <id>       # guest console, for boot problems
sudo firerunner vm rm <id>
sudo firerunner builder list
```

VMs that no job owns are deleted automatically every minute.

To drain a host, pause the runner in GitLab and wait until `vm list` shows no `job` VMs.

## Monitoring

Prometheus metrics are on `127.0.0.1:9477/metrics`. To scrape from elsewhere, install with
`FR_METRICS_ALLOW=<prometheus-ip>/32`.

Import the Grafana dashboard and the alert rules from
[`deploy/`](https://github.com/ismoilovdevml/firerunner/tree/main/deploy). Alerts page only for
failures FireRunner or the host caused, never for failing project scripts. Watch:

- **FireRunner failure rate** and **system failures by reason**.
- **Pool hit rate**: below 80 %, raise `pool.size`.
- **Committed memory** and **wait for host memory**: jobs queue when memory is full.
- **DHCP leases**: at capacity no VM gets an address.
- **Thin pool**: at 100 % no VM can start.

The daemon logs one line per job (`journalctl -u firerunner | grep '"job":"<id>"'`).

Logs: `journalctl -u firerunner -u flintlockd -u gitlab-runner`.

## Services

| Unit | Role |
|---|---|
| `firerunner` | daemon: pool, builders, cleanup, metrics |
| `flintlockd`, `containerd-flintlock` | start and stop microVMs; store their images |
| `firerunner-net`, `firerunner-dnsmasq` | microVM network, DHCP and DNS |
| `firerunner-registry` | Docker Hub cache for the VMs |
| `firerunner-cache` | storage for `cache:` |
| `gitlab-runner` | takes jobs from GitLab |

Back up `/etc/firerunner`, `/etc/opt/flintlockd` and `/etc/gitlab-runner/config.toml`. Everything
else can be rebuilt.

## Troubleshooting

Start with `sudo firerunner doctor`.

| Symptom | Fix |
|---|---|
| installer: `/dev/kvm not found` | turn on virtualization or nested virtualization |
| installer: `no blank disk found` | attach an empty disk or set `FR_DISK` |
| jobs stay pending | the job needs `tags: [firecracker]`; check the runner is online in GitLab |
| `got no DHCP lease` | `systemctl status firerunner-net firerunner-dnsmasq`; then `firerunner vm logs <id>` |
| `waiting for host memory` for long | fewer parallel jobs, a smaller `pool.size`, or more RAM; `firerunner_memory_committed_bytes` shows who holds it |
| alert: system failures | the `reason` label: `ssh_lost` usually a host OOM kill (`journalctl -k | grep -i oom`), `vm_boot` DHCP or boot, `flintlock_error` flintlockd or the thin pool |
| alert: DHCP leases, `stale` grows | leases of deleted VMs are not released: install `dnsmasq-utils` (`dhcp_release`) |
| alert: daemon down or stuck | `journalctl -u firerunner -n 200`, then `systemctl restart firerunner` |
| firewall rules need re-applying | `systemctl reload firerunner-net`; never restart it while jobs run, stopping it removes the bridge |
| jobs cannot reach an internal host | its network overlaps `10.200.0.0/24` or the VM Docker ranges; change `FR_SUBNET` or `vm.docker_bip` |
