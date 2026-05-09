package main

import (
	"context"
	"testing"
	"time"
)

type awaitResult struct {
	status   string
	result   string
	progress []string
	err      error
}

func TestBroker_GetTaskMD_And_Result(t *testing.T) {
	broker := newTestBroker(t, true, true)

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

	// Solve task (need to pick it first)
	broker.ListenRole(context.Background(), projectID, "role1", "poll", 0)
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

	// Test GetTaskResult nonexistent task
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
	broker := newTestBroker(t, true, true)

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
	broker := newTestBroker(t, true, true)

	projectID := "default"
	taskID, _ := broker.CreateTask(projectID, "coder", "Title", "MD")
	broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)

	if err := broker.ReportProgress(projectID, taskID, "step 1 done"); err != nil {
		t.Fatalf("ReportProgress failed: %v", err)
	}
	if err := broker.ReportProgress(projectID, taskID, "step 2 done"); err != nil {
		t.Fatalf("ReportProgress failed: %v", err)
	}

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
	broker := newTestBroker(t, true, true)

	err := broker.ReportProgress("default", "nonexistent", "hello")
	if err == nil {
		t.Error("Expected error for nonexistent task")
	}
}

func TestBroker_ReportProgress_AfterSolve(t *testing.T) {
	broker := newTestBroker(t, true, true)

	projectID := "default"
	taskID, _ := broker.CreateTask(projectID, "coder", "Title", "MD")
	broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)
	broker.SolveTask(projectID, taskID, "done")

	err := broker.ReportProgress(projectID, taskID, "too late")
	if err == nil {
		t.Error("Expected error for progress after solve")
	}
}

func TestBroker_AdminUpdateStatus_RequeuePickedTaskKeepsTaskActive(t *testing.T) {
	broker := newTestBroker(t, true, true)

	projectID := "default"
	taskID, err := broker.CreateTask(projectID, "coder", "Title", "task content")
	if err != nil {
		t.Fatal(err)
	}

	if _, status, err := broker.ListenRole(context.Background(), projectID, "coder", "poll", 0); err != nil || status != "picked" {
		t.Fatalf("ListenRole(poll) failed: status=%s err=%v", status, err)
	}
	if err := broker.ReportProgress(projectID, taskID, "first attempt"); err != nil {
		t.Fatalf("ReportProgress before requeue failed: %v", err)
	}

	resultCh := make(chan awaitResult, 1)
	go func() {
		status, result, progress, err := broker.AwaitTask(context.Background(), projectID, taskID, 0)
		resultCh <- awaitResult{status: status, result: result, progress: progress, err: err}
	}()
	time.Sleep(20 * time.Millisecond)

	if err := broker.AdminUpdateStatus(projectID, taskID, "queued"); err != nil {
		t.Fatalf("AdminUpdateStatus(queued) failed: %v", err)
	}

	meta, err := broker.GetTaskStatus(projectID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != StatusQueued {
		t.Fatalf("Expected status queued after admin reset, got %s", meta.Status)
	}

	task, status, err := broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)
	if err != nil || task == nil || status != "picked" {
		t.Fatalf("Expected requeued task to be picked again, got task=%v status=%s err=%v", task, status, err)
	}
	if task.MD != "task content" {
		t.Fatalf("Expected requeued task_md to be preserved, got %q", task.MD)
	}

	if err := broker.ReportProgress(projectID, taskID, "second attempt"); err != nil {
		t.Fatalf("ReportProgress after requeue failed: %v", err)
	}
	if err := broker.SolveTask(projectID, taskID, "final result"); err != nil {
		t.Fatalf("SolveTask after requeue failed: %v", err)
	}

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("AwaitTask returned error: %v", got.err)
		}
		if got.status != string(StatusSolved) {
			t.Fatalf("Expected solved status, got %s", got.status)
		}
		if got.result != "final result" {
			t.Fatalf("Expected final result, got %q", got.result)
		}
		if len(got.progress) != 2 {
			t.Fatalf("Expected 2 progress messages, got %d: %v", len(got.progress), got.progress)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AwaitTask hung after admin requeue")
	}
}

func TestBroker_AdminUpdateStatus_RequeueSolvedTaskRestoresTask(t *testing.T) {
	broker := newTestBroker(t, true, true)

	projectID := "default"
	taskID, err := broker.CreateTask(projectID, "coder", "Title", "task content")
	if err != nil {
		t.Fatal(err)
	}

	if _, status, err := broker.ListenRole(context.Background(), projectID, "coder", "poll", 0); err != nil || status != "picked" {
		t.Fatalf("ListenRole(poll) failed: status=%s err=%v", status, err)
	}
	if err := broker.SolveTask(projectID, taskID, "done"); err != nil {
		t.Fatalf("SolveTask failed: %v", err)
	}

	if err := broker.AdminUpdateStatus(projectID, taskID, "queued"); err != nil {
		t.Fatalf("AdminUpdateStatus(queued) failed: %v", err)
	}

	task, status, err := broker.ListenRole(context.Background(), projectID, "coder", "poll", 0)
	if err != nil || task == nil || status != "picked" {
		t.Fatalf("Expected solved task to be requeued and picked, got task=%v status=%s err=%v", task, status, err)
	}
	if task.MD != "task content" {
		t.Fatalf("Expected task_md to be restored after solved->queued, got %q", task.MD)
	}
	if err := broker.ReportProgress(projectID, taskID, "retry"); err != nil {
		t.Fatalf("Expected requeued solved task to be tracked in memory, got %v", err)
	}
}

func TestBroker_ListTasks_OffsetWithoutLimit(t *testing.T) {
	broker := newTestBroker(t, true, true)

	for i := 0; i < 3; i++ {
		if _, err := broker.CreateTask(testProject, "coder", "Title", "MD"); err != nil {
			t.Fatalf("CreateTask failed: %v", err)
		}
	}

	tasks, err := broker.ListTasks(testProject, "", "", 0, 1)
	if err != nil {
		t.Fatalf("ListTasks with offset only failed: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("Expected 2 tasks after offsetting one row, got %d", len(tasks))
	}
}
