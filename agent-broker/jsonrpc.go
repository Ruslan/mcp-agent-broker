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
				"description": "Returns lightweight task metadata only. Filters allowed.",
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
				"description": "Returns detailed content for one task. Defaults to most useful payload to save context.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id":           map[string]any{"type": "string"},
						"include_task_md":   map[string]any{"type": "boolean"},
						"include_result_md": map[string]any{"type": "boolean"},
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
						"task_id":   map[string]any{"type": "string"},
						"result_md": map[string]any{"type": "string"},
					},
					"required": []string{"task_id", "result_md"},
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
		return map[string]any{
			"task_id": taskID,
			"status":  "queued",
		}, nil

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
		status, res, err := h.broker.AwaitTask(ctx, projectID, p.TaskID, p.TimeoutMs)
		if err != nil {
			return nil, err
		}
		resp := map[string]any{
			"task_id": p.TaskID,
			"status":  status,
		}
		if status == string(StatusSolved) {
			resp["result_md"] = res
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

		if task == nil {
			return map[string]any{
				"task":   nil,
				"status": status, // "empty" or "timeout"
			}, nil
		}

		return map[string]any{
			"task": map[string]any{
				"task_id": task.ID,
				"title":   task.Title,
				"task_md": task.MD,
			},
		}, nil

	case "list_tasks":
		var p struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		}
		json.Unmarshal(args, &p) // ignoring error as all fields are optional

		tasks, err := h.broker.ListTasks(projectID, p.Role, p.Status)
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
		}
		if err := json.Unmarshal(args, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("invalid arguments: task_id is required")
		}

		meta, err := h.broker.GetTaskStatus(projectID, p.TaskID)
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
			md, err := h.broker.GetTaskMD(projectID, p.TaskID)
			if err == nil {
				resp["task_md"] = md
			}
		}

		if needsResultMD && meta.Status == StatusSolved {
			res, err := h.broker.GetTaskResult(projectID, p.TaskID)
			if err == nil {
				resp["result_md"] = res
			}
		}

		return resp, nil

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
