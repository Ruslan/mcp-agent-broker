package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFeatureFlags_SyncOnly(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-flags-sync-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, false)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Check tools/list
	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage("1"),
	}
	res := callHandler(handler, req, "default")
	
	tools := res.Result.(map[string]any)["tools"].([]any)
	hasSync := false
	hasAsync := false
	for _, t := range tools {
		name := t.(map[string]any)["name"].(string)
		if strings.Contains(name, "_sync") {
			hasSync = true
		}
		if strings.Contains(name, "_async") || strings.Contains(name, "get_task_") {
			hasAsync = true
		}
	}
	if !hasSync || hasAsync {
		t.Errorf("Expected only sync tools, got sync=%v, async=%v", hasSync, hasAsync)
	}

	// 2. Try calling async tool
	callReq := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"create_task_async","arguments":{"role":"coder","title":"test","task_md":"test"}}`),
		ID:      json.RawMessage("2"),
	}
	res = callHandler(handler, callReq, "default")
	if res.Error == nil || !strings.Contains(res.Error.Message, "disabled by server configuration") {
		t.Errorf("Expected disabled error, got %v", res.Error)
	}
}

func TestFeatureFlags_AsyncOnly(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-flags-async-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, false, true)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Check tools/list
	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage("1"),
	}
	res := callHandler(handler, req, "default")
	
	tools := res.Result.(map[string]any)["tools"].([]any)
	hasSync := false
	hasAsync := false
	for _, t := range tools {
		name := t.(map[string]any)["name"].(string)
		if strings.Contains(name, "_sync") {
			hasSync = true
		}
		if strings.Contains(name, "_async") || strings.Contains(name, "get_task_") {
			hasAsync = true
		}
	}
	if hasSync || !hasAsync {
		t.Errorf("Expected only async tools, got sync=%v, async=%v", hasSync, hasAsync)
	}

	// 2. Try calling sync tool
	callReq := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"create_task_sync","arguments":{"role":"coder","title":"test","task_md":"test"}}`),
		ID:      json.RawMessage("2"),
	}
	res = callHandler(handler, callReq, "default")
	if res.Error == nil || !strings.Contains(res.Error.Message, "disabled by server configuration") {
		t.Errorf("Expected disabled error, got %v", res.Error)
	}
}

func TestFeatureFlags_SolveTaskAlwaysAvailable(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-flags-solve-*")
	defer os.RemoveAll(tmpDir)

	// Sync only, but solve_task should be there
	broker, _ := NewBroker(tmpDir, true, false)
	handler := &JSONRPCHandler{broker: broker}

	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage("1"),
	}
	res := callHandler(handler, req, "default")
	tools := res.Result.(map[string]any)["tools"].([]any)
	found := false
	for _, t := range tools {
		if t.(map[string]any)["name"].(string) == "solve_task" {
			found = true
			break
		}
	}
	if !found {
		t.Error("solve_task should be available even if async is disabled")
	}
}

func TestHealthEndpoint(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-health-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, false)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Valid GET
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	r.Header.Set("X-Project-Id", "custom-project")
	handler.HealthHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)

	if res["ok"] != true || res["service"] != "agent-broker" {
		t.Errorf("Unexpected response: %v", res)
	}
	if res["version"] != ServerVersion || res["protocol_version"] != ProtocolVersion {
		t.Errorf("Unexpected version fields: %v", res)
	}
	if res["enable_sync"] != true || res["enable_async"] != false {
		t.Errorf("Unexpected flags: %v", res)
	}
	if res["project_id"] != "custom-project" {
		t.Errorf("Expected project_id custom-project, got %v", res["project_id"])
	}

	// 2. Invalid Method
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/health", nil)
	handler.HealthHandler(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestFeatureFlags_BothEnabled(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-flags-both-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, true)
	handler := &JSONRPCHandler{broker: broker}

	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage("1"),
	}
	res := callHandler(handler, req, "default")
	
	tools := res.Result.(map[string]any)["tools"].([]any)
	hasSync := false
	hasAsync := false
	for _, t := range tools {
		name := t.(map[string]any)["name"].(string)
		if strings.Contains(name, "_sync") {
			hasSync = true
		}
		if strings.Contains(name, "_async") {
			hasAsync = true
		}
	}
	if !hasSync || !hasAsync {
		t.Errorf("Expected both sync and async tools, got sync=%v, async=%v", hasSync, hasAsync)
	}
}

func TestFeatureFlags_BothDisabled(t *testing.T) {
	err := validateConfig(false, false)
	if err == nil {
		t.Error("Expected error when both sync and async are disabled")
	}
	if !strings.Contains(err.Error(), "both ENABLE_SYNC and ENABLE_ASYNC are disabled") {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func callHandler(h *JSONRPCHandler, req Request, projectID string) Response {
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/rpc", strings.NewReader(string(body)))
	if projectID != "" {
		r.Header.Set("X-Project-Id", projectID)
	}
	h.ServeHTTP(w, r)

	var res Response
	json.Unmarshal(w.Body.Bytes(), &res)
	return res
}

func TestGetEnvBool(t *testing.T) {
	os.Setenv("TEST_BOOL_TRUE", "true")
	os.Setenv("TEST_BOOL_FALSE", "false")
	os.Unsetenv("TEST_BOOL_UNSET")

	if !getEnvBool("TEST_BOOL_TRUE", false) {
		t.Error("Expected true for TEST_BOOL_TRUE")
	}
	if getEnvBool("TEST_BOOL_FALSE", true) {
		t.Error("Expected false for TEST_BOOL_FALSE")
	}
	if !getEnvBool("TEST_BOOL_UNSET", true) {
		t.Error("Expected default true for TEST_BOOL_UNSET")
	}
}
