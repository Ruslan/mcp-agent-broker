package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
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

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9197"
	}
	addr := ":" + port

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	enableSync := getEnvBool("ENABLE_SYNC", true)
	enableAsync := getEnvBool("ENABLE_ASYNC", true)

	if err := validateConfig(enableSync, enableAsync); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	broker, err := NewBroker(dataDir, enableSync, enableAsync)
	if err != nil {
		log.Fatalf("Failed to initialize broker: %v", err)
	}

	handler := &JSONRPCHandler{broker: broker}

	mux := http.NewServeMux()
	mux.Handle("/rpc", handler)
	mux.HandleFunc("/health", handler.HealthHandler)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // Allow time to read body, but not forever
		WriteTimeout:      0,                // Disabled for blocking RPC responses
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("Agent Task Broker listening on %s (data: %s)", addr, dataDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
