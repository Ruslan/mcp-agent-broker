package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// JSON-RPC 2.0 types
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return e.Message
}

const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
	ErrApp            = -32000

	defaultListTasksLimit = 20
)

type JSONRPCHandler struct {
	broker *Broker
}

func (h *JSONRPCHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	projectID, err := h.validateProjectID(r)
	if err != nil {
		h.sendError(w, nil, ErrInvalidRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":               true,
		"service":          "agent-broker",
		"version":          ServerVersion,
		"protocol_version": ProtocolVersion,
		"enable_sync":      h.broker.EnableSync,
		"enable_async":     h.broker.EnableAsync,
		"project_id":       projectID,
	})
}

func (h *JSONRPCHandler) validateProjectID(r *http.Request) (string, error) {
	projectID := r.Header.Get("X-Project-Id")
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "default"
	}
	if !isSafeID(projectID) {
		return "", fmt.Errorf("Invalid project_id: %q", projectID)
	}
	return projectID, nil
}

func (h *JSONRPCHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	projectID, err := h.validateProjectID(r)
	if err != nil {
		h.sendError(w, nil, ErrInvalidRequest, err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendError(w, nil, ErrParse, "Parse error or body too large")
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendError(w, nil, ErrParse, "Parse error")
		return
	}

	if req.JSONRPC != "2.0" || req.Method == "" {
		h.sendError(w, req.ID, ErrInvalidRequest, "Invalid request: missing jsonrpc version or method")
		return
	}

	isNotification := req.ID == nil
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	ctx := r.Context()
	// Base URL for poll_url values handed back to clients (derived per-request so
	// a remote agent gets a reachable host; overridable via BROKER_PUBLIC_URL).
	pollBase := pollBaseURL(r, h.broker.PublicURL, h.broker.TrustForwarded)

	var result any
	var rpcErr *RPCError
	var toolName string
	var toolStartedAt time.Time
	var requeueOnWriteFailure string
	var requeueOwnerProject string

	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities": map[string]any{
				"tools":   map[string]any{},
				"prompts": map[string]any{},
			},
			"serverInfo": map[string]any{"name": "agent-broker", "version": ServerVersion},
		}

	case "prompts/list":
		prompts, err := h.broker.ListPrompts()
		if err != nil {
			rpcErr = &RPCError{Code: ErrApp, Message: err.Error()}
		} else {
			result = map[string]any{
				"prompts": prompts,
			}
		}

	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			rpcErr = &RPCError{Code: ErrInvalidParams, Message: "Invalid params: name is required"}
		} else {
			prompt, content, err := h.broker.GetPrompt(p.Name, p.Arguments)
			if err != nil {
				rpcErr = &RPCError{Code: ErrApp, Message: err.Error()}
			} else {
				result = map[string]any{
					"name":        prompt.Name,
					"title":       prompt.Title,
					"description": prompt.Description,
					"arguments":   prompt.Arguments,
					"messages": []any{
						map[string]any{
							"role": "user",
							"content": map[string]any{
								"type": "text",
								"text": content,
							},
						},
					},
				}
			}
		}

	case "tools/list":
		var tools []any

		// create_task is always available
		tools = append(tools, map[string]any{
			"name":        "create_task",
			"description": "Creates a task and returns immediately with a task_id.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"role":    map[string]any{"type": "string"},
					"title":   map[string]any{"type": "string", "description": "Short task title (max 200 chars)"},
					"task_md": map[string]any{"type": "string"},
				},
				"required": []string{"role", "title", "task_md"},
			},
		})

		if h.broker.EnableSync {
			tools = append(tools, map[string]any{
				"name":        "await_task",
				"description": "Blocks until the task reaches a terminal state or timeout/cancel.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":    map[string]any{"type": "string"},
						"timeout_ms": map[string]any{"type": "integer"},
					},
					"required": []string{"task_id"},
				},
			})
		}

		// listen_role schema adapted to flags
		modes := []string{}
		if h.broker.EnableSync {
			modes = append(modes, "wait")
		}
		if h.broker.EnableAsync {
			modes = append(modes, "poll")
		}

		tools = append(tools, map[string]any{
			"name":        "listen_role",
			"description": "Single worker-facing tool for both blocking wait and non-blocking check. Modes: wait, poll.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"role":       map[string]any{"type": "string"},
					"mode":       map[string]any{"type": "string", "enum": modes},
					"timeout_ms": map[string]any{"type": "integer"},
				},
				"required": []string{"role", "mode"},
			},
		})

		// Discovery and management tools always available
		tools = append(tools,
			map[string]any{
				"name":        "list_tasks",
				"description": "Returns up to 20 recent tasks owned by this project plus global tasks assigned to it. Active assigned tasks are listed first. Filters allowed.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"role":   map[string]any{"type": "string"},
						"status": map[string]any{"type": "string"},
					},
				},
			},
			map[string]any{
				"name":        "get_task",
				"description": "Returns detailed content for an owned task or a global task assigned to this worker project. A work_token can also authorize access.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":           map[string]any{"type": "string"},
						"include_task_md":   map[string]any{"type": "boolean"},
						"include_result_md": map[string]any{"type": "boolean"},
						"work_token":        map[string]any{"type": "string", "description": "Opaque capability returned with a global task"},
					},
					"required": []string{"task_id"},
				},
			},
			map[string]any{
				"name":        "solve_task",
				"description": "Submit the final markdown report for a task.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":    map[string]any{"type": "string"},
						"result_md":  map[string]any{"type": "string"},
						"work_token": map[string]any{"type": "string", "description": "Opaque capability returned with a global task"},
					},
					"required": []string{"task_id", "result_md"},
				},
			},
			map[string]any{
				"name":        "progress_task",
				"description": "Send an intermediate progress update for a task without completing it. Call multiple times during long-running work.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":    map[string]any{"type": "string"},
						"message":    map[string]any{"type": "string", "description": "Short human-readable status update (max 500 chars)"},
						"work_token": map[string]any{"type": "string", "description": "Opaque capability returned with a global task"},
					},
					"required": []string{"task_id", "message"},
				},
			},
		)

		result = map[string]any{
			"tools": tools,
		}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			rpcErr = &RPCError{Code: ErrInvalidParams, Message: "Invalid params"}
		} else {
			toolName = params.Name
			toolStartedAt = time.Now()
			res, err := h.handleToolCall(ctx, projectID, pollBase, params.Name, params.Arguments)
			if err != nil {
				elapsed := time.Since(toolStartedAt)
				log.Printf("tool=%s project=%s err=%q elapsed=%s", params.Name, projectID, err.Error(), elapsed.Round(time.Millisecond))
				rpcErr = &RPCError{Code: ErrApp, Message: err.Error()}
			} else {
				resJSON, _ := json.Marshal(res)
				result = map[string]any{
					"content": []any{map[string]any{"type": "text", "text": string(resJSON)}},
				}
				if params.Name == "listen_role" {
					requeueOnWriteFailure = listenRoleTaskID(res)
					requeueOwnerProject = projectID
					if token := listenRoleWorkToken(res); token != "" && requeueOnWriteFailure != "" {
						if owner, resolveErr := h.broker.ResolveWorkerProject(projectID, requeueOnWriteFailure, token); resolveErr == nil {
							requeueOwnerProject = owner
						}
					}
				}
			}
		}

	default:
		rpcErr = &RPCError{Code: ErrMethodNotFound, Message: fmt.Sprintf("Method not found: %s", req.Method)}
	}

	if rpcErr != nil {
		if err := h.sendError(w, req.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("jsonrpc error response write failed: method=%s project=%s err=%v", req.Method, projectID, err)
		}
		return
	}
	if err := h.sendResult(w, req.ID, result); err != nil {
		if requeueOnWriteFailure != "" {
			if requeueErr := h.broker.RequeuePickedTask(requeueOwnerProject, requeueOnWriteFailure, "listen_role.response_write_failed"); requeueErr != nil {
				log.Printf("failed to requeue task after response write failure: project=%s task=%s err=%v", requeueOwnerProject, requeueOnWriteFailure, requeueErr)
			}
		}
		if toolName != "" {
			log.Printf("tool=%s project=%s write_err=%q elapsed=%s", toolName, projectID, err.Error(), time.Since(toolStartedAt).Round(time.Millisecond))
		} else {
			log.Printf("jsonrpc result response write failed: method=%s project=%s err=%v", req.Method, projectID, err)
		}
		return
	}
	if toolName != "" {
		log.Printf("tool=%s project=%s ok elapsed=%s", toolName, projectID, time.Since(toolStartedAt).Round(time.Millisecond))
	}
}

func listenRoleTaskID(res any) string {
	m, ok := res.(map[string]any)
	if !ok {
		return ""
	}
	task, ok := m["task"].(map[string]any)
	if !ok || task == nil {
		return ""
	}
	taskID, _ := task["task_id"].(string)
	return taskID
}

func listenRoleWorkToken(res any) string {
	m, ok := res.(map[string]any)
	if !ok {
		return ""
	}
	task, ok := m["task"].(map[string]any)
	if !ok || task == nil {
		return ""
	}
	token, _ := task["work_token"].(string)
	return token
}

// mintPollURL mints a scoped poll token for (projectID, kind, value) and returns
// the full capability URL for it. Best-effort: on any error it returns "" so the
// caller simply omits poll_url rather than failing the tool.
func (h *JSONRPCHandler) mintPollURL(projectID, base, kind, value string) string {
	tok, err := h.broker.MintPollToken(projectID, kind, value)
	if err != nil || tok == nil {
		return ""
	}
	return buildPollURL(base, tok.Token)
}

func (h *JSONRPCHandler) handleToolCall(ctx context.Context, projectID, pollBase, name string, args json.RawMessage) (any, error) {
	switch name {
	case "create_task":
		var p struct {
			Role   string `json:"role"`
			Title  string `json:"title"`
			TaskMD string `json:"task_md"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" || p.Title == "" || p.TaskMD == "" {
			return nil, fmt.Errorf("invalid arguments: role, title and task_md are required")
		}
		taskID, err := h.broker.CreateTask(projectID, p.Role, p.Title, p.TaskMD)
		if err != nil {
			return nil, err
		}
		resp := map[string]any{
			"task_id": taskID,
			"status":  "queued",
		}
		// Hand back a task-scoped poll_url so the dispatcher can arm a poller with
		// a capability URL (no master key). Best-effort — never fails task creation.
		if url := h.mintPollURL(projectID, pollBase, PollScopeTask, taskID); url != "" {
			resp["poll_url"] = url
		}
		return resp, nil

	case "await_task":
		if !h.broker.EnableSync {
			return nil, fmt.Errorf("tool \"await_task\" is disabled by server configuration (ENABLE_SYNC=false)")
		}
		var p struct {
			TaskID    string `json:"task_id"`
			TimeoutMs int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("invalid arguments: task_id is required")
		}
		status, res, progress, err := h.broker.AwaitTask(ctx, projectID, p.TaskID, p.TimeoutMs)
		if err != nil {
			return nil, err
		}
		resp := map[string]any{
			"task_id": p.TaskID,
			"status":  status,
		}
		if status == string(StatusSolved) {
			resp["result_md"] = res
			_, _ = h.broker.IncrementResultViewCount(projectID, p.TaskID)
		}
		if len(progress) > 0 {
			resp["progress"] = progress
		}
		return resp, nil

	case "listen_role":
		var p struct {
			Role      string `json:"role"`
			Mode      string `json:"mode"`
			TimeoutMs int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" || p.Mode == "" {
			return nil, fmt.Errorf("invalid arguments: role and mode are required")
		}

		if p.Mode == "wait" && !h.broker.EnableSync {
			return nil, fmt.Errorf("mode \"wait\" is disabled by server configuration (ENABLE_SYNC=false)")
		}
		if p.Mode == "poll" && !h.broker.EnableAsync {
			return nil, fmt.Errorf("mode \"poll\" is disabled by server configuration (ENABLE_ASYNC=false)")
		}

		task, status, err := h.broker.ListenRole(ctx, projectID, p.Role, p.Mode, p.TimeoutMs)
		if err != nil {
			return nil, err
		}

		// Hand back a role-scoped poll_url so the worker can arm a background
		// poller. Minted on poll mode (the async path) and on a wait-mode
		// TIMEOUT — a blocking waiter that came back empty should switch to the
		// async poller rather than block again. Gated on EnableAsync: with async
		// disabled there is no poller path to offer, so we must not mint the url
		// (nor advise it) — otherwise the poll endpoint would resurrect the exact
		// capability the operator turned off. A wait that returns a task needs none.
		pollURL := ""
		if h.broker.EnableAsync && (p.Mode == "poll" || (p.Mode == "wait" && task == nil)) {
			pollURL = h.mintPollURL(projectID, pollBase, PollScopeRole, p.Role)
		}

		if task == nil {
			resp := map[string]any{
				"task":   nil,
				"status": status, // "empty" (poll) or "timeout" (wait)
			}
			if pollURL != "" {
				resp["poll_url"] = pollURL
				if p.Mode == "wait" {
					resp["advice"] = "no task within the wait timeout — arm broker-poll.sh on poll_url to be woken asynchronously instead of blocking again"
				}
			}
			return resp, nil
		}

		resp := map[string]any{
			"task": map[string]any{
				"task_id": task.ID,
				"title":   task.Title,
				"task_md": task.MD,
			},
		}
		if workToken := h.broker.TaskWorkToken(task); workToken != "" {
			resp["task"].(map[string]any)["work_token"] = workToken
		}
		if pollURL != "" {
			resp["poll_url"] = pollURL
		}
		return resp, nil

	case "list_tasks":
		var p struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		}
		json.Unmarshal(args, &p) // ignoring error as all fields are optional

		tasks, err := h.broker.ListAccessibleTasks(projectID, p.Role, p.Status, defaultListTasksLimit, 0)
		if err != nil {
			return nil, err
		}
		if tasks == nil {
			tasks = make([]StatusMetadata, 0)
		}
		return map[string]any{
			"tasks": tasks,
		}, nil

	case "get_task":
		var p struct {
			TaskID          string `json:"task_id"`
			IncludeTaskMD   bool   `json:"include_task_md"`
			IncludeResultMD bool   `json:"include_result_md"`
			WorkToken       string `json:"work_token"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("invalid arguments: task_id is required")
		}

		ownerProjectID, err := h.broker.ResolveWorkerProject(projectID, p.TaskID, p.WorkToken)
		if err != nil {
			return nil, err
		}
		meta, err := h.broker.GetTaskStatus(ownerProjectID, p.TaskID)
		if err != nil {
			return nil, err
		}

		resp := map[string]any{
			"task_id": meta.TaskID,
			"status":  meta.Status,
		}

		needsTaskMD := p.IncludeTaskMD || (meta.Status != StatusSolved && !p.IncludeTaskMD && !p.IncludeResultMD)
		needsResultMD := p.IncludeResultMD || (meta.Status == StatusSolved && !p.IncludeTaskMD && !p.IncludeResultMD)

		if needsTaskMD {
			md, err := h.broker.GetTaskMD(ownerProjectID, p.TaskID)
			if err == nil {
				resp["task_md"] = md
			}
		}

		if needsResultMD && meta.Status == StatusSolved {
			res, err := h.broker.GetTaskResult(ownerProjectID, p.TaskID)
			if err == nil {
				resp["result_md"] = res
				_, _ = h.broker.IncrementResultViewCount(ownerProjectID, p.TaskID)
			}
		}

		// Include a fresh task-scoped poll_url (unless already solved) so a
		// dispatcher whose poller token expired can re-arm without re-creating the
		// task — the "get the task again → new poll_url" path.
		if meta.Status != StatusSolved {
			if url := h.mintPollURL(ownerProjectID, pollBase, PollScopeTask, p.TaskID); url != "" {
				resp["poll_url"] = url
			}
		}

		return resp, nil

	case "solve_task":
		var p struct {
			TaskID    string `json:"task_id"`
			ResultMD  string `json:"result_md"`
			WorkToken string `json:"work_token"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" || p.ResultMD == "" {
			return nil, fmt.Errorf("invalid arguments: task_id and result_md are required")
		}
		ownerProject, err := h.broker.ResolveWorkerProject(projectID, p.TaskID, p.WorkToken)
		if err != nil {
			return nil, err
		}
		if err := h.broker.SolveTask(ownerProject, p.TaskID, p.ResultMD); err != nil {
			if p.WorkToken != "" {
				return nil, fmt.Errorf("solve_task failed for task_id %q", p.TaskID)
			}
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "progress_task":
		var p struct {
			TaskID    string `json:"task_id"`
			Message   string `json:"message"`
			WorkToken string `json:"work_token"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" || p.Message == "" {
			return nil, fmt.Errorf("invalid arguments: task_id and message are required")
		}
		if len(p.Message) > 500 {
			return nil, fmt.Errorf("message too long (max 500 chars)")
		}
		ownerProject, err := h.broker.ResolveWorkerProject(projectID, p.TaskID, p.WorkToken)
		if err != nil {
			return nil, err
		}
		if err := h.broker.ReportProgress(ownerProject, p.TaskID, p.Message); err != nil {
			if p.WorkToken != "" {
				return nil, fmt.Errorf("progress_task failed for task_id %q", p.TaskID)
			}
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	default:
		return nil, fmt.Errorf("tool not found: %s", name)
	}
}

func (h *JSONRPCHandler) sendError(w http.ResponseWriter, id json.RawMessage, code int, message string) error {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(Response{
		JSONRPC: "2.0",
		Error:   &RPCError{Code: code, Message: message},
		ID:      id,
	}); err != nil {
		return err
	}
	return flushResponse(w)
}

func (h *JSONRPCHandler) sendResult(w http.ResponseWriter, id json.RawMessage, result any) error {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(Response{
		JSONRPC: "2.0",
		Result:  result,
		ID:      id,
	}); err != nil {
		return err
	}
	return flushResponse(w)
}

func flushResponse(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
