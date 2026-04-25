package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectTenancy_Isolation(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-tenancy-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, true)
	handler := &JSONRPCHandler{broker: broker}

	projA := "project-a"
	projB := "project-b"

	// 1. Start listener in Project A
	taskChA := make(chan Response, 1)
	go func() {
		req := Request{
			JSONRPC: "2.0",
			Method:  "tools/call",
			Params:  json.RawMessage(`{"name":"listen_role_sync","arguments":{"role":"coder"}}`),
			ID:      json.RawMessage(`"a1"`),
		}
		taskChA <- callHandler(handler, req, projA)
	}()

	// 2. Start listener in Project B
	taskChB := make(chan Response, 1)
	go func() {
		req := Request{
			JSONRPC: "2.0",
			Method:  "tools/call",
			Params:  json.RawMessage(`{"name":"listen_role_sync","arguments":{"role":"coder"}}`),
			ID:      json.RawMessage(`"b1"`),
		}
		taskChB <- callHandler(handler, req, projB)
	}()

	time.Sleep(100 * time.Millisecond)

	// 3. Create task in Project A
	createReqA := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"create_task_async","arguments":{"role":"coder","title":"Task A","task_md":"Content A"}}`),
		ID:      json.RawMessage(`"a2"`),
	}
	resCreateA := callHandler(handler, createReqA, projA)
	if resCreateA.Error != nil {
		t.Fatalf("Create task A failed: %v", resCreateA.Error)
	}

	// 4. Verify Project A listener got the task
	select {
	case res := <-taskChA:
		var taskResp struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		data, _ := json.Marshal(res.Result)
		json.Unmarshal(data, &taskResp)
		
		var task map[string]string
		json.Unmarshal([]byte(taskResp.Content[0].Text), &task)
		
		if task["title"] != "Task A" {
			t.Errorf("Project A listener got wrong task: %v", task)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Project A listener timed out")
	}

	// 5. Verify Project B listener STILL WAITING (isolation)
	select {
	case res := <-taskChB:
		t.Errorf("Project B listener got task unexpectedly: %v", res.Result)
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	// 6. Create task in Project B
	createReqB := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"create_task_async","arguments":{"role":"coder","title":"Task B","task_md":"Content B"}}`),
		ID:      json.RawMessage(`"b2"`),
	}
	callHandler(handler, createReqB, projB)

	// 7. Verify Project B listener got the task
	select {
	case res := <-taskChB:
		var taskResp struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		data, _ := json.Marshal(res.Result)
		json.Unmarshal(data, &taskResp)
		
		var task map[string]string
		json.Unmarshal([]byte(taskResp.Content[0].Text), &task)

		if task["title"] != "Task B" {
			t.Errorf("Project B listener got wrong task: %v", task)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Project B listener timed out")
	}
}

func TestProjectTenancy_InvalidProjectID(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-invalid-proj-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, true)
	handler := &JSONRPCHandler{broker: broker}

	req := Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"listen_role_sync","arguments":{"role":"coder"}}`),
		ID:      json.RawMessage("1"),
	}
	
	// Invalid project ID (contains separator)
	res := callHandler(handler, req, "proj/a")
	if res.Error == nil || !strings.Contains(res.Error.Message, "Invalid project_id") {
		t.Errorf("Expected invalid project_id error, got %v", res.Error)
	}

	// Verify /health also rejects it
	wHealth := httptest.NewRecorder()
	rHealth := httptest.NewRequest("GET", "/health", nil)
	rHealth.Header.Set("X-Project-Id", "proj/a")
	handler.HealthHandler(wHealth, rHealth)
	
	var resHealth Response
	json.Unmarshal(wHealth.Body.Bytes(), &resHealth)
	if resHealth.Error == nil || !strings.Contains(resHealth.Error.Message, "Invalid project_id") {
		t.Errorf("Expected /health to reject invalid project_id with error, got %v", resHealth.Error)
	}

	// Empty project ID (after trimming) -> should default to "default" and succeed/block
	// We'll use a short timeout for this one
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"listen_role_async","arguments":{"role":"coder"}},"id":"2"}`))
	r.Header.Set("X-Project-Id", "   ")
	r = r.WithContext(ctx)
	handler.ServeHTTP(w, r)
	
	var res2 Response
	json.Unmarshal(w.Body.Bytes(), &res2)
	if res2.Error != nil {
		t.Errorf("Expected 'default' project to work, got error: %v", res2.Error)
	}
}

func TestProjectTenancy_DiskLayout(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-disk-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, true, true)
	
	proj := "my-project"
	taskID, _ := broker.CreateTaskAsync(proj, "coder", "Title", "MD")

	// Check if directory exists
	path := filepath.Join(tmpDir, proj, taskID, "task.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Task file not found at expected path: %s", path)
	}
}
