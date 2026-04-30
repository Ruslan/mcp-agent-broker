# Plan 0.0.11: Svelte Admin UI

## Goal

A lightweight browser-based admin panel embedded in the broker binary. Shows all tasks across projects, lets you inspect content and results, and reflects live status changes.

Depends on plan-0.0.10 (SQLite) — `ListTasks` must be fast before building a UI on top of it.

## What it shows

- Task list with project, role, status, title, timestamps
- Filter by project / role / status
- Task detail: task description, result, progress messages
- Live status updates without full page reload
- Read-only for now (no create/cancel from UI)

## Tech stack

| Concern | Choice | Reason |
|---------|--------|--------|
| Framework | Svelte 5 + Vite | Small bundle, no virtual DOM overhead |
| Styling | Plain CSS (or Pico CSS) | No build-time dependency on Tailwind |
| Real-time | SSE (`/admin/events`) | Already planned for A2A; reuse the same pattern |
| Build | `vite build` → `dist/` | Static assets embedded in Go binary |
| Embedding | `//go:embed` | Single binary, no separate file serving |

No SvelteKit — plain Svelte SPA is enough for an admin panel.

## New HTTP endpoints

The existing JSON-RPC `/rpc` endpoint is not suitable for a browser UI (requires POST with specific framing). Add a thin REST layer at `/admin/api/`:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/api/projects` | List distinct project IDs |
| `GET` | `/admin/api/tasks` | List tasks; query params: `project`, `role`, `status` |
| `GET` | `/admin/api/tasks/:id` | Task detail: metadata + task_md + result_md + progress |
| `GET` | `/admin/events` | SSE stream: task status change events |
| `GET` | `/admin/*` | Serve embedded Svelte SPA |

The REST handlers call the same `Store` methods as the JSON-RPC layer — no duplication of broker logic.

### SSE event format

```
event: task_update
data: {"project_id":"default","task_id":"abc","status":"solved","updated_at":"..."}

event: task_update
data: {"project_id":"default","task_id":"xyz","status":"picked","updated_at":"..."}
```

The broker emits events on status transitions: `queued → picked → solved`. The SSE handler keeps a registry of active admin connections and fans out to all of them.

## Broker changes

### Event bus

Add a simple fan-out broadcaster to `Broker` for admin SSE clients:

```go
type statusEvent struct {
    ProjectID string
    TaskID    string
    Status    TaskStatus
    UpdatedAt time.Time
}

type Broker struct {
    // ...existing fields...
    adminSubs   map[chan statusEvent]struct{}
    adminSubsMu sync.Mutex
}
```

`publishAdminEvent(e statusEvent)` called from `CreateTask` (queued), `ListenRole` (picked), `SolveTask` (solved). Non-blocking: slow admin clients are dropped.

`Subscribe() chan statusEvent` / `Unsubscribe(ch chan statusEvent)` for SSE handler registration.

## Directory layout

```
ui/
  src/
    App.svelte
    lib/
      TaskList.svelte
      TaskDetail.svelte
      StatusBadge.svelte
      api.js          # fetch wrappers
      events.js       # SSE client
  index.html
  vite.config.js
  package.json
agent-broker/
  admin.go            # REST handlers + SSE handler
  admin_embed.go      # //go:embed ui/dist
```

## Build integration

```makefile
ui-build:
    cd ui && npm ci && npm run build

build: ui-build
    cd $(SOURCE_DIR) && go build -o ../$(BINARY_NAME) .
```

`ui/dist/` is committed to the repo so the binary can be built without Node.js installed (Go embed only needs the static files to exist at compile time). The `ui-build` target is run manually or in CI when the UI changes.

### `admin_embed.go`

```go
package main

import "embed"

//go:embed all:../ui/dist
var adminFS embed.FS
```

### `main.go` routes

```go
mux.Handle("/admin/api/", adminAPIHandler)
mux.HandleFunc("/admin/events", adminSSEHandler)
mux.Handle("/admin/", http.StripPrefix("/admin", http.FileServer(http.FS(adminFS))))
```

The `/admin/` route serves the SPA; all other paths fall through to `index.html` for client-side routing.

## Auth

The existing `AuthMiddleware` covers all routes including `/admin/`. No separate auth needed. When `API_KEY` is set, the browser must pass the token — the UI reads it from `localStorage` or a login prompt.

## UI screens

### Task list

```
[ Project: all ▼ ] [ Role: all ▼ ] [ Status: all ▼ ]   🔄 live

  Project    Role      Status   Title                  Updated
  ─────────────────────────────────────────────────────────────
  default    coder   ● queued   Add auth middleware     12:04
  default    coder   ● picked   Fix SQL migration       12:01
  project-x  reviewer ✓ solved  Review PR #42          11:58
```

Status badge colors: queued=gray, picked=yellow, solved=green.

### Task detail (side panel or modal)

```
Task: Add auth middleware
Project: default  Role: coder  Status: picked  Created: 12:04

── Description ──────────────────────────────────────────
Implement Bearer token middleware in main.go...

── Progress ─────────────────────────────────────────────
12:05  reading existing middleware code
12:06  writing AuthMiddleware function

── Result ───────────────────────────────────────────────
(pending)
```

Progress messages and result update live via SSE without reload.

## Affected files

| File | Change |
|------|--------|
| `agent-broker/admin.go` | New: REST API handlers, SSE handler |
| `agent-broker/admin_embed.go` | New: `//go:embed ui/dist` |
| `agent-broker/broker.go` | Add admin event bus (subscribe/publish) |
| `agent-broker/main.go` | Register `/admin/` routes |
| `ui/` | New: Svelte project |
| `Makefile` | Add `ui-build` target, update `build` |
| `.gitignore` | Add `ui/node_modules/` |

## Open questions

1. **Auth in browser** — when `API_KEY` is set, how does the user provide it? Options: query param (`/admin/?key=...`), a login form storing to `localStorage`, or HTTP Basic Auth. Login form + `localStorage` is the most ergonomic.
2. **Pagination** — `ListTasks` returns all tasks. For large deployments add `limit` / `offset` to the REST endpoint and infinite scroll in the UI.
3. **`ui/dist/` in git** — committing build artifacts is pragmatic but messy. Alternative: require `make ui-build` before `make build`, fail the Go build if `ui/dist/` is missing. Decide based on CI setup.
