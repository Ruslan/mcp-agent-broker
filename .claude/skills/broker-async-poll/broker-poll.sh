#!/usr/bin/env bash
# broker-poll.sh <poll_url> — WORKER side. Block until a task is queued for the
# role this poll_url is scoped to, pick it, print it as one JSON line, exit 0.
# The portable "take-a-task" waiter for harnesses that re-invoke an agent when a
# backgrounded process EXITS (Claude Code Bash run_in_background, agy).
#
# The poll_url is a CAPABILITY URL — the unguessable token is IN the path, so
# this is just `curl "$url"` with NO auth header. Get it from listen_role's
# "poll_url" result field.
#
# Usage:   bash broker-poll.sh <poll_url>
#          BROKER_POLL_URL=<poll_url> bash broker-poll.sh   # url via env (keeps it off argv)
#
# Env:
#   BROKER_POLL_URL  the poll_url, if not passed as $1.
#   BROKER_INTERVAL  seconds between polls, positive integer (default 5). Each
#                    poll RENEWS the token server-side, so polling keeps it alive.
#   BROKER_MAX_FAIL  consecutive unreachable/garbled polls before giving up,
#                    positive integer (default 12 ≈ 60s at the default interval).
#   BROKER_MAX_WAIT  max seconds to block with NO task before exiting 4,
#                    non-negative integer (default 0 = unbounded). 0 keeps the
#                    "wait forever for work" intent; set > 0 to bound each turn.
#
# Behavior: ONE-SHOT waiter. Re-GETs the poll_url every BROKER_INTERVAL seconds.
# The role endpoint PICKS a queued task on read, so when one arrives the agent is
# woken already OWNING it: do the work, solve_task, then RELAUNCH this script.
#
# Exit codes (the harness wakes the agent on exit):
#   0   a task was picked — one JSON object on stdout. Global tasks also carry
#       an opaque "work_token" that MUST be sent to progress_task/solve_task.
#   3   the broker was unreachable / returned garbage / errored too long
#   4   BROKER_MAX_WAIT elapsed with still no task — relaunch to keep waiting
#   5   the poll token EXPIRED — call listen_role again for a fresh poll_url, then relaunch
#   64  usage error (no poll_url, or a non-integer numeric env var)
#   69  a required command (curl or jq) is missing
#
# BROKER_SKILL_VERSION=7
set -u

url="${1:-${BROKER_POLL_URL:-}}"
if [ -z "$url" ]; then
  echo "usage: broker-poll.sh <poll_url>   (or set BROKER_POLL_URL)" >&2
  exit 64
fi

for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "broker-poll.sh: missing required command: $bin" >&2; exit 69; }
done

interval="${BROKER_INTERVAL:-5}"
max_fail="${BROKER_MAX_FAIL:-12}"
max_wait="${BROKER_MAX_WAIT:-0}"
fails=0
waited=0

# Accept a single 0 or a leading-zero-free run of digits; reject leading zeros so
# "08" can't hit bash's octal parser mid-loop and "010" can't silently mean 8.
is_uint() { case "$1" in 0) return 0 ;; '' | *[!0-9]* | 0*) return 1 ;; *) return 0 ;; esac; }
for pair in "BROKER_INTERVAL=$interval" "BROKER_MAX_FAIL=$max_fail" "BROKER_MAX_WAIT=$max_wait"; do
  is_uint "${pair#*=}" || { echo "broker-poll.sh: ${pair%%=*} must be a non-negative integer (no leading zeros), got: ${pair#*=}" >&2; exit 64; }
done
[ "$interval" -ge 1 ] || { echo "broker-poll.sh: BROKER_INTERVAL must be >= 1" >&2; exit 64; }
[ "$max_fail" -ge 1 ] || { echo "broker-poll.sh: BROKER_MAX_FAIL must be >= 1" >&2; exit 64; }

bail_if_stuck() {
  fails=$((fails + 1))
  if [ "$fails" -ge "$max_fail" ]; then
    echo "broker-poll.sh: broker unreachable or unresponsive after $fails tries" >&2
    exit 3
  fi
}

while :; do
  # Capture body and HTTP status together, no temp file: -w appends "\n<code>",
  # which we split back off with parameter expansion. -L follows a proxy redirect
  # (e.g. a misconfigured http->https bounce) so the poll still lands.
  resp=$(curl -sL -w '\n%{http_code}' "$url")
  if [ $? -ne 0 ]; then
    bail_if_stuck; sleep "$interval"; continue   # connection-level failure
  fi
  code=${resp##*$'\n'}
  resp=${resp%$'\n'*}
  case "$code" in
    2*) : ;;
    404) echo "broker-poll.sh: poll_url not found (expired or revoked) — call listen_role again for a fresh poll_url" >&2; exit 5 ;;
    *) bail_if_stuck; sleep "$interval"; continue ;;  # transient HTTP error
  esac

  # A 2xx with an unparseable body is a wedged broker, not an idle queue — trip
  # the failure guard instead of silently treating it as "no task yet" forever.
  if ! printf '%s' "$resp" | jq -e . >/dev/null 2>&1; then
    bail_if_stuck; sleep "$interval"; continue
  fi

  status=$(printf '%s' "$resp" | jq -r '.status // empty' 2>/dev/null)

  # Token expired → go get a fresh poll_url (call listen_role again), then relaunch.
  if [ "$status" = "expired" ]; then
    echo "broker-poll.sh: poll token expired — call listen_role again for a fresh poll_url" >&2
    exit 5
  fi
  # An error status (disabled mode, unknown scope, ...) is fatal.
  if [ "$status" = "error" ]; then
    emsg=$(printf '%s' "$resp" | jq -r '.error // "unknown error"' 2>/dev/null)
    echo "broker-poll.sh: broker returned error: $emsg" >&2
    exit 3
  fi

  task=$(printf '%s' "$resp" | jq -c '.task // empty' 2>/dev/null)
  if [ -n "$task" ]; then
    printf '%s\n' "$task"
    exit 0
  fi

  # No task yet (status "empty") — a legitimate wait. Reset the failure guard.
  fails=0
  if [ "$max_wait" -gt 0 ] && [ "$waited" -ge "$max_wait" ]; then
    echo "broker-poll.sh: no task within BROKER_MAX_WAIT=${max_wait}s — relaunch to keep waiting" >&2
    exit 4
  fi
  sleep "$interval"
  waited=$((waited + interval))
done
