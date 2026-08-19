# TASK-002: Durable global worker assignment

- **Status:** Completed
- **Priority:** P1
- **Area:** Broker tenancy, recovery, MCP clients
- **Created:** 2026-08-19

## Problem

A global task remains owned by its creator project, while the worker uses a
different `X-Project-Id`. TASK-001 authorized cross-project progress and solve
with a delivery-time `work_token`, but a client restart or context loss could
discard that token. The worker could then neither rediscover the picked task nor
reread its body through `get_task`, even though the broker still had the data.

## Implemented semantics

- Picking a global task atomically stores the worker's project ID on the task.
- `list_tasks` returns owned tasks plus global tasks assigned to the caller; an
  active assigned task sorts ahead of the normal 20-task window.
- `get_task`, `progress_task`, and `solve_task` resolve an assigned global task
  back to its owner project when called by the assigned worker project.
- `get_task` also accepts `work_token` as a bearer fallback.
- Requeue clears the assignment so the old worker project loses assignment-based
  access and a new worker can claim the task.
- Existing `work_token` behavior remains compatible. Requeue does not revoke an
  already issued token; deletion still revokes all task tokens.
- Owner-only broker/admin methods retain their original project-scoped behavior.

## Acceptance criteria

- A task created in project A and picked from a global queue by project B records
  B as its worker project.
- After broker/client restart, B sees the picked task in `list_tasks`, rereads it
  through `get_task`, and progresses/solves it without the original token.
- Project C cannot read or mutate the task without a valid token.
- More than 20 recent B-owned tasks cannot hide B's active assigned task.
- Admin and response-failure requeue clear the assignment.
- A subsequent project C pickup replaces the assignment with C.
- Local task isolation is unchanged.
- Existing databases add the nullable assignment column automatically.

