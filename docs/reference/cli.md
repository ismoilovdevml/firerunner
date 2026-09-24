# CLI reference

All commands need root.

## Host

| Command | |
|---|---|
| `firerunner status` | host, services, thin pool, VM counts, runner |
| `firerunner doctor` | runs every check, prints a hint for each failure, exits 1 on failure |
| `firerunner version`, `-v`, `--version` | print the version |
| `firerunner upgrade [--version edge\|latest\|vX.Y.Z] [--check]` | replace the binary with a release (checksum-verified) and restart the daemon |

## Configuration

| Command | |
|---|---|
| `firerunner config show` | effective configuration |
| `firerunner config keys` | all settable keys |
| `firerunner config get <key>` | one value |
| `firerunner config set <key> <value>` | validate and save; YAML values (`4`, `2m`, `[a, b]`) |
| `firerunner config unset vm.kernel_cmdline.<arg>` | remove a kernel argument |

## GitLab runner

| Command | |
|---|---|
| `firerunner runner register --url URL --token glrt-... [--concurrent N] [--name NAME]` | register this host (token also from `FIRERUNNER_RUNNER_TOKEN`) |
| `firerunner runner status` | name, URL, executor, concurrent, service state |
| `firerunner runner concurrent N` | max parallel jobs |
| `firerunner runner unregister` | remove the registration from this host and GitLab |

## microVMs

| Command | |
|---|---|
| `firerunner vm list` | id, uid, role, state, size, IP |
| `firerunner vm rm <id\|uid>... \| --all` | delete |
| `firerunner vm logs <id\|uid> [-n 100]` | guest console output |
| `firerunner pool` | pre-booted VMs (JSON) |
| `firerunner pool refresh` | replace idle pool VMs (after a new guest image) |
| `firerunner run [--keep] -- <command>` | boot a throwaway VM, run a shell command, delete it (`--keep` leaves it running) |

## Used by services

| Command | |
|---|---|
| `firerunner daemon` | the long-running daemon (`firerunner.service`) |
| `firerunner executor prepare \| run <script> <stage> \| cleanup` | called by gitlab-runner |
