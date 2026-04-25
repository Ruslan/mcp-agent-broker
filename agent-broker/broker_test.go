package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testProject = "default"

func TestBroker_AsyncLifecycle(t *testing.T) {
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

	// 1. Create Async
	taskID, err := broker.CreateTaskAsync(testProject, role, title, taskMD)
	if err != nil {
		t.Fatalf("CreateTaskAsync failed: %v", err)
	}

	// 2. Get Status (Queued)
	status, err := broker.GetTaskStatus(testProject, taskID)
	if err != nil {
		t.Fatalf("GetTaskStatus failed: %v", err)
	}
	if status.Status != StatusQueued {
		t.Errorf("Expected status queued, got %v", status.Status)
	}
	if status.Title != title {
		t.Errorf("Expected title %v, got %v", title, status.Title)
	}
	if status.ProjectID != testProject {
		t.Errorf("Expected project %v, got %v", testProject, status.ProjectID)
	}

	// 3. Listen Async (Picked)
	task, found, err := broker.ListenRoleAsync(testProject, role)
	if err != nil || !found {
		t.Fatalf("ListenRoleAsync failed: found=%v, err=%v", found, err)
	}
	if task.ID != taskID {
		t.Errorf("Expected task ID %v, got %v", taskID, task.ID)
	}
	if task.Title != title {
		t.Errorf("Expected title %v, got %v", title, task.Title)
	}

	status, _ = broker.GetTaskStatus(testProject, taskID)
	if status.Status != StatusPicked {
		t.Errorf("Expected status picked, got %v", status.Status)
	}

	// 4. Get Result (Not solved)
	res, err := broker.GetTaskResult(testProject, taskID)
	if err != nil {
		t.Fatalf("GetTaskResult failed: %v", err)
	}
	if res != "" {
		t.Errorf("Expected empty result, got %v", res)
	}

	// 5. Solve Task (Solved)
	err = broker.SolveTask(testProject, taskID, resultMD)
	if err != nil {
		t.Fatalf("SolveTask failed: %v", err)
	}

	status, _ = broker.GetTaskStatus(testProject, taskID)
	if status.Status != StatusSolved {
		t.Errorf("Expected status solved, got %v", status.Status)
	}

	res, _ = broker.GetTaskResult(testProject, taskID)
	if res != resultMD {
		t.Errorf("Expected result %v, got %v", resultMD, res)
	}
}

func TestBroker_AsyncVisibleToSync(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-v0.0.4.1-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	title := "Async task visible to sync"
	taskMD := "Content"

	// 1. Create async task
	taskID, err := broker.CreateTaskAsync(testProject, role, title, taskMD)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Consume via ListenRoleSync
	task, err := broker.ListenRoleSync(context.Background(), testProject, role)
	if err != nil {
		t.Fatalf("ListenRoleSync failed: %v", err)
	}
	if task.ID != taskID {
		t.Errorf("Expected task ID %s, got %s", taskID, task.ID)
	}
	if task.Title != title {
		t.Errorf("Expected title %s, got %s", title, task.Title)
	}

	// 3. Verify status
	status, _ := broker.GetTaskStatus(testProject, taskID)
	if status.Status != StatusPicked {
		t.Errorf("Expected status picked, got %v", status.Status)
	}

	// 4. Verify async queue is empty
	_, found, _ := broker.ListenRoleAsync(testProject, role)
	if found {
		t.Error("Task should have been removed from async queue")
	}

	// 5. Solve should still work
	err = broker.SolveTask(testProject, taskID, "Done")
	if err != nil {
		t.Errorf("SolveTask failed: %v", err)
	}
}

func TestBroker_ListenRoleSync_AsyncStatusFailure(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-status-fail-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	_, _ = broker.CreateTaskAsync(testProject, role, "Title", "Test Task")

	// Simulate status failure
	broker.persistStatus = func(projectID, taskID, role, title string, status TaskStatus) error {
		return fmt.Errorf("disk full")
	}

	_, err := broker.ListenRoleSync(context.Background(), testProject, role)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Expected status failure error, got %v", err)
	}

	// Verify task is still in queue
	broker.persistStatus = broker.defaultPersistStatus
	task, err := broker.ListenRoleSync(context.Background(), testProject, role)
	if err != nil || task == nil {
		t.Fatalf("Expected to recover task after status success, got %v", err)
	}
}

func TestBroker_CreateTaskSync_IDCollisionRetry(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-sync-collision-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	count := 0
	broker.generateID = func() string {
		count++
		if count <= 2 {
			return "constant-id"
		}
		return "new-id"
	}

	role := "coder"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Pre-create a task directory/file to cause collision in persistTask
	collisionDir := filepath.Join(tmpDir, testProject, "constant-id")
	os.MkdirAll(collisionDir, 0755)
	os.WriteFile(filepath.Join(collisionDir, "task.md"), []byte("I exist"), 0644)

	// 2. Start listener
	taskCh := make(chan *Task, 1)
	go func() {
		task, _ := broker.ListenRoleSync(ctx, testProject, role)
		if task != nil {
			taskCh <- task
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// 3. CreateTaskSync - should retry and get "new-id"
	syncTaskIDCh := make(chan string, 1)
	go func() {
		id, _, _ := broker.CreateTaskSync(ctx, testProject, role, "Title", "MD")
		syncTaskIDCh <- id
	}()

	var taskID string
	select {
	case task := <-taskCh:
		if task.ID != "new-id" {
			t.Errorf("Expected task ID new-id, got %s", task.ID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for task")
	}

	select {
	case taskID = <-syncTaskIDCh:
	case <-time.After(100 * time.Millisecond):
	}

	if count != 3 {
		t.Errorf("Expected 3 calls to generateID, got %d", count)
	}

	if taskID != "" {
		broker.SolveTask(testProject, taskID, "Done")
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

func TestBroker_DuplicateCreate_Sync(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-dup-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	role := "coder"
	title := "Sync Task"
	
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start a listener
	taskCh := make(chan *Task, 1)
	go func() {
		task, err := broker.ListenRoleSync(ctx, testProject, role)
		if err == nil {
			taskCh <- task
		}
	}()

	// Wait for listener to register
	time.Sleep(100 * time.Millisecond)

	// Create the task (it will block until solved)
	var taskID string
	go func() {
		id, _, _ := broker.CreateTaskSync(ctx, testProject, role, title, "MD 1")
		taskID = id
	}()

	// Wait for delivery
	select {
	case task := <-taskCh:
		if task.Title != title {
			t.Errorf("Expected title %s, got %s", title, task.Title)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for task delivery")
	}

	time.Sleep(50 * time.Millisecond)
	broker.SolveTask(testProject, taskID, "Done")
}

func TestBroker_IDCollisionRetry(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-collision-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	count := 0
	broker.generateID = func() string {
		count++
		if count <= 2 {
			return "constant-id"
		}
		return "new-id"
	}

	// First creation
	_, err := broker.CreateTaskAsync(testProject, "role", "Title", "MD")
	if err != nil {
		t.Fatal(err)
	}

	// Second creation - should collide and retry
	id, err := broker.CreateTaskAsync(testProject, "role", "Title 2", "MD 2")
	if err != nil {
		t.Fatal(err)
	}
	if id != "new-id" {
		t.Errorf("Expected retry to generate new-id, got %s", id)
	}
	if count != 3 {
		t.Errorf("Expected 3 calls to generateID, got %d", count)
	}
}

func TestBroker_PartialCleanup(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-cleanup-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	targetID := "failed-id"
	broker.generateID = func() string { return targetID }
	broker.persistStatus = func(projectID, taskID, role, title string, status TaskStatus) error {
		return fmt.Errorf("simulated persistStatus failure")
	}

	_, err := broker.CreateTaskAsync(testProject, "role", "Title", "MD")
	if err == nil {
		t.Fatal("Expected error from CreateTaskAsync")
	}
	
	dir := filepath.Join(tmpDir, testProject, targetID)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Expected directory %s to be removed, but it exists", dir)
	}
}

func TestBroker_DuplicateSolve(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-solve-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	taskID, _ := broker.CreateTaskAsync(testProject, "role", "Title", "MD")
	
	err := broker.SolveTask(testProject, taskID, "Result 1")
	if err != nil {
		t.Fatal(err)
	}

	err = broker.SolveTask(testProject, taskID, "Result 2")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Expected not found error on duplicate solve, got %v", err)
	}

	// Verify file was not overwritten
	data, _ := os.ReadFile(filepath.Join(tmpDir, testProject, taskID, "result.md"))
	if string(data) != "Result 1" {
		t.Errorf("Expected original result, got %s", string(data))
	}
}

func TestBroker_TitleValidation(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-title-test-*")
	defer os.RemoveAll(tmpDir)
	broker, _ := NewBroker(tmpDir, true, true)

	// Overlong title
	longTitle := strings.Repeat("a", 201)
	_, err := broker.CreateTaskAsync(testProject, "role", longTitle, "MD")
	if err == nil || !strings.Contains(longTitle, "too long") && !strings.Contains(err.Error(), "too long") {
		t.Errorf("Expected too long error, got %v", err)
	}

	// Empty title
	_, err = broker.CreateTaskAsync(testProject, "role", "", "MD")
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("Expected required error, got %v", err)
	}
}
