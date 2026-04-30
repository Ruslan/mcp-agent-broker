# Plan 0.0.10: SQLite Persistence

## Goal

Replace the filesystem persistence layer (`task.md` / `result.md` / `status.json` per task directory) with a single SQLite database. The in-memory channel architecture (`Broker.tasks`, `Broker.listeners`, `Broker.asyncQueue`) is untouched — SQLite only replaces disk I/O.

## Motivation

- `ListTasks` currently does a full directory scan + N file reads — O(N) syscalls
- Filtering by role/status requires reading every `status.json`
- Atomic updates across multiple files (e.g. status + result) are not guaranteed
- SQLite gives transactions, indexed queries, and a single file to back up

## Schema

```sql
CREATE TABLE IF NOT EXISTS tasks (
    project_id TEXT    NOT NULL,
    task_id    TEXT    NOT NULL,
    role       TEXT    NOT NULL,
    title      TEXT    NOT NULL,
    status     TEXT    NOT NULL DEFAULT 'queued',
    task_md    TEXT    NOT NULL,
    result_md  TEXT,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (project_id, task_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_project_role   ON tasks (project_id, role);
CREATE INDEX IF NOT EXISTS idx_tasks_project_status ON tasks (project_id, status);
```

All timestamps stored as ISO 8601 UTC strings (`time.RFC3339`). `result_md` is NULL until the task is solved.

## Dependency

Use `modernc.org/sqlite` — pure Go, no CGO. Single new external dependency.

```
go get modernc.org/sqlite
```

## Implementation

### New file: `agent-broker/store.go`

Encapsulates all SQLite logic behind an interface so the broker doesn't import the driver directly and tests can substitute an in-memory DB.

```go
type Store interface {
    CreateTask(projectID, taskID, role, title, taskMD string, now time.Time) error
    UpdateStatus(projectID, taskID, status string, now time.Time) error
    SaveResult(projectID, taskID, resultMD string, now time.Time) error
    GetStatus(projectID, taskID string) (*StatusMetadata, error)
    GetTaskMD(projectID, taskID string) (string, error)
    GetResult(projectID, taskID string) (string, error)
    ListTasks(projectID, role, status string) ([]StatusMetadata, error)
    Close() error
}
```

`SQLiteStore` implements `Store`. Constructor:

```go
func NewSQLiteStore(path string) (*SQLiteStore, error)
```

Pass `":memory:"` in tests for a fast, isolated DB.

### Changes in `broker.go`

Replace the `dataDir` field and all file-based methods:

| Removed | Replaced by |
|---------|-------------|
| `dataDir string` | `store Store` |
| `persistTask()` | `store.CreateTask()` |
| `defaultPersistStatus()` | `store.UpdateStatus()` |
| `persistResult()` | `store.SaveResult()` |
| `GetTaskStatus()` | `store.GetStatus()` |
| `GetTaskMD()` | `store.GetTaskMD()` |
| `GetTaskResult()` | `store.GetResult()` |
| `ListTasks()` | `store.ListTasks()` |
| `taskDir()` | removed |

`NewBroker` signature changes: `dataDir string` → `store Store`. Callers (`main.go`, tests) construct the store before calling `NewBroker`.

The `persistStatus` hook used in tests is replaced by injecting a `Store` mock — cleaner than the current function-pointer approach.

### Changes in `main.go`

```go
dbPath := os.Getenv("DB_PATH")
if dbPath == "" {
    dbPath = "broker.db"
}
store, err := NewSQLiteStore(dbPath)
if err != nil {
    log.Fatalf("Failed to open database: %v", err)
}
defer store.Close()

broker, err := NewBroker(store, promptsDir, enableSync, enableAsync)
```

### Transactions

`SolveTask` currently does two separate file writes (result + status). In SQLite, wrap both in one transaction:

```sql
BEGIN;
UPDATE tasks SET result_md = ?, status = 'solved', updated_at = ? WHERE project_id = ? AND task_id = ?;
COMMIT;
```

This eliminates the partial-write failure mode.

`CreateTask` similarly writes task content and initial status atomically.

### ID collision check

Currently `persistTask` detects ID collision via `O_EXCL` file creation. In SQLite, the PRIMARY KEY constraint handles this — `INSERT OR FAIL` returns an error on collision. The 5-attempt retry loop in `CreateTask` remains.

## Migration

No automatic migration from the old filesystem layout. Projects with existing `data/` directories must be migrated manually or start fresh. Provide a one-off migration script (`scripts/migrate_fs_to_sqlite.go`) that:

1. Walks `data/<project_id>/<task_id>/`
2. Reads `status.json`, `task.md`, `result.md`
3. Inserts rows into the SQLite DB

## Testing

- `NewSQLiteStore(":memory:")` in all broker tests — no `os.MkdirTemp` / `defer os.RemoveAll` needed
- The `persistStatus` hook in `broker.go` is removed; tests that simulate disk failures use a `Store` mock instead
- Add `store_test.go` with unit tests for each `Store` method

## Affected files

| File | Change |
|------|--------|
| `agent-broker/store.go` | New: `Store` interface + `SQLiteStore` implementation |
| `agent-broker/broker.go` | Remove file I/O, use `store Store` field |
| `agent-broker/main.go` | Construct `SQLiteStore`, pass to `NewBroker`, add `DB_PATH` env var |
| `agent-broker/broker_test.go` | Use `:memory:` store, remove `os.MkdirTemp` |
| `agent-broker/broker_extra_test.go` | Same |
| `agent-broker/*_test.go` | Update `NewBroker` call sites |
| `go.mod` / `go.sum` | Add `modernc.org/sqlite` |
| `scripts/migrate_fs_to_sqlite.go` | New: one-off migration helper |

## Open questions

1. **WAL mode** — enable `PRAGMA journal_mode=WAL` for better concurrent read performance? Probably yes given multiple agents querying simultaneously.
2. **Connection pool** — `database/sql` manages a pool; set `db.SetMaxOpenConns(1)` to serialize writes and avoid SQLite locking errors, or use WAL with multiple readers.
3. **`DB_PATH` in tests** — the `:memory:` store is per-connection; if tests share a broker instance ensure they use the same connection.
