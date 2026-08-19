# Bug backlog

Open defects found during the 2026-08-19 codebase review. Each defect has its
own file so it can be discussed, implemented, and closed independently.

| ID | Priority | Area | Summary |
|----|----------|------|---------|
| [BUG-001](bug-001-await-timeout-closed-progress.md) | P1 | Broker | `await_task` can loop forever when timeout races with solve |
| [BUG-002](bug-002-concurrent-solve-overwrites-result.md) | P1 | Broker / SQLite | Concurrent solves can overwrite the accepted result |
| [BUG-003](bug-003-sqlite-foreign-keys-disabled.md) | P2 | SQLite | `ON DELETE CASCADE` is declared but foreign-key enforcement is not enabled |
| [BUG-004](bug-004-make-test-missing-integration-script.md) | P1 | Build / test | `make test` always fails after the Go suite because its integration script is absent |
| [BUG-005](bug-005-ui-dependency-audit.md) | P2 | Admin UI | Locked frontend dependencies currently fail `npm audit` |

Priority convention:

- **P1** — can break correctness or the main verification workflow; address before
  relying on the affected path.
- **P2** — important security, data-integrity, or maintenance debt; schedule soon.
- **P3** — limited-impact issue or cleanup.
