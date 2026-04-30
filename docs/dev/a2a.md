# A2A Protocol Support

Plan for adding [Agent2Agent (A2A) protocol](https://a2aproject.github.io/A2A/) support to agent-broker, enabling gemini-cli and other A2A-compatible orchestrators to delegate tasks to the broker.

## Goal

gemini-cli (and any A2A client) discovers the broker as a remote agent, sends tasks via A2A, and gets results back — while workers continue using the existing `listen_role` / `solve_task` MCP interface unchanged.

```
gemini-cli (A2A orchestrator)
    │  GET /.well-known/agent.json   (discovery)
    │  POST /a2a  tasks/send         (delegate task)
    ▼
agent-broker  (new A2A layer on top of existing broker)
    │  internal: CreateTask + AwaitTask
    ▼
Claude Code worker  (listen_role → solve_task, unchanged)
    │  result
    ▼
agent-broker → A2A response → gemini-cli
```

Workers are not affected. The A2A layer is purely additive.

## A2A Protocol Primer

A2A v1.0 uses **JSON-RPC 2.0 over HTTP** — same as the existing `/rpc` endpoint. Key concepts:

- **Agent Card** — JSON document at `GET /.well-known/agent.json` describing the agent's name, capabilities, and skills.
- **Task** — unit of work with states: `submitted → working → completed | failed | canceled`.
- **Message / Part** — tasks carry `Message` objects with typed `Part`s (text, file, data).
- **Streaming** — optional SSE via `tasks/sendSubscribe` for long-running tasks.

A2A methods the broker needs to implement:

| Method | Description |
|--------|-------------|
| `tasks/send` | Create task, block until done, return result |
| `tasks/get` | Poll task status by ID |
| `tasks/cancel` | Cancel a running task |
| `tasks/sendSubscribe` | Create task + stream status updates via SSE (optional, phase 2) |

## Implementation Plan

### Phase 1 — Core (required for gemini-cli integration)

#### Step 1: Agent Card endpoint

Add `GET /.well-known/agent.json` to `main.go`:

```go
mux.HandleFunc("/.well-known/agent.json", handler.AgentCardHandler)
```

The Agent Card describes available skills as broker roles. Roles are discovered from the `prompts/` directory at startup (already loaded by `broker.ListPrompts()`).

```json
{
  "name": "agent-broker",
  "description": "Task broker for multi-agent workflows. Routes tasks to role-based workers.",
  "url": "http://localhost:9197/a2a",
  "version": "1.0.0",
  "capabilities": {
    "streaming": false,
    "pushNotifications": false
  },
  "skills": [
    {
      "id": "coder",
      "name": "Coder",
      "description": "Software engineering tasks: write, refactor, debug code.",
      "inputModes": ["text"],
      "outputModes": ["text"]
    },
    {
      "id": "researcher",
      "name": "Researcher",
      "description": "Research and investigation tasks.",
      "inputModes": ["text"],
      "outputModes": ["text"]
    }
  ],
  "authentication": {
    "schemes": ["Bearer"]
  }
}
```

Skills are generated dynamically from available prompts — no hardcoding needed.

#### Step 2: A2A JSON-RPC endpoint

Add `POST /a2a` route in `main.go`:

```go
mux.Handle("/a2a", a2aHandler)
```

New file `agent-broker/a2a.go` with `A2AHandler` struct wrapping the existing `*Broker`.

#### Step 3: `tasks/send`

`tasks/send` is a blocking call — it creates a task and waits for the result in one shot. This is identical to what `await_task` already does in the MCP interface (which calls `CreateTask` then `AwaitTask` back-to-back internally). No new broker logic needed.

1. Parse A2A `TaskSendParams` — extract `skill` (→ role), task text (→ task_md), optional title.
2. Call `broker.CreateTask(projectID, role, title, taskMD)` → `taskID`.
3. Call `broker.AwaitTask(ctx, projectID, taskID, timeoutMs)` → result string.
4. Return A2A `Task` object with `status.state = "completed"` and result as a text `Part`.

The split into two broker calls is only relevant for `tasks/get` (async polling). For `tasks/send` they are logically one operation.

A2A `TaskSendParams`:
```json
{
  "id": "client-generated-uuid",
  "message": {
    "role": "user",
    "parts": [{"type": "text", "text": "implement feature X"}]
  },
  "metadata": {"skill": "coder"}
}
```

Skill → role mapping: use `metadata.skill` if present, otherwise default to `"coder"`.

#### Step 4: `tasks/get`

Maps to `broker.GetTask(projectID, taskID)`:

1. Read `status.json` from disk.
2. Map broker status to A2A state: `queued/picked → working`, `solved → completed`.
3. If solved, read `result.md` and include as text Part.

#### Step 5: `tasks/cancel`

Not natively supported by the broker today. Two options:
- **Simple:** return A2A error `"task cancellation not supported"` (acceptable for v1).
- **Full:** add a `CancelTask` method to `broker.go` that closes the `done` channel and writes `canceled` status to disk.

Start with the simple option.

#### Step 6: Project ID mapping

A2A has no equivalent of `X-Project-Id`. Options:
- Use a fixed project ID per A2A endpoint (e.g., `"a2a"` or `"default"`).
- Accept project ID as a URL query param: `/a2a?project=myproject`.
- Accept it as a custom header `X-Project-Id` (same as MCP endpoint).

Recommended: support `X-Project-Id` header with fallback to `"default"` — consistent with existing behavior.

#### Step 7: Auth

The existing `AuthMiddleware` already handles Bearer tokens and wraps the entire mux. The A2A `/a2a` route is automatically protected. The Agent Card should reflect `"schemes": ["Bearer"]` only when `API_KEY` is set.

### Phase 2 — Streaming (optional)

Streaming depends on `progress_task` being implemented first (see plan-0.0.9.md). Once workers can send intermediate progress updates, SSE becomes meaningful.

#### `tasks/sendSubscribe`

For long-running tasks, A2A supports SSE streaming of status updates.

1. Create task via `CreateTask`.
2. Open SSE response (`Content-Type: text/event-stream`).
3. Stream events from `task.progress` channel as they arrive: `state: "working"` with progress message.
4. When `task.done` fires, send final `state: "completed"` event with result and close stream.

```
data: {"id":"123","status":{"state":"submitted"}}

data: {"id":"123","status":{"state":"working"},"message":{"parts":[{"type":"text","text":"migrations done, moving to models"}]}}

data: {"id":"123","status":{"state":"completed"},"result":{"parts":[{"type":"text","text":"done, here is the code..."}]}}
```

Without `progress_task`, `sendSubscribe` would only emit `submitted` then jump straight to `completed` — no different from `tasks/send`. The two features are designed together.

Note: gemini-cli works fine without streaming (phase 1 `tasks/send` uses blocking wait).

## File Changes

| File | Change |
|------|--------|
| `agent-broker/a2a.go` | New file: `A2AHandler`, all A2A methods, Agent Card builder |
| `agent-broker/main.go` | Register `/.well-known/agent.json` and `/a2a` routes |
| `agent-broker/broker.go` | Optional: add `CancelTask` for phase 1 full cancel support |
| `agent-broker/a2a_test.go` | New file: integration tests for A2A endpoints |

No changes to `jsonrpc.go`, `broker.go` core logic, or any prompts. MCP interface is untouched.

Phase 2 additionally requires changes from plan-0.0.9.md (`progress_task`).

## Testing with gemini-cli

```bash
# 1. Start broker
make run

# 2. Verify agent card
curl http://localhost:9197/.well-known/agent.json | jq .

# 3. In gemini-cli, add remote agent
# /agents add http://localhost:9197

# 4. gemini-cli should discover skills and be able to delegate tasks
```

For auth:
```bash
API_KEY=secret make run
# gemini-cli v0.33+ supports authenticated A2A agent card discovery
```

## A2A Task State Mapping

| Broker status | A2A state |
|---------------|-----------|
| `queued` | `submitted` |
| `picked` | `working` |
| `solved` | `completed` |
| — | `canceled` (phase 1: not supported) |
| — | `failed` (on broker error) |

## Open Questions

1. **Skill routing** — should the A2A skill ID map 1:1 to broker role names, or support aliases? Simplest: skill ID == role name.
2. **Task ID ownership** — A2A clients generate their own task ID (`TaskSendParams.id`). The broker generates its own internal ID. We can either use the client ID directly (after validation with `isSafeID`) or maintain a mapping. Simplest: use client ID if provided and valid, otherwise generate.
3. **Multi-part messages** — A2A messages can have multiple Parts (text + file). For now, concatenate all text Parts into a single `task_md` string. File Parts are out of scope.
