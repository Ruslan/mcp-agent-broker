# BUG-004: `make test` references a missing integration script

- **Status:** Open
- **Priority:** P1
- **Area:** `Makefile`, test infrastructure
- **Found:** 2026-08-19

## Problem

The documented project test target runs:

```make
bash .gemini/test_v0.0.3.sh
```

That file is not present in the current Git tree or its reachable history.
Consequently `make test` builds the UI and broker, completes all Go tests, and
then exits with status 127.

## Impact

The canonical verification command can never pass. It also makes future CI
ambiguous: removing the line loses the intended end-to-end coverage, while
leaving it prevents a green build.

## Suggested fix

Either restore a maintained HTTP/MCP integration test at a stable path or remove
the stale command and make the existing Go HTTP tests the declared integration
suite. Add CI that runs the chosen canonical command.

## Acceptance criteria

- `make test` succeeds from a clean checkout.
- The target does not depend on untracked or user-local files.
- At least one test exercises a complete create → pick → solve → read lifecycle
  through HTTP handlers or a running broker.

