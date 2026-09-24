# Writing jobs

This page is for engineers who write `.gitlab-ci.yml`. You do not need to know how FireRunner
works inside — a job behaves like a job on a normal Linux runner, except that it always starts
on a clean machine that is thrown away afterwards.

## Send a job to FireRunner

Add the runner's tag:

```yaml
unit-tests:
  tags: [firecracker]
  script:
    - make test
```

To send every job of a pipeline there:

```yaml
default:
  tags: [firecracker]
```

## Two ways to run your script

=== "With `image:` (like the docker executor)"

    ```yaml
    test:
      tags: [firecracker]
      image: node:22-alpine
      script:
        - npm ci
        - npm test
    ```

    Your `before_script`, `script` and `after_script` run **inside that container**, which runs
    inside your job's microVM. The repository is checked out at the usual path and mounted into
    the container. `bash` is used when the image has it, otherwise `sh`.

    The container uses the VM's network directly (`--network host`).

=== "Without `image:` (like the shell executor)"

    ```yaml
    build:
      tags: [firecracker]
      script:
        - docker build -t my-app:$CI_COMMIT_SHORT_SHA .
        - docker run --rm my-app:$CI_COMMIT_SHORT_SHA ./selftest
    ```

    Your script runs directly in the microVM as `root`, on Ubuntu 24.04 with Docker, git and the
    usual command-line tools. See [Inside the job VM](job-environment.md).

## Docker builds

Docker is available in every job VM — no `docker:dind` service, no `privileged` flag, no socket
mount. Each job has its own Docker daemon that is deleted with the VM.

```yaml
image-build:
  tags: [firecracker]
  script:
    - echo "$CI_REGISTRY_PASSWORD" | docker login -u "$CI_REGISTRY_USER" --password-stdin "$CI_REGISTRY"
    - docker build -t "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA" .
    - docker push "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA"
```

`docker buildx` and `docker compose` are installed.

### Layer cache: nothing to do

In jobs without `image:`, `docker build` runs on your **project's builder**: a BuildKit microVM
that belongs to your project only and keeps its layer cache between jobs. The job log says
which one was used:

```text
Docker layer cache: using this project's builder (warm cache)
```

The built image is loaded into the job VM as usual, so `docker push`, `docker run` and
`docker image ls` work unchanged. Measured on a .NET service: 2 s with a warm builder, same as a
shell runner with its host cache (26–30 s without any cache).

The first job of a project starts the builder and builds without it. A builder that nobody used
for 12 hours is deleted together with its cache.

### Layer cache for jobs with `image:`

Jobs that run in an `image:` container have no Docker daemon. If such a job builds images with a
tool of its own, keep the cache in `cache:` or in a registry:

```yaml
image-build:
  tags: [firecracker]
  cache:
    key: buildx-$CI_COMMIT_REF_SLUG
    paths: [.buildx-cache/]
  script:
    - docker buildx build
        --cache-from type=local,src=.buildx-cache
        --cache-to type=local,dest=.buildx-cache-new,mode=max
        -t "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA" --push .
    - rm -rf .buildx-cache && mv .buildx-cache-new .buildx-cache
```

Or keep it in your registry instead: `--cache-from type=registry,ref=$CI_REGISTRY_IMAGE:cache
--cache-to type=registry,ref=$CI_REGISTRY_IMAGE:cache,mode=max`.

## Cache

`cache:` works like on any runner. The runner host stores it (per project) and restores it in the
next pipeline:

```yaml
test:
  tags: [firecracker]
  image: node:22-alpine
  cache:
    key: npm-$CI_COMMIT_REF_SLUG
    paths: [.npm/]
  script:
    - npm ci --cache .npm --prefer-offline
    - npm test
```

Only paths inside the project directory can be cached (same as on other executors). Point tool
caches there: `npm --cache .npm`, `pip --cache-dir .pip-cache`, `NUGET_PACKAGES: $CI_PROJECT_DIR/.nuget`,
`GOMODCACHE: $CI_PROJECT_DIR/.go`, `GRADLE_USER_HOME: $CI_PROJECT_DIR/.gradle`.

## Services

`services:` work like on the docker executor: each service runs as a container in your job's VM
and is reachable under its alias (default: the image name without tag, e.g. `postgres`).

```yaml
integration:
  tags: [firecracker]
  image: python:3.12
  services:
    - name: postgres:16
      variables:
        POSTGRES_PASSWORD: test
    - name: redis:7
      alias: cache
  script:
    - pg_isready -h postgres
    - python -m pytest
```

FireRunner waits up to 30 s for each port a service image exposes before your script starts. The
alias also resolves in jobs without `image:` (it is written to `/etc/hosts` of the VM).

## Private images

Set the standard `DOCKER_AUTH_CONFIG` CI/CD variable (project or group level). FireRunner writes
it into the job VM (`/root/.docker/config.json`) before your script runs, so both `image:` and
`docker pull` in scripts can use private registries.

## Artifacts and reports

`artifacts:`, `reports:` and `dependencies:` work as usual.

## What is different from the docker executor

| | docker executor | FireRunner |
|---|---|---|
| Isolation | container on a shared kernel | **own VM and kernel per job** |
| Docker-in-Docker | `docker:dind` + privileged | **built in, not privileged** |
| Leftovers from previous jobs | possible (volumes, cache) | **never** |
| `services:` | supported | supported (containers in the job VM) |
| `cache:` between jobs | local | stored on the runner host (S3), per project |
| Docker layer cache | shared on the host | through `cache:` or your registry (see above) |

## Resources

Each job VM has **2 vCPU and 2 GB RAM** by default (operators can change this per host). If your
job needs more, ask the runner operator or use a runner with a different tag.
