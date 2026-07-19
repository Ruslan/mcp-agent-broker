# Plan 0.0.14: Mid-task steering (dispatcher → worker, in-flight)

> **Status: proposed.** Forward-looking design, not yet implemented. Builds directly on the
> `poll_url` capability + wake-on-exit machinery from [0.0.13](plan-0.0.13.md).
>
> **Scope note:** this version is not complete until the broker channel **and** its delivery into the
> Pi `broker-queue` extension both ship — see [Client integration](#client-integration--the-pi-broker-queue-extension-required).

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
5. **Two-layer ack — this is settled architecture, not an option.**
   - **Delivery ack (automatic, client-side, no worker involvement).** The moment the client injects a
     steer into the worker's turn, it **commits the read cursor** and `progress_task`s a terse
     "steer delivered" line. This is what makes a steer impossible to silently drop: the cursor moves
     on *delivery*, not on the worker's reply, so an ignored steer still can't loop or vanish.
   - **Semantic ack (the worker's reply, optional but expected).** The worker answers with a
     `<progress task_id="...">` tag (see §client-integration) reporting where it is and how it is
     adapting; the client parses it → `progress_task`. This is what tells the dispatcher the steer was
     *understood and acted on*, not merely received.

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

## Client integration — the Pi `broker-queue` extension (required)

Steer is **not done** until the reference client speaks it. The Pi extension
(`extensions/pi/broker-queue/`) is the canonical consumer and **must** land steer support as part of
this version — a broker-side channel with no client to deliver it into a running turn is dead weight.

Today that extension is a **long-poll** role loop, not a wake-on-exit poller: `/c1`/`/r1` start a
`waitLoop` (`mode="wait"`), `activeTask` holds the picked task, and a `message_end` hook detects the
worker's `<solution task_id="...">` block → `solve_task` → resumes the loop. Steer maps cleanly onto
that shape, because Pi already has the two things it needs: a running **turn** to inject into and a
first-class `activeTask` to hang a watcher off.

**The worker here is always a subagent, never a main agent.** The Pi `broker-queue` extension drives
subagent sessions only, so a steer is a message injected into the **subagent's** turn — which owns
exactly one `activeTask`. All the tag traffic below (`<steer>` in, `<progress>`/`<solution>` out)
lives inside that single subagent session; the main agent is not involved.

### The steer wire protocol — two tags, strictly agreed

Steering rides two XML tags, mirroring the existing `<solution task_id="...">` convention (parsed in
`index.ts` via regex in the `message_end` hook). They are **not** interchangeable:

- **`<steer task_id="...">` — inbound (dispatcher → worker).** The client wraps the dispatcher's raw
  steer message in this tag and injects it into the subagent turn, together with a short instruction so
  even a weak model emits the reply tag. Exact injected form:

  ```
  You received a steer for your current task.

  <steer task_id="T123">все ли хорошо? На какой ты стадии?</steer>

  Acknowledge and adapt — reply with:
  <progress task_id="T123">where you are + how you're adjusting</progress>
  ```

- **`<progress task_id="...">` — outbound (worker → dispatcher).** The worker's reply. The
  `message_end` hook parses it (same machinery as `<solution>`) → `progress_task`. This is the
  **semantic ack**. It is a *new* tag: today the extension emits only `<solution>`, never `<progress>`.

**Cursor commit is on delivery, not on `<progress>`.** When the client injects the `<steer>` block it
immediately advances the read cursor and fires the automatic **delivery ack** (`progress_task`
"steer delivered"). The worker's `<progress>` reply is the content ack layered on top. A worker that
ignores the steer therefore still cannot cause redelivery — the steer is consumed exactly once at
injection time.

What the extension must add:

1. **A task-side steer watcher, alive only while `activeTask` is set.** On task pickup (in
   `startRoleLoop`, where `activeTask` is assigned), start polling the task's steer channel —
   `get_task`/the task `poll_url` now returns unread steers (§design). Tear it down on `solve_task`
   and on `cstop`, so it obeys the same one-watcher-at-a-time discipline as `waitLoop` (reuse the
   `waitAbort`/`stopRequested` machinery, don't add a parallel lifecycle).
2. **On each steer: inject the `<steer>` block into the subagent turn** (Pi's analogue of "queue a
   message into the turn" — push into the active session, not just `ctx.ui.notify`, mirroring how
   `publishTask`/`cbump` inject the task prompt), **then commit the cursor + fire the delivery ack**
   in the same step.
3. **Parse the worker's `<progress task_id="...">` in `message_end`** (alongside the existing
   `<solution>` parse) → `progress_task` as the semantic ack.
4. **A `/steer <task_id> <message>` command (dispatcher side).** So the same extension can *send* a
   steer to a `picked` task from a dispatcher Pi session, not only receive one — calls `steer_task`.
5. **README + instructions update.** Document the steer command, the `<steer>`/`<progress>` protocol,
   and the mid-task injection behaviour in `extensions/pi/broker-queue/README.md` and the
   `instructions-*.md` files (tell the worker steers may arrive mid-task and must be honoured, and that
   it must reply with `<progress task_id="...">`).

Guardrail: the steer watcher must **never** run concurrently with the role `waitLoop` — `activeTask`
set ⇒ steer watcher, `activeTask` null ⇒ role wait. This is the same "exactly one background poller"
invariant as §worker-lifecycle, enforced client-side.

## Settled decisions

- **Ack semantics — decided.** Two layers: an automatic **delivery ack** with the read cursor committed
  at injection time (so a steer is consumed exactly once and can never be silently dropped), plus the
  worker's `<progress task_id="...">` reply as the **semantic ack**. See §two-layer-ack and
  §steer-wire-protocol.
- **Wire format — decided.** `<steer task_id="...">` inbound, `<progress task_id="...">` outbound;
  both parsed by the Pi extension's `message_end` hook, same machinery as `<solution>`.
- **Host — decided.** The worker is always a **subagent** in the Pi `broker-queue` extension; steers
  inject into that single subagent session.
- **Abort vs. steer — decided.** There is **no distinct abort signal and no new task state.** "Stop /
  wind down and solve with what you have" is just an ordinary steer message the worker chooses to
  honour → graceful `solve_task` with the partial result. The two ends of the spectrum are already
  covered and stay the only enforced mechanisms: cooperative-soft = a steer; hard = the existing
  hard-cancel (resets the task to `queued`, discarding in-flight work). We deliberately accept that a
  steer-based abort is **not guaranteed** — a hung or looping worker may never honour it — and that the
  dispatcher can't programmatically distinguish an abort steer from any other; hard-cancel is the
  backstop for the non-cooperative case. No `abort=true` flag, no `abort_task` tool.
- **Late steer — decided.** Two distinct races, handled so **no steer is ever silently lost**:
  - **A — `steer_task` called after the task is already `solved`** (terminal): the tool **rejects with
    a clear error**, e.g. `"too late: task already solved"`. Consistent with §design ("valid only while
    `picked`; a no-op/error once `solved`").
  - **B — steers accepted while `picked` but still unread when the worker `solve_task`s** (delivered to
    the channel, never consumed): `solve_task` counts the undelivered steers and **surfaces them back
    to the dispatcher** in its result, e.g. `{ solved:true, undelivered_steers:[...] }`. The dispatcher
    then knows its guidance never landed and can follow up or re-create. The cost is a little extra
    logic in `solve_task` (read the steer cursor vs. tail at solve time); accepted for the guarantee.
- **Steer is async-only — decided.** Steering lives entirely in the broker's **async flow** (the 0.0.13
  `poll_url` / background-monitor model); the older **sync flow is deprecating** and steer does not
  extend it. Concretely: `steer_task` is **fire-and-forget** — the dispatcher does **not** block on it.
  It catches the worker's `<progress>` reply the same way it already watches the task: from a
  **background progress monitor** on the task `poll_url` (`broker-monitor.sh`). A harness with **no
  background wakeup** falls back to putting an **`await_task` / `await-poll.sh`** on the same task right
  after the steer and catching `progress` events blockingly — the existing await primitive, not a new
  one. There is deliberately **no synchronous "block until this specific steer is acknowledged"**
  operation; that is the dying sync model and we do not add it.
- **No reply-to correlation — decided.** Steer and progress are **two independent message streams** on
  the task, correlated only by `task_id` + time (a chat log), **not** by a per-steer id. The tags carry
  `task_id` only — there is deliberately **no `steer_id`/`steer_seq` in `<steer>` or `<progress>`**, so
  a `<progress>` reply answers "the task," not "steer #N." This is sound because correctness rides on
  the automatic **delivery-ack + cursor**, not on the worker's reply; the semantic ack is a best-effort
  narrative, and a worker may answer several steers in one `<progress>` or none. Per-steer correlation
  would only be needed for the block-until-acknowledged pattern above, which is explicitly out of scope.
- **Admin UI — decided (shape).** No always-on second pane. The steer log is revealed **on click inside
  the task card** (alongside the likewise click-revealed progress log), keeping the task list
  uncluttered — steers are a rare per-task detail, not a first-class column. A text box on the expanded
  card `steer_task`s a `picked` task.

## Open questions

*(none blocking — the design is fully specified above.)*

## Why it fits

No new transport, no new auth model, no new scope kind, no new script type: `steer_task` is a tool like
`progress_task`; steers ride the task `poll_url` that already exists; the worker's steer poller is
`broker-poll.sh` aimed at a task URL. The whole feature is an additive channel over machinery that is
already built and reviewed — and the one-poller-at-a-time lifecycle keeps the worker simple.
