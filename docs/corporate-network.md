# Corporate networks

FireRunner works in networks where the internet is reached only through an HTTP proxy, where a
proxy inspects TLS with a company certificate, and where registries and S3 are internal. You set
it up once on the host; jobs do not change.

## What goes through the proxy

| Traffic | How it reaches the proxy |
|---|---|
| Job scripts: `curl`, `git`, `npm`, `pip`, `apt` | `http_proxy`/`https_proxy`/`no_proxy` in every SSH session of the VM |
| `image:`, `services:`, `docker pull` | the VM's Docker daemon |
| Job and service containers | the Docker CLI sets the proxy variables in every container |
| `docker build` (`RUN apk add`, `RUN npm ci`) | the Docker CLI passes the proxy as build arguments |
| Builders (base images, `ADD https://…`) | buildkitd's environment |
| Host: microVM images, the Docker Hub cache, gitlab-runner | systemd drop-ins |
| `firerunner upgrade`, the installer's downloads | the proxy, automatically |

microVMs never talk to the corporate proxy directly. They use a small forwarder on the host,
`firerunner-proxy`, at the bridge address (`10.200.0.1:3128`). It adds the proxy's credentials,
so no job can read the proxy password, and it accepts connections only from microVMs and the host
itself. Each request is logged with the client address, target, status and size
(`journalctl -u firerunner-proxy`).

## Set it up

Pass the settings to the installer (new host or existing one; they are kept for later runs):

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo \
  FR_PROXY='http://proxy.corp.example:3128' \
  FR_NO_PROXY='gitlab.corp.example,.corp.example,10.0.0.0/8' \
  bash
sudo firerunner doctor
```

Pre-booted VMs are replaced by ones with the new settings on their own. Builders are replaced
when idle; their caches are kept.

| Variable | |
|---|---|
| `FR_PROXY` | `http://host:port` of the corporate proxy (for a password, see [below](#a-proxy-with-a-password)) |
| `FR_NO_PROXY` | hosts, `.domains` and CIDRs reached directly, comma-separated |
| `FR_CA_FILE` | PEM file with the company root CA, see [below](#company-ca-and-tls-inspection); `none` removes it |
| `FR_INSECURE_REGISTRIES` | registries without a checkable certificate, see [below](#internal-registries) |

## A proxy with a password

Put the URL with the password in `/etc/firerunner/proxy-upstream` yourself, then run the
installer without `FR_PROXY`: it finds the file. A password on the command line would be visible
to other users in the process list and would stay in your shell history.

```bash
sudo install -D -m 600 /dev/null /etc/firerunner/proxy-upstream
sudoedit /etc/firerunner/proxy-upstream     # one line: http://CORP%5Cbob:p%40ss%3Aw%2Frd@proxy.corp.example:3128
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo \
  FR_NO_PROXY='gitlab.corp.example,.corp.example,10.0.0.0/8' bash
```

Write the user and password %-encoded: `p@ss:w/rd` becomes `p%40ss%3Aw%2Frd`, a domain user
`CORP\bob` becomes `CORP%5Cbob`. The forwarder uses the file only while it is a regular file
owned by root and readable by nobody else.

The file is read for every connection, so a new password takes effect at once: edit it with
`sudoedit`. If the proxy refuses the credentials, jobs get `502 Bad Gateway` and
`journalctl -u firerunner-proxy` says `upstream proxy refused the credentials`. The refused
credentials are not sent again for 5 minutes, or until you change the file, so a changed password
cannot lock the account with a flood of failed logins.

The forwarder speaks Basic authentication. For NTLM or Kerberos proxies (Windows domains), run
[CNTLM](https://cntlm.sourceforge.net/) or [px](https://github.com/genotrance/px) on the host on
a port other than 3128 (the forwarder's), and put `http://127.0.0.1:<their port>` in the file.

## What bypasses the proxy

These are never sent to the proxy: `localhost`, the bridge address and subnet (the Docker Hub
cache, `cache:` storage and builders live there), the Docker networks inside the VM, and the
aliases of the job's `services:` (a job reaches `http://minio:9000` directly).

Add everything internal that jobs use: your GitLab, internal registries, S3, package mirrors.
A missing entry sends internal traffic to the proxy, which usually cannot reach it.

```bash
sudo firerunner config set proxy.no_proxy 'gitlab.corp.example,.corp.example,10.0.0.0/8'
```

New microVMs use the new list at once. Host services (containerd, gitlab-runner) get it when you
run the installer again with the same `FR_NO_PROXY`.

CIDRs work for Go programs, curl 7.86+ and Docker. Some tools (wget, older curl) only match names,
so list internal hosts by name as well.

## What jobs can reach through the proxy

Proxied traffic leaves from the host, so the forwarder applies the host's rules for microVMs
itself. It refuses, with `403`, requests to:

- the host and other microVMs: loopback, the bridge network and the host's own addresses;
- cloud metadata and other link-local addresses;
- the networks in `FR_EGRESS_DENY` (config `network.egress_deny`);
- `localhost` names and IP addresses written in other forms, such as `2130706433`;
- host names that resolve to any of these.

HTTPS tunnels (`CONNECT`) go only to port 443. Allow more with
`sudo firerunner config set proxy.connect_ports '[443, 8443]'`.

The forwarder checks a host name when it resolves it, and the corporate proxy resolves it again,
so a name whose DNS answer changes in between can still get through. Restrict the runner's
account on the corporate proxy as well.

Each microVM address can hold 256 connections through the forwarder, all microVMs together 4096;
host services have a budget of their own, so busy jobs cannot cut containerd or gitlab-runner off.
A tunnel, or an answer, without data for 15 or 5 minutes is closed.

## Company CA and TLS inspection

When a proxy decrypts TLS, or internal services use certificates signed by a company CA, give
FireRunner the root CA:

```bash
sudo FR_CA_FILE=/path/to/company-root.pem bash install.sh
```

It is trusted by the host, by every microVM and its Docker, by builders, and by job and service
containers. Containers get the VM's bundle (public roots plus your CA) at
`/etc/firerunner/ca-bundle.crt`, and `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`,
`PIP_CERT`, `GIT_SSL_CAINFO` and `NODE_EXTRA_CA_CERTS` point at it, which covers OpenSSL, Go, curl,
Python, pip, git and Node.js.

Not covered automatically:

- `RUN` steps of `docker build` use the certificates of the image you build. Add the CA in the
  Dockerfile, for example `COPY company-root.pem /usr/local/share/ca-certificates/company-root.crt`
  and `RUN update-ca-certificates`.
- Java reads its own keystore: import the CA with `keytool` in the job or the image.

## Internal registries

A registry with a certificate from a public or company CA needs nothing (see the CA above). For
registries whose certificate cannot be checked, or that speak plain HTTP:

```bash
sudo FR_INSECURE_REGISTRIES='harbor.corp.example:443,http://10.0.0.5:5000' bash install.sh
```

`host:port` means TLS without a certificate check, `http://host:port` plain HTTP. Both the VM's
Docker (`image:`, `docker pull`, `docker push`) and builders use them. Credentials come from the
job's `DOCKER_AUTH_CONFIG` as usual.

## `cache:` in S3

The built-in `cache:` store is on the host and needs nothing. To use your own S3 (MinIO, Ceph,
AWS):

```bash
sudo firerunner runner cache s3 --server s3.corp.example:9000 --bucket gitlab-cache [--insecure]
```

`--insecure` means plain HTTP. Jobs download and upload the cache from the microVM, so an internal
S3 must be in `proxy.no_proxy`, and one signed by a company CA needs `FR_CA_FILE`.

## Check it

`sudo firerunner doctor` checks the forwarder, the proxy file, that the proxy is reachable, a
real HTTPS request through it, and the CA file. Then run a job, or on the host:

```bash
sudo firerunner run -- sh -c 'env | grep -i proxy; curl -sI https://github.com | head -1'
```

## Limits

- The corporate proxy must be an HTTP proxy (`http://`). SOCKS proxies and proxies reached over
  TLS (`https://`) are not supported. Traffic to `https://` sites is still end-to-end TLS.
- A host without any internet access (air-gapped) is not supported: the installer and the
  microVM images are downloaded from GitHub and GHCR.
- Rolling back to v0.1.0 or older: while the proxy, `vm.ca_file` and insecure registries are
  unused, `config.yaml` stays readable by older versions. Once you use them, restore the
  `config.yaml` from before the upgrade together with the older binary.

## Upgrades and restarts

`firerunner upgrade` leaves `firerunner-proxy` running on the previous build, because a restart
cuts the open tunnels of running jobs. Restart it when the runner is idle:
`sudo systemctl restart firerunner-proxy`.

The installer is careful in the same way. While jobs run it does not restart the forwarder,
containerd, flintlockd, the Docker Hub cache or gitlab-runner, and does not upgrade gitlab-runner.
It lists what it left and keeps it in `/var/lib/firerunner/pending-restarts`; run the installer
again when the runner is idle and it catches up. Removing the proxy stops the forwarder only after
the services that used it were restarted.
