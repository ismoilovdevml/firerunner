# Debugging a failed job

## Read the first lines of the job log

The *Preparing the "custom" executor* section tells you where the job ran:

```text
microVM pool-551bf7 ready at 10.200.0.236 in 300ms (pool, 2 vCPU, 2048 MB)
```

`pool` means a pre-booted VM, `cold` means a VM booted for this job.

## Typical failures

| Log line | Meaning | What to do |
|---|---|---|
| `ERROR: Job failed: exit status 1` after your script | your script failed | same as on any runner |
| `ERROR: Job failed (system failure)` during *Preparing* | no VM could be created (host busy or broken) | retry; if it repeats, tell the runner operator |
| `waiting for host memory …` | the host is at capacity; the job waits for a free slot | nothing — it continues when a VM finishes |
| `invalid image "--…"` | `image:` starts with `-` | fix the image name |
| `Cannot connect to the Docker daemon` | Docker failed to start in the VM | report to the operator with the job URL |
| `pull access denied` | private image without credentials | set `DOCKER_AUTH_CONFIG` |
| `WARNING: service … did not open port … within 30s` | a service is slow or crashed; its last log lines follow | check the service's variables (e.g. `POSTGRES_PASSWORD`) |
| `WARNING: Starting with version 17.0 the 'build_script' stage will be replaced with 'step_script'` | printed by gitlab-runner itself for every custom-executor job | harmless, ignore it |

## Reproduce locally on the runner host (operators)

```bash
sudo firerunner run --keep -- "cat /etc/os-release"
# prints the ssh command with the VM's pinned host key, e.g.
# ssh -i /etc/firerunner/executor/id_ed25519 -o UserKnownHostsFile=/run/firerunner/known_hosts/run-xxxx -o HostKeyAlias=run-xxxx root@10.200.0.x
```

`--keep` leaves the VM running so you can log in and try the job's commands; delete it with
`sudo firerunner vm rm run-xxxx`.
