package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// os and path/filepath are still used by the prompts subsystem below.

const (
	ServerVersion   = "1.0.0"
	ProtocolVersion = "2024-11-05"
)

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	StatusQueued TaskStatus = "queued"
	StatusPicked TaskStatus = "picked"
	StatusSolved TaskStatus = "solved"
)

// StatusMetadata represents the JSON shape of status.json.
type StatusMetadata struct {
	TaskID    string     `json:"task_id"`
	ProjectID string     `json:"project_id"`
	Role      string     `json:"role"`
	Title     string     `json:"title"`
	Status    TaskStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Task represents a unit of work assigned to a role.
type Task struct {
	ID        string
	ProjectID string
	Role      string
	Title     string
	MD        string
	done      chan string // buffered size 1
	progress  chan string // buffered size 32, closed by SolveTask
}

type listenerEntry struct {
	ch        chan *Task
	startedAt time.Time
	timeoutMs int
}

// Broker manages the coordination between task creators and role listeners.
type Broker struct {
	mu         sync.Mutex
	listeners  map[string]map[string]*listenerEntry // projectID -> role -> listener
	tasks      map[string]map[string]*Task          // projectID -> taskID -> *Task
	asyncQueue map[string]map[string][]*Task        // projectID -> role -> []*Task
	store      Store
	promptsDir string

	EnableSync  bool
	EnableAsync bool

	adminSubs   map[chan statusEvent]struct{}
	adminSubsMu sync.Mutex

	// Hook for testing: override task ID generation.
	generateID func() string
}

// statusEvent is sent to admin subscribers.
type statusEvent struct {
	ProjectID string     `json:"project_id"`
	TaskID    string     `json:"task_id"`
	Status    TaskStatus `json:"status"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// NewBroker initializes and returns a new Broker with the given store.
func NewBroker(store Store, promptsDir string, enableSync, enableAsync bool) (*Broker, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if promptsDir == "" {
		promptsDir = "prompts"
	}
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create prompts directory: %w", err)
	}

	b := &Broker{
		listeners:   make(map[string]map[string]*listenerEntry),
		tasks:       make(map[string]map[string]*Task),
		asyncQueue:  make(map[string]map[string][]*Task),
		store:       store,
		promptsDir:  promptsDir,
		EnableSync:  enableSync,
		EnableAsync: enableAsync,
		adminSubs:   make(map[chan statusEvent]struct{}),
	}
	b.generateID = generateTaskID

	// Restore active tasks from DB so SolveTask/ProgressTask work after a restart.
	activeTasks, err := store.LoadActiveTasks()
	if err != nil {
		return nil, fmt.Errorf("failed to restore active tasks: %w", err)
	}
	for _, r := range activeTasks {
		task := &Task{
			ID:        r.TaskID,
			ProjectID: r.ProjectID,
			Role:      r.Role,
			Title:     r.Title,
			MD:        r.TaskMD,
			done:      make(chan string, 1),
			progress:  make(chan string, 32),
		}
		if b.tasks[r.ProjectID] == nil {
			b.tasks[r.ProjectID] = make(map[string]*Task)
		}
		b.tasks[r.ProjectID][r.TaskID] = task
		if r.Status == StatusQueued {
			if b.asyncQueue[r.ProjectID] == nil {
				b.asyncQueue[r.ProjectID] = make(map[string][]*Task)
			}
			b.asyncQueue[r.ProjectID][r.Role] = append(b.asyncQueue[r.ProjectID][r.Role], task)
		}
	}
	if len(activeTasks) > 0 {
		log.Printf("Restored %d active task(s) from database", len(activeTasks))
	}

	return b, nil
}

// isSafeID checks if the taskID is safe to use as a directory name.
func isSafeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	// Reject path separators
	if strings.ContainsAny(id, "/\\") {
		return false
	}
	return true
}

// generateTaskID creates a random 16-byte hex string (UUID-like).
func generateTaskID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// Fallback to timestamp if rand fails (should not happen in practice)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (b *Broker) resultDiagnostics(projectID, taskID string) (bool, int, error) {
	result, err := b.store.GetResult(projectID, taskID)
	if err != nil {
		return false, 0, err
	}
	return result != "", len(result), nil
}

func (b *Broker) memoryDiagnostics(projectID, taskID string) (bool, string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	inTasks := false
	if projectTasks, ok := b.tasks[projectID]; ok {
		_, inTasks = projectTasks[taskID]
	}

	queuedRole := ""
	queueOccurrences := 0
	if projectQueue, ok := b.asyncQueue[projectID]; ok {
		for role, queue := range projectQueue {
			for _, task := range queue {
				if task.ID == taskID {
					if queuedRole == "" {
						queuedRole = role
					}
					queueOccurrences++
				}
			}
		}
	}

	return inTasks, queuedRole, queueOccurrences
}

func (b *Broker) logPickAttempt(projectID string, task *Task, source string, queueLen int) {
	meta, metaErr := b.store.GetStatus(projectID, task.ID)
	resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, task.ID)
	if metaErr != nil {
		log.Printf("task pick attempt: source=%s project=%s task=%s role=%s queue_len=%d status_read_error=%v result_present=%t result_len=%d result_read_error=%v", source, projectID, task.ID, task.Role, queueLen, metaErr, resultPresent, resultLen, resultErr)
		return
	}

	if meta.Status != StatusQueued || resultPresent || resultErr != nil {
		log.Printf("task pick anomaly: source=%s project=%s task=%s role=%s queue_len=%d db_status=%s result_present=%t result_len=%d result_read_error=%v", source, projectID, task.ID, task.Role, queueLen, meta.Status, resultPresent, resultLen, resultErr)
		return
	}

	log.Printf("task pick attempt: source=%s project=%s task=%s role=%s queue_len=%d db_status=%s result_present=%t result_len=%d", source, projectID, task.ID, task.Role, queueLen, meta.Status, resultPresent, resultLen)
}

// CreateTask enqueues a task for a role and returns immediately with a generated ID.
func (b *Broker) CreateTask(projectID, role, title, taskMD string) (string, error) {
	if role == "" || title == "" || taskMD == "" {
		return "", fmt.Errorf("role, title and task_md are required")
	}
	if len(title) > 200 {
		return "", fmt.Errorf("title too long (max 200 characters)")
	}

	var taskID string
	var err error

	for i := 0; i < 5; i++ {
		taskID = b.generateID()
		err = b.store.InsertTask(projectID, taskID, role, title, taskMD)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrTaskExists) {
			return "", fmt.Errorf("persistence failed: %w", err)
		}
	}
	if err != nil {
		return "", fmt.Errorf("failed to generate unique task_id after 5 attempts: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	task := &Task{
		ID:        taskID,
		ProjectID: projectID,
		Role:      role,
		Title:     title,
		MD:        taskMD,
		done:      make(chan string, 1),
		progress:  make(chan string, 32),
	}
	if b.tasks[projectID] == nil {
		b.tasks[projectID] = make(map[string]*Task)
	}
	b.tasks[projectID][taskID] = task

	// If a listener is waiting, deliver directly
	if projectListeners, ok := b.listeners[projectID]; ok {
		if listener, hasListener := projectListeners[role]; hasListener {
			listenerAge := time.Since(listener.startedAt).Round(time.Millisecond)
			b.logPickAttempt(projectID, task, "create_task.wait_listener", 0)
			if err := b.store.UpdateStatus(projectID, taskID, StatusPicked); err != nil {
				delete(b.tasks[projectID], taskID)
				b.store.DeleteTask(projectID, taskID)
				return "", fmt.Errorf("failed to update status to picked: %w", err)
			}
			log.Printf("task status transition: source=create_task.wait_listener project=%s task=%s role=%s to=%s listener_age=%s listener_timeout_ms=%d", projectID, taskID, role, StatusPicked, listenerAge, listener.timeoutMs)

			// Wait-mode listeners are one-shot. Remove the listener before
			// delivering so subsequent tasks are queued for the next listen call.
			delete(projectListeners, role)
			if len(projectListeners) == 0 {
				delete(b.listeners, projectID)
			}

			select {
			case listener.ch <- task:
				log.Printf("listen_role wait delivered: project=%s role=%s task=%s listener_age=%s", projectID, role, taskID, listenerAge)
				b.publishAdminEvent(statusEvent{
					ProjectID: projectID,
					TaskID:    taskID,
					Status:    StatusPicked,
					UpdatedAt: time.Now().UTC(),
				})
				return taskID, nil
			default:
				// Listener was busy/disappeared, rollback to queued
				if err := b.store.UpdateStatus(projectID, taskID, StatusQueued); err != nil {
					log.Printf("failed to rollback status for task %s: %v", taskID, err)
				} else {
					resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, taskID)
					log.Printf("task status transition: source=create_task.wait_listener_rollback project=%s task=%s role=%s to=%s result_present=%t result_len=%d result_read_error=%v", projectID, taskID, role, StatusQueued, resultPresent, resultLen, resultErr)
				}
			}
		}
	}

	if b.asyncQueue[projectID] == nil {
		b.asyncQueue[projectID] = make(map[string][]*Task)
	}
	b.asyncQueue[projectID][role] = append(b.asyncQueue[projectID][role], task)

	b.publishAdminEvent(statusEvent{
		ProjectID: projectID,
		TaskID:    taskID,
		Status:    StatusQueued,
		UpdatedAt: time.Now().UTC(),
	})

	log.Printf("task created: project=%s task=%s role=%s title=%q", projectID, taskID, role, title)
	return taskID, nil
}

// ReportProgress sends an intermediate progress message for an in-flight task.
// Non-blocking: if the progress buffer (32) is full, the message is dropped with a log warning.
func (b *Broker) ReportProgress(projectID, taskID, message string) error {
	b.mu.Lock()
	projectTasks, ok := b.tasks[projectID]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	task, exists := projectTasks[taskID]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}

	select {
	case task.progress <- message:
	default:
		log.Printf("progress buffer full for task %s, dropping message", taskID)
	}
	b.mu.Unlock()

	if err := b.store.AppendProgress(projectID, taskID, message); err != nil {
		log.Printf("failed to persist progress for task %s: %v", taskID, err)
	}

	return nil
}

// AwaitTask blocks until the task reaches a terminal state or timeout/cancel.
// Returns status, result, collected progress messages, and error.
func (b *Broker) AwaitTask(ctx context.Context, projectID, taskID string, timeoutMs int) (string, string, []string, error) {
	if taskID == "" {
		return "", "", nil, fmt.Errorf("task_id is required")
	}
	if !isSafeID(taskID) {
		return "", "", nil, fmt.Errorf("invalid task_id")
	}

	// First check disk to see if it's already solved
	meta, err := b.GetTaskStatus(projectID, taskID)
	if err != nil {
		return "", "", nil, err
	}
	if meta.Status == StatusSolved {
		res, err := b.GetTaskResult(projectID, taskID)
		if err == nil {
			return string(meta.Status), res, nil, nil
		}
	}

	b.mu.Lock()
	projectTasks, ok := b.tasks[projectID]
	if !ok {
		b.mu.Unlock()
		return string(meta.Status), "", nil, nil
	}
	task, exists := projectTasks[taskID]
	b.mu.Unlock()

	if !exists {
		meta, err = b.GetTaskStatus(projectID, taskID)
		if err != nil {
			return "", "", nil, err
		}
		if meta.Status == StatusSolved {
			res, err := b.GetTaskResult(projectID, taskID)
			return string(meta.Status), res, nil, err
		}
		return string(meta.Status), "", nil, nil
	}

	var timeoutCh <-chan time.Time
	if timeoutMs > 0 {
		timeoutCh = time.After(time.Duration(timeoutMs) * time.Millisecond)
	}

	select {
	case res := <-task.done:
		// Re-send to channel so subsequent AwaitTasks can also get it
		select {
		case task.done <- res:
		default:
		}
		// Drain progress messages (progress channel is closed by SolveTask before signaling done)
		var progress []string
		for msg := range task.progress {
			progress = append(progress, msg)
		}
		return string(StatusSolved), res, progress, nil
	case <-ctx.Done():
		meta, _ = b.GetTaskStatus(projectID, taskID)
		if meta != nil {
			return string(meta.Status), "", nil, ctx.Err()
		}
		return "", "", nil, ctx.Err()
	case <-timeoutCh:
		meta, _ = b.GetTaskStatus(projectID, taskID)
		var progress []string
		for {
			select {
			case msg := <-task.progress:
				progress = append(progress, msg)
			default:
				goto doneTimeout
			}
		}
	doneTimeout:
		if meta != nil {
			return string(meta.Status), "", progress, nil
		}
		return "", "", progress, fmt.Errorf("task %q not found after timeout", taskID)
	}
}

// ListenRole handles both blocking wait and non-blocking poll.
func (b *Broker) ListenRole(ctx context.Context, projectID, role, mode string, timeoutMs int) (*Task, string, error) {
	if role == "" {
		return nil, "", fmt.Errorf("role name cannot be empty")
	}
	if mode != "poll" && mode != "wait" {
		return nil, "", fmt.Errorf("invalid mode: %q (must be 'poll' or 'wait')", mode)
	}

	b.mu.Lock()
	// Check for queued async work first
	if projectQueue, ok := b.asyncQueue[projectID]; ok {
		if queue := projectQueue[role]; len(queue) > 0 {
			task := queue[0]

			b.logPickAttempt(projectID, task, "listen_role."+mode, len(queue))
			if err := b.store.UpdateStatus(projectID, task.ID, StatusPicked); err != nil {
				b.mu.Unlock()
				return nil, "", fmt.Errorf("failed to update status to picked: %w", err)
			}
			log.Printf("task status transition: source=listen_role.%s project=%s task=%s role=%s to=%s queue_len_before=%d", mode, projectID, task.ID, role, StatusPicked, len(queue))
			b.publishAdminEvent(statusEvent{
				ProjectID: projectID,
				TaskID:    task.ID,
				Status:    StatusPicked,
				UpdatedAt: time.Now().UTC(),
			})

			projectQueue[role] = queue[1:]
			b.mu.Unlock()
			return task, "picked", nil
		}
	}

	if mode == "poll" {
		b.mu.Unlock()
		return nil, "empty", nil
	}

	// Mode is wait
	if projectListeners, ok := b.listeners[projectID]; ok {
		if existing, exists := projectListeners[role]; exists {
			age := time.Since(existing.startedAt).Round(time.Millisecond)
			log.Printf("listen_role wait duplicate: project=%s role=%s existing_age=%s existing_timeout_ms=%d", projectID, role, age, existing.timeoutMs)
			b.mu.Unlock()
			return nil, "", fmt.Errorf("role %q already has a listener in project %q", role, projectID)
		}
	} else {
		b.listeners[projectID] = make(map[string]*listenerEntry)
	}

	listener := &listenerEntry{
		ch:        make(chan *Task, 1),
		startedAt: time.Now().UTC(),
		timeoutMs: timeoutMs,
	}
	b.listeners[projectID][role] = listener
	log.Printf("listen_role wait registered: project=%s role=%s timeout_ms=%d", projectID, role, timeoutMs)
	b.mu.Unlock()

	exitReason := "unknown"
	defer func() {
		b.mu.Lock()
		if projectListeners, ok := b.listeners[projectID]; ok {
			if projectListeners[role] == listener {
				delete(projectListeners, role)
				if len(projectListeners) == 0 {
					delete(b.listeners, projectID)
				}
				log.Printf("listen_role wait cleanup: project=%s role=%s reason=%s age=%s timeout_ms=%d", projectID, role, exitReason, time.Since(listener.startedAt).Round(time.Millisecond), timeoutMs)
			}
		}
		b.mu.Unlock()
	}()

	var timeoutCh <-chan time.Time
	if timeoutMs > 0 {
		timeoutCh = time.After(time.Duration(timeoutMs) * time.Millisecond)
	}

	select {
	case task := <-listener.ch:
		exitReason = "delivered"
		log.Printf("listen_role wait returning task: project=%s role=%s task=%s age=%s", projectID, role, task.ID, time.Since(listener.startedAt).Round(time.Millisecond))
		return task, "picked", nil
	case <-ctx.Done():
		exitReason = "context_canceled"
		log.Printf("listen_role wait context canceled: project=%s role=%s age=%s err=%v", projectID, role, time.Since(listener.startedAt).Round(time.Millisecond), ctx.Err())
		return nil, "", ctx.Err()
	case <-timeoutCh:
		exitReason = "timeout"
		log.Printf("listen_role wait timeout: project=%s role=%s age=%s timeout_ms=%d", projectID, role, time.Since(listener.startedAt).Round(time.Millisecond), timeoutMs)
		return nil, "timeout", nil
	}
}

// SolveTask submits a result for a task ID and unblocks the creator.
// Uses the database as the source of truth; in-memory state is only cleaned up
// after the DB write succeeds.
func (b *Broker) SolveTask(projectID, taskID, resultMD string) error {
	if taskID == "" || resultMD == "" {
		return fmt.Errorf("task_id and result_md are required")
	}

	meta, err := b.store.GetStatus(projectID, taskID)
	if err != nil {
		return fmt.Errorf("solve_task received unknown task_id %q", taskID)
	}
	resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, taskID)
	inTasks, queuedRole, queueOccurrences := b.memoryDiagnostics(projectID, taskID)
	log.Printf("task solve attempt: project=%s task=%s role=%s db_status=%s existing_result_present=%t existing_result_len=%d result_read_error=%v in_memory=%t queued_role=%q queued_occurrences=%d new_result_len=%d", projectID, taskID, meta.Role, meta.Status, resultPresent, resultLen, resultErr, inTasks, queuedRole, queueOccurrences, len(resultMD))
	if meta.Status != StatusPicked {
		log.Printf("task solve anomaly: project=%s task=%s role=%s db_status=%s expected_status=%s existing_result_present=%t existing_result_len=%d in_memory=%t queued_role=%q queued_occurrences=%d", projectID, taskID, meta.Role, meta.Status, StatusPicked, resultPresent, resultLen, inTasks, queuedRole, queueOccurrences)
	}

	if meta.Status == StatusSolved {
		if storedResult, err := b.store.GetResult(projectID, taskID); err == nil {
			resultMD = storedResult
		}
		log.Printf("task solve duplicate: project=%s task=%s role=%s stored_result_len=%d", projectID, taskID, meta.Role, len(resultMD))
		b.cleanupInMemory(projectID, taskID, resultMD)
		return nil
	}

	if err := b.store.SaveResult(projectID, taskID, resultMD); err != nil {
		return fmt.Errorf("failed to save result: %w", err)
	}

	b.publishAdminEvent(statusEvent{
		ProjectID: projectID,
		TaskID:    taskID,
		Status:    StatusSolved,
		UpdatedAt: time.Now().UTC(),
	})

	storedResultPresent, storedResultLen, storedResultErr := b.resultDiagnostics(projectID, taskID)
	log.Printf("task solved: project=%s task=%s role=%s from=%s to=%s stored_result_present=%t stored_result_len=%d result_read_error=%v", projectID, taskID, meta.Role, meta.Status, StatusSolved, storedResultPresent, storedResultLen, storedResultErr)

	b.cleanupInMemory(projectID, taskID, resultMD)
	return nil
}

// cleanupInMemory removes the task from the in-memory map and signals the
// done channel so AwaitTask callers are unblocked. Safe to call when the
// task is not in memory (no-op).
func (b *Broker) cleanupInMemory(projectID, taskID, resultMD string) {
	b.mu.Lock()
	projectTasks, ok := b.tasks[projectID]
	if !ok {
		b.mu.Unlock()
		return
	}
	task, exists := projectTasks[taskID]
	if !exists {
		b.mu.Unlock()
		return
	}
	delete(projectTasks, taskID)
	if len(projectTasks) == 0 {
		delete(b.tasks, projectID)
	}
	close(task.progress)
	b.mu.Unlock()

	select {
	case task.done <- resultMD:
	default:
	}
}

// GetTaskStatus returns the status metadata for a task.
func (b *Broker) GetTaskStatus(projectID, taskID string) (*StatusMetadata, error) {
	if !isSafeID(taskID) {
		return nil, fmt.Errorf("invalid task_id")
	}
	return b.store.GetStatus(projectID, taskID)
}

// GetTaskResult returns the result for a task if available.
func (b *Broker) GetTaskResult(projectID, taskID string) (string, error) {
	if !isSafeID(taskID) {
		return "", fmt.Errorf("invalid task_id")
	}
	return b.store.GetResult(projectID, taskID)
}

// GetTaskMD returns the task description.
func (b *Broker) GetTaskMD(projectID, taskID string) (string, error) {
	if !isSafeID(taskID) {
		return "", fmt.Errorf("invalid task_id")
	}
	return b.store.GetTaskMD(projectID, taskID)
}

// ListTasks returns task metadata filtered by optional role and status.
func (b *Broker) ListTasks(projectID, role, status string, limit, offset int) ([]StatusMetadata, error) {
	return b.store.ListTasks(projectID, role, status, limit, offset)
}

// CountTasks returns the total number of tasks matching the filters.
func (b *Broker) CountTasks(projectID, role, status string) (int, error) {
	return b.store.CountTasks(projectID, role, status)
}

// AdminUpdateStatus allows admins to manually reset a task status (e.g. unstick a hanging task).
// Allowed transitions: any status -> "queued" or "solved".
// Setting to "queued" clears result_md. Setting to "solved" is idempotent if already solved.
func (b *Broker) AdminUpdateStatus(projectID, taskID, newStatus string) error {
	if !isSafeID(taskID) {
		return fmt.Errorf("invalid task_id")
	}

	meta, err := b.store.GetStatus(projectID, taskID)
	if err != nil {
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, taskID)

	target := TaskStatus(newStatus)
	if target != StatusQueued && target != StatusSolved {
		return fmt.Errorf("admin can only set status to 'queued' or 'solved'")
	}
	log.Printf("admin status change attempt: project=%s task=%s role=%s from=%s to=%s result_present=%t result_len=%d result_read_error=%v", projectID, taskID, meta.Role, meta.Status, target, resultPresent, resultLen, resultErr)

	if meta.Status == target {
		log.Printf("admin status change noop: project=%s task=%s role=%s status=%s result_present=%t result_len=%d", projectID, taskID, meta.Role, meta.Status, resultPresent, resultLen)
		return nil
	}

	if err := b.store.UpdateStatus(projectID, taskID, target); err != nil {
		return err
	}

	if target == StatusQueued {
		if err := b.store.ClearResult(projectID, taskID); err != nil {
			return fmt.Errorf("failed to clear result: %w", err)
		}
		log.Printf("admin result cleared after requeue: project=%s task=%s role=%s result_present_before=%t result_len_before=%d", projectID, taskID, meta.Role, resultPresent, resultLen)

		md, err := b.store.GetTaskMD(projectID, taskID)
		if err != nil {
			return fmt.Errorf("failed to reload task markdown: %w", err)
		}

		b.mu.Lock()
		if b.tasks[projectID] == nil {
			b.tasks[projectID] = make(map[string]*Task)
		}
		task, exists := b.tasks[projectID][taskID]
		if !exists {
			task = &Task{
				ID:        taskID,
				ProjectID: projectID,
				Role:      meta.Role,
				Title:     meta.Title,
				MD:        md,
				done:      make(chan string, 1),
				progress:  make(chan string, 32),
			}
			b.tasks[projectID][taskID] = task
		} else if task.MD == "" {
			task.MD = md
		}

		if b.asyncQueue[projectID] == nil {
			b.asyncQueue[projectID] = make(map[string][]*Task)
		}
		b.asyncQueue[projectID][meta.Role] = append(b.asyncQueue[projectID][meta.Role], task)
		b.mu.Unlock()
	} else {
		b.cleanupInMemory(projectID, taskID, "")
	}

	b.publishAdminEvent(statusEvent{
		ProjectID: projectID,
		TaskID:    taskID,
		Status:    target,
		UpdatedAt: time.Now().UTC(),
	})

	storedResultPresent, storedResultLen, storedResultErr := b.resultDiagnostics(projectID, taskID)
	log.Printf("admin status change: project=%s task=%s role=%s %s->%s result_present=%t result_len=%d result_read_error=%v", projectID, taskID, meta.Role, meta.Status, target, storedResultPresent, storedResultLen, storedResultErr)
	return nil
}

// DeleteTask removes a task and its associated data.
func (b *Broker) DeleteTask(projectID, taskID string) error {
	if !isSafeID(taskID) {
		return fmt.Errorf("invalid task_id")
	}
	b.mu.Lock()
	projectTasks, ok := b.tasks[projectID]
	if ok {
		delete(projectTasks, taskID)
		if len(projectTasks) == 0 {
			delete(b.tasks, projectID)
		}
	}
	b.mu.Unlock()

	return b.store.DeleteTask(projectID, taskID)
}

// GetTaskProgress returns all progress messages for a task.
func (b *Broker) GetTaskProgress(projectID, taskID string) ([]string, error) {
	return b.store.GetProgress(projectID, taskID)
}

// ListProjects returns a list of distinct project IDs.
func (b *Broker) ListProjects() ([]string, error) {
	return b.store.ListProjects()
}

// PromptMetadata represents basic information about a prompt.
type PromptMetadata struct {
	Name        string                   `json:"name"`
	Title       string                   `json:"title,omitempty"`
	Description string                   `json:"description,omitempty"`
	Arguments   []PromptArgumentMetadata `json:"arguments,omitempty"`
}

// PromptArgumentMetadata describes one prompt argument.
type PromptArgumentMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type promptFrontMatter struct {
	Name        string                   `yaml:"name"`
	Title       string                   `yaml:"title"`
	Description string                   `yaml:"description"`
	Order       int                      `yaml:"order"`
	Arguments   []PromptArgumentMetadata `yaml:"arguments"`
}

type promptTemplate struct {
	promptFrontMatter
	Body string
	Path string
}

// ListPrompts scans the prompts directory for markdown files.
func (b *Broker) ListPrompts() ([]PromptMetadata, error) {
	entries, err := os.ReadDir(b.promptsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []PromptMetadata{}, nil
		}
		return nil, fmt.Errorf("failed to read prompts directory: %w", err)
	}

	var templates []promptTemplate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		tmpl, err := parsePromptTemplate(filepath.Join(b.promptsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		templates = append(templates, tmpl)
	}

	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Order != templates[j].Order {
			return templates[i].Order < templates[j].Order
		}
		return templates[i].Name < templates[j].Name
	})

	prompts := make([]PromptMetadata, 0, len(templates))
	for _, tmpl := range templates {
		prompts = append(prompts, PromptMetadata{
			Name:        tmpl.Name,
			Title:       tmpl.Title,
			Description: tmpl.Description,
			Arguments:   tmpl.Arguments,
		})
	}
	return prompts, nil
}

// GetPrompt returns the content of a specific prompt file.
func (b *Broker) GetPrompt(name string, arguments map[string]string) (PromptMetadata, string, error) {
	if !isSafeID(name) {
		return PromptMetadata{}, "", fmt.Errorf("invalid prompt name")
	}
	tmpl, err := b.findPromptTemplate(name)
	if err != nil {
		return PromptMetadata{}, "", err
	}
	meta := PromptMetadata{
		Name:        tmpl.Name,
		Title:       tmpl.Title,
		Description: tmpl.Description,
		Arguments:   tmpl.Arguments,
	}
	return meta, renderPromptTemplate(tmpl.Body, arguments), nil
}

func (b *Broker) findPromptTemplate(name string) (promptTemplate, error) {
	entries, err := os.ReadDir(b.promptsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return promptTemplate{}, fmt.Errorf("prompt %q not found", name)
		}
		return promptTemplate{}, fmt.Errorf("failed to read prompts directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		tmpl, err := parsePromptTemplate(filepath.Join(b.promptsDir, entry.Name()))
		if err != nil {
			return promptTemplate{}, err
		}
		if tmpl.Name == name {
			return tmpl, nil
		}
	}

	return promptTemplate{}, fmt.Errorf("prompt %q not found", name)
}

var frontMatterRegex = regexp.MustCompile(`(?s)^---[ \t]*\r?\n(.*?)\r?\n---[ \t]*\r?\n?(.*)`)

func parsePromptTemplate(path string) (promptTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return promptTemplate{}, fmt.Errorf("failed to read prompt: %w", err)
	}

	body := string(data)
	tmpl := promptTemplate{Path: path}

	if match := frontMatterRegex.FindStringSubmatch(body); match != nil {
		if err := yaml.Unmarshal([]byte(match[1]), &tmpl.promptFrontMatter); err != nil {
			return promptTemplate{}, fmt.Errorf("failed to parse prompt front matter in %s: %w", filepath.Base(path), err)
		}
		body = match[2]
	}

	if tmpl.Name == "" {
		tmpl.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if tmpl.Title == "" {
		tmpl.Title = tmpl.Name
	}
	if tmpl.Description == "" {
		tmpl.Description = fmt.Sprintf("Ralph Methodology: %s", tmpl.Title)
	}
	tmpl.Body = body
	return tmpl, nil
}

func renderPromptTemplate(body string, arguments map[string]string) string {
	if arguments == nil {
		arguments = map[string]string{}
	}
	roleName := strings.TrimSpace(arguments["role_name"])
	if roleName == "" {
		roleName = "coder"
	}
	replacements := make(map[string]string, len(arguments)+1)
	replacements["{{role_name}}"] = roleName
	for key, value := range arguments {
		if strings.TrimSpace(key) == "" {
			continue
		}
		replacements[fmt.Sprintf("{{%s}}", key)] = value
	}
	for placeholder, value := range replacements {
		body = strings.ReplaceAll(body, placeholder, value)
	}
	return body
}

// Subscribe returns a channel that receives all task status events.
func (b *Broker) Subscribe() chan statusEvent {
	ch := make(chan statusEvent, 32)
	b.adminSubsMu.Lock()
	b.adminSubs[ch] = struct{}{}
	b.adminSubsMu.Unlock()
	return ch
}

// Unsubscribe removes a channel from the admin subscription list.
func (b *Broker) Unsubscribe(ch chan statusEvent) {
	b.adminSubsMu.Lock()
	delete(b.adminSubs, ch)
	b.adminSubsMu.Unlock()
}

func (b *Broker) publishAdminEvent(e statusEvent) {
	b.adminSubsMu.Lock()
	defer b.adminSubsMu.Unlock()
	for ch := range b.adminSubs {
		select {
		case ch <- e:
		default:
			// Client slow or disconnected, drop event
		}
	}
}
