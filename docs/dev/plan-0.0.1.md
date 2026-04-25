# plan-0.0.1

## Goal

Add explicit `sync` and `async` delivery modes to the broker.

The motivation is product and policy safety:

- `sync` is useful for live agent-to-agent handoff where the caller can block for a long time.
- `async` is safer for human-mediated workflows, including subscription harness flows where a person manually checks an inbox and manually posts results.

This version keeps both modes instead of replacing the current blocking behavior.

## Core Decision

Yes: there are two independent axes and two pairs of tools.

1. Sending a task:
   - `sync`: send and wait for completed result
   - `async`: enqueue and return immediate acknowledgement
2. Receiving a task:
   - `sync`: wait until a task arrives
   - `async`: poll once and return a task immediately or say there is no task

For MCP clarity, expose these as separate tools instead of one tool with a `mode` flag.

Recommended tool surface:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`

`solve_task` stays shared between both modes so task completion remains uniform.

## Behavior Matrix

### 1. Send Task

#### `create_task_sync`

Input:

```json
{
  "role": "coder",
  "task_id": "task-123",
  "task_md": "Implement the plan"
}
```

Behavior:

1. Requires an active sync listener for the role.
2. Delivers the task immediately to that listener.
3. Blocks until `solve_task(task_id, result_md)` is called or request context is cancelled.
4. Returns the completed result.

Response:

```json
{
  "result_md": "Done"
}
```

#### `create_task_async`

Input:

```json
{
  "role": "coder",
  "task_id": "task-124",
  "task_md": "Implement the plan"
}
```

Behavior:

1. Does not require an active listener.
2. Stores the task in the async inbox for the role.
3. Returns immediately.
4. The caller is expected to tell the user that the task was queued for the target role.
5. In v0.0.1 this is a fire-and-forget submission: the original async sender does not wait for or receive the final result through the original request.

Response:

```json
{
  "ok": true,
  "status": "queued",
  "message": "task queued for role coder"
}
```

### 2. Receive Task

#### `listen_role_sync`

Input:

```json
{
  "role": "coder"
}
```

Behavior:

1. Registers one active blocking listener for the role.
2. Waits until a sync task is delivered.
3. Returns the task once it arrives.
4. If the request is cancelled, the listener is removed.

Response:

```json
{
  "task_id": "task-123",
  "task_md": "Implement the plan"
}
```

#### `listen_role_async`

Input:

```json
{
  "role": "coder"
}
```

Behavior:

1. Checks the async inbox for the role once.
2. If a task exists, returns it immediately.
3. If no task exists, returns a structured `no_task` response immediately.
4. Does not block.

Response when task exists:

```json
{
  "found": true,
  "task_id": "task-124",
  "task_md": "Implement the plan"
}
```

Response when no task exists:

```json
{
  "found": false,
  "status": "no_task"
}
```

## Async Delivery Semantics

Async mode needs explicit inbox state. Without it, polling is not well defined.

Minimum v0.0.1 rule set:

1. Async tasks are stored in memory by role.
2. `listen_role_async` removes one task from the role inbox when returning it.
3. Removal from the queue does not remove the task from the global unresolved registry.
4. After async delivery, the task remains tracked in `tasks` until `solve_task` is called.
5. If a worker takes a task and disappears, the task is lost from the inbox in v0.0.1 but still remains unresolved in `tasks`.
6. This unresolved leftover is accepted in v0.0.1 as a known limitation of the minimal async design.

This is acceptable for the first async version because the goal is manual inbox support, not durable queue semantics.

### Async task lifecycle

The async lifecycle in v0.0.1 is:

1. `queued`: task exists in `tasks` and is present in `asyncQueue[role]`
2. `picked`: task exists in `tasks` but has already been removed from `asyncQueue[role]`
3. `solved`: `solve_task` removes the task from `tasks`

Important consequence:

1. `listen_role_async` transitions `queued -> picked`
2. `solve_task` transitions `picked -> solved`
3. There is no recovery path from `picked` back to `queued` in v0.0.1

Explicit non-goals for v0.0.1:

1. No leases
2. No retries
3. No heartbeats
4. No persistence
5. No multiple-consumer coordination beyond simple pop-from-queue

## Broker Data Model Changes

Current state:

```go
listeners map[string]chan *Task
tasks     map[string]*Task
```

Add async inbox state:

```go
listeners  map[string]chan *Task
tasks      map[string]*Task
asyncQueue map[string][]*Task
```

Rules:

1. `tasks` remains the global registry of unresolved tasks for both sync and async modes.
2. `listeners` is used only by sync receive mode.
3. `asyncQueue[role]` stores pending async tasks for that role.
4. A task returned by `listen_role_async` is removed from `asyncQueue[role]` but remains in `tasks` until solved.

## API Changes

### MCP `tools/list`

Replace current business tools list:

1. `create_task`
2. `listen_role`
3. `solve_task`

With:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`

### MCP `tools/call`

Dispatcher must route the new names to dedicated handlers.

Recommended mapping:

1. `create_task_sync` -> `Broker.CreateTaskSync`
2. `create_task_async` -> `Broker.CreateTaskAsync`
3. `listen_role_sync` -> `Broker.ListenRoleSync`
4. `listen_role_async` -> `Broker.ListenRoleAsync`
5. `solve_task` -> `Broker.SolveTask`

## Broker Methods

Recommended method set:

```go
func (b *Broker) ListenRoleSync(ctx context.Context, role string) (*Task, error)
func (b *Broker) ListenRoleAsync(role string) (*Task, bool, error)
func (b *Broker) CreateTaskSync(ctx context.Context, role, taskID, taskMD string) (string, error)
func (b *Broker) CreateTaskAsync(role, taskID, taskMD string) error
func (b *Broker) SolveTask(taskID, resultMD string) error
```

## Validation Rules

Apply to all four task-entry tools:

1. `role` must be non-empty
2. `task_id` must be non-empty where applicable
3. `task_md` must be non-empty where applicable
4. Duplicate `task_id` must fail

Extra sync rules:

1. `create_task_sync` fails if there is no active sync listener for the role
2. `listen_role_sync` fails if a sync listener for the role already exists

Extra async rules:

1. `create_task_async` succeeds even if no worker is currently polling
2. `listen_role_async` never blocks

## User-Facing Semantics

### Human-safe async workflow

1. Orchestrator says: send task to role `coder`
2. Broker queues the markdown task
3. Caller tells the user: `Task queued for coder`
4. User manually switches to another model client
5. User asks: `check inbox for coder`
6. That agent reads the task via `listen_role_async`
7. Agent completes the work and calls `solve_task`
8. Human gets the completion message in that client
9. The original async sender does not automatically receive that result in v0.0.1

This is the primary non-automated harness flow supported by v0.0.1.

### Live sync workflow

1. Worker opens `listen_role_sync`
2. Orchestrator calls `create_task_sync`
3. Broker hands the task directly to the waiting worker
4. Worker calls `solve_task`
5. Orchestrator receives the finished result on the original blocking request

## Limitations

The async mode in v0.0.1 is intentionally lightweight.

Known limitations:

1. Async tasks are in-memory only
2. Async task pop is destructive
3. If an async worker disappears after receiving a task, recovery is not implemented
4. Picked-but-unsolved async tasks remain in `tasks` until process restart or future recovery features are added
5. There is no task status query API yet
6. There is no acknowledgement channel back to the original async sender beyond the initial `queued` response
7. Async mode is therefore not a full request/reply workflow in v0.0.1

## Acceptance Criteria

1. `create_task_sync` preserves current blocking behavior
2. `listen_role_sync` preserves current blocking behavior
3. `create_task_async` returns immediately and stores the task
4. `listen_role_async` returns immediately with either one task or `no_task`
5. `solve_task` works for tasks created by both modes
6. `tools/list` exposes all five tools
7. All new tool inputs validate required fields
8. The plan explicitly defines async lifecycle as `queued -> picked -> solved`
9. The plan explicitly states that async mode is fire-and-forget for the original sender in v0.0.1
10. `go build ./...` succeeds
11. `go vet ./...` succeeds

## Next Step After v0.0.1

The first extension after this version should be async recovery semantics:

1. `claim_task`
2. `lease_until`
3. `requeue` on timeout
4. `get_task_status`

That would turn async mode from a simple inbox into a safer queue.
