package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestHealthEndpoint(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "broker-health-*")
	defer os.RemoveAll(tmpDir)

	broker, _ := NewBroker(tmpDir, "", true, true)
	handler := &JSONRPCHandler{broker: broker}

	// 1. Valid GET
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	r.Header.Set("X-Project-Id", "custom-project")
	handler.HealthHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)

	if res["ok"] != true || res["service"] != "agent-broker" {
		t.Errorf("Unexpected response: %v", res)
	}
	if res["version"] != ServerVersion || res["protocol_version"] != ProtocolVersion {
		t.Errorf("Unexpected version fields: %v", res)
	}
	if res["enable_sync"] != true || res["enable_async"] != true {
		t.Errorf("Unexpected flags: %v", res)
	}
	if res["project_id"] != "custom-project" {
		t.Errorf("Expected project_id custom-project, got %v", res["project_id"])
	}

	// 2. Invalid Method
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/health", nil)
	handler.HealthHandler(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}
