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
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type config struct {
	DataDir        string
	DBPath         string
	InitialComfyUI string
	Port           string
	StaticDir      string
}

type app struct {
	db       *sql.DB
	log      *log.Logger
	dataDir  string
	dbPath   string
	events   map[int64]map[chan []byte]struct{}
	eventsMu sync.Mutex
}

type urlRequest struct {
	URL string `json:"url"`
}

var version = "dev"

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	a := &app{db: db, log: log.New(os.Stdout, "comfyui-server ", log.LstdFlags), dataDir: cfg.DataDir, dbPath: cfg.DBPath, events: make(map[int64]map[chan []byte]struct{})}
	if err := a.initDB(cfg.InitialComfyUI); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o755); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("GET /api/settings/backup", a.backupDatabase)
	mux.HandleFunc("GET /api/export", a.exportData)
	mux.HandleFunc("POST /api/import", a.importData)
	mux.HandleFunc("PUT /api/settings/comfyui", a.updateComfyUI)
	mux.HandleFunc("POST /api/settings/comfyui/test", a.testComfyUI)
	mux.HandleFunc("GET /api/comfyui/status", a.comfyUIStatus)
	mux.HandleFunc("GET /api/workflows", a.listWorkflows)
	mux.HandleFunc("POST /api/workflows", a.createWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}", a.getWorkflow)
	mux.HandleFunc("PUT /api/workflows/{id}", a.updateWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", a.deleteWorkflow)
	mux.HandleFunc("POST /api/workflows/detect-params", a.detectWorkflowParams)
	mux.HandleFunc("POST /api/tasks/direct", a.createDirectTask)
	mux.HandleFunc("GET /api/tasks", a.listTasks)
	mux.HandleFunc("GET /api/tasks/{id}", a.getTask)
	mux.HandleFunc("GET /api/tasks/{id}/events", a.taskEvents)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", a.cancelTask)
	mux.HandleFunc("POST /api/tasks/{id}/retry", a.retryTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", a.deleteTask)
	mux.HandleFunc("GET /api/prompts", a.listPrompts)
	mux.HandleFunc("POST /api/prompts", a.createPrompt)
	mux.HandleFunc("GET /api/prompts/groups", a.listPromptGroups)
	mux.HandleFunc("GET /api/prompts/{id}", a.getPrompt)
	mux.HandleFunc("PUT /api/prompts/{id}", a.updatePrompt)
	mux.HandleFunc("DELETE /api/prompts/{id}", a.deletePrompt)
	mux.HandleFunc("PATCH /api/prompts/{id}/favorite", a.togglePromptFavorite)
	mux.HandleFunc("POST /api/prompts/batch-delete", a.batchDeletePrompts)
	mux.HandleFunc("POST /api/prompts/batch-run", a.batchRunPrompts)
	mux.HandleFunc("POST /api/prompts/group-run", a.groupRunPrompts)
	mux.HandleFunc("POST /api/prompts/{id}/run", a.runPrompt)
	mux.HandleFunc("GET /api/json-files", a.listJSONFiles)
	mux.HandleFunc("POST /api/json-files/upload", a.uploadJSON)
	mux.HandleFunc("GET /api/json-files/{id}", a.getJSONFile)
	mux.HandleFunc("GET /api/json-files/{id}/download", a.downloadJSON)
	mux.HandleFunc("DELETE /api/json-files/{id}", a.deleteJSONFile)
	mux.HandleFunc("GET /api/images", a.listImages)
	mux.HandleFunc("GET /api/images/{id}", a.getImage)
	mux.HandleFunc("GET /api/images/{id}/file", a.imageFile)
	mux.HandleFunc("GET /api/images/{id}/download", a.downloadImage)
	mux.HandleFunc("DELETE /api/images/{id}", a.deleteImage)
	mux.HandleFunc("PATCH /api/images/{id}/favorite", a.toggleImageFavorite)
	mux.HandleFunc("GET /api/stats", a.getStats)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": version})
	})
	go a.submitter()
	go a.downloader()
	go a.recoverTasks()

	// Static file serving for frontend
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			staticFS := http.FileServer(http.Dir(cfg.StaticDir))
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				// Try to serve static file
				path := filepath.Join(cfg.StaticDir, r.URL.Path)
				if _, err := os.Stat(path); err == nil {
					staticFS.ServeHTTP(w, r)
					return
				}
				// SPA fallback: serve index.html for non-file paths
				if !strings.Contains(filepath.Base(r.URL.Path), ".") {
					http.ServeFile(w, r, filepath.Join(cfg.StaticDir, "index.html"))
					return
				}
				http.NotFound(w, r)
			})
			a.log.Printf("serving static files from %s", cfg.StaticDir)
		} else {
			a.log.Printf("static directory %s not found, skipping", cfg.StaticDir)
		}
	}

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.log.Printf("comfyui-server %s listening on %s, data directory %s", version, server.Addr, cfg.DataDir)
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
		StaticDir:      envOr("STATIC_DIR", ""),
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
  negative_prompt TEXT NOT NULL DEFAULT '',
  params_schema TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS json_files (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  filename TEXT NOT NULL,
  storage_path TEXT NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS prompts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  positive_prompt TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  completed_at DATETIME,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
  prompt_id INTEGER,
  positive_prompt TEXT NOT NULL,
  comfy_prompt_id TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  error_message TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(task_id) REFERENCES generation_tasks(id),
  FOREIGN KEY(prompt_id) REFERENCES prompts(id)
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
	if err != nil {
		return err
	}
	// Migrate schema for existing databases
	migrations := []string{
		// Add new columns to workflows
		`ALTER TABLE workflows ADD COLUMN negative_prompt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workflows ADD COLUMN params_schema TEXT NOT NULL DEFAULT '{}'`,
		// Add prompt_id to generation_items (for old databases with entry_id)
		`ALTER TABLE generation_items ADD COLUMN prompt_id INTEGER`,
		`ALTER TABLE prompts ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE images ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range migrations {
		if _, err := a.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			// Ignore duplicate column errors
		}
	}
	// Migrate data from prompt_entries to prompts if prompt_entries exists
	var hasPromptEntries int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='prompt_entries'`).Scan(&hasPromptEntries)
	if hasPromptEntries > 0 {
		_, _ = a.db.Exec(`INSERT OR IGNORE INTO prompts(id, title, description, positive_prompt, group_name, group_id, status, completed_at, created_at, updated_at)
			SELECT pe.id, pe.title, pe.description, pe.positive_prompt, COALESCE(jf.filename,''), '', pe.status, pe.completed_at, pe.created_at, pe.updated_at
			FROM prompt_entries pe LEFT JOIN json_files jf ON pe.json_file_id=jf.id`)
		// Update generation_items to use prompt_id from entry_id
		_, _ = a.db.Exec(`UPDATE generation_items SET prompt_id=entry_id WHERE prompt_id IS NULL AND entry_id IS NOT NULL`)
		_, _ = a.db.Exec(`DROP TABLE IF EXISTS prompt_entries`)
		a.log.Printf("migrated prompt_entries to prompts")
	}
	return nil
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *app) getStats(w http.ResponseWriter, r *http.Request) {
	var taskCount, imageCount, pendingEntries int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM generation_tasks`).Scan(&taskCount)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM images`).Scan(&imageCount)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM prompts WHERE status='pending'`).Scan(&pendingEntries)
	var runningTasks int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM generation_tasks WHERE status='running'`).Scan(&runningTasks)
	writeJSON(w, http.StatusOK, map[string]any{"total_tasks": taskCount, "total_images": imageCount, "pending_entries": pendingEntries, "running_tasks": runningTasks})
}

func (a *app) recoverTasks() {
	// 重启时将 submitted/running 的 item 重置为 pending，由 submitter 重新提交
	n, _ := a.db.Exec(`UPDATE generation_items SET status='pending', comfy_prompt_id='' WHERE status IN ('submitted','running')`)
	affected, _ := n.RowsAffected()
	// 将 pending/running 的 task 重置为 pending
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='pending', failed_count=0, completed_at=NULL WHERE status IN ('pending','running')`)
	if affected > 0 {
		a.log.Printf("recovered %d items to pending", affected)
	}
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

func (a *app) backupDatabase(w http.ResponseWriter, r *http.Request) {
	dbPath := a.dbPath
	if dbPath == "" {
		dbPath = filepath.Join(a.dataDir, "db", "comfyui.db")
	}
	if _, err := os.Stat(dbPath); err != nil {
		writeError(w, http.StatusNotFound, "database file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="comfyui-backup-%s.db"`, time.Now().Format("20060102_150405")))
	http.ServeFile(w, r, dbPath)
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

// parsePagination 从请求中解析分页参数，返回 (page, pageSize, offset)
func parsePagination(r *http.Request) (int, int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	return page, pageSize, offset
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
