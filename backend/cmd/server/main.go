package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type config struct {
	DataDir        string
	DBPath         string
	InitialComfyUI string
	Port           string
}

type app struct {
	db      *sql.DB
	log     *log.Logger
	dataDir string
}

type urlRequest struct {
	URL string `json:"url"`
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	a := &app{db: db, log: log.New(os.Stdout, "comfyui-server ", log.LstdFlags), dataDir: cfg.DataDir}
	if err := a.initDB(cfg.InitialComfyUI); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings/comfyui", a.updateComfyUI)
	mux.HandleFunc("POST /api/settings/comfyui/test", a.testComfyUI)
	mux.HandleFunc("GET /api/comfyui/status", a.comfyUIStatus)
	mux.HandleFunc("GET /api/workflows", a.listWorkflows)
	mux.HandleFunc("POST /api/workflows", a.createWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}", a.getWorkflow)
	mux.HandleFunc("POST /api/tasks/direct", a.createDirectTask)
	mux.HandleFunc("GET /api/tasks", a.listTasks)
	mux.HandleFunc("GET /api/tasks/{id}", a.getTask)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           withJSON(withCORS(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.log.Printf("listening on %s, data directory %s", server.Addr, cfg.DataDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig() config {
	dataDir := envOr("DATA_DIR", "../data")
	dbPath := envOr("DB_PATH", filepath.Join(dataDir, "db", "comfyui.db"))
	return config{
		DataDir:        dataDir,
		DBPath:         dbPath,
		InitialComfyUI: envOr("COMFYUI_URL", "http://127.0.0.1:8188"),
		Port:           envOr("SERVER_PORT", "8080"),
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (a *app) initDB(initialURL string) error {
	const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS workflows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  workflow_path TEXT NOT NULL,
  mapping_json TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS generation_tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_type TEXT NOT NULL,
  workflow_id INTEGER NOT NULL,
  comfyui_url TEXT NOT NULL,
  parameters_json TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  total_count INTEGER NOT NULL DEFAULT 1,
  success_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  started_at DATETIME,
  completed_at DATETIME,
  FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);
CREATE TABLE IF NOT EXISTS generation_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  positive_prompt TEXT NOT NULL,
  negative_prompt TEXT NOT NULL DEFAULT '',
  comfy_prompt_id TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  error_message TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(task_id) REFERENCES generation_tasks(id)
);
CREATE TABLE IF NOT EXISTS images (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  generation_item_id INTEGER NOT NULL,
  filename TEXT NOT NULL,
  storage_path TEXT NOT NULL,
  created_at DATETIME NOT NULL,
  FOREIGN KEY(generation_item_id) REFERENCES generation_items(id)
);
INSERT OR IGNORE INTO settings(key, value, updated_at)
VALUES ('comfyui_url', ?, CURRENT_TIMESTAMP);
`
	_, err := a.db.Exec(schema, initialURL)
	return err
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *app) getSettings(w http.ResponseWriter, r *http.Request) {
	value, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comfyui_url": value})
}

func (a *app) updateComfyUI(w http.ResponseWriter, r *http.Request) {
	var request urlRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	normalized, err := normalizeURL(request.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `
INSERT INTO settings(key, value, updated_at) VALUES('comfyui_url', ?, CURRENT_TIMESTAMP)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`, normalized); err != nil {
		writeError(w, http.StatusInternalServerError, "save setting failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comfyui_url": normalized})
}

func (a *app) testComfyUI(w http.ResponseWriter, r *http.Request) {
	baseURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bodyURL := r.URL.Query().Get("url"); bodyURL != "" {
		baseURL, err = normalizeURL(bodyURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/system_stats", nil)
	if err == nil {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				writeJSON(w, http.StatusOK, map[string]any{"reachable": true, "url": baseURL, "latency_ms": time.Since(start).Milliseconds()})
				return
			}
			err = fmt.Errorf("ComfyUI returned HTTP %d", response.StatusCode)
		} else {
			err = requestErr
		}
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"reachable": false, "url": baseURL, "latency_ms": time.Since(start).Milliseconds(), "error": err.Error()})
}

func (a *app) comfyUIStatus(w http.ResponseWriter, r *http.Request) {
	value, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": value, "reachable": false, "message": "call /api/settings/comfyui/test to check"})
}

func (a *app) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	return value, err
}

func normalizeURL(raw string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("ComfyUI 地址必须是 http(s)://主机:端口，不能包含路径或查询参数")
	}
	if parsed.Port() == "" {
		return "", errors.New("ComfyUI 地址必须包含端口")
	}
	return value, nil
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, OPTIONS")
		next.ServeHTTP(w, r)
	})
}
