package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRPCError_Error(t *testing.T) {
	err := &RPCError{
		Code:    123,
		Message: "test error message",
	}

	if err.Error() != "test error message" {
		t.Errorf("Expected 'test error message', got '%s'", err.Error())
	}
}

func TestHandleToolCall_ProgressTask(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	ctx := context.Background()
	projectID := "default"

	// Create and pick a task
	taskID, _ := broker.CreateTask(projectID, "coder", "Title", "MD")
	broker.ListenRole(ctx, projectID, "coder", "poll", 0)

	// Send progress via RPC
	args, _ := json.Marshal(map[string]string{"task_id": taskID, "message": "halfway there"})
	res, err := handler.handleToolCall(ctx, projectID, "progress_task", args)
	if err != nil {
		t.Fatalf("progress_task failed: %v", err)
	}
	if ok, _ := res.(map[string]bool)["ok"]; !ok {
		t.Errorf("Expected ok=true, got %v", res)
	}

	// Solve and check progress is included in await_task response
	go func() {
		time.Sleep(10 * time.Millisecond)
		broker.SolveTask(projectID, taskID, "done")
	}()

	awaitArgs, _ := json.Marshal(map[string]any{"task_id": taskID, "timeout_ms": 500})
	res, err = handler.handleToolCall(ctx, projectID, "await_task", awaitArgs)
	if err != nil {
		t.Fatalf("await_task failed: %v", err)
	}
	resMap := res.(map[string]any)
	progress, ok := resMap["progress"].([]string)
	if !ok || len(progress) != 1 || progress[0] != "halfway there" {
		t.Errorf("Expected progress=[\"halfway there\"], got %v", resMap["progress"])
	}
}

func TestHandleToolCall_ProgressTask_Validation(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	ctx := context.Background()
	projectID := "default"

	// Missing message
	args, _ := json.Marshal(map[string]string{"task_id": "abc"})
	_, err := handler.handleToolCall(ctx, projectID, "progress_task", args)
	if err == nil {
		t.Error("Expected error for missing message")
	}

	// Message too long
	longMsg := string(make([]byte, 501))
	args, _ = json.Marshal(map[string]string{"task_id": "abc", "message": longMsg})
	_, err = handler.handleToolCall(ctx, projectID, "progress_task", args)
	if err == nil {
		t.Error("Expected error for message too long")
	}

	// Nonexistent task
	args, _ = json.Marshal(map[string]string{"task_id": "nonexistent", "message": "hi"})
	_, err = handler.handleToolCall(ctx, projectID, "progress_task", args)
	if err == nil {
		t.Error("Expected error for nonexistent task")
	}
}
