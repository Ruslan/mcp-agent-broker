# Agent Broker

MCP/JSON-RPC broker for delegating tasks between AI agent roles.

## Integrations

The broker exposes a single MCP / JSON-RPC endpoint and can be used from different orchestrators and coding agents.

### Claude Code

```bash
claude mcp add --transport http agent-broker \
  "http://localhost:9197/rpc"
```

Or in `.mcp.json` at the project root:

```json
{
  "mcpServers": {
    "agent-broker": {
      "type": "http",
      "url": "http://localhost:9197/rpc"
    }
  }
}
```

### Gemini CLI

`.gemini/settings.json`:

```json
{
  "mcpServers": {
    "agent-broker": {
      "httpUrl": "http://localhost:9197/rpc"
    }
  }
}
```

### OpenCode

`.opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "agent-broker": {
      "type": "remote",
      "enabled": true,
      "url": "http://localhost:9197/rpc"
    }
  }
}
```

### Pi Agent

Pi does not currently support MCP directly, but this repository includes an experimental extension in `extensions/pi/broker-queue/` that connects Pi to the broker queue workflow through the MCP endpoint without stuffing broker protocol details into the main chat context. The Pi extension is currently being tested.

For multi-project usage, send `X-Project-Id` on requests so task queues stay isolated per project.
When several projects use the same broker and intentionally share a worker queue, address it as
`g:<key>:<queue>` (for example `g:shared-lib:maintainer`). The complete role is routed globally while
the task remains owned under its creator's project. When a worker picks a global task, the broker
durably records that worker's `X-Project-Id`. The same worker project can then rediscover it through
`list_tasks`, reread it with `get_task`, and call `progress_task` or `solve_task` after a client
restart. Delivery also includes an opaque `work_token` as a task-scoped bearer fallback. Local roles
remain token-free and project-local. `<key>` is a namespace label, not a secret or an ACL—broker
authentication is still the security boundary. Global queues do not span separate broker instances.
An admin requeue clears the worker-project assignment but does not revoke an already issued work
token; that token remains valid until its fixed expiry (unless the task is deleted). Requeue is
therefore routing reassignment, not capability revocation.

The current API is based on a small task lifecycle:

1. `create_task`
2. `await_task`
3. `listen_role`
4. `list_tasks`
5. `get_task`
6. `solve_task`
7. `progress_task` (optional intermediate updates)

## Repository Layout

```text
.
├── agent-broker/               # Go server
├── ui/                         # Svelte 5 admin dashboard (sources)
├── data/                       # Runtime data directory
├── docs/dev/                   # Version plans and design notes
├── docs/bugs/                  # Open defect cards and backlog index
├── docs/tasks/                 # Feature task cards and backlog index
├── deploy/                     # systemd and Kamal deployment files
├── extensions/pi/              # Experimental Pi broker-queue extension
├── examples/ralph-simple/      # Example role prompts
├── Makefile
└── README.md
```

## Prerequisites

- **Go** 1.25.5+ (the minimum declared by `agent-broker/go.mod`)
- **Node.js** `^20.19.0` or `>=22.12.0` and **npm** (required by Vite 8)
- **Make** (optional, for convenience targets)

## Core Concepts

1. Tenancy: use `X-Project-Id` to isolate task ownership and local queues between projects
2. Roles: tasks are assigned to local roles such as `coder`, or opt-in global roles such as
   `g:shared-lib:maintainer`
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

### Running as a service (systemd)

For a persistent deployment (autostart on boot, auto-restart, journald logs)
instead of `make run` in tmux, see [`deploy/README.md`](deploy/README.md).
Quick start: `make systemd-install` once, then `make systemd-restart` to
rebuild and redeploy.

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

Other HTTP surfaces:

| Method | Path | Authentication | Description |
|--------|------|----------------|-------------|
| `GET` | `/health` | Open in the broker | Liveness and version information |
| `GET` | `/poll/:token` | Token in URL | Async role/task capability polling |
| `GET` | `/skill/install` | Open in the broker | Embedded async-poller skill installer |

Admin REST API:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/api/projects` | List distinct project IDs |
| `GET` | `/admin/api/tasks` | List tasks; query params: `project`, `role`, `status`, `limit`, `offset`; response shape: `{ "tasks": [...], "total": 123 }` |
| `GET` | `/admin/api/tasks/:id` | Task detail: metadata + `task_md` + `result_md` + `progress` |
| `PATCH` | `/admin/api/tasks/:id` | Update task status; JSON body: `{ "status": "queued"|"solved" }` |
| `DELETE` | `/admin/api/tasks/:id` | Delete a task |
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
7. `BROKER_PUBLIC_URL`: base URL used to build the `poll_url` values handed to clients (e.g.
   `https://broker.example.com`). Pins scheme + host explicitly. When unset, the base is derived from
   the request scheme + `Host` — behind a TLS-terminating proxy that sets `X-Forwarded-Proto: https`,
   the derived base is already `https://` (see below), so this is optional even for a public deployment.
8. `BROKER_TRUST_FORWARDED`: when `true`, also honor `X-Forwarded-Host` when building `poll_url`.
   Default off — enable it ONLY behind a proxy you trust to set it, since a client-forged host would
   point a poller's live capability token at another host. Note `X-Forwarded-Proto` is honored
   **regardless** of this flag: it only flips the scheme (never redirects the token), so a broker behind
   Caddy/nginx emits `https://` `poll_url`s automatically, with no config and no hardcoded domain.

At least one of `ENABLE_SYNC` or `ENABLE_ASYNC` must stay enabled.

### Authentication and reverse proxies

When `API_KEY` is set, the broker expects `Authorization: Bearer <key>` on
`/rpc` and `/admin`. The current browser SPA does not prompt for that bearer
key, so a public deployment normally puts browser-friendly authentication such
as Basic Auth on a trusted reverse proxy.

If the proxy is the sole authentication boundary, protect both `/rpc` and
`/admin`, and do not expose the broker port directly. Keep `/poll/` and
`/skill/install` outside proxy authentication: poll URLs are self-authorizing
capabilities, while the installer is intentionally public. Decide separately
whether external `/health` should be public; the broker itself leaves it open so
container and load-balancer health checks work.

If `API_KEY` remains enabled behind the proxy, the proxy must pass a valid Bearer
header upstream. A browser's Basic Authorization header does not satisfy the
broker middleware by itself.

## Async pollers (keep working, get woken)

`listen_role(mode="wait")` and `await_task` block the calling turn *inside* the tool call — the agent
can't do anything else while it waits. For harnesses that re-invoke an agent when a **backgrounded
process exits** (Claude Code `run_in_background`, agy), the broker hands back a **`poll_url`** that a
dumb background script polls, turning "wait" into "keep working, get woken".

**The `poll_url` capability.** `create_task`, `listen_role(mode="poll")`, and `get_task` return a
`poll_url` like `http://host:9197/poll/<token>` — an unguessable token **in the path**. That URL *is*
the authorization: `GET /poll/<token>` is served **without any API key or header** (it isn't behind the
command-channel auth), so a poller is just `curl "$poll_url"`. A role `poll_url` picks a queued task; a
task `poll_url` reports status and returns `result_md` once solved.

- **`broker-poll.sh <poll_url>`** — worker. Polls a role `poll_url` until a task is queued, **picks**
  it, prints it, exits 0.
- **`await-poll.sh <poll_url>`** — dispatcher. Polls a task `poll_url` until `solved`, prints the
  result, exits 0.
- **`broker-monitor.sh <poll_url>`** — streaming variant that never exits (for a Monitor-style tool);
  emits each new event as a JSONL line.

Arm one on the `poll_url` in the background, keep working, and the harness wakes the agent when the
script exits. Env: `BROKER_POLL_URL` (the url, if not passed as `$1`), `BROKER_INTERVAL` (default 5),
`BROKER_MAX_FAIL` (default 12), `BROKER_MAX_WAIT` (opt-in per-turn bound, exit 4; unbounded by default).

**Token expiry — go get a new one.** The token in a `poll_url` has a **sliding ~30-minute TTL**: each
poll renews it, so an actively-polling script keeps it alive, up to a **hard 24-hour cap** from
creation. A leaked `poll_url` therefore can't be read from "a day later", and a stalled poller's token
dies within 30 minutes. An **expired** token (existed, then slid out or hit the cap) returns `200
{"status":"expired"}`; an **unknown** token returns a bare **404**, as if the URL never existed, so
probing random tokens reveals nothing about the `/poll` surface. Either way the script exits **5** and
the agent calls `listen_role` (worker) or `get_task` (dispatcher) again for a fresh `poll_url`. Because
the token is a capability, not encryption, serve a public broker over TLS.

**Installing the scripts (no local clone needed).** Any MCP client connected to the broker can pull
the scripts via the `skill-install` prompt (`prompts/get skill-install`), which prints
`broker-poll.sh` / `await-poll.sh` / `broker-monitor.sh` / `SKILL.md` verbatim (embedded in the broker
binary) with install instructions. The canonical sources live in `agent-broker/skillfiles/`;
`make sync-skillfiles` regenerates the `.claude/skills/broker-async-poll/` install copy, and a parity
test keeps the two in lockstep.

**Harness can't do MCP prompts?** The same installer is served over plain HTTP at
`GET /skill/install` — byte-for-byte the `skill-install` body as `text/markdown`, **no API key**
(non-secret embedded scripts, exempt like `/poll/`). Just `wget http://host:9197/skill/install` (or
`curl`) and hand the file to any agent; the `X-Broker-Skill-Version` response header carries the
version for update checks.

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
  "status": "queued",
  "poll_url": "http://host:9197/poll/<token>"
}
```

`poll_url` is a capability URL scoped to this `task_id`, for arming `await-poll.sh` — see
[Async pollers](#async-pollers-keep-working-get-woken). `listen_role(mode="poll")` returns an analogous
`poll_url` scoped to `(project, role)`, and `get_task` returns a fresh one.

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

Returns up to 20 most recent lightweight metadata records, newest first.

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

### `progress_task`

Appends a short intermediate update without completing the task. Messages are
persisted in the task progress log and may be reported by `await_task`.

```json
{
  "task_id": "...",
  "message": "tests pass; reviewing the migration"
}
```

The message limit is 500 characters.

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

- Go 1.25.5+
- Node.js `^20.19.0` or `>=22.12.0` / npm

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

Race and static checks:

```bash
cd agent-broker
go test -race ./...
go vet ./...
```

`make test` is intended to be the repository-wide entry point, but it currently
references an integration script that is absent from the Git tree. Until
[BUG-004](docs/bugs/bug-004-make-test-missing-integration-script.md) is resolved,
run the Go commands above plus `make ui-build`.

### Extra checks

```bash
cd agent-broker
go build ./...
```

### UI development

For local UI development with hot reload:

```bash
cd ui
npm ci
npm run dev
```

The Vite dev server proxies are not configured — this is for iterating on the UI only. For a full integration test, use `make build && make run`.

## Known defects

Open defects and their acceptance criteria are tracked as one Markdown file per
issue in [`docs/bugs/`](docs/bugs/README.md).
