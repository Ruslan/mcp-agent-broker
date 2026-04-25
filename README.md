# Agent Task Broker

A coordination layer for AI agents.

## Core Concepts

- **Tenancy**: Use the `X-Project-Id` HTTP header to isolate tasks between different projects.
- **Roles**: Tasks are assigned to roles (e.g., `coder`, `researcher`).
- **Lifecycle**: Create a task, optionally await its completion, and workers solve it.

## MCP Tools

The broker implements the Model Context Protocol (MCP).

### `create_task`
Creates a task and returns immediately.
- **Arguments**:
  - `role` (string): Target agent role.
  - `title` (string): Short task title (max 200 chars).
  - `task_md` (string): Detailed task description.
- **Returns**: `task_id`, `status`.

### `await_task`
Blocks until the task is solved or timeout.
- **Arguments**:
  - `task_id` (string): ID of the task to wait for.
  - `timeout_ms` (integer, optional): Maximum time to wait.
- **Returns**: `task_id`, `status`, `result_md` (if solved).

### `listen_role`
Worker-facing tool to fetch work.
- **Arguments**:
  - `role` (string): Role to listen for.
  - `mode` (string): `wait` (blocking) or `poll` (immediate).
  - `timeout_ms` (integer, optional): For `wait` mode.
- **Returns**: `task` object or `status` (empty/timeout).

### `list_tasks`
Overview of tasks in the current project.
- **Arguments**:
  - `role` (string, optional): Filter by role.
  - `status` (string, optional): Filter by status.
- **Returns**: List of task metadata.

### `get_task`
Detailed content for one task.
- **Arguments**:
  - `task_id` (string): ID of the task.
  - `include_task_md` (boolean, optional).
  - `include_result_md` (boolean, optional).
- **Returns**: Task details (defaults to result if solved, prompt otherwise).

### `solve_task`
Submit the result for a task.
- **Arguments**:
  - `task_id` (string): Task to resolve.
  - `result_md` (string): Final output.

## Server Configuration

Control available features via environment variables:

- `PORT`: Server port (default: `9197`).
- `DATA_DIR`: Root directory for task persistence (default: `data`).
- `ENABLE_SYNC`: Enable blocking tools (`await_task`, `listen_role` with `wait`). Default: `true`.
- `ENABLE_ASYNC`: Enable polling tools (`listen_role` with `poll`). Default: `true`.

Note: At least one mode must be enabled.

## Development

```bash
go build ./...
go test ./...
```
