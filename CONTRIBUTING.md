# Contributing to KubeVPN

Thank you for considering contributing to KubeVPN! This document outlines the process and requirements for contributing.

## Prerequisites

- **Go 1.26+**: See [go.mod](go.mod) for exact version
- Git
- Docker (for some tests and development modes)
- Kubernetes cluster (for integration tests, optional)

## Getting Started

1. Fork and clone the repository
2. Build the project to verify your setup:

```bash
go build ./...
```

## Building

```bash
# Build binary with ldflags
make kubevpn

# Build for all platforms
make kubevpn-all
```

## Testing

### No-Cluster Tests (Windows CI compatible)

These tests do NOT require a Kubernetes cluster or kubeconfig:

```bash
make ut-no-cluster
```

### Full Integration Tests

These require a valid kubeconfig and Kubernetes cluster:

```bash
make ut
```

### Compile-Only Check

Type-check the entire tree, including integration-tagged test files, without running anything:

```bash
make ut-compile
```

### TUN Device Tests (Requires CAP_NET_ADMIN)

```bash
sudo -E env PATH="$PATH" make ut-tun
```

## Generating Code

```bash
make gen
```

## Code Conventions

- Error strings: lowercase, no trailing punctuation, wrap with `%w`
- Use `plog.G(ctx)` for logging (never `log.G(ctx)`)
- Abbreviations: all caps (UID, IP)
- See [CLAUDE.md](CLAUDE.md) for more conventions
- Run `go vet ./...` before submitting changes

### AI coding agents

If you work on this repo with an AI agent (Claude Code, Codex, Cursor, Gemini CLI, ...),
point it at [AGENTS.md](AGENTS.md) — the tool-neutral development workflow — and
[.claude/skills/kubevpn-dev/SKILL.md](.claude/skills/kubevpn-dev/SKILL.md), which
follows the open Agent Skills format.

## Architecture Overview

Before making changes, read the relevant design documents in [docs/](docs/README.md):

- **Dual Daemon**: [docs/02-dual-daemon.md](docs/02-dual-daemon.md) - User daemon (control plane) vs root daemon (data plane)
- **Connection ID**: [docs/04-connection-id.md](docs/04-connection-id.md)
- **Owner ID**: [docs/05-owner-id.md](docs/05-owner-id.md)
- **Handler Architecture**: [docs/10-handler-architecture.md](docs/10-handler-architecture.md)

## Contributing Flow

1. Create a feature branch
2. Make your changes
3. Add tests for new functionality
4. Run tests and `go vet ./...`
5. Submit a pull request
6. Update documentation if needed (user docs at kubevpn.dev, design docs in this repo)

## Documentation

- **User documentation**: Hosted at [kubevpn.dev](https://kubevpn.dev)
- **Architecture/design docs**: In the [docs/](docs/README.md) directory of this repo
- **README sync**: Keep [README.md](README.md) and [README_ZH.md](README_ZH.md) in sync (bilingual, 1:1 structure)

## Security

For security issues, see [SECURITY.md](SECURITY.md).
