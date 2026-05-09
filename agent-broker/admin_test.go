package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminHandler_UpdateTaskStatus(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &AdminHandler{broker: broker}

	taskID, err := broker.CreateTask(testProject, "coder", "Title", "MD")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/admin/api/tasks/"+taskID+"?project=default", strings.NewReader(`{"status":"solved"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("Expected 204 response, got %d: %s", rr.Code, rr.Body.String())
	}

	meta, err := broker.GetTaskStatus(testProject, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != StatusSolved {
		t.Fatalf("Expected solved status after PATCH, got %s", meta.Status)
	}
}
