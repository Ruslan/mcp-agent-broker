# Plan 0.0.9: Task Progress — `progress_task` tool

## Goal

Allow a worker to send intermediate progress updates for a task without completing it. The coordinator and external clients can observe progress in real time.

Example: a worker finishes migrations and reports that before moving to the next stage. The final result is still delivered via `solve_task`.

## Changes in `broker.go`

### 1. New field in `Task`

```go
type Task struct {
    ID        string
    ProjectID string
    Role      string
    Title     string
    MD        string
    done      chan string   // final result (already exists)
    progress  chan string   // new: intermediate updates
}
```

The `progress` channel is buffered (size 32). Workers do not block if nobody is reading. Messages beyond the buffer are dropped with a log warning.

### 2. Initialization in `CreateTask`

```go
task := &Task{
    // ...existing fields...
    done:     make(chan string, 1),
    progress: make(chan string, 32),
}
```

### 3. New method `ReportProgress`

```go
func (b *Broker) ReportProgress(projectID, taskID, message string) error {
    b.mu.Lock()
    defer b.mu.Unlock()

    projectTasks, ok := b.tasks[projectID]
    if !ok {
        return fmt.Errorf("task %q not found in project %q", taskID, projectID)
    }
    task, exists := projectTasks[taskID]
    if !exists {
        return fmt.Errorf("task %q not found in project %q", taskID, projectID)
    }

    select {
    case task.progress <- message:
    default:
        log.Printf("progress buffer full for task %s, dropping message", taskID)
    }
    return nil
}
```

The mutex is held for the entire method including the channel send. This prevents a race with `SolveTask` closing the channel — sending on a closed channel causes a panic.

### 4. Close channel in `SolveTask`

In the existing `SolveTask` method, close the progress channel inside the mutex lock before unlocking:

```go
delete(projectTasks, taskID)
if len(projectTasks) == 0 {
    delete(b.tasks, projectID)
}
close(task.progress) // inside the lock — atomic with delete
b.mu.Unlock()
select {
case task.done <- resultMD:
default:
}
```

This guarantees `ReportProgress` either sends before the delete, or fails to find the task after it — panic-free.

### 5. Drain progress in `AwaitTask`

After receiving from `task.done`, drain the closed `progress` channel:

```go
case res := <-task.done:
    select {
    case task.done <- res:
    default:
    }
    var progress []string
    for msg := range task.progress {
        progress = append(progress, msg)
    }
    return string(StatusSolved), res, progress, nil
```

On timeout, drain non-blocking so polling clients see accumulated updates:

```go
case <-timeoutCh:
    var progress []string
    for {
        select {
        case msg := <-task.progress:
            progress = append(progress, msg)
        default:
            goto doneTimeout
        }
    }
doneTimeout:
    // return status, "", progress, nil
```

`AwaitTask` signature changes to `(string, string, []string, error)`.

## Changes in `jsonrpc.go`

### New tool `progress_task`

Add to `tools/list`:

```json
{
  "name": "progress_task",
  "description": "Send an intermediate progress update for a task without completing it. Call multiple times during long-running work.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "task_id": {"type": "string"},
      "message": {"type": "string", "description": "Short human-readable status update (max 500 chars)"}
    },
    "required": ["task_id", "message"]
  }
}
```

Add handler in `handleToolCall`:

```go
case "progress_task":
    var p struct {
        TaskID  string `json:"task_id"`
        Message string `json:"message"`
    }
    if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" || p.Message == "" {
        return nil, fmt.Errorf("invalid arguments: task_id and message are required")
    }
    if len(p.Message) > 500 {
        return nil, fmt.Errorf("message too long (max 500 chars)")
    }
    if err := h.broker.ReportProgress(projectID, p.TaskID, p.Message); err != nil {
        return nil, err
    }
    return map[string]bool{"ok": true}, nil
```

### `await_task` response

Include collected progress in the response when non-empty:

```json
{
  "task_id": "abc",
  "status": "solved",
  "result_md": "final result",
  "progress": [
    "migrations done",
    "models written",
    "tests added"
  ]
}
```

## Persistence

Progress messages are **not written to disk** — in-memory only. On server restart, progress history is lost (only `result.md` persists). This is intentional: progress is ephemeral.

## Implementation steps

1. Add `progress chan string` field to `Task` in `broker.go`.
2. Initialize the channel in `CreateTask`.
3. Implement `ReportProgress` in `broker.go`.
4. Move `close(task.progress)` inside the mutex lock in `SolveTask`.
5. Update `AwaitTask` signature to `(string, string, []string, error)`, drain progress on done and timeout.
6. Add `progress_task` to `tools/list` in `jsonrpc.go`.
7. Add `progress_task` handler in `handleToolCall`.
8. Include `progress` field in `await_task` response when non-empty.
9. Update all `AwaitTask` call sites in tests.
10. Write tests:
    - worker calls `progress_task` multiple times then `solve_task` — all messages returned;
    - `progress_task` on nonexistent `task_id` returns error;
    - `progress_task` after `solve_task` returns error (task removed from memory);
    - full buffer — message dropped without error;
    - `await_task` with timeout returns accumulated progress so far.

## Affected files

| File | Change |
|------|--------|
| `agent-broker/broker.go` | `progress` field in `Task`, `ReportProgress`, mutex fix in `SolveTask`, updated `AwaitTask` |
| `agent-broker/jsonrpc.go` | `progress_task` in `tools/list` and handler, `await_task` response update |
| `agent-broker/broker_extra_test.go` | Tests for `ReportProgress` |
| `agent-broker/jsonrpc_extra_test.go` | Tests for `progress_task` via RPC |

## Relation to A2A

`progress_task` is a prerequisite for A2A `tasks/sendSubscribe` (see `docs/dev/a2a.md`). The `task.progress` channel becomes the source of SSE events with `state: "working"` during streaming. Without progress updates, streaming is meaningless.

For SSE fan-out (multiple readers), a broadcaster pattern will be needed instead of direct channel access — noted in `a2a.md` open questions.
