# Connect GitLab

## 1. Create the runner in GitLab

Choose the scope:

- **Project runner**: *Project → Settings → CI/CD → Runners → New project runner*
- **Group runner**: *Group → Build → Runners → New group runner*
- **Instance runner** (admin): *Admin → CI/CD → Runners → New instance runner*

Settings:

| Field | Value |
|---|---|
| Tags | `firecracker` (jobs opt in with `tags: [firecracker]`) |
| Run untagged jobs | **off** — otherwise every untagged job lands on FireRunner |
| Description | e.g. `firerunner-host01` |

GitLab shows a runner authentication token that starts with `glrt-`. Copy it.

## 2. Register it on the host

```bash
sudo firerunner runner register --url https://gitlab.example.com --token glrt-xxxxxxxx
```

Options: `--concurrent 4` (parallel jobs) and `--name NAME`. The token is passed to
gitlab-runner through the environment, never on the command line, and ends up in
`/etc/gitlab-runner/config.toml` (mode 0600).

Or register during install:

```bash
curl -sfL https://raw.githubusercontent.com/ismoilovdevml/firerunner/main/install.sh \
  | sudo FR_GITLAB_URL=https://gitlab.example.com FR_RUNNER_TOKEN=glrt-xxxxxxxx bash
```

## 3. Check it

```bash
sudo firerunner runner status
```

In GitLab the runner shows as **online** (green). Run a first job:

```yaml
hello:
  tags: [firecracker]
  script:
    - echo "hello from $(hostname), kernel $(uname -r)"
```

## Several hosts

Install FireRunner on each host and register each with the same runner token (or a separate
runner per host). GitLab distributes jobs between them; each host manages its own microVMs.

## Unregister

```bash
sudo firerunner runner unregister
```
