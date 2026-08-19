#!/usr/bin/env bash
# await-poll.sh <poll_url> — DISPATCHER side. Block until the task you dispatched
# is solved, print its result, exit 0. The mirror of broker-poll.sh for the
# orchestrator that CREATED a task and wants to be woken when it is answered,
# instead of blocking inside await_task.
#
# The poll_url is a CAPABILITY URL — the unguessable token is IN the path, so
# this is just `curl "$url"` with NO auth header. Get it from create_task's
# "poll_url" result field (get_task also returns a fresh one).
#
# Usage:   bash await-poll.sh <poll_url>
#          BROKER_POLL_URL=<poll_url> bash await-poll.sh   # url via env (off argv)
#
# Env:
#   BROKER_POLL_URL  the poll_url, if not passed as $1.
#   BROKER_INTERVAL  seconds between polls, positive integer (default 5). Each
#                    poll RENEWS the token server-side.
#   BROKER_MAX_FAIL  consecutive unreachable/garbled polls before giving up (default 12).
#   BROKER_MAX_WAIT  max seconds to block on an UNSOLVED task before exiting 4
#                    (default 0 = unbounded).
#
# Behavior: ONE-SHOT waiter. Re-GETs the poll_url every BROKER_INTERVAL seconds
# until status=="solved", prints {"task_id","status","result_md"}, exits 0. The
# task endpoint is read-only (bar a one-time result-view counter on solve).
#
# Exit codes (the harness wakes the agent on exit):
#   0   the task is solved — one JSON object on stdout with its result_md
#   3   the broker was unreachable / errored (e.g. task deleted) too long
#   4   BROKER_MAX_WAIT elapsed while still unsolved — relaunch to keep waiting
#   5   the poll token EXPIRED — call get_task again for a fresh poll_url, then relaunch
#   64  usage error (no poll_url, or a non-integer numeric env var)
#   69  a required command (curl or jq) is missing
#
# BROKER_SKILL_VERSION=6
set -u

url="${1:-${BROKER_POLL_URL:-}}"
if [ -z "$url" ]; then
  echo "usage: await-poll.sh <poll_url>   (or set BROKER_POLL_URL)" >&2
  exit 64
fi

for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "await-poll.sh: missing required command: $bin" >&2; exit 69; }
done

interval="${BROKER_INTERVAL:-5}"
max_fail="${BROKER_MAX_FAIL:-12}"
max_wait="${BROKER_MAX_WAIT:-0}"
fails=0
waited=0

is_uint() { case "$1" in 0) return 0 ;; '' | *[!0-9]* | 0*) return 1 ;; *) return 0 ;; esac; }
for pair in "BROKER_INTERVAL=$interval" "BROKER_MAX_FAIL=$max_fail" "BROKER_MAX_WAIT=$max_wait"; do
  is_uint "${pair#*=}" || { echo "await-poll.sh: ${pair%%=*} must be a non-negative integer (no leading zeros), got: ${pair#*=}" >&2; exit 64; }
done
[ "$interval" -ge 1 ] || { echo "await-poll.sh: BROKER_INTERVAL must be >= 1" >&2; exit 64; }
[ "$max_fail" -ge 1 ] || { echo "await-poll.sh: BROKER_MAX_FAIL must be >= 1" >&2; exit 64; }

bail_if_stuck() {
  fails=$((fails + 1))
  if [ "$fails" -ge "$max_fail" ]; then
    echo "await-poll.sh: broker unreachable or unresponsive after $fails tries" >&2
    exit 3
  fi
}

while :; do
  # -L follows a proxy redirect (e.g. an http->https bounce) so the poll lands.
  resp=$(curl -sL -w '\n%{http_code}' "$url")
  if [ $? -ne 0 ]; then
    bail_if_stuck; sleep "$interval"; continue
  fi
  code=${resp##*$'\n'}
  resp=${resp%$'\n'*}
  case "$code" in
    2*) : ;;
    404) echo "await-poll.sh: poll_url not found (expired or revoked) — call get_task again for a fresh poll_url" >&2; exit 5 ;;
    *) bail_if_stuck; sleep "$interval"; continue ;;
  esac

  # A 2xx with an unparseable body is a wedged broker — trip the failure guard.
  if ! printf '%s' "$resp" | jq -e . >/dev/null 2>&1; then
    bail_if_stuck; sleep "$interval"; continue
  fi

  status=$(printf '%s' "$resp" | jq -r '.status // empty' 2>/dev/null)

  if [ "$status" = "expired" ]; then
    echo "await-poll.sh: poll token expired — call get_task again for a fresh poll_url" >&2
    exit 5
  fi
  if [ "$status" = "error" ]; then
    emsg=$(printf '%s' "$resp" | jq -r '.error // "unknown error"' 2>/dev/null)
    echo "await-poll.sh: broker returned error: $emsg" >&2
    exit 3
  fi

  if [ "$status" = "solved" ]; then
    printf '%s' "$resp" | jq -c '{task_id, status, result_md}'
    exit 0
  fi

  # Not solved yet (queued/picked) — a legitimate wait.
  fails=0
  if [ "$max_wait" -gt 0 ] && [ "$waited" -ge "$max_wait" ]; then
    echo "await-poll.sh: task still unsolved after BROKER_MAX_WAIT=${max_wait}s — relaunch to keep waiting" >&2
    exit 4
  fi
  sleep "$interval"
  waited=$((waited + interval))
done
