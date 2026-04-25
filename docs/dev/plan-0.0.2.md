# plan-0.0.2

## Goal

Add filesystem persistence for every task.

For each task, the broker must create persistent markdown files:

1. `<task_id>/task.md`
2. `<task_id>/result.md`

This version keeps the existing `sync` and `async` APIs from `v0.0.1`, and adds durable task artifacts on disk.

## Scope

`v0.0.2` is about persisted task files, not a full durable queue.

Included:

1. Persist task input on create
2. Persist task result on solve
3. Keep files on disk after completion
4. Create task directories automatically

Not included:

1. Reload tasks from disk on restart
2. Rebuild in-memory queues from disk
3. Lease/retry/requeue logic
4. Status API

## Storage Layout

Use a dedicated data root directory:

```text
./task-data/<task_id>/task.md
./task-data/<task_id>/result.md
```

Rationale:

1. Preserves the requested per-task file layout
2. Avoids polluting the repository root with many task directories
3. Makes cleanup and inspection easier

Inside each task directory:

1. `task.md` contains the original task markdown exactly as submitted
2. `result.md` contains the final result markdown exactly as submitted to `solve_task`

## Core Behavior

### `create_task_sync`

On successful task creation:

1. Validate input as in `v0.0.1`
2. Create directory `./task-data/<task_id>/`
3. Write `task.md`
4. Continue with current sync delivery flow

If file persistence fails:

1. Return an application error
2. Do not register or deliver the task in memory

### `create_task_async`

On successful task creation:

1. Validate input as in `v0.0.1`
2. Create directory `./task-data/<task_id>/`
3. Write `task.md`
4. Enqueue the task in memory
5. Return `queued`

If file persistence fails:

1. Return an application error
2. Do not enqueue the task
3. Do not leave partial task state in memory

### `solve_task`

On successful solve:

1. Validate input as in `v0.0.1`
2. Look up the task in memory
3. Write `./task-data/<task_id>/result.md`
4. Only after successful write, remove the task from in-memory unresolved registry
5. Return success

If result persistence fails:

1. Return an application error
2. Keep the task unresolved in memory
3. Do not report success to the caller

## File Semantics

### `task.md`

Rules:

1. Written exactly once per task
2. Must contain the original `task_md` body only
3. Must not be rewritten by `solve_task`

### `result.md`

Rules:

1. Written by `solve_task`
2. Must contain the original `result_md` body only
3. If the task is not solved yet, the file does not exist
4. If `solve_task` is called twice for the same task, the second call should still fail with `task not found` because the task was already resolved and removed from memory

## Broker Changes

Add storage root to the broker:

```go
type Broker struct {
    mu         sync.Mutex
    listeners  map[string]chan *Task
    tasks      map[string]*Task
    asyncQueue map[string][]*Task
    dataDir    string
}
```

Recommended default:

```go
dataDir := "task-data"
```

`NewBroker` should initialize the broker with that directory and ensure it exists.

## Helper Methods

Recommended minimal helpers:

```go
func (b *Broker) taskDir(taskID string) string
func (b *Broker) persistTask(taskID, taskMD string) error
func (b *Broker) persistResult(taskID, resultMD string) error
```

Rules for implementation:

1. Use only Go stdlib
2. Create directories with `os.MkdirAll`
3. Write files atomically when practical
4. Keep helpers small and local to `broker.go` unless they clearly need a separate file

## Atomicity Requirements

The code should avoid partial success between disk and memory state.

Required ordering:

1. For create operations: persist to disk first, then add to in-memory broker state
2. For solve operations: persist result first, then remove from in-memory broker state

This keeps memory and disk behavior consistent enough for `v0.0.2` without introducing transactions.

## Path Safety

`task_id` is now used as a directory name, so path safety matters.

Validation rules to add:

1. `task_id` must not contain path separators
2. `task_id` must not be `.` or `..`
3. Keep existing non-empty validation from `v0.0.1`

If invalid, return an application error.

## API Surface

No MCP protocol changes in `v0.0.2`.

The exposed tools remain:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`

Behavioral difference:

1. task creation now also creates on-disk files
2. task completion now also creates `result.md`

## Testing

Add coverage for:

1. Creating a sync task writes `task-data/<task_id>/task.md`
2. Creating an async task writes `task-data/<task_id>/task.md`
3. Solving a task writes `task-data/<task_id>/result.md`
4. File contents match the original markdown exactly
5. Invalid `task_id` with path separators is rejected
6. If persistence fails, the task is not inserted into broker state

Keep the current integration script and extend it or add a new one.

## Acceptance Criteria

1. `create_task_sync` persists `task.md`
2. `create_task_async` persists `task.md`
3. `solve_task` persists `result.md`
4. Per-task files are stored under `./task-data/<task_id>/`
5. Existing sync behavior still works
6. Existing async behavior still works
7. Invalid path-like `task_id` values are rejected
8. `go build ./...` succeeds
9. `go vet ./...` succeeds

## Future Work

After `v0.0.2`, the next logical step is to connect persistence to recovery:

1. load unresolved async tasks from disk on startup
2. add task status inspection
3. add claim and lease semantics
4. separate archival/completed tasks from active tasks
