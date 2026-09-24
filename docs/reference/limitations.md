# Limitations and roadmap

## Current limitations

| | Status |
|---|---|
| `cache:` between jobs | cache lives in the job VM only — [#26](https://github.com/ismoilovdevml/firerunner/issues/26) |
| Docker layer cache between jobs | none by design; use BuildKit registry cache — [#25](https://github.com/ismoilovdevml/firerunner/issues/25) |
| Guest kernel | flintlock's 5.10 kernel with `acpi=off` — own kernel in [#24](https://github.com/ismoilovdevml/firerunner/issues/24) |
| Host OS | tested on Rocky Linux 9.6; Ubuntu/Debian path not run end to end — [#7](https://github.com/ismoilovdevml/firerunner/issues/7) |
| Architecture | x86_64 only |
| Windows / macOS jobs | not supported (Linux guests only) |

## Roadmap

Tracked in [GitHub issues](https://github.com/ismoilovdevml/firerunner/issues).
