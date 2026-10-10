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
	"runtime"
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
	// AllowedOrigins 是允许跨域访问的来源白名单（逗号分隔）。
	// 留空表示不输出任何 CORS 头：开发时前端走 vite proxy，生产时前端由本服务同源托管，
	// 两种场景都不需要跨域。
	AllowedOrigins string
}

type app struct {
	db       *sql.DB
	log      *log.Logger
	dataDir  string
	dbPath   string
	events   map[int64]map[chan []byte]struct{}
	eventsMu sync.Mutex
	// 保存图片（下载/写库/写盘）的连续失败计数：itemID -> 次数。
	// 仅 downloader 单个 goroutine 使用；加锁是为防止将来被并发调用时踩 map。
	saveFailures map[int64]int
	saveFailMu   sync.Mutex
	// 卡在 submitted 的 item 已经等了几个轮询周期：itemID -> 轮数。
	// 只在 DEBUG 开启时维护，用于每隔一段时间留一次「这个任务还在等」的痕。
	waiting   map[int64]int
	waitingMu sync.Mutex
	// cleanup 是数据整理的回收站。由 initCleanup 赋值，为 nil 时相关接口返回 503。
	cleanup *cleanupStore
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

	// DSN 参数必须是 modernc.org/sqlite 的语法：
	//   _pragma=...   逐条执行 PRAGMA（可重复）
	//   _txlock=immediate  BeginTx 直接 BEGIN IMMEDIATE
	// 注意：mattn/go-sqlite3 风格的 `_journal_mode=` / `_busy_timeout=` 会被**静默忽略**：
	// 曾因此 WAL 从未开启、busy_timeout=0，稍一并发写库就 SQLITE_BUSY(database is locked)。
	// 为什么必须加 _txlock=immediate：默认 deferred 事务若先 SELECT（拿读锁）再 UPDATE
	// （升级写锁），两个并发事务会互相等待升级 → 直接 SQLITE_BUSY，且 busy_timeout 对
	// 这种「升级死锁」不生效（SQLite 为避免无限等待会立即返回）。改成 immediate 后，
	// 事务一开始就取写锁，并发写会老老实实在 busy_timeout 内排队。
	// 驱动内部会把 busy_timeout 排在最前执行，确保设置 journal_mode 时已有锁等待。
	dsn := cfg.DBPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	a := &app{db: db, log: log.New(os.Stdout, "comfyui-server ", log.LstdFlags), dataDir: cfg.DataDir, dbPath: cfg.DBPath, events: make(map[int64]map[chan []byte]struct{}), saveFailures: make(map[int64]int), waiting: make(map[int64]int)}
	if err := a.initDB(cfg.InitialComfyUI); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o755); err != nil {
		log.Fatal(err)
	}
	// 回收站与索引要在服务对外之前就绪：initCleanup 会建 trash 目录与 cleanup_batches 表，
	// 并补上条件删除依赖的索引（见 maintenance.go 里的说明）
	if err := a.initCleanup(); err != nil {
		log.Fatal(err)
	}
	initDebug(cfg.DataDir, a.log)
	// 启动快照要等库和 settings 都可用之后才写得出来，所以放在这里
	dumpStartupSnapshot(a.startupSnapshot(cfg))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("GET /api/settings/backup", a.backupDatabase)
	mux.HandleFunc("GET /api/export", a.exportData)
	mux.HandleFunc("POST /api/import", a.importData)
	mux.HandleFunc("PUT /api/settings/comfyui", a.updateComfyUI)
	mux.HandleFunc("POST /api/settings/comfyui/test", a.testComfyUI)
	mux.HandleFunc("GET /api/workflows", a.listWorkflows)
	mux.HandleFunc("POST /api/workflows", a.createWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}", a.getWorkflow)
	mux.HandleFunc("PUT /api/workflows/{id}", a.updateWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", a.deleteWorkflow)
	mux.HandleFunc("PUT /api/workflows/{id}/default", a.setDefaultWorkflow)
	// 提示词落点：正负向各写到哪个节点的哪个字段。带 id 读库里那份，
	// 不带 id 时用 body 里的 workflow_json（编辑态下还没保存也能看落点）。
	mux.HandleFunc("POST /api/workflows/prompt-targets", a.detectWorkflowPromptTargets)
	mux.HandleFunc("GET /api/workflows/{id}/prompt-targets", a.getWorkflowPromptTargets)
	// 带 id 校验库里那份；不带 id 时用 body 里的 workflow_json
	// （前端在「还没保存的新工作流」编辑态下也能先校验再保存）。
	mux.HandleFunc("POST /api/workflows/validate", a.validateWorkflow)
	mux.HandleFunc("POST /api/workflows/{id}/validate", a.validateWorkflow)
	mux.HandleFunc("POST /api/tasks/direct", a.createDirectTask)
	mux.HandleFunc("GET /api/tasks", a.listTasks)
	mux.HandleFunc("POST /api/tasks/batch-cancel", a.batchCancelTasks)
	mux.HandleFunc("POST /api/tasks/batch-retry", a.batchRetryTasks)
	mux.HandleFunc("POST /api/tasks/batch-delete", a.batchDeleteTasks)
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
	mux.HandleFunc("POST /api/json-files/import-text", a.importJSONText)
	mux.HandleFunc("GET /api/json-files/{id}", a.getJSONFile)
	mux.HandleFunc("GET /api/json-files/{id}/download", a.downloadJSON)
	mux.HandleFunc("DELETE /api/json-files/{id}", a.deleteJSONFile)
	mux.HandleFunc("GET /api/images", a.listImages)
	mux.HandleFunc("GET /api/images/{id}", a.getImage)
	mux.HandleFunc("GET /api/images/{id}/file", a.imageFile)
	mux.HandleFunc("GET /api/images/{id}/download", a.downloadImage)
	mux.HandleFunc("DELETE /api/images/{id}", a.deleteImage)
	mux.HandleFunc("POST /api/images/batch-delete", a.batchDeleteImages)
	mux.HandleFunc("POST /api/images/batch-favorite", a.batchFavoriteImages)
	mux.HandleFunc("PATCH /api/images/{id}/favorite", a.toggleImageFavorite)
	// 数据整理：条件式批量清理 + 回收站。
	// preview 只查不删，cleanup 才是破坏性操作且要求 confirm=true。
	mux.HandleFunc("POST /api/maintenance/preview", a.previewCleanup)
	mux.HandleFunc("POST /api/maintenance/cleanup", a.runCleanup)
	mux.HandleFunc("GET /api/maintenance/stats", a.cleanupStatsHandler)
	mux.HandleFunc("POST /api/maintenance/vacuum", a.runVacuum)
	mux.HandleFunc("POST /api/maintenance/orphans/purge", a.purgeOrphans)
	mux.HandleFunc("GET /api/maintenance/trash", a.listTrash)
	mux.HandleFunc("GET /api/maintenance/trash/{id}", a.trashImages)
	mux.HandleFunc("POST /api/maintenance/trash/{id}/restore", a.restoreTrash)
	mux.HandleFunc("DELETE /api/maintenance/trash/{id}", a.purgeTrash)
	mux.HandleFunc("GET /api/stats", a.getStats)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": version})
	})
	// 调试接口只在 DEBUG 开启时注册。关闭时这些路径不被注册，会落到下面的
	// 静态兜底 —— 而 /api/ 前缀在兜底里被显式判成 404，所以线上确实
	// 不存在「可以下载日志」这个面。
	if debugOn {
		mux.HandleFunc("GET /api/debug/status", a.debugStatus)
		mux.HandleFunc("GET /api/debug/files/{name}", a.debugDownload)
		mux.HandleFunc("DELETE /api/debug/files/{name}", a.debugClear)
	}
	go a.submitter()
	go a.downloader()
	go a.recoverTasks()

	// Static file serving for frontend
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			staticFS := http.FileServer(http.Dir(cfg.StaticDir))
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				// /api/ 下没被上面注册的路径 = 这个接口不存在，直接 404。
				// 必须在这里截住：否则会落到下面的 SPA 兜底，返回 index.html
				// （200 + text/html）——排查时会被误读成「接口在，只是返回怪东西」，
				// 前端也拿不到准确的 404 来判断「这个能力没开」。
				if strings.HasPrefix(r.URL.Path, "/api/") {
					http.NotFound(w, r)
					return
				}
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
		Handler:           withCORS(cfg.AllowedOrigins, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.log.Printf("comfyui-server %s listening on %s, data directory %s", version, server.Addr, cfg.DataDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig() config {
	// 约定：后端在 backend/ 目录下启动，数据目录为上一级的 ../data。
	// 若从别处启动，必须显式指定 DATA_DIR，否则会指向错误的相对路径。
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
  is_default INTEGER NOT NULL DEFAULT 0,
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
  status TEXT NOT NULL DEFAULT 'pending',
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
		`ALTER TABLE workflows ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0`,
		// Add prompt_id to generation_items (for old databases with entry_id)
		`ALTER TABLE generation_items ADD COLUMN prompt_id INTEGER`,
		`ALTER TABLE prompts ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE images ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0`,
		`UPDATE generation_items SET status='pending' WHERE status='queued'`,
		// 任务表冗余工作流名：删除工作流后历史任务仍能显示名字（配合 deleteWorkflow 放宽）
		`ALTER TABLE generation_tasks ADD COLUMN workflow_name TEXT NOT NULL DEFAULT ''`,
		`UPDATE generation_tasks SET workflow_name=(SELECT w.name FROM workflows w WHERE w.id=generation_tasks.workflow_id) WHERE workflow_name='' AND workflow_id IS NOT NULL`,
		// 提交时的自动纠正说明（repair.go）。中性文案，不占 error_message——
		// 那一栏是红色的「失败原因」，成功项上出现会让人以为出错了。
		`ALTER TABLE generation_items ADD COLUMN notes TEXT NOT NULL DEFAULT ''`,
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
	// 重启恢复策略：
	// 1) 已经拿到 comfy_prompt_id 的 item —— 说明 ComfyUI 那边已收单，保留 submitted 与原 prompt_id，
	//    交给 downloader 查 /history 收尾。之前这里无条件清空 prompt_id 并重置为 pending，
	//    会导致每次重启都把已提交的任务重新提交一遍（重复排队、重复出图）。
	kept, _ := a.db.Exec(`UPDATE generation_items SET status='submitted' WHERE status IN ('submitted','running') AND COALESCE(comfy_prompt_id,'')!=''`)
	keptN, _ := kept.RowsAffected()
	// 2) 没有 prompt_id 的 item —— 提交中途中断（HTTP 已发但结果未知除外），重置为 pending 重新提交
	reset, _ := a.db.Exec(`UPDATE generation_items SET status='pending', comfy_prompt_id='' WHERE status IN ('submitted','running') AND COALESCE(comfy_prompt_id,'')=''`)
	resetN, _ := reset.RowsAffected()
	// 3) task 状态跟随：先统一回到 pending，若仍有 in-flight item 再提升为 running
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='pending', completed_at=NULL WHERE status IN ('pending','running')`)
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='running' WHERE status='pending' AND EXISTS (
		SELECT 1 FROM generation_items gi WHERE gi.task_id=generation_tasks.id AND gi.status IN ('submitted','running'))`)
	if keptN > 0 || resetN > 0 {
		a.log.Printf("recovered tasks: %d items kept in-flight (awaiting ComfyUI history), %d items reset to pending", keptN, resetN)
		// 「重启后任务自己变了状态」是最容易让人怀疑数据被改的地方，留个记录。
		debugEvent("recover", map[string]any{
			"kept_in_flight": keptN,
			"reset_pending":  resetN,
		})
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
		response, trace, requestErr := tracedDo(http.DefaultClient, request, "system_stats", 4096)
		if requestErr == nil {
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				trace.emit("reachable", nil)
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

// startupSnapshot 汇总「这台机器上的服务到底在读写哪些地方」。
//
// 起因：云端部署时 data 卷挂错、挂空、镜像与库结构对不上，症状都是
// 「库里是空的」或「明明改了文件却没生效」，而日志里看不出来。
// 写一份快照，出问题时先看它，比一处处试快得多。
func (a *app) startupSnapshot(cfg config) map[string]any {
	comfyURL, _ := a.setting(context.Background(), "comfyui_url")
	paths := map[string]any{}
	for name, path := range map[string]string{
		"data":      cfg.DataDir,
		"db":        filepath.Dir(cfg.DBPath),
		"images":    filepath.Join(cfg.DataDir, "images"),
		"workflows": filepath.Join(cfg.DataDir, "workflows"),
		"json":      filepath.Join(cfg.DataDir, "json"),
		"debug":     debugDir,
		"static":    cfg.StaticDir,
	} {
		paths[name] = describePath(path)
	}

	counts := map[string]any{}
	for name, query := range map[string]string{
		"workflows":        `SELECT COUNT(*) FROM workflows`,
		"generation_tasks": `SELECT COUNT(*) FROM generation_tasks`,
		"generation_items": `SELECT COUNT(*) FROM generation_items`,
		"images":           `SELECT COUNT(*) FROM images`,
		"prompts":          `SELECT COUNT(*) FROM prompts`,
	} {
		var n int
		if err := a.db.QueryRow(query).Scan(&n); err == nil {
			counts[name] = n
		} else {
			counts[name] = err.Error()
		}
	}

	return map[string]any{
		"time":         time.Now().Format(time.RFC3339),
		"version":      version,
		"listen":       ":" + cfg.Port,
		"pid":          os.Getpid(),
		"go":           runtime.Version(),
		"data_dir":     absOrSelf(cfg.DataDir),
		"db_path":      absOrSelf(cfg.DBPath),
		"static_dir":   absOrSelf(cfg.StaticDir),
		"comfyui_url":  comfyURL,
		"initial_url":  cfg.InitialComfyUI,
		"paths":        paths,
		"db_counts":    counts,
		"journal_mode": journalMode(a.db),
		// 关键列的存废直接反映「库结构跟得上镜像里的代码没有」。
		// 老库挂到新镜像上，缺列的症状是各种莫名其妙的 SQL 错误。
		"schema": map[string]any{
			"generation_items.notes": columnExists(a.db, "generation_items", "notes"),
			"workflows.is_default":   columnExists(a.db, "workflows", "is_default"),
			"generation_tasks.name":  columnExists(a.db, "generation_tasks", "workflow_name"),
			"prompts.is_favorite":    columnExists(a.db, "prompts", "is_favorite"),
		},
	}
}

func absOrSelf(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func journalMode(db *sql.DB) string {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "unknown: " + err.Error()
	}
	return mode
}

func columnExists(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notNull, primaryKey int
			name, columnType         string
			defaultValue             any
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
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

// withCORS 只对 ALLOWED_ORIGINS 里显式列出的来源回显跨域头。
//
// 之前这里写死 `Access-Control-Allow-Origin: *` 并放行 DELETE/PATCH，
// 等于让任意网页都能调用 POST /api/images/batch-delete、PUT /api/settings/comfyui
// 这类破坏性接口。默认（未配置白名单）不再输出任何 CORS 头：开发时前端走 vite proxy、
// 生产时前端由本服务同源托管，两种场景都不需要跨域。
func withCORS(allowedOrigins string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range strings.Split(allowedOrigins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = true
		}
	}
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE, OPTIONS")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dataFilePath 把库里存的 storage_path 解析成当前 DATA_DIR 下的真实路径。
//
// 规则只有两条：
//  1. 先按「当前 DATA_DIR / subdir / basename」拼，命中即用；
//  2. 拼不到时，只有**绝对路径**才允许作为兜底（绝对路径是明确记录下来的，语义无歧义）。
//
// 为什么相对路径不做兜底：storage_path 是写入时按当时的工作目录拼出来的，
// 拿当前 cwd 去解释它没有意义，而且会命中**别的** data 目录。最典型的坑：
// 把 data/ 复制成副本、再从 backend/ 起服务连副本库做验证时，旧格式的
// `../data/images/x.png` 会 stat 命中真实 data/ 下的图片 —— 于是「删副本里的图」
// 实际删掉的是真实图片。把相对路径一律收敛到 DATA_DIR 之下即可根治。
//
// 两边都拿不到时返回空串，调用方应据此返回 404。
func (a *app) dataFilePath(stored, subdir string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return ""
	}
	base := filepath.Base(stored)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	if candidate := filepath.Join(a.dataDir, subdir, base); fileExists(candidate) {
		return candidate
	}
	if filepath.IsAbs(stored) && fileExists(stored) {
		return stored
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
