# plan-0.0.7

## Goal

Reduce the tool surface while keeping both autonomous and user-driven workflows ergonomic.

`v0.0.7` should remove the current sync-vs-async duplication from task creation and role listening.

## Core Idea

Creation is always non-blocking.

Waiting is a separate operation.

Polling and blocking delivery for workers are the same operation with a mode switch.

This changes the API shape from transport-style variants:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`

To one lifecycle-oriented API:

1. `create_task`
2. `await_task`
3. `listen_role`
4. `get_task`
5. `list_tasks`
6. `solve_task`

## Why

Current problems:

1. sync and async are encoded into tool names instead of usage pattern
2. there are too many overlapping tools for the same lifecycle
3. agents must learn unnecessary distinctions
4. the tool list is longer than needed for normal operation

Target behavior:

1. every created task gets a `task_id`
2. caller decides whether to wait now or later
3. worker can either block for work or poll for work through one tool
4. listing and detail retrieval are separate to control context size

## New Tool Set

### `create_task`

Creates a task and returns immediately.

Arguments:

```json
{
  "role": "coder",
  "title": "fix failing tests",
  "task_md": "..."
}
```

Response:

```json
{
  "task_id": "...",
  "status": "queued"
}
```

Notes:

1. server-generated `task_id` remains required
2. `title` and `task_md` remain required
3. project routing still comes from `X-Project-Id`

### `await_task`

Blocks until the task reaches a terminal state or timeout/cancel.

Arguments:

```json
{
  "task_id": "...",
  "timeout_ms": 30000
}
```

Response examples:

Solved:

```json
{
  "task_id": "...",
  "status": "solved",
  "result_md": "..."
}
```

Timeout:

```json
{
  "task_id": "...",
  "status": "queued"
}
```

Notes:

1. if already solved, return immediately
2. timeout should not be treated as protocol failure
3. this replaces `create_task_sync`

### `listen_role`

Single worker-facing tool for both blocking wait and non-blocking check.

Arguments:

```json
{
  "role": "coder",
  "mode": "wait",
  "timeout_ms": 30000
}
```

Modes:

1. `wait`: block until task available or timeout/cancel
2. `poll`: return immediately if no task exists

Response when task exists:

```json
{
  "task": {
    "task_id": "...",
    "title": "...",
    "task_md": "..."
  }
}
```

Response when no task exists in `poll` mode:

```json
{
  "task": null,
  "status": "empty"
}
```

Response when no task arrives before timeout in `wait` mode:

```json
{
  "task": null,
  "status": "timeout"
}
```

Notes:

1. this replaces both `listen_role_sync` and `listen_role_async`
2. `poll` covers the user scenario: "check whether this role has work"

### `list_tasks`

Returns lightweight task metadata only.

Arguments:

```json
{
  "role": "coder",
  "status": "queued"
}
```

Allowed filters:

1. `role`
2. `status`

Response shape:

```json
{
  "tasks": [
    {
      "task_id": "...",
      "role": "coder",
      "title": "fix failing tests",
      "status": "queued",
      "created_at": "...",
      "updated_at": "..."
    }
  ]
}
```

Notes:

1. no `task_md` by default
2. no `result_md` by default
3. this is an overview API only

### `get_task`

Returns detailed content for one task, but defaults to the most useful single payload to save context.

Arguments:

```json
{
  "task_id": "..."
}
```

Default behavior:

1. if task is completed, return result only
2. if task is not completed, return task prompt only

Reason:

1. completed task: result is usually what the caller wants
2. incomplete task: prompt is usually what the caller needs to inspect
3. avoids returning both large texts when not needed

Suggested default response examples:

Completed:

```json
{
  "task_id": "...",
  "status": "solved",
  "result_md": "..."
}
```

Not completed:

```json
{
  "task_id": "...",
  "status": "queued",
  "task_md": "..."
}
```

Optional expansion:

```json
{
  "task_id": "...",
  "include": "all"
}
```

Or equivalent booleans:

```json
{
  "task_id": "...",
  "include_task_md": true,
  "include_result_md": true
}
```

Notes:

1. `get_task` is the detail API
2. `list_tasks` stays metadata-only

### `solve_task`

Remains as the worker completion API.

Arguments:

```json
{
  "task_id": "...",
  "result_md": "..."
}
```

No behavior change intended beyond compatibility with the new lifecycle.

## Workflow Examples

### Autonomous Main Agent

1. call `create_task`
2. immediately call `await_task`
3. review result

### User-Driven Async Main Agent

1. call `create_task`
2. remember `task_id`
3. later call `get_task` or `list_tasks`

### Worker Waiting For Work

1. call `listen_role` with `mode="wait"`
2. do the work
3. call `solve_task`

### Quick Inbox Check

1. call `listen_role` with `mode="poll"`
2. if empty, report no tasks

## Backward Compatibility

Decision needed:

1. either ship as a clean break in `v0.0.7`
2. or keep old tools as aliases for one release and mark them deprecated

Recommended approach:

1. implement new tools first
2. keep old names as thin wrappers temporarily if migration cost matters
3. document the new canonical API in `README.md`

## Implementation Notes

Likely code changes:

1. replace separate tool schemas in `jsonrpc.go`
2. refactor broker methods around one create path and one listen path
3. keep task persistence model unchanged where possible
4. add a blocking wait helper by `task_id`
5. add task listing over persisted or in-memory metadata

## Tests

Add coverage for:

1. `create_task` returns queued task with generated ID
2. `await_task` returns immediately for already solved tasks
3. `await_task` times out cleanly without protocol error
4. `listen_role(mode=poll)` returns empty when no task exists
5. `listen_role(mode=wait)` returns task when available
6. `list_tasks` filters only by `role` and `status`
7. `get_task` default payload returns only `result_md` for solved tasks
8. `get_task` default payload returns only `task_md` for unfinished tasks
9. `get_task(include=all)` returns both texts

## Recommendation

This is a solid next version.

The key design choices are good:

1. unify sync/async creation into one `create_task`
2. unify blocking and polling worker fetch into one `listen_role`
3. use `list_tasks` for discovery and `get_task` for details
4. make `get_task` context-efficient by default

This keeps the API smaller without losing important workflows.
