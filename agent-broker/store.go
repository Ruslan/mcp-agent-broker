package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrTaskExists is returned by InsertTask when a task with the same ID already exists.
var ErrTaskExists = errors.New("task already exists")

// Store abstracts all persistence operations. Implementations may use SQLite,
// an in-memory map (tests), or any other backend.
type Store interface {
	// InsertTask creates a new task row with status=queued.
	// Returns ErrTaskExists on primary-key collision.
	InsertTask(projectID, taskID, role, title, taskMD string) error
	// UpdateStatus changes the status of an existing task.
	UpdateStatus(projectID, taskID string, status TaskStatus) error
	// SaveResult atomically stores the result and sets status=solved.
	SaveResult(projectID, taskID, resultMD string) error
	// DeleteTask removes a task row (used for cleanup on failed delivery).
	DeleteTask(projectID, taskID string) error
	// AppendProgress adds a progress message to the task's log.
	AppendProgress(projectID, taskID, message string) error
	// GetProgress retrieves all progress messages for a task.
	GetProgress(projectID, taskID string) ([]string, error)

	GetStatus(projectID, taskID string) (*StatusMetadata, error)
	GetTaskMD(projectID, taskID string) (string, error)
	GetResult(projectID, taskID string) (string, error)
	ListTasks(projectID, role, status string) ([]StatusMetadata, error)
	ListProjects() ([]string, error)

	Close() error
}

// SQLiteStore is the production Store backed by a SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database at path.
// Pass ":memory:" for an in-process database (useful in tests).
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite at %q: %w", path, err)
	}
	// Serialize all writes through a single connection to avoid SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}
	if err := sqliteMigrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func sqliteMigrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			project_id TEXT NOT NULL,
			task_id    TEXT NOT NULL,
			role       TEXT NOT NULL,
			title      TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'queued',
			task_md    TEXT NOT NULL,
			result_md  TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (project_id, task_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_project_role   ON tasks (project_id, role)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_project_status ON tasks (project_id, status)`,
		`CREATE TABLE IF NOT EXISTS task_progress (
			project_id TEXT NOT NULL,
			task_id    TEXT NOT NULL,
			sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
			message    TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (project_id, task_id) REFERENCES tasks (project_id, task_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_progress_task ON task_progress (project_id, task_id)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) InsertTask(projectID, taskID, role, title, taskMD string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO tasks (project_id, task_id, role, title, status, task_md, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
		projectID, taskID, role, title, taskMD, now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrTaskExists
		}
		return fmt.Errorf("failed to insert task: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateStatus(projectID, taskID string, status TaskStatus) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, updated_at = ? WHERE project_id = ? AND task_id = ?`,
		string(status), now, projectID, taskID,
	)
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	return nil
}

func (s *SQLiteStore) SaveResult(projectID, taskID, resultMD string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`UPDATE tasks SET result_md = ?, status = 'solved', updated_at = ? WHERE project_id = ? AND task_id = ?`,
		resultMD, now, projectID, taskID,
	)
	if err != nil {
		return fmt.Errorf("failed to save result: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	return nil
}

func (s *SQLiteStore) DeleteTask(projectID, taskID string) error {
	_, err := s.db.Exec(
		`DELETE FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	)
	return err
}

func (s *SQLiteStore) AppendProgress(projectID, taskID, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO task_progress (project_id, task_id, message, created_at)
		 VALUES (?, ?, ?, ?)`,
		projectID, taskID, message, now,
	)
	return err
}

func (s *SQLiteStore) GetProgress(projectID, taskID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT message FROM task_progress WHERE project_id = ? AND task_id = ? ORDER BY sequence ASC`,
		projectID, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to read progress: %w", err)
	}
	defer rows.Close()

	var progress []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return nil, fmt.Errorf("failed to scan progress: %w", err)
		}
		progress = append(progress, msg)
	}
	if progress == nil {
		progress = []string{}
	}
	return progress, rows.Err()
}

func (s *SQLiteStore) GetStatus(projectID, taskID string) (*StatusMetadata, error) {
	row := s.db.QueryRow(
		`SELECT project_id, task_id, role, title, status, created_at, updated_at
		 FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	)
	var m StatusMetadata
	var createdAt, updatedAt string
	err := row.Scan(&m.ProjectID, &m.TaskID, &m.Role, &m.Title, &m.Status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read task status: %w", err)
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &m, nil
}

func (s *SQLiteStore) GetTaskMD(projectID, taskID string) (string, error) {
	var taskMD string
	err := s.db.QueryRow(
		`SELECT task_md FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&taskMD)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to read task_md: %w", err)
	}
	return taskMD, nil
}

func (s *SQLiteStore) GetResult(projectID, taskID string) (string, error) {
	var resultMD sql.NullString
	err := s.db.QueryRow(
		`SELECT result_md FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&resultMD)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("task %q not found in project %q", taskID, projectID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to read result_md: %w", err)
	}
	if !resultMD.Valid {
		return "", nil
	}
	return resultMD.String, nil
}

func (s *SQLiteStore) ListTasks(projectID, role, status string) ([]StatusMetadata, error) {
	query := `SELECT project_id, task_id, role, title, status, created_at, updated_at
	          FROM tasks WHERE project_id = ?`
	args := []any{projectID}
	if role != "" {
		query += " AND role = ?"
		args = append(args, role)
	}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []StatusMetadata
	for rows.Next() {
		var m StatusMetadata
		var createdAt, updatedAt string
		if err := rows.Scan(&m.ProjectID, &m.TaskID, &m.Role, &m.Title, &m.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan task row: %w", err)
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		tasks = append(tasks, m)
	}
	if tasks == nil {
		tasks = []StatusMetadata{}
	}
	return tasks, rows.Err()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) ListProjects() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT project_id FROM tasks ORDER BY project_id ASC")
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("failed to scan project row: %w", err)
		}
		projects = append(projects, p)
	}
	if projects == nil {
		projects = []string{}
	}
	return projects, rows.Err()
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
