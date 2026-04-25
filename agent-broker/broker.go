package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
}

// Broker manages the coordination between task creators and role listeners.
type Broker struct {
	mu         sync.Mutex
	listeners  map[string]map[string]chan *Task // projectID -> role -> chan
	tasks      map[string]map[string]*Task      // projectID -> taskID -> *Task
	asyncQueue map[string]map[string][]*Task    // projectID -> role -> []*Task
	dataDir    string

	EnableSync  bool
	EnableAsync bool

	// Hooks for testing
	generateID    func() string
	persistStatus func(projectID, taskID, role, title string, status TaskStatus) error
}

// NewBroker initializes and returns a new Broker with persistence support.
func NewBroker(dataDir string, enableSync, enableAsync bool) (*Broker, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	b := &Broker{
		listeners:   make(map[string]map[string]chan *Task),
		tasks:       make(map[string]map[string]*Task),
		asyncQueue:  make(map[string]map[string][]*Task),
		dataDir:     dataDir,
		EnableSync:  enableSync,
		EnableAsync: enableAsync,
	}
	b.generateID = generateTaskID
	b.persistStatus = b.defaultPersistStatus
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

func (b *Broker) taskDir(projectID, taskID string) string {
	return filepath.Join(b.dataDir, projectID, taskID)
}

func (b *Broker) persistTask(projectID, taskID, taskMD string) error {
	dir := b.taskDir(projectID, taskID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create task directory: %w", err)
	}
	path := filepath.Join(dir, "task.md")
	
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("task file already exists")
		}
		return fmt.Errorf("failed to create task.md: %w", err)
	}
	defer f.Close()
	
	if _, err := f.Write([]byte(taskMD)); err != nil {
		return fmt.Errorf("failed to write task.md: %w", err)
	}
	return nil
}

func (b *Broker) defaultPersistStatus(projectID, taskID, role, title string, status TaskStatus) error {
	dir := b.taskDir(projectID, taskID)
	path := filepath.Join(dir, "status.json")
	
	var meta StatusMetadata
	now := time.Now().UTC()

	if _, err := os.Stat(path); err == nil {
		// Update existing
		data, err := os.ReadFile(path)
		if err == nil {
			json.Unmarshal(data, &meta)
		}
	} else {
		// New metadata
		meta.TaskID = taskID
		meta.ProjectID = projectID
		meta.Role = role
		meta.Title = title
		meta.CreatedAt = now
	}
	
	meta.Status = status
	meta.UpdatedAt = now
	
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}
	
	return os.WriteFile(path, data, 0644)
}

func (b *Broker) persistResult(projectID, taskID, resultMD string) error {
	dir := b.taskDir(projectID, taskID)
	path := filepath.Join(dir, "result.md")
	if err := os.WriteFile(path, []byte(resultMD), 0644); err != nil {
		return fmt.Errorf("failed to write result.md: %w", err)
	}
	return nil
}

// ListenRoleSync registers a listener for a specific role and blocks until a task is assigned or context is cancelled.
func (b *Broker) ListenRoleSync(ctx context.Context, projectID, role string) (*Task, error) {
	if role == "" {
		return nil, fmt.Errorf("role name cannot be empty")
	}

	b.mu.Lock()
	// Check for queued async work first
	if projectQueue, ok := b.asyncQueue[projectID]; ok {
		if queue := projectQueue[role]; len(queue) > 0 {
			task := queue[0]
			
			// Update status to picked before handing it out
			if err := b.persistStatus(projectID, task.ID, role, task.Title, StatusPicked); err != nil {
				b.mu.Unlock()
				return nil, fmt.Errorf("failed to update status to picked for async task: %w", err)
			}

			projectQueue[role] = queue[1:]
			b.mu.Unlock()
			return task, nil
		}
	}

	if projectListeners, ok := b.listeners[projectID]; ok {
		if _, exists := projectListeners[role]; exists {
			b.mu.Unlock()
			return nil, fmt.Errorf("role %q already has a listener in project %q", role, projectID)
		}
	} else {
		b.listeners[projectID] = make(map[string]chan *Task)
	}

	ch := make(chan *Task, 1)
	b.listeners[projectID][role] = ch
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if projectListeners, ok := b.listeners[projectID]; ok {
			delete(projectListeners, role)
			if len(projectListeners) == 0 {
				delete(b.listeners, projectID)
			}
		}
		b.mu.Unlock()
	}()

	select {
	case task := <-ch:
		return task, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ListenRoleAsync checks the async inbox for the role once and returns a task if found.
func (b *Broker) ListenRoleAsync(projectID, role string) (*Task, bool, error) {
	if role == "" {
		return nil, false, fmt.Errorf("role name cannot be empty")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	projectQueue, ok := b.asyncQueue[projectID]
	if !ok {
		return nil, false, nil
	}
	queue := projectQueue[role]
	if len(queue) == 0 {
		return nil, false, nil
	}

	task := queue[0]
	
	// Update status to picked before handing it out
	if err := b.persistStatus(projectID, task.ID, role, task.Title, StatusPicked); err != nil {
		return nil, false, fmt.Errorf("failed to update status to picked: %w", err)
	}

	projectQueue[role] = queue[1:]
	return task, true, nil
}

// CreateTaskSync creates a task for a role and blocks until it is solved or context is cancelled.
func (b *Broker) CreateTaskSync(ctx context.Context, projectID, role, title, taskMD string) (string, string, error) {
	if role == "" || title == "" || taskMD == "" {
		return "", "", fmt.Errorf("role, title and task_md are required")
	}
	if len(title) > 200 {
		return "", "", fmt.Errorf("title too long (max 200 characters)")
	}

	var taskID string
	var err error

	// Retry on collision (ID already exists on disk)
	for i := 0; i < 5; i++ {
		taskID = b.generateID()
		err = b.persistTask(projectID, taskID, taskMD)
		if err == nil {
			break
		}
		// If it's anything other than "already exists", fail immediately
		if !strings.Contains(err.Error(), "already exists") {
			return "", "", fmt.Errorf("persistence failed: %w", err)
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to generate unique task_id after 5 attempts: %w", err)
	}

	b.mu.Lock()
	projectListeners, hasProject := b.listeners[projectID]
	if !hasProject {
		b.mu.Unlock()
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", "", fmt.Errorf("role %q has no listener in project %q, ask user to clarify the role", role, projectID)
	}
	ch, hasListener := projectListeners[role]
	if !hasListener {
		b.mu.Unlock()
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", "", fmt.Errorf("role %q has no listener in project %q, ask user to clarify the role", role, projectID)
	}
	b.mu.Unlock()

	// Sync tasks also get status for consistency in v0.0.3 queries
	if err := b.persistStatus(projectID, taskID, role, title, StatusQueued); err != nil {
		os.RemoveAll(b.taskDir(projectID, taskID)) // Cleanup partial state
		return "", "", fmt.Errorf("failed to persist status: %w", err)
	}

	b.mu.Lock()
	projectListeners, hasProject = b.listeners[projectID]
	if !hasProject {
		b.mu.Unlock()
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", "", fmt.Errorf("project %q listeners disappeared", projectID)
	}
	ch, hasListener = projectListeners[role]
	if !hasListener {
		b.mu.Unlock()
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", "", fmt.Errorf("role %q listener in project %q disappeared", role, projectID)
	}

	task := &Task{
		ID:        taskID,
		ProjectID: projectID,
		Role:      role,
		Title:     title,
		MD:        taskMD,
		done:      make(chan string, 1),
	}
	if b.tasks[projectID] == nil {
		b.tasks[projectID] = make(map[string]*Task)
	}
	b.tasks[projectID][taskID] = task

	delivered := false
	select {
	case ch <- task:
		delivered = true
	default:
	}

	if !delivered {
		delete(b.tasks[projectID], taskID)
		if len(b.tasks[projectID]) == 0 {
			delete(b.tasks, projectID)
		}
		b.mu.Unlock()
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", "", fmt.Errorf("role %q in project %q is busy", role, projectID)
	}
	b.mu.Unlock()

	// Update status to picked immediately since sync delivery is a direct handoff
	if err := b.persistStatus(projectID, taskID, role, title, StatusPicked); err != nil {
		// Log error but continue as task was delivered
	}

	defer func() {
		b.mu.Lock()
		if projectTasks, ok := b.tasks[projectID]; ok {
			delete(projectTasks, taskID)
			if len(projectTasks) == 0 {
				delete(b.tasks, projectID)
			}
		}
		b.mu.Unlock()
	}()

	select {
	case result := <-task.done:
		return taskID, result, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// CreateTaskAsync enqueues a task for a role and returns immediately with a generated ID.
func (b *Broker) CreateTaskAsync(projectID, role, title, taskMD string) (string, error) {
	if role == "" || title == "" || taskMD == "" {
		return "", fmt.Errorf("role, title and task_md are required")
	}
	if len(title) > 200 {
		return "", fmt.Errorf("title too long (max 200 characters)")
	}

	var taskID string
	var err error

	// Retry on collision (ID already exists on disk)
	for i := 0; i < 5; i++ {
		taskID = b.generateID()
		err = b.persistTask(projectID, taskID, taskMD)
		if err == nil {
			break
		}
		// If it's anything other than "already exists", fail immediately
		if !strings.Contains(err.Error(), "already exists") {
			return "", fmt.Errorf("persistence failed: %w", err)
		}
	}
	if err != nil {
		return "", fmt.Errorf("failed to generate unique task_id after 5 attempts: %w", err)
	}

	if err := b.persistStatus(projectID, taskID, role, title, StatusQueued); err != nil {
		os.RemoveAll(b.taskDir(projectID, taskID)) // Cleanup partial state
		return "", fmt.Errorf("failed to persist status: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// No need to check exists in b.tasks because of generateTaskID + O_EXCL in persistTask
	// unless we really hit a 1 in 2^128 collision with an active memory task.

	task := &Task{
		ID:        taskID,
		ProjectID: projectID,
		Role:      role,
		Title:     title,
		MD:        taskMD,
		done:      make(chan string, 1),
	}
	if b.tasks[projectID] == nil {
		b.tasks[projectID] = make(map[string]*Task)
	}
	b.tasks[projectID][taskID] = task

	// If a sync listener is already waiting for this role, deliver directly.
	if projectListeners, ok := b.listeners[projectID]; ok {
		if ch, hasListener := projectListeners[role]; hasListener {
			if err := b.persistStatus(projectID, taskID, role, title, StatusPicked); err == nil {
				select {
				case ch <- task:
					return taskID, nil
				default:
				}
			}
		}
	}

	if b.asyncQueue[projectID] == nil {
		b.asyncQueue[projectID] = make(map[string][]*Task)
	}
	b.asyncQueue[projectID][role] = append(b.asyncQueue[projectID][role], task)

	return taskID, nil
}

// SolveTask submits a result for a task ID and unblocks the creator.
func (b *Broker) SolveTask(projectID, taskID, resultMD string) error {
	if taskID == "" || resultMD == "" {
		return fmt.Errorf("task_id and result_md are required")
	}

	b.mu.Lock()
	projectTasks, ok := b.tasks[projectID]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("project %q not found or has no active tasks", projectID)
	}
	task, exists := projectTasks[taskID]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("task %q not found in memory for project %q", taskID, projectID)
	}

	if err := b.persistResult(projectID, taskID, resultMD); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("failed to persist result: %w", err)
	}
	if err := b.persistStatus(projectID, taskID, task.Role, task.Title, StatusSolved); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("failed to update status to solved: %w", err)
	}

	delete(projectTasks, taskID)
	if len(projectTasks) == 0 {
		delete(b.tasks, projectID)
	}
	b.mu.Unlock()

	select {
	case task.done <- resultMD:
	default:
	}
	return nil
}

// GetTaskStatus returns the status metadata from disk.
func (b *Broker) GetTaskStatus(projectID, taskID string) (*StatusMetadata, error) {
	if !isSafeID(taskID) {
		return nil, fmt.Errorf("invalid task_id")
	}
	path := filepath.Join(b.taskDir(projectID, taskID), "status.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("task %q not found in project %q", taskID, projectID)
		}
		return nil, fmt.Errorf("failed to read status: %w", err)
	}
	var meta StatusMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}
	return &meta, nil
}

// GetTaskResult returns the result markdown from disk if available.
func (b *Broker) GetTaskResult(projectID, taskID string) (string, error) {
	if !isSafeID(taskID) {
		return "", fmt.Errorf("invalid task_id")
	}
	path := filepath.Join(b.taskDir(projectID, taskID), "result.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Check if task exists at all
			if _, statErr := os.Stat(filepath.Join(b.taskDir(projectID, taskID), "task.md")); statErr == nil {
				return "", nil // Task exists but no result yet
			}
			return "", fmt.Errorf("task %q not found in project %q", taskID, projectID)
		}
		return "", fmt.Errorf("failed to read result: %w", err)
	}
	return string(data), nil
}
