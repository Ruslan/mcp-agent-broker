# BUG-002: concurrent solves can overwrite the accepted result

- **Status:** Open
- **Priority:** P1
- **Area:** `agent-broker/broker.go`, `agent-broker/store.go`
- **Found:** 2026-08-19

## Problem

`Broker.SolveTask` reads the current status first and later calls
`SQLiteStore.SaveResult`. The update performed by `SaveResult` only filters on
project and task ID:

```sql
UPDATE tasks
SET result_md = ?, status = 'solved', updated_at = ?
WHERE project_id = ? AND task_id = ?
```

Two concurrent callers can both observe `picked` and then both update the row.
The later write wins even though duplicate `solve_task` calls are documented and
tested as idempotent. The in-memory `done` channel may also deliver a different
result from the value ultimately stored in SQLite.

## Impact

The dispatcher can observe inconsistent final results, and a late or duplicated
worker response can overwrite the first accepted answer.

## Suggested fix

Make accepting a result a single compare-and-set database operation, for example
an `UPDATE ... WHERE status = 'picked'`, and return both whether the row changed
and the already stored result. Signal waiters only with the value accepted by the
database.

## Acceptance criteria

- A concurrent test submits different results for the same picked task.
- Exactly one result is accepted and persisted.
- All await/get paths report that same result.
- Sequential duplicate solves remain idempotent.

