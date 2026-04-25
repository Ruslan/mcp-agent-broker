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
├── data/                       # Runtime data directory
├── docs/dev/                   # Version plans and design notes
├── examples/ralph-simple/      # Example role prompts
├── Makefile
└── README.md
```

## Core Concepts

1. Tenancy: use `X-Project-Id` to isolate tasks between projects
2. Roles: tasks are assigned to roles such as `coder`
3. Lifecycle: create a task, optionally wait for it, let a worker pick it up, then solve it

## Run

Build from the repository root:

```bash
make build
```

Run with defaults:

```bash
make run
```

Direct Go run is also fine:

```bash
cd agent-broker
go run .
```

Default server settings:

1. port: `9197`
2. data dir: `data`
3. sync enabled: `true`
4. async enabled: `true`

MCP endpoint:

```text
http://localhost:9197/rpc
```

Health endpoint:

```text
http://localhost:9197/health
```

## Environment

The server automatically loads environment variables from a `.env` file in the current working directory if it exists.

Supported environment variables:

1. `PORT`: server port, default `9197`
2. `DATA_DIR`: persistence root, default `data`
3. `API_KEY`: optional API key for authentication. If set, clients must use `Authorization: Bearer <key>` header.
4. `ENABLE_SYNC`: enables `await_task` and `listen_role(mode="wait")`, default `true`
5. `ENABLE_ASYNC`: enables `listen_role(mode="poll")`, default `true`

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

Build:

```bash
make build
```

Test:

```bash
cd agent-broker
go test -count=1 ./...
```

Extra checks:

```bash
cd agent-broker
go build ./...
go vet ./...
```
