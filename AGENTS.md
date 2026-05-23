# AGENTS.md

## Project overview

See [docs/dev/codebase-overview.md](docs/dev/codebase-overview.md) for architecture, database schema, API endpoints, and source file roles.

## Build / test commands

```bash
make ui-build      # Build frontend (Svelte → agent-broker/dist/)
make build         # Build Go binary
make run           # Run broker locally
make test          # Run Go tests
```
