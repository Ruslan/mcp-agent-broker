# Codebase Overview

Updated 2026-08-19. Update this document whenever the runtime architecture,
storage schema, or public HTTP surface changes significantly.

## Directory structure

```text
.
├── agent-broker/               # Go server, embedded assets, scripts, and tests
│   ├── main.go                 # Entrypoint, routes, environment, auth middleware
│   ├── broker.go               # Task lifecycle, queues, listeners, prompts, SSE fanout
│   ├── store.go                # Store interface and SQLite implementation
│   ├── jsonrpc.go              # MCP-compatible JSON-RPC handler and tools
│   ├── pollhandler.go          # GET /poll/{token} capability endpoint
│   ├── skillhttp.go            # GET /skill/install
│   ├── skillfiles_embed.go     # Embedded poller scripts and installer body
│   ├── skillfiles/             # Canonical async-poller skill sources
│   ├── admin.go                # Admin REST API and SSE endpoint
│   ├── admin_embed.go          # Embedded compiled Svelte UI
│   └── *_test.go
├── ui/                         # Svelte 5 admin dashboard source
├── prompts/                    # MCP prompt templates loaded from disk
├── extensions/pi/broker-queue/ # Experimental Pi integration
├── examples/ralph-simple/      # Sync and async role examples
├── deploy/                     # Dockerfile and systemd/Kamal documentation
├── config/deploy.yml           # Kamal 2 deployment configuration
├── docs/dev/                   # Historical plans and forward-looking designs
├── docs/bugs/                  # One Markdown card per open defect
├── docs/tasks/                 # One Markdown card per planned feature
├── data/                       # Runtime SQLite location for local/systemd use
└── Makefile
```

## Runtime architecture

- **Process:** one Go binary serves MCP/JSON-RPC, the admin REST API, admin SSE,
  capability polling, the skill installer, health checks, and the embedded SPA.
- **State:** task metadata, results, progress, poll capabilities, and global-task
  worker capabilities are persisted in SQLite. Active tasks, queue-addressed
  work lists, blocking listeners, and SSE subscribers also have in-memory
  representations.
- **Routing:** local queue addresses are `(project_id, role)`. Roles matching
  `g:<key>:<queue>` use a broker-global address keyed by the complete role; task
  ownership remains the creator's `project_id`.
- **Recovery:** queued and picked tasks are loaded from SQLite at startup. Queued
  tasks return to their role queues; picked tasks stay addressable for a worker to
  solve or for an administrator to requeue.
- **Frontend:** Svelte 5 compiles to `ui/dist`, which `make ui-build` copies to the
  gitignored `agent-broker/dist` directory before Go embeds it.
- **Deployment:** systemd runs the binary directly. Kamal builds a three-stage
  Node → Go → Alpine image and mounts SQLite on a persistent named volume.

## Task lifecycle

```text
queued --listen_role/poll--> picked --solve_task--> solved
   ^                              |
   └--------- admin requeue ------┘
```

`create_task` always returns immediately. A dispatcher can use blocking
`await_task`, periodically call `get_task`, or arm the task's capability
`poll_url`. Workers use `listen_role` in blocking `wait` mode or asynchronous
`poll` mode. `progress_task` persists intermediate worker messages.

The latest forward-looking design is
[`plan-0.0.14.md`](plan-0.0.14.md): mid-task dispatcher-to-worker steering. It is
proposed and not implemented.

## Database schema

SQLite is opened through `modernc.org/sqlite`, with WAL mode and one open
connection to serialize writes.

### `tasks`

| Column | Type | Notes |
|--------|------|-------|
| `project_id` | TEXT | Composite primary key; tenant boundary |
| `task_id` | TEXT | Composite primary key |
| `role` | TEXT | Worker queue name |
| `title` | TEXT | Maximum 200 characters at broker validation |
| `status` | TEXT | `queued`, `picked`, or `solved` |
| `task_md` | TEXT | Original task body |
| `result_md` | TEXT | Nullable until solved |
| `created_at` | TEXT | RFC3339 UTC |
| `updated_at` | TEXT | RFC3339 UTC |
| `result_view_count` | INTEGER | Number of result reads through client paths |

Indexes: `idx_tasks_project_role`, `idx_tasks_project_status`.

### `task_progress`

| Column | Type | Notes |
|--------|------|-------|
| `project_id` | TEXT | Task tenant |
| `task_id` | TEXT | Task ID |
| `sequence` | INTEGER | Autoincrement primary key and ordering cursor |
| `message` | TEXT | Progress text, max 500 characters at RPC validation |
| `created_at` | TEXT | RFC3339 UTC |

The table declares a composite foreign key to `tasks` with `ON DELETE CASCADE`.
Foreign-key enforcement is not currently enabled; see
[`BUG-003`](../bugs/bug-003-sqlite-foreign-keys-disabled.md).

### `poll_tokens`

| Column | Type | Notes |
|--------|------|-------|
| `token` | TEXT | Primary key; 32 random bytes encoded as hex |
| `project_id` | TEXT | Scope tenant |
| `scope_kind` | TEXT | `role` or `task` |
| `scope_value` | TEXT | Role name or task ID |
| `created_at` | TEXT | Used for the hard lifetime cap |
| `expires_at` | TEXT | Sliding expiry |

Poll tokens have an approximately 30-minute sliding TTL and a hard 24-hour
lifetime. Active tokens are reused per scope while enough lifetime remains.

### `work_tokens`

| Column | Type | Notes |
|--------|------|-------|
| `token` | TEXT | Primary key; 32 random bytes encoded as hex |
| `project_id` | TEXT | Owning task project |
| `task_id` | TEXT | Single task authorized for progress/solve |
| `created_at` | TEXT | RFC3339 UTC creation time |
| `expires_at` | TEXT | Fixed seven-day expiry |

Work tokens are minted only for global tasks, persisted across restarts, and
returned only to the worker that picks the task. Their fixed lifetime is seven
days. At delivery, an existing token is reused only when at least six days
remain; otherwise the broker mints a new token. Rotation does not revoke older
tokens: a previously delivered token remains valid until its own expiry, which
keeps picked-task restart recovery and admin requeue from acting like an
implicit worker lease cancellation. Deleting the task revokes all of its work
tokens. They are capability material accepted by `progress_task` and
`solve_task`, but omitted from task metadata, admin responses, SSE, and logs.

## HTTP surface

| Method | Path | Purpose | Broker authentication |
|--------|------|---------|-----------------------|
| `POST` | `/rpc` | MCP / JSON-RPC | Bearer `API_KEY` when configured |
| `GET` | `/health` | Liveness, version, flags | Open |
| `GET` | `/poll/{token}` | Role pick or task-result poll | Capability token in path |
| `GET` | `/skill/install` | Embedded poller skill | Open |
| `GET` | `/admin/` | Embedded SPA | Bearer `API_KEY` when configured |
| various | `/admin/api/*` | Projects, tasks, prompts | Bearer `API_KEY` when configured |
| `GET` | `/admin/events` | `task_update` SSE stream | Bearer `API_KEY` when configured |

An outer reverse proxy may add browser-friendly Basic Auth, but must preserve the
open capability and installer paths. See
[`deploy/README-kamal.md`](../../deploy/README-kamal.md#browser-admin-authentication).

### Admin REST API

| Method | Path | Handler |
|--------|------|---------|
| `GET` | `/admin/api/projects` | `listProjects` |
| `GET` | `/admin/api/tasks` | `listTasks` with filters and pagination |
| `GET` | `/admin/api/tasks/:id` | `getTask` |
| `PATCH` | `/admin/api/tasks/:id` | `updateTaskStatus` |
| `DELETE` | `/admin/api/tasks/:id` | `deleteTask` |
| `GET` | `/admin/api/prompts` | `listPrompts` |
| `GET` | `/admin/api/prompts/:name` | `getPrompt` |
| `GET` | `/admin/events` | SSE `task_update` |

### MCP tools

| Tool | Availability | Purpose |
|------|--------------|---------|
| `create_task` | Always | Create and queue a task; returns task `poll_url` when possible |
| `await_task` | `ENABLE_SYNC` | Block until solve, timeout, or cancellation |
| `listen_role` | Always | Worker pickup; allowed modes depend on feature flags |
| `list_tasks` | Always | Return up to 20 recent metadata records |
| `get_task` | Always | Context-efficient task or result lookup |
| `solve_task` | Always | Store the final result; accepts global-task `work_token` |
| `progress_task` | Always | Append progress; accepts global-task `work_token` |

The server also exposes MCP `prompts/list` and `prompts/get`. The synthetic
`skill-install` prompt returns the same installer body as `/skill/install`.

## Configuration

The binary reads `PORT`, `DB_PATH`, `PROMPTS_DIR`, `API_KEY`, `ENABLE_SYNC`,
`ENABLE_ASYNC`, `BROKER_PUBLIC_URL`, and `BROKER_TRUST_FORWARDED`. A `.env` file
in the working directory is loaded first, while non-empty process environment
values take precedence.

At least one of sync or async mode must be enabled. `X-Project-Id` selects the
tenant for MCP and health requests; blank values use `default`. Normal roles are
local to that tenant. A `g:<key>:<queue>` role is shared by all clients connected
to this broker, while `<key>` is only a namespace identifier—not a credential or
queue ACL.

## Build and verification

- Go module minimum: Go 1.25.5.
- UI toolchain: Node `^20.19.0` or `>=22.12.0` (Vite 8).
- `make build` builds and embeds the UI, then compiles `./broker`.
- `go test ./...`, `go test -race ./...`, and `go vet ./...` currently pass in
  `agent-broker/`.
- `make test` is currently broken after the Go suite because its referenced
  integration script is missing; see
  [`BUG-004`](../bugs/bug-004-make-test-missing-integration-script.md).

Open defects are indexed in [`docs/bugs/README.md`](../bugs/README.md).
