# Agent Broker

MCP/JSON-RPC broker for delegating tasks between AI agent roles.

The current API is based on a small task lifecycle:

1. `create_task`
2. `await_task`
3. `listen_role`
4. `list_tasks`
5. `get_task`
6. `solve_task`

## Repository Layout

```text
.
├── agent-broker/               # Go server
├── ui/                         # Svelte 5 admin dashboard (sources)
├── data/                       # Runtime data directory
├── docs/dev/                   # Version plans and design notes
├── examples/ralph-simple/      # Example role prompts
├── Makefile
└── README.md
```

## Prerequisites

- **Go** 1.22+
- **Node.js** 18+ and **npm** (for building the admin UI)
- **Make** (optional, for convenience targets)

## Core Concepts

1. Tenancy: use `X-Project-Id` to isolate tasks between projects
2. Roles: tasks are assigned to roles such as `coder`
3. Lifecycle: create a task, optionally wait for it, let a worker pick it up, then solve it

## Build & Run

Build from the repository root (builds the admin UI, then compiles the Go binary):

```bash
make build
```

This runs `npm ci && npm run build` in `ui/`, copies the compiled assets to `agent-broker/dist/`, then builds the Go binary with embedded static files.

Run with defaults:

```bash
make run
```

Direct Go run (requires `agent-broker/dist/` to exist from a prior `make ui-build`):

```bash
cd agent-broker
go run .
```

Default server settings:

1. port: `9197`
2. database: `data/broker.db`
3. sync enabled: `true`
4. async enabled: `true`

## Endpoints

MCP / JSON-RPC:

```text
http://localhost:9197/rpc
```

Health check:

```text
http://localhost:9197/health
```

Admin UI (browser):

```text
http://localhost:9197/admin/
```

Admin REST API:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/api/projects` | List distinct project IDs |
| `GET` | `/admin/api/tasks` | List tasks; query params: `project`, `role`, `status` |
| `GET` | `/admin/api/tasks/:id` | Task detail: metadata + task_md + result_md |
| `GET` | `/admin/events` | SSE stream: live task status updates |

## Environment

The server automatically loads environment variables from a `.env` file in the current working directory if it exists.

Supported environment variables:

1. `PORT`: server port, default `9197`
2. `DB_PATH`: SQLite database path, default `data/broker.db`
3. `PROMPTS_DIR`: prompt templates directory, default `prompts`
4. `API_KEY`: optional API key for authentication. If set, clients must use `Authorization: Bearer <key>` header.
5. `ENABLE_SYNC`: enables `await_task` and `listen_role(mode="wait")`, default `true`
6. `ENABLE_ASYNC`: enables `listen_role(mode="poll")`, default `true`

At least one of `ENABLE_SYNC` or `ENABLE_ASYNC` must stay enabled.

## Tool Summary

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

### `await_task`

Blocks until the task reaches a terminal state or timeout.

Arguments:

```json
{
  "task_id": "...",
  "timeout_ms": 30000
}
```

Solved response:

```json
{
  "task_id": "...",
  "status": "solved",
  "result_md": "..."
}
```

Timeout or still-running response example:

```json
{
  "task_id": "...",
  "status": "queued"
}
```

### `listen_role`

Worker-facing tool for both blocking wait and non-blocking polling.

Arguments:

```json
{
  "role": "coder",
  "mode": "wait",
  "timeout_ms": 30000
}
```

Modes:

1. `wait`: block until a task arrives or timeout
2. `poll`: return immediately if no task is available

Task response:

```json
{
  "task": {
    "task_id": "...",
    "title": "...",
    "task_md": "..."
  }
}
```

Empty poll response:

```json
{
  "task": null,
  "status": "empty"
}
```

Timed out wait response:

```json
{
  "task": null,
  "status": "timeout"
}
```

### `list_tasks`

Returns lightweight metadata only.

Allowed filters:

1. `role`
2. `status`

Example:

```json
{
  "role": "coder",
  "status": "queued"
}
```

Response shape:

```json
{
  "tasks": [
    {
      "task_id": "...",
      "project_id": "default",
      "role": "coder",
      "title": "fix failing tests",
      "status": "queued",
      "created_at": "...",
      "updated_at": "..."
    }
  ]
}
```

### `get_task`

Returns details for one task, but defaults to a context-efficient payload.

Arguments:

```json
{
  "task_id": "..."
}
```

Default behavior:

1. solved task: returns `result_md`
2. unfinished task: returns `task_md`

Optional full payload:

```json
{
  "task_id": "...",
  "include_task_md": true,
  "include_result_md": true
}
```

### `solve_task`

Finalizes a task.

Arguments:

```json
{
  "task_id": "...",
  "result_md": "..."
}
```

## Typical Flows

### Sync Orchestrator Flow

1. call `create_task`
2. keep the returned `task_id`
3. call `await_task`
4. review the returned result

### Async Orchestrator Flow

1. call `create_task`
2. keep the returned `task_id`
3. later call `get_task`
4. optionally use `list_tasks` for discovery

### Worker Flow

1. call `listen_role(mode="wait")` for blocking worker behavior
2. or call `listen_role(mode="poll")` for polling worker behavior
3. do the work
4. call `solve_task`

## Tenancy

Use the `X-Project-Id` header on requests.

Rules:

1. missing or blank header becomes `default`
2. invalid path-like values are rejected
3. tasks, listeners, and persisted state are isolated per project

## Example Prompts

See `examples/ralph-simple/` for example prompts for:

1. `main` sync
2. `main` async
3. `coder` sync
4. `coder` async

## Development

### Prerequisites

- Go 1.22+
- Node.js 18+ / npm

### Build

```bash
make build
```

This builds the UI first (`make ui-build`), then compiles the Go binary.

To rebuild only the UI:

```bash
make ui-build
```

### Test

```bash
cd agent-broker
go test -count=1 ./...
```

### Extra checks

```bash
cd agent-broker
go build ./...
go vet ./...
```

### UI development

For local UI development with hot reload:

```bash
cd ui
npm ci
npm run dev
```

The Vite dev server proxies are not configured — this is for iterating on the UI only. For a full integration test, use `make build && make run`.
