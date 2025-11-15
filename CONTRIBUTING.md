# Contributing to FireRunner

Thank you for your interest in contributing to FireRunner! This document provides guidelines and instructions for contributing.

## Code of Conduct

Please be respectful and constructive in all interactions. We aim to maintain a welcoming and inclusive community.

## How to Contribute

### Reporting Bugs

1. Check if the bug has already been reported in [Issues](https://github.com/ismoilovdevml/firerunner/issues)
2. If not, create a new issue with:
   - Clear title and description
   - Steps to reproduce
   - Expected vs actual behavior
   - Environment details (OS, Go version, etc.)
   - Relevant logs or screenshots

### Suggesting Features

1. Check existing [Feature Requests](https://github.com/ismoilovdevml/firerunner/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement)
2. Create a new issue describing:
   - The problem your feature would solve
   - Proposed solution
   - Alternative solutions considered
   - Any implementation ideas

### Pull Requests

1. **Fork the repository**
   ```bash
   git clone https://github.com/ismoilovdevml/firerunner.git
   cd firerunner
   ```

2. **Create a feature branch**
   ```bash
   git checkout -b feature/my-amazing-feature
   ```

3. **Make your changes**
   - Write clean, documented code
   - Follow Go best practices
   - Add tests for new functionality
   - Update documentation as needed

4. **Test your changes**
   ```bash
   make test
   make lint
   ```

5. **Commit your changes**
   ```bash
   git commit -m "feat: add amazing feature"
   ```

   Follow [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat:` - New feature
   - `fix:` - Bug fix
   - `docs:` - Documentation changes
   - `refactor:` - Code refactoring
   - `test:` - Test additions/changes
   - `chore:` - Maintenance tasks

6. **Push to your fork**
   ```bash
   git push origin feature/my-amazing-feature
   ```

7. **Create a Pull Request**
   - Provide clear description of changes
   - Reference related issues
   - Ensure CI checks pass
   - Request review from maintainers

## Development Setup

### Prerequisites

- Go 1.21 or later
- Docker (for building VM images)
- Make
- Git

### Local Development

1. **Clone and setup**
   ```bash
   git clone https://github.com/ismoilovdevml/firerunner.git
   cd firerunner
   make deps
   ```

2. **Run tests**
   ```bash
   make test
   ```

3. **Build**
   ```bash
   make build
   ```

4. **Run locally**
   ```bash
   # Start Flintlock (in separate terminal)
   flintlockd run --config config/flintlock.yaml

   # Start FireRunner
   make run
   ```

### Code Style

- Follow standard Go formatting: `make fmt`
- Use meaningful variable/function names
- Add comments for complex logic
- Keep functions focused and small
- Write self-documenting code

### Testing

- Write unit tests for all new code
- Aim for >80% code coverage
- Include integration tests for major features
- Test error paths and edge cases

Example test:
```go
func TestVMManager_CreateVM(t *testing.T) {
    manager := NewManager(client, config, logger)

    req := &VMRequest{
        JobID: "123",
        VCPU: 2,
        MemoryMB: 4096,
    }

    vm, err := manager.CreateVM(context.Background(), req)
    assert.NoError(t, err)
    assert.NotNil(t, vm)
    assert.Equal(t, "123", vm.Metadata["job_id"])
}
```

## Project Structure

```
firerunner/
├── cmd/
│   └── firerunner/      # Main application
├── pkg/
│   ├── config/          # Configuration management
│   ├── firecracker/     # VM lifecycle management
│   ├── gitlab/          # GitLab integration
│   ├── scheduler/       # Job scheduling
│   └── metrics/         # Monitoring
├── images/              # VM images (kernel, rootfs)
├── deploy/              # Deployment configs
├── docs/                # Documentation
└── examples/            # Example configurations
```

## Documentation

- Update README.md for user-facing changes
- Add godoc comments for exported functions
- Update docs/ for architectural changes
- Include examples for new features

## Release Process

1. Version bump in appropriate files
2. Update CHANGELOG.md
3. Create and push git tag
4. CI will automatically build and release

## Questions?

- Open a [Discussion](https://github.com/ismoilovdevml/firerunner/discussions)
- Join our community chat (link TBD)
- Email: firerunner@example.com

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.

---

Thank you for contributing to FireRunner! 🔥
