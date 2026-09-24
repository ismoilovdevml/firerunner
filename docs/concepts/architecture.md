# Architecture

```mermaid
flowchart LR
  GL[GitLab] -- job --> GR[gitlab-runner<br/>custom executor]
  GR -- prepare / run / cleanup --> FE[firerunner executor]
  FE -- claim VM --> D[firerunner daemon]
  D -- create / delete --> FL[flintlockd]
  FE -- create / delete --> FL
  FL -- containerd images + thin pool --> CT[(containerd)]
  FL --> VM1[microVM job-1]
  FL --> VM2[microVM job-2]
  FL --> P[pool microVMs]
  FE -- SSH, pinned host key --> VM1
  VM1 & VM2 & P --- BR{{br-fc bridge<br/>DHCP · DNS · NAT}}
```

## Components

| Component | What it does |
|---|---|
| **gitlab-runner** | polls GitLab for jobs; with the custom executor it calls `firerunner executor prepare / run / cleanup` |
| **firerunner executor** | gets a microVM for the job, streams each stage script into it over SSH, deletes it |
| **firerunner daemon** | keeps the warm pool, deletes orphaned VMs (reconcile), exports metrics, serves a root-only unix socket |
| **flintlockd** | creates and deletes Firecracker microVMs from OCI images (kernel + root filesystem) |
| **containerd** | stores the images; root disks are devmapper thin-pool snapshots (copy-on-write) |
| **Firecracker** | the VMM: one process per microVM, KVM-backed |
| **dnsmasq / nftables** | DHCP and DNS for microVMs, NAT to the outside, host and inter-VM isolation |
| **registry** | Docker Hub pull-through cache for Docker inside the VMs |
| **versitygw** | S3 store for gitlab-runner's `cache:`; jobs get presigned URLs only |

## Job lifecycle

```mermaid
sequenceDiagram
  participant G as gitlab-runner
  participant E as executor
  participant D as daemon
  participant V as microVM
  G->>E: prepare
  E->>D: claim a pool VM
  alt pool has one
    D-->>E: VM (id, IP, host key)
  else pool empty
    E->>E: boot a VM (wait for memory, DHCP lease, SSH)
  end
  E->>E: record job state (exclusive)
  loop every stage
    G->>E: run script stage
    E->>V: ssh (pinned key) — script, or docker run <image>
  end
  G->>E: cleanup
  E->>V: delete VM
  D->>D: refill the pool
```

## Why a custom executor and not webhooks

gitlab-runner already solves job pickup, retries, artifacts and cancellation. As a custom executor
FireRunner only has to provide machines, and a job can never land in a VM that was created for
another job.

## Ownership and reconcile

flintlockd does not keep labels, so FireRunner encodes the role in the VM id: `pool-<random>`,
`job-<CI_JOB_ID>`, `run-<random>`. The daemon lists VMs every minute and deletes those that nothing
owns: failed VMs, pool VMs left by an earlier daemon run, job VMs without a running job, and
anything older than `daemon.job_max_age`. Idle pool VMs are recorded in `/run/firerunner/pool.json`
and adopted again after a restart.
