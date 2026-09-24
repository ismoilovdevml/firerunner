# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Before you open a pull request

- One change per pull request, with a commit message that says why.
- `make check` passes: formatting, `go vet`, golangci-lint, govulncheck and the tests with `-race`.
- New code comes with tests, including the failure paths (a VM that does not boot, a flintlock
  error, a job that is cancelled), not only the happy path.
- Changes to `install.sh` pass `shellcheck -S warning install.sh`.
- User-facing changes update the page in `docs/` that describes them.

## Testing on a real host

Unit tests run anywhere. Anything that boots microVMs needs a Linux host with KVM: install with
`FR_BINARY=path/to/your/firerunner` to try your build, then `sudo firerunner doctor` and
`sudo firerunner run -- uname -a`.

## Reporting bugs

Include `firerunner version`, `firerunner doctor` and the job log or `journalctl -u firerunner`
lines around the failure.
