package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupDebug 把调试开关指到临时目录，返回 teardown。
func setupDebug(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("DEBUG", "1")
	debugOn = false
	debugDir = ""
	submitDumpPath = ""
	debugHTTPPath = ""
	debugEventPath = ""
	debugStartupPath = ""
	initDebug(dataDir, log.New(io.Discard, "", 0))
	if !debugOn {
		t.Fatal("debug should be on")
	}
	t.Cleanup(func() {
		debugOn = false
		debugDir = ""
		submitDumpPath = ""
		debugHTTPPath = ""
		debugEventPath = ""
		debugStartupPath = ""
	})
	return debugDir
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("bad jsonl line %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

// 非 2xx 必须自动留痕，且响应体装回 Body 后调用方仍能读全。
func TestTracedDoRecordsNon2xx(t *testing.T) {
	dir := setupDebug(t)

	const rejection = `{"error":{"type":"prompt_outputs_failed_validation"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, rejection)
	}))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/prompt", strings.NewReader("{}"))
	resp, trace, err := tracedDo(http.DefaultClient, req, "prompt", maxComfyErrorBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// tracedDo 读过一次响应体，但必须原样装回去
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != rejection {
		t.Fatalf("body must survive tracing, got %q", raw)
	}
	if trace == nil || trace.Status != http.StatusBadRequest {
		t.Fatalf("trace not filled: %+v", trace)
	}

	lines := readLines(t, filepath.Join(dir, "http.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 http record, got %d", len(lines))
	}
	if lines[0]["tag"] != "prompt" || lines[0]["status"].(float64) != 400 {
		t.Fatalf("bad record: %+v", lines[0])
	}
	if lines[0]["body"] != rejection {
		t.Fatalf("body should be captured, got %v", lines[0]["body"])
	}
}

// 2xx 不该自动留痕：/history 每 5 秒一次，全记会把日志淹掉。
// 但调用方主动 emit 时必须能补上（成功也需要留痕的场景）。
func TestTracedDoQuietOnSuccessButAllowsEmit(t *testing.T) {
	dir := setupDebug(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/history/abc", nil)
	resp, trace, err := tracedDo(http.DefaultClient, req, "history", 1024)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(dir, "http.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("success should not be recorded automatically")
	}
	trace.emit("prompt_id=abc", map[string]any{"bytes": 11})
	lines := readLines(t, filepath.Join(dir, "http.jsonl"))
	if len(lines) != 1 || lines[0]["note"] != "prompt_id=abc" || lines[0]["status"].(float64) != 200 {
		t.Fatalf("explicit emit should record: %+v", lines)
	}
}

// DEBUG 关闭时 tracedDo 必须与 client.Do 完全等价：不写文件、trace 为 nil，
// 且 nil trace 上的 emit 不能 panic（调用点因此不必判空）。
func TestTracedDoTransparentWhenDisabled(t *testing.T) {
	debugOn = false
	debugDir = ""
	debugHTTPPath = ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "nope")
	}))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/x", nil)
	resp, trace, err := tracedDo(http.DefaultClient, req, "history", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if trace != nil {
		t.Fatalf("trace should be nil when debug off, got %+v", trace)
	}
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != "nope" {
		t.Fatalf("body must pass through untouched, got %q", raw)
	}
	trace.emit("must not panic", nil) // nil receiver
	if debugHTTPPath != "" {
		t.Fatal("path should stay empty when disabled")
	}
}

// 传输错误（连不上）也要留下一条带 error 的记录。
func TestTracedDoRecordsTransportError(t *testing.T) {
	dir := setupDebug(t)

	// 127.0.0.1:9 是 discard 端口，通常无人监听
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/system_stats", nil)
	if _, _, err := tracedDo(&http.Client{Timeout: 2 * time.Second}, req, "system_stats", 1024); err == nil {
		t.Skip("port 9 unexpectedly reachable")
	}

	lines := readLines(t, filepath.Join(dir, "http.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 record, got %d", len(lines))
	}
	if lines[0]["error"] == nil || lines[0]["error"] == "" {
		t.Fatalf("transport error must be recorded: %+v", lines[0])
	}
}

// 单文件超过上限滚成 .1，且只留一代 —— DEBUG 忘了关不能把 data 卷写满。
func TestDebugFileRotation(t *testing.T) {
	dir := setupDebug(t)
	path := filepath.Join(dir, "events.jsonl")

	filler := strings.Repeat("x", 4096)
	for i := 0; i < (maxDebugFileBytes/len(filler))+2; i++ {
		appendDebugLine(path, map[string]any{"event": "filler", "blob": filler})
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file should exist: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxDebugFileBytes {
		t.Fatalf("current file should be under the cap after rotation, got %d", info.Size())
	}
}

func TestValidDebugFileName(t *testing.T) {
	allowed := []string{"startup.json", "events.jsonl", "http.jsonl", "submit-payloads.jsonl", "http.jsonl.1"}
	for _, name := range allowed {
		if !validDebugFileName(name) {
			t.Fatalf("%q should be allowed", name)
		}
	}
	rejected := []string{
		"", "../../etc/passwd", "..%2fetc", "comfyui.db", "secret.txt",
		"http.jsonl.2", "/etc/passwd", "debug/events.jsonl",
	}
	for _, name := range rejected {
		if validDebugFileName(name) {
			t.Fatalf("%q should be rejected", name)
		}
	}
}

// history 的 messages 会随节点执行累积，只保留末尾若干条，避免日志被撑爆。
func TestComfyHistoryDigestTrimsMessages(t *testing.T) {
	messages := make([]any, 0, maxDebugHistoryMessages+25)
	for i := 0; i < maxDebugHistoryMessages+25; i++ {
		messages = append(messages, []any{"executing", float64(i)})
	}
	history := map[string]any{
		"status":  map[string]any{"status_str": "error", "messages": messages},
		"outputs": map[string]any{"9": map[string]any{"images": []any{}}},
		"prompt":  []any{"should be dropped"},
	}

	digest := comfyHistoryDigest(history)
	status := digest["status"].(map[string]any)
	kept := status["messages"].([]any)
	if len(kept) != maxDebugHistoryMessages {
		t.Fatalf("want %d messages kept, got %d", maxDebugHistoryMessages, len(kept))
	}
	// 留下的是末尾（报错一定在最后）
	if last := kept[len(kept)-1].([]any)[1]; last != float64(maxDebugHistoryMessages+24) {
		t.Fatalf("should keep the tail, got %v", last)
	}
	if status["messages_dropped"] != 25 {
		t.Fatalf("dropped count wrong: %v", status["messages_dropped"])
	}
	if digest["outputs"] == nil {
		t.Fatal("outputs must be kept")
	}
	if _, ok := digest["prompt"]; ok {
		t.Fatal("prompt field is large and useless, should be dropped")
	}
}

// 事件流要能写出可解析的 JSONL，且 event/time 在字段里。
func TestDebugEventWritesJSONL(t *testing.T) {
	dir := setupDebug(t)
	debugEvent("item_failed", map[string]any{"item_id": int64(7), "error": "boom"})

	lines := readLines(t, filepath.Join(dir, "events.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 record, got %d", len(lines))
	}
	if lines[0]["event"] != "item_failed" || lines[0]["error"] != "boom" {
		t.Fatalf("bad record: %+v", lines[0])
	}
	if lines[0]["time"] == nil || lines[0]["time"] == "" {
		t.Fatalf("time missing: %+v", lines[0])
	}
}

// 启动快照是覆盖写，不是追加：重启多次只能留最新一份。
func TestStartupSnapshotOverwrites(t *testing.T) {
	dir := setupDebug(t)
	path := filepath.Join(dir, "startup.json")

	dumpStartupSnapshot(map[string]any{"version": "1.0.0"})
	dumpStartupSnapshot(map[string]any{"version": "1.1.0"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("startup.json must be a single JSON object: %v", err)
	}
	if snapshot["version"] != "1.1.0" {
		t.Fatalf("should be overwritten, got %v", snapshot["version"])
	}
}

func TestDescribePathReportsWritability(t *testing.T) {
	dir := t.TempDir()
	info := describePath(dir)
	if info["exists"] != true || info["is_dir"] != true || info["writable"] != true {
		t.Fatalf("temp dir should be a writable directory: %+v", info)
	}
	missing := describePath(filepath.Join(dir, "nope", "deep"))
	if missing["exists"] != false {
		t.Fatalf("missing path should report exists=false: %+v", missing)
	}
	// 探测文件必须清理干净，别在用户目录里留垃圾
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("write probe left files behind: %v", entries)
	}
}

// 卡在 submitted 的 item 要定期留一条心跳，否则「任务不动」在日志里等于没发生。
func TestNoteWaitingHeartbeat(t *testing.T) {
	dir := setupDebug(t)
	a := &app{waiting: map[int64]int{}}

	// 第 1 轮不记：刚提交完的那一轮本来就是 pending，记下来每张图都多一行噪音
	a.noteWaiting(7, "prompt not found in history")
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatal("first round should stay silent")
	}

	for i := 1; i < maxWaitingSilenceRounds; i++ {
		a.noteWaiting(7, "prompt not found in history")
	}
	lines := readLines(t, filepath.Join(dir, "events.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 heartbeat at round %d, got %d", maxWaitingSilenceRounds, len(lines))
	}
	if lines[0]["event"] != "waiting_for_output" || lines[0]["rounds"].(float64) != float64(maxWaitingSilenceRounds) {
		t.Fatalf("bad heartbeat: %+v", lines[0])
	}

	// 拿到结果后归零：下一轮又该沉默
	a.clearWaiting(7)
	a.noteWaiting(7, "prompt not found in history")
	if lines = readLines(t, filepath.Join(dir, "events.jsonl")); len(lines) != 1 {
		t.Fatalf("after clear the first round should be silent again, got %d records", len(lines))
	}
}

// DEBUG 关闭时不维护等待状态：没人看的日子不该让 map 无界增长。
func TestNoteWaitingNoopWhenDisabled(t *testing.T) {
	debugOn = false
	a := &app{waiting: map[int64]int{}}
	a.noteWaiting(7, "x")
	a.clearWaiting(7)
	if len(a.waiting) != 0 {
		t.Fatalf("waiting map must stay empty when debug off: %v", a.waiting)
	}
}
