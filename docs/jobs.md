# Writing jobs

A FireRunner job behaves like a job on any Linux runner, except that it always starts on a new
machine that is deleted afterwards.

## Pick the runner

```yaml
default:
  tags: [firecracker]
```

## With or without `image:`

```yaml
test:
  image: node:22          # the script runs in this container, inside the job's VM
  script:
    - npm ci && npm test

build:
  script:                 # no image: the script runs on the VM as root
    - docker build -t app:$CI_COMMIT_SHORT_SHA .
```

Without `image:` you get Ubuntu 24.04 with Docker (buildx, compose), git, curl, jq, make and python3.

## Docker builds

Every VM has its own Docker daemon. No `docker:dind`, no privileged mode.

In jobs without `image:`, `docker build` runs on your project's builder, a microVM that keeps your
project's layer cache between jobs. You change nothing; the job log says:

```text
Docker layer cache: using this project's builder (warm cache)
```

The first build of a project starts its builder (about 30 s). A builder unused for a while is
deleted, but its cache is kept on the host and comes back with the project's next builder.

## Cache

`cache:` works as on any runner. The runner host keeps it per project. Only paths inside the
project can be cached, so point tool caches there:

```yaml
test:
  image: node:22
  cache:
    key: npm-$CI_COMMIT_REF_SLUG
    paths: [.npm/]
  script:
    - npm ci --cache .npm --prefer-offline
```

`pip --cache-dir .pip`, `NUGET_PACKAGES: $CI_PROJECT_DIR/.nuget`, `GOMODCACHE: $CI_PROJECT_DIR/.go`.

## Services

```yaml
integration:
  image: python:3.12
  services:
    - name: postgres:16
      variables: { POSTGRES_PASSWORD: test }
  script:
    - pg_isready -h postgres
```

Each service runs as a container in your VM, reachable by its alias (the image name without the
tag). FireRunner waits up to 30 s for its ports before your script starts.

## Private registries

Set the `DOCKER_AUTH_CONFIG` CI/CD variable. It is written into the VM before your script runs,
for `image:`, `services:` and `docker pull` alike. Internal registries with a company CA or
without TLS are set up on the host: see [Corporate networks](corporate-network.md).

## The VM

| | |
|---|---|
| Size | 2 vCPU, 2 GB RAM, 40 GB disk (the operator may change it) |
| User | `root` |
| Network | internet and your LAN through NAT; no access to other jobs' VMs |
| Hostname | `job-<CI_JOB_ID>` |

## When a job fails

The *Preparing* section shows where the job ran:
`microVM pool-551bf7 ready at 10.200.0.236 in 300ms (pool, 2 vCPU, 2048 MB)`.

| Log | Meaning |
|---|---|
| `Job failed: exit status 1` | your script failed |
| `Job failed (system failure)` | no VM could be started; retry, then tell the operator |
| `waiting for host memory` | the host is full; the job continues when a VM frees up |
| `pull access denied` | set `DOCKER_AUTH_CONFIG` |
| `service … did not open port … within 30s` | the service crashed or starts slowly; its log follows |
| `'build_script' stage will be replaced with 'step_script'` | printed by gitlab-runner for every custom executor; ignore it |
