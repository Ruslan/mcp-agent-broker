package main

import (
	"bufio"
	"fmt"
	"io"
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

// usageText documents the ONLY interface this binary has: the environment. It is
// printed on --help and on any unrecognized argument.
const usageText = `agent-broker — task broker for AI agents (MCP over JSON-RPC).

Usage:
  broker              start the server (no arguments — it is configured by the environment)
  broker --help       print this text and exit
  broker --version    print the version and exit

Starting the server binds a TCP port and creates the SQLite database relative to the
CURRENT DIRECTORY unless DB_PATH is absolute — run it from the repo root, or set the
variables below.

Environment:
  PORT                    listen port (default 9197)
  DB_PATH                 SQLite file (default data/broker.db, created if missing)
  PROMPTS_DIR             directory of role prompt files (default prompts)
  API_KEY                 bearer token for /rpc and /admin; EMPTY MEANS NO AUTH.
                          GET /health, /poll/{token} and /skill/install stay open.
  ENABLE_SYNC             sync tools: await_task, listen_role wait (default true)
  ENABLE_ASYNC            async tools: capability poll urls (default true)
  BROKER_PUBLIC_URL       absolute base url stamped into poll urls handed to agents
  BROKER_TRUST_FORWARDED  trust X-Forwarded-Host when deriving urls (default false)

A .env file in the current directory is loaded first; real environment variables win.

Endpoints:
  POST /rpc             JSON-RPC (MCP)      GET /admin/          admin UI
  GET  /health          liveness probe      GET /poll/{token}    capability poll url
  GET  /skill/install   installer scripts
`

// parseArgs decides whether main should boot a server. The broker takes no
// positional arguments and no flags — every setting comes from the environment —
// so anything on the command line is either a request for help or a mistake.
//
// Load-bearing: this used to be absent, and the binary ignored its arguments
// entirely. `broker --help`, the first thing anyone types to orient themselves,
// therefore booted a full server: it bound port 9197 and created a stray database
// in whatever directory the caller happened to be in. Exiting non-zero on an
// unknown argument is what keeps a typo from silently starting a second broker.
func parseArgs(args []string, stdout, stderr io.Writer) (exitCode int, stop bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return 0, true
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "agent-broker %s (MCP protocol %s)\n", ServerVersion, ProtocolVersion)
		return 0, true
	default:
		fmt.Fprintf(stderr, "broker: unrecognized argument %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return 2, true
	}
}

func main() {
	if code, stop := parseArgs(os.Args[1:], os.Stdout, os.Stderr); stop {
		os.Exit(code)
	}

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
	publicURL := strings.TrimSpace(os.Getenv("BROKER_PUBLIC_URL"))

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
	broker.PublicURL = publicURL
	broker.TrustForwarded = getEnvBool("BROKER_TRUST_FORWARDED", false)

	handler := &JSONRPCHandler{broker: broker}
	adminHandler := &AdminHandler{broker: broker}
	pollHandler := &PollHandler{broker: broker}

	mux := http.NewServeMux()
	mux.Handle("/rpc", handler)
	mux.HandleFunc("/health", handler.HealthHandler)

	// Unauthenticated capability-URL poll endpoint: the token in the path is the
	// authorization (see PollHandler / AuthMiddleware's /poll/ exemption).
	mux.Handle("GET /poll/{token}", pollHandler)

	// Unauthenticated skill installer over plain HTTP, for harnesses that can't
	// pull MCP prompts (prompts/get). Same body as the skill-install prompt; the
	// content is non-secret, so /skill/ is exempt in AuthMiddleware like /poll/.
	mux.HandleFunc("GET /skill/install", skillInstallHTTP)

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

// AuthMiddleware gates the command surface with the master API_KEY (when set),
// exactly as before — EXCEPT three non-secret surfaces. GET /poll/{token}
// self-authenticates via the unguessable token in its path, so a background
// `curl "$poll_url"` (which carries no Authorization header) must reach it
// without the master key. GET /skill/install serves the embedded, open-source
// installer scripts and must be `wget`-able by a harness with no credential.
// GET /health is the liveness probe: orchestrators (kamal-proxy, Docker
// HEALTHCHECK, a load balancer) poll it with no credential, and gating it would
// mean a deploy never goes healthy once API_KEY is set. Its body carries only
// version, protocol version and the two feature flags — no secrets, no data.
// All three are deliberately not behind the command-channel credential.
func AuthMiddleware(apiKey string, next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capability-URL poll endpoint: token-in-path is the authorization.
		// Skill installer: non-secret embedded scripts, served for harnesses
		// that can't pull MCP prompts. Health: unauthenticated liveness probe.
		// All three bypass the master key.
		if r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/poll/") || strings.HasPrefix(r.URL.Path, "/skill/") {
			next.ServeHTTP(w, r)
			return
		}

		if apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Unauthorized: Missing Authorization header", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "Unauthorized: Invalid Authorization header format", http.StatusUnauthorized)
			return
		}
		if strings.TrimPrefix(authHeader, prefix) != apiKey {
			http.Error(w, "Unauthorized: Invalid API key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
