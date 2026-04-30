package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// Broker manages the coordination between task creators and role listeners.
type Broker struct {
	mu         sync.Mutex
	listeners  map[string]map[string]chan *Task // projectID -> role -> chan
	tasks      map[string]map[string]*Task      // projectID -> taskID -> *Task
	asyncQueue map[string]map[string][]*Task    // projectID -> role -> []*Task
	dataDir    string
	promptsDir string

	EnableSync  bool
	EnableAsync bool

	// Hooks for testing
	generateID    func() string
	persistStatus func(projectID, taskID, role, title string, status TaskStatus) error
}

// NewBroker initializes and returns a new Broker with persistence support.
func NewBroker(dataDir, promptsDir string, enableSync, enableAsync bool) (*Broker, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	if promptsDir == "" {
		promptsDir = "prompts"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create prompts directory: %w", err)
	}

	b := &Broker{
		listeners:   make(map[string]map[string]chan *Task),
		tasks:       make(map[string]map[string]*Task),
		asyncQueue:  make(map[string]map[string][]*Task),
		dataDir:     dataDir,
		promptsDir:  promptsDir,
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
		err = b.persistTask(projectID, taskID, taskMD)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "already exists") {
			return "", fmt.Errorf("persistence failed: %w", err)
		}
	}
	if err != nil {
		return "", fmt.Errorf("failed to generate unique task_id after 5 attempts: %w", err)
	}

	if err := b.persistStatus(projectID, taskID, role, title, StatusQueued); err != nil {
		os.RemoveAll(b.taskDir(projectID, taskID))
		return "", fmt.Errorf("failed to persist status: %w", err)
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
		if ch, hasListener := projectListeners[role]; hasListener {
			// Mark as picked on disk before attempting delivery
			if err := b.persistStatus(projectID, taskID, role, title, StatusPicked); err != nil {
				os.RemoveAll(b.taskDir(projectID, taskID))
				return "", fmt.Errorf("failed to update status to picked: %w", err)
			}

			select {
			case ch <- task:
				return taskID, nil
			default:
				// Listener was busy/disappeared, rollback status on disk to queued
				if err := b.persistStatus(projectID, taskID, role, title, StatusQueued); err != nil {
					// Log but continue to ensure task is at least in the async queue
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

// ReportProgress sends an intermediate progress message for an in-flight task.
// Non-blocking: if the progress buffer (32) is full, the message is dropped with a log warning.
func (b *Broker) ReportProgress(projectID, taskID, message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	projectTasks, ok := b.tasks[projectID]
	if !ok {
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	task, exists := projectTasks[taskID]
	if !exists {
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}

	select {
	case task.progress <- message:
	default:
		log.Printf("progress buffer full for task %s, dropping message", taskID)
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

			if err := b.persistStatus(projectID, task.ID, role, task.Title, StatusPicked); err != nil {
				b.mu.Unlock()
				return nil, "", fmt.Errorf("failed to update status to picked: %w", err)
			}

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
		if _, exists := projectListeners[role]; exists {
			b.mu.Unlock()
			return nil, "", fmt.Errorf("role %q already has a listener in project %q", role, projectID)
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

	var timeoutCh <-chan time.Time
	if timeoutMs > 0 {
		timeoutCh = time.After(time.Duration(timeoutMs) * time.Millisecond)
	}

	select {
	case task := <-ch:
		return task, "picked", nil
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-timeoutCh:
		return nil, "timeout", nil
	}
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
	close(task.progress)
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

func (b *Broker) GetTaskMD(projectID, taskID string) (string, error) {
	if !isSafeID(taskID) {
		return "", fmt.Errorf("invalid task_id")
	}
	path := filepath.Join(b.taskDir(projectID, taskID), "task.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read task.md: %w", err)
	}
	return string(data), nil
}

func (b *Broker) ListTasks(projectID, role, status string) ([]StatusMetadata, error) {
	var tasks []StatusMetadata

	dir := filepath.Join(b.dataDir, projectID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return tasks, nil
		}
		return nil, fmt.Errorf("failed to read project dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskID := entry.Name()
		if !isSafeID(taskID) {
			continue
		}

		meta, err := b.GetTaskStatus(projectID, taskID)
		if err != nil {
			continue // Skip errors
		}

		if role != "" && meta.Role != role {
			continue
		}
		if status != "" && string(meta.Status) != status {
			continue
		}

		tasks = append(tasks, *meta)
	}
	return tasks, nil
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
