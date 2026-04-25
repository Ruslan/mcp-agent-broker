# plan-0.0.6

## Goal

Add multi-project tenancy through the HTTP header `X-Project-Id`.

The broker should support one shared MCP server for many projects while keeping task state isolated per project.

## Core Behavior

Every request is assigned to a project.

Project resolution rule:

1. If request header `X-Project-Id` is present and non-empty, use that value.
2. Otherwise use the implicit project ID `default`.

This keeps backward compatibility for existing clients while enabling explicit multi-project routing.

## Why

Current state:

1. all tasks live in one global namespace
2. all listeners are shared across all work
3. all disk artifacts are stored under one task root without project boundaries

Problems:

1. one MCP server cannot safely serve many projects
2. roles like `coder` collide across unrelated projects
3. async queues and statuses are mixed together

`v0.0.6` fixes this by namespacing all broker state by project.

## Scope

Included:

1. project resolution from `X-Project-Id`
2. per-project task isolation in memory
3. per-project listener isolation
4. per-project async queue isolation
5. per-project disk layout
6. backward compatibility via `default`

Not included:

1. project listing APIs
2. auth or access control
3. project deletion or cleanup tools
4. per-project config overrides

## Header Contract

Header name:

```text
X-Project-Id
```

Rules:

1. case-insensitive as normal HTTP header handling
2. if missing, use `default`
3. if present but blank after trimming whitespace, use `default`
4. project IDs must be path-safe

## Validation

Project ID must:

1. be non-empty after defaulting
2. not be `.` or `..`
3. not contain path separators `/` or `\`
4. ideally be short and human-readable

Suggested examples:

1. `default`
2. `gemini-agent`
3. `project-a`
4. `customer-42`

If invalid, return application error.

## In-Memory Model Changes

Current model is effectively global:

```go
listeners  map[string]chan *Task
tasks      map[string]*Task
asyncQueue map[string][]*Task
```

New model should isolate by project.

Recommended shape:

```go
listeners  map[string]map[string]chan *Task
tasks      map[string]map[string]*Task
asyncQueue map[string]map[string][]*Task
```

Meaning:

1. outer key = `projectID`
2. inner key = role or task ID

Example:

1. `listeners["project-a"]["coder"]`
2. `tasks["project-a"]["uuid"]`
3. `asyncQueue["project-a"]["coder"]`

## Task Model Changes

Extend `Task` with `ProjectID`:

```go
type Task struct {
    ID        string
    ProjectID string
    Role      string
    Title     string
    MD        string
    done      chan string
}
```

Extend `StatusMetadata` with `project_id`:

```go
type StatusMetadata struct {
    TaskID     string     `json:"task_id"`
    ProjectID  string     `json:"project_id"`
    Role       string     `json:"role"`
    Title      string     `json:"title"`
    Status     TaskStatus `json:"status"`
    CreatedAt  time.Time  `json:"created_at"`
    UpdatedAt  time.Time  `json:"updated_at"`
}
```

## Storage Layout

Move from:

```text
./task-data/<task_id>/...
```

To:

```text
./task-data/<project_id>/<task_id>/task.md
./task-data/<project_id>/<task_id>/result.md
./task-data/<project_id>/<task_id>/status.json
```

This isolates artifacts by project.

Example:

```text
./task-data/default/abcd1234/task.md
./task-data/gemini-agent/efgh5678/status.json
```

## Broker Helper Changes

Task directory helper should now include project:

```go
func (b *Broker) taskDir(projectID, taskID string) string
```

Persistence helpers should now include project:

```go
func (b *Broker) persistTask(projectID, taskID, taskMD string) error
func (b *Broker) persistStatus(projectID, taskID, role, title string, status TaskStatus) error
func (b *Broker) persistResult(projectID, taskID, resultMD string) error
```

Query helpers should now include project:

```go
func (b *Broker) GetTaskStatus(projectID, taskID string) (*StatusMetadata, error)
func (b *Broker) GetTaskResult(projectID, taskID string) (string, error)
```

## Request Routing

Project routing happens at HTTP layer.

The handler should:

1. read `X-Project-Id`
2. resolve it to an effective `projectID`
3. pass that `projectID` into broker operations

The user-facing tools do not need a new `project_id` argument.

Reason:

1. tenancy belongs to transport/request context
2. avoids cluttering every tool schema
3. works naturally for MCP clients configured per project

## API Surface

Tool names do not change.

No new tool parameters are required.

The change is contextual:

1. same tool call under different `X-Project-Id` goes to different isolated broker state

## Behavioral Examples

### Same role, different projects

Scenario:

1. request A has `X-Project-Id: app-1`
2. request B has `X-Project-Id: app-2`
3. both use role `coder`

Expected:

1. `listen_role_sync(coder)` in `app-1` only sees tasks from `app-1`
2. `listen_role_sync(coder)` in `app-2` only sees tasks from `app-2`
3. no cross-project task delivery

### Missing header

Scenario:

1. request has no `X-Project-Id`

Expected:

1. task is created in project `default`
2. listeners without the header also operate in `default`

## 0.0.4.1 Interaction

The rule from `0.0.4.1` still applies, but now only within a project.

Meaning:

1. async-created tasks in project `A` are visible to `listen_role_sync` in project `A`
2. async-created tasks in project `A` are not visible to listeners in project `B`

## MCP Behavior

No changes to `tools/list` schemas are required.

Optional documentation improvement:

1. mention that project routing is determined by `X-Project-Id`
2. mention that missing header maps to `default`

## Testing

Add coverage for:

1. create task without header -> stored under `default`
2. create task with `X-Project-Id: project-a` -> stored under `project-a`
3. same role in two projects does not cross-deliver
4. same `task_id` can exist in different projects if server-generated IDs are mocked that way in tests
5. `get_task_status` only finds task inside the matching project
6. `get_task_result` only finds task inside the matching project
7. `listen_role_sync` in project A can consume async task from project A
8. `listen_role_sync` in project A cannot consume async task from project B
9. invalid `X-Project-Id` is rejected
10. disk paths are namespaced under `task-data/<project_id>/...`
11. `go build ./...` succeeds
12. `go vet ./...` succeeds
13. `go test -count=1 ./...` succeeds

## Acceptance Criteria

1. broker resolves project from `X-Project-Id`
2. missing header maps to `default`
3. all in-memory task state is isolated per project
4. listeners are isolated per project
5. async queues are isolated per project
6. status/result queries are isolated per project
7. disk layout is isolated under `task-data/<project_id>/<task_id>/`
8. `0.0.4.1` async-visible-to-sync behavior still works within one project
9. no cross-project task delivery occurs
10. `go build ./...` succeeds
11. `go vet ./...` succeeds
12. `go test -count=1 ./...` succeeds

## Future Work

After `v0.0.6`, likely next steps are:

1. project-aware task listing
2. project metrics
3. per-project retention policies
4. auth/access control for project isolation
