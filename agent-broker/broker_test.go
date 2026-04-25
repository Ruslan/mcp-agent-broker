package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const testProject = "default"

func TestBroker_Lifecycle(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "broker-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	broker, err := NewBroker(tmpDir, true, true)
	if err != nil {
		t.Fatal(err)
	}

	role := "coder"
	title := "Test Title"
	taskMD := "Test Task"
	resultMD := "Test Result"

	// 1. Create Task
	taskID, err := broker.CreateTask(testProject, role, title, taskMD)
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	// 2. Poll for Task (should be picked)
	task, status, err := broker.ListenRole(context.Background(), testProject, role, "poll", 0)
	if err != nil || task == nil {
		t.Fatalf("ListenRole(poll) failed: task=%v, status=%v, err=%v", task, status, err)
	}
	if task.ID != taskID || status != "picked" {
		t.Errorf("Expected picked task, got %v, status %s", task, status)
	}

	// 3. Await Task (Not solved, should timeout)
	status, res, err := broker.AwaitTask(context.Background(), testProject, taskID, 50)
	if err != nil || status != string(StatusPicked) || res != "" {
		t.Errorf("AwaitTask unexpected: status=%v, res=%v, err=%v", status, res, err)
	}

	// 4. Solve Task
	err = broker.SolveTask(testProject, taskID, resultMD)
	if err != nil {
		t.Fatalf("SolveTask failed: %v", err)
	}

	// 5. Await Task (Solved)
	status, res, err = broker.AwaitTask(context.Background(), testProject, taskID, 50)
	if err != nil || status != string(StatusSolved) || res != resultMD {
		t.Errorf("AwaitTask(solved) unexpected: status=%v, res=%v, err=%v", status, res, err)
	}
}

func TestBroker_LifecycleStatus(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-status-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	
	// Create task without listener -> should stay queued
	taskID, _ := broker.CreateTask(testProject, role, "Queued Task", "MD")
	
	meta, _ := broker.GetTaskStatus(testProject, taskID)
	if meta.Status != StatusQueued {
		t.Errorf("Expected status queued when no listener, got %v", meta.Status)
	}

	// Start listener and poll
	_, status, _ := broker.ListenRole(context.Background(), testProject, role, "poll", 0)
	if status != "picked" {
		t.Errorf("Expected picked status on poll, got %s", status)
	}

	meta, _ = broker.GetTaskStatus(testProject, taskID)
	if meta.Status != StatusPicked {
		t.Errorf("Expected status picked after delivery, got %v", meta.Status)
	}
}

func TestBroker_CreateTask_StatusConsistency(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-consist-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	
	// 1. Occupy the listener
	// ListenRole registers a listener and blocks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	go func() {
		broker.ListenRole(ctx, testProject, role, "wait", 5000)
	}()
	time.Sleep(100 * time.Millisecond) // Let listener register

	// 2. Deliver first task (fills buffer of size 1)
	task1ID, _ := broker.CreateTask(testProject, role, "Task 1", "MD")
	
	meta1, _ := broker.GetTaskStatus(testProject, task1ID)
	if meta1.Status != StatusPicked {
		t.Errorf("Expected task 1 to be picked, got %v", meta1.Status)
	}

	// 3. Deliver second task (listener is busy, buffer full)
	// This should hit 'default' case, rollback picked -> queued, and go to asyncQueue
	task2ID, _ := broker.CreateTask(testProject, role, "Task 2", "MD")

	meta2, _ := broker.GetTaskStatus(testProject, task2ID)
	if meta2.Status != StatusQueued {
		t.Errorf("Expected task 2 to rollback to queued, got %v", meta2.Status)
	}
}

func TestBroker_CreateTask_StatusFailure(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-stat-fail-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	
	// Start listener
	go func() {
		broker.ListenRole(context.Background(), testProject, role, "wait", 1000)
	}()
	time.Sleep(100 * time.Millisecond)

	// Simulate failure ONLY for StatusPicked
	broker.persistStatus = func(projectID, taskID, role, title string, status TaskStatus) error {
		if status == StatusPicked {
			return fmt.Errorf("disk error during pick")
		}
		return broker.defaultPersistStatus(projectID, taskID, role, title, status)
	}

	_, err := broker.CreateTask(testProject, role, "Failed Task", "MD")
	if err == nil || !strings.Contains(err.Error(), "disk error during pick") {
		t.Errorf("Expected disk error, got %v", err)
	}
}

func TestBroker_WaitDelivery(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-wait-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start listener
	taskCh := make(chan *Task, 1)
	go func() {
		task, _, _ := broker.ListenRole(ctx, testProject, role, "wait", 1000)
		if task != nil {
			taskCh <- task
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// Create task
	taskID, _ := broker.CreateTask(testProject, role, "Title", "MD")

	select {
	case task := <-taskCh:
		if task.ID != taskID {
			t.Errorf("Expected task ID %s, got %s", taskID, task.ID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for task")
	}

	meta, _ := broker.GetTaskStatus(testProject, taskID)
	if meta.Status != StatusPicked {
		t.Errorf("Expected status picked after wait delivery, got %v", meta.Status)
	}
}

func TestBroker_PollEmpty(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-poll-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	task, status, err := broker.ListenRole(context.Background(), testProject, "coder", "poll", 0)
	if err != nil || task != nil || status != "empty" {
		t.Errorf("Expected empty poll, got task=%v, status=%s, err=%v", task, status, err)
	}
}

func TestBroker_PathSafety(t *testing.T) {
	if isSafeID("../unsafe") {
		t.Error("isSafeID should reject ../unsafe")
	}
	if isSafeID(".") {
		t.Error("isSafeID should reject .")
	}
	if isSafeID("safe-id-123") == false {
		t.Error("isSafeID should accept safe-id-123")
	}
}

func TestBroker_DuplicateSolve(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-solve-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	taskID, _ := broker.CreateTask(testProject, "role", "Title", "MD")
	
	broker.ListenRole(context.Background(), testProject, "role", "poll", 0)
	err := broker.SolveTask(testProject, taskID, "Result 1")
	if err != nil {
		t.Fatal(err)
	}

	err = broker.SolveTask(testProject, taskID, "Result 2")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Expected not found error on duplicate solve, got %v", err)
	}
}

func TestBroker_TitleValidation(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-title-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	longTitle := strings.Repeat("a", 201)
	_, err := broker.CreateTask(testProject, "role", longTitle, "MD")
	if err == nil || !strings.Contains(longTitle, "too long") && !strings.Contains(err.Error(), "too long") {
		t.Errorf("Expected too long error, got %v", err)
	}

	_, err = broker.CreateTask(testProject, "role", "", "MD")
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("Expected required error, got %v", err)
	}
}

func TestBroker_ListTasks(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-list-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	broker.CreateTask(testProject, "coder", "Task 1", "MD")
	broker.CreateTask(testProject, "reviewer", "Task 2", "MD")

	tasks, _ := broker.ListTasks(testProject, "", "")
	if len(tasks) != 2 {
		t.Errorf("Expected 2 tasks, got %d", len(tasks))
	}

	tasks, _ = broker.ListTasks(testProject, "coder", "")
	if len(tasks) != 1 || tasks[0].Role != "coder" {
		t.Errorf("Expected 1 coder task, got %d", len(tasks))
	}
}
