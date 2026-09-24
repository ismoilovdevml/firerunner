# Limitations and roadmap

## Current limitations

| | Status |
|---|---|
| Docker layer cache between jobs | not shared by design; BuildKit cache in `cache:` or a registry brings a .NET build from ~30 s to ~19 s, a warm shell runner needs ~2 s — [#25](https://github.com/ismoilovdevml/firerunner/issues/25) |
| Host OS | tested on Rocky Linux 9.6; Ubuntu/Debian path not run end to end — [#7](https://github.com/ismoilovdevml/firerunner/issues/7) |
| Architecture | x86_64 only |
| Windows / macOS jobs | not supported (Linux guests only) |

## Roadmap

Tracked in [GitHub issues](https://github.com/ismoilovdevml/firerunner/issues).
