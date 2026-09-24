# Requirements

## Host

| | Minimum | Recommended |
|---|---|---|
| CPU | x86_64 with VT-x / AMD-V | 8+ vCPU |
| KVM | `/dev/kvm` present | |
| Memory | 8 GB | 16 GB+ (each job VM uses 2 GB by default) |
| OS disk | 20 GB free | |
| Thin-pool disk | a blank disk, 50 GB | 100 GB+ on SSD |
| OS | Rocky Linux 9 / RHEL 9 family with systemd | Rocky Linux 9.6 (tested) |
| Network | outbound HTTPS to GitHub, ghcr.io, your GitLab and your registries | |

!!! note "Tested platforms"
    End-to-end tested on **Rocky Linux 9.6** with SELinux enforcing and firewalld.
    The installer also has a Debian/Ubuntu (apt) path that has not been run end to end yet
    ([#7](https://github.com/ismoilovdevml/firerunner/issues/7)). Only x86_64 is supported.

## Bare metal or a VM?

Both work. FireRunner needs hardware virtualization inside the host:

- **Bare metal**: enable VT-x / AMD-V in the BIOS.
- **VMware vSphere / ESXi**: power the VM off and enable
  *CPU → Expose hardware assisted virtualization to the guest OS*
  (`govc vm.change -vm <vm> -nested-hv-enabled=true`). Use virtual hardware version 9 or newer.
- **KVM / Proxmox**: enable nested virtualization (`kvm_intel nested=1`) and use CPU type `host`.
- **Cloud**: use an instance type with nested virtualization or a bare-metal instance.

Check it on the host:

```bash
ls -l /dev/kvm                       # must exist
grep -cE 'vmx|svm' /proc/cpuinfo     # must be > 0
```

### Recommended settings for a host VM

- Give the VM a **second, blank disk** for the microVM thin pool (thin-provisioned is fine).
- Do not over-commit the VM's memory at the hypervisor level (microVM memory is real guest memory).
- Avoid live-migrating the VM while jobs run.

## GitLab

- GitLab 16.0 or newer (the *new runner* token flow, `glrt-...` tokens). Tested with GitLab CE 19.0.
- A project, group or instance runner created in the GitLab UI (see [Connect GitLab](connect-gitlab.md)).
