# plan-0.0.3

## Goal

Make async tasks observable and retrievable through MCP.

The broker should:

1. accept a new task without requiring the caller to invent `task_id`
2. generate and return a server-side task UUID
3. let the caller later query task status and final result through MCP

This makes async mode a real request/reply workflow instead of fire-and-forget.

## Why

The current async flow has a gap:

1. caller submits a task
2. worker picks it up and solves it
3. original caller has no MCP tool to ask whether the task is done and what the result was

`v0.0.3` fixes that by turning the broker into a queryable mailbox with persisted task identity and result lookup.

## Core Decision

Yes, this is the logical next step.

Required behavior:

1. `create_task_async` returns a generated `task_id`
2. caller stores that `task_id`
3. caller later uses MCP to ask for task status and result
4. broker returns the persisted result when available

## Scope

Included:

1. Server-generated task IDs for async create
2. Task status query via MCP
3. Task result query via MCP
4. Persisted result lookup backed by the `v0.0.2` filesystem layout

Not included:

1. Task leasing
2. Requeue and retry logic
3. Startup recovery into active in-memory queues
4. Push notifications or subscriptions

## Storage Model

Keep the `v0.0.2` layout:

```text
./task-data/<task_id>/task.md
./task-data/<task_id>/result.md
```

Add status metadata file:

```text
./task-data/<task_id>/status.json
```

Recommended `status.json` shape:

```json
{
  "task_id": "uuid-string",
  "role": "coder",
  "status": "queued",
  "created_at": "2026-04-25T19:30:00Z",
  "updated_at": "2026-04-25T19:30:00Z"
}
```

Minimum statuses for `v0.0.3`:

1. `queued`
2. `picked`
3. `solved`

## Task ID Generation

### Async create

`create_task_async` should stop requiring caller-supplied `task_id`.

New input:

```json
{
  "role": "coder",
  "task_md": "Review this change"
}
```

New response:

```json
{
  "ok": true,
  "status": "queued",
  "task_id": "server-generated-uuid"
}
```

Requirements:

1. Generate the ID on the server
2. Use stdlib only
3. Use a UUID-like random identifier safe for file paths

Implementation note:

1. Preferred: generate 16 random bytes via `crypto/rand` and encode as lowercase hex
2. Hyphens are optional
3. The resulting string must remain path-safe

## Sync create behavior

Keep `create_task_sync` behavior unchanged in `v0.0.3`.

Reason:

1. sync already returns the final result directly
2. the missing observability problem is specific to async
3. keeping sync stable makes this version smaller and easier to ship

So:

1. `create_task_sync` continues to accept caller-supplied `task_id`
2. `create_task_async` switches to server-generated `task_id`

This asymmetry is acceptable in `v0.0.3`.

## MCP API Changes

### Updated tool

#### `create_task_async`

Old input:

```json
{
  "role": "coder",
  "task_id": "async-1",
  "task_md": "Review this change"
}
```

New input:

```json
{
  "role": "coder",
  "task_md": "Review this change"
}
```

New response:

```json
{
  "ok": true,
  "status": "queued",
  "task_id": "generated-id"
}
```

### New tool: `get_task_status`

Input:

```json
{
  "task_id": "generated-id"
}
```

Response:

```json
{
  "task_id": "generated-id",
  "status": "picked"
}
```

If not found:

1. return application error `task "X" not found`

### New tool: `get_task_result`

Input:

```json
{
  "task_id": "generated-id"
}
```

If result exists:

```json
{
  "task_id": "generated-id",
  "status": "solved",
  "result_md": "Done"
}
```

If task exists but is not solved yet:

```json
{
  "task_id": "generated-id",
  "status": "queued",
  "result_ready": false
}
```

If not found:

1. return application error `task "X" not found`

## Broker Behavior Changes

### `CreateTaskAsync`

New method shape:

```go
func (b *Broker) CreateTaskAsync(role, taskMD string) (string, error)
```

Behavior:

1. Validate `role` and `task_md`
2. Generate task ID
3. Persist `task.md`
4. Persist `status.json` as `queued`
5. Insert into in-memory state
6. Return generated task ID

### `ListenRoleAsync`

When a worker picks a task:

1. remove the task from `asyncQueue`
2. keep it in unresolved task registry
3. update `status.json` to `picked`

If status persistence fails:

1. return an error
2. do not hand the task out

### `SolveTask`

When a worker solves a task:

1. persist `result.md`
2. persist `status.json` as `solved`
3. only then remove the task from in-memory unresolved registry

Important:

1. solved tasks must still remain queryable by `get_task_status` and `get_task_result`
2. therefore result lookup must read from disk, not only from in-memory `tasks`

## Query Semantics

### Source of truth

For `v0.0.3`, query tools should prefer disk as the durable source of truth.

Reason:

1. solved tasks are removed from live in-memory state
2. results must still be queryable after solve

Recommended behavior:

1. `get_task_status` reads `status.json`
2. `get_task_result` reads `status.json`
3. if status is `solved`, read `result.md`

### Result availability

`get_task_result` should not block.

It should:

1. return result immediately if available
2. return `result_ready: false` if not solved yet

Blocking wait semantics can be added later as a separate tool if needed.

## Filesystem Rules

For each async task:

1. create `task-data/<task_id>/task.md` on create
2. create `task-data/<task_id>/status.json` on create
3. update `status.json` to `picked` when handed to worker
4. create `result.md` and update `status.json` to `solved` on solve

## Validation

### `create_task_async`

Required:

1. `role` non-empty
2. `task_md` non-empty

Removed requirement:

1. caller no longer provides `task_id`

### `get_task_status`

Required:

1. `task_id` non-empty
2. `task_id` path-safe

### `get_task_result`

Required:

1. `task_id` non-empty
2. `task_id` path-safe

## MCP Tools List

`tools/list` should expose:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`
6. `get_task_status`
7. `get_task_result`

## Example Flow

### Async request/reply

1. orchestrator calls `create_task_async(role, task_md)`
2. broker returns `task_id`
3. human or worker later picks the task via `listen_role_async`
4. worker solves it via `solve_task(task_id, result_md)`
5. orchestrator polls `get_task_status(task_id)`
6. orchestrator reads final output via `get_task_result(task_id)`

This is the key product workflow enabled by `v0.0.3`.

## Testing

Add coverage for:

1. `create_task_async` returns generated task ID
2. generated task directory is created on disk
3. `status.json` is written as `queued`
4. `listen_role_async` changes status to `picked`
5. `solve_task` writes `result.md` and updates status to `solved`
6. `get_task_status` returns correct status before and after solve
7. `get_task_result` returns `result_ready: false` before solve
8. `get_task_result` returns final markdown after solve
9. `go build ./...` succeeds
10. `go vet ./...` succeeds

## Acceptance Criteria

1. async create no longer requires caller-supplied `task_id`
2. async create returns server-generated `task_id`
3. `get_task_status(task_id)` works through MCP
4. `get_task_result(task_id)` works through MCP
5. solved async tasks remain queryable after they leave live in-memory state
6. `task.md`, `result.md`, and `status.json` are stored under `./task-data/<task_id>/`
7. existing sync flow still works unchanged
8. `go build ./...` succeeds
9. `go vet ./...` succeeds

## Future Work

After `v0.0.3`, likely next additions are:

1. `wait_task_result(task_id)` blocking tool
2. startup recovery from `task-data`
3. claim and lease semantics
4. task listing by role or status
