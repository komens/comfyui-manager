package main

// ============================================================================
// 脚本运行台
//
// 为什么有这个：数据整理的需求一次一个样，做成固定功能（条件筛选 + 批量操作）
// 每加一种场景就要改后端、重新 build 镜像。不如把「跑一次性脚本」这条路直接
// 开出来 —— 前端写代码（或上传 .py）→ 点运行 → 看输出 → 拿走产出文件。
//
// 设计上的几条硬约束（改之前先看这里）：
//
//  1. 默认关闭。ENABLE_SCRIPTS 未设置时路由**不注册**，前端入口也不出现。
//     本项目没有鉴权，开了这个功能等于把服务变成 HTTP 可达的代码执行端点，
//     只有在可信网络里、且明确知道自己在干什么时才开。
//
//  2. 脚本对数据库是**只读**的（见 wfkit.connect 的 mode=ro）。要写数据必须走
//     HTTP 接口：库是 _txlock=immediate + busy_timeout(5000)，脚本里跑个长事务
//     会跟服务器的提交流程抢写锁，5 秒就 SQLITE_BUSY；而且直接改行会绕过收藏
//     保护和回收站，删完图就剩下孤儿文件。
//
//  3. 每次 run 一个独立工作目录 <DATA_DIR>/scripts/runs/<id>/，cwd 指过去。
//     脚本里写相对路径不会污染 data/，产出统一放 out/ 供下载。
//
//  4. 一次只跑一个。NAS 上 CPU 要和 ComfyUI 抢，排队比并发正确。
//
//  5. 输出有 2MB 上限。脚本里一个 print 循环就能把内存和库写满，
//     截断比 OOM 好。
//
//  6. 取消/超时必须杀**进程组**。只 kill 直接子进程的话，python 里起的
//     子进程会继续跑，run 显示已取消但 CPU 还在烧。
// ============================================================================

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// scriptOutputCapBytes 单次运行保留的输出上限（超出的部分丢弃并标记 truncated）。
	scriptOutputCapBytes = 2 << 20 // 2MB
	// scriptMaxCodeBytes 提交代码的体积上限。正常脚本远小于这个数。
	scriptMaxCodeBytes = 256 << 10 // 256KB
	// scriptDefaultTimeout 默认超时，可用 SCRIPT_TIMEOUT（秒）覆盖。
	scriptDefaultTimeout = 300 * time.Second
	// scriptMaxKeepRuns 保留最近多少次运行的记录与目录，超出的自动清掉。
	scriptMaxKeepRuns = 50
	// scriptQueueDepth 排队上限。一个人用，32 足够，也顺便挡住刷接口。
	scriptQueueDepth = 32
)

// scriptRuntime 一个可用的解释器。
type scriptRuntime struct {
	Language string `json:"language"`
	Path     string `json:"path"`
	Version  string `json:"version"`
}

// scriptRunner 脚本运行的调度器。为 nil 表示功能未开启。
type scriptRunner struct {
	app      *app
	log      *log.Logger
	runsDir  string
	apiBase  string
	timeout  time.Duration
	maxKeep  int
	runtimes map[string]scriptRuntime // language -> runtime
	queue    chan int64

	mu      sync.Mutex
	cancels map[int64]chan struct{}  // run id -> 取消信号
	live    map[int64]*liveScriptRun // run id -> 运行态输出缓冲
}

// liveScriptRun 一次运行的内存态：输出缓冲 + SSE 订阅者。
// 缓冲与订阅者在同一把锁下维护，避免「晚连上来的客户端」漏掉或重复收到内容。
type liveScriptRun struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	done      bool
	status    string
	subs      map[chan []byte]struct{}
}

// scriptRun 一条运行记录。code 是快照，work_dir 被清掉之后仍然可以载入重跑。
type scriptRun struct {
	ID              int64  `json:"id"`
	Title           string `json:"title"`
	Language        string `json:"language"`
	Code            string `json:"code"`
	Args            string `json:"args"`
	Status          string `json:"status"`
	ExitCode        int    `json:"exit_code"`
	DurationMS      int64  `json:"duration_ms"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"output_truncated"`
	WorkDir         string `json:"work_dir"`
	Error           string `json:"error"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
}

type scriptRunRequest struct {
	Code     string `json:"code"`
	Language string `json:"language"`
	Args     string `json:"args"`
	Title    string `json:"title"`
}

// ---------------------------------------------------------------------------
// 初始化
// ---------------------------------------------------------------------------

// initScripts 解析 ENABLE_SCRIPTS 并按需准备目录、探测解释器、起 worker。
// 未开启时返回 (nil, nil)，a.scripts 保持 nil，main 里据此不注册路由。
func (a *app) initScripts(cfg config) error {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ENABLE_SCRIPTS"))) {
	case "1", "true", "on", "yes":
	default:
		return nil
	}

	runsDir := filepath.Join(a.dataDir, "scripts", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return fmt.Errorf("create scripts dir: %w", err)
	}

	timeout := scriptDefaultTimeout
	if raw := strings.TrimSpace(os.Getenv("SCRIPT_TIMEOUT")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}

	runner := &scriptRunner{
		app:      a,
		log:      a.log,
		runsDir:  runsDir,
		apiBase:  "http://127.0.0.1:" + cfg.Port,
		timeout:  timeout,
		maxKeep:  scriptMaxKeepRuns,
		runtimes: detectScriptRuntimes(),
		queue:    make(chan int64, scriptQueueDepth),
		cancels:  make(map[int64]chan struct{}),
		live:     make(map[int64]*liveScriptRun),
	}
	if len(runner.runtimes) == 0 {
		a.log.Printf("scripts: ENABLE_SCRIPTS is on, but no python3/sh was found in PATH — runs will fail until the image ships an interpreter")
	}
	a.scripts = runner
	// 上一进程留下的 queued/running 记录已经没有对应进程了。不清掉会形成
	// 一条永远「运行中」的幽灵记录：页面一直转圈，取消接口也一直 409。
	a.recoverScriptRuns()
	go runner.worker()

	a.log.Printf("scripts: ON（脚本以服务进程权限执行，能读写 %s；仅在可信网络里开）", a.dataDir)
	a.log.Printf("scripts:   runs dir %s", runsDir)
	a.log.Printf("scripts:   timeout  %s（SCRIPT_TIMEOUT 秒可改）", runner.timeout)
	for _, rt := range runner.runtimes {
		a.log.Printf("scripts:   runtime  %-7s %s (%s)", rt.Language, rt.Path, rt.Version)
	}
	return nil
}

// recoverScriptRuns 启动时把上一个进程遗留的 in-flight 记录判掉。
// 脚本进程随服务进程一起没了，这些运行不可能再有结果。
func (a *app) recoverScriptRuns() {
	result, err := a.db.Exec(
		`UPDATE script_runs SET status='failed', exit_code=-1, error='服务重启，运行中断', finished_at=?
		 WHERE status IN ('queued','running')`, time.Now())
	if err != nil {
		a.log.Printf("scripts: recover interrupted runs failed: %v", err)
		return
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		a.log.Printf("scripts: %d 条中断的运行已标记为失败（服务重启）", affected)
	}
}

// detectScriptRuntimes 探测可用解释器。容器里默认只有 sh；
// python3 需要 Dockerfile 里装（见 Dockerfile 的 apk 列表）。
func detectScriptRuntimes() map[string]scriptRuntime {
	found := map[string]scriptRuntime{}
	python := ""
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			python = path
			break
		}
	}
	if python != "" {
		version := ""
		if out, err := exec.Command(python, "--version").Output(); err == nil {
			version = strings.TrimSpace(string(out))
		}
		found["python"] = scriptRuntime{Language: "python", Path: python, Version: version}
	}
	shell := ""
	for _, name := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(name); err == nil {
			shell = path
			break
		}
	}
	if shell != "" {
		found["sh"] = scriptRuntime{Language: "sh", Path: shell, Version: filepath.Base(shell)}
	}
	return found
}

// normalizeScriptLanguage 把前端/用户写的别名收敛成 runtime key。
func normalizeScriptLanguage(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "python", "py", "python3":
		return "python", true
	case "sh", "shell", "bash":
		return "sh", true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

func (a *app) scriptStatus(w http.ResponseWriter, r *http.Request) {
	runner := a.scripts
	runtimes := make([]scriptRuntime, 0, 2)
	if runner != nil {
		for _, rt := range []string{"python", "sh"} {
			if item, ok := runner.runtimes[rt]; ok {
				runtimes = append(runtimes, item)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  runner != nil,
		"runtimes": runtimes,
		"limits": map[string]any{
			"timeout_seconds": int(runner.timeout.Seconds()),
			"output_bytes":    scriptOutputCapBytes,
			"max_keep":        scriptMaxKeepRuns,
			"code_bytes":      scriptMaxCodeBytes,
		},
		"data_dir": a.dataDir,
	})
}

func (a *app) createScriptRun(w http.ResponseWriter, r *http.Request) {
	runner := a.scripts
	if runner == nil {
		writeError(w, http.StatusServiceUnavailable, "脚本功能未开启")
		return
	}
	var request scriptRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	code := request.Code
	if strings.TrimSpace(code) == "" {
		writeError(w, http.StatusBadRequest, "代码不能为空")
		return
	}
	if len(code) > scriptMaxCodeBytes {
		writeError(w, http.StatusBadRequest, "代码超过 256KB 上限")
		return
	}
	language, ok := normalizeScriptLanguage(request.Language)
	if !ok {
		writeError(w, http.StatusBadRequest, "只支持 python 和 sh")
		return
	}
	if _, ok := runner.runtimes[language]; !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("容器内没有 %s 解释器，请使用带 python3 的镜像", language))
		return
	}

	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = language + " · " + time.Now().Format("01-02 15:04")
	}
	if len([]rune(title)) > 60 {
		title = string([]rune(title)[:60])
	}

	result, err := a.db.Exec(
		`INSERT INTO script_runs(title, language, code, args, status, created_at) VALUES(?,?,?,?,'queued',?)`,
		title, language, code, strings.TrimSpace(request.Args), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建运行记录失败")
		return
	}
	id, _ := result.LastInsertId()

	select {
	case runner.queue <- id:
	default:
		// 队列满了：把记录标成失败，别留一条永远 queued 的幽灵记录
		_, _ = a.db.Exec(`UPDATE script_runs SET status='failed', error='队列已满，请稍后重试', finished_at=? WHERE id=?`, time.Now(), id)
		writeError(w, http.StatusTooManyRequests, "排队已满，请等当前脚本跑完")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "status": "queued"})
}

func (a *app) listScriptRuns(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM script_runs`).Scan(&total)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, title, language, status, exit_code, duration_ms, output_truncated, error, created_at, finished_at
		 FROM script_runs ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query runs failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0, pageSize)
	for rows.Next() {
		var id, exitCode int
		var durationMS int64
		var truncated int
		var title, language, status, errMsg, createdAt string
		var finishedAt sql.NullString
		if err := rows.Scan(&id, &title, &language, &status, &exitCode, &durationMS, &truncated, &errMsg, &createdAt, &finishedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "read run failed")
			return
		}
		items = append(items, map[string]any{
			"id": id, "title": title, "language": language, "status": status,
			"exit_code": exitCode, "duration_ms": durationMS,
			"output_truncated": truncated == 1, "error": errMsg,
			"created_at": createdAt, "finished_at": finishedAt.String,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (a *app) getScriptRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.loadScriptRun(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		if errors.Is(err, errInvalidID) {
			writeError(w, http.StatusBadRequest, "invalid run id")
			return
		}
		writeError(w, http.StatusInternalServerError, "read run failed")
		return
	}
	// 运行中的输出以内存态为准（库里的 output 只在结束时写一次）
	if a.scripts != nil {
		if live := a.scripts.liveRun(run.ID); live != nil {
			run.Output, run.OutputTruncated = live.snapshot()
		}
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *app) deleteScriptRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.loadScriptRun(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	if run.Status == "running" || run.Status == "queued" {
		writeError(w, http.StatusConflict, "运行中的记录不能删除，请先取消")
		return
	}
	if run.WorkDir != "" {
		_ = os.RemoveAll(run.WorkDir)
	}
	if _, err := a.db.Exec(`DELETE FROM script_runs WHERE id=?`, run.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": run.ID, "deleted": true})
}

func (a *app) cancelScriptRun(w http.ResponseWriter, r *http.Request) {
	runner := a.scripts
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := a.loadScriptRun(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	switch run.Status {
	case "queued":
		// 还在排队：直接改状态，worker 取到时会跳过
		if _, err := a.db.Exec(`UPDATE script_runs SET status='cancelled', error='已取消（未开始执行）', finished_at=? WHERE id=? AND status='queued'`, time.Now(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "cancel failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelled"})
	case "running":
		runner.mu.Lock()
		signal, ok := runner.cancels[id]
		runner.mu.Unlock()
		if !ok {
			writeError(w, http.StatusConflict, "该运行已结束")
			return
		}
		close(signal)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelling"})
	default:
		writeError(w, http.StatusConflict, "该运行已结束")
	}
}

// runScriptEvents 推一次运行的实时输出。
//
// 已结束的 run 进来先发一帧完整回放再关闭 —— 前端因此只有一条代码路径，
// 不用区分「追直播」和「看历史」。
func (a *app) runScriptEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// since 是字节偏移（前端固定传 0）：断线重连时可以只补后面那段
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	if since < 0 {
		since = 0
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	live := a.scripts.liveRun(id)
	if live == nil {
		run, err := a.loadScriptRun(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeScriptEvent(w, map[string]any{
			"type": "done", "status": run.Status, "output": run.Output,
			"truncated": run.OutputTruncated, "exit_code": run.ExitCode,
			"duration_ms": run.DurationMS, "error": run.Error,
		})
		flusher.Flush()
		return
	}

	channel, tail, finished := live.subscribe(since)
	fmt.Fprint(w, ": connected\n\n")
	if len(tail) > 0 {
		writeScriptEvent(w, map[string]any{"type": "chunk", "data": string(tail)})
	}
	flusher.Flush()
	if finished {
		// 刚好在订阅这一刻跑完：通道里只有一帧 done，收完就完
		for data := range channel {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		flusher.Flush()
		return
	}
	defer live.unsubscribe(channel)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case data, open := <-channel:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeScriptEvent(w http.ResponseWriter, event map[string]any) {
	data, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

// listScriptRunFiles 列出 out/ 下的产出文件。
func (a *app) listScriptRunFiles(w http.ResponseWriter, r *http.Request) {
	run, err := a.loadScriptRun(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	outDir := filepath.Join(run.WorkDir, "out")
	items := make([]map[string]any, 0)
	if run.WorkDir != "" {
		_ = filepath.WalkDir(outDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil {
				return nil
			}
			relative, relErr := filepath.Rel(outDir, path)
			if relErr != nil {
				return nil
			}
			items = append(items, map[string]any{
				"name": filepath.ToSlash(relative),
				"size": info.Size(),
			})
			return nil
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "dir": outDir})
}

func (a *app) downloadScriptRunFile(w http.ResponseWriter, r *http.Request) {
	run, err := a.loadScriptRun(r.PathValue("id"))
	if err != nil || run.WorkDir == "" {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	name := r.URL.Query().Get("name")
	clean := filepath.Clean("/" + name)
	target := filepath.Join(run.WorkDir, "out", clean)
	// 路径穿越：Clean 之后必须仍在 out/ 之内
	outDir := filepath.Join(run.WorkDir, "out")
	if !strings.HasPrefix(target, outDir+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "invalid file name")
		return
	}
	if !fileExists(target) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEncodePath(filepath.Base(target)))
	http.ServeFile(w, r, target)
}

func urlEncodePath(value string) string {
	// 只处理文件名，路径分隔符不会出现在这里
	var out strings.Builder
	for _, b := range []byte(value) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '-' || b == '_' {
			out.WriteByte(b)
		} else {
			out.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return out.String()
}

// loadScriptRun 读一条运行记录。
func (a *app) loadScriptRun(rawID string) (*scriptRun, error) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return nil, errInvalidID
	}
	run := &scriptRun{}
	var truncated int
	var startedAt, finishedAt sql.NullString
	err = a.db.QueryRow(
		`SELECT id, title, language, code, args, status, exit_code, duration_ms, output, output_truncated,
		        work_dir, error, created_at, started_at, finished_at
		 FROM script_runs WHERE id=?`, id).
		Scan(&run.ID, &run.Title, &run.Language, &run.Code, &run.Args, &run.Status, &run.ExitCode,
			&run.DurationMS, &run.Output, &truncated, &run.WorkDir, &run.Error, &run.CreatedAt,
			&startedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	run.OutputTruncated = truncated == 1
	run.StartedAt = startedAt.String
	run.FinishedAt = finishedAt.String
	return run, nil
}

var errInvalidID = errors.New("invalid id")

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

func (r *scriptRunner) worker() {
	for id := range r.queue {
		r.execute(id)
	}
}

func (r *scriptRunner) execute(id int64) {
	run, err := r.app.loadScriptRun(strconv.FormatInt(id, 10))
	if err != nil {
		return
	}
	workDir := filepath.Join(r.runsDir, strconv.FormatInt(id, 10))

	// 抢单用 CAS（WHERE status='queued'）：排队期间可能已被取消。
	// 先 SELECT 再 UPDATE 会留下一个窗口——取消刚好发生在两者之间时，
	// 记录已标成 cancelled，脚本却照样跑完并把状态覆盖回 succeeded。
	started := time.Now()
	result, err := r.app.db.Exec(
		`UPDATE script_runs SET status='running', started_at=?, work_dir=? WHERE id=? AND status='queued'`,
		started, workDir, id)
	if err != nil {
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return
	}

	runtime, ok := r.runtimes[run.Language]
	if !ok {
		r.failRun(id, "容器内没有 "+run.Language+" 解释器")
		return
	}

	if err := os.MkdirAll(filepath.Join(workDir, "out"), 0o755); err != nil {
		r.failRun(id, "创建运行目录失败: "+err.Error())
		return
	}

	scriptName := "main.py"
	flag := "-u"
	if run.Language == "sh" {
		scriptName = "main.sh"
		flag = ""
	}
	scriptPath := filepath.Join(workDir, scriptName)
	if err := os.WriteFile(scriptPath, []byte(run.Code), 0o644); err != nil {
		r.failRun(id, "写入脚本失败: "+err.Error())
		return
	}
	// wfkit 与脚本同目录，脚本里 `import wfkit` 即可用（sh 也能读环境变量）
	if err := os.WriteFile(filepath.Join(workDir, "wfkit.py"), []byte(wfkitSource), 0o644); err != nil {
		r.log.Printf("scripts: write wfkit for run %d failed: %v", id, err)
	}
	meta := map[string]any{
		"run_id": id, "language": run.Language, "args": run.Args,
		"title": run.Title, "data_dir": r.app.dataDir, "started_at": time.Now().Format(time.RFC3339),
	}
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(workDir, "meta.json"), data, 0o644)
	}

	now := started
	live := &liveScriptRun{subs: make(map[chan []byte]struct{})}
	r.mu.Lock()
	r.live[id] = live
	signal := make(chan struct{})
	r.cancels[id] = signal
	r.mu.Unlock()

	args := []string{scriptPath}
	if flag != "" {
		args = []string{flag, scriptPath}
	}
	args = append(args, splitScriptArgs(run.Args)...)
	cmd := exec.Command(runtime.Path, args...)
	cmd.Dir = workDir
	cmd.Env = r.scriptEnv(workDir, id)
	cmd.Stdout = live
	cmd.Stderr = live
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var timedOut, cancelled atomic.Bool
	if err := cmd.Start(); err != nil {
		r.finish(live, id, "failed", -1, 0, "启动失败: "+err.Error())
		return
	}
	pgid := cmd.Process.Pid // Setpgid 之后子进程自己就是一个进程组的组长

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-time.After(r.timeout):
			timedOut.Store(true)
			terminateScriptGroup(pgid)
		case <-signal:
			cancelled.Store(true)
			terminateScriptGroup(pgid)
		case <-watchDone:
		}
	}()

	waitErr := cmd.Wait()
	close(watchDone)
	duration := time.Since(now)

	finalStatus := "succeeded"
	exitCode := 0
	message := ""
	switch {
	case timedOut.Load():
		finalStatus, exitCode, message = "timeout", -1, fmt.Sprintf("超过 %s 未结束，已终止", r.timeout)
	case cancelled.Load():
		finalStatus, exitCode, message = "cancelled", -1, "已取消"
	case waitErr != nil:
		finalStatus = "failed"
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
			message = fmt.Sprintf("退出码 %d", exitCode)
		} else {
			exitCode = -1
			message = waitErr.Error()
		}
	}
	r.finish(live, id, finalStatus, exitCode, duration, message)
}

// finish 收尾：先广播 done（订阅者立刻拿到结果），再落库、清内存态、触发保留策略。
func (r *scriptRunner) finish(live *liveScriptRun, id int64, status string, exitCode int, duration time.Duration, message string) {
	output, truncated := live.snapshot()
	live.complete(status, exitCode, duration, message, truncated)
	r.saveRun(id, status, exitCode, duration, output, truncated, message)

	r.mu.Lock()
	delete(r.live, id)
	delete(r.cancels, id)
	r.mu.Unlock()

	if r.maxKeep > 0 {
		r.prune()
	}
}

// failRun 用于「进程还没起来就失败了」的早退路径（没有 live 态可广播）。
func (r *scriptRunner) failRun(id int64, message string) {
	r.saveRun(id, "failed", -1, 0, "", false, message)
}

func (r *scriptRunner) saveRun(id int64, status string, exitCode int, duration time.Duration, output string, truncated bool, message string) {
	truncatedFlag := 0
	if truncated {
		truncatedFlag = 1
	}
	if _, err := r.app.db.Exec(
		`UPDATE script_runs SET status=?, exit_code=?, duration_ms=?, output=?, output_truncated=?, error=?, finished_at=?
		 WHERE id=?`,
		status, exitCode, duration.Milliseconds(), output, truncatedFlag, message, time.Now(), id); err != nil {
		r.log.Printf("scripts: save run %d failed: %v", id, err)
	}
	r.log.Printf("scripts: run %d %s (exit %d)", id, status, exitCode)
	debugEvent("script_run", map[string]any{
		"run_id": id, "status": status, "exit_code": exitCode,
		"duration_ms": duration.Milliseconds(), "output_bytes": len(output),
	})
}

// prune 只保留最近 maxKeep 次运行，多余的连目录一起删。
func (r *scriptRunner) prune() {
	var count int
	if err := r.app.db.QueryRow(`SELECT COUNT(*) FROM script_runs WHERE status NOT IN ('queued','running')`).Scan(&count); err != nil {
		return
	}
	if count <= r.maxKeep {
		return
	}
	rows, err := r.app.db.Query(`SELECT id, work_dir FROM script_runs WHERE status NOT IN ('queued','running')
		ORDER BY id DESC LIMIT -1 OFFSET ?`, r.maxKeep)
	if err != nil {
		return
	}
	type stale struct {
		id      int64
		workDir string
	}
	var items []stale
	for rows.Next() {
		var item stale
		if err := rows.Scan(&item.id, &item.workDir); err == nil {
			items = append(items, item)
		}
	}
	rows.Close()
	for _, item := range items {
		if item.workDir != "" {
			_ = os.RemoveAll(item.workDir)
		}
		_, _ = r.app.db.Exec(`DELETE FROM script_runs WHERE id=?`, item.id)
	}
	if len(items) > 0 {
		r.log.Printf("scripts: pruned %d old runs (keep %d)", len(items), r.maxKeep)
	}
}

// scriptEnv 交给脚本的环境变量。
//
// 刻意不继承 os.Environ()：DEBUG 的自定义路径、COMFYUI_URL 之类没有必要暴露，
// 而且容器里能拿到什么应该是一份**写下来的契约**，不是偶然。
func (r *scriptRunner) scriptEnv(workDir string, id int64) []string {
	absData, _ := filepath.Abs(r.app.dataDir)
	absDB, _ := filepath.Abs(r.app.dbPath)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
		"LANG=C.UTF-8",
		"PYTHONUNBUFFERED=1", // 不加这个，print 会攒到进程结束才出来，日志等于没有
		"PYTHONIOENCODING=utf-8",
		"PYTHONDONTWRITEBYTECODE=1",
		"DATA_DIR=" + absData,
		"DB_PATH=" + absDB,
		"API_BASE=" + r.apiBase,
		"RUN_DIR=" + workDir,
		"RUN_ID=" + strconv.FormatInt(id, 10),
	}
	if tz := os.Getenv("TZ"); tz != "" {
		env = append(env, "TZ="+tz)
	}
	return env
}

// terminateScriptGroup 先 TERM 整个进程组，3 秒后 KILL 兜底。
//
// 必须杀组：python 里 subprocess 起的子进程不在同一棵「直接子进程」树上，
// 只 kill leader 会让它们继续跑（表现为 run 已取消，但 CPU 还在烧）。
//
// 兜底那一下存在极小的 pid 复用风险（3 秒内组号被新进程占用），
// 在 NAS 这种低进程周转的环境里可以接受——比留下一个跑飞的脚本划算。
func terminateScriptGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()
}

// splitScriptArgs 按空白切参数，支持单双引号包住带空格的值。
func splitScriptArgs(raw string) []string {
	var (
		args    []string
		current strings.Builder
		quote   rune
		has     bool
	)
	for _, ch := range raw {
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				current.WriteRune(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
			has = true
		case ch == ' ' || ch == '\t' || ch == '\n':
			if has {
				args = append(args, current.String())
				current.Reset()
				has = false
			}
		default:
			current.WriteRune(ch)
			has = true
		}
	}
	if has {
		args = append(args, current.String())
	}
	return args
}

// ---------------------------------------------------------------------------
// 运行时输出缓冲
// ---------------------------------------------------------------------------

func (l *liveScriptRun) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return len(p), nil
	}
	room := scriptOutputCapBytes - l.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			l.truncated = true
		}
		return len(p), nil
	}
	chunk := p
	if len(chunk) > room {
		chunk = chunk[:room]
		l.truncated = true
	}
	l.buf.Write(chunk)
	// 截断之后不再往前端推，否则前端内存无上限地涨
	if !l.truncated && len(l.subs) > 0 {
		frame, _ := json.Marshal(map[string]any{"type": "chunk", "data": string(p)})
		for channel := range l.subs {
			select {
			case channel <- frame:
			default: // 订阅者跟不上就丢这一帧，前端还有 GET 兜底
			}
		}
	}
	return len(p), nil
}

func (l *liveScriptRun) snapshot() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String(), l.truncated
}

// subscribe 注册订阅者并返回 [since:] 那段已有输出。
// 取快照与注册在同一把锁里，晚连的客户端不会漏也不会重复。
func (l *liveScriptRun) subscribe(since int) (chan []byte, []byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	channel := make(chan []byte, 64)
	if l.done {
		// 刚好在这一刻跑完：只给一帧带全文的 done，别再回放一遍 chunk（会重复）
		frame, _ := json.Marshal(map[string]any{
			"type": "done", "status": l.status, "output": l.buf.String(),
			"truncated": l.truncated,
		})
		channel <- frame
		close(channel)
		return channel, nil, true
	}
	full := l.buf.Bytes()
	if since > len(full) {
		since = len(full)
	}
	tail := append([]byte(nil), full[since:]...)
	l.subs[channel] = struct{}{}
	return channel, tail, false
}

func (l *liveScriptRun) unsubscribe(channel chan []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs != nil {
		delete(l.subs, channel)
	}
}

// complete 标记结束并给所有订阅者发最后一帧，然后关掉通道。
func (l *liveScriptRun) complete(status string, exitCode int, duration time.Duration, message string, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	l.status = status
	frame, _ := json.Marshal(map[string]any{
		"type": "done", "status": status, "exit_code": exitCode,
		"duration_ms": duration.Milliseconds(), "error": message,
		"truncated": truncated, "output": l.buf.String(),
	})
	for channel := range l.subs {
		select {
		case channel <- frame:
		default:
		}
		close(channel)
	}
	l.subs = nil
}

// liveRun 取运行态（不存在返回 nil）。
func (r *scriptRunner) liveRun(id int64) *liveScriptRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[id]
}
