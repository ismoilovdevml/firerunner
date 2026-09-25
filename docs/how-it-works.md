# How it works

![Architecture](images/architecture.svg)

gitlab-runner takes a job from GitLab and calls FireRunner three times:

1. **prepare** takes a pre-booted microVM from the daemon, or boots one.
2. **run** sends each stage script into the VM over SSH. With `image:`, the script runs in that
   container inside the VM.
3. **cleanup** deletes the VM. The daemon boots a replacement.

![Job lifecycle](images/job-lifecycle.svg)

| Part | Job |
|---|---|
| firerunner daemon | keeps the pool and the builders, deletes VMs nobody owns, exports metrics |
| flintlock, Firecracker | create and run the microVMs; disks are copy-on-write snapshots |
| builder microVM | one per project; runs its `docker build` and keeps the layer cache |
| registry, versitygw | Docker Hub cache and `cache:` storage on the host |

The guest is Ubuntu 24.04 on a 6.18 LTS kernel built from Firecracker's config
([`images/`](https://github.com/ismoilovdevml/firerunner/tree/main/images)).

## Security

A job may run any code as root inside its own VM. It must not reach other jobs, the host's
control plane, or anything the next job will use.

| Boundary | Control |
|---|---|
| Job and job | a new VM with its own kernel per job, never reused; the host drops all traffic from one VM to another, on the bridge and routed through the host (a job reaches only its own project's builder, through the host) |
| Job and host | the host accepts only DHCP, DNS, the Docker Hub cache and `cache:` storage from VMs; cloud metadata (`169.254.169.254`) is blocked; nothing a VM sends on its metadata interface (`eth0`) reaches the host |
| Network and job | hosts on your network cannot open connections to microVMs or to the services the host runs for them; microVMs only get replies to connections they opened |
| Project and project | each builder has its own certificate authority; a job gets a client certificate of its own for its project's builder only, valid for `daemon.job_max_age`. `cache:` is stored per project and reached through short-lived signed URLs |
| Host and VM | each VM gets its own SSH host key and every connection checks it, so scripts and secrets go only to the job's own VM. Job and project ids come from gitlab-runner, not from job variables |
| Local users | the microVM API listens on 127.0.0.1 with a token in a root-only file |
| Memory | a VM starts only when its memory fits |
| Supply chain | every download is checksum-verified; releases have an SBOM and a signed provenance (`gh attestation verify firerunner-linux-amd64 --repo ismoilovdevml/firerunner`) |

Not covered yet:

- VMs can reach your LAN through NAT, like a docker executor. Block networks jobs must not reach
  with `FR_EGRESS_DENY` (install option).

The code was audited with
[cloudflare/security-audit-skill](https://github.com/cloudflare/security-audit-skill); the findings
are fixed.
