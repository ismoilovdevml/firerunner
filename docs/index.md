# FireRunner

Run every GitLab CI job in its own Firecracker microVM, on your own hardware.

FireRunner is a GitLab Runner [custom executor](https://docs.gitlab.com/runner/executors/custom/).
gitlab-runner picks up jobs as usual. For each job FireRunner hands out a new microVM, runs every
stage in it and deletes it when the job ends.

| | Shell executor | Docker executor | FireRunner |
|---|---|---|---|
| Isolation | none | containers, shared kernel | own VM and kernel per job |
| Leftovers from earlier jobs | files, processes, images | images, volumes | none |
| `docker build` | host Docker | privileged DinD or socket | Docker in the VM, not privileged |
| Start | instant | image pull | 0.3 s from pre-booted VMs |

## Quick start

1. On a Linux host with `/dev/kvm` and a blank disk:

    ```bash
    curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh | sudo bash
    ```

2. In GitLab, create a runner with the tag `firecracker` and copy its `glrt-` token.
3. Register it and check the host:

    ```bash
    sudo firerunner runner register --url https://gitlab.example.com --token glrt-...
    sudo firerunner doctor
    ```

4. Send a job to it:

    ```yaml
    test:
      tags: [firecracker]
      script:
        - echo "running in $(hostname)"
    ```

## Where next

- Writing `.gitlab-ci.yml`: [Writing jobs](jobs.md)
- Running the hosts: [Install](install.md), [Configuration](configuration.md), [Operations](operations.md)
- Behind a proxy or with internal registries: [Corporate networks](corporate-network.md)
- How it works and what it protects: [How it works](how-it-works.md)
- Commands, metrics and limits: [Reference](reference.md)
