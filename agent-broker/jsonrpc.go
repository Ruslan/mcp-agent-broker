package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

	isNotification := req.ID == nil || string(req.ID) == "null"
	if isNotification {
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	ctx := r.Context()
	var result any
	var rpcErr *RPCError

	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-broker", "version": ServerVersion},
		}

	case "tools/list":
		var tools []any
		if h.broker.EnableSync {
			tools = append(tools,
				map[string]any{
					"name":        "create_task_sync",
					"description": "Send one sync task to a live worker and wait for the final result in this call. The sender should report that the task was sent and then wait for the returned result.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":    map[string]any{"type": "string"},
							"title":   map[string]any{"type": "string", "description": "Short task title (max 200 chars)"},
							"task_md": map[string]any{"type": "string"},
						},
						"required": []string{"role", "title", "task_md"},
					},
				},
				map[string]any{
					"name":        "listen_role_sync",
					"description": "Wait until one sync task arrives for this role. If a task is received, the worker should complete it, call solve_task with the same task_id, and then check for more tasks again.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role": map[string]any{"type": "string"},
						},
						"required": []string{"role"},
					},
				},
			)
		}
		if h.broker.EnableAsync {
			tools = append(tools,
				map[string]any{
					"name":        "create_task_async",
					"description": "Enqueue one async task and return immediately with a generated task_id. After sending, the sender should tell the human that execution is asynchronous and that they should later ask to check the result of this task_id. On the next relevant user message, the sender should check task status/result unless the user switched to an unrelated topic.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":    map[string]any{"type": "string"},
							"title":   map[string]any{"type": "string", "description": "Short task title (max 200 chars)"},
							"task_md": map[string]any{"type": "string"},
						},
						"required": []string{"role", "title", "task_md"},
					},
				},
				map[string]any{
					"name":        "listen_role_async",
					"description": "Poll once for one async task for this role. If a task is received, the worker should complete it, call solve_task with the same task_id, and then poll again for more tasks.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role": map[string]any{"type": "string"},
						},
						"required": []string{"role"},
					},
				},
				map[string]any{
					"name":        "get_task_status",
					"description": "Check the current status of an async task by task_id. Use this when the sender needs to see whether queued work is still pending, picked, or solved.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"task_id": map[string]any{"type": "string"},
						},
						"required": []string{"task_id"},
					},
				},
				map[string]any{
					"name":        "get_task_result",
					"description": "Get the final markdown result for an async task by task_id. Senders should use this after create_task_async when the user asks to check progress or result on a later relevant message.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"task_id": map[string]any{"type": "string"},
						},
						"required": []string{"task_id"},
					},
				},
			)
		}
		// solve_task is always available if the server is running (at least one mode is on)
		tools = append(tools, map[string]any{
			"name":        "solve_task",
			"description": "Submit the final markdown report for a task. Workers that received a task via listen_role_sync or listen_role_async should always call this when work is complete before checking for the next task.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":   map[string]any{"type": "string"},
					"result_md": map[string]any{"type": "string"},
				},
				"required": []string{"task_id", "result_md"},
			},
		})

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
			res, err := h.handleToolCall(ctx, projectID, params.Name, params.Arguments)
			if err != nil {
				rpcErr = &RPCError{Code: ErrApp, Message: err.Error()}
			} else {
				resJSON, _ := json.Marshal(res)
				result = map[string]any{
					"content": []any{map[string]any{"type": "text", "text": string(resJSON)}},
				}
			}
		}

	default:
		rpcErr = &RPCError{Code: ErrMethodNotFound, Message: fmt.Sprintf("Method not found: %s", req.Method)}
	}

	if rpcErr != nil {
		h.sendError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	h.sendResult(w, req.ID, result)
}

func (h *JSONRPCHandler) handleToolCall(ctx context.Context, projectID, name string, args json.RawMessage) (any, error) {
	switch name {
	case "create_task_sync":
		if !h.broker.EnableSync {
			return nil, fmt.Errorf("tool \"create_task_sync\" is disabled by server configuration")
		}
		var p struct {
			Role    string `json:"role"`
			Title   string `json:"title"`
			TaskMD  string `json:"task_md"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" || p.Title == "" || p.TaskMD == "" {
			return nil, fmt.Errorf("invalid arguments: role, title and task_md are required")
		}
		taskID, res, err := h.broker.CreateTaskSync(ctx, projectID, p.Role, p.Title, p.TaskMD)
		if err != nil {
			return nil, err
		}
		return map[string]string{"task_id": taskID, "result_md": res}, nil

	case "create_task_async":
		if !h.broker.EnableAsync {
			return nil, fmt.Errorf("tool \"create_task_async\" is disabled by server configuration")
		}
		var p struct {
			Role    string `json:"role"`
			Title   string `json:"title"`
			TaskMD  string `json:"task_md"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" || p.Title == "" || p.TaskMD == "" {
			return nil, fmt.Errorf("invalid arguments: role, title and task_md are required")
		}
		taskID, err := h.broker.CreateTaskAsync(projectID, p.Role, p.Title, p.TaskMD)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok":      true,
			"status":  "queued",
			"task_id": taskID,
		}, nil

	case "listen_role_sync":
		if !h.broker.EnableSync {
			return nil, fmt.Errorf("tool \"listen_role_sync\" is disabled by server configuration")
		}
		var p struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" {
			return nil, fmt.Errorf("invalid arguments: role is required")
		}
		task, err := h.broker.ListenRoleSync(ctx, projectID, p.Role)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"task_id": task.ID,
			"title":   task.Title,
			"task_md": task.MD,
		}, nil

	case "listen_role_async":
		if !h.broker.EnableAsync {
			return nil, fmt.Errorf("tool \"listen_role_async\" is disabled by server configuration")
		}
		var p struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Role == "" {
			return nil, fmt.Errorf("invalid arguments: role is required")
		}
		task, found, err := h.broker.ListenRoleAsync(projectID, p.Role)
		if err != nil {
			return nil, err
		}
		if !found {
			return map[string]any{
				"found":  false,
				"status": "no_task",
			}, nil
		}
		return map[string]any{
			"found":   true,
			"task_id": task.ID,
			"title":   task.Title,
			"task_md": task.MD,
		}, nil

	case "solve_task":
		var p struct {
			TaskID   string `json:"task_id"`
			ResultMD string `json:"result_md"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" || p.ResultMD == "" {
			return nil, fmt.Errorf("invalid arguments: task_id and result_md are required")
		}
		if err := h.broker.SolveTask(projectID, p.TaskID, p.ResultMD); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "get_task_status":
		if !h.broker.EnableAsync {
			return nil, fmt.Errorf("tool \"get_task_status\" is disabled by server configuration")
		}
		var p struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("invalid arguments: task_id is required")
		}
		meta, err := h.broker.GetTaskStatus(projectID, p.TaskID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"task_id": meta.TaskID,
			"title":   meta.Title,
			"status":  meta.Status,
		}, nil

	case "get_task_result":
		if !h.broker.EnableAsync {
			return nil, fmt.Errorf("tool \"get_task_result\" is disabled by server configuration")
		}
		var p struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("invalid arguments: task_id is required")
		}
		meta, err := h.broker.GetTaskStatus(projectID, p.TaskID)
		if err != nil {
			return nil, err
		}
		if meta.Status != StatusSolved {
			return map[string]any{
				"task_id":      meta.TaskID,
				"title":        meta.Title,
				"status":       meta.Status,
				"result_ready": false,
			}, nil
		}
		res, err := h.broker.GetTaskResult(projectID, p.TaskID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"task_id":   meta.TaskID,
			"title":     meta.Title,
			"status":    meta.Status,
			"result_md": res,
		}, nil

	default:
		return nil, fmt.Errorf("tool not found: %s", name)
	}
}

func (h *JSONRPCHandler) sendError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		JSONRPC: "2.0",
		Error:   &RPCError{Code: code, Message: message},
		ID:      id,
	})
}

func (h *JSONRPCHandler) sendResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		JSONRPC: "2.0",
		Result:  result,
		ID:      id,
	})
}
