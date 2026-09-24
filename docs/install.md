# Install

## Requirements

- x86_64 Linux with systemd and `/dev/kvm`. Tested on Rocky Linux 9.6 and Ubuntu 24.04.
- A blank disk for microVM disks, 100 GB or more. The installer wipes it.
- 16 GB RAM or more. Every job VM takes 2 GB by default.
- GitLab 16 or newer.

On a VM host, turn on nested virtualization first:

- VMware: power off, then *Expose hardware assisted virtualization to the guest OS*.
- KVM or Proxmox: `kvm_intel nested=1` and CPU type `host`.

Check with `ls /dev/kvm`.

## Install

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

The installer checks every download against its sha256, sets up the thin pool, the microVM
network, the image and cache servers and the services, then verifies them. You can run it again:
it only restarts what changed, and it reloads the microVM firewall in place without touching
running VMs.

Options go before `bash`, for example `sudo FR_DISK=/dev/sdb bash`:

| Variable | Default | |
|---|---|---|
| `FR_DISK` | first blank disk | disk for microVM disks, wiped |
| `FR_VM_DISK` | `40GB` | root disk of each microVM, thin-provisioned |
| `FR_POOL_SIZE` | `2` | pre-booted microVMs |
| `FR_METRICS_ALLOW` | none | CIDR that may scrape `:9477`; without it metrics stay local. Kept for later runs |
| `FR_EGRESS_DENY` | none | comma-separated CIDRs jobs must not reach, e.g. `192.168.0.0/16` |
| `FR_GITLAB_URL`, `FR_RUNNER_TOKEN` | none | register the runner during install |
| `FR_SUBNET` | `10.200.0` | /24 for microVMs |

## Connect GitLab

1. In GitLab, create a runner: *Settings → CI/CD → Runners → New runner* (project, group or
   instance). Tag: `firecracker`. Leave *Run untagged jobs* off.
2. Register it on the host:

    ```bash
    sudo firerunner runner register --url https://gitlab.example.com --token glrt-...
    ```

3. Check:

    ```bash
    sudo firerunner doctor               # every check "ok"
    sudo firerunner run -- uname -a      # boots a throwaway microVM
    ```

For more hosts, install and register each one; GitLab spreads jobs over them.

![firerunner doctor](images/cli-doctor.png)

## Uninstall

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash -s -- uninstall
```

It deletes the microVMs and removes services and binaries. The disk, images and `/etc/firerunner`
stay. Delete the runner in GitLab yourself.
