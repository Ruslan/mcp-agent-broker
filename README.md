# Agent Broker

Minimal Go MCP/JSON-RPC broker for role-based task handoff between agents.

## Run

Build the server:

```bash
cd agent-broker
go build -o ../broker .
```

Start the server:

```bash
./broker
```

Default port: `9197`

Custom port:

```bash
PORT=8080 ./broker
```

## MCP Server URL

Default MCP JSON-RPC endpoint:

```text
http://localhost:9197/rpc
```

If you set `PORT`, use:

```text
http://localhost:<PORT>/rpc
```

Recommended MCP server name:

```text
agent-broker
```

## MCP Client Config

### Gemini

File: `./.gemini/settings.json`

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

File: `./.opencode/opencode.json`

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

### Claude

For `Claude Code`, add the server with:

```bash
claude mcp add --transport http agent-broker http://localhost:9197/rpc
```

If your Claude client uses JSON config with `mcpServers`, use:

```json
{
  "mcpServers": {
    "agent-broker": {
      "url": "http://localhost:9197/rpc"
    }
  }
}
```

## Protocol

The server exposes JSON-RPC 2.0 over HTTP and is intended to work as an MCP tool server.

Available tools:

1. `create_task_sync`
2. `create_task_async`
3. `listen_role_sync`
4. `listen_role_async`
5. `solve_task`

## Sync Mode

Use sync mode when the caller is allowed to block until the worker finishes.

Flow:

1. Worker calls `listen_role_sync`
2. Orchestrator calls `create_task_sync`
3. Worker receives the task
4. Worker calls `solve_task`
5. Original `create_task_sync` call returns the finished result

### 1. Worker waits for a sync task

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "listen_role_sync",
      "arguments": {
        "role": "coder"
      }
    },
    "id": 1
  }'
```

### 2. Orchestrator sends a sync task

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "create_task_sync",
      "arguments": {
        "role": "coder",
        "task_id": "sync-1",
        "task_md": "Implement the requested change"
      }
    },
    "id": 2
  }'
```

This request may stay open for a long time until the worker finishes the task.

### 3. Worker posts the result

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "solve_task",
      "arguments": {
        "task_id": "sync-1",
        "result_md": "Done"
      }
    },
    "id": 3
  }'
```

## Async Mode

Use async mode when you want inbox-style delivery and immediate responses.

Flow:

1. Orchestrator calls `create_task_async`
2. Server queues the task and returns immediately
3. Worker polls with `listen_role_async`
4. Worker receives one queued task or `no_task`
5. Worker calls `solve_task`

In `v0.0.1`, async mode is fire-and-forget for the original sender. The sender gets `queued`, not the final result.

### 1. Orchestrator queues an async task

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "create_task_async",
      "arguments": {
        "role": "coder",
        "task_id": "async-1",
        "task_md": "Review the attached plan"
      }
    },
    "id": 4
  }'
```

### 2. Worker polls for one async task

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "listen_role_async",
      "arguments": {
        "role": "coder"
      }
    },
    "id": 5
  }'
```

If a task exists, the response contains `found: true`, `task_id`, and `task_md`.

If no task exists, the response contains:

```json
{
  "found": false,
  "status": "no_task"
}
```

### 3. Worker posts the async result

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "name": "solve_task",
      "arguments": {
        "task_id": "async-1",
        "result_md": "Done"
      }
    },
    "id": 6
  }'
```

## MCP Handshake

If your client expects MCP initialization, call:

1. `initialize`
2. `notifications/initialized`
3. `tools/list`
4. `tools/call`

Example initialize request:

```bash
curl -s -X POST http://localhost:9197/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "initialize",
    "params": {
      "protocolVersion": "2024-11-05",
      "capabilities": {},
      "clientInfo": {
        "name": "test-client",
        "version": "1.0"
      }
    },
    "id": 1
  }'
```

## Notes

1. `sync` mode is blocking by design.
2. `async` mode is in-memory only.
3. In `v0.0.1`, async tasks have no recovery, lease, or status API.
