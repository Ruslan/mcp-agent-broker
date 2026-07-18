# Plan 0.0.13: Async pollers via capability `poll_url` (self-installing)

> **Status: implemented.** This document reflects the design as built. An earlier draft (see
> [Design history](#design-history-what-changed-and-why) at the bottom) used a header-carried scoped
> token behind the existing `/rpc` auth; that was reworked into the capability-URL model below after
> review. The prior art is still QuietVoice, but the token-in-header approach was replaced by
> token-in-URL per the "cheap disposable link, polled without auth" requirement.

## Goal

Let any orchestrator (Claude Code, Gemini CLI, OpenCode, Pi) run the broker **fully async** —
create/take tasks, keep working, and be **woken by the harness** the moment a task is queued for its
role or a task it dispatched is solved — **without a human poking it** and **without checking out this
repo**. The whole thing is: a **`poll_url`** handed back by the tools, a dumb `curl` script that polls
it, and an MCP-prompt installer that ships the scripts.

## Why this shape — the wakeup model the broker was missing

A harness (Claude Code, agy) re-invokes an agent when a **backgrounded child process exits**. So the
portable primitive is a **one-shot "block-until-event-then-exit" script** built on a *quick-return*
endpoint — not a server-held long-poll. The agent arms the script in the background, keeps working, and
the harness wakes it on the script's exit with the event on stdout.

QuietVoice ([github.com/ruslan/quietvoice](https://github.com/ruslan/quietvoice)) proved this with
`say → poll_base + reply_token → arm poll.sh → woken on reply`. The one divergence here: QuietVoice
carries the token in an `X-Reply-Token` **header**; the broker puts it **in the URL path**
(`/poll/<token>`) so the poller needs no header at all — the URL *is* the capability. That keeps the
script a trivial `curl "$poll_url"`.

## The design

### 1. The `poll_url` capability

The tool results hand back a full URL, `http://host:9197/poll/<token>`, with an unguessable
32-byte-hex token in the path:

- `create_task(...)` → `{ task_id, status:"queued", poll_url }` — task-scoped (dispatcher awaits it).
- `listen_role(role, mode="poll")` → `{ task?, poll_url }` — role-scoped (worker takes tasks).
- `get_task(task_id)` → `{ ..., poll_url }` when not yet solved — a fresh task-scoped URL for re-arming.

The base is `BROKER_PUBLIC_URL` if set, else derived per-request from `X-Forwarded-Proto`/
`X-Forwarded-Host` (behind a proxy) or the request `Host` — so a remote agent gets a reachable host.
(`agent-broker/pollhandler.go` `pollBaseURL` / `buildPollURL`; wired into the tool results in
`agent-broker/jsonrpc.go`.)

### 2. The `GET /poll/{token}` endpoint — unauthenticated by construction

`AuthMiddleware` gates everything with the master `API_KEY` **except** paths under `/poll/`, which it
lets through untouched (`agent-broker/main.go`). The token in the path is the authorization — mirroring
QuietVoice, whose reply feed simply isn't behind the command-channel credential. So
`curl "$poll_url"` with no `Authorization` header works; `/rpc` and `/admin/*` still require the key.

Handler behavior (`agent-broker/pollhandler.go`):
- Resolve + **renew** the token (see §3). Two distinct not-live outcomes:
  - **unknown token** (never existed, or aged past its cap and was pruned) → bare **404** ("404 page
    not found"), as if the URL never existed — probing random tokens reveals nothing about `/poll`.
  - **expired token** (existed, slid out of its window or hit the 24h cap) → **200**
    `{"status":"expired","advice":...}` so a legitimate poller knows to re-mint.
- **role** scope → `ListenRole(project, role, "poll", 0)` → `{"task":{...}}` (picks it) or
  `{"task":null,"status":"empty"}`.
- **task** scope → status, plus `result_md` once `solved`.

Distinguishing the two requires keeping a recently-expired row alive: pruning is by the **hard cap**
(`created_at + maxLifetime`), not the sliding expiry, so an expired-but-within-24h token still answers
"expired" rather than 404. The poller scripts treat both 404 and `{"status":"expired"}` the same way —
exit **5**, go fetch a fresh poll_url — so the client behavior is uniform; the split is purely to hide
the endpoint from scanners.

### 3. Sliding token TTL + hard cap (the leak bound)

Tokens are minted with a **30-minute sliding TTL** and a **24-hour absolute cap** from creation
(`pollTokenTTL`, `pollTokenMaxLifetime` in `agent-broker/broker.go`). Every successful poll RENEWS the
token — `expires_at = min(now+30m, created_at+24h)` (`SQLiteStore.RenewPollToken` in
`agent-broker/store.go`). Consequences:
- An actively-polling script keeps its token alive, up to the 24h ceiling.
- A stalled poller's token dies within 30 min → the next poll returns `expired`.
- A leaked `poll_url` can't be read "a day later": ≤30 min after the legit poller stops, ≤24h always.

On expiry the script exits **5**; the agent calls `listen_role` (worker) or `get_task` (dispatcher)
again for a fresh `poll_url`, then relaunches. Storage: the existing
`poll_tokens(token, project_id, scope_kind, scope_value, created_at, expires_at)` table; rows are
pruned on insert and dropped when they hit the hard cap.

### 4. Three versioned poller scripts (`go:embed`-ed)

All pure `bash`+`curl`+`jq`, taking a `poll_url` as `$1` or `BROKER_POLL_URL`. A
`BROKER_SKILL_VERSION=<n>` marker near the top; numeric env (`BROKER_INTERVAL`/`MAX_FAIL`/`MAX_WAIT`)
validated as non-negative integers.

- **`broker-poll.sh` — worker, wake-on-exit.** Polls a role `poll_url` until a task is picked; prints
  it, exit 0.
- **`await-poll.sh` — dispatcher, wake-on-exit.** Polls a task `poll_url` until `solved`; prints the
  result, exit 0.
- **`broker-monitor.sh` — streaming.** Never exits; emits each new event as a JSONL line (de-duped) for
  a Monitor-style tool. Exits 0 on a solved task (terminal).

Exit codes: 0 event / 3 unreachable / 4 `MAX_WAIT` elapsed / 5 token expired / 64 usage / 69 missing
`curl`/`jq`. Files under `agent-broker/skillfiles/`, embedded via `agent-broker/skillfiles_embed.go`
(`//go:embed`); `make sync-skillfiles` regenerates the `.claude/skills/broker-async-poll/` install copy;
a parity test + a version-marker test keep canon, copy, and marker in lockstep.

### 5. `skill-install` MCP prompt

The broker serves MCP prompts (`prompts/list` / `prompts/get`). `skill-install` is rendered from the
embedded `skillfiles/*` at request time (`buildSkillInstallPrompt` in `skillfiles_embed.go`), printing
the four files verbatim with "save byte-for-byte" instructions, a `curl`+`jq` dependency check, and the
version marker so an already-installed skill is only updated when newer. Any MCP client pulls it via the
existing `prompts/get` surface — no new method, no local clone.

## Files (as implemented)

- **New** `agent-broker/skillfiles/{broker-poll.sh,await-poll.sh,broker-monitor.sh,SKILL.md}`.
- **New** `agent-broker/skillfiles_embed.go` — `//go:embed skillfiles/*`, `BrokerSkillVersion`,
  `skill-install` body assembly.
- **New** `agent-broker/pollhandler.go` — `GET /poll/{token}`, base-URL derivation, `poll_url` builder.
- `agent-broker/jsonrpc.go` — `poll_url` in `create_task`/`listen_role`/`get_task`; thread the base URL.
- `agent-broker/broker.go` — `MintPollToken` / `RenewPollToken`, TTL + cap constants, `PublicURL`.
- `agent-broker/store.go` — `poll_tokens` table, insert/lookup/get-active/**renew**, `created_at`.
- `agent-broker/main.go` — register `GET /poll/{token}`; `/poll/` exempt in `AuthMiddleware`;
  `BROKER_PUBLIC_URL`.
- `Makefile` — `sync-skillfiles`. `README.md`, `CHANGELOG.md` — the async-poller flow + env vars.
- Tests: `skillfiles_test.go` (parity/version/prompt), `polltoken_test.go` (poll_url mint, endpoint
  flow, renewal + hard cap, unauthenticated `/poll` vs gated `/rpc`).

## The loop (un-poked)

**Worker:** `prompts/get skill-install` once → `listen_role(role, mode="poll")` → arm
`broker-poll.sh "$poll_url"` in the background, keep working → harness wakes on exit owning a task →
`solve_task` → relaunch. On exit 5, re-`listen_role` for a fresh url.

**Dispatcher:** `create_task(...)` → arm `await-poll.sh "$poll_url"` per task, keep dispatching →
harness wakes per solve with the result. On exit 5, `get_task` for a fresh url.

## Honest caveats

- **Very real for Claude Code and agy** (both wake on background-process exit). **Codex** has no
  background-completion wakeup, so it runs the poll script **blocking in the foreground** with
  `BROKER_MAX_WAIT` bounding each turn (hypothesis, not field-verified).
- **`listen_role(mode="poll")` picks the task on read** — correct for "take a task" (wakeup and
  assignment are the same event). A pure peek-without-claim mode is not needed for the core loop.
- **Token-in-URL is a capability URL.** It leaks more readily than a header (proxy/access logs, `ps`
  via `argv`) — hence the 30-min sliding TTL / 24h cap, and the scripts accept the URL via
  `BROKER_POLL_URL` to keep it off `argv`. It is a capability, not encryption: serve a public broker
  over TLS.
- **This does not replace `mode="wait"`/`await_task`** — those stay for synchronous callers.

## Design history — what changed and why

The first draft (and a first implementation) carried a **scoped token in an `X-Broker-Token` header**,
accepted inside the existing `/rpc` auth, gated by `BROKER_REQUIRE_POLL_TOKEN`, with header-based
duplicate-poller detection. Review found it over-built for the goal, and a real bug: since
`AuthMiddleware` wrapped the whole mux, a scoped token also authenticated `/admin/*`. It was reworked
into the capability-URL model above:

- **Dropped:** the `X-Broker-Token` header path, scoped-principal machinery, `authorizeScopedRequest`,
  the `BROKER_REQUIRE_POLL_TOKEN` flag, per-script curl `--config` secret files, and duplicate-poller
  detection (`pollerreg.go`) — it can't be done without headers and wasn't worth re-adding.
- **Kept:** the `poll_tokens` table and minting. **Added:** the sliding-TTL renewal, the 24h hard cap,
  and the unauthenticated `GET /poll/{token}` endpoint. `AuthMiddleware` returned to its simple
  master-key form plus a `/poll/` exemption.
