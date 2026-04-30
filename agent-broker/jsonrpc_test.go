package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONRPC_ServeHTTP(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	tests := []struct {
		name         string
		method       string
		body         interface{}
		expectedCode int
	}{
		{
			name:         "GET returns not allowed",
			method:       http.MethodGet,
			body:         nil,
			expectedCode: http.StatusMethodNotAllowed,
		},
		{
			name:         "PUT returns not allowed",
			method:       http.MethodPut,
			body:         nil,
			expectedCode: http.StatusMethodNotAllowed,
		},
		{
			name:         "Invalid JSON",
			method:       http.MethodPost,
			body:         "invalid json",
			expectedCode: http.StatusOK, // RPCError sent as JSON HTTP 200
		},
		{
			name:   "Valid JSONRPC initialize",
			method: http.MethodPost,
			body: Request{
				JSONRPC: "2.0",
				Method:  "initialize",
				ID:      json.RawMessage(`1`),
			},
			expectedCode: http.StatusOK,
		},
		{
			name:   "Missing JSONRPC version",
			method: http.MethodPost,
			body: Request{
				Method: "initialize",
				ID:     json.RawMessage(`1`),
			},
			expectedCode: http.StatusOK, // returns RPC Error Response
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqBody []byte
			if str, ok := tt.body.(string); ok {
				reqBody = []byte(str)
			} else if tt.body != nil {
				reqBody, _ = json.Marshal(tt.body)
			}

			req := httptest.NewRequest(tt.method, "/", bytes.NewReader(reqBody))
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedCode {
				t.Errorf("Expected status code %d, got %d", tt.expectedCode, rr.Code)
			}
		})
	}
}

func TestJSONRPC_HandleToolCall(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	ctx := context.Background()
	projectID := "default"

	// 1. Create task
	createArgs := `{"role":"coder", "title":"Test Task", "task_md":"Do something"}`
	res, err := handler.handleToolCall(ctx, projectID, "create_task", json.RawMessage(createArgs))
	if err != nil {
		t.Fatalf("create_task failed: %v", err)
	}
	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Expected map[string]any, got %T", res)
	}
	taskID, ok := resMap["task_id"].(string)
	if !ok || taskID == "" {
		t.Fatalf("Expected valid task_id, got %v", resMap["task_id"])
	}

	// 2. Listen role
	listenArgs := `{"role":"coder", "mode":"poll", "timeout_ms":0}`
	res, err = handler.handleToolCall(ctx, projectID, "listen_role", json.RawMessage(listenArgs))
	if err != nil {
		t.Fatalf("listen_role failed: %v", err)
	}

	// 3. Solve task
	solveArgs := map[string]string{
		"task_id":   taskID,
		"result_md": "Done",
	}
	solveArgsBytes, _ := json.Marshal(solveArgs)
	res, err = handler.handleToolCall(ctx, projectID, "solve_task", json.RawMessage(solveArgsBytes))
	if err != nil {
		t.Fatalf("solve_task failed: %v", err)
	}

	// 4. Get task
	getArgs := map[string]any{
		"task_id":           taskID,
		"include_task_md":   true,
		"include_result_md": true,
	}
	getArgsBytes, _ := json.Marshal(getArgs)
	res, err = handler.handleToolCall(ctx, projectID, "get_task", json.RawMessage(getArgsBytes))
	if err != nil {
		t.Fatalf("get_task failed: %v", err)
	}

	// 5. List tasks
	listArgs := `{"role":"coder"}`
	res, err = handler.handleToolCall(ctx, projectID, "list_tasks", json.RawMessage(listArgs))
	if err != nil {
		t.Fatalf("list_tasks failed: %v", err)
	}

	// 6. Await task
	awaitArgs := map[string]any{
		"task_id":    taskID,
		"timeout_ms": 100,
	}
	awaitArgsBytes, _ := json.Marshal(awaitArgs)
	res, err = handler.handleToolCall(ctx, projectID, "await_task", json.RawMessage(awaitArgsBytes))
	if err != nil {
		t.Fatalf("await_task failed: %v", err)
	}
}

func TestJSONRPC_ServeHTTP_ToolCalls(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	tests := []struct {
		name   string
		method string
		params map[string]any
	}{
		{
			name:   "tools/list",
			method: "tools/list",
			params: map[string]any{},
		},
		{
			name:   "prompts/list",
			method: "prompts/list",
			params: map[string]any{},
		},
		{
			name:   "prompts/get",
			method: "prompts/get",
			params: map[string]any{
				"name":      "coder-async",
				"arguments": map[string]string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paramsBytes, _ := json.Marshal(tt.params)
			reqBody := Request{
				JSONRPC: "2.0",
				Method:  tt.method,
				ID:      json.RawMessage(`1`),
				Params:  paramsBytes,
			}
			reqBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(reqBytes))
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("Expected status code 200, got %d", rr.Code)
			}
		})
	}
}
