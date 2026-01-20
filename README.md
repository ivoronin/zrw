# zrw

**z**ellij **r**un --**w**ait — run commands in Zellij panes with exit code propagation

[![CI](https://github.com/ivoronin/zrw/actions/workflows/test.yml/badge.svg)](https://github.com/ivoronin/zrw/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/ivoronin/zrw)](https://github.com/ivoronin/zrw/releases)

## Table of Contents

[Why This Exists](#why-this-exists) · [Overview](#overview) · [Features](#features) · [Installation](#installation) · [Usage](#usage) · [Requirements](#requirements) · [License](#license)

```bash
# zellij run spawns a pane and returns immediately - no waiting, no exit code
zellij run -- make build  # Returns instantly, build runs in background

# zrw waits for completion and propagates the exit code
zrw -- make build && echo "Build succeeded"  # Waits, then chains on actual result
```

## Why This Exists

Zellij's `zellij run` command executes commands in new panes but returns immediately without waiting for completion or providing an exit code. This breaks scripting workflows where you need to chain commands based on success/failure. zrw fills this gap.

## Overview

zrw wraps Zellij's `run` command to enable waiting and exit code propagation between panes. It operates as a parent-child pair communicating over Unix sockets using gob encoding. The parent process creates a temporary socket, launches Zellij with the child zrw as the command, then waits for the child to execute the actual command and report its exit code. SIGINT signals are forwarded to the child process.

## Features

- Waits for command completion in Zellij panes
- Exit code propagation to calling shell
- Full environment variable passthrough to child process
- Working directory preservation across panes
- SIGINT forwarding to child process
- All `zellij run` options supported (floating panes, sizing, naming)
- Cross-platform: Linux and macOS (amd64 and arm64)

## Installation

### GitHub Releases

Download from [Releases](https://github.com/ivoronin/zrw/releases).

### Homebrew

```bash
brew install ivoronin/ivoronin/zrw
```

## Usage

```bash
zrw -- cargo build && zrw -- cargo test   # Build then test, stop on failure
zrw -f -- kubectl logs -f deployment/api  # Watch logs in floating pane
zrw -n "migrations" -- alembic upgrade head && zrw -- pytest tests/integration
```

All `zellij run` options are supported. See `zellij run --help` for available options.

```bash
zrw --version
```

## Requirements

- [Zellij](https://zellij.dev/) terminal multiplexer installed and available in PATH
- Must be run from within a Zellij session

## License

[GPL-3.0](LICENSE)
