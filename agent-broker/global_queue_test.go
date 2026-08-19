package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodedToolPayload(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("tool result has type %T", value)
	}
	if content, ok := result["content"].([]any); ok {
		text := content[0].(map[string]any)["text"].(string)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	return result
}

func TestGlobalQueueCrossProjectCapabilityLifecycle(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	ctx := context.Background()

	created, err := handler.handleToolCall(ctx, "project-a", "", "create_task", json.RawMessage(`{"role":"g:shared-lib:maintainer","title":"Fix","task_md":"Do it"}`))
	if err != nil {
		t.Fatal(err)
	}
	taskID := decodedToolPayload(t, created)["task_id"].(string)

	delivered, err := handler.handleToolCall(ctx, "project-b", "", "listen_role", json.RawMessage(`{"role":"g:shared-lib:maintainer","mode":"poll"}`))
	if err != nil {
		t.Fatal(err)
	}
	task := decodedToolPayload(t, delivered)["task"].(map[string]any)
	workToken, _ := task["work_token"].(string)
	if task["task_id"] != taskID || workToken == "" {
		t.Fatalf("global delivery missing task capability: %v", task)
	}
	readArgs, _ := json.Marshal(map[string]string{"task_id": taskID, "work_token": workToken})
	readBack, err := handler.handleToolCall(ctx, "capability-only-project", "", "get_task", readArgs)
	if err != nil || readBack.(map[string]any)["task_md"] != "Do it" {
		t.Fatalf("work_token did not authorize get_task: response=%v err=%v", readBack, err)
	}

	aTasks, _ := broker.ListTasks("project-a", "", "", 20, 0)
	bTasks, _ := broker.ListTasks("project-b", "", "", 20, 0)
	if len(aTasks) != 1 || aTasks[0].TaskID != taskID || len(bTasks) != 0 {
		t.Fatalf("ownership changed during global delivery: A=%v B=%v", aTasks, bTasks)
	}
	if _, err := broker.GetTaskStatus("project-b", taskID); err == nil {
		t.Fatal("listener project unexpectedly gained task visibility")
	}

	if _, err := handler.handleToolCall(ctx, "project-c", "", "progress_task", json.RawMessage(`{"task_id":"`+taskID+`","message":"not assigned"}`)); err == nil {
		t.Fatal("unassigned project unexpectedly progressed global task without work_token")
	}

	otherID, err := broker.CreateTask("project-c", "g:other:queue", "Other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := broker.ListenRole(ctx, "project-b", "g:other:queue", "poll", 0)
	if err != nil || other == nil || other.ID != otherID {
		t.Fatalf("failed to pick token-mismatch fixture: %v %v", other, err)
	}
	badArgs, _ := json.Marshal(map[string]string{"task_id": taskID, "message": "wrong token", "work_token": other.workToken})
	if _, err := handler.handleToolCall(ctx, "project-b", "", "progress_task", badArgs); err == nil || err.Error() != "invalid work_token" {
		t.Fatalf("mismatched token should fail generically, got %v", err)
	}

	progressArgs, _ := json.Marshal(map[string]string{"task_id": taskID, "message": "halfway", "work_token": workToken})
	if _, err := handler.handleToolCall(ctx, "project-b", "", "progress_task", progressArgs); err != nil {
		t.Fatalf("capability progress failed: %v", err)
	}
	solveArgs, _ := json.Marshal(map[string]string{"task_id": taskID, "result_md": "done", "work_token": workToken})
	if _, err := handler.handleToolCall(ctx, "project-b", "", "solve_task", solveArgs); err != nil {
		t.Fatalf("capability solve failed: %v", err)
	}
	status, result, _, err := broker.AwaitTask(ctx, "project-a", taskID, 10)
	if err != nil || status != string(StatusSolved) || result != "done" {
		t.Fatalf("owner did not receive result: status=%s result=%q err=%v", status, result, err)
	}
	progress, _ := broker.GetTaskProgress("project-a", taskID)
	if len(progress) != 1 || progress[0] != "halfway" {
		t.Fatalf("owner did not receive progress: %v", progress)
	}
}

func TestGlobalQueueAddressesRemainIsolated(t *testing.T) {
	broker := newTestBroker(t, true, true)
	ctx := context.Background()

	globalID, _ := broker.CreateTask("project-a", "g:key-one:queue", "G", "G")
	if task, status, err := broker.ListenRole(ctx, "project-b", "g:key-two:queue", "poll", 0); err != nil || task != nil || status != "empty" {
		t.Fatalf("different global key crossed queues: task=%v status=%s err=%v", task, status, err)
	}
	if task, _, _ := broker.ListenRole(ctx, "project-b", "g:key-one:other", "poll", 0); task != nil {
		t.Fatal("different global queue name crossed queues")
	}
	if task, _, err := broker.ListenRole(ctx, "project-b", "g:key-one:queue", "poll", 0); err != nil || task == nil || task.ID != globalID {
		t.Fatalf("matching global address did not route: task=%v err=%v", task, err)
	}

	localA, _ := broker.CreateTask("project-a", "coder", "A", "A")
	localB, _ := broker.CreateTask("project-b", "coder", "B", "B")
	gotB, _, _ := broker.ListenRole(ctx, "project-b", "coder", "poll", 0)
	gotA, _, _ := broker.ListenRole(ctx, "project-a", "coder", "poll", 0)
	if gotA == nil || gotA.ID != localA || gotB == nil || gotB.ID != localB {
		t.Fatalf("local isolation regressed: A=%v B=%v", gotA, gotB)
	}
}

func TestGlobalQueueBlockingWaitCarriesCapability(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := handler.handleToolCall(context.Background(), "project-b", "", "listen_role", json.RawMessage(`{"role":"g:shared:waiter","mode":"wait","timeout_ms":2000}`))
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	addr := addressFor("project-b", "g:shared:waiter")
	deadline := time.Now().Add(time.Second)
	for {
		broker.mu.Lock()
		_, ready := broker.listeners[addr]
		broker.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("global blocking listener did not register")
		}
		time.Sleep(time.Millisecond)
	}
	taskID, err := broker.CreateTask("project-a", "g:shared:waiter", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case result := <-resultCh:
		task := decodedToolPayload(t, result)["task"].(map[string]any)
		if task["task_id"] != taskID || task["work_token"] == "" {
			t.Fatalf("blocking global delivery lost capability: %v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking global delivery timed out")
	}
}

func TestGlobalQueueRolePollAndAdminDoNotExposeCapability(t *testing.T) {
	broker := newTestBroker(t, true, true)
	roleToken, err := broker.MintPollToken("project-b", PollScopeRole, "g:shared:worker")
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := broker.CreateTask("project-a", "g:shared:worker", "T", "M")

	var logs bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(originalWriter)

	_, body := pollHTTP(t, &PollHandler{broker: broker}, roleToken.Token)
	task := body["task"].(map[string]any)
	workToken, _ := task["work_token"].(string)
	if task["task_id"] != taskID || workToken == "" {
		t.Fatalf("role poll did not carry global work_token: %v", body)
	}

	admin := &AdminHandler{broker: broker}
	for _, path := range []string{
		"/admin/api/tasks?project=project-a",
		"/admin/api/tasks/" + taskID + "?project=project-a",
	} {
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(w.Body.String(), workToken) || strings.Contains(w.Body.String(), "work_token") {
			t.Fatalf("admin response exposed work capability: %s", w.Body.String())
		}
	}
	if strings.Contains(logs.String(), workToken) {
		t.Fatal("logs exposed work capability material")
	}
}

func TestGlobalQueueRestartRestoresRoutingAndCapability(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "broker.db")
	promptsDir := t.TempDir()
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	broker1, err := NewBroker(store1, promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := broker1.CreateTask("project-a", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	broker2, err := NewBroker(store2, promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := broker2.ListenRole(context.Background(), "project-b", "g:shared:worker", "poll", 0)
	if err != nil || task == nil || task.ID != taskID || task.ProjectID != "project-a" || task.workToken == "" {
		t.Fatalf("restart recovery lost global routing/capability: task=%+v err=%v", task, err)
	}

	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	store3, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	broker3, err := NewBroker(store3, promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := broker3.ResolveWorkerProject("project-b", taskID, task.workToken)
	if err != nil || owner != "project-a" {
		t.Fatalf("work capability did not survive restart: owner=%q err=%v", owner, err)
	}
	if err := broker3.SolveTask(owner, taskID, "done after restart"); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalQueueResponseWriteFailureRequeuesToOwner(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"listen_role","arguments":{"role":"g:shared:worker","mode":"wait","timeout_ms":5000}},"id":1}`)))
	req.Header.Set("X-Project-Id", "project-b")
	w := &failingResponseWriter{header: make(http.Header)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	addr := addressFor("project-b", "g:shared:worker")
	deadline := time.Now().Add(time.Second)
	for {
		broker.mu.Lock()
		_, ready := broker.listeners[addr]
		broker.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("global listener did not register")
		}
		time.Sleep(time.Millisecond)
	}

	taskID, err := broker.CreateTask("project-a", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed response did not return")
	}
	meta, err := broker.GetTaskStatus("project-a", taskID)
	if err != nil || meta.Status != StatusQueued || meta.WorkerProjectID != "" {
		t.Fatalf("task was not requeued under owner: meta=%v err=%v", meta, err)
	}
	redelivered, _, err := broker.ListenRole(context.Background(), "project-c", "g:shared:worker", "poll", 0)
	if err != nil || redelivered == nil || redelivered.ID != taskID || redelivered.workToken == "" {
		t.Fatalf("global redelivery failed: task=%+v err=%v", redelivered, err)
	}
}

func TestWorkTokenInvalidExpiredAndDeletedAreIndistinguishable(t *testing.T) {
	broker := newTestBroker(t, true, true)
	taskID, err := broker.CreateTask("project-a", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	expiredAt := time.Now().UTC().Add(-time.Minute)
	if err := broker.store.InsertWorkToken("expired-work-token", "project-a", taskID, expiredAt.Add(-time.Hour), expiredAt); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"unknown-work-token", "expired-work-token"} {
		if _, err := broker.ResolveWorkerProject("project-b", taskID, token); err == nil || err.Error() != "invalid work_token" {
			t.Fatalf("token %q did not fail generically: %v", token, err)
		}
	}

	task, _, err := broker.ListenRole(context.Background(), "project-b", "g:shared:worker", "poll", 0)
	if err != nil || task == nil || task.workToken == "" {
		t.Fatalf("failed to obtain live capability: task=%+v err=%v", task, err)
	}
	if err := broker.DeleteTask("project-a", taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.ResolveWorkerProject("project-b", taskID, task.workToken); err == nil || err.Error() != "invalid work_token" {
		t.Fatalf("deleted-task capability remained live: %v", err)
	}
}

func TestExistingDatabaseMigratesWorkTokens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (
		project_id TEXT NOT NULL, task_id TEXT NOT NULL, role TEXT NOT NULL,
		title TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', task_md TEXT NOT NULL,
		result_md TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		PRIMARY KEY (project_id, task_id)
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("automatic migration failed: %v", err)
	}
	defer store.Close()
	var tableName string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='work_tokens'`).Scan(&tableName); err != nil || tableName != "work_tokens" {
		t.Fatalf("work_tokens migration missing: name=%q err=%v", tableName, err)
	}
	var workerColumn string
	if err := store.db.QueryRow(`SELECT name FROM pragma_table_info('tasks') WHERE name='worker_project_id'`).Scan(&workerColumn); err != nil || workerColumn != "worker_project_id" {
		t.Fatalf("worker assignment migration missing: name=%q err=%v", workerColumn, err)
	}
}

func TestGlobalWorkerAssignmentSurvivesRestartAndRestoresAccess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "broker.db")
	promptsDir := t.TempDir()
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	broker1, err := NewBroker(store1, promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := broker1.CreateTask("project-owner", "g:bench:reviewer", "Recover me", "full durable task body")
	if err != nil {
		t.Fatal(err)
	}
	delivered, _, err := broker1.ListenRole(context.Background(), "worker-project-b", "g:bench:reviewer", "poll", 0)
	if err != nil || delivered == nil || delivered.ID != taskID {
		t.Fatalf("initial global claim failed: task=%+v err=%v", delivered, err)
	}
	meta, err := broker1.GetTaskStatus("project-owner", taskID)
	if err != nil || meta.WorkerProjectID != "worker-project-b" {
		t.Fatalf("worker assignment was not persisted: meta=%+v err=%v", meta, err)
	}

	// Fill the worker's own recent-task window. The active assigned task must
	// still sort first instead of disappearing behind these rows.
	for i := 0; i < defaultListTasksLimit+5; i++ {
		if _, err := broker1.CreateTask("worker-project-b", "local", fmt.Sprintf("local-%02d", i), "local body"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	broker2, err := NewBroker(store2, promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}
	handler := &JSONRPCHandler{broker: broker2}
	ctx := context.Background()

	listed, err := handler.handleToolCall(ctx, "worker-project-b", "", "list_tasks", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	tasks := listed.(map[string]any)["tasks"].([]StatusMetadata)
	if len(tasks) == 0 || tasks[0].TaskID != taskID || tasks[0].ProjectID != "project-owner" || tasks[0].WorkerProjectID != "worker-project-b" {
		t.Fatalf("assigned task was not prioritized in worker list: first=%+v", tasks)
	}

	got, err := handler.handleToolCall(ctx, "worker-project-b", "", "get_task", json.RawMessage(`{"task_id":"`+taskID+`"}`))
	if err != nil {
		t.Fatalf("assigned worker could not reread task: %v", err)
	}
	if body := got.(map[string]any)["task_md"]; body != "full durable task body" {
		t.Fatalf("recovered wrong task body: %v", body)
	}
	if _, err := handler.handleToolCall(ctx, "other-worker", "", "get_task", json.RawMessage(`{"task_id":"`+taskID+`"}`)); err == nil {
		t.Fatal("unassigned project unexpectedly read global task")
	}

	if _, err := handler.handleToolCall(ctx, "worker-project-b", "", "progress_task", json.RawMessage(`{"task_id":"`+taskID+`","message":"recovered"}`)); err != nil {
		t.Fatalf("assigned worker could not report progress without token: %v", err)
	}
	if _, err := handler.handleToolCall(ctx, "worker-project-b", "", "solve_task", json.RawMessage(`{"task_id":"`+taskID+`","result_md":"done after restart"}`)); err != nil {
		t.Fatalf("assigned worker could not solve without token: %v", err)
	}
	status, result, _, err := broker2.AwaitTask(ctx, "project-owner", taskID, 10)
	if err != nil || status != string(StatusSolved) || result != "done after restart" {
		t.Fatalf("owner did not receive recovered result: status=%s result=%q err=%v", status, result, err)
	}
}

func TestGlobalWorkerAssignmentMovesOnRequeue(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}
	ctx := context.Background()
	taskID, err := broker.CreateTask("project-owner", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	if task, _, err := broker.ListenRole(ctx, "worker-one", "g:shared:worker", "poll", 0); err != nil || task == nil {
		t.Fatalf("first claim failed: task=%+v err=%v", task, err)
	}
	if err := broker.AdminUpdateStatus("project-owner", taskID, string(StatusQueued)); err != nil {
		t.Fatal(err)
	}
	meta, err := broker.GetTaskStatus("project-owner", taskID)
	if err != nil || meta.WorkerProjectID != "" {
		t.Fatalf("requeue did not clear assignment: meta=%+v err=%v", meta, err)
	}
	if _, err := handler.handleToolCall(ctx, "worker-one", "", "get_task", json.RawMessage(`{"task_id":"`+taskID+`"}`)); err == nil {
		t.Fatal("old worker retained assignment access after requeue")
	}
	if task, _, err := broker.ListenRole(ctx, "worker-two", "g:shared:worker", "poll", 0); err != nil || task == nil {
		t.Fatalf("second claim failed: task=%+v err=%v", task, err)
	}
	meta, err = broker.GetTaskStatus("project-owner", taskID)
	if err != nil || meta.WorkerProjectID != "worker-two" {
		t.Fatalf("new assignment was not persisted: meta=%+v err=%v", meta, err)
	}
	if _, err := handler.handleToolCall(ctx, "worker-two", "", "solve_task", json.RawMessage(`{"task_id":"`+taskID+`","result_md":"done"}`)); err != nil {
		t.Fatalf("new assigned worker could not solve: %v", err)
	}
}

func TestGlobalBlockingWaitPersistsWorkerAssignment(t *testing.T) {
	broker := newTestBroker(t, true, true)
	resultCh := make(chan *Task, 1)
	errCh := make(chan error, 1)
	go func() {
		task, _, err := broker.ListenRole(context.Background(), "worker-project-b", "g:shared:wait", "wait", 2000)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- task
	}()

	addr := addressFor("worker-project-b", "g:shared:wait")
	deadline := time.Now().Add(time.Second)
	for {
		broker.mu.Lock()
		_, ready := broker.listeners[addr]
		broker.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not register")
		}
		time.Sleep(time.Millisecond)
	}
	taskID, err := broker.CreateTask("project-owner", "g:shared:wait", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case task := <-resultCh:
		if task == nil || task.ID != taskID {
			t.Fatalf("wrong delivered task: %+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("wait delivery timed out")
	}
	meta, err := broker.GetTaskStatus("project-owner", taskID)
	if err != nil || meta.WorkerProjectID != "worker-project-b" {
		t.Fatalf("blocking wait assignment missing: meta=%+v err=%v", meta, err)
	}
}

func TestGlobalQueueDeliveryRotatesNearExpiryWorkToken(t *testing.T) {
	broker := newTestBroker(t, true, true)
	taskID, err := broker.CreateTask("project-a", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	store := broker.store.(*SQLiteStore)
	if _, err := store.db.Exec(`DELETE FROM work_tokens WHERE project_id = ? AND task_id = ?`, "project-a", taskID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.InsertWorkToken("near-expiry-token", "project-a", taskID, now.Add(-workTokenLifetime+time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	task, _, err := broker.ListenRole(context.Background(), "project-b", "g:shared:worker", "poll", 0)
	if err != nil || task == nil {
		t.Fatalf("delivery failed: task=%v err=%v", task, err)
	}
	deliveredToken := broker.TaskWorkToken(task)
	if deliveredToken == "" || deliveredToken == "near-expiry-token" {
		t.Fatalf("near-expiry capability was delivered instead of rotated: %q", deliveredToken)
	}
	scope, err := store.GetWorkToken(deliveredToken)
	if err != nil || scope == nil || time.Until(scope.ExpiresAt) < workTokenReuseFloor {
		t.Fatalf("rotated capability lacks delivery window: scope=%+v err=%v", scope, err)
	}
	oldScope, err := store.GetWorkToken("near-expiry-token")
	if err != nil || oldScope == nil {
		t.Fatalf("rotation unexpectedly revoked old unexpired capability: scope=%+v err=%v", oldScope, err)
	}
}

func TestTaskWorkTokenAccessorSynchronizesRefresh(t *testing.T) {
	broker := newTestBroker(t, true, true)
	taskID, err := broker.CreateTask("project-a", "g:shared:worker", "T", "M")
	if err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	task := broker.tasks["project-a"][taskID]
	broker.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			broker.mu.Lock()
			if err := broker.ensureTaskWorkToken(task); err != nil {
				broker.mu.Unlock()
				t.Errorf("refresh failed: %v", err)
				return
			}
			broker.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if token := broker.TaskWorkToken(task); token == "" {
				t.Error("accessor returned an empty capability")
				return
			}
		}
	}()
	wg.Wait()
}
