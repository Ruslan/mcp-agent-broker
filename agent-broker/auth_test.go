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
