# plan-0.0.5

## Goal

Unify task identity and add task titles for tracking.

This version changes the task creation model so that:

1. the server always generates the `task_id`
2. both sync and async task creation require a short `title`
3. task metadata becomes easier to track, list, inspect, and analyze

## Why

Current state:

1. `create_task_async` generates `task_id` on the server
2. `create_task_sync` still requires caller-supplied `task_id`
3. tasks have only markdown body, so short tracking labels are missing

Problems:

1. inconsistent API between sync and async
2. caller has to invent IDs in sync mode
3. analytics and tracking are harder without a short task label

`v0.0.5` fixes that by making identity consistent and adding explicit task titles.

## Core Decision

For both sync and async task creation:

1. `task_id` is always server-generated
2. `title` is required
3. `task_md` remains the full task body

Example:

1. `title`: `implement 0.0.9 version`
2. `task_md`: full detailed description

## API Changes

### `create_task_sync`

Old input:

```json
{
  "role": "coder",
  "task_id": "sync-task-1",
  "task_md": "Implement the feature"
}
```

New input:

```json
{
  "role": "coder",
  "title": "implement 0.0.9 version",
  "task_md": "Implement the feature"
}
```

New response:

```json
{
  "task_id": "generated-uuid",
  "result_md": "Done"
}
```

Notes:

1. still blocks until solved
2. now also returns the generated `task_id`

### `create_task_async`

Old input:

```json
{
  "role": "coder",
  "task_md": "Implement the feature"
}
```

New input:

```json
{
  "role": "coder",
  "title": "implement 0.0.9 version",
  "task_md": "Implement the feature"
}
```

New response:

```json
{
  "ok": true,
  "status": "queued",
  "task_id": "generated-uuid"
}
```

### `listen_role_sync`

Old response:

```json
{
  "task_id": "generated-uuid",
  "task_md": "Implement the feature"
}
```

New response:

```json
{
  "task_id": "generated-uuid",
  "title": "implement 0.0.9 version",
  "task_md": "Implement the feature"
}
```

### `listen_role_async`

Old response:

```json
{
  "found": true,
  "task_id": "generated-uuid",
  "task_md": "Implement the feature"
}
```

New response:

```json
{
  "found": true,
  "task_id": "generated-uuid",
  "title": "implement 0.0.9 version",
  "task_md": "Implement the feature"
}
```

### `get_task_status`

Response should now also include `title`:

```json
{
  "task_id": "generated-uuid",
  "title": "implement 0.0.9 version",
  "status": "queued"
}
```

### `get_task_result`

Response should now also include `title`:

```json
{
  "task_id": "generated-uuid",
  "title": "implement 0.0.9 version",
  "status": "solved",
  "result_md": "Done"
}
```

## Broker Model Changes

Extend `Task`:

```go
type Task struct {
    ID    string
    Role  string
    Title string
    MD    string
    done  chan string
}
```

Extend `StatusMetadata`:

```go
type StatusMetadata struct {
    TaskID    string     `json:"task_id"`
    Role      string     `json:"role"`
    Title     string     `json:"title"`
    Status    TaskStatus `json:"status"`
    CreatedAt time.Time  `json:"created_at"`
    UpdatedAt time.Time  `json:"updated_at"`
}
```

## Storage Model

Keep existing layout:

```text
./task-data/<task_id>/task.md
./task-data/<task_id>/result.md
./task-data/<task_id>/status.json
```

No new files are required.

`title` should be stored in `status.json`.

## Behavior Changes

### `create_task_sync`

New method shape:

```go
func (b *Broker) CreateTaskSync(ctx context.Context, role, title, taskMD string) (taskID string, resultMD string, err error)
```

Behavior:

1. validate `role`, `title`, `task_md`
2. generate a server-side task ID
3. persist `task.md`
4. persist `status.json` with `title` and `queued`
5. deliver task to sync listener
6. wait for result
7. return both `task_id` and `result_md`

### `create_task_async`

New method shape:

```go
func (b *Broker) CreateTaskAsync(role, title, taskMD string) (string, error)
```

Behavior:

1. validate `role`, `title`, `task_md`
2. generate a server-side task ID
3. persist `task.md`
4. persist `status.json` with `title` and `queued`
5. enqueue task
6. return generated `task_id`

### `listen_role_sync` and `listen_role_async`

Both should return `title` in addition to `task_id` and `task_md`.

### `get_task_status` and `get_task_result`

Both should include `title` in responses.

## Validation

### `title`

Rules:

1. required
2. non-empty after trimming whitespace
3. should be short

Recommended limit:

1. maximum 200 characters

If invalid, return application error.

### `task_id`

Callers no longer provide `task_id` for either sync or async create.

Server-generated IDs must:

1. remain path-safe
2. be unique in practice
3. continue to use stdlib-only generation

## MCP Tools List

Tool names do not change:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`
6. `get_task_status`
7. `get_task_result`

Only input/output schemas change.

## Tool Description Updates

Update `tools/list` schemas and descriptions so they reflect:

1. `title` is required for both create tools
2. `task_id` is no longer accepted on task creation
3. task receivers will see `title` as part of the task payload

## Compatibility Impact

This is a breaking API change for sync task creation and for any client that assumes create requests provide `task_id`.

That is acceptable for this pre-1.0 broker version.

## Testing

Add or update coverage for:

1. `create_task_sync` generates and returns `task_id`
2. `create_task_async` generates and returns `task_id`
3. both create methods reject missing `title`
4. both create methods reject overlong `title`
5. `listen_role_sync` returns `title`
6. `listen_role_async` returns `title`
7. `get_task_status` returns `title`
8. `get_task_result` returns `title`
9. `status.json` stores `title`
10. `go build ./...` succeeds
11. `go vet ./...` succeeds
12. `go test -count=1 ./...` succeeds

## Acceptance Criteria

1. server generates `task_id` for sync create
2. server generates `task_id` for async create
3. `title` is required for both create tools
4. `listen_role_sync` returns `title`
5. `listen_role_async` returns `title`
6. `get_task_status` returns `title`
7. `get_task_result` returns `title`
8. `status.json` contains `title`
9. existing solve flow still works
10. `go build ./...` succeeds
11. `go vet ./...` succeeds
12. `go test -count=1 ./...` succeeds

## Future Work

After `v0.0.5`, likely next steps are:

1. multi-project namespace via `X-Project-Id`
2. task listing by role/status/title
3. metrics and analytics by title pattern
