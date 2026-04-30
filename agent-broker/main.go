package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func getEnvBool(name string, defaultVal bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	return v == "true"
}

func validateConfig(enableSync, enableAsync bool) error {
	if !enableSync && !enableAsync {
		return fmt.Errorf("both ENABLE_SYNC and ENABLE_ASYNC are disabled")
	}
	return nil
}

func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return // Optional: .env doesn't have to exist
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		// Basic unquoting
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}

		// Only set if not already present in environment
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadDotEnv()

	port := os.Getenv("PORT")
	if port == "" {
		port = "9197"
	}
	addr := ":" + port

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/broker.db"
	}

	promptsDir := os.Getenv("PROMPTS_DIR")
	if promptsDir == "" {
		promptsDir = "prompts"
	}

	enableSync := getEnvBool("ENABLE_SYNC", true)
	enableAsync := getEnvBool("ENABLE_ASYNC", true)
	apiKey := os.Getenv("API_KEY")

	if err := validateConfig(enableSync, enableAsync); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	broker, err := NewBroker(store, promptsDir, enableSync, enableAsync)
	if err != nil {
		log.Fatalf("Failed to initialize broker: %v", err)
	}

	handler := &JSONRPCHandler{broker: broker}
	adminHandler := &AdminHandler{broker: broker}

	mux := http.NewServeMux()
	mux.Handle("/rpc", handler)
	mux.HandleFunc("/health", handler.HealthHandler)

	// Admin API
	mux.Handle("/admin/api/", adminHandler)
	mux.Handle("/admin/events", adminHandler)

	// Admin UI (SPA)
	adminDist, err := fs.Sub(adminFS, "dist")
	if err != nil {
		log.Fatalf("Failed to open embedded admin UI: %v", err)
	}
	mux.Handle("/admin/", http.StripPrefix("/admin", http.FileServer(http.FS(adminDist))))

	wrappedMux := AuthMiddleware(apiKey, mux)

	server := &http.Server{
		Addr:              addr,
		Handler:           wrappedMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // Allow time to read body, but not forever
		WriteTimeout:      0,                // Disabled for blocking RPC responses
		IdleTimeout:       120 * time.Second,
	}

	authStatus := "disabled"
	if apiKey != "" {
		authStatus = "enabled"
	}

	log.Printf("Agent Task Broker listening on %s (db: %s, sync: %v, async: %v, auth: %s)", addr, dbPath, enableSync, enableAsync, authStatus)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func AuthMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Unauthorized: Missing Authorization header", http.StatusUnauthorized)
			return
		}

		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "Unauthorized: Invalid Authorization header format", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, prefix)
		if token != apiKey {
			http.Error(w, "Unauthorized: Invalid API key", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
