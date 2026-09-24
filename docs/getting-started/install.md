# Install

## One command

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
```

The installer is non-interactive and safe to re-run. It:

1. checks root, x86_64, systemd and `/dev/kvm`;
2. installs packages (lvm2, dnsmasq, nftables, …);
3. installs **containerd** (dedicated instance) with a **devmapper thin pool** on the first blank disk;
4. installs **Firecracker** + jailer and **flintlockd** (listening on 127.0.0.1 only, token in a 0600 file);
5. creates the microVM network: bridge `br-fc`, DHCP/DNS (dnsmasq), NAT, host firewall rules,
   isolation between microVMs;
6. installs a **Docker Hub pull-through cache** and an **S3 store for `cache:`** for the microVMs;
7. installs **firerunner** and **gitlab-runner**, starts `firerunner.service`;
8. verifies every service and prints next steps.

Every downloaded artifact is checked against its published sha256 checksum.

![firerunner doctor](../images/cli-doctor.png)

## Options

Pass environment variables to `bash`:

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh \
  | sudo FR_DISK=/dev/sdb FR_POOL_SIZE=4 FR_METRICS_ALLOW=10.0.0.5/32 bash
```

| Variable | Default | Meaning |
|---|---|---|
| `FR_DISK` | first blank disk | disk for the thin pool — **it is wiped** |
| `FR_POOL_SIZE` | `2` | number of pre-booted microVMs |
| `FR_METRICS_ALLOW` | none | CIDR allowed to scrape `:9477`; without it metrics listen on 127.0.0.1 only |
| `FR_GITLAB_URL`, `FR_RUNNER_TOKEN` | none | register the runner during install |
| `FR_RUNNER_CONCURRENT` | `4` | parallel jobs (= microVMs in use) |
| `FR_VERSION` | `edge` | FireRunner release; `edge` is the latest `main` build, or a tag like `v1.0.0` |
| `FR_BRIDGE` | `br-fc` | bridge name |
| `FR_SUBNET` | `10.200.0` | /24 prefix for microVMs |
| `CONTAINERD_VERSION`, `FIRECRACKER_VERSION`, `FLINTLOCK_VERSION`, `GITLAB_RUNNER_VERSION`, `REGISTRY_VERSION` | pinned | component versions |

## Verify

```bash
sudo firerunner doctor               # every check must be "ok"
sudo firerunner run -- uname -a      # boots a throwaway microVM, prints the guest kernel
```

## Firewall

- On **firewalld** hosts the installer adds `br-fc` to the trusted zone and enables masquerade.
- On **ufw** hosts it allows routed traffic from `br-fc`.
- The FireRunner nftables rules (`table inet firerunner`, `table bridge firerunner`) are independent
  of firewalld/ufw and restrict what microVMs may reach on the host.

## Uninstall

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash -s -- uninstall
```

This deletes all microVMs and removes the services and binaries. The thin pool, cached images and
`/etc/firerunner` are kept; the command prints how to remove them too. The runner stays registered
in GitLab — delete it there if you no longer need it.
