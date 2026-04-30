package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type AdminHandler struct {
	broker *Broker
}

func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/admin/api/projects") {
		h.listProjects(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/tasks/") {
		h.getTask(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/tasks") {
		h.listTasks(w, r)
		return
	}
	if r.URL.Path == "/admin/events" {
		h.eventsHandler(w, r)
		return
	}
	http.NotFound(w, r)
}

func (h *AdminHandler) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := h.broker.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(projects)
}

func (h *AdminHandler) listTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		projectID = "default"
	}
	role := r.URL.Query().Get("role")
	status := r.URL.Query().Get("status")

	tasks, err := h.broker.ListTasks(projectID, role, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tasks)
}

func (h *AdminHandler) getTask(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	taskID := parts[len(parts)-1]
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		projectID = "default"
	}

	meta, err := h.broker.GetTaskStatus(projectID, taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	taskMD, _ := h.broker.GetTaskMD(projectID, taskID)
	resultMD, _ := h.broker.GetTaskResult(projectID, taskID)

	resp := map[string]any{
		"metadata":  meta,
		"task_md":   taskMD,
		"result_md": resultMD,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *AdminHandler) eventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.broker.Subscribe()
	defer h.broker.Unsubscribe(ch)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case event := <-ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: task_update\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
