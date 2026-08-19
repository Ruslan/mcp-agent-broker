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
	TaskID          string     `json:"task_id"`
	ProjectID       string     `json:"project_id"`
	Role            string     `json:"role"`
	Title           string     `json:"title"`
	Status          TaskStatus `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ResultViewCount int        `json:"result_view_count"`
}

// Task represents a unit of work assigned to a role.
type Task struct {
	ID        string
	ProjectID string
	Role      string
	Title     string
	MD        string
	workToken string
	done      chan string // buffered size 1
	progress  chan string // buffered size 32, closed by SolveTask
}

// queueAddress keeps routing separate from task ownership. Local addresses are
// scoped to a project; global addresses deliberately ignore the caller project.
type queueAddress struct {
	ProjectID string
	Role      string
	Global    bool
}

func addressFor(projectID, role string) queueAddress {
	if isGlobalRole(role) {
		return queueAddress{Role: role, Global: true}
	}
	return queueAddress{ProjectID: projectID, Role: role}
}

func isGlobalRole(role string) bool {
	if !strings.HasPrefix(role, "g:") {
		return false
	}
	parts := strings.SplitN(role, ":", 3)
	return len(parts) == 3 && parts[1] != "" && parts[2] != ""
}

type listenerEntry struct {
	ch        chan *Task
	ctx       context.Context
	startedAt time.Time
	timeoutMs int
}

// Broker manages the coordination between task creators and role listeners.
type Broker struct {
	mu         sync.Mutex
	listeners  map[queueAddress]*listenerEntry // routing address -> listener
	tasks      map[string]map[string]*Task     // projectID -> taskID -> *Task
	asyncQueue map[queueAddress][]*Task        // routing address -> []*Task
	store      Store
	promptsDir string

	EnableSync  bool
	EnableAsync bool

	// PublicURL, when set (BROKER_PUBLIC_URL), overrides the base used to build
	// poll_url values handed to clients. Empty → derive the base per-request from
	// the request scheme + Host. X-Forwarded-Proto is always honored (scheme-only,
	// safe); X-Forwarded-Host only when TrustForwarded is set.
	PublicURL string
	// TrustForwarded (BROKER_TRUST_FORWARDED) additionally allows X-Forwarded-Host
	// to set the poll_url host — only enable it behind a proxy you trust, as a
	// forged Host would redirect a live capability token to an attacker. (X-
	// Forwarded-Proto is honored regardless: it only flips the scheme.)
	TrustForwarded bool

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
		listeners:   make(map[queueAddress]*listenerEntry),
		tasks:       make(map[string]map[string]*Task),
		asyncQueue:  make(map[queueAddress][]*Task),
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
		workToken := ""
		if isGlobalRole(r.Role) {
			scope, tokenErr := b.MintWorkToken(r.ProjectID, r.TaskID)
			if tokenErr != nil {
				return nil, fmt.Errorf("failed to restore work capability: %w", tokenErr)
			}
			workToken = scope.Token
		}
		task := &Task{
			ID:        r.TaskID,
			ProjectID: r.ProjectID,
			Role:      r.Role,
			Title:     r.Title,
			MD:        r.TaskMD,
			workToken: workToken,
			done:      make(chan string, 1),
			progress:  make(chan string, 32),
		}
		if b.tasks[r.ProjectID] == nil {
			b.tasks[r.ProjectID] = make(map[string]*Task)
		}
		b.tasks[r.ProjectID][r.TaskID] = task
		if r.Status == StatusQueued {
			addr := addressFor(r.ProjectID, r.Role)
			b.asyncQueue[addr] = append(b.asyncQueue[addr], task)
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

// Poll token constants. A poll token is the capability embedded in a poll_url
// (GET /poll/{token}); it authorizes exactly one scoped poll and nothing else.
const (
	// pollTokenTTL is the sliding lifetime: each poll of /poll/{token} renews the
	// token by this much, so an actively-polling script keeps it alive. Stall
	// longer than this and the token dies — go fetch a fresh poll_url.
	pollTokenTTL = 30 * time.Minute
	// pollTokenMaxLifetime is the absolute cap from creation, regardless of
	// renewal, so a leaked token can never be read from "a day later": even a
	// continuously-polled token rotates out at this bound.
	pollTokenMaxLifetime = 24 * time.Hour
	// pollTokenReuseFloor: reuse an existing token for a scope only while it has
	// at least this much life left, so a caller never gets a near-dead one.
	pollTokenReuseFloor = 5 * time.Minute
	PollScopeRole       = "role"
	PollScopeTask       = "task"
	// Work capabilities use a fixed lifetime: long enough for multi-day agent
	// work, but bounded so a leaked capability does not remain valid forever.
	workTokenLifetime = 7 * 24 * time.Hour
	// A queued task should not be delivered with a capability near the end of
	// that lifetime. Reuse only when almost the full working window remains.
	workTokenReuseFloor = 6 * 24 * time.Hour
)

// MintPollToken returns a scoped poller token for (projectID, kind, value),
// reusing an existing token for the same scope when it still has life left so
// repeated mints don't accumulate rows. kind is PollScopeRole or PollScopeTask.
func (b *Broker) MintPollToken(projectID, kind, value string) (*PollTokenScope, error) {
	if existing, err := b.store.GetActivePollToken(projectID, kind, value); err == nil && existing != nil {
		if time.Until(existing.ExpiresAt) >= pollTokenReuseFloor {
			return existing, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("failed to generate poll token: %w", err)
	}
	now := time.Now().UTC()
	scope := &PollTokenScope{
		Token:     hex.EncodeToString(raw),
		ProjectID: projectID,
		Kind:      kind,
		Value:     value,
		CreatedAt: now,
		ExpiresAt: now.Add(pollTokenTTL),
	}
	if err := b.store.InsertPollToken(scope.Token, scope.ProjectID, scope.Kind, scope.Value, scope.CreatedAt, scope.ExpiresAt); err != nil {
		return nil, err
	}
	return scope, nil
}

// RenewPollToken validates and slides a token's expiry forward. It returns the
// live scope, or (nil, found) where found distinguishes an expired token
// (found=true) from an unknown one (found=false). See store.RenewPollToken.
func (b *Broker) RenewPollToken(token string) (*PollTokenScope, bool, error) {
	return b.store.RenewPollToken(token, pollTokenTTL, pollTokenMaxLifetime)
}

// MintWorkToken returns a restart-safe capability for one global task. Active
// tokens are reused so response retries and administrative requeues preserve
// the capability carried by a worker integration.
func (b *Broker) MintWorkToken(projectID, taskID string) (*WorkTokenScope, error) {
	if existing, err := b.store.GetActiveWorkToken(projectID, taskID); err != nil {
		return nil, err
	} else if existing != nil && time.Until(existing.ExpiresAt) >= workTokenReuseFloor {
		return existing, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("failed to generate work token: %w", err)
	}
	now := time.Now().UTC()
	scope := &WorkTokenScope{
		Token: hex.EncodeToString(raw), ProjectID: projectID, TaskID: taskID,
		CreatedAt: now, ExpiresAt: now.Add(workTokenLifetime),
	}
	if err := b.store.InsertWorkToken(scope.Token, scope.ProjectID, scope.TaskID, scope.CreatedAt, scope.ExpiresAt); err != nil {
		return nil, err
	}
	return scope, nil
}

// ResolveWorkerProject applies the worker capability boundary. Omitting a token
// retains local behavior. Supplying one must resolve to this exact task; all
// invalid, expired, or mismatched capabilities intentionally share one error.
func (b *Broker) ResolveWorkerProject(callerProjectID, taskID, workToken string) (string, error) {
	if workToken == "" {
		return callerProjectID, nil
	}
	scope, err := b.store.GetWorkToken(workToken)
	if err != nil {
		return "", fmt.Errorf("invalid work_token")
	}
	if scope == nil || scope.TaskID != taskID {
		return "", fmt.Errorf("invalid work_token")
	}
	return scope.ProjectID, nil
}

func (b *Broker) ensureTaskWorkToken(task *Task) error {
	if !isGlobalRole(task.Role) {
		task.workToken = ""
		return nil
	}
	scope, err := b.MintWorkToken(task.ProjectID, task.ID)
	if err != nil {
		return err
	}
	task.workToken = scope.Token
	return nil
}

// TaskWorkToken returns the capability to include in a worker delivery. Task
// fields can be refreshed by admin/requeue paths, so callers must not read the
// field directly after ListenRole releases the broker lock.
func (b *Broker) TaskWorkToken(task *Task) string {
	if task == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return task.workToken
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
	for addr, queue := range b.asyncQueue {
		for _, task := range queue {
			if task.ProjectID == projectID && task.ID == taskID {
				if queuedRole == "" {
					queuedRole = addr.Role
				}
				queueOccurrences++
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

func (b *Broker) deleteListenerLocked(addr queueAddress, listener *listenerEntry) bool {
	if b.listeners[addr] != listener {
		return false
	}
	delete(b.listeners, addr)
	return true
}

func (b *Broker) enqueueUniqueLocked(addr queueAddress, task *Task) bool {
	for _, queued := range b.asyncQueue[addr] {
		if queued.ProjectID == task.ProjectID && queued.ID == task.ID {
			return false
		}
	}
	b.asyncQueue[addr] = append(b.asyncQueue[addr], task)
	return true
}

func (b *Broker) requeueDeliveredTask(listener *listenerEntry, source string) {
	select {
	case task := <-listener.ch:
		if err := b.RequeuePickedTask(task.ProjectID, task.ID, source); err != nil {
			log.Printf("failed to requeue delivered task after listener exit: source=%s project=%s task=%s err=%v", source, task.ProjectID, task.ID, err)
		}
	default:
	}
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

	workToken := ""
	if isGlobalRole(role) {
		scope, tokenErr := b.MintWorkToken(projectID, taskID)
		if tokenErr != nil {
			_ = b.store.DeleteTask(projectID, taskID)
			return "", fmt.Errorf("failed to create work capability: %w", tokenErr)
		}
		workToken = scope.Token
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	task := &Task{
		ID:        taskID,
		ProjectID: projectID,
		Role:      role,
		Title:     title,
		MD:        taskMD,
		workToken: workToken,
		done:      make(chan string, 1),
		progress:  make(chan string, 32),
	}
	if b.tasks[projectID] == nil {
		b.tasks[projectID] = make(map[string]*Task)
	}
	b.tasks[projectID][taskID] = task

	addr := addressFor(projectID, role)
	// If a listener is waiting, deliver directly.
	if listener, hasListener := b.listeners[addr]; hasListener {
		if listener.ctx != nil && listener.ctx.Err() != nil {
			listenerAge := time.Since(listener.startedAt).Round(time.Millisecond)
			b.deleteListenerLocked(addr, listener)
			log.Printf("listen_role wait stale listener skipped: project=%s role=%s age=%s err=%v", projectID, role, listenerAge, listener.ctx.Err())
		} else {
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
			b.deleteListenerLocked(addr, listener)

			select {
			case <-listener.ctx.Done():
				if err := b.store.UpdateStatus(projectID, taskID, StatusQueued); err != nil {
					log.Printf("failed to rollback status for canceled listener task %s: %v", taskID, err)
				} else {
					resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, taskID)
					log.Printf("task status transition: source=create_task.wait_listener_canceled project=%s task=%s role=%s to=%s result_present=%t result_len=%d result_read_error=%v", projectID, taskID, role, StatusQueued, resultPresent, resultLen, resultErr)
				}
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

	b.enqueueUniqueLocked(addr, task)

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
	addr := addressFor(projectID, role)
	// Check for queued async work first
	if _, ok := b.asyncQueue[addr]; ok {
		for queue := b.asyncQueue[addr]; len(queue) > 0; queue = b.asyncQueue[addr] {
			task := queue[0]
			b.asyncQueue[addr] = queue[1:]

			meta, metaErr := b.store.GetStatus(task.ProjectID, task.ID)
			resultPresent, resultLen, resultErr := b.resultDiagnostics(task.ProjectID, task.ID)
			if metaErr != nil || meta.Status != StatusQueued || resultPresent || resultErr != nil {
				status := ""
				if meta != nil {
					status = string(meta.Status)
				}
				log.Printf("task pick skipped: source=listen_role.%s project=%s task=%s role=%s queue_len_before=%d db_status=%s status_read_error=%v result_present=%t result_len=%d result_read_error=%v", mode, task.ProjectID, task.ID, role, len(queue), status, metaErr, resultPresent, resultLen, resultErr)
				continue
			}

			if err := b.ensureTaskWorkToken(task); err != nil {
				b.asyncQueue[addr] = append([]*Task{task}, b.asyncQueue[addr]...)
				b.mu.Unlock()
				return nil, "", fmt.Errorf("failed to create work capability: %w", err)
			}
			b.logPickAttempt(task.ProjectID, task, "listen_role."+mode, len(queue))
			if err := b.store.UpdateStatus(task.ProjectID, task.ID, StatusPicked); err != nil {
				b.mu.Unlock()
				return nil, "", fmt.Errorf("failed to update status to picked: %w", err)
			}
			log.Printf("task status transition: source=listen_role.%s project=%s task=%s role=%s to=%s queue_len_before=%d", mode, task.ProjectID, task.ID, role, StatusPicked, len(queue))
			b.publishAdminEvent(statusEvent{
				ProjectID: task.ProjectID,
				TaskID:    task.ID,
				Status:    StatusPicked,
				UpdatedAt: time.Now().UTC(),
			})

			b.mu.Unlock()
			return task, "picked", nil
		}
	}

	if mode == "poll" {
		b.mu.Unlock()
		return nil, "empty", nil
	}

	// Mode is wait
	if existing, exists := b.listeners[addr]; exists {
		age := time.Since(existing.startedAt).Round(time.Millisecond)
		log.Printf("listen_role wait duplicate: project=%s role=%s existing_age=%s existing_timeout_ms=%d", projectID, role, age, existing.timeoutMs)
		b.mu.Unlock()
		if addr.Global {
			return nil, "", fmt.Errorf("role %q already has a listener", role)
		}
		return nil, "", fmt.Errorf("role %q already has a listener in project %q", role, projectID)
	}

	listenerCtx := ctx
	var cancelListener context.CancelFunc
	if timeoutMs > 0 {
		listenerCtx, cancelListener = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	}
	if cancelListener != nil {
		defer cancelListener()
	}

	listener := &listenerEntry{
		ch:        make(chan *Task, 1),
		ctx:       listenerCtx,
		startedAt: time.Now().UTC(),
		timeoutMs: timeoutMs,
	}
	b.listeners[addr] = listener
	log.Printf("listen_role wait registered: project=%s role=%s timeout_ms=%d", projectID, role, timeoutMs)
	b.mu.Unlock()

	exitReason := "unknown"
	defer func() {
		b.mu.Lock()
		if b.deleteListenerLocked(addr, listener) {
			log.Printf("listen_role wait cleanup: project=%s role=%s reason=%s age=%s timeout_ms=%d", projectID, role, exitReason, time.Since(listener.startedAt).Round(time.Millisecond), timeoutMs)
		}
		b.mu.Unlock()
	}()

	var timeoutCh <-chan struct{}
	if timeoutMs > 0 {
		timeoutCh = listenerCtx.Done()
	}

	select {
	case task := <-listener.ch:
		exitReason = "delivered"
		log.Printf("listen_role wait returning task: project=%s role=%s task=%s age=%s", projectID, role, task.ID, time.Since(listener.startedAt).Round(time.Millisecond))
		return task, "picked", nil
	case <-ctx.Done():
		exitReason = "context_canceled"
		b.requeueDeliveredTask(listener, "listen_role.context_canceled_after_delivery")
		log.Printf("listen_role wait context canceled: project=%s role=%s age=%s err=%v", projectID, role, time.Since(listener.startedAt).Round(time.Millisecond), ctx.Err())
		return nil, "", ctx.Err()
	case <-timeoutCh:
		if ctx.Err() != nil {
			exitReason = "context_canceled"
			b.requeueDeliveredTask(listener, "listen_role.context_canceled_after_delivery")
			log.Printf("listen_role wait context canceled: project=%s role=%s age=%s err=%v", projectID, role, time.Since(listener.startedAt).Round(time.Millisecond), ctx.Err())
			return nil, "", ctx.Err()
		}
		exitReason = "timeout"
		b.requeueDeliveredTask(listener, "listen_role.timeout_after_delivery")
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

func (b *Broker) IncrementResultViewCount(projectID, taskID string) (int, error) {
	return b.store.IncrementResultViewCount(projectID, taskID)
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

// RequeuePickedTask returns a delivered task back to the queue when delivery to
// the worker failed before the broker could rely on the worker processing it.
func (b *Broker) RequeuePickedTask(projectID, taskID, source string) error {
	if !isSafeID(taskID) {
		return fmt.Errorf("invalid task_id")
	}
	if source == "" {
		source = "unknown"
	}

	meta, err := b.store.GetStatus(projectID, taskID)
	if err != nil {
		return err
	}
	resultPresent, resultLen, resultErr := b.resultDiagnostics(projectID, taskID)
	if meta.Status != StatusPicked {
		log.Printf("task requeue skipped: source=%s project=%s task=%s role=%s status=%s result_present=%t result_len=%d result_read_error=%v", source, projectID, taskID, meta.Role, meta.Status, resultPresent, resultLen, resultErr)
		return nil
	}
	if resultErr != nil {
		return fmt.Errorf("failed to inspect result before requeue: %w", resultErr)
	}
	if resultPresent {
		log.Printf("task requeue skipped: source=%s project=%s task=%s role=%s status=%s result_present=%t result_len=%d", source, projectID, taskID, meta.Role, meta.Status, resultPresent, resultLen)
		return nil
	}

	md, err := b.store.GetTaskMD(projectID, taskID)
	if err != nil {
		return fmt.Errorf("failed to reload task markdown: %w", err)
	}
	updated, err := b.store.UpdateStatusIfCurrent(projectID, taskID, StatusPicked, StatusQueued)
	if err != nil {
		return err
	}
	if !updated {
		latest, latestErr := b.store.GetStatus(projectID, taskID)
		if latestErr != nil {
			return latestErr
		}
		log.Printf("task requeue skipped: source=%s project=%s task=%s role=%s status_changed_to=%s", source, projectID, taskID, latest.Role, latest.Status)
		return nil
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
	if err := b.ensureTaskWorkToken(task); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("failed to refresh work capability: %w", err)
	}
	enqueued := b.enqueueUniqueLocked(addressFor(projectID, meta.Role), task)
	b.mu.Unlock()

	b.publishAdminEvent(statusEvent{
		ProjectID: projectID,
		TaskID:    taskID,
		Status:    StatusQueued,
		UpdatedAt: time.Now().UTC(),
	})

	log.Printf("task requeued: source=%s project=%s task=%s role=%s from=%s to=%s enqueued=%t", source, projectID, taskID, meta.Role, meta.Status, StatusQueued, enqueued)
	return nil
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

		if err := b.ensureTaskWorkToken(task); err != nil {
			b.mu.Unlock()
			return fmt.Errorf("failed to refresh work capability: %w", err)
		}
		b.enqueueUniqueLocked(addressFor(projectID, meta.Role), task)
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

	prompts := make([]PromptMetadata, 0, len(templates)+1)
	for _, tmpl := range templates {
		prompts = append(prompts, PromptMetadata{
			Name:        tmpl.Name,
			Title:       tmpl.Title,
			Description: tmpl.Description,
			Arguments:   tmpl.Arguments,
		})
	}
	// skill-install is served from the embedded skillfiles, not a disk template,
	// so its body stays byte-identical to the shipped scripts by construction.
	prompts = append(prompts, skillInstallPromptMetadata())
	return prompts, nil
}

// skillInstallPromptMetadata is the ListPrompts/GetPrompt metadata for the
// embed-backed skill-install prompt.
func skillInstallPromptMetadata() PromptMetadata {
	return PromptMetadata{
		Name:        skillInstallPromptName,
		Title:       "Install broker-async-poll skill",
		Description: skillInstallPromptDescription,
	}
}

// GetPrompt returns the content of a specific prompt file.
func (b *Broker) GetPrompt(name string, arguments map[string]string) (PromptMetadata, string, error) {
	if !isSafeID(name) {
		return PromptMetadata{}, "", fmt.Errorf("invalid prompt name")
	}
	// skill-install is assembled from the embedded skillfiles at request time,
	// keeping the installer prompt and the shipped scripts identical.
	if name == skillInstallPromptName {
		return skillInstallPromptMetadata(), buildSkillInstallPrompt(), nil
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
