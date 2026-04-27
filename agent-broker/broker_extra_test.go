package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestBroker_GetTaskMD_And_Result(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "broker-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	broker, err := NewBroker(tmpDir, "", true, true)
	if err != nil {
		t.Fatal(err)
	}

	projectID := "default"
	taskID, err := broker.CreateTask(projectID, "role1", "title1", "task content")
	if err != nil {
		t.Fatal(err)
	}

	// Test GetTaskMD
	md, err := broker.GetTaskMD(projectID, taskID)
	if err != nil {
		t.Fatalf("GetTaskMD failed: %v", err)
	}
	if md != "task content" {
		t.Errorf("Expected 'task content', got '%s'", md)
	}

	// Test GetTaskMD invalid id
	_, err = broker.GetTaskMD(projectID, "../invalid")
	if err == nil {
		t.Error("Expected error for invalid task ID")
	}

	// Test GetTaskResult before solve
	res, err := broker.GetTaskResult(projectID, taskID)
	if err != nil {
		t.Fatalf("GetTaskResult failed: %v", err)
	}
	if res != "" {
		t.Errorf("Expected empty result, got '%s'", res)
	}

	// Solve task
	err = broker.SolveTask(projectID, taskID, "result content")
	if err != nil {
		t.Fatal(err)
	}

	// Test GetTaskResult after solve
	res, err = broker.GetTaskResult(projectID, taskID)
	if err != nil {
		t.Fatalf("GetTaskResult failed: %v", err)
	}
	if res != "result content" {
		t.Errorf("Expected 'result content', got '%s'", res)
	}

	// Test GetTaskResult invalid task
	_, err = broker.GetTaskResult(projectID, "nonexistent")
	if err == nil {
		t.Error("Expected error for nonexistent task")
	}

	// Test GetTaskResult invalid id
	_, err = broker.GetTaskResult(projectID, "../invalid")
	if err == nil {
		t.Error("Expected error for invalid task ID")
	}
}

func TestBroker_AwaitTask_Timeout(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "broker-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	broker, err := NewBroker(tmpDir, "", true, true)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	projectID := "default"
	taskID, _ := broker.CreateTask(projectID, "role1", "title1", "task content")

	status, res, err := broker.AwaitTask(ctx, projectID, taskID, 50)
	if err != nil {
		t.Fatalf("AwaitTask error: %v", err)
	}
	if status != string(StatusQueued) {
		t.Errorf("Expected status queued, got %s", status)
	}
	if res != "" {
		t.Errorf("Expected empty result, got %s", res)
	}

	// Invalid task id
	_, _, err = broker.AwaitTask(ctx, projectID, "../invalid", 50)
	if err == nil {
		t.Error("Expected error for invalid task id")
	}

	// Nonexistent task
	_, _, err = broker.AwaitTask(ctx, projectID, "nonexistent", 50)
	if err == nil {
		t.Error("Expected error for nonexistent task id")
	}
}
