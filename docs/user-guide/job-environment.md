# Inside the job VM

| | |
|---|---|
| OS | Ubuntu 24.04 LTS, guest kernel 6.18 LTS (Firecracker microvm config + Docker networking) |
| User | `root` |
| CPU / RAM | 2 vCPU / 2 GB by default |
| Disk | 10 GB root filesystem (copy-on-write, discarded after the job) |
| Tools | git, git-lfs, curl, wget, jq, unzip, zip, xz, tar, make, python3, sudo, iproute2, dnsutils |
| Docker | Docker CE (current stable) with buildx and compose, started on first use |
| gitlab-runner | present in the VM (used for artifacts and cache) |
| Build directory | `/root/builds/<group>/<project>` |
| Hostname | `job-<CI_JOB_ID>` |

## Network

- Outbound: internet and your company network through NAT on the runner host.
- DNS: the runner host's resolver.
- Docker Hub pulls go through a cache on the runner host (transparent).
- Your job **cannot** reach other jobs' VMs, and can reach the host only for DHCP, DNS, the
  image cache and the `cache:` store.
- Docker inside the VM uses `10.201.0.0/24` for its default bridge and `10.202.0.0/16` for
  user-defined networks, so it does not collide with `172.17.0.0/16` networks.

## Lifecycle

1. Your job gets a VM that was booted before the job started (usually within ~0.3 s), or a
   freshly booted one (~15 s) when the pool is empty.
2. GitLab's usual stages run in that VM: get sources, restore artifacts, your script, upload artifacts.
3. When the job finishes, fails or is cancelled, the VM is deleted. Nothing is kept.

## Environment variables

All CI/CD variables are available as usual. `CI_JOB_ID` must be the numeric id assigned by GitLab
(do not override it).
