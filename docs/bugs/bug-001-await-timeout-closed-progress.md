# BUG-001: `await_task` can loop forever when timeout races with solve

- **Status:** Open
- **Priority:** P1
- **Area:** `agent-broker/broker.go`
- **Found:** 2026-08-19

## Problem

The timeout branch in `Broker.AwaitTask` drains `task.progress` with a receive
that does not inspect whether the channel is closed:

```go
case msg := <-task.progress:
    progress = append(progress, msg)
```

`SolveTask` closes that channel before it signals `task.done`. If the timeout
and solve paths become ready together, the timeout branch may win. A receive
from the closed progress channel then succeeds forever with the zero value, so
the loop never reaches its `default` branch.

## Impact

The affected request hangs and continuously appends empty strings. In the worst
case this can consume memory until the broker is terminated.

## Suggested fix

Use the two-value channel receive and stop draining when `ok` is false. Prefer a
timer created with `time.NewTimer` and stopped on the non-timeout paths while
touching this code.

## Acceptance criteria

- A deterministic test closes `task.progress` while the timeout path is ready.
- `AwaitTask` returns without hanging or adding empty progress messages.
- `go test -race ./...` remains green.

