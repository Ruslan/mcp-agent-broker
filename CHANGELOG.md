# Changelog

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
