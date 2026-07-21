---
name: broker-async-poll
description: >
  Run the agent-broker fully async — take tasks for a role, or await answers to tasks you
  dispatched, WITHOUT blocking a turn and without a human poking the agent. create_task / listen_role
  hand back a poll_url (a capability URL with the token in the path); arm ONE background poller on it
  and keep working. The harness wakes the agent when the poller script exits (a task was picked, or a
  dispatched task was solved). This is the wake-on-process-exit complement to the blocking
  listen_role(mode="wait") / await_task tools.
---

# broker-async-poll — background task-tracking for the agent-broker

## The core idea: a poll_url capability
The broker's tool results hand back a **`poll_url`** — a URL like `http://host:9197/poll/<token>` with
an unguessable token **in the path**. That URL *is* the authorization: you `curl "$poll_url"` with **no
header, no API key**. It's a cheap, disposable capability — hand it to a dumb background script.

- `create_task(...)` → `{ task_id, status:"queued", poll_url }` — the poll_url tracks THAT task.
- `listen_role(role, mode="poll")` → `{ task?, poll_url }` — the poll_url takes tasks for THAT role.
- `get_task(task_id)` → also returns a fresh `poll_url` (so a dispatcher can re-arm after expiry).

## Strict capability-URL policy
Poll the **exact** `poll_url` the broker returned, verbatim — host, scheme, and token as given. Do
**not** rewrite its host to `localhost`, hunt for a local broker process, or scan the local system when
a poll fails: the token is scoped to that one URL, and a network error just means "retry the same URL"
(or the token expired → fetch a fresh `poll_url` from `listen_role` / `get_task`). The `poll_url` is the
only broker endpoint the poller ever needs.

## The scripts
Three tiny `curl`+`jq` scripts, each taking a poll_url (as `$1` or `BROKER_POLL_URL`):

- **`broker-poll.sh <poll_url>` — worker, wake-on-exit.** Blocks until a task is queued for the role,
  **picks** it, prints it, exits 0. The wakeup and the assignment are the same event.
- **`await-poll.sh <poll_url>` — dispatcher, wake-on-exit.** Blocks until the task is `solved`, prints
  the result, exits 0. Read-only (bar a one-time view counter on solve).
- **`broker-monitor.sh <poll_url>` — streaming.** Never exits; prints each new event as a JSONL line
  for a Monitor-style tool. Use ONLY with a streaming tool, never `run_in_background`.

## Contract — worker (wake-on-exit)
1. `listen_role(role="coder", mode="poll")` → capture `poll_url`.
2. Arm **one** `broker-poll.sh` in the **background** (Claude Code: Bash tool `run_in_background`):
   ```
   BROKER_POLL_URL=<poll_url> bash .claude/skills/broker-async-poll/broker-poll.sh
   ```
   (or pass the url as `$1`). Then **keep working / go idle**.
3. The harness wakes you when the script exits 0 with one JSON line
   `{"task_id":...,"title":...,"task_md":...}` — you already **own** the task. Do the work,
   `solve_task(task_id, result_md)`, then **relaunch** the script. No human in the loop.

## Contract — dispatcher (wake-on-exit)
1. `create_task(role="coder", title=..., task_md=...)` → `{ task_id, poll_url }`.
2. Arm **one** `await-poll.sh` per outstanding task in the background:
   ```
   BROKER_POLL_URL=<poll_url> bash .claude/skills/broker-async-poll/await-poll.sh
   ```
   Keep dispatching more tasks (each gets its own waiter).
3. The harness wakes you per solve with `{"task_id":...,"status":"solved","result_md":...}` — review,
   dispatch follow-ups, done.

## Token expiry — the "go get a new one" rule
The token in a poll_url has a **sliding ~30-minute TTL**: every poll RENEWS it, so an actively-polling
script keeps it alive — up to a **hard 24-hour cap** from creation. This bounds leaks: a poll_url that
lands in a log can't be read from "a day later", and a poller that stalls for >30 min lets its token die.

When the token expires (script exits **5**, or a poll returns `{"status":"expired"}`), you **go get a
fresh poll_url**:
- worker → call `listen_role(role, mode="poll")` again for a new poll_url, then relaunch.
- dispatcher → call `get_task(task_id)` again for a new poll_url, then relaunch.

## Exit codes (broker-poll.sh / await-poll.sh)
- **0** — event fired (task picked / task solved), JSONL on stdout. Act, then relaunch.
- **3** — broker unreachable / garbled / errored too long.
- **4** — `BROKER_MAX_WAIT` elapsed with no event (opt-in; default unbounded). Relaunch to keep waiting.
- **5** — poll token expired → fetch a fresh poll_url (listen_role / get_task), then relaunch.
- **64** — usage error (no poll_url, or a bad numeric env var).
- **69** — missing `curl` or `jq`.

## Env
- `BROKER_POLL_URL` — the poll_url, if not passed as `$1`. Passing via env keeps the token off `argv`
  (out of `ps` / `/proc`), which is a little safer for a long-running poller.
- `BROKER_INTERVAL` — seconds between polls (default 5). Each poll renews the token.
- `BROKER_MAX_FAIL` — consecutive unreachable/garbled polls before giving up (default 12 ≈ 60s).
- `BROKER_MAX_WAIT` — max seconds to block with no event before exit 4 (default 0 = unbounded).

## Requirements
`bash`, `curl`, and `jq` on PATH (`command -v curl jq`). Nothing else — no API key, no headers. The
poll_url already carries everything (host + capability token). Treat it like a secret (it grants the
scoped poll for its lifetime); it is a capability, not encryption — a public broker should serve it
over TLS.

## Runtime compatibility
- **Claude Code / agy — wake-on-exit.** Arm `broker-poll.sh` / `await-poll.sh` with the Bash tool's
  `run_in_background`; the harness re-invokes you when it exits. Relaunch on each wake.
- **Claude Code — streaming.** Run `broker-monitor.sh` under the Monitor tool (persistent) for an
  inline event stream; do NOT `run_in_background` it (it never exits).
- **Codex — HYPOTHESIS (untested).** Run `broker-poll.sh` **blocking in the foreground** with
  `BROKER_MAX_WAIT=<seconds>` bounding each turn.

## Installing this skill into another project
Any MCP client talking to the same broker can pull these scripts via the `skill-install` prompt
(`prompts/get skill-install`), which prints `broker-poll.sh` / `await-poll.sh` / `broker-monitor.sh` /
this `SKILL.md` verbatim (embedded in the broker binary) plus install instructions and a `curl`+`jq`
dependency check. Each script carries a `BROKER_SKILL_VERSION=<n>` marker; `skill-install` states its
version so an already-installed skill is only updated when the offered version is newer.

## Canonical source — read this before editing
**This is the canonical source.** This file and its siblings under `agent-broker/skillfiles/` are what
a developer edits and what `//go:embed` ships inside the broker binary. The copy at
`.claude/skills/broker-async-poll/` is a **generated install copy** (`make sync-skillfiles`) — never
edit it directly. When you change a script, edit it here, bump `BrokerSkillVersion` (+1) in
`agent-broker/skillfiles_embed.go`, update the `BROKER_SKILL_VERSION=<n>` marker in every script to
match, then run `make sync-skillfiles`. The parity + version tests keep canon, install copy, and marker
in lockstep.
