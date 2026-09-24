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

!!! tip "Build speed"
    Every job starts with an empty Docker layer cache. Base images from Docker Hub come from a
    cache on the runner host; ask your operator to add frequently used non-Docker-Hub images to
    `pool.preload_images`. For layer caching use BuildKit's registry cache:
    `docker buildx build --cache-from type=registry,ref=$CI_REGISTRY_IMAGE:cache --cache-to type=registry,ref=$CI_REGISTRY_IMAGE:cache,mode=max ...`

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
| `services:` | supported | not yet ([#27](https://github.com/ismoilovdevml/firerunner/issues/27)) |
| `cache:` between jobs | local | not yet ([#26](https://github.com/ismoilovdevml/firerunner/issues/26)) |
| Docker layer cache | shared on the host | per job (see tip above) |

## Resources

Each job VM has **2 vCPU and 2 GB RAM** by default (operators can change this per host). If your
job needs more, ask the runner operator or use a runner with a different tag.
