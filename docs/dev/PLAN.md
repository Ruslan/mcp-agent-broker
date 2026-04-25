# Agent Task Broker — Plan

## What to Build

An HTTP server in **pure Go** (stdlib only, zero external dependencies) that acts as a coordination layer between AI agents. Agents communicate via **JSON-RPC 2.0 over HTTP**.

The server holds all state in memory. One role = one active listener at a time. Tasks are rendezvous points: `create_task` blocks until a worker solves the task, `listen_role` blocks until a task arrives.

> Note: This protocol is compatible with MCP (Model Context Protocol) Streamable HTTP transport — MCP is just JSON-RPC 2.0 over HTTP with a specific initialization handshake. The server described here IS a valid MCP server. If you know MCP, treat this as an MCP tool server. If you don't, treat it as plain JSON-RPC 2.0 — it's the same wire format.

---

## File Structure

```
agent-broker/
├── main.go       // HTTP server, listen on :8080, route POST /rpc
├── broker.go     // Broker struct, Task struct, all channel logic
└── jsonrpc.go    // JSON-RPC 2.0 types and dispatcher
```

---

## JSON-RPC 2.0 Protocol

All requests are HTTP POST to `/rpc` with `Content-Type: application/json`.

### Request format

```json
{
  "jsonrpc": "2.0",
  "method": "<method_name>",
  "params": { ... },
  "id": 1
}
```

### Response format (success)

```json
{
  "jsonrpc": "2.0",
  "result": { ... },
  "id": 1
}
```

### Response format (error)

```json
{
  "jsonrpc": "2.0",
  "error": {
    "code": -32000,
    "message": "role \"coder\" has no listener"
  },
  "id": 1
}
```

### Standard JSON-RPC error codes to use

| Code | Meaning |
|---|---|
| -32700 | Parse error (invalid JSON) |
| -32600 | Invalid request (missing jsonrpc/method) |
| -32601 | Method not found |
| -32602 | Invalid params (missing required field) |
| -32000 | Application error (use for all business logic errors) |

---

## MCP Initialization Handshake (implement this!)

MCP clients send an initialization sequence before calling any tools. The server must handle these methods or MCP clients will refuse to connect. From a JSON-RPC perspective these are just regular methods:

### `initialize`

Request:
```json
{
  "jsonrpc": "2.0",
  "method": "initialize",
  "params": {
    "protocolVersion": "2024-11-05",
    "capabilities": {},
    "clientInfo": { "name": "claude", "version": "1.0" }
  },
  "id": 1
}
```

Response:
```json
{
  "jsonrpc": "2.0",
  "result": {
    "protocolVersion": "2024-11-05",
    "capabilities": { "tools": {} },
    "serverInfo": { "name": "agent-broker", "version": "1.0.0" }
  },
  "id": 1
}
```

### `notifications/initialized`

This is a JSON-RPC notification (no `id` field). The server must accept it and return HTTP 200 with no body (or empty response). Do not return a JSON-RPC response for notifications.

### `tools/list`

Returns the list of available tools so MCP clients know what methods exist:

```json
{
  "jsonrpc": "2.0",
  "result": {
    "tools": [
      {
        "name": "create_task",
        "description": "Create a task for an agent with the given role. Blocks until the task is solved.",
        "inputSchema": {
          "type": "object",
          "properties": {
            "role":    { "type": "string", "description": "Target agent role" },
            "task_id": { "type": "string", "description": "Unique task identifier" },
            "task_md": { "type": "string", "description": "Task description in Markdown" }
          },
          "required": ["role", "task_id", "task_md"]
        }
      },
      {
        "name": "listen_role",
        "description": "Register as a worker for the given role. Blocks until a task is assigned.",
        "inputSchema": {
          "type": "object",
          "properties": {
            "role": { "type": "string", "description": "Role this worker handles" }
          },
          "required": ["role"]
        }
      },
      {
        "name": "solve_task",
        "description": "Submit the result for a task. Unblocks the waiting create_task caller.",
        "inputSchema": {
          "type": "object",
          "properties": {
            "task_id":   { "type": "string", "description": "Task identifier to resolve" },
            "result_md": { "type": "string", "description": "Result in Markdown" }
          },
          "required": ["task_id", "result_md"]
        }
      }
    ]
  },
  "id": 1
}
```

### `tools/call`

MCP clients call tools via `tools/call`, not directly by method name:

```json
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {
    "name": "create_task",
    "arguments": {
      "role": "coder",
      "task_id": "t1",
      "task_md": "# Fix the login bug\n..."
    }
  },
  "id": 2
}
```

Response wraps the result in MCP content format:

```json
{
  "jsonrpc": "2.0",
  "result": {
    "content": [
      { "type": "text", "text": "{\"result_md\": \"# Done\\n...\"}" }
    ]
  },
  "id": 2
}
```

The dispatcher must route `tools/call` to the correct tool by `params.name`.

---

## Three Business Methods

### `create_task`

**Params:** `role` (string), `task_id` (string), `task_md` (string)

**Behavior:**
1. Acquire broker lock
2. Check `listeners[role]` exists — if not, return error `-32000`: `role "X" has no listener, ask user to clarify the role`
3. Check `tasks[task_id]` does not exist — if it does, return error `-32000`: `task "X" already exists`
4. Create `Task{ID, Role, MD, done: make(chan string, 1)}`
5. Store in `tasks[task_id]`
6. Send task to `listeners[role]` channel
7. Release lock
8. **Block** on `select { case result := <-task.done; case <-ctx.Done() }`
9. On result: return `{"result_md": result}`
10. On ctx cancel: clean up task from map, return error

**This HTTP request hangs until `solve_task` is called for this task_id.**

---

### `listen_role`

**Params:** `role` (string)

**Behavior:**
1. Acquire broker lock
2. Check `listeners[role]` does not exist — if it does, return error `-32000`: `role "X" already has a listener`
3. Create `ch := make(chan *Task, 1)`
4. Store `listeners[role] = ch`
5. Release lock
6. Defer: on return, acquire lock and `delete(listeners, role)`
7. **Block** on `select { case task := <-ch; case <-ctx.Done() }`
8. On task: return `{"task_id": task.ID, "task_md": task.MD}`
9. On ctx cancel: defer fires, listener removed, return error

**This HTTP request hangs until `create_task` sends a task for this role.**

---

### `solve_task`

**Params:** `task_id` (string), `result_md` (string)

**Behavior:**
1. Acquire broker lock
2. Look up `tasks[task_id]` — if not found, return error `-32000`: `task "X" not found`
3. Delete from `tasks` map
4. Release lock
5. Send `result_md` to `task.done` channel (non-blocking, channel is buffered size 1)
6. Return `{"ok": true}`

**This returns immediately.**

---

## Data Structures (broker.go)

```go
type Task struct {
    ID   string
    Role string
    MD   string
    done chan string // buffered size 1
}

type Broker struct {
    mu        sync.Mutex
    listeners map[string]chan *Task
    tasks     map[string]*Task
}

func NewBroker() *Broker

func (b *Broker) ListenRole(ctx context.Context, role string) (*Task, error)
func (b *Broker) CreateTask(ctx context.Context, role, taskID, taskMD string) (string, error)
func (b *Broker) SolveTask(taskID, resultMD string) error
```

---

## JSON-RPC Dispatcher (jsonrpc.go)

```go
type Request struct {
    JSONRPC string          `json:"jsonrpc"`
    Method  string          `json:"method"`
    Params  json.RawMessage `json:"params,omitempty"`
    ID      json.RawMessage `json:"id"` // can be number, string, or null
}

type Response struct {
    JSONRPC string    `json:"jsonrpc"`
    Result  any       `json:"result,omitempty"`
    Error   *RPCError `json:"error,omitempty"`
    ID      json.RawMessage `json:"id"`
}

type RPCError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}
```

Dispatcher logic in `ServeHTTP`:
1. Decode `Request` from body — on failure return parse error
2. Validate `jsonrpc == "2.0"` and `method != ""`
3. If no `id` field (notification) — handle silently, return 200 empty
4. Route by method:
   - `initialize` → return capabilities
   - `notifications/initialized` → treat as notification
   - `tools/list` → return tools schema
   - `tools/call` → extract `params.name`, dispatch to broker method
   - anything else → method not found error

---

## HTTP Server (main.go)

```go
// Listen on :8080 (or PORT env var)
// Single route: POST /rpc → handler
// Set no read timeout (requests block intentionally)
// Set write timeout to 0 (responses block intentionally)
```

Critical: `http.Server` write timeout must be **0 (disabled)**. The blocking methods hold HTTP connections open for seconds or minutes. A write timeout would kill them.

```go
server := &http.Server{
    Addr:         addr,
    Handler:      handler,
    ReadTimeout:  5 * time.Second,   // headers only
    WriteTimeout: 0,                  // disabled — blocking RPC
    IdleTimeout:  120 * time.Second,
}
```

---

## Edge Cases to Handle

| Case | What to do |
|---|---|
| `create_task` with no listener | Immediate error, do not store task |
| Duplicate `task_id` | Immediate error |
| `listen_role` when role already listening | Immediate error |
| Worker disconnects during `listen_role` | `ctx.Done()` fires, defer removes from listeners map |
| Orchestrator disconnects during `create_task` | `ctx.Done()` fires, task removed from map. If task was already delivered to worker, worker's `solve_task` will get "task not found" |
| `solve_task` for unknown `task_id` | Error |

---

## Acceptance Criteria

- [ ] `go build ./...` succeeds with zero external imports
- [ ] `go vet ./...` clean
- [ ] Two goroutines can rendezvous: one calling `listen_role`, another calling `create_task`, result flows back via `solve_task`
- [ ] MCP handshake works: `initialize` → `notifications/initialized` → `tools/list` → `tools/call`
- [ ] Worker disconnect during `listen_role` does not leak the listener entry
- [ ] Orchestrator disconnect during `create_task` does not leak the task entry
- [ ] All error responses use correct JSON-RPC error format
