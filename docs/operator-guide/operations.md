# Day-2 operations

![firerunner status](../images/cli-status.png)

## Health

```bash
sudo firerunner status     # one-screen overview
sudo firerunner doctor     # every check with a hint when it fails (exit code 1 on failure)
```

## Upgrade

Releases: every push to `main` publishes the rolling `edge` release; tags `vX.Y.Z` publish stable
releases. Binaries are checksum-verified and carry a signed build provenance.

```bash
sudo firerunner upgrade --check              # what would be installed
sudo firerunner upgrade                      # edge (latest main)
sudo firerunner upgrade --version v1.2.0     # a specific release
sudo firerunner -v
```

`upgrade` replaces the binary atomically and restarts `firerunner.service`. Running jobs are not
interrupted, and the warm pool is kept (`/run/firerunner/pool.json`).

To upgrade the host stack (containerd, Firecracker, flintlock, gitlab-runner, network rules),
re-run the installer. It only restarts services whose files changed.

Verify a release yourself:

```bash
gh attestation verify firerunner-linux-amd64 --repo ismoilovdevml/firerunner
```

## Guest image updates

A new `firerunner-rootfs:latest` is published when `images/` changes. Replace idle pool VMs so the
next jobs use it:

```bash
sudo firerunner pool refresh
```

## microVMs

```bash
sudo firerunner vm list              # ID, role (pool/job/run), state, IP
sudo firerunner vm logs job-74274    # guest console (boot problems)
sudo firerunner vm rm job-74274      # force-delete one
sudo firerunner pool                 # pre-booted VMs
```

Orphans (VMs whose job is gone) are deleted automatically every `daemon.reconcile_interval`.

## Services

| Unit | Role |
|---|---|
| `containerd-flintlock` | image store and thin-pool snapshots |
| `flintlockd` | microVM lifecycle (Firecracker) |
| `firerunner-net` | bridge, NAT, nftables |
| `firerunner-dnsmasq` | DHCP/DNS for microVMs |
| `firerunner-registry` | Docker Hub pull-through cache |
| `firerunner-cache` | S3 store for `cache:` ([versitygw](https://github.com/versity/versitygw), files in `/var/lib/firerunner/cache`); `firerunner-cache-clean.timer` deletes archives older than `FR_CACHE_DAYS` (14) |
| `firerunner` | daemon: pool, reconcile, metrics |
| `gitlab-runner` | talks to GitLab, calls `firerunner executor` |

## Draining a host

1. Pause the runner in GitLab (*Settings → CI/CD → Runners → Pause*); no new jobs are picked up.
2. Wait until `sudo firerunner vm list` shows only `pool` VMs.
3. Do the maintenance; resume the runner in GitLab.

## Backups

Nothing on the host needs a backup except `/etc/firerunner`, `/etc/gitlab-runner/config.toml` and
`/etc/opt/flintlockd`. Job VMs are disposable.
