# BUG-003: SQLite foreign-key enforcement is not enabled

- **Status:** Open
- **Priority:** P2
- **Area:** `agent-broker/store.go`
- **Found:** 2026-08-19

## Problem

The `task_progress` table declares a composite foreign key with
`ON DELETE CASCADE`, but `NewSQLiteStore` enables only WAL mode. SQLite does not
enforce foreign keys unless `PRAGMA foreign_keys = ON` is enabled for the
connection.

As a result, `SQLiteStore.DeleteTask` may delete only the task row while leaving
its progress rows behind.

## Impact

Repeated task deletion can accumulate orphaned progress data. The database
schema promises stronger integrity than the running configuration provides.

## Suggested fix

Enable `PRAGMA foreign_keys = ON` immediately after opening the database and
verify its value. Since the store deliberately uses one open connection, the
connection-scoped pragma is sufficient for the current implementation.

## Acceptance criteria

- A store test asserts `PRAGMA foreign_keys` is enabled.
- Deleting a task also deletes its `task_progress` rows.
- Existing databases open and migrate without manual intervention.

