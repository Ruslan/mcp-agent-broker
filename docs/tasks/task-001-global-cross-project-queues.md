# TASK-001: Global cross-project queues

- **Status:** Completed
- **Priority:** P1
- **Area:** Broker routing, task capabilities, MCP clients
- **Created:** 2026-08-19

## Use case

Several projects use one broker but retain distinct `X-Project-Id` values. Two
consumer projects may depend on a library owned by a third project. The consumers
must be able to send work to the library owner's worker without joining the same
project namespace or exposing their unrelated local queues.

```text
project-a ─┐
           ├─→ g:shared-lib:maintainer ─→ project-lib
project-b ─┘
```

This feature is about clients on different machines using **one broker**. Sharing
queues across independent broker instances is out of scope.

## Required semantics

### Queue addressing

- Existing roles remain local and are routed by `(X-Project-Id, role)`.
- A role matching `g:<key>:<queue>` is global and is routed by the complete global
  role, independently of the caller's `X-Project-Id`.
- Different keys or queue names are isolated.
- In this version, `<key>` is a namespace identifier, not a secret or an ACL. The
  broker-level authentication boundary remains `API_KEY` or the trusted outer
  proxy. A future queue ACL must use a separate credential that is not logged or
  displayed as a role name.

### Ownership and visibility

- A task is persisted under the creator's `X-Project-Id`; that project remains the
  owner.
- `list_tasks`, `get_task`, `await_task`, admin filtering, and SSE continue to show
  the task under its owner project.
- Listening to a global queue does not grant access to other tasks in the owner
  project.

### Worker capability

- A global task delivered by `listen_role` or a role `poll_url` includes an opaque
  `work_token` scoped to that task and owner project.
- `progress_task` and `solve_task` accept an optional `work_token`.
- Existing local calls without a token retain their current behavior.
- When the caller's `X-Project-Id` is not the task owner, a valid work token is
  required. The broker resolves the owner from the token; the worker must not
  override `X-Project-Id` or receive general access to the owner project.
- Invalid, mismatched, or expired tokens fail without revealing another project's
  task details.
- Tokens must never be written to logs or admin task metadata.

## Implementation outline

1. Introduce an internal comparable queue address that distinguishes local
   `(project, role)` routing from global `role` routing. Do not overload persisted
   `project_id`; it remains ownership.
2. Route both blocking listeners and async queues through that address. On dequeue,
   database operations and events must use `task.ProjectID`, not the listening
   worker's project.
3. Restore queued global tasks into the global routing address after restart.
4. Persist or otherwise make work capabilities restart-safe. Bind each token to
   owner project and task ID, use cryptographically random values, and define a
   bounded lifetime suitable for long agent tasks.
5. Carry `work_token` through direct `listen_role`, `/poll/{role-token}`, response
   write-failure requeue, and all worker-facing integrations.
6. Update the Pi `broker-queue` extension and embedded async-poller skill so global
   workers preserve and submit the token for progress and solve calls.
7. Document the global role format and the fact that `<key>` is not an access
   secret.

## Compatibility

- No behavior or response field is removed.
- Local roles and local task operations remain valid without `work_token`.
- Global routing is opt-in through the reserved `g:` prefix.
- Existing databases migrate automatically.

## Acceptance criteria

- Project A creates a task for `g:shared-lib:maintainer`; a listener using project
  B receives it.
- The task remains visible to A and is not listed as a B task.
- B can report progress and solve it with the returned `work_token`.
- B cannot mutate it without the token or with a token for another task.
- A receives the progress and final result through existing APIs.
- Local `coder` queues for A and B remain isolated.
- `g:key-one:queue` and `g:key-two:queue` remain isolated.
- Blocking wait delivery, async role polling, response-write requeue, and restart
  recovery preserve global routing and task ownership.
- Token material is absent from logs and admin API responses.
- Go unit, race, and relevant integration tests pass; the UI builds.
