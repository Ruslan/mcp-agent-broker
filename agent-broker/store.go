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

// TaskRecord is a full task row returned by LoadActiveTasks (used for startup recovery).
type TaskRecord struct {
	ProjectID string
	TaskID    string
	Role      string
	Title     string
	TaskMD    string
	Status    TaskStatus
}

// Store abstracts all persistence operations. Implementations may use SQLite,
// an in-memory map (tests), or any other backend.
type Store interface {
	// InsertTask creates a new task row with status=queued.
	// Returns ErrTaskExists on primary-key collision.
	InsertTask(projectID, taskID, role, title, taskMD string) error
	// UpdateStatus changes the status of an existing task.
	UpdateStatus(projectID, taskID string, status TaskStatus) error
	// UpdateStatusIfCurrent changes status only if the task is still in the expected status.
	UpdateStatusIfCurrent(projectID, taskID string, current, next TaskStatus) (bool, error)
	// SaveResult atomically stores the result and sets status=solved.
	SaveResult(projectID, taskID, resultMD string) error
	// ClearResult clears the result_md field (used when admin resets task to queued).
	ClearResult(projectID, taskID string) error
	// DeleteTask removes a task row (used for cleanup on failed delivery).
	DeleteTask(projectID, taskID string) error
	// AppendProgress adds a progress message to the task's log.
	AppendProgress(projectID, taskID, message string) error
	// GetProgress retrieves all progress messages for a task.
	GetProgress(projectID, taskID string) ([]string, error)

	IncrementResultViewCount(projectID, taskID string) (int, error)
	GetStatus(projectID, taskID string) (*StatusMetadata, error)
	GetTaskMD(projectID, taskID string) (string, error)
	GetResult(projectID, taskID string) (string, error)
	ListTasks(projectID, role, status string, limit, offset int) ([]StatusMetadata, error)
	CountTasks(projectID, role, status string) (int, error)
	ListProjects() ([]string, error)
	// LoadActiveTasks returns all queued and picked tasks for memory restoration on startup.
	LoadActiveTasks() ([]TaskRecord, error)

	// InsertPollToken persists a scoped poller capability token. Implementations
	// prune expired rows on write so the table stays bounded.
	InsertPollToken(token, projectID, scopeKind, scopeValue string, createdAt, expiresAt time.Time) error
	// GetActivePollToken returns an existing unexpired token for the given scope,
	// or nil if none — so repeated mints for the same scope reuse one token.
	GetActivePollToken(projectID, scopeKind, scopeValue string) (*PollTokenScope, error)
	// RenewPollToken validates a token and, if live, slides its expiry forward by
	// ttl (capped at createdAt+maxLifetime), returning the refreshed scope. The
	// bool reports whether a row EXISTED (even if expired/capped), so the caller
	// can distinguish an expired token (found=true, scope=nil → "expired") from an
	// unknown one (found=false → 404, as if the URL never existed).
	RenewPollToken(token string, ttl, maxLifetime time.Duration) (scope *PollTokenScope, found bool, err error)

	// Work tokens are task-scoped worker capabilities. Unlike role poll tokens,
	// they authorize only progress/solve for one task and carry the owning
	// project so a worker in another project never needs tenant-wide access.
	InsertWorkToken(token, projectID, taskID string, createdAt, expiresAt time.Time) error
	GetActiveWorkToken(projectID, taskID string) (*WorkTokenScope, error)
	GetWorkToken(token string) (*WorkTokenScope, error)

	Close() error
}

// PollTokenScope is a scoped poller capability: the narrow authority to poll one
// role queue (kind="role", value=<role>) or await one task (kind="task",
// value=<task_id>) within one project. It carries a sliding expiry (renewed on
// each poll) and, via CreatedAt + an absolute cap, a hard maximum lifetime.
type PollTokenScope struct {
	Token     string
	ProjectID string
	Kind      string
	Value     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// WorkTokenScope binds an opaque worker capability to exactly one task owner.
type WorkTokenScope struct {
	Token     string
	ProjectID string
	TaskID    string
	CreatedAt time.Time
	ExpiresAt time.Time
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
		`ALTER TABLE tasks ADD COLUMN result_view_count INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS poll_tokens (
			token       TEXT PRIMARY KEY,
			project_id  TEXT NOT NULL,
			scope_kind  TEXT NOT NULL,
			scope_value TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			expires_at  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_poll_tokens_scope ON poll_tokens (project_id, scope_kind, scope_value)`,
		`CREATE TABLE IF NOT EXISTS work_tokens (
			token       TEXT PRIMARY KEY,
			project_id  TEXT NOT NULL,
			task_id     TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			expires_at  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_work_tokens_task ON work_tokens (project_id, task_id, expires_at)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") || strings.Contains(err.Error(), "already exists") {
				continue
			}
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

func (s *SQLiteStore) UpdateStatusIfCurrent(projectID, taskID string, current, next TaskStatus) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, updated_at = ? WHERE project_id = ? AND task_id = ? AND status = ?`,
		string(next), now, projectID, taskID, string(current),
	)
	if err != nil {
		return false, fmt.Errorf("failed to update status: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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

func (s *SQLiteStore) ClearResult(projectID, taskID string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET result_md = NULL WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	)
	return err
}

func (s *SQLiteStore) DeleteTask(projectID, taskID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM work_tokens WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	); err != nil {
		return err
	}
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

func (s *SQLiteStore) IncrementResultViewCount(projectID, taskID string) (int, error) {
	_, err := s.db.Exec(
		`UPDATE tasks SET result_view_count = result_view_count + 1 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to increment result_view_count: %w", err)
	}
	var count int
	s.db.QueryRow(
		`SELECT result_view_count FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&count)
	return count, nil
}

func (s *SQLiteStore) GetStatus(projectID, taskID string) (*StatusMetadata, error) {
	row := s.db.QueryRow(
		`SELECT project_id, task_id, role, title, status, created_at, updated_at, result_view_count
		 FROM tasks WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	)
	var m StatusMetadata
	var createdAt, updatedAt string
	err := row.Scan(&m.ProjectID, &m.TaskID, &m.Role, &m.Title, &m.Status, &createdAt, &updatedAt, &m.ResultViewCount)
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

func (s *SQLiteStore) ListTasks(projectID, role, status string, limit, offset int) ([]StatusMetadata, error) {
	query := `SELECT project_id, task_id, role, title, status, created_at, updated_at, result_view_count
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
	query += " ORDER BY created_at DESC, rowid DESC"

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	} else if offset > 0 {
		query += " LIMIT -1"
	}
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []StatusMetadata
	for rows.Next() {
		var m StatusMetadata
		var createdAt, updatedAt string
		if err := rows.Scan(&m.ProjectID, &m.TaskID, &m.Role, &m.Title, &m.Status, &createdAt, &updatedAt, &m.ResultViewCount); err != nil {
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

func (s *SQLiteStore) CountTasks(projectID, role, status string) (int, error) {
	query := `SELECT COUNT(*) FROM tasks WHERE project_id = ?`
	args := []any{projectID}
	if role != "" {
		query += " AND role = ?"
		args = append(args, role)
	}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}

	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count tasks: %w", err)
	}
	return count, nil
}

func (s *SQLiteStore) LoadActiveTasks() ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT project_id, task_id, role, title, task_md, status
		 FROM tasks WHERE status IN ('queued', 'picked') ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load active tasks: %w", err)
	}
	defer rows.Close()

	var records []TaskRecord
	for rows.Next() {
		var r TaskRecord
		if err := rows.Scan(&r.ProjectID, &r.TaskID, &r.Role, &r.Title, &r.TaskMD, &r.Status); err != nil {
			return nil, fmt.Errorf("failed to scan active task: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// InsertPollToken persists a scoped token and opportunistically prunes expired
// rows so the table trends back to empty on an idle broker. Timestamps are
// stored as RFC3339 UTC, which is lexicographically ordered, so string
// comparison against a formatted "now" is a valid expiry test.
func (s *SQLiteStore) InsertPollToken(token, projectID, scopeKind, scopeValue string, createdAt, expiresAt time.Time) error {
	// Prune only rows past their absolute lifetime (created_at + maxLifetime), NOT
	// merely past their sliding expiry — a recently-expired row must survive so
	// GET /poll can answer "expired" (not 404) to a legit poller that just stalled.
	// created_at is RFC3339 UTC (lexicographically ordered), so a string compare
	// against the cutoff is valid.
	capCutoff := time.Now().UTC().Add(-pollTokenMaxLifetime).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM poll_tokens WHERE created_at <= ?`, capCutoff); err != nil {
		return fmt.Errorf("failed to prune expired poll tokens: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO poll_tokens (token, project_id, scope_kind, scope_value, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		token, projectID, scopeKind, scopeValue, createdAt.UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("failed to insert poll token: %w", err)
	}
	return nil
}

// GetActivePollToken returns the most-distant unexpired token for a scope, so a
// repeated mint reuses one token instead of accumulating rows.
func (s *SQLiteStore) GetActivePollToken(projectID, scopeKind, scopeValue string) (*PollTokenScope, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	row := s.db.QueryRow(
		`SELECT token, project_id, scope_kind, scope_value, created_at, expires_at
		 FROM poll_tokens
		 WHERE project_id = ? AND scope_kind = ? AND scope_value = ? AND expires_at > ?
		 ORDER BY expires_at DESC LIMIT 1`,
		projectID, scopeKind, scopeValue, now,
	)
	return scanPollToken(row)
}

// RenewPollToken slides a live token's expiry forward by ttl, capped at
// createdAt+maxLifetime. The row is loaded regardless of expiry so the caller can
// tell apart:
//   - found=false, scope=nil → the token never existed (or aged past its cap and
//     was pruned): the endpoint answers 404.
//   - found=true,  scope=nil → the token existed but is expired (slid out) or hit
//     its hard cap: the endpoint answers "expired".
//   - scope!=nil            → live and renewed.
func (s *SQLiteStore) RenewPollToken(token string, ttl, maxLifetime time.Duration) (*PollTokenScope, bool, error) {
	now := time.Now().UTC()
	row := s.db.QueryRow(
		`SELECT token, project_id, scope_kind, scope_value, created_at, expires_at
		 FROM poll_tokens WHERE token = ?`,
		token,
	)
	scope, err := scanPollToken(row)
	if err != nil {
		return nil, false, err
	}
	if scope == nil {
		return nil, false, nil // unknown
	}
	hardCap := scope.CreatedAt.Add(maxLifetime)
	if !now.Before(hardCap) {
		// Past the absolute lifetime — expired; drop the row so it can't linger.
		_, _ = s.db.Exec(`DELETE FROM poll_tokens WHERE token = ?`, token)
		return nil, true, nil
	}
	if !now.Before(scope.ExpiresAt) {
		// Slid out of its sliding window (poller stalled) — expired, keep the row
		// until the hard cap so repeated polls keep getting "expired", not 404.
		return nil, true, nil
	}
	newExp := now.Add(ttl)
	if newExp.After(hardCap) {
		newExp = hardCap
	}
	if _, err := s.db.Exec(`UPDATE poll_tokens SET expires_at = ? WHERE token = ?`, newExp.UTC().Format(time.RFC3339), token); err != nil {
		return nil, true, fmt.Errorf("failed to renew poll token: %w", err)
	}
	scope.ExpiresAt = newExp
	return scope, true, nil
}

func scanPollToken(row *sql.Row) (*PollTokenScope, error) {
	var (
		scope   PollTokenScope
		created string
		expires string
	)
	err := row.Scan(&scope.Token, &scope.ProjectID, &scope.Kind, &scope.Value, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan poll token: %w", err)
	}
	if t, perr := time.Parse(time.RFC3339, created); perr == nil {
		scope.CreatedAt = t
	}
	if t, perr := time.Parse(time.RFC3339, expires); perr == nil {
		scope.ExpiresAt = t
	}
	return &scope, nil
}

// InsertWorkToken persists a restart-safe task capability and opportunistically
// removes capabilities past their fixed lifetime.
func (s *SQLiteStore) InsertWorkToken(token, projectID, taskID string, createdAt, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM work_tokens WHERE expires_at <= ?`, now); err != nil {
		return fmt.Errorf("failed to prune expired work tokens: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO work_tokens (token, project_id, task_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		token, projectID, taskID, createdAt.UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("failed to insert work token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetActiveWorkToken(projectID, taskID string) (*WorkTokenScope, error) {
	row := s.db.QueryRow(
		`SELECT token, project_id, task_id, created_at, expires_at
		 FROM work_tokens
		 WHERE project_id = ? AND task_id = ? AND expires_at > ?
		 ORDER BY expires_at DESC LIMIT 1`,
		projectID, taskID, time.Now().UTC().Format(time.RFC3339),
	)
	return scanWorkToken(row)
}

func (s *SQLiteStore) GetWorkToken(token string) (*WorkTokenScope, error) {
	row := s.db.QueryRow(
		`SELECT token, project_id, task_id, created_at, expires_at
		 FROM work_tokens WHERE token = ? AND expires_at > ?`,
		token, time.Now().UTC().Format(time.RFC3339),
	)
	return scanWorkToken(row)
}

func scanWorkToken(row *sql.Row) (*WorkTokenScope, error) {
	var scope WorkTokenScope
	var created, expires string
	err := row.Scan(&scope.Token, &scope.ProjectID, &scope.TaskID, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan work token: %w", err)
	}
	scope.CreatedAt, _ = time.Parse(time.RFC3339, created)
	scope.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	return &scope, nil
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
