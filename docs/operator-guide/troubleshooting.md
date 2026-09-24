# Troubleshooting

Start with `sudo firerunner doctor`; every failed check prints a hint.

## The installer stops

It prints `ERROR: line N: <command> (exit X)`. Common causes:

| Symptom | Fix |
|---|---|
| `/dev/kvm not found` | enable VT-x/AMD-V or nested virtualization ([Requirements](../getting-started/requirements.md)) |
| `no blank disk found for the thin pool` | attach an empty disk or set `FR_DISK=/dev/sdX` (wiped) |
| `download failed` / `checksum mismatch` | check outbound HTTPS to github.com, ghcr.io, gitlab-runner-downloads.s3.amazonaws.com |

## Jobs stay pending in GitLab

- The job has no `tags: [firecracker]` and the runner does not run untagged jobs.
- The runner is paused or offline in GitLab: `sudo firerunner runner status`, `journalctl -u gitlab-runner`.

## Jobs fail in "Preparing the custom executor"

```bash
sudo firerunner vm list
sudo firerunner vm logs <id>          # guest console
journalctl -u flintlockd -n 100
```

| Symptom | Cause / fix |
|---|---|
| `got no DHCP lease within 3m` | `firerunner-dnsmasq` or `firerunner-net` down; the guest failed to boot (see `vm logs`) |
| console shows `Unable to mount root fs` / `Cannot open root device` | kernel and cmdline do not match: FireRunner's kernel needs ACPI (no `acpi=off`); flintlock's old 5.10 kernel needs `acpi=off` |
| `cache:` shows `Failed to extract cache` on the first run | normal: nothing cached yet. If it repeats: `firerunner doctor` (cache server), `journalctl -u firerunner-cache` |
| `did not answer SSH` | cloud-init failed in the guest; check `vm logs` |
| `waiting for host memory` forever | lower `runner concurrent` or `pool.size`, or add RAM ([Capacity](capacity.md)) |
| `failed reading from content store` | transient flintlockd race during deletes; retried automatically |

## Jobs cannot reach internal hosts

The guest's Docker uses `10.201.0.0/24` and `10.202.0.0/16`. If your network uses those ranges,
change `vm.docker_bip` / `vm.docker_address_pool`. If the microVM subnet `10.200.0.0/24` collides,
reinstall with `FR_SUBNET`.

## Many microVMs, host out of memory

Should not happen: memory admission counts every VM. If it does, check that you have not lowered
`vm.host_reserve_mb` to 0 and that nothing else big runs on the host. `flintlockd` runs with
`OOMPolicy=continue`, so one killed VM does not take the others down.

## The pool never fills

- `firerunner_admission_waits_total` grows: not enough memory for `pool.size` + running jobs.
- `pool.preload_images` contains an image that cannot be pulled: see `journalctl -u firerunner` (`image preload failed`).
