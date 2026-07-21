package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestSkillInstallHTTP checks the plain-HTTP skill installer: it is reachable
// with NO Authorization header even when a master key is set, carries the same
// script bodies + version as the MCP prompt, and does not open a hole for the
// gated command surface.
func TestSkillInstallHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install", skillInstallHTTP)
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := AuthMiddleware("secret", mux)

	// /skill/install reachable with no credential.
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/skill/install", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("skill/install: got %d, want 200", rr.Code)
	}

	body := rr.Body.String()
	if body != buildSkillInstallPrompt() {
		t.Error("body is not byte-identical to buildSkillInstallPrompt()")
	}
	for _, want := range []string{"BROKER_SKILL_VERSION", "broker-poll.sh", "await-poll.sh", "broker-monitor.sh", "Where to put them"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if got, want := rr.Header().Get("X-Broker-Skill-Version"), strconv.Itoa(BrokerSkillVersion); got != want {
		t.Errorf("version header = %q, want %q", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content-type = %q, want text/markdown", ct)
	}

	// The exemption must not leak into the gated command surface.
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/rpc", nil))
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("rpc without key: got %d, want 401", rr2.Code)
	}
}
