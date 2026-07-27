package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	tests := []struct {
		name           string
		apiKey         string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "No API key required, no header provided",
			apiKey:         "",
			authHeader:     "",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "API key required, valid header provided",
			apiKey:         "secret",
			authHeader:     "Bearer secret",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "API key required, missing header",
			apiKey:         "secret",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "API key required, invalid format",
			apiKey:         "secret",
			authHeader:     "secret",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "API key required, wrong key",
			apiKey:         "secret",
			authHeader:     "Bearer wrong",
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := AuthMiddleware(tt.apiKey, dummyHandler)
			req := httptest.NewRequest("GET", "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

// TestHealthEndpointUnauthenticated: /health must answer with NO credential even
// when API_KEY is set. Deploy orchestrators (kamal-proxy, Docker HEALTHCHECK, a
// load balancer) probe it anonymously; gating it would make every deploy time out
// waiting for a container that is actually up. Load-bearing — see AuthMiddleware.
func TestHealthEndpointUnauthenticated(t *testing.T) {
	reached := false
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	handler := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest("GET", "/health", nil) // deliberately no Authorization
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /health with no credential: expected 200, got %d", rr.Code)
	}
	if !reached {
		t.Error("GET /health did not reach the wrapped handler — the auth exemption is gone")
	}

	// The exemption is an exact path match: it must NOT open a /health* prefix.
	for _, path := range []string{"/healthz", "/health/secrets", "/healthy"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s with no credential: expected 401, got %d", path, rr.Code)
		}
	}
}
