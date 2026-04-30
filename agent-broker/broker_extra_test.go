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

	status, res, _, err := broker.AwaitTask(ctx, projectID, taskID, 50)
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
	_, _, _, err = broker.AwaitTask(ctx, projectID, "../invalid", 50)
	if err == nil {
		t.Error("Expected error for invalid task id")
	}

	// Nonexistent task
	_, _, _, err = broker.AwaitTask(ctx, projectID, "nonexistent", 50)
	if err == nil {
		t.Error("Expected error for nonexistent task id")
	}
}

func TestBroker_ReportProgress(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-progress-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, "", true, true)

	projectID := "default"
	taskID, _ := broker.CreateTask(projectID, "coder", "Title", "MD")
	broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)

	// Send progress updates
	if err := broker.ReportProgress(projectID, taskID, "step 1 done"); err != nil {
		t.Fatalf("ReportProgress failed: %v", err)
	}
	if err := broker.ReportProgress(projectID, taskID, "step 2 done"); err != nil {
		t.Fatalf("ReportProgress failed: %v", err)
	}

	// Solve and await — progress should be returned
	go func() {
		time.Sleep(10 * time.Millisecond)
		broker.SolveTask(projectID, taskID, "final result")
	}()

	_, result, progress, err := broker.AwaitTask(context.Background(), projectID, taskID, 500)
	if err != nil {
		t.Fatalf("AwaitTask failed: %v", err)
	}
	if result != "final result" {
		t.Errorf("Expected final result, got %q", result)
	}
	if len(progress) != 2 {
		t.Errorf("Expected 2 progress messages, got %d: %v", len(progress), progress)
	}
	if progress[0] != "step 1 done" || progress[1] != "step 2 done" {
		t.Errorf("Unexpected progress messages: %v", progress)
	}
}

func TestBroker_ReportProgress_NotFound(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-progress-notfound-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, "", true, true)

	err := broker.ReportProgress("default", "nonexistent", "hello")
	if err == nil {
		t.Error("Expected error for nonexistent task")
	}
}

func TestBroker_ReportProgress_AfterSolve(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-progress-solved-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, "", true, true)

	projectID := "default"
	taskID, _ := broker.CreateTask(projectID, "coder", "Title", "MD")
	broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)
	broker.SolveTask(projectID, taskID, "done")

	// Task is removed from memory after solve — progress_task should fail
	err := broker.ReportProgress(projectID, taskID, "too late")
	if err == nil {
		t.Error("Expected error for progress after solve")
	}
}
