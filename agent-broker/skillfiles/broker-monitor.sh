#!/usr/bin/env bash
# broker-monitor.sh <poll_url> — STREAMING variant. Same capability-URL poll as
# broker-poll.sh / await-poll.sh, but it NEVER exits on an event: it keeps GETting
# the poll_url and prints each new event as one JSONL line, forever. This is the
# feed for a harness that streams a long-running script's stdout line-by-line
# (Claude Code's Monitor tool), turning each line into an inline wake-up.
#
# Works for either scope by shape:
#   - role poll_url  → prints each picked task as it arrives ({"task":{...}})
#   - task poll_url  → prints each status change until "solved"
# It de-dupes: an "empty" heartbeat and an unchanged status are not re-emitted.
#
# The poll_url is a CAPABILITY URL (token in the path) — just `curl "$url"`, no
# auth header.
#
# Usage:   run under the Monitor tool (persistent, no timeout):
#          bash broker-monitor.sh <poll_url>
#          BROKER_POLL_URL=<poll_url> bash broker-monitor.sh
#
# ⚠️ Match the script to the launch method. broker-monitor.sh NEVER exits, so it
# only reaches you through a STREAMING tool (Monitor). Do NOT launch it with a
# background-exit tool (Bash run_in_background) — it would poll forever and never
# wake you. Use broker-poll.sh / await-poll.sh for wake-on-exit.
#
# Env: BROKER_POLL_URL, BROKER_INTERVAL (default 5), BROKER_MAX_FAIL (default 12).
#
# Exit codes: 3 broker unreachable too long, 5 token expired (re-arm with a fresh
# poll_url), 64 usage, 69 missing curl/jq. On "solved" (task scope) it prints the
# result and exits 0 — the task is terminal.
#
# BROKER_SKILL_VERSION=5
set -u

url="${1:-${BROKER_POLL_URL:-}}"
if [ -z "$url" ]; then
  echo "usage: broker-monitor.sh <poll_url>   (or set BROKER_POLL_URL)" >&2
  exit 64
fi

for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "broker-monitor.sh: missing required command: $bin" >&2; exit 69; }
done

interval="${BROKER_INTERVAL:-5}"
max_fail="${BROKER_MAX_FAIL:-12}"
fails=0
last=""

is_uint() { case "$1" in 0) return 0 ;; '' | *[!0-9]* | 0*) return 1 ;; *) return 0 ;; esac; }
for pair in "BROKER_INTERVAL=$interval" "BROKER_MAX_FAIL=$max_fail"; do
  is_uint "${pair#*=}" || { echo "broker-monitor.sh: ${pair%%=*} must be a non-negative integer (no leading zeros), got: ${pair#*=}" >&2; exit 64; }
done
[ "$interval" -ge 1 ] || { echo "broker-monitor.sh: BROKER_INTERVAL must be >= 1" >&2; exit 64; }
[ "$max_fail" -ge 1 ] || { echo "broker-monitor.sh: BROKER_MAX_FAIL must be >= 1" >&2; exit 64; }

bail_if_stuck() {
  fails=$((fails + 1))
  if [ "$fails" -ge "$max_fail" ]; then
    echo "broker-monitor.sh: broker unreachable or unresponsive after $fails tries" >&2
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
    404) echo "broker-monitor.sh: poll_url not found (expired or revoked) — re-arm with a fresh poll_url" >&2; exit 5 ;;
    *) bail_if_stuck; sleep "$interval"; continue ;;
  esac

  # A 2xx with an unparseable body is a wedged broker — trip the failure guard.
  if ! printf '%s' "$resp" | jq -e . >/dev/null 2>&1; then
    bail_if_stuck; sleep "$interval"; continue
  fi
  fails=0

  status=$(printf '%s' "$resp" | jq -r '.status // empty' 2>/dev/null)
  if [ "$status" = "expired" ]; then
    echo "broker-monitor.sh: poll token expired — re-arm with a fresh poll_url" >&2
    exit 5
  fi
  if [ "$status" = "error" ]; then
    emsg=$(printf '%s' "$resp" | jq -r '.error // "unknown error"' 2>/dev/null)
    echo "broker-monitor.sh: broker returned error: $emsg" >&2
    exit 3
  fi

  # Emit a new event (skip "empty" heartbeats and unchanged repeats).
  if [ "$status" != "empty" ]; then
    line=$(printf '%s' "$resp" | jq -c '.' 2>/dev/null)
    if [ -n "$line" ] && [ "$line" != "$last" ]; then
      printf '%s\n' "$line"
      last="$line"
    fi
    # A solved task is terminal — nothing more will change.
    if [ "$status" = "solved" ]; then
      exit 0
    fi
  fi

  sleep "$interval"
done
