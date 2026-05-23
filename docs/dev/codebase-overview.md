# Codebase Overview

Generated 2026-05-23. Update when architecture changes significantly.

## Directory Structure

```
.
├── agent-broker/               # Go backend binary + source
│   ├── main.go                 # Entrypoint: server, routing, auth middleware
│   ├── broker.go               # Core task broker logic (create/listen/solve/await)
│   ├── store.go                # Storage abstraction + SQLite implementation
│   ├── admin.go                # Admin REST API handler (tasks, projects, prompts, SSE)
│   ├── admin_embed.go          # Embeds compiled UI dist/ into Go binary
│   ├── jsonrpc.go              # JSON-RPC 2.0 handler (MCP protocol)
│   ├── go.mod / go.sum
│   ├── dist/                   # Compiled Svelte frontend (embedded at build time)
│   └── *_test.go
├── ui/                         # Svelte 5 frontend source
│   ├── src/
│   │   ├── App.svelte          # Single-page admin UI component
│   │   ├── main.js             # Svelte mount entry
│   │   └── app.css             # Global styles (PicoCSS + dark theme)
│   ├── index.html
│   ├── package.json            # deps: svelte, vite, marked, dompurify, picocss
│   ├── vite.config.js
│   └── svelte.config.js
├── data/                       # SQLite database (runtime)
├── docs/dev/                   # Design docs and version plans
├── examples/ralph-simple/
├── prompts/                    # Built-in prompt templates
├── Makefile                    # build, run, test, clean, ui-build targets
└── README.md
```

## Architecture

- **Backend**: Single Go binary serving JSON-RPC 2.0 (MCP protocol), REST admin API, SSE events, and embedded Svelte SPA.
- **Frontend**: Svelte 5 SPA compiled to static files, embedded via Go `embed.FS` at build time (`make ui-build`).
- **Database**: SQLite via `modernc.org/sqlite` (pure Go, WAL mode, in `data/broker.db`).

## Database Schema

### Table: `tasks`

| Column     | Type | Notes                       |
|------------|------|-----------------------------|
| project_id | TEXT | NOT NULL                    |
| task_id    | TEXT | NOT NULL                    |
| role       | TEXT | NOT NULL                    |
| title      | TEXT | NOT NULL                    |
| status     | TEXT | DEFAULT 'queued'            |
| task_md    | TEXT | NOT NULL                    |
| result_md  | TEXT | NULL until solved           |
| created_at | TEXT | RFC3339                     |
| updated_at | TEXT | RFC3339                     |

PRIMARY KEY (project_id, task_id)

INDEXES: `idx_tasks_project_role`, `idx_tasks_project_status`

### Table: `task_progress`

| Column     | Type    | Notes                              |
|------------|---------|------------------------------------|
| project_id | TEXT    | NOT NULL                           |
| task_id    | TEXT    | NOT NULL                           |
| sequence   | INTEGER | PRIMARY KEY AUTOINCREMENT          |
| message    | TEXT    | NOT NULL                           |
| created_at | TEXT    | NOT NULL                           |

FOREIGN KEY (project_id, task_id) REFERENCES tasks ON DELETE CASCADE

## API Endpoints

### Admin REST API

| Method | Path                       | Handler          |
|--------|----------------------------|------------------|
| GET    | /admin/api/projects        | listProjects     |
| GET    | /admin/api/tasks           | listTasks        |
| GET    | /admin/api/tasks/:id       | getTask          |
| PATCH  | /admin/api/tasks/:id       | updateTaskStatus |
| DELETE | /admin/api/tasks/:id       | deleteTask       |
| GET    | /admin/api/prompts         | listPrompts      |
| GET    | /admin/api/prompts/:name   | getPrompt        |
| GET    | /admin/events              | SSE task_update  |

### JSON-RPC API (all at POST /rpc)

| tool name           | Description                        |
|---------------------|------------------------------------|
| list_tasks          | Up to 20 StatusMetadata, filtered  |
| get_task            | Full task details + result         |
| create_task         | Creates task, returns task_id      |
| solve_task          | Submit result, transition to solved|
| await_task          | Block until solved/timeout         |
| listen_role         | Worker pick-up (wait/poll)         |
| progress_task       | Append progress message            |

## Go Source Files (key types)

- `broker.go` — `TaskStatus`, `StatusMetadata`, `Task` struct, `Broker` core logic.
- `store.go` — `Store` interface, `SQLiteStore` implementation.
- `admin.go` — `AdminHandler`, all REST endpoints.
- `jsonrpc.go` — `JSONRPCHandler`, tool call dispatch.
- `main.go` — Server setup, routing, auth middleware.
