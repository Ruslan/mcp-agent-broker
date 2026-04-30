package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFeatureFlags_SyncOnly(t *testing.T) {
	broker := newTestBroker(t, true, false)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Check tools/list - await_task should be there, listen_role should have only "wait"
	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage(`"1"`),
	}
	res := callHandler(handler, req, "default")

	tools := res.Result.(map[string]any)["tools"].([]any)
	hasAwait := false
	modes := []any{}
	for _, t := range tools {
		name := t.(map[string]any)["name"].(string)
		if name == "await_task" {
			hasAwait = true
		}
		if name == "listen_role" {
			modes = t.(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)["mode"].(map[string]any)["enum"].([]any)
		}
	}

	if !hasAwait {
		t.Error("Expected await_task tool to be present")
	}
	if len(modes) != 1 || modes[0].(string) != "wait" {
		t.Errorf("Expected only 'wait' mode, got %v", modes)
	}

	// 2. Try calling poll mode
	callReq := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"listen_role","arguments":{"role":"coder","mode":"poll"}}`),
		ID:      json.RawMessage(`"2"`),
	}
	res = callHandler(handler, callReq, "default")
	if res.Error == nil || !strings.Contains(res.Error.Message, "disabled") {
		t.Errorf("Expected disabled error for poll mode, got %v", res.Error)
	}
}

func TestFeatureFlags_AsyncOnly(t *testing.T) {
	broker := newTestBroker(t, false, true)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Check tools/list - await_task should be missing, listen_role should have only "poll"
	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage(`"1"`),
	}
	res := callHandler(handler, req, "default")

	tools := res.Result.(map[string]any)["tools"].([]any)
	hasAwait := false
	modes := []any{}
	for _, t := range tools {
		name := t.(map[string]any)["name"].(string)
		if name == "await_task" {
			hasAwait = true
		}
		if name == "listen_role" {
			modes = t.(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)["mode"].(map[string]any)["enum"].([]any)
		}
	}

	if hasAwait {
		t.Error("Did not expect await_task tool")
	}
	if len(modes) != 1 || modes[0].(string) != "poll" {
		t.Errorf("Expected only 'poll' mode, got %v", modes)
	}

	// 2. Try calling await_task
	callReq := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"await_task","arguments":{"task_id":"123"}}`),
		ID:      json.RawMessage(`"2"`),
	}
	res = callHandler(handler, callReq, "default")
	if res.Error == nil || !strings.Contains(res.Error.Message, "disabled") {
		t.Errorf("Expected disabled error for await_task, got %v", res.Error)
	}
}
