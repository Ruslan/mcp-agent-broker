package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PollHandler serves the unauthenticated capability-URL poll endpoint,
// GET /poll/{token}. The token in the path IS the authorization — there is no
// Authorization header — so a background poll script is just `curl "$url"`.
//
// Each successful poll RENEWS the token (sliding TTL), so an actively-polling
// script keeps it alive up to the absolute lifetime cap. An expired or capped
// token (one that existed) returns 200 {"status":"expired"} with advice to go
// back to the tool (listen_role for a worker, get_task for a dispatcher) for a
// fresh poll_url; an unknown token returns a bare 404, as if it never existed.
type PollHandler struct {
	broker *Broker
}

func (h *PollHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "error": "method not allowed"})
		return
	}

	// Support both the {token} wildcard pattern and a plain /poll/<token> prefix.
	token := r.PathValue("token")
	if token == "" {
		token = strings.TrimPrefix(r.URL.Path, "/poll/")
	}
	if token == "" || !isSafeID(token) {
		// A malformed token is just an unknown one — hide the endpoint.
		http.NotFound(w, r)
		return
	}

	scope, found, err := h.broker.RenewPollToken(token)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": "internal error"})
		return
	}
	if scope == nil {
		if !found {
			// Unknown token — respond 404 as if this URL never existed, so probing
			// random tokens reveals nothing about the /poll surface.
			http.NotFound(w, r)
			return
		}
		// The token existed but has expired (slid out or hit its 24h cap) — tell a
		// legitimate poller to go get a fresh poll_url.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "expired",
			"advice": "poll token expired — call listen_role again (worker) or get_task again (dispatcher) for a fresh poll_url",
		})
		return
	}

	switch scope.Kind {
	case PollScopeRole:
		// Async picking is what this endpoint does for a role token; if the
		// operator disabled it, refuse here too (the tool schema already gates
		// listen_role poll — this closes the direct-endpoint path).
		if !h.broker.EnableAsync {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "async polling is disabled by server configuration (ENABLE_ASYNC=false)"})
			return
		}
		// Worker: pick a queued task for the role (or report empty).
		task, status, err := h.broker.ListenRole(r.Context(), scope.ProjectID, scope.Value, "poll", 0)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
			return
		}
		if task == nil {
			writeJSON(w, http.StatusOK, map[string]any{"task": nil, "status": status})
			return
		}
		taskPayload := map[string]any{"task_id": task.ID, "title": task.Title, "task_md": task.MD}
		if workToken := h.broker.TaskWorkToken(task); workToken != "" {
			taskPayload["work_token"] = workToken
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": taskPayload})

	case PollScopeTask:
		// Dispatcher: report the task's status; include result_md once solved.
		meta, err := h.broker.GetTaskStatus(scope.ProjectID, scope.Value)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
			return
		}
		resp := map[string]any{"task_id": meta.TaskID, "status": meta.Status}
		if meta.Status == StatusSolved {
			if res, rerr := h.broker.GetTaskResult(scope.ProjectID, scope.Value); rerr == nil {
				resp["result_md"] = res
				_, _ = h.broker.IncrementResultViewCount(scope.ProjectID, scope.Value)
			}
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "unknown token scope"})
	}
}

// pollBaseURL derives the externally-reachable base URL used to build poll_url,
// so a remote agent polls the right host. Precedence: explicit BROKER_PUBLIC_URL
// override → X-Forwarded-Proto (always) / X-Forwarded-Host (only when trusted) →
// the request scheme + Host.
//
// The two X-Forwarded-* headers carry very different risk, so they are gated
// differently:
//
//   - X-Forwarded-Proto only flips the scheme (http/https) of the URL handed
//     back to the SAME caller; it never changes which host the token is sent to,
//     so a forged value can at most downgrade the caller's own poll_url — no
//     cross-tenant exfiltration. It is therefore honored UNCONDITIONALLY, which
//     lets a broker behind a TLS-terminating proxy (Caddy, nginx) emit https://
//     poll_urls with no config and no hardcoded public domain.
//   - X-Forwarded-Host changes WHERE the token goes: a forged value would point
//     the victim's background `curl` (carrying a live capability token) at an
//     attacker's host. It stays gated behind BROKER_TRUST_FORWARDED.
//
// A public deployment can still set BROKER_PUBLIC_URL to pin both explicitly.
// Returns "" when no host resolves (client falls back to localhost).
func pollBaseURL(r *http.Request, override string, trustForwarded bool) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// X-Forwarded-Proto is safe to trust (scheme-only, same host) — honor it so a
	// broker behind a TLS proxy reports https without configuration.
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		// A proxy chain may send a comma-separated list; the client-facing scheme
		// is the first. Accept only http/https so a junk value can't produce a
		// "garbage://host" poll_url.
		if i := strings.IndexByte(p, ','); i >= 0 {
			p = strings.TrimSpace(p[:i])
		}
		if p == "http" || p == "https" {
			scheme = p
		}
	}
	host := r.Host
	// X-Forwarded-Host redirects the token to an arbitrary host — gate it.
	if trustForwarded {
		if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// buildPollURL assembles the full capability URL for a token. With no resolvable
// base it returns a path-only URL the client resolves against its known base.
func buildPollURL(base, token string) string {
	return base + "/poll/" + token
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
