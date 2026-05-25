# broker-queue Pi extension

Queue-driven broker workflow for coder/reviewer roles.

## Location

- Extension entry: `~/.pi/agent/extensions/broker-queue/index.ts`
- Shared broker client: `~/.pi/agent/extensions/broker-queue/broker-client.ts`

## Config

Global config file (required):

`~/.pi/agent/broker.json`

```json
{
  "url": "https://your-broker/mcp",
  "username": "your-user",
  "password": "your-password",
  "xProjectId": "default-project-id",
  "timeout": 3700000
}
```

Localhost / no-auth example:

```json
{
  "url": "http://127.0.0.1:8000/mcp",
  "xProjectId": "local-project",
  "timeout": 3700000
}
```

Optional project override (current working directory):

`<cwd>/.pi/agent/broker.json`

```json
{
  "xProjectId": "project-specific-id"
}
```

Notes:
- Required after merge: `url`, `xProjectId`.
- Basic auth is optional. If both `username` and `password` are set, extension sends:
  - `Authorization: Basic <base64(username:password)>`
- Extension always sends:
  - `x-project-id: <xProjectId>`
- Do **not** commit secrets to Git. Keep real credentials in `~/.pi/agent/broker.json`.

## Commands

- `/c1 [role]`
  - Start loop with **coder instructions**.
  - Default role: `coder`.
- `/r1 [role]`
  - Start loop with **reviewer instructions**.
  - Default role: `reviewer`.
- `/cstop`
  - Stop active waiting loop.
- `/cstatus`
  - Show current state (`Idle`, waiting, or active task).
- `/cbump`
  - Re-send full prompt for current active task.

Examples:
- `/c1`
- `/c1 coder2`
- `/r1`
- `/r1 rw_cheap`

## Runtime behavior

1. Command first checks for already picked tasks (`list_tasks --status picked`).
   - `list_tasks` may return lightweight metadata only, so extension fetches full task text via `get_task` before publishing resumed picked tasks.
2. If none is picked, command starts wait loop (`listen_role --mode wait`).
3. If transport fails, it retries automatically.
4. When one task arrives, extension posts:
   - basic role rules,
   - task id,
   - task markdown,
   - required solution template.
5. Assistant must reply with:

```xml
<solution task_id="TASK_ID">
# Result
...markdown...
</solution>
```

6. Extension parses block and submits `solve_task`.
7. If continuous mode is active (started via `/c1` or `/r1` and not stopped), it immediately resumes waiting for next task.

## Important constraints

- Only one active task is handled at a time.
- New tasks are not pulled while `activeTask` is unsolved.
- Resume mode picks the first `picked` task for the requested role. It assumes a single worker per role/project or broker-side ownership; if multiple workers share the same role, configure distinct roles.
- `task_id` can be any non-empty quoted value (except quote/newline).

## Reload

After editing extension/config:

- Run `/reload` in Pi.
