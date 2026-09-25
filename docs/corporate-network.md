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
itself.

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
| `FR_PROXY` | `http://[user:password@]host:port` of the corporate proxy |
| `FR_NO_PROXY` | hosts, `.domains` and CIDRs reached directly, comma-separated |
| `FR_CA_FILE` | PEM file with the company root CA, see [below](#company-ca-and-tls-inspection) |
| `FR_INSECURE_REGISTRIES` | registries without a checkable certificate, see [below](#internal-registries) |

## A proxy with a password

Write the password %-encoded: `p@ss:w/rd` becomes `p%40ss%3Aw%2Frd`, a domain user `CORP\bob`
becomes `CORP%5Cbob`.

```bash
sudo FR_PROXY='http://CORP%5Cbob:p%40ss%3Aw%2Frd@proxy.corp.example:3128' bash install.sh
```

The URL is stored in `/etc/firerunner/proxy-upstream`, readable by root only; the forwarder
refuses to use it if others can read it. It is read for every connection, so a new password takes
effect at once:

```bash
echo 'http://CORP%5Cbob:NEW%21pass@proxy.corp.example:3128' | sudo tee /etc/firerunner/proxy-upstream >/dev/null
```

If the proxy refuses the credentials, jobs get `502 Bad Gateway` and
`journalctl -u firerunner-proxy` says `upstream proxy refused the credentials`.

The forwarder speaks Basic authentication. For NTLM or Kerberos proxies (Windows domains), run
[CNTLM](https://cntlm.sourceforge.net/) or [px](https://github.com/genotrance/px) on the host and
set `FR_PROXY=http://127.0.0.1:<their port>`.

## What bypasses the proxy

These are never sent to the proxy: `localhost`, the bridge address and subnet (the Docker Hub
cache, `cache:` storage and builders live there), the Docker networks inside the VM, and the
aliases of the job's `services:` (a job reaches `http://minio:9000` directly).

Add everything internal that jobs use: your GitLab, internal registries, S3, package mirrors.
A missing entry sends internal traffic to the proxy, which usually cannot reach it.

```bash
sudo firerunner config set proxy.no_proxy 'gitlab.corp.example,.corp.example,10.0.0.0/8'
```

CIDRs work for Go programs, curl 7.86+ and Docker. Some tools (wget, older curl) only match names,
so list internal hosts by name as well.

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
- Rolling back to a FireRunner version older than this feature needs the `config.yaml` from
  before the upgrade: older versions refuse the `proxy` keys.
