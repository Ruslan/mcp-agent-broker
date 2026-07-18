package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pollURLFromToolResult extracts poll_url from a tools/call Response whose result
// content is the JSON-encoded tool payload.
func pollURLFromToolResult(t *testing.T, resp Response) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tool call errored: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	var payload struct {
		PollURL string `json:"poll_url"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal tool payload: %v", err)
	}
	return payload.PollURL
}

// tokenFromPollURL returns the trailing token segment of a /poll/<token> URL.
func tokenFromPollURL(url string) string {
	i := strings.LastIndex(url, "/poll/")
	if i < 0 {
		return ""
	}
	return url[i+len("/poll/"):]
}

// callTool issues a tools/call over the full JSON-RPC handler with a Host set so
// poll_url gets a real base.
func callTool(t *testing.T, handler *JSONRPCHandler, project, argsJSON string) Response {
	t.Helper()
	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(argsJSON),
		ID:      json.RawMessage(`"1"`),
	}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/rpc", strings.NewReader(string(body)))
	r.Host = "broker.example:9197"
	if project != "" {
		r.Header.Set("X-Project-Id", project)
	}
	handler.ServeHTTP(w, r)
	var res Response
	json.Unmarshal(w.Body.Bytes(), &res)
	return res
}

// TestCreateTaskReturnsTaskPollURL: create_task hands back a task-scoped poll_url
// built from the request host.
func TestCreateTaskReturnsTaskPollURL(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	resp := callTool(t, handler, "default", `{"name":"create_task","arguments":{"role":"coder","title":"T","task_md":"M"}}`)
	url := pollURLFromToolResult(t, resp)
	if !strings.HasPrefix(url, "http://broker.example:9197/poll/") {
		t.Fatalf("unexpected poll_url: %q", url)
	}
	scope, _, err := broker.RenewPollToken(tokenFromPollURL(url))
	if err != nil || scope == nil {
		t.Fatalf("token did not resolve: scope=%v err=%v", scope, err)
	}
	if scope.Kind != PollScopeTask {
		t.Fatalf("expected task scope, got %+v", scope)
	}
}

// TestListenRolePollReturnsRolePollURL: listen_role(poll) hands back a
// role-scoped poll_url; wait mode does not.
func TestListenRolePollReturnsRolePollURL(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	resp := callTool(t, handler, "default", `{"name":"listen_role","arguments":{"role":"coder","mode":"poll"}}`)
	url := pollURLFromToolResult(t, resp)
	if url == "" {
		t.Fatal("listen_role poll returned no poll_url")
	}
	scope, _, _ := broker.RenewPollToken(tokenFromPollURL(url))
	if scope == nil || scope.Kind != PollScopeRole || scope.Value != "coder" {
		t.Fatalf("unexpected scope: %+v", scope)
	}
}

// TestListenRoleWaitTimeoutReturnsPollURL: a wait that times out empty hands back
// a role poll_url plus advice to go async; a wait that returns a task does not.
func TestListenRoleWaitTimeoutReturnsPollURL(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	// Empty queue + tiny timeout → timeout with poll_url + advice.
	resp := callTool(t, handler, "default", `{"name":"listen_role","arguments":{"role":"coder","mode":"wait","timeout_ms":50}}`)
	result := resp.Result.(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload struct {
		Status  string `json:"status"`
		PollURL string `json:"poll_url"`
		Advice  string `json:"advice"`
	}
	json.Unmarshal([]byte(text), &payload)
	if payload.Status != "timeout" {
		t.Fatalf("expected timeout, got %q", payload.Status)
	}
	if payload.PollURL == "" {
		t.Fatal("wait timeout did not return a poll_url")
	}
	if payload.Advice == "" {
		t.Fatal("wait timeout did not return advice")
	}
	scope, _, _ := broker.RenewPollToken(tokenFromPollURL(payload.PollURL))
	if scope == nil || scope.Kind != PollScopeRole {
		t.Fatalf("wait-timeout poll_url is not a role token: %+v", scope)
	}
}

// TestAsyncDisabledNoPollURL: with ENABLE_ASYNC off, a wait-timeout must NOT hand
// back a role poll_url, and the poll endpoint must refuse a role token — the flag
// can't be bypassed through the poll surface.
func TestAsyncDisabledNoPollURL(t *testing.T) {
	broker := newTestBroker(t, true, false) // sync on, async off
	handler := &JSONRPCHandler{broker: broker}

	resp := callTool(t, handler, "default", `{"name":"listen_role","arguments":{"role":"coder","mode":"wait","timeout_ms":50}}`)
	result := resp.Result.(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "poll_url") {
		t.Fatalf("async disabled: wait-timeout must not return a poll_url, got %s", text)
	}

	// A role token that somehow exists is refused by the poll endpoint too.
	tok, _ := broker.MintPollToken("default", PollScopeRole, "coder")
	code, got := pollHTTP(t, &PollHandler{broker: broker}, tok.Token)
	if code != http.StatusOK || got["status"] != "error" {
		t.Fatalf("async disabled: role poll should error, got %d %v", code, got)
	}
}

// pollHTTP GETs /poll/{token} through a mux with the wildcard pattern and returns
// the HTTP status code and the decoded JSON body (nil body for a non-JSON 404).
func pollHTTP(t *testing.T, ph *PollHandler, token string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /poll/{token}", ph)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/poll/"+token, nil)
	mux.ServeHTTP(w, r)
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// TestPollEndpointWorkerAndDispatcher drives the whole capability-URL flow with
// no auth headers: role token picks a queued task; task token reports solved.
func TestPollEndpointWorkerAndDispatcher(t *testing.T) {
	broker := newTestBroker(t, true, true)
	ph := &PollHandler{broker: broker}

	// Dispatcher side: create a task, get a task token, poll it.
	taskID, _ := broker.CreateTask("default", "coder", "T", "M")
	taskTok, _ := broker.MintPollToken("default", PollScopeTask, taskID)

	_, got := pollHTTP(t, ph, taskTok.Token)
	if got["status"] != string(StatusQueued) {
		t.Fatalf("task poll before solve: expected queued, got %v", got["status"])
	}

	// Worker side: role token picks the queued task.
	roleTok, _ := broker.MintPollToken("default", PollScopeRole, "coder")
	_, got = pollHTTP(t, ph, roleTok.Token)
	task, ok := got["task"].(map[string]any)
	if !ok || task["task_id"] != taskID {
		t.Fatalf("role poll did not pick the task: %v", got)
	}

	// Solve it, then the task poll returns the result.
	if err := broker.SolveTask("default", taskID, "the answer"); err != nil {
		t.Fatalf("solve: %v", err)
	}
	_, got = pollHTTP(t, ph, taskTok.Token)
	if got["status"] != string(StatusSolved) || got["result_md"] != "the answer" {
		t.Fatalf("task poll after solve: %v", got)
	}
}

// TestPollEndpointUnknownVsExpired: an unknown token 404s (as if the URL never
// existed); a token that existed but expired returns 200 {"status":"expired"}.
func TestPollEndpointUnknownVsExpired(t *testing.T) {
	broker := newTestBroker(t, true, true)
	ph := &PollHandler{broker: broker}

	// Unknown token → 404, no expired JSON.
	code, got := pollHTTP(t, ph, "deadbeefdeadbeef")
	if code != http.StatusNotFound {
		t.Fatalf("unknown token: expected 404, got %d (body %v)", code, got)
	}

	// Insert a token that already expired (created recently, within the 24h cap)
	// → it existed, so poll returns 200 expired with advice, not 404.
	created := time.Now().UTC().Add(-1 * time.Hour)
	if err := broker.store.InsertPollToken("expiredtok", "default", PollScopeTask, "t1", created, created.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	code, got = pollHTTP(t, ph, "expiredtok")
	if code != http.StatusOK || got["status"] != "expired" {
		t.Fatalf("expired token: expected 200 expired, got %d %v", code, got)
	}
	if _, ok := got["advice"].(string); !ok {
		t.Fatalf("expired response missing advice: %v", got)
	}
}

// TestPollTokenRenewalAndHardCap: each poll slides the expiry forward, but never
// past the absolute lifetime cap.
func TestPollTokenRenewalAndHardCap(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Fresh token: renewal slides expiry to ~now+ttl.
	created := time.Now().UTC()
	if err := store.InsertPollToken("tok1", "default", PollScopeRole, "coder", created, created.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	scope, found, err := store.RenewPollToken("tok1", 30*time.Minute, 24*time.Hour)
	if err != nil || scope == nil || !found {
		t.Fatalf("renew fresh token: scope=%v found=%v err=%v", scope, found, err)
	}
	if time.Until(scope.ExpiresAt) < 25*time.Minute {
		t.Fatalf("renewal did not slide expiry forward: %v", scope.ExpiresAt)
	}

	// Token created > maxLifetime ago: renewal refuses and drops it (past hard cap),
	// but reports found=true so the endpoint answers "expired", not 404.
	old := time.Now().UTC().Add(-25 * time.Hour)
	if err := store.InsertPollToken("tok2", "default", PollScopeRole, "coder", old, time.Now().UTC().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	scope, found, err = store.RenewPollToken("tok2", 30*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("renew capped token errored: %v", err)
	}
	if scope != nil || !found {
		t.Fatalf("token past hard cap should be expired (found, no scope), got scope=%+v found=%v", scope, found)
	}

	// A never-seen token: not found → the endpoint 404s.
	scope, found, err = store.RenewPollToken("nope", 30*time.Minute, 24*time.Hour)
	if err != nil || scope != nil || found {
		t.Fatalf("unknown token should be not-found, got scope=%+v found=%v err=%v", scope, found, err)
	}
}

// TestPollEndpointUnauthenticated: /poll/{token} is reachable with NO auth even
// when the master API_KEY is set, while /rpc still requires it.
func TestPollEndpointUnauthenticated(t *testing.T) {
	broker := newTestBroker(t, true, true)
	taskID, _ := broker.CreateTask("default", "coder", "T", "M")
	tok, _ := broker.MintPollToken("default", PollScopeTask, taskID)

	mux := http.NewServeMux()
	mux.Handle("GET /poll/{token}", &PollHandler{broker: broker})
	mux.Handle("/rpc", &JSONRPCHandler{broker: broker})
	stack := AuthMiddleware("master-secret", mux)

	// No Authorization header on /poll → still 200.
	w := httptest.NewRecorder()
	stack.ServeHTTP(w, httptest.NewRequest("GET", "/poll/"+tok.Token, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/poll without auth: expected 200, got %d", w.Code)
	}
	// No Authorization header on /rpc → 401.
	w = httptest.NewRecorder()
	stack.ServeHTTP(w, httptest.NewRequest("POST", "/rpc", strings.NewReader("{}")))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/rpc without auth: expected 401, got %d", w.Code)
	}
}
