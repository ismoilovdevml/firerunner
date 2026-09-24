# Limitations and roadmap

## Current limitations

| | Status |
|---|---|
| Docker layer cache | per project (builder microVM); the first build of a project, and the first after `builder.idle_ttl`, is cold |
| Host OS | tested on Rocky Linux 9.6; Ubuntu/Debian path not run end to end — [#7](https://github.com/ismoilovdevml/firerunner/issues/7) |
| Architecture | x86_64 only |
| Windows / macOS jobs | not supported (Linux guests only) |

## Roadmap

Tracked in [GitHub issues](https://github.com/ismoilovdevml/firerunner/issues).
