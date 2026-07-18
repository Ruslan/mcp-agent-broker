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
		writeJSON(w, http.StatusOK, map[string]any{
			"task": map[string]any{"task_id": task.ID, "title": task.Title, "task_md": task.MD},
		})

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
// override → (only when trustForwarded) X-Forwarded-Proto/Host → the request Host.
//
// X-Forwarded-* is client-controllable through a naive proxy; trusting it blindly
// would let an attacker forge a poll_url pointing the victim's background `curl`
// (which carries a live capability token) at the attacker's host. So it is
// honored ONLY behind an explicitly-trusted proxy (BROKER_TRUST_FORWARDED). A
// public deployment should set BROKER_PUBLIC_URL, which sidesteps the headers
// entirely. Returns "" when no host resolves (client falls back to localhost).
func pollBaseURL(r *http.Request, override string, trustForwarded bool) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if trustForwarded {
		if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
			scheme = p
		}
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
