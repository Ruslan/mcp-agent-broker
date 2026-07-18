# Plan 0.0.14: Mid-task steering (dispatcher → worker, in-flight)

> **Status: proposed.** Forward-looking design, not yet implemented. Builds directly on the
> `poll_url` capability + wake-on-exit machinery from [0.0.13](plan-0.0.13.md).

## Goal

Today the broker is **one-shot per task**: a dispatcher `create_task`s, a worker `listen_role`-picks
it, works, and `solve_task`s. There is no channel to nudge a worker that is **already working** — to
add a constraint, correct course, reprioritize, or abort gracefully — short of cancelling and
recreating the task, which throws away in-flight progress.

**Steering** closes that gap: let the dispatcher (or a human via the admin UI) inject guidance into an
in-flight task, and have the worker pick it up mid-work and act on it. It is the missing
**dispatcher → worker** direction, the mirror of `progress_task` (worker → dispatcher).

## Prior art / what "steer" means

"Steer" is mid-execution guidance injection: change what a running agent is doing without restarting
it. Claude Code does it by queueing a message into a running turn; multi-agent frameworks expose a
"steer channel" from an orchestrator to a sub-agent. The broker already has every primitive needed —
this is `progress_task` in reverse, delivered over the existing task `poll_url`.

## Design — reuse the existing task `poll_url` (no new scope, no new endpoint)

The key simplification: **do not add a `steer` scope or a `steer_url`.** Steer rides on the task
`poll_url` that already exists. The worker, while working, watches its OWN task's `poll_url`; a steer
lands as an event on that channel and wakes it.

1. **`steer_task(task_id, message)`** — new dispatcher/admin tool. Appends `message` to the task's
   steer channel (bounded length like `progress_task`). Valid only while the task is `picked`; a
   no-op/error once `solved`.
2. **Storage** — a `steer_messages(project_id, task_id, seq, message, created_at)` table (mirrors
   `task_progress`), cursor-based so the worker reads each steer once.
3. **Task `poll_url` extended.** `GET /poll/<task-token>` already returns task status (and `result_md`
   once solved); it additionally returns any **unread steer messages** for the task and advances the
   cursor. No new scope kind — the task scope now carries both "is it solved?" (dispatcher's use) and
   "any steers?" (worker's use).
4. **Worker gets a task `poll_url` on pickup.** Today only the dispatcher gets a task `poll_url` (from
   `create_task`). The task delivered to a worker (via `broker-poll.sh` / the `listen_role` result)
   must ALSO carry a task `poll_url`, so the worker can watch its own task for steers.
5. **`progress_task` as the ack.** When the worker consumes a steer it `progress_task`s a short
   acknowledgement ("got it — adjusting for X"), so the dispatcher sees in the progress log that the
   steer landed and was accepted.

## Worker lifecycle — exactly ONE background poller at a time

This is the important part. The worker is a two-state machine and **never runs two background pollers
concurrently** — it alternates between an *idle* role poller and a *working* task poller:

1. **Start of session → idle.** The worker is told to poll: it tool-calls `listen_role(role,
   mode="poll")`, gets a role `poll_url`, and arms `broker-poll.sh` (role poller) in the background.
   It is now idle, waiting for work. (One background poller: the role poller.)
2. **Task arrives → role poller exits.** A task is queued and picked; `broker-poll.sh` prints the task
   and **exits 0**, waking the worker.
3. **Work → swap to the task poller.** The worker takes the task details **and its task `poll_url`**.
   It does **not** re-arm the role poller. Instead it arms a **task poller for steers** on the task
   `poll_url` (a steer-flavoured `broker-poll.sh`, exit-on-event), then does the work. (Still one
   background poller — now the task poller.) While working:
   - **Steer arrives → task poller exits with the steer.** The worker incorporates it, `progress_task`s
     an ack, and **re-arms the task poller** to keep listening. (Back to work; one poller.)
4. **Solve → task poller exits terminal → back to idle.** The worker `solve_task`s. Its background task
   poller sees the task is `solved` and **exits** (terminal). Because the worker itself closed the
   task, that exit means "nothing to do" — it simply re-arms the **role poller** from step 1. (One
   poller again: the role poller.)

Net: `role poller → (task) → task poller ⟲ (steers) → (solve) → role poller`. Always exactly one
backgrounded process, so the harness never juggles two wake sources and there is no double-poll.

## The scripts

No new script *type* is required — the worker's task-side steer poller is the exit-on-event poller
(`broker-poll.sh`) pointed at a task `poll_url` instead of a role one, exiting on either a new steer
(with the steer payload) or the task turning `solved` (terminal, worker moves on). The exact
exit-code split (steer-present vs solved) is the one script detail to nail down.

## Open questions

- **Ack semantics:** is `progress_task` a sufficient ack, or does steer want an explicit
  read-cursor-commit so a steer can never be silently dropped between the poll and the worker acting?
- **Abort vs. steer:** is "stop / wind down and solve with what you have" just a steer message the
  worker chooses to honour, or a distinct signal? (Contrast the existing hard-cancel path that resets a
  task to `queued`.)
- **Admin UI:** a text box to `steer_task` a `picked` task from the dashboard, with the steer log shown
  next to the progress log.
- **Late steer:** a `steer_task` that lands after `solve_task` — reject with a clear error, or surface
  to the dispatcher as "too late"?

## Why it fits

No new transport, no new auth model, no new scope kind, no new script type: `steer_task` is a tool like
`progress_task`; steers ride the task `poll_url` that already exists; the worker's steer poller is
`broker-poll.sh` aimed at a task URL. The whole feature is an additive channel over machinery that is
already built and reviewed — and the one-poller-at-a-time lifecycle keeps the worker simple.
