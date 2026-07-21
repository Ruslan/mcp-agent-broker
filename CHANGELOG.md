# Changelog

## 2026-07-21

- `poll_url` now honors `X-Forwarded-Proto` **by default** (no `BROKER_TRUST_FORWARDED` needed), so a
  broker behind a TLS-terminating proxy (Caddy/nginx) emits `https://` URLs automatically — no
  cleartext hop that would expose the capability token, and no hardcoded public domain. Only
  `X-Forwarded-Host` stays gated behind `BROKER_TRUST_FORWARDED`, since a forged host (unlike a forged
  scheme) would redirect a live token to another host.
- Poller scripts (`BROKER_SKILL_VERSION` 4 → 5): `curl -sL` so a stray proxy redirect still lands
  (defense in depth), and SKILL.md gains a strict capability-URL policy — poll the exact `poll_url`
  verbatim, never rewrite the host to `localhost` or scan the local system.
- Added a plain-HTTP skill installer at `GET /skill/install` for harnesses that can't pull MCP prompts.
  It returns the same body as the `skill-install` prompt as `text/markdown` with **no API key** (exempt
  like `/poll/`), plus an `X-Broker-Skill-Version` header — so `wget http://host:9197/skill/install`
  hands any agent the embedded, self-contained installer.

## 2026-07-18

- Added a `poll_url` capability URL for fully-async orchestration. `create_task`,
  `listen_role(mode="poll")`, and `get_task` return a `poll_url` (`GET /poll/<token>`, token in the
  path) that is served **without any API key** — a background script just `curl "$poll_url"`. A role
  URL picks a queued task; a task URL reports status and the result once solved.
- Poll tokens have a **sliding 30-minute TTL** (each poll renews) and a **hard 24-hour cap**, so a
  leaked `poll_url` can't be read a day later and a stalled poller's token dies within 30 minutes. An
  expired token returns `200 {"status":"expired"}`; an unknown token returns a bare `404` (as if the
  URL never existed) so probing reveals nothing. The client re-fetches a fresh `poll_url` in both cases.
- Added self-installing poller scripts (`broker-poll.sh`, `await-poll.sh`, `broker-monitor.sh`),
  embedded in the binary and installable over MCP via the `skill-install` prompt.
- Added `BROKER_PUBLIC_URL` to override the base used when building `poll_url` (default: derived from
  `X-Forwarded-*` / request `Host`).

## 2026-06-26

- Fixed MCP JSON-RPC compatibility with IDE and harness clients that expect notifications to return `202 Accepted` with an empty body.

## 2026-06-14

- Made closed tag optional for weaker models.

## 2026-05-25

- Improved broker queue recovery and cancellation.
- Added broker integration examples and Pi extension docs.
- Resolved UI dependency audit warnings.

## 2026-05-23

- Made `listen_role` delivery resilient.
- Tracked result views.

## 2026-05-12

- Enforced the 20-task list limit.
- Added URL-based project persistence.
- Clarified task completion requirements.

## 2026-05-09

- Implemented paginated task listing.
- Added admin status updates.
- Made task resolution idempotent.

## 2026-05-01

- Restored task state on startup.
- Added detailed logging for agent operations.

## 0.0.12

- Added prompts management dashboard and admin API endpoints.
- Added task progress-log persistence.
- Added admin task deletion API and UI improvements.

## 0.0.11

- Implemented the Svelte admin UI.
- Set the default database path to `data/broker.db`.

## 0.0.10

- Implemented SQLite persistence.
- Added scaffolding for the admin dashboard UI.
