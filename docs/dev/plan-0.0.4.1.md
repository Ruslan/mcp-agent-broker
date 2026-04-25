# plan-0.0.4.1

## Goal

Make async-created tasks visible to sync listeners.

Current behavior splits task delivery by mode:

1. tasks created with `create_task_sync` are consumed by `listen_role_sync`
2. tasks created with `create_task_async` are consumed by `listen_role_async`

This makes the worker pool fragmented.

`v0.0.4.1` changes this so that a sync worker can also receive queued async tasks.

## Why

For a practical worker role like `coder`, we want one live worker to be able to process work regardless of how the orchestrator created it.

Desired behavior:

1. async task may sit in queue until some worker asks for work
2. if a sync listener is active first, it may receive that async task
3. if an async poller asks first, it may receive that async task

This makes the broker more useful as a shared work channel.

## Core Decision

Async-created tasks should be available to both:

1. `listen_role_async`
2. `listen_role_sync`

Sync-created tasks remain sync-only.

Rationale:

1. sync-created tasks are request/response handoffs tied to a waiting sender
2. async-created tasks are queued work and should be consumable by any worker path

## Delivery Rules

### Sync-created task

1. created by `create_task_sync`
2. must still require an active sync listener
3. delivered directly to that sync listener
4. not visible through `listen_role_async`

### Async-created task

1. created by `create_task_async`
2. stored in async queue
3. may be claimed by `listen_role_async`
4. may also be claimed by `listen_role_sync`
5. once claimed by either path, it must not be handed out again

## Broker Behavior Changes

### `ListenRoleSync`

Current behavior:

1. registers a sync listener channel
2. waits for direct sync delivery only

New behavior:

1. register the sync listener channel as before
2. before blocking, check whether `asyncQueue[role]` already contains queued async work
3. if queued async work exists, remove one task from `asyncQueue[role]`, update its status to `picked`, and return it immediately
4. otherwise continue waiting for direct sync delivery

This means a live sync worker can drain queued async work.

### `CreateTaskAsync`

Behavior stays the same:

1. create queued async task
2. return immediately

No direct handoff to sync listeners is required in `v0.0.4.1`.
It is enough that queued async tasks are visible to future `listen_role_sync` calls.

### `ListenRoleAsync`

Behavior stays the same:

1. pop one async task from queue
2. mark as picked
3. return it

## State Rules

For async-created tasks:

1. source of queued work remains `asyncQueue[role]`
2. whichever listener path consumes the task first removes it from queue
3. task remains in unresolved registry until `solve_task`
4. status transitions still follow:
   - `queued`
   - `picked`
   - `solved`

## Query Semantics

No changes to:

1. `get_task_status`
2. `get_task_result`
3. `solve_task`

Once a sync listener picks an async task, status should still become `picked` and later `solved` exactly like the async path.

## API Surface

No new tools.

No schema changes.

Only behavioral change:

1. `listen_role_sync` may now return an async-created queued task

## Testing

Add coverage for:

1. create async task, then receive it through `listen_role_sync`
2. received task is removed from async queue
3. task status becomes `picked`
4. solving that task still works
5. async poller no longer sees the same task after sync listener already claimed it
6. existing sync-only direct handoff still works

## Acceptance Criteria

1. async-created tasks are visible to `listen_role_sync`
2. sync-created tasks still work as before
3. each async task is delivered at most once
4. task status remains correct when async work is picked by sync listener
5. `go build ./...` succeeds
6. `go vet ./...` succeeds
7. `go test -count=1 ./...` succeeds
