# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Before you open a pull request

- One change per pull request, with a commit message that says why.
- `make check` passes: formatting, `go vet`, `go mod verify` and `go mod tidy -diff`,
  golangci-lint, shellcheck, govulncheck and the tests with `-race`.
- New code comes with tests, including the failure paths (a VM that does not boot, a flintlock
  error, a job that is cancelled), not only the happy path.
- User-facing changes update the page in `docs/` that describes them.

CI runs more than `make check`. These need Docker or other tools; run the ones your change
touches:

- Shell scripts: `make shellcheck`, which runs `shellcheck -S warning` on `install.sh`,
  `images/rootfs/firerunner-netfilter`, `test/network/run.sh` and `test/installer/proxy.sh`.
- Host firewall (`install.sh`): the rules are tested in network namespaces, with and without
  br_netfilter, on a Linux host:

    ```bash
    sudo modprobe br_netfilter
    for brnf in 1 0; do
      docker run --rm --privileged -e BRNF=$brnf -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/network/run.sh
    done
    ```

- Installer proxy, CA and registry steps:
  `docker run --rm -v "$PWD":/src:ro ubuntu:24.04 bash /src/test/installer/proxy.sh`
- Alert rules (`deploy/prometheus/`): in that directory,
  `promtool check rules firerunner-alerts.yml && promtool test rules firerunner-alerts_test.yml`.
- Documentation: `mkdocs build --strict`, with the mkdocs and mkdocs-material versions in
  `.github/workflows/docs.yml`.
- Diagrams: each `docs/images/*.svg` is exported from the `.excalidraw` file next to it. Edit the
  `.excalidraw` source and export the SVG again, so the two stay the same.

## Testing on a real host

Unit tests run anywhere. Anything that boots microVMs needs a Linux host with KVM: install with
`FR_BINARY=path/to/your/firerunner` to try your build, then `sudo firerunner doctor` and
`sudo firerunner run -- uname -a`.

## Reporting bugs

Include `firerunner version`, `firerunner doctor` and the job log or `journalctl -u firerunner`
lines around the failure.
